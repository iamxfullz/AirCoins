package handlers

import (
	"aircoins-api/models"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// qdisc.go implements traffic shaping (CAKE / FQ_CODEL) on portal
// interfaces. Rules are persisted in system_settings under key
// 'portal_qdisc_rules' as a JSON map keyed by interface name. The Apply
// endpoint runs the actual `tc` commands on the live interface.

// ============================================
// TYPES
// ============================================

// QdiscRule is the per-interface shaping config stored in the DB.
type QdiscRule struct {
	Qdisc           string `json:"qdisc"`              // "cake" | "fq_codel"
	GlobalBwMbps    int    `json:"global_bw_mbps"`     // total cap, > 0
	PerDeviceBwMbps int    `json:"per_device_bw_mbps"` // 0 = global only
}

// QdiscHandler serves the admin qdisc endpoints.
type QdiscHandler struct {
	DB *sql.DB
	// mu serialises all apply operations so a double-click Apply Now
	// cannot fork two concurrent tc streams that corrupt the HTB tree.
	mu sync.Mutex
}

const qdiscSettingKey = "portal_qdisc_rules"
const qdiscSettingDesc = "Per-interface traffic shaping rules (CAKE / FQ_CODEL) as a JSON map keyed by interface"

// allowed qdisc types
var allowedQdiscs = map[string]bool{
	"cake":     true,
	"fq_codel": true,
}

// macRegex is declared in taprules.go (same package).

func isValidMAC(mac string) bool { return macRegex.MatchString(mac) }

// macToClassID derives a stable HTB class ID minor number from a MAC address.
// Uses FNV-1a hash mod 65534 + 2 so that:
//   - class IDs 0 and 1 are reserved (HTB root and default class)
//   - the result stays within HTB's 16-bit minor limit (65535)
//
// The same MAC always maps to the same classID so add and remove are symmetric.
func macToClassID(mac string) int {
	h := fnv.New32a()
	h.Write([]byte(strings.ToLower(mac)))
	return int(h.Sum32()%65534) + 2
}

// ============================================
// LOAD / SAVE HELPERS
// ============================================

// loadQdiscRules reads the full portal_qdisc_rules map from system_settings.
func loadQdiscRules(db *sql.DB) map[string]QdiscRule {
	var raw string
	err := db.QueryRow("SELECT value FROM system_settings WHERE key=$1", qdiscSettingKey).Scan(&raw)
	if err != nil {
		return make(map[string]QdiscRule)
	}
	var rules map[string]QdiscRule
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		log.Printf("loadQdiscRules: unmarshal failed: %v (returning empty)", err)
		return make(map[string]QdiscRule)
	}
	return rules
}

// saveQdiscRules persists the full map to system_settings.
func saveQdiscRules(db *sql.DB, rules map[string]QdiscRule) error {
	b, err := json.Marshal(rules)
	if err != nil {
		return err
	}
	return upsertSetting(db, qdiscSettingKey, string(b), qdiscSettingDesc)
}

// portalInterfaces returns the list of interfaces that have a portal
// server provisioned (from the flat-file source of truth).
func portalInterfaces() []string {
	entries := readPortalConfig()
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Interface)
	}
	return out
}

// portalInterfaceSet returns a set of provisioned portal interface names.
func portalInterfaceSet() map[string]bool {
	entries := readPortalConfig()
	s := make(map[string]bool, len(entries))
	for _, e := range entries {
		s[e.Interface] = true
	}
	return s
}

// ============================================
// GET /api/admin/portal/qdisc
// ============================================

// GetQdisc returns the saved qdisc rules for all portal interfaces.
func (h *QdiscHandler) GetQdisc(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	all := loadQdiscRules(h.DB)

	// Filter to only portal interfaces
	portalIfaces := portalInterfaces()
	filtered := make(map[string]QdiscRule, len(portalIfaces))
	for _, iface := range portalIfaces {
		if rule, ok := all[iface]; ok {
			filtered[iface] = rule
		}
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"data": map[string]interface{}{
			"interfaces": filtered,
		},
	})
}

// ============================================
// POST /api/admin/portal/qdisc
// ============================================

