package handlers

import (
	"aircoins-api/models"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// wan.go manages WAN configuration: auto-detect the true WAN port,
// persist mode/static/VLAN settings to system_settings, and optionally
// apply to the OS (dhcpcd.conf or VLAN interface creation).

// ============================================
// WAN PORT AUTO-DETECTION
// ============================================

// detectWANIface returns the interface carrying the default route by
// parsing `ip -o route show default`. Returns "" when no default route
// exists (e.g. no uplink connected).
func detectWANIface() string {
	// 1. Try default route
	out, err := exec.Command("ip", "-o", "route", "show", "default").Output()
	if err == nil && len(out) > 0 {
		// Typical output: "default via 192.168.1.1 dev end0 proto dhcp metric 100"
		re := regexp.MustCompile(`dev\s+(\S+)`)
		m := re.FindStringSubmatch(string(out))
		if len(m) >= 2 {
			return m[1]
		}
	}

	// 2. Scan /sys/class/net for the first Ethernet-like interface
	//    that is UP and is NOT a loopback, bridge, VLAN, or wireless.
	entries, err := os.ReadDir("/sys/class/net")
	if err == nil {
		for _, e := range entries {
			name := e.Name()
			// Skip known non-WAN interfaces
			if name == "lo" || strings.HasPrefix(name, "br") || strings.HasPrefix(name, "docker") ||
				strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "wlan") ||
				strings.HasPrefix(name, "wifi") || strings.Contains(name, ".") {
				continue
			}
			// Accept end0, eth0, enp*, ens*, enx* (Ethernet-like)
			if strings.HasPrefix(name, "end") || strings.HasPrefix(name, "eth") ||
				strings.HasPrefix(name, "enp") || strings.HasPrefix(name, "ens") ||
				strings.HasPrefix(name, "enx") || strings.HasPrefix(name, "en") {
				// Verify it's UP
				if data, err := os.ReadFile("/sys/class/net/" + name + "/operstate"); err == nil {
					state := strings.TrimSpace(string(data))
					if state == "up" || state == "unknown" {
						return name
					}
				}
			}
		}
	}

	// 3. Last resort: try stored eth_interface setting
	var eth string
	if err := models.DB.QueryRow("SELECT value FROM system_settings WHERE key='eth_interface'").Scan(&eth); err == nil && eth != "" {
		// Verify it actually exists
		if _, err := os.Stat("/sys/class/net/" + eth); err == nil {
			return eth
		}
	}

	return ""
}

// ============================================
// WAN IP INFO (live address, gateway, DNS)
// ============================================

// getWANIPInfo returns the live IP configuration of the WAN interface
// by parsing ip-route and ip-addr output.  All fields are best-effort;
// missing values are returned as empty strings.
func getWANIPInfo(iface string) map[string]string {
	info := map[string]string{
		"ip":      "",
		"subnet":  "",
		"gateway": "",
		"dns":     "",
		"status":  "down",
	}
	if iface == "" {
		return info
	}

	// --- IP address + CIDR ---
	// ip -o -4 addr show <iface>
	// → "2: eth0    inet 192.168.1.50/24 brd ..."
	if out, err := exec.Command("ip", "-o", "-4", "addr", "show", iface).Output(); err == nil {
		line := strings.TrimSpace(string(out))
		if line != "" {
			info["status"] = "up"
			re := regexp.MustCompile(`inet\s+(\S+)`)
			if m := re.FindStringSubmatch(line); m != nil {
				cidr := m[1] // e.g. "192.168.1.50/24"
				parts := strings.SplitN(cidr, "/", 2)
				info["ip"] = parts[0]
				if len(parts) == 2 {
					info["subnet"] = "/" + parts[1]
				}
			}
		}
	}

	// --- Gateway ---
	// ip -o route show default dev <iface>
	// → "default via 192.168.1.1 dev eth0 proto dhcp metric 100"
	if out, err := exec.Command("ip", "-o", "route", "show", "default", "dev", iface).Output(); err == nil {
		line := strings.TrimSpace(string(out))
		re := regexp.MustCompile(`via\s+(\S+)`)
		if m := re.FindStringSubmatch(line); m != nil {
			info["gateway"] = m[1]
		}
	}

	// --- DNS (from /etc/resolv.conf) ---
	if data, err := os.ReadFile("/etc/resolv.conf"); err == nil {
		var servers []string
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "nameserver") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					servers = append(servers, fields[1])
				}
			}
		}
		info["dns"] = strings.Join(servers, ", ")
	}

	return info
}

