package handlers

import (
	"aircoins-api/models"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type SystemHandler struct {
	DB *sql.DB
}

// ============================================
// SERVICE STATUS CACHE
// ============================================

type serviceInfo struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Description string `json:"description"`
}

type servicesCache struct {
	mu        sync.Mutex
	data      []serviceInfo
	fetchedAt time.Time
}

var svcCache = &servicesCache{}

var knownServices = []struct {
	Name        string
	Description string
}{
	{"lighttpd", "Web Server"},
	{"aircoins-api", "API Server"},
	{"gpio-coin-listener", "GPIO Coin Listener"},
	{"pisowifi-session", "Session Manager"},
}

// GetServices returns the status of all monitored systemd services.
func (h *SystemHandler) GetServices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	svcCache.mu.Lock()
	if time.Since(svcCache.fetchedAt) < 10*time.Second && svcCache.data != nil {
		cached := svcCache.data
		svcCache.mu.Unlock()
		sendJSON(w, http.StatusOK, map[string]interface{}{"services": cached})
		return
	}
	svcCache.mu.Unlock()

	// Fetch fresh data
	var result []serviceInfo
	for _, svc := range knownServices {
		status := "inactive"
		out, err := exec.Command("systemctl", "is-active", svc.Name).Output()
		if err == nil {
			status = strings.TrimSpace(string(out))
		}
		result = append(result, serviceInfo{
			Name:        svc.Name,
			Status:      status,
			Description: svc.Description,
		})
	}

	svcCache.mu.Lock()
	svcCache.data = result
	svcCache.fetchedAt = time.Now()
	svcCache.mu.Unlock()

	sendJSON(w, http.StatusOK, map[string]interface{}{"services": result})
}

// ControlService starts, stops, or restarts a whitelisted systemd service.
func (h *SystemHandler) ControlService(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse path: /api/system/services/{name}/{action}
	path := strings.TrimPrefix(r.URL.Path, "/api/system/services/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid path. Use /api/system/services/{name}/{action}"})
		return
	}

	serviceName := parts[0]
	action := parts[1]

	// Whitelist services
	allowed := map[string]bool{
		"lighttpd":           true,
		"aircoins-api":       true,
		"gpio-coin-listener": true,
		"pisowifi-session":   true,
	}
	if !allowed[serviceName] {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Unknown service: " + serviceName})
		return
	}

	// Whitelist actions
	allowedActions := map[string]bool{"start": true, "stop": true, "restart": true}
	if !allowedActions[action] {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid action: " + action + ". Allowed: start, stop, restart"})
		return
	}

	cmd := exec.Command("systemctl", action, serviceName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("systemctl %s %s failed: %v — %s", action, serviceName, err, string(output))
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{
			Success: false,
			Message: fmt.Sprintf("Failed to %s %s: %s", action, serviceName, string(output)),
		})
		return
	}

	// Invalidate cache so next GetServices fetches fresh data
	svcCache.mu.Lock()
	svcCache.fetchedAt = time.Time{}
	svcCache.mu.Unlock()

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"service": serviceName,
		"action":  action,
	})
}

// ============================================
// SYSTEM INFO
// ============================================

// GetSystemInfo returns real system information from /proc and os.
func (h *SystemHandler) GetSystemInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	info := map[string]interface{}{}

	// Hostname
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	info["hostname"] = hostname

	// Board model
	info["board_model"] = readBoardModel()

	// Uptime
	uptimeSec, uptimeStr := readUptime()
	info["uptime"] = uptimeStr
	info["uptime_seconds"] = uptimeSec

	// Disk usage
	info["disk"] = readDiskUsage()

	// Memory
	info["memory"] = readMemory()

	// Load average
	info["load_avg"] = readLoadAvg()

	sendJSON(w, http.StatusOK, info)
}

