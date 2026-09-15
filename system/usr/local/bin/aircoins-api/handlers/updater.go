package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// UpdaterHandler caches Supabase manifest lookups to avoid hammering the API.
type UpdaterHandler struct {
	mu       sync.RWMutex
	cachedAt time.Time
	cached   *updateManifest
	client   *http.Client
}

type updateManifest struct {
	Version      string          `json:"version"`
	ReleasedAt   string          `json:"released_at"`
	ReleaseNotes string          `json:"release_notes"`
	Tarball      string          `json:"tarball"`
	SHA256       string          `json:"sha256"`
	Versions     []manifestEntry `json:"versions"`
}

type manifestEntry struct {
	Version      string `json:"version"`
	ReleasedAt   string `json:"released_at"`
	ReleaseNotes string `json:"release_notes"`
	Tarball      string `json:"tarball"`
	SHA256       string `json:"sha256"`
}

// NewUpdaterHandler creates an UpdaterHandler with a 10-second HTTP client.
func NewUpdaterHandler() *UpdaterHandler {
	return &UpdaterHandler{
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// fetchLatest returns the latest update manifest from Supabase Storage,
// using the cache when possible. If force is true the cache is bypassed.
func (h *UpdaterHandler) fetchLatest(force bool) (*updateManifest, error) {
	// Check cache (unless forced refresh)
	h.mu.RLock()
	if !force && h.cached != nil && time.Since(h.cachedAt) < 1*time.Hour {
		manifest := *h.cached
		h.mu.RUnlock()
		return &manifest, nil
	}
	h.mu.RUnlock()

	supabaseURL := strings.TrimRight(os.Getenv("SUPABASE_URL"), "/")
	if supabaseURL == "" {
		return nil, fmt.Errorf("SUPABASE_URL not set")
	}

	manifestURL := fmt.Sprintf("%s/storage/v1/object/public/aircoins/manifest.json", supabaseURL)
	log.Printf("[updater] fetching manifest: %s", manifestURL)

	// Use a 5-second timeout client for manifest fetch
	manifestClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := manifestClient.Get(manifestURL)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		log.Printf("[updater] manifest fetch failed: HTTP %d from %s: %s", resp.StatusCode, manifestURL, string(body))
		return nil, fmt.Errorf("HTTP %d fetching manifest: %s", resp.StatusCode, string(body))
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read manifest body: %w", err)
	}
	// Strip UTF-8 BOM if present (Windows PowerShell adds it)
	if len(bodyBytes) >= 3 && bodyBytes[0] == 0xEF && bodyBytes[1] == 0xBB && bodyBytes[2] == 0xBF {
		bodyBytes = bodyBytes[3:]
		log.Printf("[updater] stripped UTF-8 BOM from manifest")
	}
	var manifest updateManifest
	if err := json.Unmarshal(bodyBytes, &manifest); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}

	// Update cache
	h.mu.Lock()
	h.cached = &manifest
	h.cachedAt = time.Now()
	h.mu.Unlock()

	return &manifest, nil
}

// CheckForUpdate queries the latest Supabase manifest and compares versions.
// Returns all available versions for the version cards UI.
func (h *UpdaterHandler) CheckForUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Always force fetch when the check update button is pressed
	force := true
	if r.URL.Query().Get("force") == "false" {
		force = false
	}

	manifest, err := h.fetchLatest(force)
	if err != nil {
		sendJSON(w, 200, map[string]interface{}{
			"current_version":  apiVersion,
			"update_available": false,
			"error":            fmt.Sprintf("Unable to check for updates: %v", err),
			"versions":         []manifestEntry{},
		})
		return
	}

	updateAvailable := compareSemver(apiVersion, manifest.Version) < 0

	// Build versions list: prefer the versions array from manifest,
	// fall back to just the latest entry if versions array is empty.
	versions := manifest.Versions
	if len(versions) == 0 && manifest.Version != "" {
		versions = []manifestEntry{{
			Version:      manifest.Version,
			ReleasedAt:   manifest.ReleasedAt,
			ReleaseNotes: manifest.ReleaseNotes,
			Tarball:      manifest.Tarball,
			SHA256:       manifest.SHA256,
		}}
	}

	sendJSON(w, 200, map[string]interface{}{
		"current_version":  apiVersion,
		"latest_version":   manifest.Version,
		"update_available": updateAvailable,
		"release_notes":    manifest.ReleaseNotes,
		"published_at":     manifest.ReleasedAt,
		"checked_at":       time.Now().UTC().Format(time.RFC3339),
		"versions":         versions,
	})
}

