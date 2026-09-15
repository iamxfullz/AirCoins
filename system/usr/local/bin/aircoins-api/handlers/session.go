package handlers

import (
	"aircoins-api/models"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
)

type SessionHandler struct {
	DB             *sql.DB
	LicenseHandler *LicenseHandler
}

// banActiveError is returned by creditSession when the caller's MAC is
// currently banned. The Start handler catches it and returns a 403.
type banActiveError struct {
	Until time.Time
}

func (e *banActiveError) Error() string {
	return "temporarily banned for tap abuse"
}

// Sessions are wall-clock based: a session is alive while
// expires_at > NOW() (DB clock). remaining_seconds is kept as a snapshot
// for display/legacy rows only — every response computes the remaining
// time from expires_at inside SQL so no timezone assumptions leak out.

// remainingSQL computes the live remaining seconds of a session row.
const remainingSQL = `GREATEST(0, EXTRACT(EPOCH FROM (expires_at - NOW())))::int`

// sessionRow holds the common columns returned by session lookups.
type sessionRow struct {
	id            int
	mac           string
	ip            string
	coins         int
	total         int
	remaining     int
	startedAt     time.Time
	expiresAt     time.Time
	pausedAt      sql.NullTime
	shapedMbps    *int
	token         string
	pausable      bool
	expirationHrs int
	pauseDeadline sql.NullTime
}

// sessionMutexes serializes concurrent MAC migrations for the same session.
var sessionMutexes sync.Map

// lookupSessionByTokenOrMAC finds an active session by token, MAC, or IP
// (in that priority order). Returns the session, the lookup source, and
// any error. Each lookup is a single index-seek query (no OR).
func lookupSessionByTokenOrMAC(db *sql.DB, token, mac, ip string) (*sessionRow, string, error) {
	lookupSQL := `SELECT id, COALESCE(client_mac,''), COALESCE(client_ip,''),
		coins_inserted, total_seconds, ` + remainingSQL + `,
		started_at, expires_at, paused_at, shaped_mbps, COALESCE(session_token,''),
		COALESCE(pausable, true), COALESCE(expiration_hours, 0), pause_expires_at
		FROM sessions
		WHERE status = 'active' AND (paused_at IS NOT NULL OR expires_at > NOW())`

	// 1. Token lookup (uses idx_sessions_session_token unique index)
	if token != "" {
		var s sessionRow
		err := db.QueryRow(lookupSQL+` AND session_token = $1 LIMIT 1`, token).
			Scan(&s.id, &s.mac, &s.ip, &s.coins, &s.total, &s.remaining,
				&s.startedAt, &s.expiresAt, &s.pausedAt, &s.shapedMbps, &s.token,
				&s.pausable, &s.expirationHrs, &s.pauseDeadline)
		if err == nil {
			return &s, "token", nil
		}
		if err != sql.ErrNoRows {
			return nil, "", err
		}
	}

	// 2. MAC lookup (uses sessions_client_mac_uniq partial unique index)
	if mac != "" {
		var s sessionRow
		err := db.QueryRow(lookupSQL+` AND client_mac = $1 ORDER BY started_at DESC LIMIT 1`, mac).
			Scan(&s.id, &s.mac, &s.ip, &s.coins, &s.total, &s.remaining,
				&s.startedAt, &s.expiresAt, &s.pausedAt, &s.shapedMbps, &s.token,
				&s.pausable, &s.expirationHrs, &s.pauseDeadline)
		if err == nil {
			return &s, "mac", nil
		}
		if err != sql.ErrNoRows {
			return nil, "", err
		}
	}

	// 3. IP lookup (uses idx_sessions_client_ip)
	if ip != "" {
		var s sessionRow
		err := db.QueryRow(lookupSQL+` AND client_ip = $1 ORDER BY started_at DESC LIMIT 1`, ip).
			Scan(&s.id, &s.mac, &s.ip, &s.coins, &s.total, &s.remaining,
				&s.startedAt, &s.expiresAt, &s.pausedAt, &s.shapedMbps, &s.token,
				&s.pausable, &s.expirationHrs, &s.pauseDeadline)
		if err == nil {
			return &s, "ip", nil
		}
		if err != sql.ErrNoRows {
			return nil, "", err
		}
	}

	return nil, "none", nil
}

// migrateSessionMAC migrates a session from one MAC/IP to another when a
// device roams between SSIDs (MAC randomization). Per-session mutex
// prevents concurrent migrations races.
func migrateSessionMAC(db *sql.DB, sessionID int, oldIP, oldMAC, newIP, newMAC, token string) error {
	// Get-or-create per-session mutex
	muI, _ := sessionMutexes.LoadOrStore(sessionID, &sync.Mutex{})
	mu := muI.(*sync.Mutex)
	mu.Lock()
	defer func() {
		mu.Unlock()
		sessionMutexes.Delete(sessionID)
	}()

	// Re-read session (may have been migrated by a concurrent poll)
	var currentMAC string
	err := db.QueryRow(`SELECT COALESCE(client_mac,'') FROM sessions WHERE id = $1 AND status = 'active'`, sessionID).Scan(&currentMAC)
	if err != nil {
		return fmt.Errorf("re-read session %d: %w", sessionID, err)
	}
	if currentMAC == newMAC {
		return nil // already migrated
	}

	// Transaction: expire conflicting row on newMAC, then UPDATE
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// If newMAC already owns a different active session, expire it first
	_, err = tx.Exec(`
		UPDATE sessions
		SET status = 'expired', expires_at = NOW(), remaining_seconds = 0
		WHERE client_mac = $1 AND id <> $2 AND status = 'active'
	`, newMAC, sessionID)
	if err != nil {
		return err
	}

	// Migrate the session to the new MAC/IP
	_, err = tx.Exec(`UPDATE sessions SET client_ip = $1, client_mac = $2 WHERE id = $3`, newIP, newMAC, sessionID)
	if err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Post-commit (non-fatal): iptables + tc adjustments
	runCaptiveRules(db, "unauth", oldMAC, "token-migrate")
	EnsurePerDeviceClass(db, "", oldMAC, oldIP, "remove")
	runCaptiveRules(db, "auth", newMAC, "token-migrate")
	EnsurePerDeviceClass(db, "", newMAC, newIP, "add")

	// Re-apply shaped_mbps override on the new class if the session had one
	var shapedMbps sql.NullInt64
	_ = db.QueryRow(`SELECT shaped_mbps FROM sessions WHERE id = $1`, sessionID).Scan(&shapedMbps)
	if shapedMbps.Valid && shapedMbps.Int64 > 0 {
		newIface := ifaceForClientIP(db, newIP)
		if newIface != "" {
			rules := loadQdiscRules(db)
			if rule, ok := rules[newIface]; ok && rule.Qdisc == "fq_codel" && rule.PerDeviceBwMbps > 0 {
				if cerr := ChangeClientClassRate(newIface, newMAC, int(shapedMbps.Int64)); cerr != nil {
					log.Printf("migrateSessionMAC: ChangeClientClassRate failed for session %d: %v", sessionID, cerr)
				}
			}
		}
	}

	logAction(db, "INFO", "session", fmt.Sprintf("session %d migrated from %s/%s to %s/%s via token %s",
		sessionID, oldMAC, oldIP, newMAC, newIP, token))

	return nil
}