// SaveQdisc persists (upserts) a qdisc rule for one interface.
func (h *QdiscHandler) SaveQdisc(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Interface       string `json:"interface"`
		Qdisc           string `json:"qdisc"`
		GlobalBwMbps    int    `json:"global_bw_mbps"`
		PerDeviceBwMbps int    `json:"per_device_bw_mbps"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid JSON: " + err.Error()})
		return
	}

	// Validate interface
	if !validateIfaceName(req.Interface) {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid interface name"})
		return
	}
	portals := portalInterfaceSet()
	if !portals[req.Interface] {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{
			Success: false,
			Message: "Interface " + req.Interface + " has no portal provisioned",
		})
		return
	}

	// Validate qdisc type
	if !allowedQdiscs[req.Qdisc] {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{
			Success: false,
			Message: "qdisc must be 'cake' or 'fq_codel'",
		})
		return
	}

	// Validate bandwidths
	if req.GlobalBwMbps <= 0 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "global_bw_mbps must be > 0"})
		return
	}
	if req.PerDeviceBwMbps < 0 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "per_device_bw_mbps must be >= 0"})
		return
	}

	// Warn if CAKE + per-device (cake ignores it)
	if req.Qdisc == "cake" && req.PerDeviceBwMbps > 0 {
		log.Printf("qdisc save: warning — CAKE ignores per_device_bw_mbps (%d) on %s", req.PerDeviceBwMbps, req.Interface)
	}

	// Finding #7: warn when CAKE at high bandwidth may saturate the H3 CPU.
	if req.Qdisc == "cake" && req.GlobalBwMbps > 200 {
		log.Printf("qdisc save: WARNING — CAKE at %d Mbps on %s may overload the Orange Pi H3 CPU", req.GlobalBwMbps, req.Interface)
	}

	rule := QdiscRule{
		Qdisc:           req.Qdisc,
		GlobalBwMbps:    req.GlobalBwMbps,
		PerDeviceBwMbps: req.PerDeviceBwMbps,
	}

	all := loadQdiscRules(h.DB)
	all[req.Interface] = rule
	if err := saveQdiscRules(h.DB, all); err != nil {
		log.Printf("SaveQdisc: persist failed: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to save qdisc rules"})
		return
	}

	logAction(h.DB, "INFO", "portal", fmt.Sprintf("Qdisc rule saved for %s: %s global=%dMbps per_device=%dMbps",
		req.Interface, req.Qdisc, req.GlobalBwMbps, req.PerDeviceBwMbps))

	sendJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": rule})
}

// ============================================
// POST /api/admin/portal/qdisc/apply
// ============================================

// ApplyQdisc runs the live `tc` commands on one (or all saved) interfaces.
func (h *QdiscHandler) ApplyQdisc(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check that tc is available before doing anything
	if _, err := exec.LookPath("tc"); err != nil {
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{
			Success: false,
			Message: "tc command not found — install iproute2 to use traffic shaping",
		})
		return
	}

	// Serialize all apply operations to prevent concurrent tc fork/exec
	// streams that could leave the interface with a partial HTB tree.
	h.mu.Lock()
	defer h.mu.Unlock()

	var req struct {
		Interface string `json:"interface"`
	}
	// Empty body = apply all; partial body with interface = apply one
	_ = json.NewDecoder(r.Body).Decode(&req)

	all := loadQdiscRules(h.DB)
	if len(all) == 0 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "No qdisc rules saved"})
		return
	}

	if req.Interface != "" {
		// Validate interface name (same check SaveQdisc performs).
		if !validateIfaceName(req.Interface) {
			sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid interface name: " + req.Interface})
			return
		}
		// Check the interface exists before attempting tc commands
		if out, err := exec.Command("ip", "link", "show", req.Interface).CombinedOutput(); err != nil {
			sendJSON(w, http.StatusInternalServerError, models.APIResponse{
				Success: false,
				Message: fmt.Sprintf("Interface %s not found or down: %s — %s", req.Interface, err, strings.TrimSpace(string(out))),
			})
			return
		}
		// Apply a single interface
		rule, ok := all[req.Interface]
		if !ok {
			sendJSON(w, http.StatusNotFound, models.APIResponse{
				Success: false,
				Message: "No saved qdisc rule for " + req.Interface,
			})
			return
		}
		partialFailures, err := applyQdiscRule(h.DB, req.Interface, rule)
		if err != nil {
			sendJSON(w, http.StatusInternalServerError, models.APIResponse{
				Success: false,
				Message: fmt.Sprintf("Failed to apply qdisc on %s: %v", req.Interface, err),
			})
			return
		}
		logAction(h.DB, "INFO", "portal", "Qdisc applied on "+req.Interface)
		if len(partialFailures) > 0 {
			sendJSON(w, http.StatusOK, map[string]interface{}{
				"success":          false,
				"applied":          true,
				"interface":        req.Interface,
				"partial_failures": partialFailures,
			})
			return
		}
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"success":   true,
			"applied":   true,
			"interface": req.Interface,
		})
		return
	}

	// Apply all saved interfaces — but only those with a provisioned portal
	// (same filter that ApplyQdiscOnBoot uses), not every saved rule.
	portals := portalInterfaceSet()
	var applied []string
	var errors []string
	var allPartialFailures []string
	for iface, rule := range all {
		if !portals[iface] {
			continue
		}
		if !validateIfaceName(iface) {
			errors = append(errors, iface+": invalid interface name")
			continue
		}
		partialFailures, err := applyQdiscRule(h.DB, iface, rule)
		if err != nil {
			errors = append(errors, iface+": "+err.Error())
		} else {
			applied = append(applied, iface)
			logAction(h.DB, "INFO", "portal", "Qdisc applied on "+iface)
			for _, pf := range partialFailures {
				allPartialFailures = append(allPartialFailures, iface+"/"+pf)
			}
		}
	}

	if len(errors) > 0 {
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{
			Success: false,
			Message: "Qdisc apply failed: " + strings.Join(errors, "; "),
		})
		return
	}

	if len(allPartialFailures) > 0 {
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"success":          false,
			"applied":          true,
			"applied_ifaces":   applied,
			"partial_failures": allPartialFailures,
		})
		return
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"applied": applied,
	})
}

// ============================================
// TC COMMAND EXECUTION
// ============================================

// runTC executes a tc command and returns a combined error with output on failure.
func runTC(args ...string) error {
	cmd := exec.Command("tc", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tc %s: %v — %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// wipeQdisc removes any existing root qdisc on the interface.
// Ignores "Cannot find device" / "RTNETLINK" errors (nothing to wipe).
func wipeQdisc(iface string) {
	cmd := exec.Command("tc", "qdisc", "del", "dev", iface, "root")
	if out, err := cmd.CombinedOutput(); err != nil {
		s := string(out)
		if !strings.Contains(s, "Cannot find device") &&
			!strings.Contains(s, "RTNETLINK answers: No such file or directory") &&
			!strings.Contains(s, "RTNETLINK answers: Invalid argument") {
			log.Printf("wipeQdisc(%s): note: %v — %s", iface, err, strings.TrimSpace(s))
		}
	}
}

// verifyQdisc runs `tc -s qdisc show dev <iface>` and returns the output.
// Used to verify the qdisc was applied; on failure the output is in the error.
func verifyQdisc(iface string) (string, error) {
	cmd := exec.Command("tc", "-s", "qdisc", "show", "dev", iface)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("tc verify on %s: %v — %s", iface, err, s)
	}
	return s, nil
}

// applyQdiscRule runs the full tc recipe for a single interface+rule.
// For per-device FQ_CODEL it returns any per-MAC partial failures.
func applyQdiscRule(db *sql.DB, iface string, rule QdiscRule) ([]string, error) {
	// 1. Wipe existing qdisc
	wipeQdisc(iface)

	switch rule.Qdisc {
	case "cake":
		return nil, applyCAKE(iface, rule)
	case "fq_codel":
		if rule.PerDeviceBwMbps > 0 {
			return applyFQCodelPerDevice(db, iface, rule)
		}
		return nil, applyFQCodelGlobal(iface, rule)
	default:
		return nil, fmt.Errorf("unknown qdisc type: %s", rule.Qdisc)
	}
}

// applyCAKE: tc qdisc add dev <iface> root cake bandwidth <X>Mbit
// CAKE handles fairness internally; per_device_bw is ignored (logged above).
func applyCAKE(iface string, rule QdiscRule) error {
	bwArg := fmt.Sprintf("%dMbit", rule.GlobalBwMbps)
	if err := runTC("qdisc", "add", "dev", iface, "root", "cake", "bandwidth", bwArg); err != nil {
		return err
	}
	_, verr := verifyQdisc(iface)
	return verr
}

// applyFQCodelGlobal: HTB root with a single default class at global rate,
// fq_codel leaf qdisc on that class.
//
//	tc qdisc add dev <iface> root handle 1: htb default 1
//	tc class add dev <iface> parent 1: classid 1:1 htb rate <X>Mbit ceil <X>Mbit
//	tc qdisc add dev <iface> parent 1:1 fq_codel
func applyFQCodelGlobal(iface string, rule QdiscRule) error {
	bwArg := fmt.Sprintf("%dMbit", rule.GlobalBwMbps)

	if err := runTC("qdisc", "add", "dev", iface, "root", "handle", "1:", "htb", "default", "1"); err != nil {
		return err
	}
	if err := runTC("class", "add", "dev", iface, "parent", "1:", "classid", "1:1",
		"htb", "rate", bwArg, "ceil", bwArg); err != nil {
		return err
	}
	if err := runTC("qdisc", "add", "dev", iface, "parent", "1:1", "fq_codel"); err != nil {
		return err
	}
	_, verr := verifyQdisc(iface)
	return verr
}

// applyFQCodelPerDevice: HTB root with a default class at global rate +
// per-MAC classes at per_device rate for each active session on this iface.
//
//	tc qdisc add dev <iface> root handle 1: htb default 1
//	tc class add dev <iface> parent 1: classid 1:1 htb rate <global>Mbit ceil <global>Mbit
//	tc qdisc add dev <iface> parent 1:1 fq_codel
//	# For each active session MAC (egress = Pi→client, so match dst MAC):
//	tc class add dev <iface> parent 1: classid 1:<N> htb rate <per_device>Mbit ceil <per_device>Mbit
//	tc qdisc add dev <iface> parent 1:<N> fq_codel
//	tc filter add dev <iface> parent 1: protocol ip u32 match ether dst <mac> flowid 1:<N>
func applyFQCodelPerDevice(db *sql.DB, iface string, rule QdiscRule) ([]string, error) {
	globalArg := fmt.Sprintf("%dMbit", rule.GlobalBwMbps)
	perDevArg := fmt.Sprintf("%dMbit", rule.PerDeviceBwMbps)

	if err := runTC("qdisc", "add", "dev", iface, "root", "handle", "1:", "htb", "default", "1"); err != nil {
		return nil, err
	}
	if err := runTC("class", "add", "dev", iface, "parent", "1:", "classid", "1:1",
		"htb", "rate", globalArg, "ceil", globalArg); err != nil {
		return nil, err
	}
	if err := runTC("qdisc", "add", "dev", iface, "parent", "1:1", "fq_codel"); err != nil {
		return nil, err
	}

	// Fetch active sessions on this interface
	macs := activeSessionMACs(db, iface)
	var partialFailures []string

	for _, mac := range macs {
		// Use a stable classID derived from the MAC so add and remove are symmetric.
		classID := macToClassID(mac)
		if err := addClientClass(iface, mac, classID, perDevArg); err != nil {
			log.Printf("applyFQCodelPerDevice: failed to add class for %s on %s: %v", mac, iface, err)
			partialFailures = append(partialFailures, fmt.Sprintf("%s: %v", mac, err))
			// Continue with remaining MACs — one failure shouldn't block all
		}
	}

	_, verr := verifyQdisc(iface)
	return partialFailures, verr
}

// ensureIngressQdisc adds an ingress qdisc to the interface if not already present.
func ensureIngressQdisc(iface string) {
	// Simple check: see if ingress is already added
	cmd := exec.Command("tc", "qdisc", "show", "dev", iface, "ingress")
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "ingress ffff:") {
		runTC("qdisc", "add", "dev", iface, "handle", "ffff:", "ingress")
	}
}

// addClientClass creates a per-MAC HTB class + fq_codel leaf + u32 filter.
// The MAC is validated before being passed to tc; invalid MACs are rejected.
func addClientClass(iface, mac string, classID int, rateArg string) error {
	if !isValidMAC(mac) {
		return fmt.Errorf("invalid MAC format: %q — skipping tc filter", mac)
	}
	cid := fmt.Sprintf("1:%x", classID)
	if err := runTC("class", "add", "dev", iface, "parent", "1:", "classid", cid,
		"htb", "rate", rateArg, "ceil", rateArg); err != nil {
		return err
	}
	if err := runTC("qdisc", "add", "dev", iface, "parent", cid, "fq_codel"); err != nil {
		return err
	}
	// Download (Egress)
	if err := runTC("filter", "add", "dev", iface, "parent", "1:", "protocol", "ip",
		"u32", "match", "ether", "dst", mac,
		"flowid", cid); err != nil {
		return err
	}
	
	// Upload (Ingress Police)
	ensureIngressQdisc(iface)
	// Apply ingress policing (rate limit) to match upload speed
	runTC("filter", "add", "dev", iface, "parent", "ffff:", "protocol", "ip",
		"u32", "match", "ether", "src", mac,
		"action", "police", "rate", rateArg, "burst", "10k", "drop")
	return nil
}

// delClientClass removes a per-MAC class (best-effort — errors are logged).
// The MAC is validated; invalid MACs are skipped with a warning.
func delClientClass(iface, mac string, classID int) {
	if !isValidMAC(mac) {
		log.Printf("delClientClass: invalid MAC format %q on %s — skipping", mac, iface)
		return
	}
	cid := fmt.Sprintf("1:%x", classID)
	// Delete egress filter
	exec.Command("tc", "filter", "del", "dev", iface, "parent", "1:", "protocol", "ip",
		"u32", "match", "ether", "dst", mac, "flowid", cid).Run()
	
	// Delete ingress filter (police)
	exec.Command("tc", "filter", "del", "dev", iface, "parent", "ffff:", "protocol", "ip",
		"u32", "match", "ether", "src", mac).Run()

	exec.Command("tc", "qdisc", "del", "dev", iface, "parent", cid).Run()
	exec.Command("tc", "class", "del", "dev", iface, "parent", "1:", "classid", cid).Run()
}

// activeSessionMACs returns distinct MACs of active sessions whose client
// IP is on the given interface's subnet (matching portal_interfaces).

// ChangeClientClassRate updates the per-MAC HTB class rate on a live
// interface. If the class does not exist (e.g. the session just started
// before the class was added), it falls back to `tc class add`.
func ChangeClientClassRate(iface, mac string, mbps int) error {
	if !isValidMAC(mac) {
		return fmt.Errorf("invalid MAC format: %q", mac)
	}
	classID := macToClassID(mac)
	cid := fmt.Sprintf("1:%x", classID)
	rateArg := fmt.Sprintf("%dMbit", mbps)

	// Try `tc class change` first (the class should already exist from
	// addClientClass / EnsurePerDeviceClass).
	err := runTC("class", "change", "dev", iface, "parent", "1:", "classid", cid,
		"htb", "rate", rateArg, "ceil", rateArg)
	if err == nil {
		return nil
	}
	// Fall back to `tc class add` if the class was missing.
	log.Printf("ChangeClientClassRate: change failed for %s on %s (%v), trying add", mac, iface, err)
	return runTC("class", "add", "dev", iface, "parent", "1:", "classid", cid,
		"htb", "rate", rateArg, "ceil", rateArg)
}

func activeSessionMACs(db *sql.DB, iface string) []string {
	// Get the portal's subnet from portal_servers
	var cidr string
	err := db.QueryRow("SELECT portal_ip_cidr FROM portal_servers WHERE interface=$1", iface).Scan(&cidr)
	if err != nil {
		log.Printf("activeSessionMACs: cannot find portal for %s: %v", iface, err)
		return nil
	}

	// Extract the subnet prefix from the CIDR (e.g. "10.0.22.1/24" → "10.0.22.")
	parts := strings.SplitN(cidr, "/", 2)
	if len(parts) != 2 {
		return nil
	}
	ipParts := strings.Split(parts[0], ".")
	if len(ipParts) != 4 {
		return nil
	}
	prefix := ipParts[0] + "." + ipParts[1] + "." + ipParts[2] + "."

	rows, err := db.Query(`
		SELECT DISTINCT client_mac FROM sessions
		WHERE status = 'active' AND expires_at > NOW()
		  AND client_mac IS NOT NULL AND client_mac <> '' AND client_mac <> '-'
		  AND client_ip LIKE $1
	`, prefix+"%")
	if err != nil {
		log.Printf("activeSessionMACs: query failed: %v", err)
		return nil
	}
	defer rows.Close()

	var macs []string
	for rows.Next() {
		var mac string
		if err := rows.Scan(&mac); err != nil {
			continue
		}
		macs = append(macs, mac)
	}
	return macs
}

// ============================================
// APPLY-ON-BOOT
// ============================================

// ApplyQdiscOnBoot re-applies saved qdisc rules to all portal interfaces
// that have them. Called from main.go after the DB is up. Failures are
// logged but never fatal — the portal still works without shaping.
// Returns the list of interfaces that failed so the caller can retry.
func ApplyQdiscOnBoot(db *sql.DB) (failed []string, succeeded []string) {
	if _, err := exec.LookPath("tc"); err != nil {
		log.Printf("ApplyQdiscOnBoot: tc not found — skipping qdisc re-apply (install iproute2)")
		return nil, nil
	}

	rules := loadQdiscRules(db)
	if len(rules) == 0 {
		return nil, nil
	}

	portals := portalInterfaceSet()
	for iface, rule := range rules {
		if !portals[iface] {
			log.Printf("ApplyQdiscOnBoot: %s has saved qdisc rules but no portal — skipping", iface)
			continue
		}
		// Only apply if the interface actually exists (VLAN may be down)
		if _, err := exec.Command("ip", "link", "show", iface).CombinedOutput(); err != nil {
			log.Printf("ApplyQdiscOnBoot: %s does not exist — skipping", iface)
			failed = append(failed, iface)
			continue
		}
		if _, err := applyQdiscRule(db, iface, rule); err != nil {
			log.Printf("ApplyQdiscOnBoot: failed to apply qdisc on %s: %v", iface, err)
			failed = append(failed, iface)
		} else {
			log.Printf("ApplyQdiscOnBoot: qdisc re-applied on %s (%s, global=%dMbps, per_device=%dMbps)",
				iface, rule.Qdisc, rule.GlobalBwMbps, rule.PerDeviceBwMbps)
			succeeded = append(succeeded, iface)
		}
	}
	return failed, succeeded
}

// ApplyQdiscOnBootWithRetry wraps ApplyQdiscOnBoot in a goroutine so it
// does not block the HTTP listener startup, and adds a 30 s delayed retry
// pass for any interfaces whose first apply failed (e.g. VLAN not yet up).
// Called from main.go.
func ApplyQdiscOnBootWithRetry(db *sql.DB) {
	go func() {
		failed, _ := ApplyQdiscOnBoot(db)
		if len(failed) == 0 {
			return
		}
		// Retry pass: wait 30 s for VLAN interfaces to come up, then
		// re-apply any interfaces that failed the first attempt.
		log.Printf("ApplyQdiscOnBoot: %d interface(s) failed — retrying in 30s: %v", len(failed), failed)
		time.Sleep(30 * time.Second)
		log.Printf("ApplyQdiscOnBoot: retrying %d failed interface(s)", len(failed))
		ApplyQdiscOnBoot(db) // re-runs all; already-succeeded ones are idempotent
	}()
}

// ============================================
// PER-DEVICE SESSION-LIFECYCLE HOOKS
// ============================================

// ifaceForClientIP returns the portal interface whose subnet contains the
// given client IP.  It matches the first three octets (a /24 heuristic that
// matches how portal subnets are provisioned).  Returns "" when no portal
// matches — the caller should treat that as a no-op.
func ifaceForClientIP(db *sql.DB, clientIP string) string {
	ip := net.ParseIP(clientIP)
	if ip == nil {
		return ""
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return ""
	}
	prefix := fmt.Sprintf("%d.%d.%d.", ip4[0], ip4[1], ip4[2])

	rows, err := db.Query("SELECT interface, portal_ip_cidr FROM portal_servers")
	if err != nil {
		log.Printf("ifaceForClientIP: query failed: %v", err)
		return ""
	}
	defer rows.Close()
	for rows.Next() {
		var iface, cidr string
		if err := rows.Scan(&iface, &cidr); err != nil {
			continue
		}
		portalIP := net.ParseIP(strings.SplitN(cidr, "/", 2)[0])
		if portalIP == nil {
			continue
		}
		p4 := portalIP.To4()
		if p4 == nil {
			continue
		}
		if fmt.Sprintf("%d.%d.%d.", p4[0], p4[1], p4[2]) == prefix {
			return iface
		}
	}
	return ""
}

// EnsurePerDeviceClass adds or removes the per-MAC tc class+filter for a
// single session, enabling session-lifecycle hooks in per-device FQ_CODEL
// mode.  It looks up the saved QdiscRule for the portal interface that
// owns the client's IP; if no rule exists or PerDeviceBwMbps is 0
// (global-only mode), it returns immediately.
//
// Call sites in session.go:
//   - action="add"    after runCaptiveRules("auth")  in Start, AdminCreate, Resume
//   - action="remove" after runCaptiveRules("unauth") in End, Pause, ExpireOverdueSessions
//
// Failures are logged but never returned — shaping is secondary to the core
// session state machine and must not block session transitions.
func EnsurePerDeviceClass(db *sql.DB, iface, mac, clientIP, action string) {
	if iface == "" {
		iface = ifaceForClientIP(db, clientIP)
	}
	if iface == "" {
		return // no portal interface found for this client
	}
	if !isValidMAC(mac) {
		log.Printf("EnsurePerDeviceClass: invalid MAC %q — skipping", mac)
		return
	}

	rules := loadQdiscRules(db)
	rule, ok := rules[iface]
	if !ok || rule.PerDeviceBwMbps == 0 {
		return // no rule or global-only mode
	}
	if rule.Qdisc != "fq_codel" {
		return // CAKE doesn't use per-MAC classes
	}

	classID := macToClassID(mac)
	switch action {
	case "add":
		perDevArg := fmt.Sprintf("%dMbit", rule.PerDeviceBwMbps)
		if err := addClientClass(iface, mac, classID, perDevArg); err != nil {
			log.Printf("EnsurePerDeviceClass: add %s on %s failed: %v", mac, iface, err)
		}
	case "remove":
		delClientClass(iface, mac, classID)
	default:
		log.Printf("EnsurePerDeviceClass: unknown action %q", action)
	}
}

// ============================================
// GET /api/admin/portal/qdiag
// ============================================

// QDiag runs tc diagnostic commands on an interface and returns raw output.
// Usage: GET /api/admin/portal/qdiag?iface=end0.22[&client_ip=10.0.22.110]
// When client_ip is provided, runs `ip -o route get <client_ip>` and includes
// the output as a "route" field so the admin can verify traffic flows through
// the expected interface (a common cause of "CAKE applies but doesn't shape").
func (h *QdiscHandler) QDiag(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	iface := r.URL.Query().Get("iface")
	if iface == "" {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Missing 'iface' query parameter"})
		return
	}
	if !validateIfaceName(iface) {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid interface name"})
		return
	}

	runShow := func(subcmd string) string {
		cmd := exec.Command("tc", "-s", subcmd, "show", "dev", iface)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Sprintf("(error: %v)\n%s", err, strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out))
	}

	resp := map[string]interface{}{
		"success": true,
		"iface":   iface,
		"qdisc":   runShow("qdisc"),
		"class":   runShow("class"),
		"filter":  runShow("filter"),
	}

	// Optional: route lookup for a client IP to verify traffic path.
	clientIP := r.URL.Query().Get("client_ip")
	if clientIP != "" {
		// Validate IP to prevent shell injection via the ip command.
		if net.ParseIP(clientIP) == nil {
			resp["route"] = "(error: invalid IP address)"
		} else {
			cmd := exec.Command("ip", "-o", "route", "get", clientIP)
			out, err := cmd.CombinedOutput()
			if err != nil {
				resp["route"] = fmt.Sprintf("(error: %v)\n%s", err, strings.TrimSpace(string(out)))
			} else {
				resp["route"] = strings.TrimSpace(string(out))
			}
		}
	}

	sendJSON(w, http.StatusOK, resp)
}

// ============================================
// GET /api/admin/portal/qdiag/installed
// ============================================

// QDiagInstalled checks whether the tc binary is available on this system.
func (h *QdiscHandler) QDiagInstalled(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path, err := exec.LookPath("tc")
	installed := err == nil
	if !installed {
		path = ""
	}
	sendJSON(w, http.StatusOK, map[string]interface{}{
		"installed": installed,
		"path":      path,
	})
}
