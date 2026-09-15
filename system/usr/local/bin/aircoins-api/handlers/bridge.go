package handlers

import (
	"aircoins-api/models"
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// bridge.go manages Linux bridge interfaces: create/delete the bridge,
// enslave/remove member interfaces, and persist the configuration.
// A bridge groups multiple interfaces (VLANs, physical ports) into a
// single L2 broadcast domain. The portal stack (portal.go) can be
// provisioned on a bridge just like on a VLAN interface.

const bridgeConfigPath = "/etc/pisowifi/bridges.conf"

// ============================================
// BRIDGE CONFIG FILE HELPERS
// ============================================
// bridges.conf format: bridge_name description member1 member2 ...
// One bridge per line; description is the second field (single token,
// use "_" for spaces or leave empty with "-"), followed by member
// interface names. Comments start with #.

// bridgeConfigEntry represents one line in /etc/pisowifi/bridges.conf.
type bridgeConfigEntry struct {
	Name        string
	Description string
	Members     []string
}

// readBridgesConfig reads all entries from /etc/pisowifi/bridges.conf.
func readBridgesConfig() []bridgeConfigEntry {
	f, err := os.Open(bridgeConfigPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	var entries []bridgeConfigEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) < 1 {
			continue
		}

		entry := bridgeConfigEntry{
			Name: parts[0],
		}

		// Second field is description (use "-" for empty)
		rest := parts[1:]
		if len(rest) > 0 {
			if rest[0] == "-" {
				entry.Description = ""
			} else {
				entry.Description = strings.ReplaceAll(rest[0], "_", " ")
			}
			rest = rest[1:]
		}

		// Remaining fields are members
		if len(rest) > 0 {
			entry.Members = rest
		}
		entries = append(entries, entry)
	}

	return entries
}

// writeBridgesConfig rewrites the entire bridges.conf from the given entries.
func writeBridgesConfig(entries []bridgeConfigEntry) {
	if err := os.MkdirAll(filepath.Dir(bridgeConfigPath), 0755); err != nil {
		log.Printf("Failed to create config dir: %v", err)
		return
	}

	f, err := os.OpenFile(bridgeConfigPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		log.Printf("Failed to open bridge config for writing: %v", err)
		return
	}
	defer f.Close()

	for _, e := range entries {
		// Format: bridge_name description member1 member2 ...
		// Description is a single token (spaces replaced with "_"); use "-" for empty
		desc := "-"
		if e.Description != "" {
			desc = strings.ReplaceAll(e.Description, " ", "_")
		}
		line := e.Name + " " + desc
		if len(e.Members) > 0 {
			line += " " + strings.Join(e.Members, " ")
		}
		fmt.Fprintln(f, line)
	}
}

// writeBridgeConfigEntry appends or updates a single entry, then rewrites the file.
func writeBridgeConfigEntry(entry bridgeConfigEntry) {
	entries := readBridgesConfig()

	found := false
	for i := range entries {
		if entries[i].Name == entry.Name {
			entries[i] = entry
			found = true
			break
		}
	}
	if !found {
		entries = append(entries, entry)
	}

	writeBridgesConfig(entries)
}

// removeBridgeConfigEntry deletes an entry and rewrites the file.
func removeBridgeConfigEntry(name string) {
	entries := readBridgesConfig()

	var filtered []bridgeConfigEntry
	for _, e := range entries {
		if e.Name == name {
			continue
		}
		filtered = append(filtered, e)
	}

	writeBridgesConfig(filtered)
}

// ============================================
// KERNEL HELPERS
// ============================================

// getBridgeMembersFromKernel returns the list of interfaces enslaved to
// a bridge by reading /sys/class/net/<bridge>/brif/.
func getBridgeMembersFromKernel(bridge string) []string {
	brifPath := "/sys/class/net/" + bridge + "/brif"
	entries, err := os.ReadDir(brifPath)
	if err != nil {
		return nil
	}

	var members []string
	for _, e := range entries {
		members = append(members, e.Name())
	}
	return members
}

