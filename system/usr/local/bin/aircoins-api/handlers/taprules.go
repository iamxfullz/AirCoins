package handlers

import (
	"aircoins-api/models"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// taprules.go implements the portal-side anti-abuse (tap counter + per-MAC
// ban) and pause-rule settings. The ban storage lives in the client_bans
// table; sliding-window tap counters in tap_activity. Both are ephemeral
// and cleaned up by the session expiry enforcer goroutine.

// TapRules is the JSON shape stored in system_settings under key
// 'portal_tap_rules'. Defaults match migration 010's seed.
type TapRules struct {
	MaxTaps       int `json:"max_taps"`
	WindowSeconds int `json:"window_seconds"`
	BanSeconds    int `json:"ban_seconds"`
}

// PauseRules is the JSON shape stored under key 'portal_pause_rules'.
// pause_limit = 0 means unlimited pauses per session.
type PauseRules struct {
	PauseLimit int `json:"pause_limit"`
}

var defaultTapRules = TapRules{MaxTaps: 5, WindowSeconds: 60, BanSeconds: 300}
var defaultPauseRules = PauseRules{PauseLimit: 0}

// loadTapRules reads portal_tap_rules from system_settings, falling back
// to the default if the row is missing or unparseable.
func loadTapRules(db *sql.DB) TapRules {
	r := defaultTapRules
	var raw string
	if err := db.QueryRow(`SELECT value FROM system_settings WHERE key='portal_tap_rules'`).Scan(&raw); err != nil {
		return r
	}
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		log.Printf("loadTapRules: unmarshal failed: %v (using defaults)", err)
		return defaultTapRules
	}
	if r.MaxTaps <= 0 || r.WindowSeconds <= 0 || r.BanSeconds <= 0 {
		return defaultTapRules
	}
	return r
}

// loadPauseRules reads portal_pause_rules from system_settings.
func loadPauseRules(db *sql.DB) PauseRules {
	r := defaultPauseRules
	var raw string
	if err := db.QueryRow(`SELECT value FROM system_settings WHERE key='portal_pause_rules'`).Scan(&raw); err != nil {
		return r
	}
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		log.Printf("loadPauseRules: unmarshal failed: %v (using defaults)", err)
		return defaultPauseRules
	}
	if r.PauseLimit < 0 {
		return defaultPauseRules
	}
	return r
}

// upsertSetting writes or updates a system_settings row.
func upsertSetting(db *sql.DB, key, value, description string) error {
	_, err := db.Exec(`
		INSERT INTO system_settings (key, value, description)
		VALUES ($1, $2, $3)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()
	`, key, value, description)
	return err
}

// activeBan returns the still-active ban row for a MAC, or ErrNoRows.
// An expired ban is auto-removed (belt-and-braces on top of the ticker).
func activeBan(db *sql.DB, mac string) (time.Time, string, int, bool) {
	var until time.Time
	var reason sql.NullString
	var attempts sql.NullInt64
	err := db.QueryRow(`
		SELECT banned_until, reason, attempts_in_window
		FROM client_bans
		WHERE client_mac = $1 AND banned_until > NOW()
	`, mac).Scan(&until, &reason, &attempts)
	if err == sql.ErrNoRows {
		// Belt-and-braces: any stale row should already be cleaned up
		// by the 30s ticker, but clear it on sight to be safe.
		db.Exec(`DELETE FROM client_bans WHERE client_mac = $1`, mac)
		return time.Time{}, "", 0, false
	}
	if err != nil {
		log.Printf("activeBan: query failed: %v", err)
		return time.Time{}, "", 0, false
	}
	return until, reason.String, int(attempts.Int64), true
}

