package handlers

import (
	"aircoins-api/models"
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// vlan.go manages VLAN NETWORK IDENTITY ONLY: create/delete the 802.1Q
// interface and persist parent iface + vlan id + name. It deliberately
// does NOT touch IPs, DHCP, DNS hijack or captive rules — that whole
// hotspot stack is provisioned per-interface by portal.go, so plain
// VLANs (uplinks/management) carry no portal baggage.

const vlanConfigPath = "/etc/pisowifi/vlans.conf"

// ============================================
// VLAN LIST
// ============================================

// VLANList returns all VLANs (active interfaces merged with saved config),
// heals interfaces that exist in the config but are missing from the
// kernel (e.g. after a partial boot), and flags attached portal servers.
func VLANList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get active VLANs from ip command
	out, err := exec.Command("ip", "-d", "link", "show", "type", "vlan").Output()
	if err != nil {
		log.Printf("Warning: failed to list active VLANs: %v", err)
		// Continue with empty active list; saved config entries will still be returned
		out = []byte{}
	}

	// Parse ip -d link show type vlan output
	// Block format:
	//   N: name@parent: <FLAGS> ...
	//       ...
	//       vlan id X ...
	vlanRe := regexp.MustCompile(`^\d+:\s+(\S+)@(\S+):`)
	idRe := regexp.MustCompile(`vlan\s+id\s+(\d+)`)

	var activeVLANs []models.VLANInfo
	lines := strings.Split(string(out), "\n")
	var currentIface, currentParent string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if m := vlanRe.FindStringSubmatch(trimmed); m != nil {
			currentIface = m[1]
			currentParent = m[2]
			continue
		}

		if currentIface == "" {
			continue
		}

		if m := idRe.FindStringSubmatch(trimmed); m != nil {
			vid, _ := strconv.Atoi(m[1])

			activeVLANs = append(activeVLANs, models.VLANInfo{
				Interface: currentParent,
				VLANID:    vid,
				Name:      currentIface,
				IP:        getVLANIP(currentIface),
				Active:    true,
			})

			currentIface = ""
			currentParent = ""
		}
	}

	// Merge with saved config (descriptions)
	cfgEntries := readVLANConfig()
	cfgMap := make(map[string]*vlanConfigEntry)
	for i := range cfgEntries {
		key := fmt.Sprintf("%s.%d", cfgEntries[i].Interface, cfgEntries[i].VLANID)
		cfgMap[key] = &cfgEntries[i]
	}

	activeSet := make(map[string]bool)
	for i := range activeVLANs {
		if cfg, ok := cfgMap[activeVLANs[i].Name]; ok {
			activeVLANs[i].Description = cfg.Description
		}
		activeSet[activeVLANs[i].Name] = true
	}

	// Heal: recreate interfaces that are saved but missing from the
	// kernel (parent must exist). Keeps the config authoritative after
	// partial boots without requiring a manual re-create.
	for _, cfg := range cfgEntries {
		name := fmt.Sprintf("%s.%d", cfg.Interface, cfg.VLANID)
		if activeSet[name] {
			continue
		}

		restored := false
		if _, err := os.Stat("/sys/class/net/" + name); err == nil {
			// Already in the kernel — the active-list parse above missed it
			// (e.g. iproute2 without "-d link show type vlan" support).
			// Treat it as live instead of failing the redundant add.
			exec.Command("ip", "link", "set", name, "up").Run()
			restored = true
		} else if _, err := os.Stat("/sys/class/net/" + cfg.Interface); err == nil {
			if out, err := exec.Command("ip", "link", "add", "link", cfg.Interface,
				"name", name, "type", "vlan", "id", strconv.Itoa(cfg.VLANID)).CombinedOutput(); err == nil {
				exec.Command("ip", "link", "set", name, "up").Run()
				restored = true
				log.Printf("VLANList: recreated missing VLAN interface %s", name)
			} else {
				log.Printf("VLANList: failed to recreate %s: %v — %s", name, err, string(out))
			}
		}

		activeVLANs = append(activeVLANs, models.VLANInfo{
			Interface:   cfg.Interface,
			VLANID:      cfg.VLANID,
			Name:        name,
			IP:          getVLANIP(name),
			Description: cfg.Description,
			Active:      restored,
		})
	}

	// Flag VLANs that have a portal server attached (portal.go owns them)
	portalMap := make(map[string]portalConfigEntry)
	for _, p := range readPortalConfig() {
		portalMap[p.Interface] = p
	}
	for i := range activeVLANs {
		if p, ok := portalMap[activeVLANs[i].Name]; ok {
			activeVLANs[i].HasPortal = true
			activeVLANs[i].PortalEnabled = p.Enabled
		}
	}

	if activeVLANs == nil {
		activeVLANs = []models.VLANInfo{}
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{"vlans": activeVLANs})
}

