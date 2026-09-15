package handlers

import (
	"aircoins-api/models"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
)

// SessionAdminHandler provides CRUD endpoints for individual admin session
// management: View (GET), Edit (PATCH), Delete (DELETE).
type SessionAdminHandler struct {
	DB *sql.DB
}

// ============================================
// GET /api/admin/sessions/<id>  (detail + coin_events)
// ============================================

// GetSessionDetail returns ALL session fields plus the last 20 coin_events.
func (h *SessionAdminHandler) GetSessionDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id, err := parseSessionID(r.URL.Path)
	if err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid session ID"})
		return
	}

	var s models.Session
	err = h.DB.QueryRow(`
		SELECT id, COALESCE(client_ip, ''), COALESCE(client_mac, ''),
		       coins_inserted, total_seconds,
		       CASE WHEN status = 'active' AND expires_at IS NOT NULL
		            THEN GREATEST(0, EXTRACT(EPOCH FROM (expires_at - NOW())))::int
		            ELSE COALESCE(remaining_seconds, 0) END AS remaining_seconds,
		       status, started_at, activated_at, expired_at, expires_at,
		       paused_at, remaining_seconds_at_pause,
		       COALESCE(pause_count, 0),
		       shaped_mbps, COALESCE(session_token, ''), created_at
		FROM sessions
		WHERE id = $1
	`, id).Scan(
		&s.ID, &s.ClientIP, &s.ClientMAC,
		&s.CoinsInserted, &s.TotalSeconds, &s.RemainingSeconds,
		&s.Status, &s.StartedAt, &s.ActivatedAt, &s.ExpiredAt, &s.ExpiresAt,
		&s.PausedAt, &s.RemainingSecondsAtPause, &s.PauseCount,
		&s.ShapedMbps, &s.SessionToken, &s.CreatedAt,
	)
	if err == sql.ErrNoRows {
		sendJSON(w, http.StatusNotFound, models.APIResponse{Success: false, Message: "Session not found"})
		return
	} else if err != nil {
		log.Printf("GetSessionDetail: query failed for id=%d: %v", id, err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to fetch session"})
		return
	}

	// Resolve hostname if not stored
	if s.Hostname == "" {
		s.Hostname = ResolveHostname(s.ClientIP, s.ClientMAC)
	}

	// Fetch last 20 coin_events for this session
	coinRows, err := h.DB.Query(`
		SELECT id, coin_value, detected_at, processed, session_id
		FROM coin_events
		WHERE session_id = $1
		ORDER BY detected_at DESC
		LIMIT 20
	`, id)
	if err != nil {
		log.Printf("GetSessionDetail: coin_events query failed for id=%d: %v", id, err)
		// Non-fatal: return session without coin events
	}
	coinEvents := make([]models.CoinEvent, 0)
	if coinRows != nil {
		defer coinRows.Close()
		for coinRows.Next() {
			var e models.CoinEvent
			if err := coinRows.Scan(&e.ID, &e.CoinValue, &e.DetectedAt, &e.Processed, &e.SessionID); err != nil {
				log.Printf("GetSessionDetail: scan coin event: %v", err)
				continue
			}
			coinEvents = append(coinEvents, e)
		}
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"data": map[string]interface{}{
			"session":     s,
			"coin_events": coinEvents,
		},
	})
}

// ============================================
// PATCH /api/admin/sessions/<id>  (partial update)
// ============================================

// patchSessionRequest holds the optional fields accepted by PATCH.
// Only non-nil fields are updated.
type patchSessionRequest struct {
	ClientIP         *string `json:"client_ip"`
	ClientMAC        *string `json:"client_mac"`
	CoinsInserted    *int    `json:"coins_inserted"`
	TotalSeconds     *int    `json:"total_seconds"`
	RemainingSeconds *int    `json:"remaining_seconds"`
	Status           *string `json:"status"`
	ShapedMbps       *int    `json:"shaped_mbps"`
	SessionToken     *string `json:"session_token"`
}