// ============================================
// COIN WINDOW (unprocessed coin_events)
// ============================================

// windowCoins is the credit accumulated in the current armed window.
type windowCoins struct {
	ids          []int64
	count        int
	totalValue   int
	totalMinutes int
}

// unprocessedWindowCoins sums the UNPROCESSED coin_events of the current
// armed window — the same window logic as GET /api/coinslot/status
// (age-based, so DB/API timezone disagreements cannot break it). The
// source argument scopes the window to one coinslot: the caller's
// sub-vendo ('subvendo:<id>') or the local GPIO ('local_gpio'), which is
// what keeps one VLAN's coins from crediting another VLAN's client. If
// the armed markers are already gone (portal closed the modal first, API
// restarted mid-window) it falls back to the widest possible window so a
// paying customer is never robbed of freshly inserted coins: coins are
// only recorded while armed, so recent unprocessed rows can only belong
// to the customer at the coin slot.
func (h *SessionHandler) unprocessedWindowCoins(source string) windowCoins {
	coins := windowCoins{}

	armed, expiresAt := readArmedState()
	armedAt := readArmedAt(expiresAt)

	windowAge := int64(maxArmDuration)
	if armed && armedAt > 0 {
		windowAge = time.Now().Unix() - armedAt + 1
		if windowAge < 1 {
			windowAge = 1
		}
	}

	rows, err := h.DB.Query(`
		SELECT id, coin_value
		FROM coin_events
		WHERE processed = false
		  AND detected_at >= NOW() - ($1::int * INTERVAL '1 second')
		  AND source = $3
		ORDER BY id ASC
		LIMIT $2
	`, windowAge, maxWindowCoins, source)
	if err != nil {
		log.Printf("Error fetching unprocessed coin events: %v", err)
		return coins
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var value int
		if err := rows.Scan(&id, &value); err != nil {
			log.Printf("Error scanning coin event: %v", err)
			continue
		}

		coins.ids = append(coins.ids, id)
		coins.count++
		coins.totalValue += value
	}
	if err := rows.Err(); err != nil {
		log.Printf("Error reading coin events: %v", err)
	}

	// Tiered credit: resolve ONE pricing tier for the accumulated total
	// (highest active tier at or below the total). Operators configure
	// per-denomination rates — e.g. P1=12min, P5=120min, P10=300min — and
	// the tier takes effect as soon as the window total reaches it, instead
	// of the old per-pulse summation (5 x the P1 tier) which made those
	// multi-peso tiers unreachable on pulse-train coin acceptors.
	//
	// Any part of the total beyond the matched tier is credited by SCALING
	// the matched tier proportionally (integer division). E.g. only a
	// P10=300min tier exists and the total is P20 -> 20/10 * 300 = 600 min.
	// Exact tier matches fall out of the same formula (P10 -> 10/10*300 = 300).
	if coins.totalValue > 0 {
		m, perr := MinutesForAmount(h.DB, coins.totalValue)
		if perr != nil {
			log.Printf("Error resolving pricing for P%d: %v", coins.totalValue, perr)
			m = PricingMatch{}
		}
		if m.MatchedCoin > 0 && m.Minutes > 0 {
			coins.totalMinutes = int(int64(coins.totalValue) * int64(m.Minutes) / int64(m.MatchedCoin))
		} else {
			coins.totalMinutes = 0
		}
	}
	return coins
}

// ============================================
// SESSION START (public, portal "Done Paying")
// ============================================