func readBoardModel() string {
	// Try /proc/device-tree/model first (ARM boards)
	data, err := os.ReadFile("/proc/device-tree/model")
	if err == nil {
		model := strings.TrimSpace(strings.TrimRight(string(data), "\x00"))
		if model != "" {
			return model
		}
	}

	// Try /etc/os-release for PRETTY_NAME
	data, err = os.ReadFile("/etc/os-release")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "PRETTY_NAME=") {
				val := strings.TrimPrefix(line, "PRETTY_NAME=")
				val = strings.Trim(val, "\"")
				return val
			}
		}
	}

	return "Unknown"
}

func readUptime() (int64, string) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, "unknown"
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0, "unknown"
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, "unknown"
	}
	totalSec := int64(secs)
	days := totalSec / 86400
	hours := (totalSec % 86400) / 3600
	mins := (totalSec % 3600) / 60
	return totalSec, fmt.Sprintf("%dd %dh %dm", days, hours, mins)
}

func readDiskUsage() map[string]interface{} {
	out, err := exec.Command("df", "-B1", "/").Output()
	if err != nil {
		return map[string]interface{}{"total": "?", "used": "?", "available": "?", "percent": "?"}
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return map[string]interface{}{"total": "?", "used": "?", "available": "?", "percent": "?"}
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 5 {
		return map[string]interface{}{"total": "?", "used": "?", "available": "?", "percent": "?"}
	}

	total, _ := strconv.ParseInt(fields[1], 10, 64)
	used, _ := strconv.ParseInt(fields[2], 10, 64)
	avail, _ := strconv.ParseInt(fields[3], 10, 64)

	return map[string]interface{}{
		"total":     formatBytes(total),
		"used":      formatBytes(used),
		"available": formatBytes(avail),
		"percent":   fields[4],
	}
}

func formatBytes(b int64) string {
	const gb = 1073741824
	const mb = 1048576
	if b >= gb {
		return fmt.Sprintf("%.1fG", float64(b)/float64(gb))
	}
	return fmt.Sprintf("%.1fM", float64(b)/float64(mb))
}

func readMemory() map[string]interface{} {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return map[string]interface{}{"total_mb": 0, "used_mb": 0, "available_mb": 0, "percent": "0%"}
	}

	var memTotal, memAvailable int64
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			memTotal = parseMemInfoValue(line)
		} else if strings.HasPrefix(line, "MemAvailable:") {
			memAvailable = parseMemInfoValue(line)
		}
	}

	used := memTotal - memAvailable
	pct := int64(0)
	if memTotal > 0 {
		pct = (used * 100) / memTotal
	}

	return map[string]interface{}{
		"total_mb":     memTotal / 1024,
		"used_mb":      used / 1024,
		"available_mb": memAvailable / 1024,
		"percent":      fmt.Sprintf("%d%%", pct),
	}
}

func parseMemInfoValue(line string) int64 {
	// "MemTotal:       1024000 kB"
	fields := strings.Fields(line)
	if len(fields) >= 2 {
		v, _ := strconv.ParseInt(fields[1], 10, 64)
		return v
	}
	return 0
}

func readLoadAvg() []float64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return []float64{0, 0, 0}
	}
	fields := strings.Fields(string(data))
	result := make([]float64, 3)
	for i := 0; i < 3 && i < len(fields); i++ {
		result[i], _ = strconv.ParseFloat(fields[i], 64)
	}
	return result
}

// ============================================
// BOARD INFO
// ============================================