// UpdateSession applies a partial update to a session row.
// On status change from active → expired/cancelled, it revokes iptables
// access and removes the per-device tc class (same as End handler).
func (h *SessionAdminHandler) UpdateSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id, err := parseSessionID(r.URL.Path)
	if err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid session ID"})
		return
	}

	var req patchSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid request body"})
		return
	}

	// --- Validate fields --------------------------------------------------
	if req.ClientIP != nil {
		if net.ParseIP(*req.ClientIP) == nil {
			sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid IP address format"})
			return
		}
	}
	if req.ClientMAC != nil {
		if !macRegex.MatchString(*req.ClientMAC) {
			sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid MAC address format (expected XX:XX:XX:XX:XX:XX)"})
			return
		}
	}
	if req.CoinsInserted != nil && *req.CoinsInserted < 0 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "coins_inserted must be non-negative"})
		return
	}
	if req.TotalSeconds != nil && *req.TotalSeconds < 0 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "total_seconds must be non-negative"})
		return
	}
	if req.RemainingSeconds != nil && *req.RemainingSeconds < 0 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "remaining_seconds must be non-negative"})
		return
	}
	if req.ShapedMbps != nil && *req.ShapedMbps < 0 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "shaped_mbps must be non-negative"})
		return
	}
	validStatuses := map[string]bool{"active": true, "expired": true, "cancelled": true, "paused": true}
	if req.Status != nil && !validStatuses[*req.Status] {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "status must be one of: active, expired, cancelled, paused"})
		return
	}

	// --- Fetch current row to detect status transition --------------------
	var currentStatus, currentMAC, currentIP string
	err = h.DB.QueryRow(`
		SELECT COALESCE(status, ''), COALESCE(client_mac, ''), COALESCE(client_ip, '')
		FROM sessions WHERE id = $1
	`, id).Scan(&currentStatus, &currentMAC, &currentIP)
	if err == sql.ErrNoRows {
		sendJSON(w, http.StatusNotFound, models.APIResponse{Success: false, Message: "Session not found"})
		return
	} else if err != nil {
		log.Printf("UpdateSession: fetch failed for id=%d: %v", id, err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to fetch session"})
		return
	}

	// --- Build dynamic UPDATE with COALESCE-style logic -------------------
	setClauses := []string{}
	args := []interface{}{}
	argIdx := 1

	if req.ClientIP != nil {
		setClauses = append(setClauses, fmt.Sprintf("client_ip = $%d", argIdx))
		args = append(args, *req.ClientIP)
		argIdx++
	}
	if req.ClientMAC != nil {
		setClauses = append(setClauses, fmt.Sprintf("client_mac = $%d", argIdx))
		args = append(args, *req.ClientMAC)
		argIdx++
	}
	if req.CoinsInserted != nil {
		setClauses = append(setClauses, fmt.Sprintf("coins_inserted = $%d", argIdx))
		args = append(args, *req.CoinsInserted)
		argIdx++
	}
	if req.TotalSeconds != nil {
		setClauses = append(setClauses, fmt.Sprintf("total_seconds = $%d", argIdx))
		args = append(args, *req.TotalSeconds)
		argIdx++
	}
	if req.RemainingSeconds != nil {
		setClauses = append(setClauses, fmt.Sprintf("remaining_seconds = $%d", argIdx))
		args = append(args, *req.RemainingSeconds)
		argIdx++
	}
	if req.Status != nil {
		setClauses = append(setClauses, fmt.Sprintf("status = $%d", argIdx))
		args = append(args, *req.Status)
		argIdx++
		// If transitioning to a terminal state, also set expired_at and expires_at
		if *req.Status == "expired" || *req.Status == "cancelled" {
			setClauses = append(setClauses, "expired_at = NOW()", "expires_at = NOW()", "remaining_seconds = 0")
		}
	}
	if req.ShapedMbps != nil {
		if *req.ShapedMbps == 0 {
			setClauses = append(setClauses, "shaped_mbps = NULL")
		} else {
			setClauses = append(setClauses, fmt.Sprintf("shaped_mbps = $%d", argIdx))
			args = append(args, *req.ShapedMbps)
			argIdx++
		}
	}
	if req.SessionToken != nil {
		if *req.SessionToken == "" {
			setClauses = append(setClauses, "session_token = NULL")
		} else {
			setClauses = append(setClauses, fmt.Sprintf("session_token = $%d", argIdx))
			args = append(args, *req.SessionToken)
			argIdx++
		}
	}

	if len(setClauses) == 0 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "No fields to update"})
		return
	}

	query := fmt.Sprintf(`UPDATE sessions SET %s WHERE id = $%d`, strings.Join(setClauses, ", "), argIdx)
	args = append(args, id)

	_, err = h.DB.Exec(query, args...)
	if err != nil {
		log.Printf("UpdateSession: update failed for id=%d: %v", id, err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to update session"})
		return
	}

	// --- Handle status transition: active → inactive ----------------------
	// Same cleanup as the End handler: revoke iptables and remove tc class.
	if req.Status != nil && currentStatus == "active" && (*req.Status == "expired" || *req.Status == "cancelled") {
		mac := currentMAC
		ip := currentIP
		if req.ClientMAC != nil {
			mac = *req.ClientMAC
		}
		if req.ClientIP != nil {
			ip = *req.ClientIP
		}
		runCaptiveRules(h.DB, "unauth", mac, "admin-edit-status")
		EnsurePerDeviceClass(h.DB, "", mac, ip, "remove")
		logAction(h.DB, "INFO", "session", fmt.Sprintf("Session %d status changed active→%s by admin (unauth + qdisc remove)", id, *req.Status))
	}

	// --- Return updated session -------------------------------------------
	var updated models.Session
	err = h.DB.QueryRow(`
		SELECT id, COALESCE(client_ip, ''), COALESCE(client_mac, ''),
		       coins_inserted, total_seconds,
		       CASE WHEN status = 'active' AND expires_at IS NOT NULL
		            THEN GREATEST(0, EXTRACT(EPOCH FROM (expires_at - NOW())))::int
		            ELSE COALESCE(remaining_seconds, 0) END AS remaining_seconds,
		       status, started_at, activated_at, expired_at, expires_at,
		       paused_at, remaining_seconds_at_pause,
		       COALESCE(pause_count, 0),
		       shaped_mbps, COALESCE(session_token, ''), created_at
		FROM sessions WHERE id = $1
	`, id).Scan(
		&updated.ID, &updated.ClientIP, &updated.ClientMAC,
		&updated.CoinsInserted, &updated.TotalSeconds, &updated.RemainingSeconds,
		&updated.Status, &updated.StartedAt, &updated.ActivatedAt, &updated.ExpiredAt, &updated.ExpiresAt,
		&updated.PausedAt, &updated.RemainingSecondsAtPause, &updated.PauseCount,
		&updated.ShapedMbps, &updated.SessionToken, &updated.CreatedAt,
	)
	if err != nil {
		log.Printf("UpdateSession: re-fetch failed for id=%d: %v", id, err)
		sendJSON(w, http.StatusOK, models.APIResponse{Success: true, Message: "Session updated"})
		return
	}

	// Resolve hostname for the returned session
	updated.Hostname = ResolveHostname(updated.ClientIP, updated.ClientMAC)

	logAction(h.DB, "INFO", "session", fmt.Sprintf("Session %d updated by admin", id))
	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"data": map[string]interface{}{
			"session": updated,
		},
	})
}

