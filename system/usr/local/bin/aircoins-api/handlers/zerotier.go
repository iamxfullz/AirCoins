package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// ============================================
// ZEROTIER HANDLER
// ============================================

// ZeroTierNetwork represents a single ZeroTier network the device has joined.
type ZeroTierNetwork struct {
	NetworkID string   `json:"network_id"`
	Name      string   `json:"name"`
	Status    string   `json:"status"`
	Type      string   `json:"type"`
	MAC       string   `json:"mac"`
	Addresses []string `json:"addresses"`
	MTU       int      `json:"mtu"`
	PortError int      `json:"port_error"`
}

// ZeroTierStatus is the full response payload for GET /api/admin/zerotier/status.
type ZeroTierStatus struct {
	Installed bool              `json:"installed"`
	Version   string            `json:"version"`
	ServiceUp bool              `json:"service_up"`
	Address   string            `json:"address"` // 10-digit ZeroTier node address
	Networks  []ZeroTierNetwork `json:"networks"`
	Online    bool              `json:"online"`
	LastError string            `json:"last_error,omitempty"`
}

// zerotierStatus returns the current ZeroTier state.
func zerotierStatus() ZeroTierStatus {
	st := ZeroTierStatus{
		Networks: []ZeroTierNetwork{},
	}

	// Check if zerotier-cli is installed
	cliPath, err := exec.LookPath("zerotier-cli")
	if err != nil {
		// Also try /usr/local/bin directly
		cliPath = "/usr/local/bin/zerotier-cli"
		if _, err2 := exec.LookPath(cliPath); err2 != nil {
			st.Installed = false
			return st
		}
	}
	st.Installed = true

	// Check systemd service first (zerotier-cli won't work if service is down)
	if out, err := exec.Command("systemctl", "is-active", "zerotier-one").Output(); err == nil {
		st.ServiceUp = strings.TrimSpace(string(out)) == "active"
	}

	if !st.ServiceUp {
		return st
	}

	// Single call to get node info: version, address, and online status
	if out, err := exec.Command(cliPath, "-j", "info").Output(); err == nil {
		var info map[string]interface{}
		if json.Unmarshal(out, &info) == nil {
			if v, ok := info["version"].(string); ok {
				st.Version = v
			}
			if addr, ok := info["address"].(string); ok {
				st.Address = addr
			}
			if online, ok := info["online"].(bool); ok {
				st.Online = online
			}
		}
	}

	// Get network list
	if out, err := exec.Command(cliPath, "-j", "listnetworks").Output(); err == nil {
		var networks []map[string]interface{}
		if json.Unmarshal(out, &networks) == nil {
			for _, n := range networks {
				ztNet := ZeroTierNetwork{}
				if v, ok := n["id"].(string); ok {
					ztNet.NetworkID = v
				}
				if v, ok := n["name"].(string); ok {
					ztNet.Name = v
				}
				if v, ok := n["status"].(string); ok {
					ztNet.Status = v
				}
				if v, ok := n["type"].(string); ok {
					ztNet.Type = v
				}
				if v, ok := n["mac"].(string); ok {
					ztNet.MAC = v
				}
				if mtu, ok := n["mtu"].(float64); ok {
					ztNet.MTU = int(mtu)
				}
				if pe, ok := n["portError"].(float64); ok {
					ztNet.PortError = int(pe)
				}
				// assignedAddresses is an array of strings
				if addrs, ok := n["assignedAddresses"].([]interface{}); ok {
					for _, a := range addrs {
						if s, ok := a.(string); ok {
							ztNet.Addresses = append(ztNet.Addresses, s)
						}
					}
				}
				if ztNet.Addresses == nil {
					ztNet.Addresses = []string{}
				}
				st.Networks = append(st.Networks, ztNet)
			}
		}
	}

	return st
}

// ============================================
// HTTP ENDPOINTS
// ============================================

// ZeroTierGetStatus handles GET /api/admin/zerotier/status
func ZeroTierGetStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	st := zerotierStatus()
	sendJSON(w, http.StatusOK, st)
}

