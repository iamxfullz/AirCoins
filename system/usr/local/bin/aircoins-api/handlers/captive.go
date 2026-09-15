package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strings"
)

// captive.go manages captive portal DNS refresh for authorized clients.
// When the ISP changes, authorized clients' DNS rules may still point to
// the old ISP's DNS server. The refresh-dns endpoint triggers a refresh
// of all authorized clients' DNS rules.

// RefreshDNSResponse is the response from the refresh-dns endpoint
type RefreshDNSResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Count   int    `json:"count,omitempty"`
}

// RefreshDNSHandler handles POST /api/admin/captive/refresh-dns
// It runs `aircoins-captive-rules refresh` to update DNS rules for all
// authorized clients after an ISP change.
func RefreshDNSHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(RefreshDNSResponse{
			Success: false,
			Message: "Method not allowed",
		})
		return
	}

	// Run aircoins-captive-rules refresh
	out, err := exec.Command("aircoins-captive-rules", "refresh").CombinedOutput()
	output := strings.TrimSpace(string(out))

	if err != nil {
		log.Printf("RefreshDNSHandler: refresh failed: %v (%s)", err, output)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(RefreshDNSResponse{
			Success: false,
			Message: "Failed to refresh DNS rules: " + output,
		})
		return
	}

	// Parse the output to get the count
	// Expected output: "Refreshed N client(s) with new DNS: x.x.x.x"
	count := 0
	if strings.Contains(output, "Refreshed") {
		// Try to extract count from output
		parts := strings.Split(output, " ")
		for i, p := range parts {
			if p == "Refreshed" && i+1 < len(parts) {
				// Next part should be the number
				fmt.Sscanf(parts[i+1], "%d", &count)
				break
			}
		}
	}

	// Check if DNS was unchanged
	if strings.Contains(output, "unchanged") || strings.Contains(output, "no refresh needed") {
		json.NewEncoder(w).Encode(RefreshDNSResponse{
			Success: true,
			Message: output,
			Count:   0,
		})
		return
	}

	log.Printf("RefreshDNSHandler: %s", output)
	json.NewEncoder(w).Encode(RefreshDNSResponse{
		Success: true,
		Message: output,
		Count:   count,
	})
}