// ============================================
// VLAN CREATE
// ============================================

// VLANCreate creates the 802.1Q interface (link only, no IP/DHCP) and
// persists the identity. Provisioning a hotspot on it is a separate,
// explicit step via the portal endpoints.
func VLANCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req models.VLANRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid JSON: " + err.Error()})
		return
	}

	// Validate VLAN ID
	if req.VLANID < 1 || req.VLANID > 4094 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "VLAN ID must be between 1 and 4094"})
		return
	}

	// Validate interface exists and is not lo or a path-traversal attempt
	if !validateIfaceName(req.Interface) {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid interface name"})
		return
	}
	if _, err := os.Stat("/sys/class/net/" + req.Interface); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Interface does not exist: " + req.Interface})
		return
	}

	vlanName := fmt.Sprintf("%s.%d", req.Interface, req.VLANID)

	// 1. Create VLAN interface (skip if it already exists — re-create is
	// treated as an upsert of the saved identity)
	if _, err := os.Stat("/sys/class/net/" + vlanName); err != nil {
		cmdOut, err := exec.Command("ip", "link", "add", "link", req.Interface, "name", vlanName, "type", "vlan", "id", strconv.Itoa(req.VLANID)).CombinedOutput()
		if err != nil {
			sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to create VLAN: " + string(cmdOut)})
			return
		}
	}

	// 2. Bring interface up
	if cmdOut, err := exec.Command("ip", "link", "set", vlanName, "up").CombinedOutput(); err != nil {
		exec.Command("ip", "link", "delete", vlanName).Run()
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to bring VLAN up: " + string(cmdOut)})
		return
	}

	// Save to database (UPSERT)
	var existing int
	err := models.DB.QueryRow(
		"SELECT id FROM vlan_config WHERE interface=$1 AND vlan_id=$2",
		req.Interface, req.VLANID,
	).Scan(&existing)

	if err == sql.ErrNoRows {
		_, err = models.DB.Exec(
			"INSERT INTO vlan_config (interface, vlan_id, description) VALUES ($1,$2,$3)",
			req.Interface, req.VLANID, req.Description,
		)
	} else if err == nil {
		_, err = models.DB.Exec(
			"UPDATE vlan_config SET description=$3 WHERE interface=$1 AND vlan_id=$2",
			req.Interface, req.VLANID, req.Description,
		)
	}
	if err != nil {
		log.Printf("Warning: failed to persist VLAN to DB: %v", err)
	}

	// Save to config file
	writeVLANConfigEntry(vlanConfigEntry{
		Interface:   req.Interface,
		VLANID:      req.VLANID,
		Description: req.Description,
	})

	vlan := models.VLANInfo{
		Interface:   req.Interface,
		VLANID:      req.VLANID,
		Name:        vlanName,
		Description: req.Description,
		Active:      true,
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{"success": true, "vlan": vlan})
}

// ============================================
// VLAN DELETE
// ============================================

