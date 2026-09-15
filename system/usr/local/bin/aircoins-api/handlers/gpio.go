package handlers

import (
	"aircoins-api/models"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

type GPIOHandler struct {
	DB *sql.DB
}

// hasLocalGPIO reports whether this build has a local hardware GPIO
// coinslot. SBC builds (arm) do; x86 dev boxes have no GPIO and rely on
// file/API-simulated coin events. This was previously defined alongside the
// (now removed) Sub-Vendo support.
func hasLocalGPIO() bool {
	switch runtime.GOARCH {
	case "amd64", "386":
		return false
	}
	return true
}

// Config handles GPIO configuration (GET and POST)
func (h *GPIOHandler) Config(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.getConfig(w, r)
	case http.MethodPost:
		h.updateConfig(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *GPIOHandler) getConfig(w http.ResponseWriter, r *http.Request) {
	var config models.GPIOConfig
	err := h.DB.QueryRow(`
		SELECT id, pin, coin_value, pulse_mode, COALESCE(board_model, 'auto'),
		       COALESCE(debounce_ms, 30),
		       COALESCE(relay_enabled, false), COALESCE(relay_pin, 5),
		       COALESCE(relay_intensity, 5), updated_at
		FROM gpio_config ORDER BY id DESC LIMIT 1
	`).Scan(&config.ID, &config.Pin, &config.CoinValue, &config.PulseMode, &config.BoardModel, &config.DebounceMs, &config.RelayEnabled, &config.RelayPin, &config.RelayIntensity, &config.UpdatedAt)

	if err == sql.ErrNoRows {
		sendJSON(w, http.StatusOK, models.APIResponse{Success: true, Data: models.GPIOConfig{Pin: 7, CoinValue: 1, PulseMode: "falling", BoardModel: "auto", DebounceMs: 30, RelayEnabled: false, RelayPin: 5, RelayIntensity: 5}})
		return
	} else if err != nil {
		log.Printf("Error fetching GPIO config: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to fetch GPIO config"})
		return
	}

	sendJSON(w, http.StatusOK, models.APIResponse{Success: true, Data: config})
}

func (h *GPIOHandler) updateConfig(w http.ResponseWriter, r *http.Request) {
	var req models.GPIOConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid request body"})
		return
	}

	// req.Pin is a PHYSICAL header pin number (1-40), translated to a
	// kernel GPIO line by aircoins-gpio-lib / readGpioPin
	if req.Pin < 1 || req.Pin > 40 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid physical header pin (1-40)"})
		return
	}

	if req.CoinValue <= 0 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Coin value must be positive"})
		return
	}

	if req.PulseMode != "falling" && req.PulseMode != "rising" && req.PulseMode != "both" {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid pulse mode"})
		return
	}

	if req.BoardModel == "" {
		req.BoardModel = "auto"
	}

	// Debounce: edges arriving within this window after a pulse are
	// contact bounce of the same coin. 0 = keep the 30 ms default;
	// fast acceptors (≈50 ms pulse spacing) under-count above that,
	// so only raise it if one physical pulse double-counts.
	// (Admin > Settings > GPIO).
	if req.DebounceMs == 0 {
		req.DebounceMs = 30
	}
	if req.DebounceMs < 5 || req.DebounceMs > 1000 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Debounce must be between 5 and 1000 ms"})
		return
	}

	// Relay / light pin: 0 = not chosen yet (default to physical pin 5);
	// when enabled it must be a valid header pin and differ from the coin pin.
	if req.RelayPin == 0 {
		req.RelayPin = 5
	}
	if req.RelayPin < 1 || req.RelayPin > 40 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid relay pin (1-40)"})
		return
	}
	if req.RelayEnabled && req.RelayPin == req.Pin {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Relay pin must differ from the coin pin"})
		return
	}

	// Relay intensity: blink tempo 1 (slow) .. 10 (fast dance). 0 = default 5.
	if req.RelayIntensity == 0 {
		req.RelayIntensity = 5
	}
	if req.RelayIntensity < 1 || req.RelayIntensity > 10 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Relay intensity must be between 1 and 10"})
		return
	}

	// Insert new config (keep history)
	_, err := h.DB.Exec(`
		INSERT INTO gpio_config (pin, coin_value, pulse_mode, board_model, debounce_ms, relay_enabled, relay_pin, relay_intensity, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
	`, req.Pin, req.CoinValue, req.PulseMode, req.BoardModel, req.DebounceMs, req.RelayEnabled, req.RelayPin, req.RelayIntensity)

	if err != nil {
		log.Printf("Error saving GPIO config: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to save GPIO config"})
		return
	}

	// Write config file for gpio-coin-listener to read
	writeGPIOConfigFile(req.Pin, req.CoinValue, req.PulseMode, req.BoardModel, req.DebounceMs, req.RelayEnabled, req.RelayPin, req.RelayIntensity)

	logAction(h.DB, "INFO", "gpio", "GPIO config updated: physical pin="+strconv.Itoa(req.Pin)+" board="+req.BoardModel+" debounce="+strconv.Itoa(req.DebounceMs)+"ms")
	sendJSON(w, http.StatusOK, models.APIResponse{Success: true, Message: "GPIO config saved. Restart GPIO listener to apply."})
}

