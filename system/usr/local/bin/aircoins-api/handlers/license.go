package handlers

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// LicenseState is stored as JSON in system_settings key 'license_state'.
type LicenseState struct {
	Status                 string     `json:"status"`
	TrialStartedAt         time.Time  `json:"trial_started_at"`
	TrialExpiresAt         time.Time  `json:"trial_expires_at"`
	LicenseKey             string     `json:"license_key"`
	ActivatedAt            *time.Time `json:"activated_at"`
	ExpiresAt              *time.Time `json:"expires_at"`
	OwnerEmail             string     `json:"owner_email"`
	HardwareID             string     `json:"hardware_id"`
	LastHeartbeatAt        *time.Time `json:"last_heartbeat_at"`
	LastSupabaseResponseAt *time.Time `json:"last_supabase_response_at"`
	LastSupabaseError      string     `json:"last_supabase_error"`
}

// LicenseHandler manages the license state for this AirCoins device.
type LicenseHandler struct {
	db    *sql.DB
	mu    sync.RWMutex
	state LicenseState
}

const licenseSettingKey = "license_state"
const licenseSettingDesc = "License management state (trial/active/locked/revoked)"

// NewLicenseHandler creates a LicenseHandler backed by the given DB.
func NewLicenseHandler(db *sql.DB) *LicenseHandler {
	return &LicenseHandler{db: db}
}

// HardwareFingerprint returns a stable hardware identifier unique to each
// physical SBC board. It tries multiple sources in order of reliability:
//  1. CPU serial from /proc/cpuinfo (unique per SoC, when available)
//  2. Device tree serial-number (Allwinner SID)
//  3. Ethernet MAC address (usually burned into the board)
//  4. /etc/machine-id (on SD card — regenerated if stale)
//
// If /etc/machine-id is used and doesn't match the current hardware stamp,
// it is regenerated so that cloned SD cards get a unique identity.
func HardwareFingerprint() string {
	// 1. CPU serial from /proc/cpuinfo
	f, err := os.Open("/proc/cpuinfo")
	if err == nil {
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "Serial") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					serial := strings.TrimSpace(parts[1])
					if serial != "" && !isAllZeros(serial) {
						f.Close()
						return serial
					}
				}
			}
		}
		f.Close()
	}

	// 2. Device tree serial-number (Allwinner SID e-fuse)
	if data, err := os.ReadFile("/sys/firmware/devicetree/base/serial-number"); err == nil {
		serial := strings.TrimRight(string(data), "\x00\n\r ")
		if serial != "" && !isAllZeros(serial) {
			return "dt-" + serial
		}
	}

	// 3. Ethernet MAC address (usually unique per board)
	if mac := readEthMAC(); mac != "" {
		// Reject common dummy/universal MACs
		if mac != "00:00:00:00:00:00" && mac != "02:00:00:00:00:00" && !strings.HasPrefix(mac, "02:00:00") {
			return "mac-" + strings.ReplaceAll(mac, ":", "")
		}
	}

	// 4. /etc/machine-id — but regenerate if it doesn't match current hardware
	machineID := readFirstLine("/etc/machine-id")
	hwStamp := buildHardwareStamp()

	stampFile := "/etc/machine-id.hardware-stamp"
	oldStamp := readFirstLine(stampFile)

	if machineID != "" && oldStamp == hwStamp {
		// machine-id matches this hardware — safe to use
		return machineID
	}

	// Hardware stamp changed (or no stamp file) — regenerate machine-id
	// This handles cloned SD cards: new board → new stamp → new machine-id
	newID := generateMachineID()
	os.WriteFile("/etc/machine-id", []byte(newID+"\n"), 0444)
	os.WriteFile(stampFile, []byte(hwStamp+"\n"), 0644)
	log.Printf("license: regenerated /etc/machine-id for new hardware (stamp=%s)", hwStamp)
	return newID
}

// isAllZeros returns true if s consists entirely of '0' characters.
func isAllZeros(s string) bool {
	for _, c := range s {
		if c != '0' {
			return false
		}
	}
	return true
}

// readFirstLine reads the first line of a file, returning "" on any error.
func readFirstLine(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)[0]
}

// readEthMAC returns the MAC address of the first Ethernet-like interface
// found in /sys/class/net (end0, eth0, enp*, ens*, enx*). Returns "" if none.
func readEthMAC() string {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return ""
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "end") || strings.HasPrefix(name, "eth") ||
			strings.HasPrefix(name, "enp") || strings.HasPrefix(name, "ens") ||
			strings.HasPrefix(name, "enx") {
			if mac := readFirstLine("/sys/class/net/" + name + "/address"); mac != "" {
				return strings.ToLower(strings.TrimSpace(mac))
			}
		}
	}
	return ""
}