// Start converts the coins of the current armed window into an internet
// session for the CALLING device. The credited time comes exclusively
// from unprocessed coin_events + the pricing table — the request body is
// ignored, so a client cannot grant itself time.
func (h *SessionHandler) Start(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse pay_ticket and session_token from request body.
	var req struct {
		PayTicket string `json:"pay_ticket"`
		Coinslot  string `json:"coinslot"`

		SessionToken string `json:"session_token"`
	}
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&req)
	}

	// Session token: body wins over header
	sessionToken := req.SessionToken
	if sessionToken == "" {
		sessionToken = r.Header.Get("X-Session-Token")
	}

	clientIP := clientIPFromRequest(r)
	clientMAC := resolveClientMAC(clientIP)
	if clientMAC == "" {
		log.Printf("session start: no MAC resolved for %s (continuing IP-only)", clientIP)
	}

	// F6: Pre-lock ban check — mirrors Arm's pattern so a banned client
	// cannot proceed (the in-tx check stays as TOCTOU net).
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

	// --- Ticket validation + lock release (arm→start handoff) --------
	// The per-VLAN lock was acquired by Arm. Start validates the ticket
	// and releases the lock. If the ticket is missing/mismatched the
	// client didn't go through Arm (or the watchdog released the lock).
	lockReason, released := PayLockValidateAndRelease(clientIP, req.PayTicket)
	if !released {
		if lockReason == "invalid_ticket" {
			sendJSON(w, http.StatusForbidden, map[string]interface{}{
				"success": false,
				"message": "Invalid or missing pay_ticket",
			})
			return
		}
		// lock_gone: the watchdog already released, or another device
		// has since acquired the lock.
		sendJSON(w, http.StatusLocked, map[string]interface{}{
			"success": false,
			"code":    "paying",
			"message": "SOMEBODY IS PAYING, PLEASE WAIT FOR YOUR TURN",
		})
		return
	}
	// Lock is now released — the critical section (creditSession) runs
	// without the per-VLAN lock since the ticket already proved this
	// client is the legitimate holder. The unprocessed coin_events
	// are consumed by the transaction below.

	// The only coinslot on an SBC is the local GPIO listener (Sub-Vendo /
	// NodeMCU support was removed), so the credited window is always
	// 'local_gpio' regardless of the (backward-compat) coinslot parameter.
	source := "local_gpio"

	coins := h.unprocessedWindowCoins(source)
	if coins.totalMinutes == 0 {
		msg := "no credited coins"
		if coins.count > 0 {
			msg = "no credited coins: no pricing tier configured for the inserted amount"
		}
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: msg})
		return
	}

	// Resolve the pause rules for the purchase: snapshot the matched rate's
	// pausable flag and expiration window so the session knows whether the
	// portal may show a Pause button and when a paused session must be
	// force-expired if the user stays away too long.
	pricingMatch, perr := MinutesForAmount(h.DB, coins.totalValue)
	if perr != nil {
		log.Printf("Error resolving rate rules for P%d: %v", coins.totalValue, perr)
		pricingMatch = PricingMatch{Pausable: true}
	}

	session, extended, err := h.creditSession(clientIP, clientMAC, sessionToken, coins.totalValue, coins.totalMinutes, coins.ids, pricingMatch.Pausable, pricingMatch.ExpirationHours)
	if err != nil {
		// Check if the error is a ban rejection (F6: ban check inside tx)
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
		log.Printf("Error crediting session for %s: %v", clientIP, err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to create session"})
		return
	}

	reason := "start"
	verb := "started"
	if extended {
		reason = "extend"
		verb = "extended"
	}

	// The purchase is complete: close the armed window so the GPIO
	// listener goes back to idle.
	os.Remove(armedFile)
	os.Remove(armedAtFile)

	logAction(h.DB, "INFO", "session", "Session "+verb+" for "+clientIP+" ("+clientMAC+"): P"+
		strconv.Itoa(coins.totalValue)+" = "+strconv.Itoa(coins.totalMinutes)+" min")

	// Send the completion response FIRST. runCaptiveRules spawns
	// `aircoins-captive-rules auth <mac>` as a blocking subprocess that can
	// take a moment (it also starts the DNS forwarder), and waiting on it
	// makes the portal hang at "Starting session...". Opening the client's
	// internet access is safe in the background — it is non-fatal and lands
	// a fraction of a second later.
	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success":  true,
		"message":  "Session " + verb,
		"extended": extended,
		"session":  session,
	})

	go func() {
		// Open the client's internet access + per-device tc class/filter.
		runCaptiveRules(h.DB, "auth", clientMAC, reason)
		EnsurePerDeviceClass(h.DB, "", clientMAC, clientIP, "add")
	}()
}

