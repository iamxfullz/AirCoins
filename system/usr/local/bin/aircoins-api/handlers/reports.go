package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"
)

type ReportsHandler struct {
	DB *sql.DB
}

// GetEarningsSummary returns aggregated earnings summary for a date range
func (h *ReportsHandler) GetEarningsSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	from, to := parseDateRange(r)

	var totalEarnings, totalCoins, totalSessions int
	err := h.DB.QueryRow(`
		SELECT COALESCE(SUM(total_earnings),0) as earnings, 
		       COALESCE(SUM(total_coins),0) as coins, 
		       COALESCE(SUM(total_sessions),0) as sessions 
		FROM daily_stats 
		WHERE date >= $1 AND date <= $2
	`, from, to).Scan(&totalEarnings, &totalCoins, &totalSessions)
	if err != nil {
		log.Printf("Error fetching earnings summary: %v", err)
		sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "Failed to fetch earnings summary",
		})
		return
	}

	// Calculate average session duration
	var avgDurationSec sql.NullFloat64
	err = h.DB.QueryRow(`
		SELECT AVG(EXTRACT(EPOCH FROM (ended_at - started_at))) 
		FROM sessions 
		WHERE started_at >= $1 AND started_at <= $2 AND ended_at IS NOT NULL
	`, from, to).Scan(&avgDurationSec)
	if err != nil {
		log.Printf("Error fetching avg duration: %v", err)
	}

	avgDurationMinutes := 0.0
	if avgDurationSec.Valid {
		avgDurationMinutes = avgDurationSec.Float64 / 60.0
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"summary": map[string]interface{}{
			"total_earnings":       float64(totalEarnings),
			"total_coins":          totalCoins,
			"total_sessions":       totalSessions,
			"avg_duration_minutes": roundTo2(avgDurationMinutes),
			"period_from":          from.Format("2006-01-02"),
			"period_to":            to.Format("2006-01-02"),
		},
	})
}

// GetDailyBreakdown returns daily statistics for a date range
func (h *ReportsHandler) GetDailyBreakdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	from, to := parseDateRange(r)

	rows, err := h.DB.Query(`
		SELECT date, total_earnings, total_coins, total_sessions 
		FROM daily_stats 
		WHERE date >= $1 AND date <= $2 
		ORDER BY date DESC
	`, from, to)
	if err != nil {
		log.Printf("Error fetching daily breakdown: %v", err)
		sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "Failed to fetch daily breakdown",
		})
		return
	}
	defer rows.Close()

	type DailyStat struct {
		Date     string  `json:"date"`
		Earnings float64 `json:"earnings"`
		Coins    int     `json:"coins"`
		Sessions int     `json:"sessions"`
	}

	daily := make([]DailyStat, 0)
	for rows.Next() {
		var ds DailyStat
		var dateVal time.Time
		var earnings int
		if err := rows.Scan(&dateVal, &earnings, &ds.Coins, &ds.Sessions); err != nil {
			log.Printf("Error scanning daily stat: %v", err)
			continue
		}
		ds.Date = dateVal.Format("2006-01-02")
		ds.Earnings = float64(earnings)
		daily = append(daily, ds)
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"daily": daily,
	})
}

