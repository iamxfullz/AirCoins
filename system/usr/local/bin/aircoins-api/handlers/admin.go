package handlers

import (
	"aircoins-api/models"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type AdminHandler struct {
	DB *sql.DB
}

// Login handles admin authentication
func (h *AdminHandler) Login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req models.LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid request body"})
		return
	}

	// Get user from database
	var user models.AdminUser
	err := h.DB.QueryRow(
		"SELECT id, username, password_hash FROM admin_users WHERE username = $1",
		req.Username,
	).Scan(&user.ID, &user.Username, &user.PasswordHash)

	if err == sql.ErrNoRows {
		sendJSON(w, http.StatusUnauthorized, models.LoginResponse{Success: false, Message: "Invalid credentials"})
		return
	} else if err != nil {
		log.Printf("Database error: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Internal error"})
		return
	}

	// Compare password with hash
	err = bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password))
	if err != nil {
		sendJSON(w, http.StatusUnauthorized, models.LoginResponse{Success: false, Message: "Invalid credentials"})
		return
	}

	// Update last login
	_, err = h.DB.Exec("UPDATE admin_users SET last_login = NOW() WHERE id = $1", user.ID)
	if err != nil {
		log.Printf("Failed to update last login: %v", err)
	}

	// Log the login
	h.logAction("INFO", "admin", "Admin login successful: "+req.Username)

	// Generate a real auth token
	token := GenerateToken(user.ID, user.Username)

	sendJSON(w, http.StatusOK, models.LoginResponse{
		Success: true,
		Message: "Login successful",
		Token:   token,
	})
}

// ChangePassword updates the password of the currently authenticated admin.
// The account is resolved from the Bearer token, so a session can only ever
// change its own password.
func (h *AdminHandler) ChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Resolve the admin from the token the AuthMiddleware already validated.
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	adminID, username, valid := ValidateToken(token)
	if !valid {
		sendJSON(w, http.StatusUnauthorized, models.APIResponse{Success: false, Message: "Unauthorized"})
		return
	}

	var req models.ChangePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid request body"})
		return
	}

	if req.CurrentPassword == "" || req.NewPassword == "" {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Current and new password are required"})
		return
	}
	if len(req.NewPassword) < 6 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "New password must be at least 6 characters"})
		return
	}

	var currentHash string
	err := h.DB.QueryRow("SELECT password_hash FROM admin_users WHERE id = $1", adminID).Scan(&currentHash)
	if err == sql.ErrNoRows {
		sendJSON(w, http.StatusNotFound, models.APIResponse{Success: false, Message: "Admin user not found"})
		return
	} else if err != nil {
		log.Printf("Error fetching admin password hash: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Internal error"})
		return
	}

	// Verify the current password. Use 400 (not 401) so the admin UI shows the
	// error inline instead of treating it as an expired session and logging out.
	if err := bcrypt.CompareHashAndPassword([]byte(currentHash), []byte(req.CurrentPassword)); err != nil {
		h.logAction("WARN", "admin", "Password change failed (wrong current password): "+username)
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Current password is incorrect"})
		return
	}

	newHash, err := GeneratePasswordHash(req.NewPassword)
	if err != nil {
		log.Printf("Error hashing new password: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to hash password"})
		return
	}

	_, err = h.DB.Exec("UPDATE admin_users SET password_hash = $1 WHERE id = $2", newHash, adminID)
	if err != nil {
		log.Printf("Error updating admin password: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to update password"})
		return
	}

	h.logAction("INFO", "admin", "Admin password changed: "+username)
	sendJSON(w, http.StatusOK, models.APIResponse{Success: true, Message: "Password changed successfully"})
}