// checkInternetConnectivity tries to reach a public DNS server to
// determine if the device has working internet.  Returns "yes", "no",
// or "unknown" (when the ping binary is missing).
func checkInternetConnectivity() string {
	// Try ping first, fall back to wget
	if _, err := exec.LookPath("ping"); err == nil {
		// ping -c 1 -W 3 8.8.8.8  (1 packet, 3-second timeout)
		err := exec.Command("ping", "-c", "1", "-W", "3", "8.8.8.8").Run()
		if err == nil {
			return "yes"
		}
		return "no"
	}
	if _, err := exec.LookPath("wget"); err == nil {
		err := exec.Command("wget", "-q", "--spider", "--timeout=3", "http://8.8.8.8").Run()
		if err == nil {
			return "yes"
		}
		return "no"
	}
	return "unknown"
}

// ============================================
// AVAILABLE VLANS (not claimed by any portal)
// ============================================

// availableVLANs returns VLANs that exist on the system but are NOT
// currently claimed by any portal server. The admin picks one of these
// as the ISP VLAN in "VLAN DHCP" mode.
func availableVLANs() []models.WANAvailableVLAN {
	// 1. All VLANs on the system (from ip link show type vlan)
	out, err := exec.Command("ip", "-d", "link", "show", "type", "vlan").Output()
	if err != nil {
		log.Printf("availableVLANs: failed to list VLANs: %v", err)
		out = []byte{}
	}

	vlanRe := regexp.MustCompile(`^\d+:\s+(\S+)@(\S+):`)
	idRe := regexp.MustCompile(`vlan\s+id\s+(\d+)`)

	type vlanEntry struct {
		iface string // e.g. eth0.100
		vid   int
	}
	var systemVLANs []vlanEntry
	lines := strings.Split(string(out), "\n")
	var currentIface string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if m := vlanRe.FindStringSubmatch(trimmed); m != nil {
			currentIface = m[1]
			continue
		}
		if currentIface == "" {
			continue
		}
		if m := idRe.FindStringSubmatch(trimmed); m != nil {
			vid, _ := strconv.Atoi(m[1])
			systemVLANs = append(systemVLANs, vlanEntry{iface: currentIface, vid: vid})
			currentIface = ""
		}
	}

	// Also include VLANs from the DB config that may not be in the kernel
	for _, cfg := range readVLANConfig() {
		name := fmt.Sprintf("%s.%d", cfg.Interface, cfg.VLANID)
		found := false
		for _, sv := range systemVLANs {
			if sv.iface == name {
				found = true
				break
			}
		}
		if !found {
			systemVLANs = append(systemVLANs, vlanEntry{iface: name, vid: cfg.VLANID})
		}
	}

	// 2. VLANs claimed by portal servers (from portal_servers table)
	portalIfaceSet := make(map[string]bool)
	rows, err := models.DB.Query("SELECT interface FROM portal_servers")
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var iface string
			if err := rows.Scan(&iface); err == nil {
				portalIfaceSet[iface] = true
			}
		}
	}

	// 3. Filter: system VLANs minus portal-claimed ones
	var result []models.WANAvailableVLAN
	for _, sv := range systemVLANs {
		if portalIfaceSet[sv.iface] {
			continue
		}
		result = append(result, models.WANAvailableVLAN{
			VLANID: sv.vid,
			Iface:  sv.iface,
		})
	}
	if result == nil {
		result = []models.WANAvailableVLAN{}
	}
	return result
}

// ============================================
// GET /api/admin/wan
// ============================================