// listKernelBridges returns the names of all bridge interfaces in the kernel.
func listKernelBridges() []string {
	out, err := exec.Command("ip", "-d", "link", "show", "type", "bridge").Output()
	if err != nil {
		return nil
	}

	// Parse output: lines like "N: name: <FLAGS> ..."
	var bridges []string
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Match lines like "3: br0: <BROADCAST,MULTICAST,UP,LOWER_UP> ..."
		if len(trimmed) > 2 && trimmed[0] >= '0' && trimmed[0] <= '9' {
			parts := strings.Fields(trimmed)
			if len(parts) >= 2 {
				// Second field is "name:" — strip the colon
				name := strings.TrimSuffix(parts[1], ":")
				// Also handle "name@..." format (shouldn't happen for bridges but be safe)
				if idx := strings.Index(name, "@"); idx > 0 {
					name = name[:idx]
				}
				if name != "" {
					bridges = append(bridges, name)
				}
			}
		}
	}
	return bridges
}

// bridgeExistsInKernel checks whether a bridge interface exists.
func bridgeExistsInKernel(name string) bool {
	// Check both that the interface exists and that it's a bridge
	if _, err := os.Stat("/sys/class/net/" + name); err != nil {
		return false
	}
	// Verify it's actually a bridge by checking for brif directory
	if _, err := os.Stat("/sys/class/net/" + name + "/brif"); err != nil {
		return false
	}
	return true
}

// interfaceHasIP checks whether an interface has any IPv4 address assigned.
func interfaceHasIP(iface string) bool {
	out, err := exec.Command("ip", "-4", "addr", "show", iface).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "inet ")
}

// interfaceHasMaster checks whether an interface is already enslaved to a bridge.
// Returns the master bridge name if enslaved, empty string otherwise.
func interfaceHasMaster(iface string) string {
	out, err := exec.Command("ip", "-d", "link", "show", iface).Output()
	if err != nil {
		return ""
	}
	// Look for "master <bridge>" in the output
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "master ") {
			parts := strings.Fields(trimmed)
			for i, p := range parts {
				if p == "master" && i+1 < len(parts) {
					return parts[i+1]
				}
			}
		}
	}
	return ""
}

// getBridgeIP returns the first IPv4 address (with CIDR) for a bridge interface.
func getBridgeIP(bridge string) string {
	out, err := exec.Command("ip", "-4", "addr", "show", bridge).Output()
	if err != nil {
		return ""
	}
	return parseIPFromOutput(string(out))
}

// ============================================
// BRIDGE LIST
// ============================================

