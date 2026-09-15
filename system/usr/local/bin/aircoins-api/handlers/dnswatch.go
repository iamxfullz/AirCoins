package handlers

import (
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// dnswatch.go keeps authorized captive-portal clients online across ISP
// changes. When the unit is moved (e.g. plugged in at a client's house) or
// the ISP's DNS changes while running, authorized clients' per-MAC DNAT/DNS
// rules still point at the OLD ISP's DNS and they lose internet.
//
// StartDNSWatcher:
//   1. runs one pass ~30s after boot (giving WAN/DHCP time to come up),
//   2. then polls every 60s.
//
// Each pass detects the current upstream DNS the same way
// `aircoins-captive-rules detect_upstream_dns()` does (systemd-resolved
// per-link servers -> /etc/resolv.conf -> default gateway), compares it
// with the last-seen value persisted in /var/lib/pisowifi/captive/last_dns,
// and runs `aircoins-captive-rules refresh` when it changed. The refresh
// command itself is a no-op when the DNS is unchanged, so extra calls are
// harmless.

const (
	dnsWatchStateFile  = "/var/lib/pisowifi/captive/last_dns"
	dnsWatchBootDelay  = 30 * time.Second
	dnsWatchPollEvery  = 60 * time.Second
	dnsWatchStableNeed = 2 // consecutive identical readings before acting
)

// detectUpstreamDNS mirrors the shell script's detect_upstream_dns():
// 1) systemd-resolved per-link servers (skip the 127.0.0.53 stub),
// 2) first nameserver in /etc/resolv.conf (skip stubs),
// 3) the default gateway (ISP routers usually act as DNS).
func detectUpstreamDNS() string {
	// 1) systemd-resolved
	if out, err := exec.Command("resolvectl", "status").Output(); err == nil {
		re := regexp.MustCompile(`DNS Servers: (.+)`)
		for _, line := range strings.Split(string(out), "\n") {
			if m := re.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
				for _, srv := range strings.Fields(m[1]) {
					if srv != "127.0.0.53" && srv != "127.0.0.1" && srv != "#" {
						return srv
					}
				}
			}
		}
	}

	// 2) /etc/resolv.conf
	if data, err := os.ReadFile("/etc/resolv.conf"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "nameserver") {
				fields := strings.Fields(line)
				if len(fields) >= 2 && fields[1] != "127.0.0.53" && fields[1] != "127.0.0.1" {
					return fields[1]
				}
			}
		}
	}

	// 3) Default gateway as DNS (ISP routers usually act as DNS).
	if out, err := exec.Command("ip", "route", "show", "default").Output(); err == nil {
		re := regexp.MustCompile(`via\s+(\S+)`)
		if m := re.FindStringSubmatch(string(out)); m != nil &&
			m[1] != "127.0.0.1" && m[1] != "127.0.0.53" {
			return m[1]
		}
	}

	return ""
}

// readLastDNS loads the last-seen upstream DNS from the state file.
// Returns "" when the file is missing (first boot / fresh install).
func readLastDNS() string {
	data, err := os.ReadFile(dnsWatchStateFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// writeLastDNS persists the last-seen upstream DNS.
func writeLastDNS(dns string) {
	if dns == "" {
		return
	}
	if err := os.MkdirAll("/var/lib/pisowifi/captive", 0755); err != nil {
		log.Printf("[dnswatch] cannot create state dir: %v", err)
		return
	}
	if err := os.WriteFile(dnsWatchStateFile, []byte(dns), 0644); err != nil {
		log.Printf("[dnswatch] cannot write state file: %v", err)
	}
}

// runDNSRefresh executes `aircoins-captive-rules refresh` and logs the
// result. The script prints "DNS unchanged ... no refresh needed" when the
// rules already match, so redundant calls are cheap and safe.
func runDNSRefresh() {
	out, err := exec.Command("aircoins-captive-rules", "refresh").CombinedOutput()
	output := strings.TrimSpace(string(out))
	if err != nil {
		log.Printf("[dnswatch] refresh failed: %v (%s)", err, output)
		return
	}
	log.Printf("[dnswatch] refresh: %s", output)
}

// dnsWatchPass performs one detect-compare-refresh cycle. It only acts when
// the new reading has been stable for dnsWatchStableNeed consecutive polls,
// which prevents acting on a flapping link mid-ISP-reboot.
func dnsWatchPass(streak *int, pending *string) {
	current := detectUpstreamDNS()
	if current == "" {
		// No usable DNS yet (WAN still down) — reset the stability streak.
		*streak = 0
		*pending = ""
		return
	}

	last := readLastDNS()
	if last == current {
		*streak = 0
		*pending = ""
		return
	}

	// DNS differs from the stored value. Wait for stability, then act.
	if *pending == current {
		*streak++
	} else {
		*pending = current
		*streak = 1
	}
	if *streak < dnsWatchStableNeed {
		log.Printf("[dnswatch] DNS change detected (%s -> %s), waiting for it to stabilize (%d/%d)",
			last, current, *streak, dnsWatchStableNeed)
		return
	}

	log.Printf("[dnswatch] DNS changed: %s -> %s — refreshing authorized clients", last, current)
	runDNSRefresh()
	writeLastDNS(current)
	*streak = 0
	*pending = ""
}

// StartDNSWatcher launches the boot pass and the polling loop. It never
// returns; call it as `go handlers.StartDNSWatcher()` during startup.
func StartDNSWatcher() {
	// Give RestoreWANOnBoot / DHCP time to bring the WAN up after boot.
	time.Sleep(dnsWatchBootDelay)

	// Boot pass: the unit may have been moved to a different ISP (plugged
	// in at a client's house). If the detected DNS differs from the stored
	// value — or nothing is stored yet — align all tracked clients now.
	bootStreak, bootPending := 0, ""
	dnsWatchPass(&bootStreak, &bootPending)

	ticker := time.NewTicker(dnsWatchPollEvery)
	defer ticker.Stop()
	streak, pending := 0, ""
	for range ticker.C {
		dnsWatchPass(&streak, &pending)
	}
}