// creditSession creates a new active session or extends the caller's
// existing one, and marks the credited coin_events processed — all in one
// transaction so a crash can neither double-credit nor eat coins.
func (h *SessionHandler) creditSession(clientIP, clientMAC, sessionToken string, coinValue, minutes int, eventIDs []int64, pausable bool, expirationHours int) (map[string]interface{}, bool, error) {
	addSeconds := minutes * 60

	tx, err := h.DB.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	// F6: Ban check INSIDE the creditSession transaction to close the
	// TOCTOU window — a ban created between the pre-check and the UPSERT
	// would otherwise be missed. The coinslot arm endpoint also rejects
	// banned MACs as a first line of defense.
	if clientMAC != "" {
		var until time.Time
		banErr := tx.QueryRow(`
			SELECT banned_until FROM client_bans
			WHERE client_mac = $1 AND banned_until > NOW()
		`, clientMAC).Scan(&until)
		if banErr == nil {
			// Ban is active — abort the transaction
			return nil, false, &banActiveError{Until: until}
		}
		if banErr != sql.ErrNoRows {
			return nil, false, banErr
		}
	}

	// Existing ACTIVE session for this device (MAC first, IP fallback)?
	var existingID int
	err = tx.QueryRow(`
		SELECT id FROM sessions
		WHERE status = 'active' AND expires_at > NOW()
		  AND (($1 <> '' AND client_mac = $1) OR client_ip = $2)
		ORDER BY started_at DESC
		LIMIT 1
	`, clientMAC, clientIP).Scan(&existingID)
	if err != nil && err != sql.ErrNoRows {
		return nil, false, err
	}
	extended := err == nil

	var (
		id, coinsTotal, totalSeconds, remaining int
		expiresAt                               time.Time
		tokenOut                                string
	)

	if extended {
		// EXTEND: push expires_at out by the new minutes (never create a
		// duplicate row for a device that is already online).
		err = tx.QueryRow(`
			UPDATE sessions
			SET coins_inserted = coins_inserted + $1::int,
			    total_seconds = total_seconds + $2::int,
			    expires_at = GREATEST(expires_at, NOW()) + ($2::int * INTERVAL '1 second'),
			    remaining_seconds = `+remainingSQL+` + $2::int,
			    client_ip = $3,
			    client_mac = CASE WHEN $4 <> '' AND NOT EXISTS (
			                     SELECT 1 FROM sessions x WHERE x.client_mac = $4 AND x.id <> sessions.id
			                 ) THEN $4 ELSE client_mac END,
			    session_token = CASE
			        WHEN COALESCE(session_token, '') <> '' THEN session_token
			        WHEN $6 <> '' THEN $6
			        ELSE session_token
			    END
			WHERE id = $5
			RETURNING id, coins_inserted, total_seconds, `+remainingSQL+`, expires_at, COALESCE(session_token, '')
		`, coinValue, addSeconds, clientIP, clientMAC, existingID, sessionToken).
			Scan(&id, &coinsTotal, &totalSeconds, &remaining, &expiresAt, &tokenOut)
	} else if clientMAC != "" {
		// NEW/REUSE: one session row per device. If the MAC already owns a
		// row in a terminal state (expired/cancelled/...), RESET that row
		// to a fresh active session — counters start from this payment
		// only — instead of inserting a duplicate. Requires the partial
		// unique index sessions_client_mac_uniq (migration 008); the
		// conflict target repeats the index predicate as Postgres demands
		// for partial-index arbiters. paused_at / remaining_seconds_at_pause
		// / pause_count are cleared so a fresh start cannot inherit stale
		// pause state.
		if sessionToken == "" {
			sessionToken = generateSessionToken()
		}
		err = tx.QueryRow(`
			INSERT INTO sessions (client_ip, client_mac, coins_inserted, total_seconds,
			                      remaining_seconds, status, started_at, activated_at, expires_at,
			                      paused_at, remaining_seconds_at_pause, pause_count, session_token,
			                      pausable, expiration_hours)
			VALUES ($1, $2, $3::int, $4::int, $4::int, 'active', NOW(), NOW(), NOW() + ($4::int * INTERVAL '1 second'), NULL, NULL, 0, $5, $6, $7)
			ON CONFLICT (client_mac) WHERE client_mac IS NOT NULL AND client_mac <> '' AND client_mac <> '-'
			DO UPDATE SET
			    client_ip = EXCLUDED.client_ip,
			    coins_inserted = EXCLUDED.coins_inserted,
			    total_seconds = EXCLUDED.total_seconds,
			    remaining_seconds = EXCLUDED.remaining_seconds,
			    status = 'active',
			    started_at = NOW(),
			    activated_at = NOW(),
			    expired_at = NULL,
			    expires_at = EXCLUDED.expires_at,
			    paused_at = NULL,
			    remaining_seconds_at_pause = NULL,
			    pause_count = 0,
			    pausable = EXCLUDED.pausable,
			    expiration_hours = EXCLUDED.expiration_hours,
			    pause_expires_at = NULL,
			    session_token = COALESCE(NULLIF(sessions.session_token, ''), EXCLUDED.session_token)
			RETURNING id, coins_inserted, total_seconds, `+remainingSQL+`, expires_at, COALESCE(session_token, '')
		`, clientIP, clientMAC, coinValue, addSeconds, sessionToken, pausable, expirationHours).
			Scan(&id, &coinsTotal, &totalSeconds, &remaining, &expiresAt, &tokenOut)
	} else {
		// No resolvable MAC: excluded from the unique index, plain insert.
		err = tx.QueryRow(`
			INSERT INTO sessions (client_ip, client_mac, coins_inserted, total_seconds,
			                      remaining_seconds, status, started_at, activated_at, expires_at,
			                      pausable, expiration_hours)
			VALUES ($1, $2, $3::int, $4::int, $4::int, 'active', NOW(), NOW(), NOW() + ($4::int * INTERVAL '1 second'), $5, $6)
			RETURNING id, coins_inserted, total_seconds, `+remainingSQL+`, expires_at, ''
		`, clientIP, clientMAC, coinValue, addSeconds, pausable, expirationHours).
			Scan(&id, &coinsTotal, &totalSeconds, &remaining, &expiresAt, &tokenOut)
	}
	if err != nil {
		return nil, false, err
	}

	// Consume the coins so a second "Done Paying" cannot credit them again.
	if len(eventIDs) > 0 {
		if _, err = tx.Exec(`
			UPDATE coin_events SET processed = true, session_id = $1
			WHERE id = ANY($2)
		`, id, pq.Array(eventIDs)); err != nil {
			return nil, false, err
		}
	}

	if err = tx.Commit(); err != nil {
		return nil, false, err
	}

	return map[string]interface{}{
		"id":            id,
		"status":        "active",
		"ip":            clientIP,
		"mac":           clientMAC,
		"coins":         coinsTotal,
		"total":         totalSeconds,
		"remaining":     remaining,
		"expires_at":    expiresAt.Format(time.RFC3339),
		"session_token": tokenOut,
	}, extended, nil
}

// ============================================
// SESSION STATUS (public, portal poll)
// ============================================

// Status returns the calling device's session for the portal poll.
// Supports token-based lookup for MAC-randomization roaming: if the
// X-Session-Token header matches a session whose MAC differs from the
// caller's current ARP-resolved MAC, the session is migrated.
func (h *SessionHandler) Status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Lazy expiry: never report a session the ticker has not caught yet.
	ExpireOverdueSessions(h.DB, "expire")

	clientIP := r.URL.Query().Get("ip")
	if clientIP == "" {
		clientIP = clientIPFromRequest(r)
	}
	// Cheap neighbour-table lookup only — this poll runs every few
	// seconds, so no ping probe here.
	clientMAC := neighborMAC(clientIP)

	// Read session token from header (portal attaches from localStorage)
	sessionToken := r.Header.Get("X-Session-Token")

	sess, source, err := lookupSessionByTokenOrMAC(h.DB, sessionToken, clientMAC, clientIP)
	if err != nil {
		log.Printf("Error fetching session status: %v", err)
	}

	response := map[string]interface{}{
		"timestamp":     makeTimestamp(),
		"has_session":   sess != nil,
		"session_token": "",
	}

	if sess != nil {
		response["session_token"] = sess.token

		// Trigger MAC migration when token matched but device is on a
		// different MAC (roamed to another SSID/VLAN).
		if source == "token" && sess.mac != "" && clientMAC != "" && sess.mac != clientMAC {
			if merr := migrateSessionMAC(h.DB, sess.id, sess.ip, sess.mac, clientIP, clientMAC, sessionToken); merr != nil {
				log.Printf("Status: migration failed for session %d: %v", sess.id, merr)
			} else {
				// Update in-memory row to reflect the new MAC/IP
				sess.mac = clientMAC
				sess.ip = clientIP
			}
		}

		remaining := sess.remaining
		paused := sess.pausedAt.Valid
		if paused {
			var snap sql.NullInt64
			_ = h.DB.QueryRow(`SELECT remaining_seconds_at_pause FROM sessions WHERE id=$1`, sess.id).Scan(&snap)
			if snap.Valid {
				remaining = int(snap.Int64)
			}
		}
		pauseExpiresAt := ""
		if sess.pauseDeadline.Valid {
			pauseExpiresAt = sess.pauseDeadline.Time.Format(time.RFC3339)
		}
		response["session"] = map[string]interface{}{
			"id":               sess.id,
			"status":           "active",
			"coins":            sess.coins,
			"total":            sess.total,
			"remaining":        remaining,
			"started":          sess.startedAt.Unix(),
			"mac":              sess.mac,
			"expires_at":       sess.expiresAt.Format(time.RFC3339),
			"paused":           paused,
			"pausable":         sess.pausable,
			"expiration_hours": sess.expirationHrs,
			"pause_expires_at": pauseExpiresAt,
		}
	}

	// Ban state for the caller's MAC (portal uses this to drive the ban
	// overlay; belt-and-braces on top of the dedicated /api/session/ban-status
	// poll).
	if clientMAC != "" {
		if until, reason, attempts, banned := activeBan(h.DB, clientMAC); banned {
			response["ban"] = map[string]interface{}{
				"banned":             true,
				"banned_until":       until.Format(time.RFC3339),
				"reason":             reason,
				"attempts_in_window": attempts,
			}
		}
	}

	licenseRequired := false
	if h.LicenseHandler != nil {
		licenseRequired = !h.LicenseHandler.IsLicenseValid()
	}
	response["license_required"] = licenseRequired

	response["system"] = map[string]interface{}{
		"online":     checkLighttpdRunning(),
		"ip":         clientIP,
		"client_mac": clientMAC,
	}

	sendJSON(w, http.StatusOK, response)
}

