package handlers

import (
	"aircoins-api/models"
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// portal.go provisions the HOTSPOT PORTAL STACK on one interface:
// portal IP (owned by networkd), dnsmasq DHCP + wildcard DNS hijack,
// and the captive iptables rules. The interface is usually a VLAN from
// vlan.go (e.g. end0.22) but any interface name is accepted. VLANs
// without a portal server are plain networks — vlan.go never touches
// IPs or DHCP.

const portalConfigPath = "/etc/pisowifi/portals.conf"

// captiveRulesPath is the helper that installs/removes the per-interface
// captive portal iptables rules (DNS + HTTP capture, MASQUERADE for
// paying clients). Contract: aircoins-captive-rules add|del <iface> <gateway>
const captiveRulesPath = "/usr/local/bin/aircoins-captive-rules"

const defaultDHCPLease = "12h"

// ============================================
// PORTAL CONFIG FILE HELPERS
// ============================================
// portals.conf is the boot-time source of truth (aircoins-vlan-apply
// phase 2 runs before the API/DB is up). The DB row is a mirror kept
// for the admin UI and reporting, same pattern as vlans.conf.

// portalConfigEntry represents one line in /etc/pisowifi/portals.conf.
// Format: interface ip/cidr dhcp_start dhcp_end lease enabled|disabled anti_hotspot_on|anti_hotspot_off
type portalConfigEntry struct {
	Interface   string
	IPCIDR      string
	DHCPStart   string
	DHCPEnd     string
	Lease       string
	Enabled     bool
	AntiHotspot bool
}

// readPortalConfig reads all entries from /etc/pisowifi/portals.conf.
func readPortalConfig() []portalConfigEntry {
	f, err := os.Open(portalConfigPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	var entries []portalConfigEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) < 6 {
			continue
		}

		entry := portalConfigEntry{
			Interface: parts[0],
			IPCIDR:    parts[1],
			DHCPStart: parts[2],
			DHCPEnd:   parts[3],
			Lease:     parts[4],
			Enabled:   parts[5] == "enabled",
		}
		// 7th column (anti_hotspot) is optional for backward compatibility
		if len(parts) >= 7 {
			entry.AntiHotspot = parts[6] == "anti_hotspot_on"
		}
		entries = append(entries, entry)
	}

	return entries
}

// writePortalConfig rewrites the entire portals.conf from the given entries.
func writePortalConfig(entries []portalConfigEntry) {
	if err := os.MkdirAll(filepath.Dir(portalConfigPath), 0755); err != nil {
		log.Printf("Failed to create config dir: %v", err)
		return
	}

	f, err := os.OpenFile(portalConfigPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		log.Printf("Failed to open portal config for writing: %v", err)
		return
	}
	defer f.Close()

	for _, e := range entries {
		state := "disabled"
		if e.Enabled {
			state = "enabled"
		}
		antiHotspot := "anti_hotspot_off"
		if e.AntiHotspot {
			antiHotspot = "anti_hotspot_on"
		}
		fmt.Fprintf(f, "%s %s %s %s %s %s %s\n",
			e.Interface, e.IPCIDR, e.DHCPStart, e.DHCPEnd, e.Lease, state, antiHotspot)
	}
}

// writePortalConfigEntry upserts a single entry (keyed by interface).
func writePortalConfigEntry(entry portalConfigEntry) {
	entries := readPortalConfig()

	found := false
	for i := range entries {
		if entries[i].Interface == entry.Interface {
			entries[i] = entry
			found = true
			break
		}
	}
	if !found {
		entries = append(entries, entry)
	}

	writePortalConfig(entries)
}

// removePortalConfigEntry deletes an entry and rewrites the file.
func removePortalConfigEntry(iface string) {
	entries := readPortalConfig()

	var filtered []portalConfigEntry
	for _, e := range entries {
		if e.Interface == iface {
			continue
		}
		filtered = append(filtered, e)
	}

	writePortalConfig(filtered)
}

// findPortalEntry returns the saved entry for an interface, if any.
func findPortalEntry(iface string) (portalConfigEntry, bool) {
	for _, e := range readPortalConfig() {
		if e.Interface == iface {
			return e, true
		}
	}
	return portalConfigEntry{}, false
}

// portalAttached reports whether a portal server is configured on the
// interface (used by VLANDelete to refuse deleting a portal-bearing VLAN).
func portalAttached(iface string) bool {
	if _, ok := findPortalEntry(iface); ok {
		return true
	}
	// Fall back to the DB mirror in case the flat file was lost
	var one int
	if err := models.DB.QueryRow("SELECT 1 FROM portal_servers WHERE interface=$1", iface).Scan(&one); err == nil {
		return true
	}
	return false
}

// ============================================
// DHCP RANGE HELPERS
// ============================================

// u32ToIP formats a uint32 as dotted-quad.
func u32ToIP(v uint32) string {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)).String()
}

