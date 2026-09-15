package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// apiVersion is set from main via SetAPIVersion at startup.
var apiVersion = "dev"

// SetAPIVersion stores the build version for use in middleware headers
// and other handler responses. Call once from main before starting the server.
func SetAPIVersion(v string) {
	apiVersion = v
}

type tokenEntry struct {
	AdminID   int
	Username  string
	ExpiresAt time.Time
}

var (
	tokenStore = make(map[string]tokenEntry)
	tokenMu    sync.RWMutex
)

// GenerateToken creates a random token for an admin user with 24-hour expiry.
func GenerateToken(adminID int, username string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// fallback — should never happen
		return hex.EncodeToString([]byte(time.Now().String()))
	}
	token := hex.EncodeToString(b)

	tokenMu.Lock()
	tokenStore[token] = tokenEntry{
		AdminID:   adminID,
		Username:  username,
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}
	tokenMu.Unlock()

	return token
}

// ValidateToken checks whether a token is valid and not expired.
func ValidateToken(token string) (adminID int, username string, valid bool) {
	tokenMu.RLock()
	entry, ok := tokenStore[token]
	tokenMu.RUnlock()

	if !ok {
		return 0, "", false
	}

	if time.Now().After(entry.ExpiresAt) {
		// Expired — remove it
		tokenMu.Lock()
		delete(tokenStore, token)
		tokenMu.Unlock()
		return 0, "", false
	}

	return entry.AdminID, entry.Username, true
}

// LicenseGateMiddleware gates most admin routes behind a valid license.
// Routes that must remain reachable even when the license is invalid
// (license endpoints, updater, login, health) are passed through so
// users can still update software or purchase a license.
func LicenseGateMiddleware(lh *LicenseHandler, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		// Always allow these routes through even when locked:
		// - License endpoints (to activate/purchase)
		// - Update endpoints (to update software)
		// - Login (to authenticate)
		// - Health (for auth check)
		if strings.HasPrefix(path, "/api/admin/license/") ||
			strings.HasPrefix(path, "/api/admin/update/") ||
			strings.HasPrefix(path, "/api/admin/login") ||
			strings.HasPrefix(path, "/api/health") {
			next.ServeHTTP(w, r)
			return
		}
		if !lh.IsLicenseValid() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusPaymentRequired)
			lh.mu.RLock()
			st := lh.state.Status
			lh.mu.RUnlock()
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error":   "license_required",
				"status":  st,
				"message": "A valid license is required to access this resource.",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// AuthMiddleware wraps a handler, requiring a valid Bearer token.
func AuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Version", apiVersion)

		auth := r.Header.Get("Authorization")
		if auth == "" || !strings.HasPrefix(auth, "Bearer ") {
			sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}

		token := strings.TrimPrefix(auth, "Bearer ")
		_, _, valid := ValidateToken(token)
		if !valid {
			sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}

		next.ServeHTTP(w, r)
	}
}