// DownloadUpdate fetches the manifest, downloads the tarball from Supabase
// Storage, verifies its SHA256, and saves it to /opt/aircoins/updates/.
// Accepts an optional "version" field in the JSON body to download a specific
// version. If omitted, downloads the latest version.
func (h *UpdaterHandler) DownloadUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse optional version from request body
	var reqBody struct {
		Version string `json:"version"`
	}
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&reqBody)
	}

	// Get manifest for tarball URL
	manifest, err := h.fetchLatest(false)
	if err != nil {
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"success": false, "message": "Cannot fetch manifest: " + err.Error(),
		})
		return
	}

	// Find the target version entry
	var targetEntry *manifestEntry
	if reqBody.Version != "" {
		// Look for the specific version in the versions array
		targetVersion := strings.TrimPrefix(reqBody.Version, "v")
		for i := range manifest.Versions {
			v := strings.TrimPrefix(manifest.Versions[i].Version, "v")
			if v == targetVersion {
				targetEntry = &manifest.Versions[i]
				break
			}
		}
		if targetEntry == nil {
			sendJSON(w, http.StatusOK, map[string]interface{}{
				"success": false, "message": "Version " + reqBody.Version + " not found in manifest",
			})
			return
		}
	} else {
		// Use the latest version
		targetEntry = &manifestEntry{
			Version:      manifest.Version,
			ReleasedAt:   manifest.ReleasedAt,
			ReleaseNotes: manifest.ReleaseNotes,
			Tarball:      manifest.Tarball,
			SHA256:       manifest.SHA256,
		}
	}

	// Create download directory
	os.MkdirAll("/opt/aircoins/updates", 0755)

	// Construct full download URL
	supabaseURL := strings.TrimRight(os.Getenv("SUPABASE_URL"), "/")
	if supabaseURL == "" {
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"success": false, "message": "SUPABASE_URL is not set on this device.",
		})
		return
	}
	tarball := strings.TrimLeft(targetEntry.Tarball, "/")
	downloadURL := fmt.Sprintf("%s/storage/v1/object/public/aircoins/%s", supabaseURL, tarball)
	log.Printf("[updater] downloading tarball: %s (version %s)", downloadURL, targetEntry.Version)

	// Use a separate client with 5-minute timeout for large downloads
	dlClient := &http.Client{Timeout: 5 * time.Minute}
	resp, err := dlClient.Get(downloadURL)
	if err != nil {
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"success": false, "message": "Download failed: " + err.Error(),
		})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		log.Printf("[updater] download failed: HTTP %d from %s: %s", resp.StatusCode, downloadURL, string(body))
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"success": false, "message": fmt.Sprintf("Download returned status %d: %s", resp.StatusCode, string(body)),
		})
		return
	}

	filename := fmt.Sprintf("aircoins-v%s.tar.gz", strings.TrimPrefix(targetEntry.Version, "v"))
	filePath := "/opt/aircoins/updates/" + filename

	outFile, err := os.Create(filePath)
	if err != nil {
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"success": false, "message": "Cannot create file: " + err.Error(),
		})
		return
	}
	defer outFile.Close()

	// Stream download to file
	if _, err := io.Copy(outFile, resp.Body); err != nil {
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"success": false, "message": "Failed writing file: " + err.Error(),
		})
		return
	}

	// Verify SHA256 after download
	downloadedFile, err := os.ReadFile(filePath)
	if err != nil {
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"success": false, "message": "Cannot read downloaded file for verification: " + err.Error(),
		})
		return
	}
	hash := sha256.Sum256(downloadedFile)
	actualSHA := hex.EncodeToString(hash[:])

	if targetEntry.SHA256 != "" && !strings.EqualFold(actualSHA, targetEntry.SHA256) {
		os.Remove(filePath)
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("SHA256 mismatch: expected %s, got %s", targetEntry.SHA256, actualSHA),
		})
		return
	}

	// Return success JSON
	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success":  true,
		"message":  "Download complete",
		"version":  targetEntry.Version,
		"filename": filename,
	})
}

