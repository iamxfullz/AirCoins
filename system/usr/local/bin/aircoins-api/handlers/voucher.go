package handlers

import (
	"aircoins-api/models"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// voucher.go implements the PRE-PAID VOUCHER SYSTEM:
//   - Admin CRUD: batch-generate unique 6-char alphanumeric codes, list,
//     edit (duration/status/notes) and delete vouchers.
//   - Public redemption: POST /api/session/redeem credits the voucher's
//     minutes to the CALLING device and binds the voucher to the
//     resulting session + session_token so the subscriber can roam
//     between SSIDs (the same token-roaming used by coin sessions).
//
// A monthly subscription voucher is simply a voucher with
// duration_minutes = 43200 (30 days) and plan = 'monthly'.

type VoucherHandler struct {
	DB *sql.DB
}

// voucherCodeAlphabet excludes visually ambiguous glyphs (0/O, 1/I/L) so
// printed codes stay readable.
const voucherCodeAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"
const voucherCodeLen = 6

const maxVoucherBatch = 100
const maxVoucherMinutes = 525600 // 1 year

// generateVoucherCode returns a fresh unique 6-char code.
func generateVoucherCode(db *sql.DB) (string, error) {
	for attempt := 0; attempt < 50; attempt++ {
		buf := make([]byte, voucherCodeLen)
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		var code strings.Builder
		for _, b := range buf {
			code.WriteByte(voucherCodeAlphabet[int(b)%len(voucherCodeAlphabet)])
		}
		var one int
		err := db.QueryRow(`SELECT 1 FROM vouchers WHERE code = $1`, code.String()).Scan(&one)
		if err == sql.ErrNoRows {
			return code.String(), nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("could not generate a unique voucher code")
}

// generateBatchCode returns a fresh unique batch code for a print run,
// e.g. "B-7KQ3XM". Every voucher of one generate call shares it.
func generateBatchCode(db *sql.DB) (string, error) {
	for attempt := 0; attempt < 50; attempt++ {
		buf := make([]byte, voucherCodeLen)
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		var code strings.Builder
		for _, b := range buf {
			code.WriteByte(voucherCodeAlphabet[int(b)%len(voucherCodeAlphabet)])
		}
		batch := "B-" + code.String()
		var one int
		err := db.QueryRow(`SELECT 1 FROM vouchers WHERE batch_code = $1 LIMIT 1`, batch).Scan(&one)
		if err == sql.ErrNoRows {
			return batch, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("could not generate a unique batch code")
}

// ============================================
// ADMIN: GENERATE
// ============================================

// Generate creates a batch of vouchers.
// POST /api/admin/vouchers/generate  {count, minutes, plan, price, notes}
func (h *VoucherHandler) Generate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Count           int     `json:"count"`
		Minutes         int     `json:"minutes"`
		Plan            string  `json:"plan"` // 'time' (default) or 'monthly'
		Price           float64 `json:"price"`
		Notes           string  `json:"notes"`
		Pausable        *bool   `json:"pausable"`        // nil = default pausable
		ExpirationHours int     `json:"expiration_hours"` // pause deadline after first use
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid request body"})
		return
	}
	if req.Count <= 0 {
		req.Count = 1
	}
	if req.Count > maxVoucherBatch {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Batch size limit is " + strconv.Itoa(maxVoucherBatch)})
		return
	}
	// Monthly subscription preset: 30 days.
	if req.Plan == "monthly" && req.Minutes <= 0 {
		req.Minutes = 43200
	}
	if req.Minutes <= 0 || req.Minutes > maxVoucherMinutes {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "minutes must be between 1 and " + strconv.Itoa(maxVoucherMinutes)})
		return
	}
	if req.Price < 0 || req.Price > 1000000 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "price must be between 0 and 1,000,000"})
		return
	}
	plan := "time"
	if req.Plan == "monthly" || req.Minutes >= 43200 {
		plan = "monthly"
	}
	pausable := true
	if req.Pausable != nil {
		pausable = *req.Pausable
	}
	if req.ExpirationHours < 0 || req.ExpirationHours > 87600 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "expiration_hours out of range"})
		return
	}
	if !pausable {
		req.ExpirationHours = 0 // consumable vouchers cannot pause
	}

	codes := make([]string, 0, req.Count)
	batchCode, err := generateBatchCode(h.DB)
	if err != nil {
		log.Printf("voucher batch code: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to generate batch code"})
		return
	}
	for i := 0; i < req.Count; i++ {
		code, err := generateVoucherCode(h.DB)
		if err != nil {
			log.Printf("voucher generate: %v", err)
			sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to generate unique code"})
			return
		}
		if _, err := h.DB.Exec(`
			INSERT INTO vouchers (code, batch_code, duration_minutes, plan, status, price, notes, pausable, expiration_hours)
			VALUES ($1, $2, $3, $4, 'unused', $5, $6, $7, $8)
		`, code, batchCode, req.Minutes, plan, req.Price, req.Notes, pausable, req.ExpirationHours); err != nil {
			log.Printf("voucher insert failed: %v", err)
			sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to save voucher"})
			return
		}
		codes = append(codes, code)
	}

	logAction(h.DB, "INFO", "voucher", "Generated "+strconv.Itoa(len(codes))+" voucher(s) batch "+batchCode+": "+
		strconv.Itoa(req.Minutes)+" min, plan="+plan)

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("Generated %d voucher(s)", len(codes)),
		"data": map[string]interface{}{
			"codes":            codes,
			"batch_code":       batchCode,
			"duration_minutes": req.Minutes,
			"plan":             plan,
			"price":            req.Price,
		},
	})
}