// WANGet returns the current WAN config, auto-detected interface,
// live IP info (address, gateway, DNS), VLAN IP info (when in VLAN mode),
// internet connectivity status, and the available VLAN list.
func WANGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	iface := detectWANIface()

	// Load persisted config from system_settings
	cfg := loadWANConfig()

	// Get live IP info from the base WAN interface
	ipInfo := getWANIPInfo(iface)

	// When in VLAN DHCP mode, also get IP info from the VLAN interface
	var vlanIPInfo map[string]string
	if cfg.Mode == "vlan_dhcp" && cfg.VLANID != nil && iface != "" {
		vlanName := fmt.Sprintf("%s.%d", iface, *cfg.VLANID)
		vlanIPInfo = getWANIPInfo(vlanName)
		vlanIPInfo["interface"] = vlanName
		vlanIPInfo["vlan_id"] = strconv.Itoa(*cfg.VLANID)
	}

	// Check internet connectivity
	internet := checkInternetConnectivity()

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success":         true,
		"iface":           iface,
		"mode":            cfg.Mode,
		"static_config":   cfg.StaticConfig,
		"vlan_id":         cfg.VLANID,
		"available_vlans": availableVLANs(),
		"ip_info":         ipInfo,
		"vlan_ip_info":    vlanIPInfo,
		"internet":        internet,
	})
}

// ============================================
// GET /api/admin/wan/available-vlans
// ============================================

// WANAvailableVLANs returns just the VLAN list (for dropdown refresh).
func WANAvailableVLANs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sendJSON(w, http.StatusOK, map[string]interface{}{
		"available_vlans": availableVLANs(),
	})
}

// ============================================
// POST /api/admin/wan
// ============================================

// WANPost validates, persists, and optionally applies the WAN config.
func WANPost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req models.WANRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid JSON: " + err.Error()})
		return
	}

	// Validate mode
	switch req.Mode {
	case "dhcp":
		// No extra fields required
	case "static":
		if req.StaticConfig == nil {
			sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Static mode requires static_config"})
			return
		}
		if err := validateStaticConfig(req.StaticConfig); err != nil {
			sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: err.Error()})
			return
		}
	case "vlan_dhcp":
		if req.VLANID == nil || *req.VLANID < 1 || *req.VLANID > 4094 {
			sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "VLAN DHCP mode requires a valid vlan_id (1-4094)"})
			return
		}
	default:
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid mode: must be dhcp, static, or vlan_dhcp"})
		return
	}

	// Build the config to persist
	cfg := models.WANConfig{
		Mode:         req.Mode,
		StaticConfig: req.StaticConfig,
		VLANID:       req.VLANID,
	}

	// Persist to system_settings (key: wan_config, JSON value)
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to serialize config"})
		return
	}

	_, err = models.DB.Exec(`
		INSERT INTO system_settings (key, value, updated_at)
		VALUES ('wan_config', $1, NOW())
		ON CONFLICT (key) DO UPDATE SET value = $1, updated_at = NOW()
	`, string(cfgJSON))
	if err != nil {
		log.Printf("WANPost: failed to persist wan_config: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to save config to DB"})
		return
	}

	log.Printf("WANPost: wan_config saved — mode=%s", req.Mode)

	// Optionally apply to OS
	var applyResult map[string]interface{}
	if req.ApplyToOS {
		applyResult = applyWANToOS(cfg)
	}

	resp := map[string]interface{}{
		"success": true,
		"message": "WAN config saved",
	}
	if applyResult != nil {
		resp["apply"] = applyResult
	}
	sendJSON(w, http.StatusOK, resp)
}

// ============================================
// VALIDATION
// ============================================