// BridgeList returns all bridges (active kernel interfaces merged with saved
// config), heals bridges that exist in the config but are missing from the
// kernel, and flags attached portal servers.
func BridgeList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get active bridges from kernel
	kernelBridges := listKernelBridges()
	kernelSet := make(map[string]bool)
	for _, b := range kernelBridges {
		kernelSet[b] = true
	}

	// Read saved config
	cfgEntries := readBridgesConfig()
	cfgMap := make(map[string]*bridgeConfigEntry)
	for i := range cfgEntries {
		cfgMap[cfgEntries[i].Name] = &cfgEntries[i]
	}

	// Build the result list: start with kernel bridges
	var bridges []models.BridgeInfo
	for _, name := range kernelBridges {
		info := models.BridgeInfo{
			Name:    name,
			Members: getBridgeMembersFromKernel(name),
			IPCIDR:  getBridgeIP(name),
			Active:  true,
		}
		if info.Members == nil {
			info.Members = []string{}
		}
		// Merge description from config
		if cfg, ok := cfgMap[name]; ok {
			info.Description = cfg.Description
		}
		bridges = append(bridges, info)
	}

	// Heal: recreate bridges that are saved but missing from the kernel
	for _, cfg := range cfgEntries {
		if kernelSet[cfg.Name] {
			continue
		}

		restored := false
		if out, err := exec.Command("ip", "link", "add", "name", cfg.Name, "type", "bridge").CombinedOutput(); err == nil {
			exec.Command("ip", "link", "set", cfg.Name, "up").Run()
			restored = true
			log.Printf("BridgeList: recreated missing bridge interface %s", cfg.Name)

			// Re-enslave saved members
			for _, member := range cfg.Members {
				if _, err := os.Stat("/sys/class/net/" + member); err == nil {
					if out, err := exec.Command("ip", "link", "set", member, "master", cfg.Name).CombinedOutput(); err != nil {
						log.Printf("BridgeList: failed to re-enslave %s to %s: %v — %s", member, cfg.Name, err, string(out))
					}
				}
			}
		} else {
			log.Printf("BridgeList: failed to recreate %s: %v — %s", cfg.Name, err, string(out))
		}

		members := getBridgeMembersFromKernel(cfg.Name)
		if members == nil {
			members = cfg.Members // Fall back to saved members if kernel read failed
		}
		if members == nil {
			members = []string{}
		}

		bridges = append(bridges, models.BridgeInfo{
			Name:        cfg.Name,
			Description: cfg.Description,
			Members:     members,
			IPCIDR:      getBridgeIP(cfg.Name),
			Active:      restored,
		})
	}

	// Auto-enslave unclaimed physical interfaces (e.g. hot-plugged USB LAN
	// adapters) to br0 so they show up as bridge members automatically.
	autoAdded := autoEnslavePhysicalMembers()
	if autoAdded == nil {
		autoAdded = []string{}
	}

	// Flag bridges that have a portal server attached
	portalMap := make(map[string]portalConfigEntry)
	for _, p := range readPortalConfig() {
		portalMap[p.Interface] = p
	}
	for i := range bridges {
		if p, ok := portalMap[bridges[i].Name]; ok {
			bridges[i].HasPortal = true
			bridges[i].PortalEnabled = p.Enabled
		}
	}

	if bridges == nil {
		bridges = []models.BridgeInfo{}
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"bridges":     bridges,
		"auto_added":  autoAdded,
	})
}

// ============================================
// AUTO-ENSLAVE PHYSICAL INTERFACES
// ============================================