// GetCoinEvents returns paginated coin events for a date range
func (h *ReportsHandler) GetCoinEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	from, to := parseDateRange(r)

	// Parse pagination params
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > 100 {
		limit = 100
	}

	offset := 0
	if o := r.URL.Query().Get("offset"); o != "" {
		if parsed, err := strconv.Atoi(o); err == nil && parsed >= 0 {
			offset = parsed
		}
	}

	// Get total count
	var total int
	err := h.DB.QueryRow(`
		SELECT COUNT(*) 
		FROM coin_events 
		WHERE detected_at >= $1 AND detected_at <= $2
	`, from, to).Scan(&total)
	if err != nil {
		log.Printf("Error counting coin events: %v", err)
		sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "Failed to count coin events",
		})
		return
	}

	// Get paginated events
	rows, err := h.DB.Query(`
		SELECT id, coin_value, detected_at, processed, session_id 
		FROM coin_events 
		WHERE detected_at >= $1 AND detected_at <= $2 
		ORDER BY detected_at DESC 
		LIMIT $3 OFFSET $4
	`, from, to, limit, offset)
	if err != nil {
		log.Printf("Error fetching coin events: %v", err)
		sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "Failed to fetch coin events",
		})
		return
	}
	defer rows.Close()

	type CoinEventRow struct {
		ID         int       `json:"id"`
		CoinValue  int       `json:"coin_value"`
		DetectedAt time.Time `json:"detected_at"`
		Processed  bool      `json:"processed"`
		SessionID  *int      `json:"session_id,omitempty"`
	}

	events := make([]CoinEventRow, 0)
	for rows.Next() {
		var e CoinEventRow
		if err := rows.Scan(&e.ID, &e.CoinValue, &e.DetectedAt, &e.Processed, &e.SessionID); err != nil {
			log.Printf("Error scanning coin event: %v", err)
			continue
		}
		events = append(events, e)
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"events": events,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// parseDateRange extracts from/to query params, defaulting to last 30 days
func parseDateRange(r *http.Request) (time.Time, time.Time) {
	q := r.URL.Query()

	// Default to last 30 days
	to := time.Now()
	from := to.AddDate(0, 0, -30)

	if f := q.Get("from"); f != "" {
		if t, err := time.Parse("2006-01-02", f); err == nil {
			from = t
		}
	}

	if t := q.Get("to"); t != "" {
		if parsed, err := time.Parse("2006-01-02", t); err == nil {
			to = parsed
		}
	}

	return from, to
}

// roundTo2 rounds a float to 2 decimal places
func roundTo2(val float64) float64 {
	return float64(int(val*100+0.5)) / 100
}

	// ResetSalesReports deletes financial sales data: daily_stats and coin_events.
// It deliberately preserves the sessions table so active user sessions and device time are never wiped out.
func (h *ReportsHandler) ResetSalesReports(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse optional scope from request body
	var req struct {
		Scope string `json:"scope"` // "all" (default), "daily_stats", "coin_events"
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	scope := req.Scope
	if scope == "" {
		scope = "all"
	}

	deleted := map[string]int{}

	tx, err := h.DB.Begin()
	if err != nil {
		log.Printf("ResetSalesReports: begin tx failed: %v", err)
		sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false, "message": "Failed to start reset transaction",
		})
		return
	}
	defer tx.Rollback()

	if scope == "all" || scope == "coin_events" {
		res, err := tx.Exec("DELETE FROM coin_events")
		if err != nil {
			log.Printf("ResetSalesReports: delete coin_events failed: %v", err)
			sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"success": false, "message": "Failed to delete coin events: " + err.Error(),
			})
			return
		}
		n, _ := res.RowsAffected()
		deleted["coin_events"] = int(n)
	}

	if scope == "all" || scope == "daily_stats" {
		res, err := tx.Exec("DELETE FROM daily_stats")
		if err != nil {
			log.Printf("ResetSalesReports: delete daily_stats failed: %v", err)
			sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"success": false, "message": "Failed to delete daily stats: " + err.Error(),
			})
			return
		}
		n, _ := res.RowsAffected()
		deleted["daily_stats"] = int(n)
	}

	if err := tx.Commit(); err != nil {
		log.Printf("ResetSalesReports: commit failed: %v", err)
		sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false, "message": "Failed to commit reset",
		})
		return
	}

	total := 0
	for _, v := range deleted {
		total += v
	}

	log.Printf("ResetSalesReports: reset complete (scope=%s) — deleted %d total rows: %v", scope, total, deleted)

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("Sales reports reset — deleted %d records", total),
		"deleted": deleted,
	})
}