// validateStaticConfig checks IP, subnet, gateway, DNS fields.
func validateStaticConfig(sc *models.WANStaticConfig) error {
	if sc.IP == "" || sc.Subnet == "" || sc.Gateway == "" || sc.DNS1 == "" || sc.DNS2 == "" {
		return fmt.Errorf("all static fields are required: ip, subnet, gateway, dns1, dns2")
	}
	if net.ParseIP(sc.IP) == nil {
		return fmt.Errorf("invalid IP address: %s", sc.IP)
	}
	if net.ParseIP(sc.Gateway) == nil {
		return fmt.Errorf("invalid gateway: %s", sc.Gateway)
	}
	if net.ParseIP(sc.DNS1) == nil {
		return fmt.Errorf("invalid DNS1: %s", sc.DNS1)
	}
	if net.ParseIP(sc.DNS2) == nil {
		return fmt.Errorf("invalid DNS2: %s", sc.DNS2)
	}
	// Subnet: accept CIDR (e.g. /24) or dotted-quad (e.g. 255.255.255.0)
	if !strings.HasPrefix(sc.Subnet, "/") {
		if net.ParseIP(sc.Subnet) == nil {
			// Try as CIDR suffix
			if _, err := strconv.Atoi(strings.TrimPrefix(sc.Subnet, "/")); err != nil {
				return fmt.Errorf("invalid subnet mask: %s (use CIDR like /24 or dotted-quad like 255.255.255.0)", sc.Subnet)
			}
		}
	}
	return nil
}

// ============================================
// DB HELPERS
// ============================================

// loadWANConfig reads the wan_config JSON from system_settings.
func loadWANConfig() models.WANConfig {
	var val string
	err := models.DB.QueryRow("SELECT value FROM system_settings WHERE key='wan_config'").Scan(&val)
	if err != nil {
		// No config yet — default to DHCP
		return models.WANConfig{Mode: "dhcp"}
	}
	var cfg models.WANConfig
	if err := json.Unmarshal([]byte(val), &cfg); err != nil {
		log.Printf("loadWANConfig: failed to parse stored config: %v", err)
		return models.WANConfig{Mode: "dhcp"}
	}
	return cfg
}

// ============================================
// OS APPLY
// ============================================

// subnetToCIDR converts a dotted-quad subnet mask to a CIDR prefix length.
// E.g. "255.255.255.0" -> "24". If already a CIDR suffix ("/24"), returns "24".
func subnetToCIDR(subnet string) string {
	s := strings.TrimPrefix(subnet, "/")
	if _, err := strconv.Atoi(s); err == nil {
		return s // already a CIDR prefix
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return "24" // fallback
	}
	mask := ip.To4()
	if mask == nil {
		return "24"
	}
	ones, _ := net.IPv4Mask(mask[0], mask[1], mask[2], mask[3]).Size()
	return strconv.Itoa(ones)
}

// applyWANToOS applies the WAN configuration to the operating system.
// It is defensive: errors are returned in the response but never cause
// a rollback — the admin opted in knowing the risks.
func applyWANToOS(cfg models.WANConfig) map[string]interface{} {
	wanIface := detectWANIface()
	if wanIface == "" {
		return map[string]interface{}{
			"success": false,
			"error":   "no WAN interface detected (tried default route, /sys/class/net scan, and stored eth_interface)",
		}
	}

	switch cfg.Mode {
	case "dhcp":
		return applyDHCP(wanIface)
	case "static":
		return applyStatic(wanIface, cfg.StaticConfig)
	case "vlan_dhcp":
		return applyVLANDHCP(wanIface, cfg)
	}
	return map[string]interface{}{"success": true, "message": "no OS changes needed"}
}

// applyDHCP ensures the WAN interface uses DHCP. If switching from
// Static, removes any static config from dhcpcd.conf and restarts.
func applyDHCP(wanIface string) map[string]interface{} {
	// Remove any static block we previously wrote to dhcpcd.conf
	if err := removeStaticFromDhcpcd(wanIface); err != nil {
		log.Printf("applyDHCP: warning: %v", err)
	}

	// Try to restart via systemd service first (works if a DHCP service exists)
	svc := detectDHCPClientService()
	if svc != "" {
		if out, err := exec.Command("systemctl", "restart", svc).CombinedOutput(); err == nil {
			return map[string]interface{}{"success": true, "message": "DHCP applied to " + wanIface + " (via " + svc + ")"}
		} else {
			log.Printf("applyDHCP: systemctl restart %s failed: %s — falling back to direct DHCP request", svc, string(out))
		}
	}

	// Fallback: use requestDHCP which runs the DHCP binary directly
	if err := requestDHCP(wanIface); err != nil {
		return map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("DHCP request on %s failed: %s", wanIface, err.Error()),
		}
	}
	return map[string]interface{}{"success": true, "message": "DHCP applied to " + wanIface}
}

