package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// payLockTimeout is the hard ceiling after which a held lock is forcibly
// released. Protects against a client that armed, walked away and never
// called /api/session/start — the next client would otherwise be stuck
// until the API process restarts.
// 120s covers the full arm→coins→done-paying window with headroom.
// The gen-counter mechanism prevents stale watchdogs from draining newer
// holders' semaphores.
const payLockTimeout = 120 * time.Second

// payLockEntry is one per-VLAN semaphore (buffered channel of size 1)
// together with the IP of the current holder for diagnostics, a ticket
// for the arm→start handoff, and a generation counter to prevent stale
// watchdogs from draining a newer holder's semaphore.
type payLockEntry struct {
	sem    chan struct{} // capacity 1: empty = free, full = held
	holder string        // IP of the current holder (empty when free)
	ticket string        // random token issued on Arm, validated on Start
	gen    uint64        // incremented on each acquire; watchdog checks gen
	mu     sync.Mutex    // protects holder + ticket + gen writes
}

// payLockRegistry is the process-wide map of per-VLAN locks. The API
// runs as a single process so no cross-process coordination is needed.
var payLockRegistry = struct {
	sync.Mutex
	entries map[string]*payLockEntry
}{entries: make(map[string]*payLockEntry)}

// getOrCreateEntry returns the lock entry for the given VLAN key,
// creating it on first use.
func getOrCreateEntry(vlanKey string) *payLockEntry {
	payLockRegistry.Lock()
	defer payLockRegistry.Unlock()
	e, ok := payLockRegistry.entries[vlanKey]
	if !ok {
		e = &payLockEntry{sem: make(chan struct{}, 1)}
		payLockRegistry.entries[vlanKey] = e
	}
	return e
}

// generateTicket produces a 16-hex-char random token using crypto/rand.
func generateTicket() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Fallback: extremely unlikely on any supported OS.
		log.Printf("paylock: crypto/rand failed: %v — using timestamp fallback", err)
		return hex.EncodeToString([]byte(time.Now().Format("150405.000")))
	}
	return hex.EncodeToString(b)
}

// PayLockTryAcquire attempts a non-blocking lock on the caller's VLAN.
// On success it generates a random ticket, stores it in the entry, and
// returns (true, "", vlanKey, ticket). The caller MUST pass the ticket
// to PayLockValidateAndRelease when done (e.g. in session Start).
//
// Returns (false, holderIP, vlanKey, "") when another client on the same
// VLAN already holds the lock.
//
// If the caller's IP cannot be resolved to a VLAN (e.g. admin from WAN),
// the lock is skipped and (true, "", "", "") is returned — unknown callers
// are never blocked, only known same-VLAN races are serialized.
func PayLockTryAcquire(clientIP string) (acquired bool, holder string, vlanKey string, ticket string) {
	vlan := resolveVLAN(clientIP)
	if vlan == "" {
		// Cannot resolve to a VLAN — let the request through.
		return true, "", "", ""
	}

	entry := getOrCreateEntry(vlan)
	select {
	case entry.sem <- struct{}{}:
		// Acquired. Record holder, generate ticket, bump generation, and
		// start a watchdog goroutine that force-releases after
		// payLockTimeout so a stuck client cannot wedge the VLAN.
		// The watchdog captures the current generation and only drains
		// if gen still matches at fire time — a stale watchdog from a
		// previous holder cannot steal the semaphore from a newer holder.
		t := generateTicket()
		entry.mu.Lock()
		entry.holder = clientIP
		entry.ticket = t
		entry.gen++
		myGen := entry.gen
		entry.mu.Unlock()

		go func(e *payLockEntry, key string, spawnGen uint64) {
			time.Sleep(payLockTimeout)
			e.mu.Lock()
			currentGen := e.gen
			e.mu.Unlock()
			if currentGen != spawnGen {
				// A newer holder acquired the lock after us — our
				// watchdog is stale; do nothing.
				return
			}
			select {
			case <-e.sem:
				// Drain succeeded — the holder didn't release in time.
				e.mu.Lock()
				old := e.holder
				e.holder = ""
				e.ticket = ""
				e.mu.Unlock()
				log.Printf("paylock: watchdog force-released VLAN %s (holder %s exceeded %v)", key, old, payLockTimeout)
			default:
				// Already released by the holder — nothing to do.
			}
		}(entry, vlan, myGen)

		return true, "", vlan, t
	default:
		// Semaphore is full — another client holds the lock.
		entry.mu.Lock()
		h := entry.holder
		entry.mu.Unlock()
		return false, h, vlan, ""
	}
}