// Test performs a real GPIO pin test.
//
// The incoming "pin" is a PHYSICAL HEADER PIN number (as printed on the
// board), never a kernel/sunxi/BCM number. The test is delegated to
// aircoins-gpio-lib so the admin panel, the CGI pin test and the coin
// listener all resolve the pin — and report busy lines — identically.
func (h *GPIOHandler) Test(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Pin int `json:"pin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid request body"})
		return
	}

	// Preferred path: the shared shell library (knows the header map,
	// the backend quirks and how to detect a claimed line)
	if res, err := gpioTestPinViaLib(req.Pin); err == nil {
		resp := map[string]interface{}{
			"status":          firstNonEmpty(res["status"], "error"),
			"method":          res["method"],
			"pin":             req.Pin,
			"pin_numbering":   "physical",
			"gpio":            res["gpio"],
			"label":           res["label"],
			"chip":            res["chip"],
			"busy":            firstNonEmpty(res["busy"], "unknown"),
			"busy_consumer":   res["busy_consumer"],
			"board_model":     res["board_model"],
			"board_family":    res["board_family"],
			"pin_map_trusted": res["pin_map_trusted"] != "0",
		}
		if res["warning"] != "" {
			resp["warning"] = res["warning"]
		}

		if res["status"] != "ok" {
			resp["message"] = firstNonEmpty(res["message"], "GPIO test failed")
			if res["busy"] == "yes" {
				logAction(h.DB, "ERROR", "gpio", "GPIO test: "+resp["message"].(string))
			}
			sendJSON(w, http.StatusOK, resp)
			return
		}

		state, _ := strconv.Atoi(res["state"])
		resp["state"] = state
		resp["state_text"], resp["state_color"] = gpioStateText(state)
		resp["pull_up"] = "enabled"
		if res["method"] == "libgpiod" || res["method"] == "sysfs" {
			resp["pull_up"] = "unavailable (external pull-up required)"
		}
		sendJSON(w, http.StatusOK, resp)
		return
	}

	// Fallback: library missing (e.g. running the API standalone)
	state, method, gpioNum, err := readGpioPin(req.Pin)
	if err != nil {
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"status":        "error",
			"pin":           req.Pin,
			"pin_numbering": "physical",
			"gpio":          gpioNum,
			"method":        method,
			"busy":          gpioBusyFlag(err),
			"message":       "GPIO test failed: " + err.Error(),
		})
		return
	}

	stateText, stateColor := gpioStateText(state)
	sendJSON(w, http.StatusOK, map[string]interface{}{
		"status":        "ok",
		"method":        method,
		"pin":           req.Pin,
		"pin_numbering": "physical",
		"gpio":          gpioNum,
		"state":         state,
		"state_text":    stateText,
		"state_color":   stateColor,
		"busy":          "unknown",
		"pull_up":       "enabled",
	})
}

// Coin records a coin event from the GPIO listener
func (h *GPIOHandler) Coin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req models.CoinEventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid request body"})
		return
	}

	if req.CoinValue <= 0 {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid coin value"})
		return
	}

	// Insert coin event
	_, err := h.DB.Exec(`
		INSERT INTO coin_events (coin_value, detected_at, processed)
		VALUES ($1, NOW(), false)
	`, req.CoinValue)

	if err != nil {
		log.Printf("Error recording coin event: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to record coin event"})
		return
	}

	// Resolve minutes from the pricing table only (1 peso = 1 pulse).
	// A blank/incomplete pricing table credits nothing — the operator must
	// add the tier in the admin panel.
	pricingMatch, err := MinutesForAmount(h.DB, req.CoinValue)
	if err != nil {
		log.Printf("Error resolving pricing for P%d: %v", req.CoinValue, err)
	}
	minutes := pricingMatch.Minutes

	if minutes == 0 {
		logAction(h.DB, "ERROR", "gpio", "Coin detected: P"+strconv.Itoa(req.CoinValue)+" but NO pricing tier is configured for it — no time credited")
		sendJSON(w, http.StatusOK, models.APIResponse{
			Success: false,
			Message: "Coin recorded but no pricing tier is configured for P" + strconv.Itoa(req.CoinValue),
			Data: map[string]interface{}{
				"coin_value": req.CoinValue,
				"minutes":    0,
				"seconds":    0,
			},
		})
		return
	}

	logAction(h.DB, "INFO", "gpio", "Coin detected: P"+strconv.Itoa(req.CoinValue)+" = "+strconv.Itoa(minutes)+" min")

	sendJSON(w, http.StatusOK, models.APIResponse{
		Success: true,
		Message: "Coin event recorded",
		Data: map[string]interface{}{
			"coin_value": req.CoinValue,
			"minutes":    minutes,
			"seconds":    minutes * 60,
		},
	})
}

const gpioLibPath = "/usr/local/bin/aircoins-gpio-lib"

// orangePiH3PinToGPIO maps PHYSICAL header pins of the 40-pin Allwinner
// H3/H5 Orange Pi header (PC / PC Plus / One / Lite / Plus 2E) to the
// sunxi GPIO number (bank*32 + line, PA=0 PC=64 PD=96 PG=192). That
// number is both the sysfs GPIO number and the gpiochip0 line number.
//
// Physical pin 3 = PA12 = GPIO 12 — NOT GPIO 3 (which is PA3, physical
// pin 15). Feeding the header pin number straight to gpioget/sysfs is
// what made the traditional PisoWifi pin-3 wiring look dead.
var orangePiH3PinToGPIO = map[int]int{
	3: 12, 5: 11, 7: 6, 8: 13, 10: 14, 11: 1, 12: 110, 13: 0,
	15: 3, 16: 68, 18: 71, 19: 64, 21: 65, 22: 2, 23: 66, 24: 67,
	26: 21, 27: 19, 28: 18, 29: 7, 31: 8, 32: 200, 33: 9, 35: 10,
	36: 201, 37: 20, 38: 198, 40: 199,
}

// gpioTestPinViaLib runs gpio_test_pin from the shared shell library and
// returns its key=value output as a map.
func gpioTestPinViaLib(pin int) (map[string]string, error) {
	if _, err := os.Stat(gpioLibPath); err != nil {
		return nil, err
	}
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		return nil, err
	}

	// gpio_test_pin exits non-zero on a failed/busy pin but still prints
	// the detail lines, so parse the output before trusting the exit code.
	out, runErr := exec.Command(bashPath, "-c",
		"source "+gpioLibPath+" >/dev/null 2>&1; gpio_test_pin "+strconv.Itoa(pin)).Output()

	res := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		key, value, found := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if found && key != "" {
			res[key] = value
		}
	}
	if len(res) == 0 {
		if runErr != nil {
			return nil, runErr
		}
		return nil, os.ErrInvalid
	}
	return res, nil
}

// readGpioPin reads the state of a PHYSICAL header pin, trying multiple
// methods. It returns the state, the method used and the kernel GPIO
// number the physical pin resolved to.
func readGpioPin(pin int) (int, string, int, error) {
	pinStr := strconv.Itoa(pin)

	gpioNum, ok := orangePiH3PinToGPIO[pin]
	if !ok {
		return 1, "none", 0, errors.New("physical pin " + pinStr +
			" is not a usable GPIO on the 40-pin Orange Pi header")
	}
	gpioNumStr := strconv.Itoa(gpioNum)

	// Method 1: WiringOP (gpio command) — Orange Pi.
	// "-1" selects PHYSICAL header numbering, so pass the pin as given.
	gpioPath, err := exec.LookPath("gpio")
	if err == nil {
		exec.Command(gpioPath, "-1", "mode", pinStr, "in").Run()
		exec.Command(gpioPath, "-1", "mode", pinStr, "up").Run()

		out, err := exec.Command(gpioPath, "-1", "read", pinStr).Output()
		if err == nil {
			stateStr := strings.TrimSpace(string(out))
			if state, convErr := strconv.Atoi(stateStr); convErr == nil {
				return state, "wiringop", gpioNum, nil
			}
		}
	}

	// Method 2: raspi-gpio — Raspberry Pi (BCM numbering; this fallback
	// map is Orange Pi only, so keep using the resolved number)
	raspiGpioPath, err := exec.LookPath("raspi-gpio")
	if err == nil {
		out, err := exec.Command(raspiGpioPath, "get", gpioNumStr).Output()
		if err == nil {
			outStr := string(out)
			if strings.Contains(outStr, "level=0") {
				return 0, "raspi-gpio", gpioNum, nil
			} else if strings.Contains(outStr, "level=1") {
				return 1, "raspi-gpio", gpioNum, nil
			}
		}
	}

	// Method 3: libgpiod (gpioget command) — chip line number
	gpiogetPath, err := exec.LookPath("gpioget")
	if err == nil {
		cmd := exec.Command(gpiogetPath, "gpiochip0", gpioNumStr)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err == nil {
			stateStr := strings.TrimSpace(string(out))
			if state, convErr := strconv.Atoi(stateStr); convErr == nil {
				return state, "libgpiod", gpioNum, nil
			}
		}
		if isGpioBusy(stderr.String()) {
			return 1, "libgpiod", gpioNum, errors.New("physical pin " + pinStr +
				" (gpiochip0 line " + gpioNumStr + ") is busy - claimed by another driver" +
				" (i2c/spi/uart overlay?). Disable it or pick another pin")
		}
	}

	// Method 4: sysfs fallback — global (sunxi) GPIO number
	if _, err := os.Stat("/sys/class/gpio"); err == nil {
		// Export if needed
		if _, err := os.Stat("/sys/class/gpio/gpio" + gpioNumStr); os.IsNotExist(err) {
			if werr := os.WriteFile("/sys/class/gpio/export", []byte(gpioNumStr), 0644); werr != nil && isGpioBusy(werr.Error()) {
				return 1, "sysfs", gpioNum, errors.New("physical pin " + pinStr +
					" (GPIO " + gpioNumStr + ") is busy - claimed by another driver" +
					" (i2c/spi/uart overlay?). Disable it or pick another pin")
			}
		}
		os.WriteFile("/sys/class/gpio/gpio"+gpioNumStr+"/direction", []byte("in"), 0644)

		data, err := os.ReadFile("/sys/class/gpio/gpio" + gpioNumStr + "/value")
		if err == nil {
			stateStr := strings.TrimSpace(string(data))
			state, _ := strconv.Atoi(stateStr)
			return state, "sysfs", gpioNum, nil
		}
	}

	return 1, "none", gpioNum, nil // Default HIGH (pull-up)
}

// isGpioBusy reports whether an error message means the GPIO line is
// already claimed (EBUSY), e.g. by an i2c overlay on pins 3/5.
func isGpioBusy(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "busy")
}

func gpioBusyFlag(err error) string {
	if err != nil && isGpioBusy(err.Error()) {
		return "yes"
	}
	return "unknown"
}

func gpioStateText(state int) (string, string) {
	if state == 0 {
		return "LOW (0)", "#ff4444"
	}
	return "HIGH (1)", "#00ff88"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// writeGPIOConfigFile writes config to file for gpio-coin-listener
func writeGPIOConfigFile(pin, coinValue int, pulseMode, boardModel string, debounceMs int, relayEnabled bool, relayPin, relayIntensity int) {
	relayOn := "false"
	if relayEnabled {
		relayOn = "true"
	}

	// PRESERVE the master-toggle state (GPIO_ENABLED). The file is written
	// by BOTH this API handler and the admin panel's set_gpio_config CGI, so
	// a rewrite here must never silently drop the toggle — otherwise a reload
	// of the panel would show "Enable GPIO" flipped back to the stale value.
	// The master toggle lives ONLY in this file (not the DB), so read it back
	// and carry it over; default to true (SBC boards) when it is absent.
	enabledStr := "true"
	if data, rerr := os.ReadFile("/var/lib/pisowifi/gpio_config"); rerr == nil {
		for _, line := range strings.Split(string(data), "\n") {
			tr := strings.TrimSpace(line)
			if strings.HasPrefix(tr, "GPIO_ENABLED=") {
				val := strings.TrimSpace(strings.TrimPrefix(tr, "GPIO_ENABLED="))
				if val == "true" || val == "TRUE" || val == "True" || val == "1" || val == "on" || val == "yes" {
					enabledStr = "true"
				} else {
					enabledStr = "false"
				}
			}
		}
	}

	content := "# AirCoins GPIO Config - Written by API\n"
	content += "# COIN_PULSE_PIN is a PHYSICAL HEADER PIN number (not a GPIO number)\n"
	content += "GPIO_ENABLED=" + enabledStr + "\n"
	content += "COIN_PULSE_PIN=" + strconv.Itoa(pin) + "\n"
	content += "COIN_VALUE=" + strconv.Itoa(coinValue) + "\n"
	content += "PULSE_MODE=\"" + pulseMode + "\"\n"
	content += "BOARD_MODEL=\"" + boardModel + "\"\n"
	content += "DEBOUNCE_MS=" + strconv.Itoa(debounceMs) + "\n"
	content += "# RELAY_PIN is a PHYSICAL HEADER PIN number (not a GPIO number)\n"
	content += "RELAY_ENABLED=" + relayOn + "\n"
	content += "RELAY_PIN=" + strconv.Itoa(relayPin) + "\n"
	content += "RELAY_INTENSITY=" + strconv.Itoa(relayIntensity) + "\n"

	os.MkdirAll("/var/lib/pisowifi", 0755)
	err := os.WriteFile("/var/lib/pisowifi/gpio_config", []byte(content), 0644)
	if err != nil {
		log.Printf("Failed to write GPIO config file: %v", err)
	}
}