// applyStatic writes a static IP config using iproute2 commands directly.
// This works regardless of whether dhcpcd, dhclient, or NetworkManager is installed.
func applyStatic(wanIface string, sc *models.WANStaticConfig) map[string]interface{} {
	cidr := subnetToCIDR(sc.Subnet)

	// Remove any static block from dhcpcd.conf (cleanup if switching from dhcpcd mode)
	if err := removeStaticFromDhcpcd(wanIface); err != nil {
		log.Printf("applyStatic: warning removing dhcpcd block: %v", err)
	}

	// Apply static IP directly via iproute2 (works on all modern Linux)
	// 1. Flush existing addresses on the interface
	if out, err := exec.Command("ip", "addr", "flush", "dev", wanIface).CombinedOutput(); err != nil {
		log.Printf("applyStatic: ip addr flush warning: %s", string(out))
	}

	// 2. Add the static IP
	addCmd := fmt.Sprintf("%s/%s", sc.IP, cidr)
	if out, err := exec.Command("ip", "addr", "add", addCmd, "dev", wanIface).CombinedOutput(); err != nil {
		return map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("ip addr add %s failed: %s", addCmd, string(out)),
		}
	}

	// 3. Bring interface up
	exec.Command("ip", "link", "set", wanIface, "up").Run()

	// 4. Add default gateway
	if sc.Gateway != "" {
		exec.Command("ip", "route", "del", "default").Run()
		if out, err := exec.Command("ip", "route", "add", "default", "via", sc.Gateway, "dev", wanIface).CombinedOutput(); err != nil {
			log.Printf("applyStatic: gateway warning: %s", string(out))
		}
	}

	// 5. Set DNS via resolv.conf
	if sc.DNS1 != "" {
		dns := sc.DNS1
		if sc.DNS2 != "" {
			dns += "\nnameserver " + sc.DNS2
		}
		os.WriteFile("/etc/resolv.conf", []byte("nameserver "+dns+"\n"), 0644)
	}

	log.Printf("applyStatic: applied %s/%s to %s (gateway=%s)", sc.IP, cidr, wanIface, sc.Gateway)
	return map[string]interface{}{"success": true, "message": fmt.Sprintf("Static IP %s applied to %s", addCmd, wanIface)}
}

// applyVLANDHCP creates the VLAN interface if missing, brings it up,
// and requests DHCP on it using whatever DHCP client is available.
func applyVLANDHCP(wanIface string, cfg models.WANConfig) map[string]interface{} {
	if cfg.VLANID == nil {
		return map[string]interface{}{"success": false, "error": "vlan_id is required"}
	}
	vid := *cfg.VLANID
	vlanName := fmt.Sprintf("%s.%d", wanIface, vid)

	// Create VLAN interface if it doesn't exist
	if _, err := os.Stat("/sys/class/net/" + vlanName); err != nil {
		if out, err := exec.Command("ip", "link", "add", "link", wanIface,
			"name", vlanName, "type", "vlan", "id", strconv.Itoa(vid)).CombinedOutput(); err != nil {
			return map[string]interface{}{
				"success": false,
				"error":   fmt.Sprintf("failed to create VLAN interface %s: %s", vlanName, string(out)),
			}
		}
	}

	// Bring the interface up
	if out, err := exec.Command("ip", "link", "set", vlanName, "up").CombinedOutput(); err != nil {
		return map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("failed to bring up %s: %s", vlanName, string(out)),
		}
	}

	// Request DHCP on the VLAN interface using the available client
	if err := requestDHCP(vlanName); err != nil {
		return map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		}
	}

	return map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("VLAN %s created and DHCP obtained", vlanName),
	}
}

// ============================================
// DHCP CLIENT DETECTION
// ============================================