// defaultRouteInterface returns the interface used by the default route
// (the WAN uplink), or "" if none. Used to avoid bridging the WAN port.
func defaultRouteInterface() string {
	out, err := exec.Command("ip", "route", "show", "default").Output()
	if err != nil {
		return ""
	}
	// Parse "default via x.x.x.x dev <iface> ..."
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// wanCandidateInterfaces returns every interface that could be the WAN
// uplink and must never be bridged:
//   - the interface carrying the default route (live)
//   - detectWANIface() result (default route, first UP Ethernet-like, or
//     the stored eth_interface setting)
//   - the stored eth_interface system setting
func wanCandidateInterfaces() map[string]bool {
	wanSet := make(map[string]bool)
	add := func(name string) {
		if name != "" {
			wanSet[name] = true
		}
	}
	add(defaultRouteInterface())
	add(detectWANIface())
	var eth string
	if err := models.DB.QueryRow("SELECT value FROM system_settings WHERE key='eth_interface'").Scan(&eth); err == nil {
		add(eth)
	}
	return wanSet
}

// autoEnslavePhysicalMembers detects physical (wired) interfaces that are
// NOT enslaved to any bridge, have NO portal server attached, and are NOT
// part of the WAN setup, then adds them as members of br0. This makes
// hot-plugged USB LAN adapters join the LAN bridge automatically without
// manual configuration.
//
// Skipped interfaces:
//   - lo, bridge interfaces themselves, VLAN subinterfaces (name contains '.')
//   - wireless interfaces (wl*/uap* — managed by hostapd)
//   - interfaces already enslaved to a bridge
//   - interfaces with a portal server attached (they are router ports)
//   - WAN-setup interfaces: default-route iface, detectWANIface() result,
//     and the stored eth_interface setting
//   - interfaces with an IP address (likely in use)
//   - non-physical devices (no /sys/class/net/<iface>/device)
//
// Returns the list of interfaces that were newly added to br0.
func autoEnslavePhysicalMembers() []string {
	const targetBridge = "br0"
	if !bridgeExistsInKernel(targetBridge) {
		return nil
	}

	// Portal interfaces must never be bridged (router ports).
	portalSet := make(map[string]bool)
	for _, p := range readPortalConfig() {
		portalSet[p.Interface] = true
	}

	// Never enslave a bridge into another bridge.
	bridgeSet := make(map[string]bool)
	for _, b := range listKernelBridges() {
		bridgeSet[b] = true
	}

	wanSet := wanCandidateInterfaces()

	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		log.Printf("autoEnslave: cannot list /sys/class/net: %v", err)
		return nil
	}

	var added []string
	for _, e := range entries {
		iface := e.Name()

		// Skip loopback, bridges, VLAN subinterfaces, wireless.
		if iface == "lo" || bridgeSet[iface] || strings.Contains(iface, ".") ||
			strings.HasPrefix(iface, "wl") || strings.HasPrefix(iface, "uap") {
			continue
		}

		// Only physical devices (USB LAN, onboard Ethernet).
		if _, err := os.Stat("/sys/class/net/" + iface + "/device"); err != nil {
			continue
		}

		// Skip already-enslaved interfaces.
		if master := interfaceHasMaster(iface); master != "" {
			continue
		}

		// Skip interfaces with a portal server attached.
		if portalSet[iface] || portalAttached(iface) {
			continue
		}

		// Skip WAN-setup interfaces (default route, detected WAN, stored
		// eth_interface setting) and interfaces with an IP address.
		if wanSet[iface] || interfaceHasIP(iface) {
			continue
		}

		// Bring the interface up and enslave it to br0.
		exec.Command("ip", "link", "set", "dev", iface, "up").Run()
		if out, err := exec.Command("ip", "link", "set", iface, "master", targetBridge).CombinedOutput(); err != nil {
			log.Printf("autoEnslave: failed to add %s to %s: %v — %s", iface, targetBridge, err, string(out))
			continue
		}

		// Persist to DB (best-effort) — bridges.conf is synced below.
		if _, err := models.DB.Exec(
			"INSERT INTO bridge_members (bridge_id, member_iface) SELECT id, $2 FROM bridge_config WHERE name = $1",
			targetBridge, iface,
		); err != nil {
			log.Printf("autoEnslave: warning: failed to persist %s to DB: %v", iface, err)
		}

		log.Printf("autoEnslave: added physical interface %s to %s", iface, targetBridge)
		added = append(added, iface)
	}

	if len(added) > 0 {
		updateBridgeConfigMembers(targetBridge)
	}
	return added
}

// ============================================
// BRIDGE CREATE
// ============================================