// GetBoardInfo returns board detection and GPIO capability info.
func (h *SystemHandler) GetBoardInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Execute the shared GPIO library to get board info
	cmd := exec.Command("bash", "-c",
		`source /usr/local/bin/aircoins-gpio-lib && `+
			`detect_board && get_gpio_method && `+
			`echo "$BOARD_FAMILY" && echo "$BOARD_MODEL" && echo "$BOARD_CHIPSET" && `+
			`echo "$GPIO_METHOD" && echo "$GPIO_PIN_COUNT" && `+
			`get_available_pins`)

	out, err := cmd.Output()
	if err != nil {
		log.Printf("Failed to run gpio-lib detection: %v", err)
		// Fallback: read /proc/device-tree/model directly
		boardModel := readBoardModel()
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"board_model":    boardModel,
			"board_family":   "unknown",
			"board_chipset":  "unknown",
			"gpio_method":    "none",
			"gpio_pin_count": 40,
			"available_pins": []int{},
		})
		return
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 6 {
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Unexpected gpio-lib output"})
		return
	}

	boardFamily := strings.TrimSpace(lines[0])
	boardModel := strings.TrimSpace(lines[1])
	boardChipset := strings.TrimSpace(lines[2])
	gpioMethod := strings.TrimSpace(lines[3])
	gpioPinCount := 40
	if n, err := strconv.Atoi(strings.TrimSpace(lines[4])); err == nil {
		gpioPinCount = n
	}

	// Parse available pins
	var availablePins []int
	if len(lines) > 5 {
		for _, s := range strings.Fields(strings.TrimSpace(lines[5])) {
			if n, err := strconv.Atoi(s); err == nil {
				availablePins = append(availablePins, n)
			}
		}
	}
	if availablePins == nil {
		availablePins = []int{}
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"board_model":    boardModel,
		"board_family":   boardFamily,
		"board_chipset":  boardChipset,
		"gpio_method":    gpioMethod,
		"gpio_pin_count": gpioPinCount,
		"available_pins": availablePins,
	})
}

// ============================================
// STATUS (existing)
// ============================================

// Status returns system health check
func (h *SystemHandler) Status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check database connection
	dbStatus := "ok"
	err := h.DB.Ping()
	if err != nil {
		dbStatus = "error: " + err.Error()
	}

	// Check services
	lighttpdRunning := checkServiceRunning("lighttpd")

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"status": "ok",
		"services": map[string]interface{}{
			"database": dbStatus,
			"lighttpd": lighttpdRunning,
		},
		"system_online": lighttpdRunning,
	})
}

// ============================================
// LOGS (enhanced with filtering + pagination)
// ============================================

// Logs returns system logs with optional filtering and pagination.
func (h *SystemHandler) Logs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()

	// Parse limit (default 50, max 200)
	limit := 50
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 200 {
		limit = 200
	}

	// Parse offset (default 0)
	offset := 0
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	level := q.Get("level")
	component := q.Get("component")

	// Build WHERE clause
	var conditions []string
	var args []interface{}
	argIdx := 1

	if level != "" {
		conditions = append(conditions, fmt.Sprintf("level = $%d", argIdx))
		args = append(args, level)
		argIdx++
	}
	if component != "" {
		conditions = append(conditions, fmt.Sprintf("component = $%d", argIdx))
		args = append(args, component)
		argIdx++
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	// Get total count
	countQuery := "SELECT COUNT(*) FROM system_logs " + whereClause
	var total int
	err := h.DB.QueryRow(countQuery, args...).Scan(&total)
	if err != nil {
		log.Printf("Error counting logs: %v", err)
		total = 0
	}

	// Fetch page
	dataQuery := fmt.Sprintf(
		`SELECT id, level, component, message, created_at
		FROM system_logs
		%s
		ORDER BY created_at DESC
		LIMIT $%d OFFSET $%d`,
		whereClause, argIdx, argIdx+1,
	)
	args = append(args, limit, offset)

	rows, err := h.DB.Query(dataQuery, args...)
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

	sendJSON(w, http.StatusOK, models.PaginatedResponse{
		Success: true,
		Data:    logs,
		Total:   total,
		Limit:   limit,
		Offset:  offset,
	})
}

// ============================================
// REBOOT
// ============================================

// Reboot initiates a system reboot. Uses cmd.Start() so the HTTP response
// is sent before the server is killed by the reboot.
func (h *SystemHandler) Reboot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cmd := exec.Command("sudo", "reboot")
	if err := cmd.Start(); err != nil {
		sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "Failed to initiate reboot: " + err.Error(),
		})
		return
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "System is rebooting...",
	})
}

// checkServiceRunning checks if a systemd service is running
func checkServiceRunning(service string) bool {
	cmd := exec.Command("systemctl", "is-active", "--quiet", service)
	err := cmd.Run()
	return err == nil
}