// detectDHCPClientService returns the name of the active DHCP client
// systemd service. Checks dhclient, dhcpcd, NetworkManager, and
// systemd-networkd in order of likelihood on Armbian/Debian systems.
func detectDHCPClientService() string {
	candidates := []string{"dhclient", "dhcpcd", "NetworkManager", "systemd-networkd"}
	for _, svc := range candidates {
		if _, err := exec.LookPath(svc); err == nil {
			// Verify the service is actually active
			if out, err := exec.Command("systemctl", "is-active", svc).CombinedOutput(); err == nil {
				if strings.TrimSpace(string(out)) == "active" {
					return svc
				}
			}
		}
	}
	// Fallback: just check if the binary exists even if service isn't "active"
	for _, svc := range candidates {
		if _, err := exec.LookPath(svc); err == nil {
			return svc
		}
	}
	return ""
}

// requestDHCP obtains a DHCP lease on the given interface using
// whichever DHCP client is available on the system.
func requestDHCP(iface string) error {
	// Try dhclient first (most common on Armbian/Debian)
	if dhclient, err := exec.LookPath("dhclient"); err == nil {
		// Kill any existing dhclient on this interface, then request a lease
		exec.Command(dhclient, "-r", iface).Run()
		if out, err := exec.Command(dhclient, "-v", iface).CombinedOutput(); err != nil {
			return fmt.Errorf("dhclient %s failed: %s", iface, string(out))
		}
		log.Printf("wan: obtained DHCP on %s via dhclient", iface)
		return nil
	}

	// Try dhcpcd
	if dhcpcd, err := exec.LookPath("dhcpcd"); err == nil {
		if out, err := exec.Command(dhcpcd, iface).CombinedOutput(); err != nil {
			return fmt.Errorf("dhcpcd %s failed: %s", iface, string(out))
		}
		log.Printf("wan: obtained DHCP on %s via dhcpcd", iface)
		return nil
	}

	// Try udhcpc (BusyBox, common on embedded/Armbian)
	if udhcpc, err := exec.LookPath("udhcpc"); err == nil {
		if out, err := exec.Command(udhcpc, "-i", iface, "-n", "-q").CombinedOutput(); err != nil {
			return fmt.Errorf("udhcpc %s failed: %s", iface, string(out))
		}
		log.Printf("wan: obtained DHCP on %s via udhcpc", iface)
		return nil
	}

	// Try NetworkManager via nmcli
	if nmcli, err := exec.LookPath("nmcli"); err == nil {
		if out, err := exec.Command(nmcli, "device", "connect", iface).CombinedOutput(); err != nil {
			return fmt.Errorf("nmcli connect %s failed: %s", iface, string(out))
		}
		log.Printf("wan: obtained DHCP on %s via NetworkManager", iface)
		return nil
	}

	return fmt.Errorf("no DHCP client found (tried dhclient, dhcpcd, udhcpc, nmcli) — install one: apt install isc-dhcp-client")
}

// ============================================
// dhcpcd.conf HELPERS
// ============================================

// stripBlock removes a previously-written AirCoins block from a config file.
func stripBlock(content, beginMarker, endMarker string) string {
	beginIdx := strings.Index(content, beginMarker)
	if beginIdx < 0 {
		return content
	}
	endIdx := strings.Index(content, endMarker)
	if endIdx < 0 || endIdx < beginIdx {
		return content
	}
	endIdx += len(endMarker)
	// Consume the trailing newline after the end marker
	if endIdx < len(content) && content[endIdx] == '\n' {
		endIdx++
	}
	return content[:beginIdx] + content[endIdx:]
}