// CanStart is a lightweight pre-flight check (F10): the portal calls
// this BEFORE /api/session/start to surface "SOMEBODY IS PAYING" without
// consuming the user's arm. It does a non-blocking TryAcquire + immediate
// Release and returns {can_start: true} or {can_start: false, code: "paying"}.
// PUBLIC — no auth required.
func (h *SessionHandler) CanStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	clientIP := clientIPFromRequest(r)
	acquired, holder, vlanKey, _ := PayLockTryAcquire(clientIP)
	if acquired {
		PayLockRelease(clientIP, vlanKey)
		sendJSON(w, http.StatusOK, map[string]interface{}{"can_start": true})
		return
	}
	// Lock is held — but if the holder is THIS client (same IP), they are
	// the legitimate arm-holder doing a pre-flight check before Start.
	// Let them through; Start will validate the ticket and release the lock.
	if holder == clientIP {
		sendJSON(w, http.StatusOK, map[string]interface{}{"can_start": true})
		return
	}
	sendJSON(w, http.StatusOK, map[string]interface{}{
		"can_start": false,
		"code":      "paying",
		"message":   "SOMEBODY IS PAYING, PLEASE WAIT FOR YOUR TURN",
	})
}

// GetCurrent returns the active session for a client IP (legacy shape)
func (h *SessionHandler) GetCurrent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	clientIP := r.URL.Query().Get("ip")
	if clientIP == "" {
		clientIP = clientIPFromRequest(r)
	}
	clientMAC := neighborMAC(clientIP)

	var session models.Session
	err := h.DB.QueryRow(`
		SELECT id, client_ip, client_mac, coins_inserted, total_seconds,
		       `+remainingSQL+`, status, started_at, activated_at, expired_at, expires_at,
		       COALESCE(pausable, true), COALESCE(expiration_hours, 0), pause_expires_at
		FROM sessions
		WHERE status = 'active' AND expires_at > NOW()
		  AND (($1 <> '' AND client_mac = $1) OR client_ip = $2)
		ORDER BY started_at DESC
		LIMIT 1
	`, clientMAC, clientIP).Scan(
		&session.ID, &session.ClientIP, &session.ClientMAC, &session.CoinsInserted,
		&session.TotalSeconds, &session.RemainingSeconds, &session.Status,
		&session.StartedAt, &session.ActivatedAt, &session.ExpiredAt, &session.ExpiresAt,
		&session.Pausable, &session.ExpirationHours, &session.PauseExpiresAt,
	)

	if err == sql.ErrNoRows {
		sendJSON(w, http.StatusOK, models.SessionStatusResponse{HasActive: false})
		return
	} else if err != nil {
		log.Printf("Error fetching session: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to fetch session"})
		return
	}

	sendJSON(w, http.StatusOK, models.SessionStatusResponse{
		HasActive:        true,
		Session:          &session,
		RemainingSeconds: session.RemainingSeconds,
		CoinsInserted:    session.CoinsInserted,
	})
}

// ============================================
// ADMIN SESSION MANAGEMENT (AuthMiddleware'd in main.go)
// ============================================

// AdminCreate lets the operator grant a session manually (Sessions tab).
func (h *SessionHandler) AdminCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ClientIP string `json:"client_ip"`
		Minutes  int    `json:"minutes"`
		Coins    int    `json:"coins"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid request body"})
		return
	}
	if req.ClientIP == "" || req.Minutes <= 0 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "client_ip and minutes are required"})
		return
	}

	clientMAC := resolveClientMAC(req.ClientIP)
	// Admin-created sessions default to a pausable rate with no pause-expiry
	// window (legacy behaviour) unless the operator sets otherwise.
	session, extended, err := h.creditSession(req.ClientIP, clientMAC, "", req.Coins, req.Minutes, nil, true, 0)
	if err != nil {
		log.Printf("Error creating admin session for %s: %v", req.ClientIP, err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to create session"})
		return
	}

	runCaptiveRules(h.DB, "auth", clientMAC, "admin-create")
	// Add per-device tc class+filter for FQ_CODEL per-device mode.
	EnsurePerDeviceClass(h.DB, "", clientMAC, req.ClientIP, "add")
	logAction(h.DB, "INFO", "session", "Admin session for "+req.ClientIP+" ("+clientMAC+"): "+strconv.Itoa(req.Minutes)+" min")

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success":  true,
		"message":  "Session created",
		"extended": extended,
		"session":  session,
	})
}

// Extend adds time to an existing session (admin only)
func (h *SessionHandler) Extend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		SessionID int `json:"session_id"`
		Minutes   int `json:"minutes"`
		Seconds   int `json:"seconds"` // legacy field, used when minutes is absent
		Coins     int `json:"coins"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid request body"})
		return
	}

	addSeconds := req.Minutes * 60
	if addSeconds <= 0 {
		addSeconds = req.Seconds
	}
	if addSeconds <= 0 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "minutes must be positive"})
		return
	}

	var mac string
	err := h.DB.QueryRow(`
		UPDATE sessions
		SET coins_inserted = coins_inserted + $1::int,
		    total_seconds = total_seconds + $2::int,
		    expires_at = GREATEST(expires_at, NOW()) + ($2::int * INTERVAL '1 second'),
		    remaining_seconds = `+remainingSQL+` + $2::int
		WHERE id = $3 AND status = 'active'
		RETURNING COALESCE(client_mac, '')
	`, req.Coins, addSeconds, req.SessionID).Scan(&mac)

	if err == sql.ErrNoRows {
		sendJSON(w, http.StatusNotFound, models.APIResponse{Success: false, Message: "Session not found or not active"})
		return
	} else if err != nil {
		log.Printf("Error extending session: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to extend session"})
		return
	}

	// Re-auth in case the client's rules were lost (idempotent).
	runCaptiveRules(h.DB, "auth", mac, "admin-extend")

	logAction(h.DB, "INFO", "session", "Session "+strconv.Itoa(req.SessionID)+" extended by admin (+"+strconv.Itoa(addSeconds/60)+" min)")
	sendJSON(w, http.StatusOK, models.APIResponse{Success: true, Message: "Session extended"})
}

