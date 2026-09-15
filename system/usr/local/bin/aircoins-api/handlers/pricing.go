package handlers

import (
	"aircoins-api/models"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
)

type PricingHandler struct {
	DB *sql.DB
}

// PRICING MODEL
// ------------
// 1 peso coin = 1 pulse from the coin acceptor. A pricing row maps the
// inserted peso amount (= number of pulses) to minutes of internet.
// There is no rate-per-minute: minutes always come from the table, and
// the table starts blank (the operator adds every tier manually).

// Handle handles pricing GET, POST, and DELETE
func (h *PricingHandler) Handle(w http.ResponseWriter, r *http.Request) {
	// Check if this is a DELETE request with an ID in the path
	// Path format: /api/pricing/{id}
	path := r.URL.Path
	if strings.HasPrefix(path, "/api/pricing/") && path != "/api/pricing/" {
		idStr := strings.TrimPrefix(path, "/api/pricing/")
		if idStr != "" {
			if r.Method == http.MethodDelete {
				h.deletePricing(w, r, idStr)
				return
			}
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
	}

	switch r.Method {
	case http.MethodGet:
		h.getPricing(w, r)
	case http.MethodPost:
		h.updatePricing(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *PricingHandler) getPricing(w http.ResponseWriter, r *http.Request) {
	// ?coin=N (alias ?amount=N) resolves a single inserted peso amount to
	// minutes — used by the session manager and the captive portal.
	amountStr := r.URL.Query().Get("coin")
	if amountStr == "" {
		amountStr = r.URL.Query().Get("amount")
	}
	if amountStr != "" {
		h.quotePricing(w, amountStr)
		return
	}

	includeInactive := r.URL.Query().Get("include_inactive") == "true"

	query := `
		SELECT id, coin_value, minutes, active, pausable, expiration_hours, created_at, updated_at
		FROM pricing
	`
	if !includeInactive {
		query += ` WHERE active = true`
	}
	query += ` ORDER BY coin_value ASC`

	rows, err := h.DB.Query(query)
	if err != nil {
		log.Printf("Error fetching pricing: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to fetch pricing"})
		return
	}
	defer rows.Close()

	// Empty (not null) is the normal state of a freshly installed device
	pricing := []models.Pricing{}
	for rows.Next() {
		var p models.Pricing
		if err := rows.Scan(&p.ID, &p.CoinValue, &p.Minutes, &p.Active, &p.Pausable, &p.ExpirationHours, &p.CreatedAt, &p.UpdatedAt); err != nil {
			log.Printf("Error scanning pricing: %v", err)
			continue
		}
		pricing = append(pricing, p)
	}

	sendJSON(w, http.StatusOK, models.APIResponse{Success: true, Data: pricing})
}

// quotePricing answers "how many minutes does P<amount> buy?"
func (h *PricingHandler) quotePricing(w http.ResponseWriter, amountStr string) {
	amount, err := strconv.Atoi(amountStr)
	if err != nil || amount <= 0 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid coin amount"})
		return
	}

	match, err := MinutesForAmount(h.DB, amount)
	if err != nil {
		log.Printf("Error resolving pricing for P%d: %v", amount, err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to resolve pricing"})
		return
	}
	minutes := match.Minutes

	if minutes == 0 {
		sendJSON(w, http.StatusOK, models.APIResponse{
			Success: false,
			Message: "No pricing tier configured for P" + strconv.Itoa(amount),
			Data: map[string]interface{}{
				"coin_value": amount,
				"minutes":    0,
				"seconds":    0,
				"matched":    0,
			},
		})
		return
	}

	sendJSON(w, http.StatusOK, models.APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"coin_value": amount,
			"minutes":    minutes,
			"seconds":    minutes * 60,
			"matched":    match.MatchedCoin,
		},
	})
}

// PricingMatch is the resolved pricing row for an inserted amount.
type PricingMatch struct {
	Minutes         int
	MatchedCoin     int
	Pausable        bool
	ExpirationHours int
}

// MinutesForAmount resolves an inserted peso amount (= pulse count) to minutes
// using the pricing table only, returning the matched row's pause rules too.
//
// Resolution order:
//  1. exact active row for that amount
//  2. otherwise the highest active row whose coin_value is below the amount
//     (partial credit, e.g. P7 with tiers 1/5/10 credits the P5 tier)
//
// Returns Match.Minutes = 0 when the pricing table has nothing usable — callers
// must treat that as "no time credited" and surface it to the operator.
func MinutesForAmount(db *sql.DB, amount int) (PricingMatch, error) {
	if amount <= 0 {
		return PricingMatch{}, nil
	}

	var m PricingMatch
	// Deterministic winner when several active rows share the same
	// coin_value (e.g. an old P1=240min tier left active next to a new
	// P1=3-day tier): prefer the longest duration, then the newest row.
	err := db.QueryRow(`
		SELECT minutes, coin_value, pausable, expiration_hours FROM pricing
		WHERE active = true AND coin_value <= $1
		ORDER BY coin_value DESC, minutes DESC, id DESC
		LIMIT 1
	`, amount).Scan(&m.Minutes, &m.MatchedCoin, &m.Pausable, &m.ExpirationHours)

	if err == sql.ErrNoRows {
		return m, nil
	} else if err != nil {
		return m, err
	}

	return m, nil
}

func (h *PricingHandler) updatePricing(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Pricing []models.PricingRequest `json:"pricing"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid request body"})
		return
	}

	for _, p := range req.Pricing {
		if p.CoinValue <= 0 || p.Minutes <= 0 {
			sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Coin value and minutes must be positive"})
			return
		}
		if p.ExpirationHours < 0 {
			sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Expiration hours must be 0 or greater"})
			return
		}

		active := true
		if p.Active != nil {
			active = *p.Active
		}
		// Default: pausable = true (a consumable rate has to be explicitly
		// created with pausable=false by the operator).
		pausable := true
		if p.Pausable != nil {
			pausable = *p.Pausable
		}

		_, err := h.DB.Exec(`
			INSERT INTO pricing (coin_value, minutes, active, pausable, expiration_hours, updated_at)
			VALUES ($1, $2, $3, $4, $5, NOW())
			ON CONFLICT (coin_value) DO UPDATE
			SET minutes = $2, active = $3, pausable = $4, expiration_hours = $5, updated_at = NOW()
		`, p.CoinValue, p.Minutes, active, pausable, p.ExpirationHours)
		if err != nil {
			log.Printf("Error updating pricing: %v", err)
		}
	}

	logAction(h.DB, "INFO", "pricing", "Pricing updated")
	sendJSON(w, http.StatusOK, models.APIResponse{Success: true, Message: "Pricing updated"})
}

func (h *PricingHandler) deletePricing(w http.ResponseWriter, r *http.Request, idStr string) {
	id, err := strconv.Atoi(idStr)
	if err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid pricing ID"})
		return
	}

	result, err := h.DB.Exec(`UPDATE pricing SET active = false, updated_at = NOW() WHERE id = $1`, id)
	if err != nil {
		log.Printf("Error deleting pricing: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to delete pricing"})
		return
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		log.Printf("Error checking rows affected: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to delete pricing"})
		return
	}

	if rowsAffected == 0 {
		sendJSON(w, http.StatusNotFound, models.APIResponse{Success: false, Message: "Pricing tier not found"})
		return
	}

	logAction(h.DB, "INFO", "pricing", "Pricing tier deactivated: ID "+idStr)
	sendJSON(w, http.StatusOK, models.APIResponse{Success: true, Message: "Pricing tier deactivated"})
}