// BridgeCreate creates a bridge interface and persists the identity.
// Provisioning a portal on it is a separate, explicit step via the
// portal endpoints.
func BridgeCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req models.BridgeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid JSON: " + err.Error()})
		return
	}

	// Validate name
	if !validateIfaceName(req.Name) {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid bridge name"})
		return
	}

	// Check not already exists in kernel
	if bridgeExistsInKernel(req.Name) {
		sendJSON(w, http.StatusConflict, models.APIResponse{Success: false, Message: "Bridge already exists in kernel: " + req.Name})
		return
	}

	// Check not already exists in DB
	var existingID int
	err := models.DB.QueryRow("SELECT id FROM bridge_config WHERE name=$1", req.Name).Scan(&existingID)
	if err == nil {
		sendJSON(w, http.StatusConflict, models.APIResponse{Success: false, Message: "Bridge already exists in database: " + req.Name})
		return
	}

	// Create bridge interface
	if cmdOut, err := exec.Command("ip", "link", "add", "name", req.Name, "type", "bridge").CombinedOutput(); err != nil {
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to create bridge: " + string(cmdOut)})
		return
	}

	// Bring bridge up
	if cmdOut, err := exec.Command("ip", "link", "set", req.Name, "up").CombinedOutput(); err != nil {
		exec.Command("ip", "link", "delete", req.Name, "type", "bridge").Run()
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to bring bridge up: " + string(cmdOut)})
		return
	}

	// Save to database
	_, err = models.DB.Exec(
		"INSERT INTO bridge_config (name, description) VALUES ($1, $2)",
		req.Name, req.Description,
	)
	if err != nil {
		log.Printf("Warning: failed to persist bridge to DB: %v", err)
	}

	// Save to config file
	writeBridgeConfigEntry(bridgeConfigEntry{
		Name:        req.Name,
		Description: req.Description,
		Members:     []string{},
	})

	bridge := models.BridgeInfo{
		Name:        req.Name,
		Description: req.Description,
		Members:     []string{},
		Active:      true,
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{"success": true, "bridge": bridge})
}

// ============================================
// BRIDGE DELETE
// ============================================

// BridgeDelete removes a bridge interface and its persisted identity. It
// REFUSES to delete a bridge that still has a portal server attached or
// has member interfaces — the operator must remove those first.
func BridgeDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid JSON: " + err.Error()})
		return
	}

	if !validateIfaceName(req.Name) {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid bridge name"})
		return
	}

	// Check: refuse if portal attached
	if portalAttached(req.Name) {
		sendJSON(w, http.StatusConflict, models.APIResponse{
			Success: false,
			Message: fmt.Sprintf("A portal server is attached to %s. Delete the portal server first, then delete the bridge.", req.Name),
		})
		return
	}

	// Check: refuse if has members in DB
	var memberCount int
	err := models.DB.QueryRow(
		"SELECT COUNT(*) FROM bridge_members WHERE bridge_id = (SELECT id FROM bridge_config WHERE name=$1)",
		req.Name,
	).Scan(&memberCount)
	if err == nil && memberCount > 0 {
		sendJSON(w, http.StatusConflict, models.APIResponse{
			Success: false,
			Message: fmt.Sprintf("Bridge %s still has %d member(s). Remove all members first.", req.Name, memberCount),
		})
		return
	}

	// Also check kernel for members (in case DB is out of sync)
	kernelMembers := getBridgeMembersFromKernel(req.Name)
	if len(kernelMembers) > 0 {
		sendJSON(w, http.StatusConflict, models.APIResponse{
			Success: false,
			Message: fmt.Sprintf("Bridge %s still has %d member(s) in kernel. Remove all members first.", req.Name, len(kernelMembers)),
		})
		return
	}

	// Delete from kernel (already-gone is fine — still clean the config)
	if bridgeExistsInKernel(req.Name) {
		if cmdOut, err := exec.Command("ip", "link", "delete", req.Name, "type", "bridge").CombinedOutput(); err != nil {
			sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to delete bridge: " + string(cmdOut)})
			return
		}
	}

	// Remove from config file
	removeBridgeConfigEntry(req.Name)

	// Delete from database (cascade will remove bridge_members)
	if _, err := models.DB.Exec("DELETE FROM bridge_config WHERE name=$1", req.Name); err != nil {
		log.Printf("Warning: failed to remove bridge from DB: %v", err)
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

// ============================================
// BRIDGE ADD MEMBER
// ============================================

// BridgeAddMember enslaves an interface to a bridge.
func BridgeAddMember(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req models.BridgeMemberRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid JSON: " + err.Error()})
		return
	}

	if !validateIfaceName(req.BridgeName) {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid bridge name"})
		return
	}
	if !validateIfaceName(req.MemberIface) {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid member interface name"})
		return
	}

	// Validate: bridge exists in kernel
	if !bridgeExistsInKernel(req.BridgeName) {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Bridge does not exist: " + req.BridgeName})
		return
	}

	// Validate: member interface exists in kernel
	if _, err := os.Stat("/sys/class/net/" + req.MemberIface); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Member interface does not exist: " + req.MemberIface})
		return
	}

	// Validate: member has no IP assigned
	if interfaceHasIP(req.MemberIface) {
		sendJSON(w, http.StatusConflict, models.APIResponse{
			Success: false,
			Message: fmt.Sprintf("Member %s has an IP address assigned. Remove the IP before adding to a bridge.", req.MemberIface),
		})
		return
	}

	// Validate: member has no portal attached
	if portalAttached(req.MemberIface) {
		sendJSON(w, http.StatusConflict, models.APIResponse{
			Success: false,
			Message: fmt.Sprintf("A portal server is attached to %s. Delete the portal server first.", req.MemberIface),
		})
		return
	}

	// Validate: member not already in another bridge
	if master := interfaceHasMaster(req.MemberIface); master != "" {
		sendJSON(w, http.StatusConflict, models.APIResponse{
			Success: false,
			Message: fmt.Sprintf("Member %s is already enslaved to bridge %s. Remove it first.", req.MemberIface, master),
		})
		return
	}

	// Add member to bridge in kernel
	if cmdOut, err := exec.Command("ip", "link", "set", req.MemberIface, "master", req.BridgeName).CombinedOutput(); err != nil {
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to add member to bridge: " + string(cmdOut)})
		return
	}

	// Save to database
	_, err := models.DB.Exec(
		"INSERT INTO bridge_members (bridge_id, member_iface) SELECT id, $2 FROM bridge_config WHERE name = $1",
		req.BridgeName, req.MemberIface,
	)
	if err != nil {
		log.Printf("Warning: failed to persist bridge member to DB: %v", err)
	}

	// Update config file
	updateBridgeConfigMembers(req.BridgeName)

	sendJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