// CheckDownloadedFile checks if downloaded update tarballs exist in
// /opt/aircoins/updates/ and returns their metadata.
func (h *UpdaterHandler) CheckDownloadedFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	updateDir := "/opt/aircoins/updates"
	entries, err := os.ReadDir(updateDir)
	if err != nil {
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"found":    false,
			"versions": []interface{}{},
		})
		return
	}

	var downloaded []map[string]interface{}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "aircoins-v") && strings.HasSuffix(e.Name(), ".tar.gz") {
			info, _ := e.Info()
			// Extract version from filename: "aircoins-v1.8.0.tar.gz" -> "v1.8.0"
			version := strings.TrimPrefix(strings.TrimSuffix(e.Name(), ".tar.gz"), "aircoins-")
			downloaded = append(downloaded, map[string]interface{}{
				"filename": e.Name(),
				"version":  version,
				"size":     info.Size(),
			})
		}
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"found":    len(downloaded) > 0,
		"versions": downloaded,
	})
}

// PerformUpdate installs from a previously downloaded local tarball file.
// Accepts an optional "version" field in the JSON body to install a specific
// version. If omitted, installs whatever tarball is found in /opt/aircoins/updates/.
func (h *UpdaterHandler) PerformUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse optional version from request body
	var reqBody struct {
		Version string `json:"version"`
	}
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&reqBody)
	}

	// Find the downloaded tarball in /opt/aircoins/updates/
	updateDir := "/opt/aircoins/updates"
	entries, _ := os.ReadDir(updateDir)
	var tarballPath string
	targetName := ""
	if reqBody.Version != "" {
		// Look for the specific version tarball
		targetName = fmt.Sprintf("aircoins-v%s.tar.gz", strings.TrimPrefix(reqBody.Version, "v"))
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "aircoins-v") || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		if targetName != "" {
			if e.Name() == targetName {
				tarballPath = updateDir + "/" + e.Name()
				break
			}
		} else {
			tarballPath = updateDir + "/" + e.Name()
			break
		}
	}
	if tarballPath == "" {
		msg := "No downloaded update file found. Please download first."
		if targetName != "" {
			msg = fmt.Sprintf("Version %s tarball not found. Please download it first.", reqBody.Version)
		}
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"success": false, "message": msg,
		})
		return
	}

	// Extract to a staging directory under /opt/aircoins — NOT /tmp, because the
	// service runs with PrivateTmp=true and the detached updater below runs
	// outside our mount namespace, where our private /tmp does not exist.
	tmpDir := "/opt/aircoins/updates/staging"
	os.RemoveAll(tmpDir)
	extractDir := tmpDir + "/extracted"
	os.MkdirAll(extractDir, 0755)
	extractCmd := exec.Command("tar", "-xzf", tarballPath, "-C", extractDir)
	if output, err := extractCmd.CombinedOutput(); err != nil {
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"success": false, "message": "Extract failed: " + string(output),
		})
		return
	}

	// Find extracted directory
	dirEntries, _ := os.ReadDir(extractDir)
	var installDir string
	for _, e := range dirEntries {
		if e.IsDir() {
			installDir = extractDir + "/" + e.Name()
			break
		}
	}
	if installDir == "" {
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"success": false, "message": "No directory found in extracted tarball",
		})
		return
	}

	// Send response BEFORE install (install kills the server)
	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "Installing update...",
	})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Clear cache so next check fetches fresh
	h.mu.Lock()
	h.cachedAt = time.Time{}
	h.cached = nil
	h.mu.Unlock()

	// Targeted file replacement — does NOT run install.sh (which destroys the system).
	// Only copies binary, HTML, scripts, CGI, recovery, and changelog.
	// Preserves: .env, database, systemd units, network config.
	// The script redirects its own output, so the launcher below needs no pipes.
	const updateLogPath = "/tmp/aircoins-update.log"
	updateScript := fmt.Sprintf(`#!/bin/bash
exec >> %s 2>&1
echo "=== AirCoins Update: $(date) ==="
DIR="%s"
# Give the HTTP response time to reach the browser before we kill the API.
sleep 2
sudo systemctl stop aircoins-api 2>/dev/null
sleep 1
# Update binary
if [ -f "$DIR/system/usr/local/bin/aircoins-api/aircoins-api" ]; then
    sudo cp "$DIR/system/usr/local/bin/aircoins-api/aircoins-api" /usr/local/bin/aircoins-api/aircoins-api
    sudo chmod +x /usr/local/bin/aircoins-api/aircoins-api
    echo "Binary updated"
fi
# Update HTML
[ -f "$DIR/admin.html" ] && sudo cp "$DIR/admin.html" /var/www/html/admin.html
[ -f "$DIR/index.html" ] && sudo cp "$DIR/index.html" /var/www/html/index.html
echo "HTML updated"
# Update scripts (aircoins-gpio-lib MUST be here: gpio-coin-listener
# sources it for the edge-stream helpers; without it the listener falls
# back to lossy polling after an update. aircoins-captive-rules MUST be
# here too: it carries the per-MAC captive release rules).
for s in gpio-coin-listener aircoins-gpio-lib aircoins-captive-rules pisowifi-api-update pisowifi-ctl pisowifi-session-manager; do
    [ -f "$DIR/system/usr/local/bin/$s" ] && sudo cp "$DIR/system/usr/local/bin/$s" /usr/local/bin/$s && sudo chmod +x /usr/local/bin/$s
done
sudo sed -i -e 's/\r$//' /usr/local/bin/aircoins-* /usr/local/bin/gpio-coin-listener /usr/local/bin/pisowifi-* 2>/dev/null || true
echo "Scripts updated and normalized"
# Restart the coin listener so the new script+lib actually load into
# memory. bash daemons keep executing the OLD code they read at startup
# until restarted, so without this step every listener fix shipped via
# OTA stays dead until the device is rebooted.
sudo systemctl restart gpio-coin-listener 2>/dev/null || true
echo "gpio-coin-listener restarted"
# Update recovery
[ -f "$DIR/aircoins-recover.sh" ] && sudo cp "$DIR/aircoins-recover.sh" /opt/aircoins/aircoins-recover.sh && sudo chmod +x /opt/aircoins/aircoins-recover.sh
# Update CGI
[ -d "$DIR/system/usr/lib/cgi-bin" ] && sudo cp "$DIR/system/usr/lib/cgi-bin/"* /usr/lib/cgi-bin/ 2>/dev/null && sudo chmod +x /usr/lib/cgi-bin/* 2>/dev/null
# SELF-HEAL lighttpd CGI alias: the admin CGIs live in /usr/lib/cgi-bin,
# which is NOT under server.document-root. Without aliasing /cgi-bin/ to
# that directory, lighttpd 404s under the docroot and error-handler-404
# serves index.html (HTTP 200) — so the GPIO toggle read/write silently
# returned portal HTML and the toggle always showed OFF. Idempotent:
# only applied when the alias line is missing, with backup + revert if
# the patched config does not validate.
if [ -f /etc/lighttpd/lighttpd.conf ] && ! grep -q 'alias.url' /etc/lighttpd/lighttpd.conf; then
    sudo cp /etc/lighttpd/lighttpd.conf /etc/lighttpd/lighttpd.conf.bak-aircoins
    if ! grep -q '"mod_alias"' /etc/lighttpd/lighttpd.conf; then
        sudo sed -i '/^server.modules = (/a\    "mod_alias",' /etc/lighttpd/lighttpd.conf
    fi
    sudo tee -a /etc/lighttpd/lighttpd.conf > /dev/null << 'LITEOF'

# /cgi-bin/ must alias to the physical scripts dir, otherwise the admin
# CGIs 404 into error-handler-404 (index.html) and silently never run.
$HTTP["url"] =~ "^/cgi-bin/" {
    alias.url += ( "/cgi-bin/" => "/usr/lib/cgi-bin/" )
    cgi.assign = ( "" => "/bin/bash" )
}
LITEOF
    if sudo lighttpd -t -f /etc/lighttpd/lighttpd.conf > /dev/null 2>&1; then
        echo "lighttpd /cgi-bin/ alias added (CGI self-heal)"
    else
        echo "lighttpd config test FAILED after alias patch — reverting"
        sudo cp /etc/lighttpd/lighttpd.conf.bak-aircoins /etc/lighttpd/lighttpd.conf
    fi
fi
# Ensure /var/log/lighttpd permissions
[ -f "$DIR/system/etc/tmpfiles.d/lighttpd.conf" ] && sudo cp "$DIR/system/etc/tmpfiles.d/lighttpd.conf" /etc/tmpfiles.d/lighttpd.conf && sudo systemd-tmpfiles --create /etc/tmpfiles.d/lighttpd.conf 2>/dev/null || true
# Update CHANGELOG
[ -f "$DIR/CHANGELOG.md" ] && sudo cp "$DIR/CHANGELOG.md" /opt/aircoins/CHANGELOG.md
# Restart services with retry — a single start can lose a race with the stop
for i in 1 2 3; do
    sudo systemctl start aircoins-api && break
    sleep 2
done
sleep 2
sudo systemctl restart lighttpd
sudo systemctl restart dnsmasq
(sudo systemctl restart hostapd 2>/dev/null || true)
sleep 2
sudo systemctl status aircoins-api --no-pager
sudo systemctl status lighttpd --no-pager
# Clean up downloaded tarball and staging files
rm -f /opt/aircoins/updates/aircoins-v*.tar.gz
rm -rf %s
echo "=== Update Complete: $(date) ==="
`, updateLogPath, installDir, tmpDir)

	// The script must outlive us: its first real action is `systemctl stop
	// aircoins-api`, which SIGTERMs every process in this unit's cgroup.
	scriptPath := "/opt/aircoins/updates/do-update.sh"
	if err := os.WriteFile(scriptPath, []byte(updateScript), 0755); err != nil {
		log.Printf("updater: cannot write %s: %v", scriptPath, err)
		return
	}

	// systemd-run puts the script in its own transient unit, so it lives outside
	// our cgroup and survives the stop. setsid (new session, new process group)
	// is the fallback when systemd-run is unavailable.
	var updateCmd *exec.Cmd
	if runner, err := exec.LookPath("systemd-run"); err == nil {
		updateCmd = exec.Command(runner, "--unit=aircoins-update", "--collect",
			"--no-block", "/bin/bash", scriptPath)
	} else {
		updateCmd = exec.Command("setsid", "bash", scriptPath)
	}
	// Start but don't wait — this process is about to be stopped.
	if err := updateCmd.Start(); err != nil {
		log.Printf("updater: cannot launch %s: %v", scriptPath, err)
	}
}