// PayLockValidateAndRelease validates that the given ticket matches the
// stored ticket for the caller's VLAN, then releases the lock. This is
// the "start" side of the arm→start handoff.
//
// Returns:
//   - ("", true) on success (ticket matched, lock released)
//   - ("invalid_ticket", false) if ticket is missing or doesn't match
//   - ("lock_gone", false) if no lock is held for this VLAN (watchdog
//     released it, or lock was never acquired)
//   - ("no_vlan", true) if client IP doesn't resolve to a VLAN (pass-through)
func PayLockValidateAndRelease(clientIP, ticket string) (reason string, released bool) {
	vlan := resolveVLAN(clientIP)
	if vlan == "" {
		// No VLAN — pass through (same as TryAcquire's unresolvable path).
		return "no_vlan", true
	}

	payLockRegistry.Lock()
	entry, ok := payLockRegistry.entries[vlan]
	payLockRegistry.Unlock()
	if !ok {
		return "lock_gone", false
	}

	entry.mu.Lock()
	storedTicket := entry.ticket
	entry.mu.Unlock()

	if ticket == "" || storedTicket == "" || ticket != storedTicket {
		return "invalid_ticket", false
	}

	// Ticket matches — drain the semaphore and clear state.
	select {
	case <-entry.sem:
		entry.mu.Lock()
		entry.holder = ""
		entry.ticket = ""
		entry.mu.Unlock()
		return "", true
	default:
		// Watchdog already drained it — ticket was valid but lock is gone.
		entry.mu.Lock()
		entry.ticket = ""
		entry.mu.Unlock()
		log.Printf("paylock: ValidateAndRelease default case — watchdog likely already drained VLAN %s (client %s)", vlan, clientIP)
		return "lock_gone", false
	}
}

// payLockCurrentTicket returns the current ticket stored in the lock
// entry for the given VLAN key. Returns "" if the entry doesn't exist
// or has no ticket. Used by the Arm handler to re-issue the existing
// ticket when the same client re-arms (double-tap protection).
func payLockCurrentTicket(vlanKey string) string {
	if vlanKey == "" {
		return ""
	}
	payLockRegistry.Lock()
	entry, ok := payLockRegistry.entries[vlanKey]
	payLockRegistry.Unlock()
	if !ok {
		return ""
	}
	entry.mu.Lock()
	t := entry.ticket
	entry.mu.Unlock()
	return t
}

// PayLockRelease releases the per-VLAN lock unconditionally (used by the
// can-start peek and by watchdog-free paths). If the watchdog already
// drained the semaphore this is a harmless no-op (logged for post-mortem).
//
// Accepts the pre-resolved vlanKey from TryAcquire to avoid a second
// `ip route get` syscall. If vlanKey is empty the caller was unresolvable
// and no lock was taken.
func PayLockRelease(clientIP, vlanKey string) {
	if vlanKey == "" {
		return
	}

	payLockRegistry.Lock()
	entry, ok := payLockRegistry.entries[vlanKey]
	payLockRegistry.Unlock()
	if !ok {
		return
	}

	select {
	case <-entry.sem:
		// Successfully drained — we were still the holder.
		entry.mu.Lock()
		entry.holder = ""
		entry.ticket = ""
		entry.mu.Unlock()
	default:
		// Watchdog already drained it — log for post-mortem.
		log.Printf("paylock: Release default case — watchdog likely already drained VLAN %s (client %s)", vlanKey, clientIP)
	}
}

// resolveVLAN returns the gateway interface name (e.g. "end0.22") for
// the given client IP by parsing `ip -o route get <ip>`. Returns ""
// when the IP cannot be resolved (loopback, WAN admin, unreachable).
// If the route goes through a gateway hop ("via" token before "dev"),
// the output interface is the gateway's outgoing interface, not the
// client's VLAN — return "" in that case.
func resolveVLAN(ip string) string {
	// `ip -o route get <ip>` outputs a single line like:
	//   10.0.22.5 dev end0.22 table 100 src 10.0.22.1 uid 0
	// or for gateway-routed:
	//   10.0.22.5 via 10.0.0.1 dev eth0 table 100 src 10.0.0.1 uid 0
	// We extract the field after "dev", but only if no "via" precedes it.
	out, err := exec.Command("ip", "-o", "route", "get", ip).Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "via" {
			// Gateway-routed — the dev is the gateway's outgoing iface,
			// not the client's VLAN. Let the request through without locking.
			return ""
		}
		if f == "dev" && i+1 < len(fields) {
			dev := fields[i+1]
			// Ignore loopback — it's not a real VLAN.
			if dev == "lo" {
				return ""
			}
			return dev
		}
	}
	return ""
}