// End terminates a session (admin only) and revokes internet access
func (h *SessionHandler) End(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		SessionID int    `json:"session_id"`
		Reason    string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid request body"})
		return
	}

	status := "expired"
	if req.Reason == "cancelled" {
		status = "cancelled"
	}

	var mac, clientIP string
	err := h.DB.QueryRow(`
		UPDATE sessions
		SET status = $1, expired_at = NOW(), expires_at = NOW(), remaining_seconds = 0
		WHERE id = $2
		RETURNING COALESCE(client_mac, ''), COALESCE(client_ip, '')
	`, status, req.SessionID).Scan(&mac, &clientIP)

	if err == sql.ErrNoRows {
		sendJSON(w, http.StatusNotFound, models.APIResponse{Success: false, Message: "Session not found"})
		return
	} else if err != nil {
		log.Printf("Error ending session: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to end session"})
		return
	}

	runCaptiveRules(h.DB, "unauth", mac, "admin-terminate")
	// Remove per-device tc class+filter for FQ_CODEL per-device mode.
	EnsurePerDeviceClass(h.DB, "", mac, clientIP, "remove")

	logAction(h.DB, "INFO", "session", "Session "+strconv.Itoa(req.SessionID)+" ended by admin: "+status)
	sendJSON(w, http.StatusOK, models.APIResponse{Success: true, Message: "Session ended"})
}

// ============================================
// PER-SESSION SPEED OVERRIDE (admin only)
// ============================================