// removeStaticFromDhcpcd strips any AirCoins static block from dhcpcd.conf.
func removeStaticFromDhcpcd(iface string) error {
	confPath := "/etc/dhcpcd.conf"
	data, err := os.ReadFile(confPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	marker := "# --- AirCoins WAN static config BEGIN ---"
	endMarker := "# --- AirCoins WAN static config END ---"
	cleaned := stripBlock(string(data), marker, endMarker)
	if cleaned == string(data) {
		return nil // nothing to remove
	}
	return os.WriteFile(confPath, []byte(cleaned), 0644)
}

// ============================================
// STARTUP RESTORE
// ============================================

// RestoreWANOnBoot loads the saved WAN config from the database and
// re-applies it to the OS. For static mode, it writes a systemd-networkd
// config so the IP persists across reboots. For DHCP/VLAN modes, it
// ensures the interface is configured on boot.
func RestoreWANOnBoot() {
	cfg := loadWANConfig()
	if cfg.Mode == "" || cfg.Mode == "dhcp" {
		// DHCP mode: OS handles this automatically via dhclient/dhcpcd
		log.Printf("RestoreWANOnBoot: DHCP mode, OS handles automatically")
		return
	}

	wanIface := detectWANIface()
	if wanIface == "" {
		log.Printf("RestoreWANOnBoot: no WAN interface detected, skipping")
		return
	}

	switch cfg.Mode {
	case "static":
		if cfg.StaticConfig == nil {
			log.Printf("RestoreWANOnBoot: static mode but no config saved")
			return
		}
		// Write systemd-networkd config for static IP so it survives reboots
		if err := writeStaticNetworkd(wanIface, cfg.StaticConfig); err != nil {
			log.Printf("RestoreWANOnBoot: failed to write networkd config: %v", err)
		} else {
			log.Printf("RestoreWANOnBoot: static IP %s/%s persisted to networkd for %s",
				cfg.StaticConfig.IP, cfg.StaticConfig.Subnet, wanIface)
		}
	case "vlan_dhcp":
		if cfg.VLANID == nil {
			log.Printf("RestoreWANOnBoot: vlan_dhcp mode but no VLAN ID saved")
			return
		}
		// Create VLAN interface and write networkd config for DHCP
		vlanName := fmt.Sprintf("%s.%d", wanIface, *cfg.VLANID)
		if err := writeVLAN_DHCPNetworkd(wanIface, vlanName, *cfg.VLANID); err != nil {
			log.Printf("RestoreWANOnBoot: failed to write VLAN networkd config: %v", err)
		} else {
			log.Printf("RestoreWANOnBoot: VLAN %s DHCP persisted to networkd", vlanName)
		}
	}
}

// writeStaticNetworkd writes a systemd-networkd config for static IP.
// This ensures the static IP survives reboots.
func writeStaticNetworkd(iface string, sc *models.WANStaticConfig) error {
	cidr := subnetToCIDR(sc.Subnet)
	content := fmt.Sprintf(`[Match]
Name=%s

[Network]
Address=%s/%s
Gateway=%s
DNS=%s
DNS=%s
`, iface, sc.IP, cidr, sc.Gateway, sc.DNS1, sc.DNS2)

	path := fmt.Sprintf("/etc/systemd/network/03-aircoins-wan-%s.network", iface)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return err
	}
	exec.Command("networkctl", "reload").Run()
	return nil
}

// writeVLAN_DHCPNetworkd writes a systemd-networkd config for a VLAN
// interface that uses DHCP. This ensures the VLAN and DHCP survive reboots.
func writeVLAN_DHCPNetworkd(parentIface, vlanName string, vlanID int) error {
	// First, create the VLAN device if it doesn't exist
	vlanDev := fmt.Sprintf("/etc/systemd/network/02-aircoins-%s.netdev", vlanName)
	vlanDevContent := fmt.Sprintf(`[NetDev]
Name=%s
Kind=vlan

[VLAN]
Id=%d
`, vlanName, vlanID)
	if err := os.WriteFile(vlanDev, []byte(vlanDevContent), 0644); err != nil {
		return fmt.Errorf("failed to write netdev: %w", err)
	}

	// Then, configure DHCP on the VLAN interface
	vlanNetwork := fmt.Sprintf("/etc/systemd/network/03-aircoins-%s.network", vlanName)
	vlanNetworkContent := fmt.Sprintf(`[Match]
Name=%s

[Network]
DHCP=yes

[Link]
RequiredForOnline=no
`, vlanName)
	if err := os.WriteFile(vlanNetwork, []byte(vlanNetworkContent), 0644); err != nil {
		return fmt.Errorf("failed to write network: %w", err)
	}

	exec.Command("networkctl", "reload").Run()
	return nil
}