// ipToU32 packs an IPv4 address into a uint32 for ordering/offset math.
// Returns 0 for non-IPv4 input.
func ipToU32(ip net.IP) uint32 {
	p4 := ip.To4()
	if p4 == nil {
		return 0
	}
	return uint32(p4[0])<<24 | uint32(p4[1])<<16 | uint32(p4[2])<<8 | uint32(p4[3])
}

// dhcpRangeFor derives a DHCP range from the portal CIDR: start at
// startIP if given (and inside the subnet), else network+100; end is
// start+100, clamped to broadcast-1. Matches the pre-split defaults so
// migrated portals keep the exact same range (e.g. .100–.200 on a /24).
func dhcpRangeFor(ipCIDR string, startIP string) (string, string) {
	_, ipNet, err := net.ParseCIDR(ipCIDR)
	if err != nil {
		return "", ""
	}
	network := ipNet.IP.To4()
	if network == nil {
		return "", ""
	}

	base := ipToU32(network)

	ones, bits := ipNet.Mask.Size()
	hostBits := uint(bits - ones)
	if hostBits < 2 {
		return "", ""
	}
	maxHost := (uint32(1) << hostBits) - 1 // e.g. 255 for /24

	var startOffset uint32 = 100
	if startIP != "" {
		if parsed := net.ParseIP(startIP); parsed != nil {
			if p4 := parsed.To4(); p4 != nil && ipNet.Contains(parsed) {
				if offset := ipToU32(p4) - base; offset > 0 && offset < maxHost {
					startOffset = offset
				}
			}
		}
	}
	// Subnets too small for the default offset: start right after the gateway
	if startOffset >= maxHost-1 {
		startOffset = 2
	}

	start := base + startOffset
	end := start + 100
	if end-base >= maxHost {
		end = base + maxHost - 1
	}

	return u32ToIP(start), u32ToIP(end)
}

// ============================================
// PROVISIONING PRIMITIVES
// ============================================

// applyCaptiveRules runs aircoins-captive-rules for an interface. action
// is "add" or "del". Failures are logged but never fatal: a portal
// without captive rules is still usable, it just won't auto-pop.
// antiHotspot, when true, sets TTL=1 on ALL forwarded packets from the
// portal interface (mangle PREROUTING). This is exactly like MikroTik
// hotspot server TTL=1.
func applyCaptiveRules(action, iface, ipCIDR string, antiHotspot bool) {
	gateway := strings.Split(ipCIDR, "/")[0]
	if gateway == "" {
		log.Printf("applyCaptiveRules: skipping %s for %s: no gateway IP", action, iface)
		return
	}

	if _, err := os.Stat(captiveRulesPath); err != nil {
		log.Printf("applyCaptiveRules: %s not installed; skipping captive rules for %s", captiveRulesPath, iface)
		return
	}

	// Call the shell script with standard 3 args only (backward compatible).
	if out, err := exec.Command(captiveRulesPath, action, iface, gateway).CombinedOutput(); err != nil {
		log.Printf("applyCaptiveRules: %s %s %s failed: %v — %s", action, iface, gateway, err, string(out))
		return
	}

	// Anti-hotspot: set TTL=1 on ALL packets from this portal interface.
	// Done directly in Go via iptables — no shell script dependency.
	// This is exactly like MikroTik hotspot server TTL=1.
	if antiHotspot {
		applyTTL1(iface)
	} else if action == "del" {
		removeTTL1(iface)
	}

	log.Printf("applyCaptiveRules: %s captive rules for %s (%s, anti_hotspot=%v)", action, iface, gateway, antiHotspot)
}