// VLANDelete removes a VLAN interface and its persisted identity. It
// REFUSES to delete a VLAN that still has a portal server attached —
// the operator must delete the portal first, so the hotspot stack is
// always torn down deliberately, never as a side effect.
func VLANDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Interface string `json:"interface"`
		VLANID    int    `json:"vlan_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid JSON: " + err.Error()})
		return
	}

	if req.VLANID < 1 || req.VLANID > 4094 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "VLAN ID must be between 1 and 4094"})
		return
	}
	if !validateIfaceName(req.Interface) {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid interface name"})
		return
	}

	vlanName := fmt.Sprintf("%s.%d", req.Interface, req.VLANID)

	if portalAttached(vlanName) {
		sendJSON(w, http.StatusConflict, models.APIResponse{
			Success: false,
			Message: fmt.Sprintf("A portal server is attached to %s. Delete the portal server first, then delete the VLAN.", vlanName),
		})
		return
	}

	// Delete the interface (already-gone is fine — still clean the config)
	if _, err := os.Stat("/sys/class/net/" + vlanName); err == nil {
		if cmdOut, err := exec.Command("ip", "link", "delete", vlanName).CombinedOutput(); err != nil {
			sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to delete VLAN: " + string(cmdOut)})
			return
		}
	}

	// Remove from config file
	removeVLANConfigEntry(req.Interface, req.VLANID)

	// Delete from database
	if _, err := models.DB.Exec("DELETE FROM vlan_config WHERE interface=$1 AND vlan_id=$2", req.Interface, req.VLANID); err != nil {
		log.Printf("Warning: failed to remove VLAN from DB: %v", err)
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

// ============================================
// VLAN INTERFACES
// ============================================

// VLANInterfaces returns physical network interfaces with their IPs and state.
func VLANInterfaces(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to read interfaces: " + err.Error()})
		return
	}

	var ifaces []models.InterfaceInfo
	for _, entry := range entries {
		name := entry.Name()
		if name == "lo" {
			continue
		}
		// Exclude VLAN sub-interfaces (e.g. eth0.100)
		if strings.Contains(name, ".") {
			continue
		}
		// Only include physical devices (check for /sys/class/net/<name>/device symlink)
		// and bridge interfaces (check for /sys/class/net/<name>/bridge directory).
		// This excludes other virtual interfaces like docker0, veth*, etc.
		devicePath := "/sys/class/net/" + name + "/device"
		bridgePath := "/sys/class/net/" + name + "/bridge"
		if _, err := os.Stat(devicePath); err != nil {
			if _, err := os.Stat(bridgePath); err != nil {
				continue
			}
		}

		ip := getInterfaceIP(name)
		state := getInterfaceState(name)

		ifaces = append(ifaces, models.InterfaceInfo{
			Name:  name,
			IP:    ip,
			State: state,
		})
	}

	if ifaces == nil {
		ifaces = []models.InterfaceInfo{}
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{"interfaces": ifaces})
}

// ============================================
// IP HELPERS
// ============================================

// getVLANIP returns the first IPv4 address (with CIDR) for a VLAN interface.
func getVLANIP(iface string) string {
	out, err := exec.Command("ip", "-4", "addr", "show", iface).Output()
	if err != nil {
		return ""
	}
	return parseIPFromOutput(string(out))
}

// getInterfaceIP returns the first IPv4 address (with CIDR) for a physical interface.
func getInterfaceIP(iface string) string {
	out, err := exec.Command("ip", "-4", "addr", "show", iface).Output()
	if err != nil {
		return ""
	}
	return parseIPFromOutput(string(out))
}

// parseIPFromOutput extracts the first inet address from `ip addr show` output.
func parseIPFromOutput(out string) string {
	re := regexp.MustCompile(`inet\s+(\S+)`)
	for _, line := range strings.Split(out, "\n") {
		if m := re.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

// getInterfaceState reads the operstate of a network interface from sysfs.
// Bridges and VLAN subinterfaces without an IP address report operstate
// "down" (RFC 2863 operational state) even while they pass traffic, so we
// fall back to the kernel LOWER_UP flag, which reflects the real link path.
func getInterfaceState(iface string) string {
	data, err := os.ReadFile("/sys/class/net/" + iface + "/operstate")
	if err != nil {
		return "unknown"
	}
	state := strings.TrimSpace(string(data))
	if state == "up" || hasLowerUp(iface) {
		return "up"
	}
	if state == "" {
		return "unknown"
	}
	return state
}

// hasLowerUp reports whether the interface has the kernel LOWER_UP flag set
// (i.e. there is an actual carrier/link path), regardless of operstate.
func hasLowerUp(iface string) bool {
	out, err := exec.Command("ip", "-o", "link", "show", "dev", iface).Output()
	return err == nil && strings.Contains(string(out), "LOWER_UP")
}

// ============================================
// VLAN CONFIG FILE HELPERS
// ============================================

// vlanConfigEntry represents one line in /etc/pisowifi/vlans.conf.
type vlanConfigEntry struct {
	Interface   string
	VLANID      int
	Description string

	// Legacy fields from the pre portal-server-split format, where the
	// portal config (ip/cidr, start_ip, "portal" flag) lived inline in
	// vlans.conf. Read-only: MigrateLegacyPortalConfig (portal.go) uses
	// them ONCE to seed portals.conf; the writer never emits them.
	LegacyIP      string
	LegacyStartIP string
}

// readVLANConfig reads all entries from /etc/pisowifi/vlans.conf.
// New format:    interface vlan_id description...
// Legacy format: interface vlan_id ip/cidr [start_ip|-] description [portal]
// Legacy lines are detected by a CIDR third field and still parsed so the
// startup migration can move their portal config into portals.conf.
func readVLANConfig() []vlanConfigEntry {
	return readVLANConfigFrom(vlanConfigPath)
}

// readVLANConfigFrom parses a vlans.conf-formatted file.
func readVLANConfigFrom(path string) []vlanConfigEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var entries []vlanConfigEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}

		vid, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}

		entry := vlanConfigEntry{
			Interface: parts[0],
			VLANID:    vid,
		}

		rest := parts[2:]

		// Legacy line: third field is an ip/cidr
		if len(rest) > 0 && strings.Contains(rest[0], "/") {
			if _, _, err := net.ParseCIDR(rest[0]); err == nil {
				entry.LegacyIP = rest[0]
				rest = rest[1:]
				// Optional start_ip column ("-" when unset)
				if len(rest) > 0 {
					if rest[0] == "-" {
						rest = rest[1:]
					} else if net.ParseIP(rest[0]) != nil {
						entry.LegacyStartIP = rest[0]
						rest = rest[1:]
					}
				}
				// Optional trailing "portal" flag
				if len(rest) > 0 && rest[len(rest)-1] == "portal" {
					rest = rest[:len(rest)-1]
				}
			}
		}

		entry.Description = strings.Join(rest, " ")
		entries = append(entries, entry)
	}

	return entries
}

// writeVLANConfig rewrites the entire vlans.conf from the given entries.
// Always emits the new identity-only format (legacy IP fields are dropped;
// by the time this runs the migration has moved them to portals.conf).
func writeVLANConfig(entries []vlanConfigEntry) {
	if err := os.MkdirAll(filepath.Dir(vlanConfigPath), 0755); err != nil {
		log.Printf("Failed to create config dir: %v", err)
		return
	}

	f, err := os.OpenFile(vlanConfigPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		log.Printf("Failed to open VLAN config for writing: %v", err)
		return
	}
	defer f.Close()

	for _, e := range entries {
		line := fmt.Sprintf("%s %d %s", e.Interface, e.VLANID, e.Description)
		fmt.Fprintln(f, strings.TrimRight(line, " "))
	}
}

// writeVLANConfigEntry appends or updates a single entry, then rewrites the file.
func writeVLANConfigEntry(entry vlanConfigEntry) {
	entries := readVLANConfig()

	found := false
	for i := range entries {
		if entries[i].Interface == entry.Interface && entries[i].VLANID == entry.VLANID {
			entries[i] = entry
			found = true
			break
		}
	}
	if !found {
		entries = append(entries, entry)
	}

	writeVLANConfig(entries)
}

// removeVLANConfigEntry deletes an entry and rewrites the file.
func removeVLANConfigEntry(iface string, vlanID int) {
	entries := readVLANConfig()

	var filtered []vlanConfigEntry
	for _, e := range entries {
		if e.Interface == iface && e.VLANID == vlanID {
			continue
		}
		filtered = append(filtered, e)
	}

	writeVLANConfig(filtered)
}

// ============================================
// VALIDATION HELPERS
// ============================================

// validateIfaceName rejects interface names that could cause path traversal or
// shell injection. Only alphanumeric chars, dots, hyphens, and underscores are
// allowed; the name must not start with a dot or contain "..".
func validateIfaceName(name string) bool {
	if name == "" || name == "lo" {
		return false
	}
	if strings.Contains(name, "..") || strings.ContainsAny(name, "/\\") {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