// setBan upserts a client_bans row for a MAC.
func setBan(db *sql.DB, mac string, until time.Time, reason string, attempts int) {
	_, err := db.Exec(`
		INSERT INTO client_bans (client_mac, banned_until, reason, attempts_in_window, updated_at)
		VALUES ($1, $2::timestamptz, $3, $4::int, NOW())
		ON CONFLICT (client_mac) DO UPDATE SET
			banned_until = EXCLUDED.banned_until,
			reason = EXCLUDED.reason,
			attempts_in_window = EXCLUDED.attempts_in_window,
			updated_at = NOW()
	`, mac, until, reason, attempts)
	if err != nil {
		log.Printf("setBan: upsert failed: %v", err)
	}
}

// clearBan removes a ban row (admin unban or auto-expiry).
func clearBan(db *sql.DB, mac string) {
	if _, err := db.Exec(`DELETE FROM client_bans WHERE client_mac = $1`, mac); err != nil {
		log.Printf("clearBan: %v", err)
	}
}

// recordTap increments the sliding-window tap counter for a MAC and
// returns the counter value in the current window. Uses an atomic
// INSERT ... ON CONFLICT pattern so concurrent taps from the same MAC
// never lose increments (the previous two-step UPDATE+INSERT had a race
// where the ON CONFLICT fallback unconditionally reset counter=1).
func recordTap(db *sql.DB, mac string, windowSeconds int) int {
	var counter int
	err := db.QueryRow(`
		INSERT INTO tap_activity (client_mac, counter, window_started_at)
		VALUES ($1, 1, NOW())
		ON CONFLICT (client_mac) DO UPDATE SET
		    counter = CASE
		        WHEN EXTRACT(EPOCH FROM (NOW() - tap_activity.window_started_at)) > $2::int
		            THEN 1
		        ELSE tap_activity.counter + 1
		    END,
		    window_started_at = CASE
		        WHEN EXTRACT(EPOCH FROM (NOW() - tap_activity.window_started_at)) > $2::int
		            THEN NOW()
		        ELSE tap_activity.window_started_at
		    END,
		    updated_at = NOW()
		RETURNING counter
	`, mac, windowSeconds).Scan(&counter)
	if err != nil {
		log.Printf("recordTap: %v", err)
		return 0
	}
	return counter
}

// ============================================
// TAP RULES (public GET + admin POST)
// ============================================

// GetTapRules returns the current tap_rules JSON. PUBLIC — the portal
// uses it to show the tap limits next to the INSERT COIN button.
func (h *AppearanceHandler) GetTapRules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rules := loadTapRules(h.DB)
	sendJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": rules})
}

// HandleTapRules dispatches GET (read) and POST (save) on the admin path.
func (h *AppearanceHandler) HandleTapRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.GetTapRules(w, r)
	case http.MethodPost:
		h.SaveTapRules(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// SaveTapRules upserts portal_tap_rules. ADMIN only.
func (h *AppearanceHandler) SaveTapRules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req TapRules
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid JSON: " + err.Error()})
		return
	}
	if req.MaxTaps <= 0 || req.WindowSeconds <= 0 || req.BanSeconds <= 0 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{
			Success: false,
			Message: "max_taps, window_seconds, and ban_seconds must all be positive",
		})
		return
	}

	b, _ := json.Marshal(req)
	if err := upsertSetting(h.DB, "portal_tap_rules", string(b),
		"INSERT COIN anti-abuse limits (per MAC, per window)"); err != nil {
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to save tap rules"})
		return
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": req})
}

// ============================================
// PAUSE RULES (admin GET + POST)
// ============================================

// GetPauseRules returns the current pause_rules JSON. ADMIN only.
func (h *AppearanceHandler) GetPauseRules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rules := loadPauseRules(h.DB)
	sendJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": rules})
}