// ============================================
// BRIDGE REMOVE MEMBER
// ============================================

// BridgeRemoveMember removes an interface from a bridge.
func BridgeRemoveMember(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req models.BridgeMemberRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid JSON: " + err.Error()})
		return
	}

	if !validateIfaceName(req.BridgeName) {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid bridge name"})
		return
	}
	if !validateIfaceName(req.MemberIface) {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid member interface name"})
		return
	}

	// Remove from kernel (best-effort: interface may already be gone)
	if _, err := os.Stat("/sys/class/net/" + req.MemberIface); err == nil {
		if cmdOut, err := exec.Command("ip", "link", "set", req.MemberIface, "nomaster").CombinedOutput(); err != nil {
			sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to remove member from bridge: " + string(cmdOut)})
			return
		}
	}

	// Remove from database
	_, err := models.DB.Exec(
		"DELETE FROM bridge_members WHERE bridge_id = (SELECT id FROM bridge_config WHERE name = $1) AND member_iface = $2",
		req.BridgeName, req.MemberIface,
	)
	if err != nil {
		log.Printf("Warning: failed to remove bridge member from DB: %v", err)
	}

	// Update config file
	updateBridgeConfigMembers(req.BridgeName)

	sendJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

// ============================================
// CONFIG FILE SYNC HELPERS
// ============================================

// updateBridgeConfigMembers refreshes the members list in bridges.conf
// for a given bridge, reading the current state from the kernel while
// preserving the saved description.
func updateBridgeConfigMembers(bridgeName string) {
	members := getBridgeMembersFromKernel(bridgeName)
	if members == nil {
		members = []string{}
	}

	entries := readBridgesConfig()
	found := false
	for i := range entries {
		if entries[i].Name == bridgeName {
			entries[i].Members = members
			found = true
			break
		}
	}
	if !found {
		entries = append(entries, bridgeConfigEntry{
			Name:    bridgeName,
			Members: members,
		})
	}

	writeBridgesConfig(entries)
}