// ============================================
// ADMIN: LIST / EDIT / DELETE
// ============================================

// List returns vouchers with optional status/batch filter + code/notes search.
// GET /api/admin/vouchers?status=&batch=&search=&limit=&offset=
func (h *VoucherHandler) List(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	status := q.Get("status")
	batch := strings.TrimSpace(q.Get("batch"))
	search := strings.TrimSpace(q.Get("search"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}

	where := []string{"true"}
	args := []interface{}{}
	if status != "" {
		args = append(args, status)
		where = append(where, "status = $"+strconv.Itoa(len(args)))
	}
	if batch != "" {
		args = append(args, batch)
		where = append(where, "batch_code = $"+strconv.Itoa(len(args)))
	}
	if search != "" {
		args = append(args, "%"+search+"%")
		where = append(where, "(code ILIKE $"+strconv.Itoa(len(args))+
			" OR notes ILIKE $"+strconv.Itoa(len(args))+")")
	}
	whereSQL := strings.Join(where, " AND ")

	var total int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM vouchers WHERE `+whereSQL, args...).Scan(&total); err != nil {
		log.Printf("voucher list count failed: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to count vouchers"})
		return
	}

	// Global status stats for the dashboard cards (ignores filters).
	var statsUnused, statsUsed, statsDisabled, statsTotal int
	if err := h.DB.QueryRow(`
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE status = 'unused'),
		       COUNT(*) FILTER (WHERE status = 'used'),
		       COUNT(*) FILTER (WHERE status = 'disabled')
		FROM vouchers
	`).Scan(&statsTotal, &statsUnused, &statsUsed, &statsDisabled); err != nil {
		log.Printf("voucher stats failed: %v", err)
	}

	args = append(args, limit, offset)
	rows, err := h.DB.Query(`
		SELECT id, code, COALESCE(batch_code, ''), duration_minutes, plan, status, price, notes,
		       COALESCE(pausable, TRUE), expiration_hours,
		       redeemed_at, COALESCE(redeemed_mac, ''), session_id,
		       COALESCE(session_token, ''), created_at
		FROM vouchers
		WHERE `+whereSQL+`
		ORDER BY created_at DESC, id DESC
		LIMIT $`+strconv.Itoa(len(args)-1)+` OFFSET $`+strconv.Itoa(len(args)),
		args...)
	if err != nil {
		log.Printf("voucher list failed: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to list vouchers"})
		return
	}
	defer rows.Close()

	vouchers := []models.Voucher{}
	for rows.Next() {
		var v models.Voucher
		if err := rows.Scan(&v.ID, &v.Code, &v.BatchCode, &v.DurationMinutes, &v.Plan, &v.Status,
			&v.Price, &v.Notes, &v.Pausable, &v.ExpirationHours, &v.RedeemedAt, &v.RedeemedMAC, &v.SessionID,
			&v.SessionToken, &v.CreatedAt); err != nil {
			log.Printf("voucher scan failed: %v", err)
			continue
		}
		vouchers = append(vouchers, v)
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"data":    vouchers,
		"total":   total,
		"limit":   limit,
		"offset":  offset,
		"stats": map[string]int{
			"total":    statsTotal,
			"unused":   statsUnused,
			"used":     statsUsed,
			"disabled": statsDisabled,
		},
	})
}

// Dispatch handles PATCH (edit) and DELETE on /api/admin/vouchers/<id>.
func (h *VoucherHandler) Dispatch(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/api/admin/vouchers/")
	idStr = strings.Trim(idStr, "/")
	id, err := strconv.Atoi(idStr)
	if err != nil || id <= 0 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid voucher ID"})
		return
	}

	switch r.Method {
	case http.MethodPatch:
		h.Update(w, r, id)
	case http.MethodDelete:
		h.Delete(w, r, id)
	case http.MethodGet:
		h.GetOne(w, r, id)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// GetOne returns a single voucher (detail view).
func (h *VoucherHandler) GetOne(w http.ResponseWriter, r *http.Request, id int) {
	var v models.Voucher
	err := h.DB.QueryRow(`
		SELECT id, code, COALESCE(batch_code, ''), duration_minutes, plan, status, price, notes,
		       COALESCE(pausable, TRUE), expiration_hours,
		       redeemed_at, COALESCE(redeemed_mac, ''), session_id,
		       COALESCE(session_token, ''), created_at
		FROM vouchers WHERE id = $1
	`, id).Scan(&v.ID, &v.Code, &v.BatchCode, &v.DurationMinutes, &v.Plan, &v.Status,
		&v.Price, &v.Notes, &v.Pausable, &v.ExpirationHours, &v.RedeemedAt, &v.RedeemedMAC, &v.SessionID,
		&v.SessionToken, &v.CreatedAt)
	if err == sql.ErrNoRows {
		sendJSON(w, http.StatusNotFound, models.APIResponse{Success: false, Message: "Voucher not found"})
		return
	} else if err != nil {
		log.Printf("voucher get failed: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to fetch voucher"})
		return
	}
	sendJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": v})
}

// Update edits duration/status/notes. Setting status back to 'unused'
// clears the redemption binding (re-issues the voucher).
// PATCH /api/admin/vouchers/<id>
func (h *VoucherHandler) Update(w http.ResponseWriter, r *http.Request, id int) {
	var req struct {
		DurationMinutes *int     `json:"duration_minutes"`
		Status          *string  `json:"status"`
		Notes           *string  `json:"notes"`
		Plan            *string  `json:"plan"`
		Price           *float64 `json:"price"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid request body"})
		return
	}

	if req.DurationMinutes != nil && (*req.DurationMinutes <= 0 || *req.DurationMinutes > maxVoucherMinutes) {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "duration_minutes out of range"})
		return
	}
	if req.Status != nil && *req.Status != "unused" && *req.Status != "disabled" {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "status must be 'unused' or 'disabled'"})
		return
	}
	if req.Plan != nil && *req.Plan != "time" && *req.Plan != "monthly" {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "plan must be 'time' or 'monthly'"})
		return
	}
	if req.Price != nil && (*req.Price < 0 || *req.Price > 1000000) {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "price out of range"})
		return
	}

	sets := []string{}
	args := []interface{}{}
	if req.DurationMinutes != nil {
		args = append(args, *req.DurationMinutes)
		sets = append(sets, "duration_minutes = $"+strconv.Itoa(len(args)))
	}
	if req.Status != nil {
		args = append(args, *req.Status)
		sets = append(sets, "status = $"+strconv.Itoa(len(args)))
		// Re-issuing: clear the redemption binding so the code is fresh.
		if *req.Status == "unused" {
			sets = append(sets, "redeemed_at = NULL", "redeemed_mac = NULL",
				"session_id = NULL", "session_token = NULL")
		}
	}
	if req.Notes != nil {
		args = append(args, *req.Notes)
		sets = append(sets, "notes = $"+strconv.Itoa(len(args)))
	}
	if req.Plan != nil {
		args = append(args, *req.Plan)
		sets = append(sets, "plan = $"+strconv.Itoa(len(args)))
	}
	if req.Price != nil {
		args = append(args, *req.Price)
		sets = append(sets, "price = $"+strconv.Itoa(len(args)))
	}
	if len(sets) == 0 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Nothing to update"})
		return
	}

	args = append(args, id)
	res, err := h.DB.Exec(`UPDATE vouchers SET `+strings.Join(sets, ", ")+
		` WHERE id = $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		log.Printf("voucher update failed: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to update voucher"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		sendJSON(w, http.StatusNotFound, models.APIResponse{Success: false, Message: "Voucher not found"})
		return
	}

	logAction(h.DB, "INFO", "voucher", "Voucher "+strconv.Itoa(id)+" updated by admin")
	sendJSON(w, http.StatusOK, models.APIResponse{Success: true, Message: "Voucher updated"})
}

// Delete permanently removes a voucher.
// DELETE /api/admin/vouchers/<id>
func (h *VoucherHandler) Delete(w http.ResponseWriter, r *http.Request, id int) {
	res, err := h.DB.Exec(`DELETE FROM vouchers WHERE id = $1`, id)
	if err != nil {
		log.Printf("voucher delete failed: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to delete voucher"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		sendJSON(w, http.StatusNotFound, models.APIResponse{Success: false, Message: "Voucher not found"})
		return
	}
	logAction(h.DB, "INFO", "voucher", "Voucher "+strconv.Itoa(id)+" deleted by admin")
	sendJSON(w, http.StatusOK, models.APIResponse{Success: true, Message: "Voucher deleted"})
}

// ============================================
// PUBLIC: REDEEM (portal-facing)
// ============================================

// redeemFailTracker throttles repeated failed redemption attempts per IP
// so codes cannot be brute-forced from a captive client.
var (
	redeemFailMu     sync.Mutex
	redeemFailCounts = map[string]struct {
		count int
		since time.Time
	}{}
)

const redeemFailLimit = 10
const redeemFailWindow = 5 * time.Minute

func redeemFailAdd(ip string) bool {
	redeemFailMu.Lock()
	defer redeemFailMu.Unlock()
	now := time.Now()
	e := redeemFailCounts[ip]
	if now.Sub(e.since) > redeemFailWindow {
		e = struct {
			count int
			since time.Time
		}{0, now}
	}
	e.count++
	redeemFailCounts[ip] = e
	return e.count > redeemFailLimit
}

func redeemFailClear(ip string) {
	redeemFailMu.Lock()
	defer redeemFailMu.Unlock()
	delete(redeemFailCounts, ip)
}

// RedeemVoucher converts a valid unused voucher into an internet session
// for the CALLING device. Like coin Start, the caller is identified by
// its own IP/MAC — the body only carries the code and the optional
// roaming session_token. The voucher row is bound to the resulting
// session + token so the subscriber can roam between SSIDs.
// POST /api/session/redeem  {code, session_token}
func (h *SessionHandler) RedeemVoucher(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Code         string `json:"code"`
		SessionToken string `json:"session_token"`
	}
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&req)
	}
	code := strings.ToUpper(strings.TrimSpace(req.Code))
	if req.SessionToken == "" {
		req.SessionToken = r.Header.Get("X-Session-Token")
	}

	fail := func(status int, msg string) {
		sendJSON(w, status, models.APIResponse{Success: false, Message: msg})
	}

	clientIP := clientIPFromRequest(r)

	// Brute-force throttle on failed attempts per IP.
	redeemFailMu.Lock()
	overLimit := false
	if e, ok := redeemFailCounts[clientIP]; ok && time.Since(e.since) <= redeemFailWindow && e.count > redeemFailLimit {
		overLimit = true
	}
	redeemFailMu.Unlock()
	if overLimit {
		fail(http.StatusTooManyRequests, "Too many attempts. Please try again later.")
		return
	}

	// Validate shape: 6 alphanumeric chars (codes are generated from the
	// unambiguous alphabet but stay lenient on accepted characters).
	if len(code) != voucherCodeLen {
		if redeemFailAdd(clientIP) {
			log.Printf("voucher redeem: throttle tripped for %s", clientIP)
		}
		fail(http.StatusBadRequest, "Invalid voucher code")
		return
	}

	clientMAC := resolveClientMAC(clientIP)

	// Ban check mirrors the coin Start path.
	if clientMAC != "" {
		if until, _, _, banned := activeBan(h.DB, clientMAC); banned {
			sendJSON(w, http.StatusForbidden, map[string]interface{}{
				"success":      false,
				"banned":       true,
				"banned_until": until.Format(time.RFC3339),
				"reason":       "tap_abuse",
				"message":      "Temporarily banned for tap abuse. Please try again later.",
			})
			return
		}
	}

	// Atomic claim: only ONE caller can flip a voucher from unused to
	// used. This is the double-redemption guard.
	var (
		voucherID       int
		minutes         int
		plan            string
		price           float64
		pausable        bool
		expirationHours int
	)
	err := h.DB.QueryRow(`
		UPDATE vouchers
		SET status = 'used', redeemed_at = NOW(), redeemed_mac = $2
		WHERE code = $1 AND status = 'unused'
		RETURNING id, duration_minutes, plan, price, COALESCE(pausable, TRUE), expiration_hours
	`, code, clientMAC).Scan(&voucherID, &minutes, &plan, &price, &pausable, &expirationHours)
	if err == sql.ErrNoRows {
		if redeemFailAdd(clientIP) {
			log.Printf("voucher redeem: throttle tripped for %s", clientIP)
		}
		fail(http.StatusNotFound, "Invalid or already used voucher code")
		return
	} else if err != nil {
		log.Printf("voucher redeem claim failed: %v", err)
		fail(http.StatusInternalServerError, "Failed to redeem voucher")
		return
	}

	// Credit the session (create or extend the caller's existing one)
	// using the exact same path as coin payments. The caller's roaming
	// token is passed through so the voucher binds to it. The voucher's
	// pause rules are applied at FIRST redemption — an unused voucher
	// never ages; the pause-expiry clock starts when the code is used.
	session, extended, err := h.creditSession(clientIP, clientMAC, req.SessionToken, 0, minutes, nil, pausable, expirationHours)
	if err != nil {
		// Roll the voucher back to unused so a transient failure does
		// not eat a paid code.
		if _, rerr := h.DB.Exec(`
			UPDATE vouchers SET status = 'unused', redeemed_at = NULL, redeemed_mac = NULL
			WHERE id = $1 AND status = 'used'
		`, voucherID); rerr != nil {
			log.Printf("voucher redeem rollback failed for %d: %v", voucherID, rerr)
		}
		if banErr, ok := err.(*banActiveError); ok {
			sendJSON(w, http.StatusForbidden, map[string]interface{}{
				"success":      false,
				"banned":       true,
				"banned_until": banErr.Until.Format(time.RFC3339),
				"reason":       "tap_abuse",
				"message":      "Temporarily banned for tap abuse. Please try again later.",
			})
			return
		}
		log.Printf("voucher redeem credit failed for %s: %v", clientIP, err)
		fail(http.StatusInternalServerError, "Failed to start session")
		return
	}

	// Bind the voucher to the session + roaming token.
	tokenOut, _ := session["session_token"].(string)
	sessionID, _ := session["id"].(int)
	if _, err := h.DB.Exec(`
		UPDATE vouchers SET session_id = $2, session_token = $3 WHERE id = $1
	`, voucherID, sessionID, tokenOut); err != nil {
		log.Printf("voucher bind failed for %d: %v", voucherID, err)
	}

	// Open the client's internet access.
	runCaptiveRules(h.DB, "auth", clientMAC, "voucher-redeem")
	EnsurePerDeviceClass(h.DB, "", clientMAC, clientIP, "add")

	redeemFailClear(clientIP)

	verb := "redeemed"
	if extended {
		verb = "redeemed (extended)"
	}
	logAction(h.DB, "INFO", "voucher", "Voucher "+code+" "+verb+" for "+clientIP+" ("+clientMAC+"): "+
		strconv.Itoa(minutes)+" min, plan="+plan)

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success":       true,
		"message":       "Voucher accepted",
		"extended":      extended,
		"session":       session,
		"session_token": tokenOut,
		"voucher": map[string]interface{}{
			"code":             code,
			"plan":             plan,
			"duration_minutes": minutes,
			"price":            price,
			"pausable":         pausable,
			"expiration_hours": expirationHours,
		},
	})
}