// HandlePauseRules dispatches GET (read) and POST (save) on the same path.
func (h *AppearanceHandler) HandlePauseRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.GetPauseRules(w, r)
	case http.MethodPost:
		h.SavePauseRules(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// SavePauseRules upserts portal_pause_rules. ADMIN only.
func (h *AppearanceHandler) SavePauseRules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req PauseRules
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid JSON: " + err.Error()})
		return
	}
	if req.PauseLimit < 0 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{
			Success: false,
			Message: "pause_limit must be >= 0 (0 means unlimited)",
		})
		return
	}

	b, _ := json.Marshal(req)
	if err := upsertSetting(h.DB, "portal_pause_rules", string(b),
		"Session pause rules (pause_limit=0 means unlimited)"); err != nil {
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to save pause rules"})
		return
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": req})
}

// ============================================
// BANS (portal GET + admin list/unban)
// ============================================

// BanStatus returns the caller's active ban (or {banned:false}).
// PUBLIC — polled by the portal to drive the ban overlay.
func (h *SessionHandler) BanStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	clientIP := clientIPFromRequest(r)
	mac := neighborMAC(clientIP)

	until, reason, attempts, banned := activeBan(h.DB, mac)
	resp := map[string]interface{}{
		"banned": banned,
		"mac":    mac,
		"ip":     clientIP,
	}
	if banned {
		resp["banned_until"] = until.Format(time.RFC3339)
		resp["reason"] = reason
		resp["attempts_in_window"] = attempts
	}
	sendJSON(w, http.StatusOK, resp)
}

// ListBans returns every currently-active ban (admin UI table). ADMIN only.
func (h *AppearanceHandler) ListBans(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rows, err := h.DB.Query(`
		SELECT client_mac, banned_until, COALESCE(reason,''), COALESCE(attempts_in_window,0), created_at, updated_at
		FROM client_bans
		WHERE banned_until > NOW()
		ORDER BY banned_until DESC
	`)
	if err != nil {
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to list bans: " + err.Error()})
		return
	}
	defer rows.Close()

	type BanRow struct {
		MAC       string `json:"client_mac"`
		Until     string `json:"banned_until"`
		Reason    string `json:"reason"`
		Attempts  int    `json:"attempts_in_window"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
	}
	out := []BanRow{}
	for rows.Next() {
		var b BanRow
		var until, createdAt, updatedAt time.Time
		if err := rows.Scan(&b.MAC, &until, &b.Reason, &b.Attempts, &createdAt, &updatedAt); err != nil {
			log.Printf("ListBans: scan: %v", err)
			continue
		}
		b.Until = until.Format(time.RFC3339)
		b.CreatedAt = createdAt.Format(time.RFC3339)
		b.UpdatedAt = updatedAt.Format(time.RFC3339)
		out = append(out, b)
	}
	sendJSON(w, http.StatusOK, map[string]interface{}{"success": true, "bans": out})
}

// macRegex validates a MAC address format (XX:XX:XX:XX:XX:XX).
var macRegex = regexp.MustCompile(`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`)

// UnbanByPath removes a single ban. Path: /api/admin/portal/ban/<mac>.
// ADMIN only. F8: MAC is validated against the canonical format before
// use to prevent log-injection via the path segment.
func (h *AppearanceHandler) UnbanByPath(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	mac := strings.TrimPrefix(r.URL.Path, "/api/admin/portal/ban/")
	mac = strings.TrimSpace(mac)
	if mac == "" {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "mac is required"})
		return
	}

	// F8: Validate MAC format to prevent log-injection via path segment.
	if !macRegex.MatchString(mac) {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid MAC format (expected XX:XX:XX:XX:XX:XX)"})
		return
	}

	clearBan(h.DB, mac)
	logAction(h.DB, "INFO", "portal", "Admin unbanned MAC "+mac)
	sendJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": "Ban removed"})
}

// CleanExpiredBans is called by the expiry enforcer ticker — removes
// every ban whose banned_until has passed. Cheap (banned_until index).
func CleanExpiredBans(db *sql.DB) int {
	res, err := db.Exec(`DELETE FROM client_bans WHERE banned_until <= NOW()`)
	if err != nil {
		log.Printf("CleanExpiredBans: %v", err)
		return 0
	}
	n, _ := res.RowsAffected()
	return int(n)
}