// applyTTL1 sets TTL=1 on ALL packets going out this interface (the
// download path). Uses mangle POSTROUTING so it affects packets after
// routing. This is exactly like MikroTik hotspot server TTL=1.
//
// The download-path stamp needs NO per-session bypass: a directly
// connected client receives TTL=1 packets fine (one hop), while a
// client that is actually a hotspot/router must forward them and drops
// them (TTL expires), which is the anti-tethering effect.
func applyTTL1(iface string) {
	// Create the mangle chain
	if _, err := exec.Command("iptables", "-t", "mangle", "-S", "AIRCOINS_TTL").CombinedOutput(); err != nil {
		if out, err := exec.Command("iptables", "-t", "mangle", "-N", "AIRCOINS_TTL").CombinedOutput(); err != nil {
			log.Printf("applyTTL1: failed to create chain: %v — %s", err, string(out))
			return
		}
	}

	// Purge legacy upstream-design rules for this interface. Old AirCoins
	// builds (and manual iptables setups) stamped TTL=1 on INCOMING client
	// packets (-i) and relied on a per-MAC RETURN bypass added at auth.
	// The current build never manages those bypasses, so any leftover -i
	// stamp silently kills a paying client's internet the moment its
	// bypass is missing (iPhone pays, gets TTL=1 upstream, ISP router
	// drops everything beyond itself). Delete every -i rule for this
	// interface — TTL stamps and MAC bypasses alike.
	if out, err := exec.Command("iptables", "-t", "mangle", "-S", "AIRCOINS_TTL").CombinedOutput(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "-A AIRCOINS_TTL -i "+iface+" ") {
				continue
			}
			spec := strings.Fields(strings.TrimPrefix(line, "-A AIRCOINS_TTL "))
			args := append([]string{"-t", "mangle", "-D", "AIRCOINS_TTL"}, spec...)
			exec.Command("iptables", args...).Run()
			log.Printf("applyTTL1: purged legacy upstream rule: %s", line)
		}
	}

	// Drop a stale PREROUTING hook left by the legacy design (the chain
	// is hooked in POSTROUTING below; a PREROUTING jump is useless for
	// -o rules and only ever carried the legacy -i stamps).
	for exec.Command("iptables", "-t", "mangle", "-C", "PREROUTING", "-j", "AIRCOINS_TTL").Run() == nil {
		if exec.Command("iptables", "-t", "mangle", "-D", "PREROUTING", "-j", "AIRCOINS_TTL").Run() != nil {
			break
		}
		log.Printf("applyTTL1: removed stale PREROUTING hook for AIRCOINS_TTL")
	}

	// Remove stray direct POSTROUTING TTL stamps for this interface. These
	// are manual duplicates of this chain's own rule (commonly added by
	// hand); they double-stamp and — worse — are typically frozen to disk
	// with netfilter-persistent, which resurrects stale whitelists on boot.
	// AirCoins owns the stamp: managed chain + re-applied on every boot.
	for _, ttl := range []string{"1", "64"} {
		for exec.Command("iptables", "-t", "mangle", "-C", "POSTROUTING", "-o", iface, "-j", "TTL", "--ttl-set", ttl).Run() == nil {
			if exec.Command("iptables", "-t", "mangle", "-D", "POSTROUTING", "-o", iface, "-j", "TTL", "--ttl-set", ttl).Run() != nil {
				break
			}
			log.Printf("applyTTL1: removed stray manual POSTROUTING TTL rule for %s (ttl-set %s)", iface, ttl)
		}
	}

	// Hook into mangle POSTROUTING
	if _, err := exec.Command("iptables", "-t", "mangle", "-C", "POSTROUTING", "-j", "AIRCOINS_TTL").CombinedOutput(); err != nil {
		if out, err := exec.Command("iptables", "-t", "mangle", "-I", "POSTROUTING", "1", "-j", "AIRCOINS_TTL").CombinedOutput(); err != nil {
			log.Printf("applyTTL1: failed to hook chain: %v — %s", err, string(out))
			return
		}
	}

	// Set TTL=1 on ALL packets going out this interface — no exceptions
	if _, err := exec.Command("iptables", "-t", "mangle", "-C", "AIRCOINS_TTL", "-o", iface, "-j", "TTL", "--ttl-set", "1").CombinedOutput(); err != nil {
		if out, err := exec.Command("iptables", "-t", "mangle", "-A", "AIRCOINS_TTL", "-o", iface, "-j", "TTL", "--ttl-set", "1").CombinedOutput(); err != nil {
			log.Printf("applyTTL1: failed to set TTL=1 for %s: %v — %s", iface, err, string(out))
			return
		}
	}

	log.Printf("applyTTL1: TTL=1 set on ALL packets to %s (mangle POSTROUTING)", iface)
}

// removeTTL1 removes the TTL=1 rule for a portal interface. When the
// last portal's rule is gone, the POSTROUTING hook is removed too.
func removeTTL1(iface string) {
	exec.Command("iptables", "-t", "mangle", "-D", "AIRCOINS_TTL", "-o", iface, "-j", "TTL", "--ttl-set", "1").Run()
	log.Printf("removeTTL1: removed TTL=1 for %s", iface)

	// If no TTL stamps remain in the chain, unhook it from POSTROUTING.
	if out, err := exec.Command("iptables", "-t", "mangle", "-S", "AIRCOINS_TTL").CombinedOutput(); err == nil {
		if !strings.Contains(string(out), "--ttl-set") {
			exec.Command("iptables", "-t", "mangle", "-D", "POSTROUTING", "-j", "AIRCOINS_TTL").Run()
			log.Printf("removeTTL1: chain empty, POSTROUTING hook removed")
		}
	}
}

// writeNetworkdConfig writes a per-interface systemd-networkd .network file
// so networkd OWNS the static portal IP instead of flushing it as a foreign
// address on every `networkctl reload`. The 04- prefix makes it win
// (lexically) over both the 05-aircoins-vlans.network catch-all and the
// 10-netplan runtime config, since networkd applies only the FIRST
// matching .network file.
func writeNetworkdConfig(iface, cidr string) error {
	content := fmt.Sprintf(`[Match]
Name=%s

[Network]
Address=%s
DHCP=no
ConfigureWithoutCarrier=yes
`, iface, cidr)
	path := fmt.Sprintf("/etc/systemd/network/04-aircoins-%s.network", iface)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return err
	}
	exec.Command("networkctl", "reload").Run() // best-effort
	return nil
}

