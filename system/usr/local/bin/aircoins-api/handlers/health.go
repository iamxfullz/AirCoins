package handlers

import (
	"aircoins-api/models"
	"encoding/json"
	"net/http"
	"time"
)

// HealthResponse represents the structure of the system health check response
type HealthResponse struct {
	Status    string    `json:"status"`
	Version   string    `json:"version,omitempty"`
	Database  string    `json:"database"`
	Timestamp time.Time `json:"timestamp"`
}

// SystemHealth reports the current status and health of the API server and its connections.
func SystemHealth(version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dbStatus := "connected"
		if models.DB == nil {
			dbStatus = "disconnected"
		} else if err := models.DB.Ping(); err != nil {
			dbStatus = "error: " + err.Error()
		}

		resp := HealthResponse{
			Status:    "OK",
			Version:   version,
			Database:  dbStatus,
			Timestamp: time.Now().UTC(),
		}

		w.Header().Set("Content-Type", "application/json")
		if dbStatus != "connected" {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}