// buildHardwareStamp creates a fingerprint of the current hardware by
// combining all available hardware identifiers. This is used to detect
// when the SD card has been moved to a different board.
func buildHardwareStamp() string {
	var parts []string

	// CPU serial
	if f, err := os.Open("/proc/cpuinfo"); err == nil {
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "Serial") {
				p := strings.SplitN(line, ":", 2)
				if len(p) == 2 {
					s := strings.TrimSpace(p[1])
					if s != "" && !isAllZeros(s) {
						parts = append(parts, "cpu:"+s)
					}
				}
			}
		}
		f.Close()
	}

	// Device tree serial
	if data, err := os.ReadFile("/sys/firmware/devicetree/base/serial-number"); err == nil {
		s := strings.TrimRight(string(data), "\x00\n\r ")
		if s != "" && !isAllZeros(s) {
			parts = append(parts, "dt:"+s)
		}
	}

	// Ethernet MAC
	if mac := readEthMAC(); mac != "" {
		if mac != "00:00:00:00:00:00" {
			parts = append(parts, "mac:"+mac)
		}
	}

	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, "|")
}

// generateMachineID creates a random 32-character hex string in the same
// format as systemd-machine-id-setup.
func generateMachineID() string {
	f, err := os.Open("/dev/urandom")
	if err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	defer f.Close()
	buf := make([]byte, 16)
	f.Read(buf)
	return fmt.Sprintf("%x", buf)
}

// InitOrLoadLicense initialises the license state from the database, or
// seeds a fresh 7-day trial if no row exists. Safe to call on every start.
// If the hardware ID has changed (SD card cloned to a new board), it resets
// to a fresh 7-day trial bound to the new hardware — this is the expected
// business flow for deploying images to multiple buyers.
func (h *LicenseHandler) InitOrLoadLicense() error {
	var value string
	err := h.db.QueryRow(
		"SELECT value FROM system_settings WHERE key = $1", licenseSettingKey,
	).Scan(&value)

	if err == sql.ErrNoRows {
		// First run — seed a trial
		now := time.Now().UTC()
		hwID := HardwareFingerprint()
		h.mu.Lock()
		h.state = LicenseState{
			Status:         "trial",
			TrialStartedAt: now,
			TrialExpiresAt: now.Add(7 * 24 * time.Hour),
			HardwareID:     hwID,
		}
		h.mu.Unlock()
		h.saveState()
		log.Printf("license: seeded 7-day trial (hardware_id=%s)", hwID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("license: DB read failed: %w", err)
	}

	var st LicenseState
	if err := json.Unmarshal([]byte(value), &st); err != nil {
		return fmt.Errorf("license: corrupt JSON in system_settings: %w", err)
	}

	// CRITICAL: Validate hardware identity on every load.
	// If the hardware ID changed (SD card cloned to new board), reset to a
	// fresh 7-day trial. This is the expected business flow: flash image →
	// new board → new hardware ID → 7-day trial → buyer purchases license.
	liveHW := HardwareFingerprint()
	if st.HardwareID != "" && st.HardwareID != liveHW {
		log.Printf("license: HARDWARE CHANGE DETECTED — old=%s new=%s — resetting to fresh 7-day trial", st.HardwareID, liveHW)
		now := time.Now().UTC()
		st = LicenseState{
			Status:         "trial",
			TrialStartedAt: now,
			TrialExpiresAt: now.Add(7 * 24 * time.Hour),
			HardwareID:     liveHW,
		}
	} else if st.HardwareID == "" {
		// No hardware ID stored (legacy state) — bind to current hardware
		st.HardwareID = liveHW
		log.Printf("license: binding to hardware_id=%s (was empty)", liveHW)
	}

	h.mu.Lock()
	h.state = st
	h.mu.Unlock()
	h.saveState()
	log.Printf("license: loaded state status=%s hardware_id=%s", st.Status, st.HardwareID)
	return nil
}

// isLicenseValidLocked must be called with h.mu at least RLocked.
func (h *LicenseHandler) isLicenseValidLocked() bool {
	switch h.state.Status {
	case "trial":
		if time.Now().After(h.state.TrialExpiresAt) {
			return false
		}
	case "active":
		// Check for license expiration
		if h.state.ExpiresAt != nil && time.Now().After(*h.state.ExpiresAt) {
			return false
		}
		// Active licenses are valid even if heartbeat is stale
		return true
	default:
		return false
	}
	// Heartbeat check (only for trials)
	if h.state.LastSupabaseResponseAt != nil {
		if time.Since(*h.state.LastSupabaseResponseAt) > 24*time.Hour {
			return false
		}
	}
	return true
}

// IsLicenseValid reports whether the device is currently licensed to operate.
// Valid states: "trial" (not expired) or "active".
// If a Supabase heartbeat has occurred, the last response must be < 24 h old.
func (h *LicenseHandler) IsLicenseValid() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.isLicenseValidLocked()
}