// removeNetworkdConfig deletes the per-interface networkd file and reloads networkd.
func removeNetworkdConfig(iface string) {
	os.Remove(fmt.Sprintf("/etc/systemd/network/04-aircoins-%s.network", iface))
	exec.Command("networkctl", "reload").Run()
}

// isDHCPActive checks whether the dnsmasq template instance is active for
// a given interface name (e.g. "end0.13").
func isDHCPActive(iface string) bool {
	cmd := exec.Command("systemctl", "is-active", fmt.Sprintf("dnsmasq@%s", iface))
	return cmd.Run() == nil
}

// startPortalDHCP writes the dnsmasq config for a portal and starts+enables
// the template service. It stops any running instance before writing the
// new config to avoid stale state.
func startPortalDHCP(e portalConfigEntry) error {
	gateway := strings.Split(e.IPCIDR, "/")[0]

	// DNS is deliberately ENABLED here: address=/#/<gateway> answers every
	// hostname with the gateway IP, which is what captures the captive portal
	// detection probes (captive.apple.com, connectivitycheck.gstatic.com, ...)
	// and makes the portal pop up automatically.
	//
	// except-interface=lo together with bind-dynamic is what keeps multiple
	// per-interface instances from fighting over 127.0.0.1:53 — do NOT go
	// back to port=0 to fix a bind conflict, it disables the portal capture.
	//
	// Authorized clients are not affected: aircoins-captive-rules DNATs their
	// port 53 traffic to a real upstream resolver.
	configContent := fmt.Sprintf(`# Auto-generated by AirCoins portal server for %s
interface=%s
bind-dynamic
except-interface=lo
no-resolv
no-hosts
address=/#/%s
dhcp-range=%s,%s,%s
dhcp-option=3,%s
dhcp-option=6,%s
`, e.Interface, e.Interface, gateway, e.DHCPStart, e.DHCPEnd, e.Lease, gateway, gateway)

	configPath := fmt.Sprintf("/etc/dnsmasq.d/%s.conf", e.Interface)
	unitName := fmt.Sprintf("dnsmasq@%s", e.Interface)

	// Stop the existing service before writing a new config so the old
	// process doesn't hold stale file descriptors or conflict.
	if out, err := exec.Command("systemctl", "stop", unitName).CombinedOutput(); err != nil {
		// Non-fatal: service may not have been running
		log.Printf("startPortalDHCP: note: systemctl stop %s: %v — %s", unitName, err, string(out))
	}

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		err = fmt.Errorf("failed to write dnsmasq config for %s: %w", e.Interface, err)
		log.Printf("startPortalDHCP: %v", err)
		return err
	}

	if out, err := exec.Command("systemctl", "start", unitName).CombinedOutput(); err != nil {
		err = fmt.Errorf("failed to start %s: %w — %s", unitName, err, string(out))
		log.Printf("startPortalDHCP: %v", err)
		return err
	}

	if out, err := exec.Command("systemctl", "enable", unitName).CombinedOutput(); err != nil {
		// Non-fatal: service is running but won't survive reboot
		log.Printf("startPortalDHCP: warning: failed to enable %s: %v — %s", unitName, err, string(out))
	}

	log.Printf("startPortalDHCP: successfully started and enabled %s", unitName)
	return nil
}

// stopDHCP disables and stops the dnsmasq template instance and removes its config file.
func stopDHCP(iface string) {
	unitName := fmt.Sprintf("dnsmasq@%s", iface)

	if out, err := exec.Command("systemctl", "disable", unitName).CombinedOutput(); err != nil {
		log.Printf("stopDHCP: warning: failed to disable %s: %v — %s", unitName, err, string(out))
	}

	if out, err := exec.Command("systemctl", "stop", unitName).CombinedOutput(); err != nil {
		log.Printf("stopDHCP: warning: failed to stop %s: %v — %s", unitName, err, string(out))
	}

	configPath := fmt.Sprintf("/etc/dnsmasq.d/%s.conf", iface)
	if err := os.Remove(configPath); err != nil && !os.IsNotExist(err) {
		log.Printf("stopDHCP: warning: failed to remove dnsmasq config %s: %v", configPath, err)
	}

	log.Printf("stopDHCP: stopped and disabled %s", unitName)
}

// captiveRulesActive reports whether the AIRCOINS_CAPTIVE chain has
// capture rules for the interface.
func captiveRulesActive(iface string) bool {
	out, err := exec.Command("iptables", "-t", "nat", "-S", "AIRCOINS_CAPTIVE").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), fmt.Sprintf("-i %s ", iface))
}