// DeleteDownload removes downloaded update tarballs from /opt/aircoins/updates/.
func (h *UpdaterHandler) DeleteDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	updateDir := "/opt/aircoins/updates"
	entries, _ := os.ReadDir(updateDir)
	deleted := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "aircoins-v") && strings.HasSuffix(e.Name(), ".tar.gz") {
			os.Remove(updateDir + "/" + e.Name())
			deleted++
		}
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("Deleted %d file(s)", deleted),
	})
}

// compareSemver compares two semantic version strings.
// Returns -1 if a < b, 0 if equal, +1 if a > b.
// "dev" is always considered less than any version.
func compareSemver(a, b string) int {
	if a == "dev" && b == "dev" {
		return 0
	}
	if a == "dev" {
		return -1
	}
	if b == "dev" {
		return 1
	}

	a = strings.TrimLeft(a, "vV")
	b = strings.TrimLeft(b, "vV")

	aParts := strings.SplitN(a, ".", 3)
	bParts := strings.SplitN(b, ".", 3)

	if len(aParts) < 3 || len(bParts) < 3 {
		return 0
	}

	for i := 0; i < 3; i++ {
		// Strip pre-release suffix (e.g., "0-beta" → "0")
		if idx := strings.IndexAny(aParts[i], "-+"); idx >= 0 {
			aParts[i] = aParts[i][:idx]
		}
		if idx := strings.IndexAny(bParts[i], "-+"); idx >= 0 {
			bParts[i] = bParts[i][:idx]
		}
		av, err := strconv.Atoi(aParts[i])
		if err != nil {
			return 0
		}
		bv, err := strconv.Atoi(bParts[i])
		if err != nil {
			return 0
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return 0
}