// getSupabaseAPIKey returns the anon/publishable key if available,
// falling back to service_role key for backward compatibility.
// Client/customer devices should ONLY be given SUPABASE_ANON_KEY.
func getSupabaseAPIKey() string {
	if key := os.Getenv("SUPABASE_ANON_KEY"); key != "" {
		return key
	}
	return os.Getenv("SUPABASE_SERVICE_ROLE_KEY")
}

// supabaseRequest is a helper for Supabase REST API calls.
func supabaseRequest(method, url string, body io.Reader) (*http.Request, error) {
	apiKey := getSupabaseAPIKey()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apikey", apiKey)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if method == http.MethodPatch || method == http.MethodPost {
		req.Header.Set("Prefer", "return=representation")
	}
	return req, nil
}

// HeartbeatSupabase phones home to the Supabase license table.
// Non-fatal on any error — the device keeps working with the last known
// state so a network outage doesn't kill a running piso WiFi machine.
func (h *LicenseHandler) HeartbeatSupabase() error {
	baseURL := os.Getenv("SUPABASE_URL")
	apiKey := getSupabaseAPIKey()

	if baseURL == "" || apiKey == "" {
		log.Printf("license: SUPABASE_URL or API key not set — skipping heartbeat")
		h.mu.Lock()
		h.state.LastSupabaseError = "Supabase credentials not configured"
		h.mu.Unlock()
		h.saveState()
		return nil
	}

	h.mu.RLock()
	licenseKey := h.state.LicenseKey
	hwID := h.state.HardwareID
	status := h.state.Status
	h.mu.RUnlock()

	// Use the live hardware ID for heartbeat (in case it changed since init)
	liveHW := HardwareFingerprint()
	if liveHW != "" && liveHW != "unknown" {
		hwID = liveHW
	}

	client := &http.Client{Timeout: 15 * time.Second}
	now := time.Now().UTC()

	if licenseKey != "" {
		// Licensed device: GET current row, then PATCH heartbeat
		getURL := fmt.Sprintf("%s/rest/v1/aircoins_licenses?license_key=eq.%s&select=*", baseURL, licenseKey)
		req, err := supabaseRequest(http.MethodGet, getURL, nil)
		if err != nil {
			return h.recordHeartbeatError(err, "build GET request")
		}
		resp, err := client.Do(req)
		if err != nil {
			return h.recordHeartbeatError(err, "Supabase GET")
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)

		if resp.StatusCode >= 400 {
			return h.recordHeartbeatError(fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body)), "Supabase GET")
		}

		var rows []map[string]interface{}
		if err := json.Unmarshal(body, &rows); err != nil || len(rows) == 0 {
			return h.recordHeartbeatError(fmt.Errorf("no license row found for key"), "Supabase GET")
		}
		row := rows[0]

		// Sync remote status
		remoteStatus, _ := row["status"].(string)
		h.mu.Lock()
		if remoteStatus != "" && remoteStatus != "active" && remoteStatus != "available" {
			h.state.Status = remoteStatus
		}
		h.state.LastHeartbeatAt = &now
		h.state.LastSupabaseResponseAt = &now
		h.state.LastSupabaseError = ""
		h.mu.Unlock()
		h.saveState()

		// PATCH heartbeat timestamp on Supabase
		patchURL := fmt.Sprintf("%s/rest/v1/aircoins_licenses?license_key=eq.%s", baseURL, licenseKey)
		patchBody, _ := json.Marshal(map[string]interface{}{
			"last_heartbeat_at": now.Format(time.RFC3339),
			"hardware_id":       hwID,
		})
		patchReq, err := supabaseRequest(http.MethodPatch, patchURL, bytes.NewReader(patchBody))
		if err == nil {
			if pResp, err := client.Do(patchReq); err == nil {
				pResp.Body.Close()
			}
		}
	} else {
		// Trial device: POST a heartbeat row so Supabase knows this device exists
		postURL := baseURL + "/rest/v1/aircoins_license_heartbeats"
		postBody, _ := json.Marshal(map[string]interface{}{
			"hardware_id": hwID,
			"status":      "trial",
		})
		req, err := supabaseRequest(http.MethodPost, postURL, bytes.NewReader(postBody))
		if err != nil {
			return h.recordHeartbeatError(err, "build POST request")
		}
		resp, err := client.Do(req)
		if err != nil {
			return h.recordHeartbeatError(err, "Supabase POST heartbeat")
		}
		defer resp.Body.Close()

		h.mu.Lock()
		h.state.LastHeartbeatAt = &now
		h.state.LastSupabaseResponseAt = &now
		h.state.LastSupabaseError = ""
		h.mu.Unlock()
		h.saveState()
	}

	log.Printf("license: heartbeat ok (status=%s)", status)
	return nil
}