// provisionPortal brings up the full portal stack for an entry: IP
// (immediate via ip + persistent via networkd), dnsmasq DHCP/DNS hijack,
// captive rules. Returns an error only for the fatal step (IP assignment);
// DHCP/captive problems are logged and visible in the status fields.
func provisionPortal(e portalConfigEntry) error {
	// Bring the interface up (best-effort; VLANs report DOWN until traffic)
	exec.Command("ip", "link", "set", e.Interface, "up").Run()

	// Own the portal IP: flush stale global addresses, add fresh
	exec.Command("ip", "addr", "flush", "dev", e.Interface, "scope", "global").Run()
	if out, err := exec.Command("ip", "addr", "add", e.IPCIDR, "dev", e.Interface).CombinedOutput(); err != nil {
		if !strings.Contains(string(out), "File exists") {
			return fmt.Errorf("failed to assign %s to %s: %s", e.IPCIDR, e.Interface, string(out))
		}
	}

	// Persist the static IP in a per-interface networkd file so networkd
	// owns it and won't flush it on reload. The `ip addr add` above stays
	// for immediate effect; this makes it stick.
	if err := writeNetworkdConfig(e.Interface, e.IPCIDR); err != nil {
		log.Printf("provisionPortal: warning: failed to write networkd config for %s: %v", e.Interface, err)
	}

	if err := startPortalDHCP(e); err != nil {
		log.Printf("provisionPortal: DHCP failed for %s: %v", e.Interface, err)
	}

	applyCaptiveRules("add", e.Interface, e.IPCIDR, e.AntiHotspot)
	return nil
}

// teardownPortal stops the portal stack on an interface: captive rules,
// dnsmasq (+ config), networkd file, portal IP. The interface itself
// (VLAN or physical) is left untouched.
func teardownPortal(e portalConfigEntry) {
	// Remove captive rules while the subnet is still resolvable
	applyCaptiveRules("del", e.Interface, e.IPCIDR, e.AntiHotspot)
	stopDHCP(e.Interface)
	removeNetworkdConfig(e.Interface)
	exec.Command("ip", "addr", "flush", "dev", e.Interface, "scope", "global").Run()
}

// healPortal verifies and repairs drift for an ENABLED portal: missing
// IP, missing networkd file, missing/outdated dnsmasq config, stopped
// dnsmasq, missing captive rules. Same auto-recovery philosophy as the
// old VLANList heal path.
func healPortal(e portalConfigEntry) {
	if _, err := os.Stat("/sys/class/net/" + e.Interface); err != nil {
		log.Printf("healPortal: interface %s not present; skipping", e.Interface)
		return
	}

	exec.Command("ip", "link", "set", e.Interface, "up").Run()

	// IP drift: interface lost the portal IP (or carries a different one)
	if getVLANIP(e.Interface) != e.IPCIDR {
		exec.Command("ip", "addr", "flush", "dev", e.Interface, "scope", "global").Run()
		if out, err := exec.Command("ip", "addr", "add", e.IPCIDR, "dev", e.Interface).CombinedOutput(); err != nil {
			log.Printf("healPortal: failed to re-add %s on %s: %s", e.IPCIDR, e.Interface, string(out))
		} else {
			log.Printf("healPortal: restored portal IP %s on %s", e.IPCIDR, e.Interface)
		}
	}

	// networkd file must exist so the IP survives `networkctl reload`
	netFile := fmt.Sprintf("/etc/systemd/network/04-aircoins-%s.network", e.Interface)
	if _, err := os.Stat(netFile); os.IsNotExist(err) {
		if err := writeNetworkdConfig(e.Interface, e.IPCIDR); err != nil {
			log.Printf("healPortal: warning: failed to write networkd config for %s: %v", e.Interface, err)
		} else {
			log.Printf("healPortal: created missing networkd config for %s", e.Interface)
		}
	}

	// dnsmasq config drift: missing file, pre-captive-portal content
	// (port=0 / no wildcard hijack), or a range that no longer matches
	// the saved portal config.
	confPath := fmt.Sprintf("/etc/dnsmasq.d/%s.conf", e.Interface)
	confBytes, err := os.ReadFile(confPath)
	conf := string(confBytes)
	wantRange := fmt.Sprintf("dhcp-range=%s,%s,%s", e.DHCPStart, e.DHCPEnd, e.Lease)
	needsRegen := err != nil ||
		!strings.Contains(conf, "bind-dynamic") ||
		!strings.Contains(conf, "address=/#/") ||
		strings.Contains(conf, "port=0") ||
		!strings.Contains(conf, wantRange)

	switch {
	case needsRegen:
		log.Printf("healPortal: dnsmasq config for %s missing or outdated; regenerating", e.Interface)
		if err := startPortalDHCP(e); err != nil {
			log.Printf("healPortal: failed to regenerate+restart DHCP for %s: %v", e.Interface, err)
		}
	case !isDHCPActive(e.Interface):
		unitName := fmt.Sprintf("dnsmasq@%s", e.Interface)
		log.Printf("healPortal: DHCP not running on %s; attempting auto-start", e.Interface)
		if out, err := exec.Command("systemctl", "start", unitName).CombinedOutput(); err != nil {
			log.Printf("healPortal: auto-start DHCP failed for %s: %v — %s", e.Interface, err, string(out))
		} else if out, err := exec.Command("systemctl", "enable", unitName).CombinedOutput(); err != nil {
			log.Printf("healPortal: warning: failed to enable %s: %v — %s", unitName, err, string(out))
		}
	}

	// Captive rules are idempotent — always (re)apply
	applyCaptiveRules("add", e.Interface, e.IPCIDR, e.AntiHotspot)
}