// ============================================
// DELETE /api/admin/sessions/<id>
// ============================================

// DeleteSession permanently removes a session row: deletes its coin_events,
// detaches any vouchers that reference it (voucher sale history is preserved),
// then deletes the session row itself. Daily stats aggregates are NOT touched
// (revenue reports preserve history).
func (h *SessionAdminHandler) DeleteSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id, err := parseSessionID(r.URL.Path)
	if err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid session ID"})
		return
	}

	// Fetch current session state
	var status, mac, clientIP string
	err = h.DB.QueryRow(`
		SELECT COALESCE(status, ''), COALESCE(client_mac, ''), COALESCE(client_ip, '')
		FROM sessions WHERE id = $1
	`, id).Scan(&status, &mac, &clientIP)
	if err == sql.ErrNoRows {
		sendJSON(w, http.StatusNotFound, models.APIResponse{Success: false, Message: "Session not found"})
		return
	} else if err != nil {
		log.Printf("DeleteSession: fetch failed for id=%d: %v", id, err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to fetch session"})
		return
	}

	// Step 1: If session is active, revoke iptables and remove tc class.
	if status == "active" {
		runCaptiveRules(h.DB, "unauth", mac, "admin-delete")
		EnsurePerDeviceClass(h.DB, "", mac, clientIP, "remove")
	}

	// Step 2: Hard-delete — remove the session row and its coin events in a
	// transaction. Vouchers keep their sale history (session_id is nulled out
	// rather than cascade-deleted). The UI promises "permanently removes the
	// row and its coin events", so the row must actually disappear from the
	// sessions list after delete.
	tx, err := h.DB.Begin()
	if err != nil {
		log.Printf("DeleteSession: begin tx failed for id=%d: %v", id, err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to delete session"})
		return
	}
	if _, err = tx.Exec(`DELETE FROM coin_events WHERE session_id = $1`, id); err != nil {
		tx.Rollback()
		log.Printf("DeleteSession: coin_events delete failed for id=%d: %v", id, err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to delete session"})
		return
	}
	if _, err = tx.Exec(`UPDATE vouchers SET session_id = NULL WHERE session_id = $1`, id); err != nil {
		tx.Rollback()
		log.Printf("DeleteSession: voucher detach failed for id=%d: %v", id, err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to delete session"})
		return
	}
	res, err := tx.Exec(`DELETE FROM sessions WHERE id = $1`, id)
	if err != nil {
		tx.Rollback()
		log.Printf("DeleteSession: delete failed for id=%d: %v", id, err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to delete session"})
		return
	}
	if err = tx.Commit(); err != nil {
		log.Printf("DeleteSession: commit failed for id=%d: %v", id, err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to delete session"})
		return
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		sendJSON(w, http.StatusNotFound, models.APIResponse{Success: false, Message: "Session not found"})
		return
	}

	logAction(h.DB, "INFO", "session", fmt.Sprintf("Session %d soft-deleted by admin (mac=%s, ip=%s)", id, mac, clientIP))
	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success":    true,
		"deleted":    true,
		"session_id": id,
		"mac":        mac,
		"ip":         clientIP,
	})
}

// ============================================
// HELPERS
// ============================================

// parseSessionID extracts the session ID from /api/admin/sessions/<id>[...]
func parseSessionID(path string) (int, error) {
	trimmed := strings.TrimPrefix(path, "/api/admin/sessions/")
	// Strip any trailing path segments (e.g. /shape)
	if idx := strings.Index(trimmed, "/"); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	id, err := strconv.Atoi(trimmed)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid session ID: %s", trimmed)
	}
	return id, nil
}

// DispatchSessionAdmin routes PATCH and DELETE requests to the appropriate
// handler method based on HTTP verb. Used by main.go's mux routing.
func (h *SessionAdminHandler) Dispatch(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.GetSessionDetail(w, r)
	case http.MethodPatch:
		h.UpdateSession(w, r)
	case http.MethodDelete:
		h.DeleteSession(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}