// recordHeartbeatError stores an error string without changing the license
// status, so a network failure never locks the device.
func (h *LicenseHandler) recordHeartbeatError(err error, context string) error {
	msg := fmt.Sprintf("%s: %v", context, err)
	log.Printf("license: heartbeat error: %s", msg)
	h.mu.Lock()
	h.state.LastSupabaseError = msg
	h.mu.Unlock()
	h.saveState()
	return fmt.Errorf("%s", msg)
}

// ActivateLicense validates a license key against Supabase and, if eligible,
// activates it for this device.
func (h *LicenseHandler) ActivateLicense(key, email string) error {
	baseURL := os.Getenv("SUPABASE_URL")
	apiKey := getSupabaseAPIKey()
	if baseURL == "" || apiKey == "" {
		return fmt.Errorf("Supabase credentials not configured")
	}

	client := &http.Client{Timeout: 15 * time.Second}

	// Fetch the license row
	getURL := fmt.Sprintf("%s/rest/v1/aircoins_licenses?license_key=eq.%s&select=*", baseURL, key)
	req, err := supabaseRequest(http.MethodGet, getURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("Supabase request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return fmt.Errorf("Supabase error HTTP %d: %s", resp.StatusCode, string(body))
	}

	var rows []map[string]interface{}
	if err := json.Unmarshal(body, &rows); err != nil || len(rows) == 0 {
		return fmt.Errorf("license key not found")
	}
	row := rows[0]

	remoteStatus, _ := row["status"].(string)
	if remoteStatus != "available" {
		return fmt.Errorf("license is not available (current status: %s)", remoteStatus)
	}

	remoteHW, _ := row["hardware_id"].(string)
	h.mu.RLock()
	localHW := h.state.HardwareID
	h.mu.RUnlock()
	if remoteHW != "" && remoteHW != localHW {
		return fmt.Errorf("license is bound to different hardware")
	}

	// PATCH: activate
	now := time.Now().UTC()
	patchURL := fmt.Sprintf("%s/rest/v1/aircoins_licenses?license_key=eq.%s", baseURL, key)
	patchBody, _ := json.Marshal(map[string]interface{}{
		"status":            "active",
		"activated_at":      now.Format(time.RFC3339),
		"owner_email":       email,
		"hardware_id":       localHW,
		"last_heartbeat_at": now.Format(time.RFC3339),
	})
	patchReq, err := supabaseRequest(http.MethodPatch, patchURL, bytes.NewReader(patchBody))
	if err != nil {
		return fmt.Errorf("build PATCH request: %w", err)
	}
	pResp, err := client.Do(patchReq)
	if err != nil {
		return fmt.Errorf("Supabase PATCH failed: %w", err)
	}
	defer pResp.Body.Close()
	if pResp.StatusCode >= 400 {
		pBody, _ := io.ReadAll(pResp.Body)
		return fmt.Errorf("Supabase PATCH error HTTP %d: %s", pResp.StatusCode, string(pBody))
	}

	h.mu.Lock()
	h.state.Status = "active"
	h.state.LicenseKey = key
	h.state.OwnerEmail = email
	h.state.ActivatedAt = &now
	h.state.LastHeartbeatAt = &now
	h.state.LastSupabaseResponseAt = &now
	h.state.LastSupabaseError = ""
	h.mu.Unlock()
	h.saveState()
	log.Printf("license: activated key=%s email=%s", key, email)
	return nil
}

// DeactivateLicense revokes the license on Supabase and moves the local
// state to "locked".
func (h *LicenseHandler) DeactivateLicense() error {
	baseURL := os.Getenv("SUPABASE_URL")
	apiKey := getSupabaseAPIKey()

	h.mu.RLock()
	key := h.state.LicenseKey
	h.mu.RUnlock()

	if key == "" {
		return fmt.Errorf("no license key to deactivate")
	}

	if baseURL != "" && apiKey != "" {
		client := &http.Client{Timeout: 15 * time.Second}
		patchURL := fmt.Sprintf("%s/rest/v1/aircoins_licenses?license_key=eq.%s", baseURL, key)
		patchBody, _ := json.Marshal(map[string]interface{}{
			"status":      "revoked",
			"hardware_id": nil,
		})
		patchReq, err := supabaseRequest(http.MethodPatch, patchURL, bytes.NewReader(patchBody))
		if err != nil {
			return fmt.Errorf("remote deactivation failed: %w", err)
		}
		pResp, err := client.Do(patchReq)
		if err != nil {
			return fmt.Errorf("remote deactivation failed: %w", err)
		}
		defer pResp.Body.Close()
		if pResp.StatusCode >= 400 {
			return fmt.Errorf("remote deactivation failed (HTTP %d) — license still active on server", pResp.StatusCode)
		}
	}

	h.mu.Lock()
	h.state.Status = "locked"
	h.mu.Unlock()
	h.saveState()
	log.Printf("license: deactivated (local status=locked)")
	return nil
}

// saveState marshals the current state and upserts it into system_settings.
func (h *LicenseHandler) saveState() {
	h.mu.RLock()
	st := h.state
	h.mu.RUnlock()

	doc, err := json.Marshal(st)
	if err != nil {
		log.Printf("license: marshal error: %v", err)
		return
	}

	_, err = h.db.Exec(`
		INSERT INTO system_settings (key, value, description, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (key) DO UPDATE SET value = $2, updated_at = NOW()
	`, licenseSettingKey, string(doc), licenseSettingDesc)
	if err != nil {
		log.Printf("license: saveState DB error: %v", err)
	}
}

// ============================================
// HTTP HANDLERS
// ============================================

// Status returns the current license state and whether it is valid.
func (h *LicenseHandler) Status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.mu.RLock()
	st := h.state
	valid := h.isLicenseValidLocked()
	h.mu.RUnlock()

	maskedKey := ""
	if len(st.LicenseKey) > 4 {
		maskedKey = "****" + st.LicenseKey[len(st.LicenseKey)-4:]
	} else if len(st.LicenseKey) > 0 {
		maskedKey = "****"
	}

	resp := map[string]interface{}{
		"status":                    st.Status,
		"trial_started_at":          st.TrialStartedAt.Format(time.RFC3339),
		"trial_expires_at":          st.TrialExpiresAt.Format(time.RFC3339),
		"license_key":               maskedKey,
		"owner_email":               st.OwnerEmail,
		"hardware_id":               st.HardwareID,
		"live_hardware_id":          HardwareFingerprint(),
		"valid":                     valid,
		"last_heartbeat_at":         formatTimePtr(st.LastHeartbeatAt),
		"last_supabase_response_at": formatTimePtr(st.LastSupabaseResponseAt),
		"last_supabase_error":       st.LastSupabaseError,
	}
	if st.ActivatedAt != nil {
		resp["activated_at"] = st.ActivatedAt.Format(time.RFC3339)
	}
	if st.ExpiresAt != nil {
		resp["expires_at"] = st.ExpiresAt.Format(time.RFC3339)
	}
	sendJSON(w, http.StatusOK, resp)
}

// Activate handles POST /api/admin/license/activate.
func (h *LicenseHandler) Activate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		LicenseKey string `json:"license_key"`
		OwnerEmail string `json:"owner_email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false, "message": "Invalid request body",
		})
		return
	}
	if req.LicenseKey == "" {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false, "message": "license_key is required",
		})
		return
	}
	if err := h.ActivateLicense(req.LicenseKey, req.OwnerEmail); err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false, "message": err.Error(),
		})
		return
	}
	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success": true, "message": "License activated",
	})
}

// Deactivate handles POST /api/admin/license/deactivate.
func (h *LicenseHandler) Deactivate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := h.DeactivateLicense(); err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false, "message": err.Error(),
		})
		return
	}
	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success": true, "message": "License deactivated",
	})
}

func formatTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}