// Shape sets or clears the per-session speed override (shaped_mbps) and
// live-applies the tc class rate when FQ_CODEL per-device shaping is active.
// POST /api/admin/sessions/<id>/shape  body: {"mbps": N}  (N>0 set, N=0 clear)
func (h *SessionHandler) Shape(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract session ID from path: /api/admin/sessions/<id>/shape
	path := strings.TrimPrefix(r.URL.Path, "/api/admin/sessions/")
	path = strings.TrimSuffix(path, "/shape")
	sessionID, err := strconv.Atoi(path)
	if err != nil || sessionID <= 0 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid session ID"})
		return
	}

	var req struct {
		Mbps int `json:"mbps"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid request body"})
		return
	}
	if req.Mbps < 0 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "mbps must be >= 0"})
		return
	}

	// Persist the override in the DB. NULL when cleared (mbps == 0).
	var shapedMbps *int
	if req.Mbps > 0 {
		shapedMbps = &req.Mbps
	}
	_, err = h.DB.Exec("UPDATE sessions SET shaped_mbps = $1 WHERE id = $2", shapedMbps, sessionID)
	if err != nil {
		log.Printf("Shape: DB update failed for session %d: %v", sessionID, err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to update session"})
		return
	}

	// Fetch session details for live-apply.
	var mac, clientIP, status string
	err = h.DB.QueryRow(`
		SELECT COALESCE(client_mac, ''), COALESCE(client_ip, ''), status
		FROM sessions WHERE id = $1
	`, sessionID).Scan(&mac, &clientIP, &status)
	if err != nil {
		log.Printf("Shape: failed to fetch session %d: %v", sessionID, err)
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"session_id":  sessionID,
				"mac":         mac,
				"shaped_mbps": shapedMbps,
				"applied":     false,
				"apply_error": "session not found after update",
			},
		})
		return
	}

	// Live-apply: only when session is active, MAC is known, and the portal
	// interface has FQ_CODEL per-device shaping active.
	applied := false
	applyError := ""
	if status == "active" && mac != "" && clientIP != "" && req.Mbps > 0 {
		iface := ifaceForClientIP(h.DB, clientIP)
		if iface != "" {
			rules := loadQdiscRules(h.DB)
			if rule, ok := rules[iface]; ok && rule.Qdisc == "fq_codel" && rule.PerDeviceBwMbps > 0 {
				if err := ChangeClientClassRate(iface, mac, req.Mbps); err != nil {
					applyError = err.Error()
					log.Printf("Shape: live-apply failed for session %d (%s): %v", sessionID, mac, err)
				} else {
					applied = true
				}
			} else {
				applyError = "portal qdisc is not FQ_CODEL with per-device shaping"
			}
		} else {
			applyError = "no portal interface found for client IP"
		}
	} else if req.Mbps == 0 {
		// Clearing the override — nothing to apply live (the class reverts
		// to the global per_device rate on next EnsurePerDeviceClass cycle).
		applied = true
	}

	logAction(h.DB, "INFO", "session", fmt.Sprintf("Session %d speed override: %d Mbps (applied=%v)", sessionID, req.Mbps, applied))
	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"data": map[string]interface{}{
			"session_id":  sessionID,
			"mac":         mac,
			"shaped_mbps": shapedMbps,
			"applied":     applied,
			"apply_error": applyError,
		},
	})
}

// ============================================
// PAUSE / RESUME (public, portal-facing)
// ============================================

// Pause freezes the caller's active session: timer stops and internet
// access is revoked (captive-rules unauth). The remaining_seconds at
// pause time is snapshotted so Resume can rebuild expires_at.
func (h *SessionHandler) Pause(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	clientIP := clientIPFromRequest(r)
	clientMAC := resolveClientMAC(clientIP)
	if clientMAC == "" {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Cannot resolve device MAC"})
		return
	}

	// F4: Load pause rules and check the per-session pause limit.
	pauseRules := loadPauseRules(h.DB)

	// Find the active, non-paused session for this MAC
	var id int
	var expiresAt time.Time
	var pauseCount sql.NullInt64
	var pausable bool
	var expirationHours int
	err := h.DB.QueryRow(`
		SELECT id, expires_at, COALESCE(pause_count, 0), COALESCE(pausable, true), COALESCE(expiration_hours, 0) FROM sessions
		WHERE status = 'active' AND expires_at > NOW() AND paused_at IS NULL
		  AND client_mac = $1
		ORDER BY started_at DESC LIMIT 1
	`, clientMAC).Scan(&id, &expiresAt, &pauseCount, &pausable, &expirationHours)
	if err == sql.ErrNoRows {
		sendJSON(w, http.StatusNotFound, models.APIResponse{Success: false, Message: "No active non-paused session found"})
		return
	} else if err != nil {
		log.Printf("Pause: query failed: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to pause session"})
		return
	}

	// Consumable rates (pausable = false) cannot be paused: there is no
	// Pause button in the portal and the backend enforces the same rule.
	if !pausable {
		sendJSON(w, http.StatusForbidden, models.APIResponse{
			Success: false,
			Message: "This rate is consumable and cannot be paused",
		})
		return
	}

	// F4: Enforce pause limit (0 = unlimited)
	if pauseRules.PauseLimit > 0 && int(pauseCount.Int64) >= pauseRules.PauseLimit {
		sendJSON(w, http.StatusForbidden, models.APIResponse{
			Success: false,
			Message: "Pause limit reached (" + strconv.Itoa(int(pauseCount.Int64)) + "/" + strconv.Itoa(pauseRules.PauseLimit) + ")",
		})
		return
	}

	remaining := int(time.Until(expiresAt).Seconds())
	if remaining < 0 {
		remaining = 0
	}

	// When the rate carries a pause-expiry window, set an absolute wall-clock
	// deadline (paused_at + expiration_hours). If the user stays paused past
	// it, ExpireOverduePausedSessions forces expiry and the user must insert
	// coin again. expiration_hours = 0 means frozen indefinitely (legacy).
	var pauseDeadline interface{} = nil
	if expirationHours > 0 {
		pauseDeadline = time.Now().Add(time.Duration(expirationHours) * time.Hour)
	}

	// F2: Tight WHERE clause prevents pausing an already-expired session.
	// RowsAffected check catches the race where expiry ran between the
	// SELECT above and this UPDATE.
	res, err := h.DB.Exec(`
		UPDATE sessions
		SET paused_at = NOW(), remaining_seconds_at_pause = $1,
		    pause_count = COALESCE(pause_count, 0) + 1,
		    pause_expires_at = $2
		WHERE id = $3 AND status = 'active' AND expires_at > NOW() AND paused_at IS NULL
	`, remaining, pauseDeadline, id)
	if err != nil {
		log.Printf("Pause: update failed: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to pause session"})
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		sendJSON(w, http.StatusConflict, models.APIResponse{Success: false, Message: "No active session to pause"})
		return
	}

	// Close internet access for this MAC
	runCaptiveRules(h.DB, "unauth", clientMAC, "pause")
	// Remove per-device tc class+filter for FQ_CODEL per-device mode.
	EnsurePerDeviceClass(h.DB, "", clientMAC, clientIP, "remove")

	pausedAt := time.Now()
	logAction(h.DB, "INFO", "session", "Session "+strconv.Itoa(id)+" paused ("+clientMAC+"): "+strconv.Itoa(remaining)+"s remaining")

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"data": map[string]interface{}{
			"paused_at":         pausedAt.Format(time.RFC3339),
			"remaining_seconds": remaining,
		},
	})
}

// Resume unfreezes a paused session: timer continues, internet is
// re-authorized. expires_at is rebuilt from the snapshotted remaining.
func (h *SessionHandler) Resume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	clientIP := clientIPFromRequest(r)
	clientMAC := resolveClientMAC(clientIP)
	if clientMAC == "" {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Cannot resolve device MAC"})
		return
	}

	// Find the active, currently-paused session
	var id, remainingAtPause int
	var pauseDeadline sql.NullTime
	err := h.DB.QueryRow(`
		SELECT id, COALESCE(remaining_seconds_at_pause, 0), pause_expires_at FROM sessions
		WHERE status = 'active' AND paused_at IS NOT NULL
		  AND client_mac = $1
		ORDER BY started_at DESC LIMIT 1
	`, clientMAC).Scan(&id, &remainingAtPause, &pauseDeadline)
	if err == sql.ErrNoRows {
		sendJSON(w, http.StatusNotFound, models.APIResponse{Success: false, Message: "No active paused session found"})
		return
	} else if err != nil {
		log.Printf("Resume: query failed: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to resume session"})
		return
	}

	// If the session's pause window has already elapsed (the user stayed
	// paused longer than the rate's expiration_hours), the purchase is
	// forfeited: remaining becomes 0 and the user must insert coin again.
	if pauseDeadline.Valid && pauseDeadline.Time.Before(time.Now()) {
		forceExpirePaused(h.DB, id, clientIP, clientMAC, "pause-window-expired")
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"success":   true,
			"expired":   true,
			"remaining": 0,
			"message":   "Your paused session expired — insert coin again to regain internet access.",
		})
		return
	}

	newExpiresAt := time.Now().Add(time.Duration(remainingAtPause) * time.Second)

	// F7: Tight WHERE clause + RowsAffected check.
	// pause_count is NOT reset on resume (only on fresh start).
	res, err := h.DB.Exec(`
		UPDATE sessions
		SET paused_at = NULL, remaining_seconds_at_pause = NULL, pause_expires_at = NULL,
		    expires_at = NOW() + ($1::int * INTERVAL '1 second'),
		    remaining_seconds = $1
		WHERE id = $2 AND status = 'active' AND paused_at IS NOT NULL
	`, remainingAtPause, id)
	if err != nil {
		log.Printf("Resume: update failed: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to resume session"})
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		sendJSON(w, http.StatusConflict, models.APIResponse{Success: false, Message: "No active paused session to resume"})
		return
	}

	// Re-open internet access
	runCaptiveRules(h.DB, "auth", clientMAC, "resume")
	// Add per-device tc class+filter for FQ_CODEL per-device mode.
	EnsurePerDeviceClass(h.DB, "", clientMAC, clientIP, "add")

	resumedAt := time.Now()
	logAction(h.DB, "INFO", "session", "Session "+strconv.Itoa(id)+" resumed ("+clientMAC+"): "+strconv.Itoa(remainingAtPause)+"s remaining")

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"data": map[string]interface{}{
			"resumed_at":        resumedAt.Format(time.RFC3339),
			"expires_at":        newExpiresAt.Format(time.RFC3339),
			"remaining_seconds": remainingAtPause,
		},
	})
}

// ============================================
// EXPIRY ENFORCEMENT
// ============================================

// forceExpirePaused expires a single paused session whose pause window has
// elapsed and revokes the client's internet access. remaining is zeroed so
// the user must insert coin again to regain internet.
func forceExpirePaused(db *sql.DB, id int, ip, mac, reason string) {
	_, err := db.Exec(`
		UPDATE sessions
		SET status = 'expired', expired_at = NOW(), remaining_seconds = 0,
		    paused_at = NULL, remaining_seconds_at_pause = NULL, pause_expires_at = NULL
		WHERE id = $1 AND status = 'active'
	`, id)
	if err != nil {
		log.Printf("forceExpirePaused: update session %d failed: %v", id, err)
		return
	}
	log.Printf("session %d force-expired (%s, ip=%s, mac=%s)", id, reason, ip, mac)
	logAction(db, "INFO", "session", "Session "+strconv.Itoa(id)+" pause-expired ("+ip+") — user must insert coin again")
	runCaptiveRules(db, "unauth", mac, reason)
	EnsurePerDeviceClass(db, "", mac, ip, "remove")
}

// ExpireOverduePausedSessions forcibly expires every paused session whose
// pause_expires_at deadline has passed. A user who pauses a time-limited rate
// and does not resume before the rate's expiration_hours window forfeits the
// remaining time and must insert coin again to regain internet.
func ExpireOverduePausedSessions(db *sql.DB) int {
	rows, err := db.Query(`
		SELECT id, COALESCE(client_mac, ''), COALESCE(client_ip, '')
		FROM sessions
		WHERE status = 'active' AND paused_at IS NOT NULL
		  AND pause_expires_at IS NOT NULL AND pause_expires_at <= NOW()
	`)
	if err != nil {
		log.Printf("Error listing overdue paused sessions: %v", err)
		return 0
	}
	defer rows.Close()

	expired := 0
	for rows.Next() {
		var id int
		var mac, ip string
		if err := rows.Scan(&id, &mac, &ip); err != nil {
			continue
		}
		forceExpirePaused(db, id, ip, mac, "pause-window-expired")
		expired++
	}
	return expired
}

// ExpireOverdueSessions expires every active session whose expires_at has
// passed (or that predates the expires_at column — legacy demo rows) and
// closes each client's internet access. Paused sessions are FROZEN and
// must never be expired here.
func ExpireOverdueSessions(db *sql.DB, reason string) int {
	rows, err := db.Query(`
		UPDATE sessions
		SET status = 'expired', expired_at = NOW(), remaining_seconds = 0
		WHERE status = 'active' AND paused_at IS NULL
		  AND (expires_at <= NOW() OR expires_at IS NULL)
		RETURNING id, COALESCE(client_mac, ''), COALESCE(client_ip, '')
	`)
	if err != nil {
		log.Printf("Error expiring sessions: %v", err)
		return 0
	}
	defer rows.Close()

	expired := 0
	for rows.Next() {
		var id int
		var mac, ip string
		if err := rows.Scan(&id, &mac, &ip); err != nil {
			log.Printf("Error scanning expired session: %v", err)
			continue
		}
		expired++
		log.Printf("session %d expired (%s, ip=%s, mac=%s)", id, reason, ip, mac)
		logAction(db, "INFO", "session", "Session "+strconv.Itoa(id)+" expired ("+ip+")")
		runCaptiveRules(db, "unauth", mac, reason)
		// Remove per-device tc class+filter for FQ_CODEL per-device mode.
		EnsurePerDeviceClass(db, "", mac, ip, "remove")
	}
	if err := rows.Err(); err != nil {
		log.Printf("Error reading expired sessions: %v", err)
	}
	return expired
}

// StartExpiryEnforcer recovers the iptables state after a reboot/restart
// (auth every still-active non-paused MAC, expire+unauth the overdue ones)
// and then enforces expiry every 30 seconds in the background.
func StartExpiryEnforcer(db *sql.DB) {
	// Startup recovery: clear the overdue first so a stale session cannot
	// be re-authorized below.
	if n := ExpireOverdueSessions(db, "startup-recovery"); n > 0 {
		log.Printf("startup recovery: expired %d overdue session(s)", n)
	}
	// Expire paused sessions whose pause window elapsed while offline.
	if n := ExpireOverduePausedSessions(db); n > 0 {
		log.Printf("startup recovery: expired %d overdue paused session(s)", n)
	}

	// Re-auth only active NON-PAUSED MACs. Paused sessions have their
	// internet closed (captive-rules unauth on pause), so they must NOT
	// be re-authorized here.
	rows, err := db.Query(`
		SELECT DISTINCT client_mac FROM sessions
		WHERE status = 'active' AND expires_at > NOW() AND paused_at IS NULL
		  AND COALESCE(client_mac, '') <> ''
	`)
	if err != nil {
		log.Printf("startup recovery: failed to list active sessions: %v", err)
	} else {
		defer rows.Close()
		for rows.Next() {
			var mac string
			if err := rows.Scan(&mac); err != nil {
				continue
			}
			runCaptiveRules(db, "auth", mac, "startup-recovery")
		}
	}

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			ExpireOverdueSessions(db, "expire")
			// Forcibly expire paused sessions whose pause window elapsed.
			ExpireOverduePausedSessions(db)
			// Auto-clear expired bans (belt-and-brazes on top of
			// the per-status-poll activeBan check).
			CleanExpiredBans(db)
		}
	}()
}

// ============================================
// HELPERS
// ============================================

// Helper to log actions
func logAction(db *sql.DB, level, component, message string) {
	_, err := db.Exec(`
		INSERT INTO system_logs (level, component, message, created_at)
		VALUES ($1, $2, $3, NOW())
	`, level, component, message)
	if err != nil {
		log.Printf("Failed to log action: %v", err)
	}
}

// Helper to make timestamp
func makeTimestamp() int64 {
	return time.Now().Unix()
}