// portalStatus builds the live status view of a portal entry.
func portalStatus(e portalConfigEntry) models.PortalInfo {
	info := models.PortalInfo{
		Interface:   e.Interface,
		IPCIDR:      e.IPCIDR,
		DHCPStart:   e.DHCPStart,
		DHCPEnd:     e.DHCPEnd,
		DHCPLease:   e.Lease,
		Enabled:     e.Enabled,
		AntiHotspot: e.AntiHotspot,
	}

	if _, err := os.Stat("/sys/class/net/" + e.Interface); err != nil {
		return info
	}
	info.IfaceExists = true
	info.IfaceUp = getInterfaceState(e.Interface) == "up"
	info.IPOK = getVLANIP(e.Interface) == e.IPCIDR
	info.DHCPActive = isDHCPActive(e.Interface)
	info.CaptiveActive = captiveRulesActive(e.Interface)
	return info
}

// ============================================
// DB MIRROR HELPERS (warnings only, never fatal)
// ============================================

func upsertPortalDB(e portalConfigEntry) {
	_, err := models.DB.Exec(`
		INSERT INTO portal_servers (interface, portal_ip_cidr, dhcp_start, dhcp_end, dhcp_lease, enabled, anti_hotspot)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (interface) DO UPDATE SET
			portal_ip_cidr = EXCLUDED.portal_ip_cidr,
			dhcp_start = EXCLUDED.dhcp_start,
			dhcp_end = EXCLUDED.dhcp_end,
			dhcp_lease = EXCLUDED.dhcp_lease,
			enabled = EXCLUDED.enabled,
			anti_hotspot = EXCLUDED.anti_hotspot,
			updated_at = NOW()`,
		e.Interface, e.IPCIDR, e.DHCPStart, e.DHCPEnd, e.Lease, e.Enabled, e.AntiHotspot)
	if err != nil {
		log.Printf("Warning: failed to persist portal server to DB: %v", err)
	}
}

func deletePortalDB(iface string) {
	if _, err := models.DB.Exec("DELETE FROM portal_servers WHERE interface=$1", iface); err != nil {
		log.Printf("Warning: failed to remove portal server from DB: %v", err)
	}
}

// ============================================
// PORTAL LIST
// ============================================

// PortalList returns all portal servers with live status. Enabled
// portals are healed first (drift repair), so a GET doubles as the
// auto-recovery pass — same pattern the VLAN list used before the split.
func PortalList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	entries := readPortalConfig()
	portals := []models.PortalInfo{}
	for _, e := range entries {
		if e.Enabled {
			healPortal(e)
		}
		portals = append(portals, portalStatus(e))
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{"portals": portals})
}

// ============================================
// PORTAL CREATE / UPDATE
// ============================================

// PortalCreate provisions (or re-provisions) a portal server on an
// interface: portal IP, DHCP, DNS hijack, captive rules. Upsert keyed
// by interface.
func PortalCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req models.PortalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid JSON: " + err.Error()})
		return
	}

	if !validateIfaceName(req.Interface) {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid interface name"})
		return
	}
	if _, err := os.Stat("/sys/class/net/" + req.Interface); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Interface does not exist: " + req.Interface})
		return
	}

	// Refuse creating a portal on a bridge member interface (has a master bridge).
	// Bridge members are enslaved to a bridge and cannot host their own portal stack.
	if master := interfaceHasMaster(req.Interface); master != "" {
		sendJSON(w, http.StatusConflict, models.APIResponse{
			Success: false,
			Message: fmt.Sprintf("Interface %s is a bridge member of %s. Remove it from the bridge before creating a portal.", req.Interface, master),
		})
		return
	}

	ip, ipNet, err := net.ParseCIDR(req.IPCIDR)
	if err != nil || ip.To4() == nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid portal IP/CIDR (expected e.g. 10.0.22.1/24)"})
		return
	}
	if ip.Equal(ipNet.IP) {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Portal IP must be a host address, not the network address"})
		return
	}

	// Default / validate the DHCP range
	start, end := req.DHCPStart, req.DHCPEnd
	if start == "" || end == "" {
		defStart, defEnd := dhcpRangeFor(req.IPCIDR, start)
		if start == "" {
			start = defStart
		}
		if end == "" {
			end = defEnd
		}
	}
	if start == "" || end == "" {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Could not derive a DHCP range from " + req.IPCIDR})
		return
	}
	var bounds [2]uint32
	for i, addr := range []string{start, end} {
		parsed := net.ParseIP(addr)
		if parsed == nil || !ipNet.Contains(parsed) {
			sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "DHCP address outside portal subnet: " + addr})
			return
		}
		bounds[i] = ipToU32(parsed)
	}
	// An inverted range makes dnsmasq refuse to start, which would leave
	// the portal dead behind a 200 response.
	if bounds[0] > bounds[1] {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "DHCP start must be before DHCP end"})
		return
	}

	lease := strings.TrimSpace(req.DHCPLease)
	if lease == "" {
		lease = defaultDHCPLease
	}
	if strings.ContainsAny(lease, " \t\n,") {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid DHCP lease: " + lease})
		return
	}

	entry := portalConfigEntry{
		Interface:   req.Interface,
		IPCIDR:      req.IPCIDR,
		DHCPStart:   start,
		DHCPEnd:     end,
		Lease:       lease,
		Enabled:     true,
		AntiHotspot: req.AntiHotspot,
	}

	if err := provisionPortal(entry); err != nil {
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: err.Error()})
		return
	}

	writePortalConfigEntry(entry)
	upsertPortalDB(entry)

	sendJSON(w, http.StatusOK, map[string]interface{}{"success": true, "portal": portalStatus(entry)})
}