// GetStats returns dashboard statistics
func (h *AdminHandler) GetStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var stats models.StatsResponse

	// Get today's stats
	var todayEarnings int
	err := h.DB.QueryRow(`
		SELECT COALESCE(total_earnings, 0), COALESCE(total_coins, 0), COALESCE(total_sessions, 0)
		FROM daily_stats WHERE date = CURRENT_DATE
	`).Scan(&todayEarnings, &stats.Today.Coins, &stats.Today.Sessions)
	if err == sql.ErrNoRows {
		todayEarnings = 0
	} else if err != nil {
		log.Printf("Error fetching today stats: %v", err)
	}
	stats.Today.Earnings = float64(todayEarnings)

	// Get total stats (all time)
	var totalEarnings int
	err = h.DB.QueryRow(`
		SELECT COALESCE(SUM(total_earnings), 0), COALESCE(SUM(total_coins), 0), COALESCE(SUM(total_sessions), 0)
		FROM daily_stats
	`).Scan(&totalEarnings, &stats.Total.Coins, &stats.Total.Sessions)
	if err != nil {
		log.Printf("Error fetching total stats: %v", err)
	}
	stats.Total.Earnings = float64(totalEarnings)

	// Get active sessions count (expiry is wall-clock based; the stored
	// remaining_seconds is only a snapshot, so trust status + expires_at)
	err = h.DB.QueryRow(`
		SELECT COUNT(*) FROM sessions
		WHERE status = 'active' AND (expires_at IS NULL OR expires_at > NOW())
	`).Scan(&stats.ActiveSessions)
	if err != nil {
		log.Printf("Error fetching active sessions: %v", err)
	}

	// Check if system is online (lighttpd running)
	stats.SystemOnline = checkLighttpdRunning()

	// Weekly stats when period=week is requested
	if r.URL.Query().Get("period") == "week" {
		rows, err := h.DB.Query(`
			SELECT date, total_earnings, total_coins, total_sessions
			FROM daily_stats
			WHERE date >= CURRENT_DATE - INTERVAL '7 days'
			ORDER BY date ASC
		`)
		if err != nil {
			log.Printf("Error fetching weekly stats: %v", err)
		} else {
			defer rows.Close()
			stats.WeeklyStats = make([]models.WeeklyStat, 0)
			for rows.Next() {
				var ws models.WeeklyStat
				if err := rows.Scan(&ws.Date, &ws.Earnings, &ws.Coins, &ws.Sessions); err != nil {
					log.Printf("Error scanning weekly stat: %v", err)
					continue
				}
				stats.WeeklyStats = append(stats.WeeklyStats, ws)
			}
		}
	}

	sendJSON(w, http.StatusOK, stats)
}

