package handlers

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// ============================================
// CLIENT IDENTIFICATION (IP -> MAC)
// ============================================
// The portal endpoints are reached through lighttpd, which proxies /api/*
// to this process from 127.0.0.1. lighttpd sets X-Forwarded-For
// (proxy.forwarded, lighttpd >= 1.4.56), so the paying device is
// identified by that header first and by RemoteAddr only as a fallback.
// The MAC comes from the kernel neighbour table — clients sit on directly
// attached VLAN subnets, so the gateway always has (or can populate) an
// ARP entry for them.

// captiveRulesBin opens/closes internet access per client MAC
// (subcommands: auth <mac> / unauth <mac>).
const captiveRulesBin = "/usr/local/bin/aircoins-captive-rules"

var macRegexp = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)

// generateSessionToken returns 8 hex chars (4 random bytes) for the
// per-device session token used in MAC-randomization roaming.
func generateSessionToken() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand.Read on Linux never fails in practice; fall back
		// to a deterministic value so the caller always gets a token.
		log.Printf("generateSessionToken: crypto/rand failed: %v", err)
		return "00000000"
	}
	return hex.EncodeToString(b)
}

// clientIPFromRequest resolves the real client IP of an HTTP request:
// first hop of X-Forwarded-For when present (requests come through
// lighttpd), else X-Real-IP, else the RemoteAddr host.
func clientIPFromRequest(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first := strings.TrimSpace(strings.Split(xff, ",")[0])
		if net.ParseIP(first) != nil {
			return first
		}
	}

	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		if net.ParseIP(realIP) != nil {
			return realIP
		}
	}

	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// normalizeMAC lowercases a MAC, converts dash separators to colons and
// rejects anything that is not a plausible unicast hardware address.
func normalizeMAC(mac string) string {
	mac = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(mac, "-", ":")))
	if !macRegexp.MatchString(mac) {
		return ""
	}
	if mac == "00:00:00:00:00:00" || mac == "ff:ff:ff:ff:ff:ff" {
		return ""
	}
	return mac
}

// neighborMAC looks the IP up in the kernel neighbour table without
// probing. Cheap enough for the portal status poll.
func neighborMAC(ip string) string {
	if net.ParseIP(ip) == nil {
		return ""
	}

	// Preferred: ip neigh show <ip>  ->  "<ip> dev end0.22 lladdr aa:bb:.. REACHABLE"
	if out, err := exec.Command("ip", "neigh", "show", ip).Output(); err == nil {
		fields := strings.Fields(string(out))
		for i, f := range fields {
			if f == "lladdr" && i+1 < len(fields) {
				if mac := normalizeMAC(fields[i+1]); mac != "" {
					return mac
				}
			}
		}
	}

	// Fallback: /proc/net/arp ("IP address  HW type  Flags  HW address ...")
	if data, err := os.ReadFile("/proc/net/arp"); err == nil {
		for _, line := range strings.Split(string(data), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) >= 4 && fields[0] == ip {
				if mac := normalizeMAC(fields[3]); mac != "" {
					return mac
				}
			}
		}
	}

	return ""
}

// resolveClientMAC resolves an IP to its MAC, retrying once after a ping
// probe: a single echo request repopulates the ARP entry because the
// clients are on directly attached VLANs.
func resolveClientMAC(ip string) string {
	if mac := neighborMAC(ip); mac != "" {
		return mac
	}
	if net.ParseIP(ip) == nil {
		return ""
	}
	exec.Command("ping", "-c1", "-W1", ip).Run()
	return neighborMAC(ip)
}

// ============================================
// CAPTIVE PORTAL AUTH/UNAUTH
// ============================================

// runCaptiveRules executes `aircoins-captive-rules auth|unauth <mac>` and
// logs the outcome with the MAC and the reason (start / extend / expire /
// admin-terminate / startup-recovery / ...). A failure is NON-FATAL: dev
// machines have no iptables layer, and the session row must exist either
// way so the operator can see what happened.
//
// Anti-hotspot needs NO per-session work here: TTL=1 is stamped on the
// download path (mangle POSTROUTING, see applyTTL1), which a directly
// connected client receives fine — only hotspot/tethering routers drop
// it. There is no per-MAC bypass to add or remove.
func runCaptiveRules(db *sql.DB, action, mac, reason string) {
	mac = normalizeMAC(mac)
	if mac == "" {
		log.Printf("captive %s skipped (%s): no MAC known", action, reason)
		return
	}

	out, err := exec.Command(captiveRulesBin, action, mac).CombinedOutput()
	output := strings.TrimSpace(string(out))
	if err != nil {
		log.Printf("captive %s %s (%s) failed: %v — %s", action, mac, reason, err, output)
		if db != nil {
			logAction(db, "WARN", "captive", action+" "+mac+" ("+reason+") failed: "+err.Error())
		}
		return
	}

	log.Printf("captive %s %s (%s): %s", action, mac, reason, output)
	if db != nil {
		logAction(db, "INFO", "captive", action+" "+mac+" ("+reason+")")
	}
}