// ============================================
// PORTAL ENABLE / DISABLE
// ============================================

// decodePortalRef extracts the {interface} body used by enable/disable/delete.
func decodePortalRef(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req struct {
		Interface string `json:"interface"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid JSON: " + err.Error()})
		return "", false
	}
	if !validateIfaceName(req.Interface) {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid interface name"})
		return "", false
	}
	return req.Interface, true
}

// PortalEnable re-provisions and starts the portal stack from the saved config.
func PortalEnable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	iface, ok := decodePortalRef(w, r)
	if !ok {
		return
	}

	entry, found := findPortalEntry(iface)
	if !found {
		sendJSON(w, http.StatusNotFound, models.APIResponse{Success: false, Message: "No portal server configured on " + iface})
		return
	}

	entry.Enabled = true
	if err := provisionPortal(entry); err != nil {
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: err.Error()})
		return
	}

	writePortalConfigEntry(entry)
	upsertPortalDB(entry)

	sendJSON(w, http.StatusOK, map[string]interface{}{"success": true, "portal": portalStatus(entry)})
}

// PortalDisable stops the portal stack (DHCP, captive rules, IP) but
// keeps the saved config so it can be re-enabled with one call. The
// interface becomes a plain network again.
func PortalDisable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	iface, ok := decodePortalRef(w, r)
	if !ok {
		return
	}

	entry, found := findPortalEntry(iface)
	if !found {
		sendJSON(w, http.StatusNotFound, models.APIResponse{Success: false, Message: "No portal server configured on " + iface})
		return
	}

	teardownPortal(entry)
	entry.Enabled = false
	writePortalConfigEntry(entry)
	upsertPortalDB(entry)

	sendJSON(w, http.StatusOK, map[string]interface{}{"success": true, "portal": portalStatus(entry)})
}

// ============================================
// PORTAL DELETE
// ============================================

// PortalDelete tears down the portal stack and removes the saved config.
// The underlying interface (VLAN or physical) is untouched.
func PortalDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	iface, ok := decodePortalRef(w, r)
	if !ok {
		return
	}

	entry, found := findPortalEntry(iface)
	if !found {
		// Nothing on disk — still clear a stale DB mirror row
		deletePortalDB(iface)
		sendJSON(w, http.StatusNotFound, models.APIResponse{Success: false, Message: "No portal server configured on " + iface})
		return
	}

	teardownPortal(entry)
	removePortalConfigEntry(iface)
	deletePortalDB(iface)

	sendJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

// ============================================
// PORTAL UPDATE (EDIT)
// ============================================

// PortalUpdate edits an existing portal server's settings (IP, DHCP range,
// lease, anti-hotspot). It tears down the old stack and re-provisions with
// the new settings. The interface cannot be changed (use create+delete instead).
func PortalUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req models.PortalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid JSON: " + err.Error()})
		return
	}

	if !validateIfaceName(req.Interface) {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid interface name"})
		return
	}

	// Find the existing entry
	oldEntry, found := findPortalEntry(req.Interface)
	if !found {
		sendJSON(w, http.StatusNotFound, models.APIResponse{Success: false, Message: "No portal server configured on " + req.Interface})
		return
	}

	// Validate the new IP/CIDR
	ip, ipNet, err := net.ParseCIDR(req.IPCIDR)
	if err != nil || ip.To4() == nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid portal IP/CIDR (expected e.g. 10.0.22.1/24)"})
		return
	}
	if ip.Equal(ipNet.IP) {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Portal IP must be a host address, not the network address"})
		return
	}

	// Default / validate the DHCP range
	start, end := req.DHCPStart, req.DHCPEnd
	if start == "" || end == "" {
		defStart, defEnd := dhcpRangeFor(req.IPCIDR, start)
		if start == "" {
			start = defStart
		}
		if end == "" {
			end = defEnd
		}
	}
	if start == "" || end == "" {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Could not derive a DHCP range from " + req.IPCIDR})
		return
	}
	var bounds [2]uint32
	for i, addr := range []string{start, end} {
		parsed := net.ParseIP(addr)
		if parsed == nil || !ipNet.Contains(parsed) {
			sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "DHCP address outside portal subnet: " + addr})
			return
		}
		bounds[i] = ipToU32(parsed)
	}
	if bounds[0] > bounds[1] {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "DHCP start must be before DHCP end"})
		return
	}

	lease := strings.TrimSpace(req.DHCPLease)
	if lease == "" {
		lease = defaultDHCPLease
	}
	if strings.ContainsAny(lease, " \t\n,") {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid DHCP lease: " + lease})
		return
	}

	// Build the new entry (keep the same interface)
	newEntry := portalConfigEntry{
		Interface:   req.Interface,
		IPCIDR:      req.IPCIDR,
		DHCPStart:   start,
		DHCPEnd:     end,
		Lease:       lease,
		Enabled:     oldEntry.Enabled,
		AntiHotspot: req.AntiHotspot,
	}

	// Tear down old stack, provision with new settings
	teardownPortal(oldEntry)
	if newEntry.Enabled {
		if err := provisionPortal(newEntry); err != nil {
			// Revert to old entry on failure
			provisionPortal(oldEntry)
			sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: err.Error()})
			return
		}
	}

	writePortalConfigEntry(newEntry)
	upsertPortalDB(newEntry)

	sendJSON(w, http.StatusOK, map[string]interface{}{"success": true, "portal": portalStatus(newEntry)})
}

// ============================================
// STARTUP: Apply TTL=1 for all anti-hotspot portals
// ============================================

// ApplyAntiHotspotOnStartup reads the portal config and applies TTL=1
// mangle rules for every portal that has anti-hotspot enabled. This
// ensures TTL=1 is active immediately after the API restarts, without
// waiting for the admin to visit the portal page.
func ApplyAntiHotspotOnStartup() {
	entries := readPortalConfig()
	applied := 0
	for _, e := range entries {
		if e.AntiHotspot {
			applyTTL1(e.Interface)
			applied++
		}
	}
	if applied > 0 {
		log.Printf("ApplyAntiHotspotOnStartup: TTL=1 applied for %d portal(s)", applied)
	} else {
		log.Printf("ApplyAntiHotspotOnStartup: no portals with anti-hotspot enabled")
	}
}

// HealAllPortalsOnBoot iterates every ENABLED portal from portals.conf
// and calls healPortal to restore the full stack: IP, networkd file,
// dnsmasq config+service, and captive iptables rules. This ensures the
// captive portal survives reboots without requiring the admin to visit
// the portal page first.
func HealAllPortalsOnBoot() {
	entries := readPortalConfig()
	healed := 0
	for _, e := range entries {
		if !e.Enabled {
			continue
		}
		log.Printf("HealAllPortalsOnBoot: healing portal %s (%s)", e.Interface, e.IPCIDR)
		healPortal(e)
		healed++
	}
	if healed > 0 {
		log.Printf("HealAllPortalsOnBoot: healed %d portal(s)", healed)
	} else {
		log.Printf("HealAllPortalsOnBoot: no enabled portals to heal")
	}
}

// ============================================
// LEGACY MIGRATION
// ============================================

// MigrateLegacyPortalConfig converts pre-split flat files ONCE at API
// startup: every vlans.conf line that still carries an inline ip/cidr
// (the old all-in-one format) gets a portals.conf entry with the same
// derived DHCP range the old code used, then vlans.conf is rewritten in
// the new identity-only format. The user's working portal (e.g. end0.22,
// 10.0.22.1/24, DHCP .100-.200) therefore survives the upgrade with zero
// manual reconfiguration. The DB side is handled by migration 007.
func MigrateLegacyPortalConfig() {
	entries := readVLANConfig()

	existing := make(map[string]bool)
	for _, p := range readPortalConfig() {
		existing[p.Interface] = true
	}

	migrated := 0
	legacy := false
	for _, e := range entries {
		if e.LegacyIP == "" {
			continue
		}
		legacy = true

		iface := fmt.Sprintf("%s.%d", e.Interface, e.VLANID)
		if existing[iface] {
			continue
		}

		start, end := dhcpRangeFor(e.LegacyIP, e.LegacyStartIP)
		if start == "" || end == "" {
			log.Printf("MigrateLegacyPortalConfig: cannot derive DHCP range for %s (%s); skipping", iface, e.LegacyIP)
			continue
		}

		entry := portalConfigEntry{
			Interface: iface,
			IPCIDR:    e.LegacyIP,
			DHCPStart: start,
			DHCPEnd:   end,
			Lease:     defaultDHCPLease,
			Enabled:   true,
		}
		writePortalConfigEntry(entry)
		upsertPortalDB(entry)
		migrated++
		log.Printf("MigrateLegacyPortalConfig: migrated %s -> portal server (%s, DHCP %s-%s)", iface, e.LegacyIP, start, end)
	}

	if legacy {
		// Rewrite vlans.conf in the identity-only format (drops the
		// inline IP columns that now live in portals.conf)
		writeVLANConfig(entries)
		log.Printf("MigrateLegacyPortalConfig: rewrote %s in identity-only format (%d portal(s) migrated)", vlanConfigPath, migrated)
	}
}