// GetSessions returns session history with filtering and pagination
func (h *AdminHandler) GetSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()

	// Parse pagination params
	limit := 25
	if l := q.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > 100 {
		limit = 100
	}

	offset := 0
	if o := q.Get("offset"); o != "" {
		if parsed, err := strconv.Atoi(o); err == nil && parsed >= 0 {
			offset = parsed
		}
	}

	// Build dynamic query
	where := []string{}
	args := []interface{}{}
	argIdx := 1

	// status filter
	if status := q.Get("status"); status != "" {
		where = append(where, fmt.Sprintf("status = $%d", argIdx))
		args = append(args, status)
		argIdx++
	}

	// date_from filter
	if df := q.Get("date_from"); df != "" {
		if t, err := time.Parse(time.RFC3339, df); err == nil {
			where = append(where, fmt.Sprintf("started_at >= $%d", argIdx))
			args = append(args, t)
			argIdx++
		} else if t, err := time.Parse("2006-01-02", df); err == nil {
			where = append(where, fmt.Sprintf("started_at >= $%d", argIdx))
			args = append(args, t)
			argIdx++
		}
	}

	// date_to filter
	if dt := q.Get("date_to"); dt != "" {
		if t, err := time.Parse(time.RFC3339, dt); err == nil {
			where = append(where, fmt.Sprintf("started_at <= $%d", argIdx))
			args = append(args, t)
			argIdx++
		} else if t, err := time.Parse("2006-01-02", dt); err == nil {
			// Include the whole day
			t = t.Add(24*time.Hour - time.Second)
			where = append(where, fmt.Sprintf("started_at <= $%d", argIdx))
			args = append(args, t)
			argIdx++
		}
	}

	// search filter (client_ip or client_mac ILIKE)
	if search := q.Get("search"); search != "" {
		where = append(where, fmt.Sprintf("(client_ip ILIKE $%d OR client_mac ILIKE $%d)", argIdx, argIdx))
		args = append(args, "%"+search+"%")
		argIdx++
	}

	whereClause := ""
	if len(where) > 0 {
		whereClause = "WHERE " + strings.Join(where, " AND ")
	}

	// Count query
	countQuery := "SELECT COUNT(*) FROM sessions " + whereClause
	var total int
	countArgs := make([]interface{}, len(args))
	copy(countArgs, args)
	err := h.DB.QueryRow(countQuery, countArgs...).Scan(&total)
	if err != nil {
		log.Printf("Error counting sessions: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to count sessions"})
		return
	}

	// Data query. remaining_seconds is computed live from expires_at for
	// active sessions so the admin table shows a real countdown.
	// paused_at is included so the UI can render a "paused" badge.
	// shaped_mbps is included so the UI can render a speed override input.
	// session_token is included so the UI can render the roaming token.
	dataQuery := fmt.Sprintf(`
		SELECT id, client_ip, client_mac, coins_inserted, total_seconds,
		       CASE WHEN status = 'active' AND expires_at IS NOT NULL
		            THEN GREATEST(0, EXTRACT(EPOCH FROM (expires_at - NOW())))::int
		            ELSE COALESCE(remaining_seconds, 0) END AS remaining_seconds,
		       status, started_at, activated_at, expired_at, expires_at, paused_at,
		       shaped_mbps, COALESCE(session_token, '')
		FROM sessions
		%s
		ORDER BY started_at DESC
		LIMIT $%d OFFSET $%d
	`, whereClause, argIdx, argIdx+1)
	args = append(args, limit, offset)

	rows, err := h.DB.Query(dataQuery, args...)
	if err != nil {
		log.Printf("Error fetching sessions: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to fetch sessions"})
		return
	}
	defer rows.Close()

	// Load qdisc rules once for all sessions so we can attach qdisc_info.
	qdiscRules := loadQdiscRules(h.DB)

	// Pre-compute lifetime coin totals per MAC (Option A: single aggregation query).
	lifetimeCoins := make(map[string]int)
	ltRows, ltErr := h.DB.Query(`
		SELECT client_mac, COALESCE(SUM(coins_inserted), 0)
		FROM sessions
		WHERE client_mac IS NOT NULL AND client_mac <> '' AND client_mac <> '-'
		GROUP BY client_mac
	`)
	if ltErr != nil {
		log.Printf("Error fetching lifetime coin totals: %v", ltErr)
	} else {
		for ltRows.Next() {
			var mac string
			var total int
			if err := ltRows.Scan(&mac, &total); err == nil {
				lifetimeCoins[mac] = total
			}
		}
		ltRows.Close()
	}

	sessions := make([]models.Session, 0)
	for rows.Next() {
		var s models.Session
		err := rows.Scan(
			&s.ID, &s.ClientIP, &s.ClientMAC, &s.CoinsInserted, &s.TotalSeconds,
			&s.RemainingSeconds, &s.Status, &s.StartedAt, &s.ActivatedAt, &s.ExpiredAt, &s.ExpiresAt,
			&s.PausedAt, &s.ShapedMbps, &s.SessionToken,
		)
		if err != nil {
			log.Printf("Error scanning session: %v", err)
			continue
		}
		// Attach lifetime coin total for this session's MAC.
		if s.ClientMAC != "" && s.ClientMAC != "-" {
			s.TotalCoinsLifetime = lifetimeCoins[s.ClientMAC]
		}
		// Attach qdisc_info for this session's portal interface.
		iface := ifaceForClientIP(h.DB, s.ClientIP)
		if iface != "" {
			if rule, ok := qdiscRules[iface]; ok {
				s.QdiscInfo = &models.QdiscInfo{
					Type:          rule.Qdisc,
					PerDeviceMbps: rule.PerDeviceBwMbps,
				}
			}
		}
		// Resolve client hostname from IP/MAC (cached, non-blocking).
		s.Hostname = ResolveHostname(s.ClientIP, s.ClientMAC)
		sessions = append(sessions, s)
	}

	sendJSON(w, http.StatusOK, models.PaginatedResponse{
		Success: true,
		Data:    sessions,
		Total:   total,
		Limit:   limit,
		Offset:  offset,
	})
}

// GetSession returns a single session by ID
func (h *AdminHandler) GetSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract session ID from URL path: /api/admin/sessions/{id}
	path := strings.TrimPrefix(r.URL.Path, "/api/admin/sessions/")
	id, err := strconv.Atoi(path)
	if err != nil || id <= 0 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid session ID"})
		return
	}

	var s models.Session
	err = h.DB.QueryRow(`
		SELECT id, client_ip, client_mac, coins_inserted, total_seconds,
		       CASE WHEN status = 'active' AND expires_at IS NOT NULL
		            THEN GREATEST(0, EXTRACT(EPOCH FROM (expires_at - NOW())))::int
		            ELSE COALESCE(remaining_seconds, 0) END AS remaining_seconds,
		       status, started_at, activated_at, expired_at, expires_at
		FROM sessions
		WHERE id = $1
	`, id).Scan(
		&s.ID, &s.ClientIP, &s.ClientMAC, &s.CoinsInserted, &s.TotalSeconds,
		&s.RemainingSeconds, &s.Status, &s.StartedAt, &s.ActivatedAt, &s.ExpiredAt, &s.ExpiresAt,
	)

	if err == sql.ErrNoRows {
		sendJSON(w, http.StatusNotFound, models.APIResponse{Success: false, Message: "Session not found"})
		return
	} else if err != nil {
		log.Printf("Error fetching session: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to fetch session"})
		return
	}

	sendJSON(w, http.StatusOK, models.APIResponse{Success: true, Data: s})
}