// ZeroTierInstall handles POST /api/admin/zerotier/install
// Downloads and runs the official ZeroTier install script.
func ZeroTierInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check if already installed
	if _, err := exec.LookPath("zerotier-cli"); err == nil {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "ZeroTier is already installed",
		})
		return
	}

	log.Println("zerotier: starting installation via official install script")

	// Run the official ZeroTier install script: curl -s https://install.zerotier.com | bash
	cmd := exec.Command("bash", "-c", "curl -s https://install.zerotier.com | bash")
	cmd.Env = append(cmd.Environ(), "DEBIAN_FRONTEND=noninteractive")

	// Give it a generous timeout — install can take a while on slow connections
	done := make(chan error, 1)
	go func() {
		out, err := cmd.CombinedOutput()
		if err != nil {
			done <- fmt.Errorf("install failed: %w — output: %s", err, string(out))
			return
		}
		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			log.Printf("zerotier: install error: %v", err)
			sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case <-time.After(3 * time.Minute):
		cmd.Process.Kill()
		sendJSON(w, http.StatusGatewayTimeout, map[string]interface{}{
			"success": false,
			"message": "Installation timed out after 3 minutes",
		})
		return
	}

	// Verify installation
	if _, err := exec.LookPath("zerotier-cli"); err != nil {
		sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "Installation completed but zerotier-cli not found in PATH",
		})
		return
	}

	// Ensure the service is running
	exec.Command("systemctl", "enable", "--now", "zerotier-one").Run()
	time.Sleep(2 * time.Second) // give the service a moment to start

	st := zerotierStatus()
	log.Printf("zerotier: installation complete (version=%s, service_up=%v)", st.Version, st.ServiceUp)
	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "ZeroTier installed successfully",
		"status":  st,
	})
}

// ZeroTierJoin handles POST /api/admin/zerotier/join
// Body: { "network_id": "16-digit hex network ID" }
func ZeroTierJoin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		NetworkID string `json:"network_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "Invalid request body",
		})
		return
	}

	networkID := strings.TrimSpace(strings.ToLower(req.NetworkID))
	if networkID == "" {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "network_id is required",
		})
		return
	}

	// Validate: must be exactly 16 hex characters
	if len(networkID) != 16 {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "network_id must be exactly 16 hex characters",
		})
		return
	}
	for _, c := range networkID {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			sendJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"message": "network_id must contain only hex characters (0-9, a-f)",
			})
			return
		}
	}

	// Check if ZeroTier is installed and running
	st := zerotierStatus()
	if !st.Installed {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "ZeroTier is not installed. Install it first.",
		})
		return
	}
	if !st.ServiceUp {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "ZeroTier service is not running",
		})
		return
	}

	// Check if already joined
	for _, n := range st.Networks {
		if n.NetworkID == networkID {
			sendJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"message": "Already joined network " + networkID,
			})
			return
		}
	}

	// Join the network
	cmd := exec.Command("zerotier-cli", "join", networkID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("zerotier: join %s failed: %v — %s", networkID, err, string(out))
		sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("Failed to join network: %s", string(out)),
		})
		return
	}

	log.Printf("zerotier: joined network %s", networkID)
	time.Sleep(2 * time.Second) // wait for network to initialize

	newSt := zerotierStatus()
	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "Joined ZeroTier network " + networkID,
		"output":  strings.TrimSpace(string(out)),
		"status":  newSt,
	})
}

// ZeroTierLeave handles POST /api/admin/zerotier/leave
// Body: { "network_id": "16-digit hex network ID" }
func ZeroTierLeave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		NetworkID string `json:"network_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "Invalid request body",
		})
		return
	}

	networkID := strings.TrimSpace(strings.ToLower(req.NetworkID))
	if networkID == "" {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "network_id is required",
		})
		return
	}

	// Validate
	if len(networkID) != 16 {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "network_id must be exactly 16 hex characters",
		})
		return
	}

	// Check if installed
	st := zerotierStatus()
	if !st.Installed {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "ZeroTier is not installed",
		})
		return
	}

	// Leave the network
	cmd := exec.Command("zerotier-cli", "leave", networkID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("zerotier: leave %s failed: %v — %s", networkID, err, string(out))
		sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("Failed to leave network: %s", string(out)),
		})
		return
	}

	log.Printf("zerotier: left network %s", networkID)
	time.Sleep(1 * time.Second)

	newSt := zerotierStatus()
	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "Left ZeroTier network " + networkID,
		"output":  strings.TrimSpace(string(out)),
		"status":  newSt,
	})
}