// Settings handles system settings (GET and POST)
func (h *AdminHandler) Settings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.getSettings(w, r)
	case http.MethodPost:
		h.updateSettings(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *AdminHandler) getSettings(w http.ResponseWriter, r *http.Request) {
	rows, err := h.DB.Query("SELECT key, value FROM system_settings")
	if err != nil {
		log.Printf("Error fetching settings: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to fetch settings"})
		return
	}
	defer rows.Close()

	settings := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			log.Printf("Error scanning setting: %v", err)
			continue
		}
		settings[key] = value
	}

	sendJSON(w, http.StatusOK, models.APIResponse{Success: true, Data: settings})
}

func (h *AdminHandler) updateSettings(w http.ResponseWriter, r *http.Request) {
	var req models.SettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid request body"})
		return
	}

	for key, value := range req.Settings {
		_, err := h.DB.Exec(`
			INSERT INTO system_settings (key, value, updated_at) 
			VALUES ($1, $2, NOW())
			ON CONFLICT (key) DO UPDATE SET value = $2, updated_at = NOW()
		`, key, value)
		if err != nil {
			log.Printf("Error updating setting %s: %v", key, err)
		}
	}

	h.logAction("INFO", "admin", "System settings updated")
	sendJSON(w, http.StatusOK, models.APIResponse{Success: true, Message: "Settings updated"})
}

// GetLogs returns system logs
func (h *AdminHandler) GetLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rows, err := h.DB.Query(`
		SELECT id, level, component, message, created_at
		FROM system_logs
		ORDER BY created_at DESC
		LIMIT 100
	`)
	if err != nil {
		log.Printf("Error fetching logs: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to fetch logs"})
		return
	}
	defer rows.Close()

	var logs []models.SystemLog
	for rows.Next() {
		var l models.SystemLog
		if err := rows.Scan(&l.ID, &l.Level, &l.Component, &l.Message, &l.CreatedAt); err != nil {
			log.Printf("Error scanning log: %v", err)
			continue
		}
		logs = append(logs, l)
	}

	sendJSON(w, http.StatusOK, models.APIResponse{Success: true, Data: logs})
}

// GetCoinEvents returns recent coin events
func (h *AdminHandler) GetCoinEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse limit query param (default 10, max 100)
	limit := 10
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > 100 {
		limit = 100
	}

	rows, err := h.DB.Query(`
		SELECT id, coin_value, detected_at, processed, session_id
		FROM coin_events
		ORDER BY detected_at DESC
		LIMIT $1
	`, limit)
	if err != nil {
		log.Printf("Error fetching coin events: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to fetch coin events"})
		return
	}
	defer rows.Close()

	events := make([]models.CoinEvent, 0)
	for rows.Next() {
		var e models.CoinEvent
		if err := rows.Scan(&e.ID, &e.CoinValue, &e.DetectedAt, &e.Processed, &e.SessionID); err != nil {
			log.Printf("Error scanning coin event: %v", err)
			continue
		}
		events = append(events, e)
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"events": events,
	})
}

// Helper function to log actions
func (h *AdminHandler) logAction(level, component, message string) {
	_, err := h.DB.Exec(`
		INSERT INTO system_logs (level, component, message, created_at)
		VALUES ($1, $2, $3, NOW())
	`, level, component, message)
	if err != nil {
		log.Printf("Failed to log action: %v", err)
	}
}

// Helper to check if lighttpd is running
func checkLighttpdRunning() bool {
	cmd := exec.Command("systemctl", "is-active", "lighttpd")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "active"
}

// Helper to send JSON response
func sendJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// GeneratePasswordHash creates a bcrypt hash for a password
func GeneratePasswordHash(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(bytes), err
}

func init() {
	// This can be used to generate password hashes for new admin users
	_ = GeneratePasswordHash
	_ = time.Now
}
