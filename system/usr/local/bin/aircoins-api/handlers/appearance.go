package handlers

import (
	"aircoins-api/models"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// AppearanceHandler manages the captive portal look (theme id, the 7
// colors, optional background image). The whole config is ONE JSON
// document stored in system_settings under key 'portal_appearance'
// (seeded by migration 009); the uploaded background image lives as a
// static file under <web root>/portal-assets/ so lighttpd serves it.
type AppearanceHandler struct {
	DB *sql.DB
}

const appearanceSettingKey = "portal_appearance"
const appearanceSettingDesc = "Captive portal appearance (theme, colors, background image) as a JSON document"

// Device web root served by lighttpd (lighttpd.conf server.document-root).
// Overridable via WEB_ROOT for development machines that have no
// /var/www/html — same env-with-default pattern as main.go's getEnv.
const defaultWebRoot = "/var/www/html"

// portalAssetsDir is the directory (under the web root) holding
// admin-uploaded portal assets; install.sh pre-creates it on the device.
const portalAssetsDir = "portal-assets"

// maxBackgroundBytes caps background uploads at 5 MB.
const maxBackgroundBytes = 5 << 20

func webRoot() string {
	if v := os.Getenv("WEB_ROOT"); v != "" {
		return v
	}
	return defaultWebRoot
}

// Preset theme ids — MUST stay in sync with the PORTAL_THEMES object in
// admin.html / index.html. "custom" means the operator edited colors.
var appearanceThemes = map[string]bool{
	"dark":        true,
	"light":       true,
	"educational": true,
	"gaming":      true,
	"sunset":      true,
	"custom":      true,
}

var hexColorRe = regexp.MustCompile(`^#([0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

// backgroundImageRe strictly limits stored background paths to a single
// safe filename directly under /portal-assets/. No quotes, spaces,
// parentheses or semicolons can appear, so the value can never break out
// of the CSS url(...) the frontends interpolate it into.
var backgroundImageRe = regexp.MustCompile(`^/portal-assets/[A-Za-z0-9._-]+$`)

// Only these image types are accepted; the extension is derived from the
// SNIFFED content type, never from the uploaded filename.
var backgroundExtByMIME = map[string]string{
	"image/jpeg":  "jpg",
	"image/pjpeg": "jpg",
	"image/png":   "png",
	"image/x-png": "png",
	"image/webp":  "webp",
}

// backgroundExts lists every extension a stored background can have.
var backgroundExts = []string{"jpg", "png", "webp"}

// defaultAppearance mirrors the 'dark' preset (the portal's built-in look).
func defaultAppearance() models.PortalAppearance {
	return models.PortalAppearance{
		Theme: "dark",
		Colors: models.PortalColors{
			Primary:    "#1a1a2e",
			Accent:     "#ffd700",
			Background: "#16213e",
			Card:       "#1f2b47",
			Text:       "#eaeaea",
			Button:     "#ffa500",
			ButtonText: "#1a1a2e",
		},
		BackgroundImage: "",
	}
}

// validateAppearance enforces the fixed contract: known theme id, 7 valid
// hex colors, background image empty or a path under /portal-assets/.
func validateAppearance(a *models.PortalAppearance) error {
	if !appearanceThemes[a.Theme] {
		return fmt.Errorf("unknown theme %q", a.Theme)
	}

	colors := map[string]string{
		"primary":     a.Colors.Primary,
		"accent":      a.Colors.Accent,
		"background":  a.Colors.Background,
		"card":        a.Colors.Card,
		"text":        a.Colors.Text,
		"button":      a.Colors.Button,
		"button_text": a.Colors.ButtonText,
	}
	for name, val := range colors {
		if !hexColorRe.MatchString(val) {
			return fmt.Errorf("invalid %s color %q: must be #rgb or #rrggbb hex", name, val)
		}
	}

	if a.BackgroundImage != "" && !backgroundImageRe.MatchString(a.BackgroundImage) {
		return fmt.Errorf("background_image must be empty or a filename under /%s/", portalAssetsDir)
	}

	if a.HeaderImage != "" && !backgroundImageRe.MatchString(a.HeaderImage) {
		return fmt.Errorf("header_image must be empty or a filename under /%s/", portalAssetsDir)
	}

	// redirect_url is optional. When set it must be a well-formed absolute
	// http(s) URL — the portal navigates clients to it after payment, so a
	// malformed or dangerous value (javascript:, data:, etc.) is rejected.
	if a.RedirectURL != "" {
		if len(a.RedirectURL) > 500 {
			return fmt.Errorf("redirect_url too long (max 500 characters)")
		}
		u, err := url.Parse(a.RedirectURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("redirect_url must be an absolute http:// or https:// URL (or empty to disable)")
		}
	}

	return nil
}

// GetAppearance handles GET /api/portal/appearance.
// PUBLIC — the captive portal reads it on every page load, so it must
// never require auth and always answer something usable: any missing or
// unreadable config falls back to the default dark preset.
func (h *AppearanceHandler) GetAppearance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg := defaultAppearance()

	var value string
	var updatedAt sql.NullTime
	err := h.DB.QueryRow(
		"SELECT value, updated_at FROM system_settings WHERE key = $1",
		appearanceSettingKey,
	).Scan(&value, &updatedAt)
	if err == nil {
		var stored models.PortalAppearance
		if json.Unmarshal([]byte(value), &stored) == nil && validateAppearance(&stored) == nil {
			cfg = stored
			if updatedAt.Valid {
				cfg.UpdatedAt = updatedAt.Time.Format(time.RFC3339)
			}
		} else {
			log.Printf("Invalid portal_appearance document in system_settings, serving defaults")
		}
	} else if err != sql.ErrNoRows {
		log.Printf("Error fetching portal appearance: %v", err)
	}

	sendJSON(w, http.StatusOK, models.APIResponse{Success: true, Data: cfg})
}

// SaveAppearance handles POST /api/admin/portal/appearance (admin auth).
// Body is the same shape as the GET data: theme + colors + background_image.
func (h *AppearanceHandler) SaveAppearance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Appearance documents are tiny; reject oversized bodies outright
	// (same pattern as the upload handler's MaxBytesReader).
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)

	var req models.PortalAppearance
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Invalid request body"})
		return
	}

	// background_image is SERVER-OWNED: it only changes through the
	// upload/delete endpoints, which construct the path themselves.
	// Whatever the client sent here is discarded and the stored value
	// preserved, so a crafted path can never be saved via this route.
	stored, err := h.loadAppearance()
	if err != nil {
		log.Printf("Error loading stored appearance: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to load current appearance"})
		return
	}
	req.BackgroundImage = stored.BackgroundImage
	req.HeaderImage = stored.HeaderImage

	if err := validateAppearance(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: err.Error()})
		return
	}

	// The stored document carries no timestamp — the row's updated_at
	// column is authoritative (returned by GET).
	req.UpdatedAt = ""
	doc, err := json.Marshal(req)
	if err != nil {
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to encode appearance"})
		return
	}

	// Upsert relies on the UNIQUE (key) constraint from migration 002.
	_, err = h.DB.Exec(`
		INSERT INTO system_settings (key, value, description, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (key) DO UPDATE SET value = $2, updated_at = NOW()
	`, appearanceSettingKey, string(doc), appearanceSettingDesc)
	if err != nil {
		log.Printf("Error saving portal appearance: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to save appearance"})
		return
	}

	sendJSON(w, http.StatusOK, models.APIResponse{Success: true, Message: "Portal appearance saved"})
}

// Background handles /api/admin/portal/background (admin auth):
// POST uploads a new background image, DELETE removes it.
func (h *AppearanceHandler) Background(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.uploadBackground(w, r)
	case http.MethodDelete:
		h.deleteBackground(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *AppearanceHandler) uploadBackground(w http.ResponseWriter, r *http.Request) {
	// Hard cap the whole request body: 5 MB file + a little headroom for
	// the multipart framing. A 6 MB upload fails right here.
	r.Body = http.MaxBytesReader(w, r.Body, maxBackgroundBytes+64<<10)
	if err := r.ParseMultipartForm(maxBackgroundBytes + 64<<10); err != nil {
		sendJSON(w, http.StatusRequestEntityTooLarge, models.APIResponse{Success: false, Message: "Image too large (max 5 MB)"})
		return
	}
	defer r.MultipartForm.RemoveAll()

	file, header, err := r.FormFile("image")
	if err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Missing 'image' file field"})
		return
	}
	defer file.Close()

	if header.Size > maxBackgroundBytes {
		sendJSON(w, http.StatusRequestEntityTooLarge, models.APIResponse{Success: false, Message: "Image too large (max 5 MB)"})
		return
	}

	// Sniff the real content type from the first bytes — never trust the
	// uploaded file extension (a renamed .exe must be rejected).
	head := make([]byte, 512)
	n, err := file.Read(head)
	if err != nil && err != io.EOF {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Failed to read image"})
		return
	}
	head = head[:n]

	ctype := http.DetectContentType(head)
	ext, ok := backgroundExtByMIME[ctype]
	if !ok || ctype == "application/octet-stream" {
		origExt := strings.ToLower(filepath.Ext(header.Filename))
		switch origExt {
		case ".jpg", ".jpeg":
			ext = "jpg"
			ok = true
		case ".png":
			ext = "png"
			ok = true
		case ".webp":
			ext = "webp"
			ok = true
		}
	}
	if !ok {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{
			Success: false,
			Message: fmt.Sprintf("Unsupported image type %q (detected %q) — use jpg, png or webp", header.Filename, ctype),
		})
		return
	}

	assetsPath := filepath.Join(webRoot(), portalAssetsDir)
	if err := os.MkdirAll(assetsPath, 0755); err != nil {
		log.Printf("Error creating %s: %v", assetsPath, err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to create assets directory"})
		return
	}

	// Write atomically: temp file in the same dir, then rename over the
	// final name so lighttpd never serves a half-written image.
	tmp, err := os.CreateTemp(assetsPath, ".background-*")
	if err != nil {
		log.Printf("Error creating temp background: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to save image"})
		return
	}
	tmpName := tmp.Name()

	_, err = tmp.Write(head)
	if err == nil {
		_, err = io.Copy(tmp, file)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmpName)
		log.Printf("Error writing background image: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to save image"})
		return
	}

	finalName := "background." + ext
	finalPath := filepath.Join(assetsPath, finalName)
	os.Chmod(tmpName, 0644)
	if err := os.Rename(tmpName, finalPath); err != nil {
		os.Remove(tmpName)
		log.Printf("Error renaming background image: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to save image"})
		return
	}

	// Delete any previous background with a DIFFERENT extension so only
	// one background.* file ever exists.
	for _, other := range backgroundExts {
		if other == ext {
			continue
		}
		os.Remove(filepath.Join(assetsPath, "background."+other))
	}

	publicPath := "/" + portalAssetsDir + "/" + finalName
	if err := h.setBackgroundImage(publicPath); err != nil {
		log.Printf("Error updating background_image in config: %v", err)
	}

	sendJSON(w, http.StatusOK, models.APIResponse{
		Success: true,
		Data:    map[string]string{"path": publicPath},
	})
}

func (h *AppearanceHandler) deleteBackground(w http.ResponseWriter, r *http.Request) {
	assetsPath := filepath.Join(webRoot(), portalAssetsDir)
	for _, ext := range backgroundExts {
		os.Remove(filepath.Join(assetsPath, "background."+ext))
	}

	if err := h.setBackgroundImage(""); err != nil {
		log.Printf("Error clearing background_image in config: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to update appearance config"})
		return
	}

	sendJSON(w, http.StatusOK, models.APIResponse{Success: true, Message: "Background removed"})
}

// loadAppearance returns the stored appearance document, falling back to
// the defaults when the row is missing or its content is unreadable.
func (h *AppearanceHandler) loadAppearance() (models.PortalAppearance, error) {
	cfg := defaultAppearance()

	var value string
	err := h.DB.QueryRow(
		"SELECT value FROM system_settings WHERE key = $1",
		appearanceSettingKey,
	).Scan(&value)
	if err == nil {
		var stored models.PortalAppearance
		if json.Unmarshal([]byte(value), &stored) == nil && validateAppearance(&stored) == nil {
			cfg = stored
		}
	} else if err != sql.ErrNoRows {
		return cfg, err
	}

	return cfg, nil
}

// setBackgroundImage rewrites only the background_image field of the
// stored appearance document (keeping theme/colors as saved), creating
// the document from defaults when it doesn't exist yet.
func (h *AppearanceHandler) setBackgroundImage(path string) error {
	cfg, err := h.loadAppearance()
	if err != nil {
		return err
	}

	cfg.BackgroundImage = path
	cfg.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	doc, err := json.Marshal(cfg)
	if err != nil {
		return err
	}

	_, err = h.DB.Exec(`
		INSERT INTO system_settings (key, value, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()
	`, appearanceSettingKey, string(doc))
	return err
}

// HeaderImage handles /api/admin/portal/header-image (admin auth):
// POST uploads a new header image (max 5 MB), DELETE removes it.
func (h *AppearanceHandler) HeaderImage(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.uploadHeaderImage(w, r)
	case http.MethodDelete:
		h.deleteHeaderImage(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *AppearanceHandler) uploadHeaderImage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBackgroundBytes+64<<10)
	if err := r.ParseMultipartForm(maxBackgroundBytes + 64<<10); err != nil {
		sendJSON(w, http.StatusRequestEntityTooLarge, models.APIResponse{Success: false, Message: "Image too large (max 5 MB)"})
		return
	}
	defer r.MultipartForm.RemoveAll()

	file, header, err := r.FormFile("image")
	if err != nil {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Missing 'image' file field"})
		return
	}
	defer file.Close()

	if header.Size > maxBackgroundBytes {
		sendJSON(w, http.StatusRequestEntityTooLarge, models.APIResponse{Success: false, Message: "Image too large (max 5 MB)"})
		return
	}

	head := make([]byte, 512)
	n, err := file.Read(head)
	if err != nil && err != io.EOF {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{Success: false, Message: "Failed to read image"})
		return
	}
	head = head[:n]

	ctype := http.DetectContentType(head)
	ext, ok := backgroundExtByMIME[ctype]
	if !ok || ctype == "application/octet-stream" {
		origExt := strings.ToLower(filepath.Ext(header.Filename))
		switch origExt {
		case ".jpg", ".jpeg":
			ext = "jpg"
			ok = true
		case ".png":
			ext = "png"
			ok = true
		case ".webp":
			ext = "webp"
			ok = true
		}
	}
	if !ok {
		sendJSON(w, http.StatusBadRequest, models.APIResponse{
			Success: false,
			Message: fmt.Sprintf("Unsupported image type %q (detected %q) — use jpg, png or webp", header.Filename, ctype),
		})
		return
	}

	assetsPath := filepath.Join(webRoot(), portalAssetsDir)
	if err := os.MkdirAll(assetsPath, 0755); err != nil {
		log.Printf("Error creating %s: %v", assetsPath, err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to create assets directory"})
		return
	}

	tmp, err := os.CreateTemp(assetsPath, ".header-*")
	if err != nil {
		log.Printf("Error creating temp header image: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to save image"})
		return
	}
	tmpName := tmp.Name()

	_, err = tmp.Write(head)
	if err == nil {
		_, err = io.Copy(tmp, file)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmpName)
		log.Printf("Error writing header image: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to save image"})
		return
	}

	finalName := "header." + ext
	finalPath := filepath.Join(assetsPath, finalName)
	os.Chmod(tmpName, 0644)
	if err := os.Rename(tmpName, finalPath); err != nil {
		os.Remove(tmpName)
		log.Printf("Error renaming header image: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to save image"})
		return
	}

	for _, other := range backgroundExts {
		if other == ext {
			continue
		}
		os.Remove(filepath.Join(assetsPath, "header."+other))
	}

	publicPath := "/" + portalAssetsDir + "/" + finalName
	if err := h.setHeaderImage(publicPath); err != nil {
		log.Printf("Error updating header_image in config: %v", err)
	}

	sendJSON(w, http.StatusOK, models.APIResponse{
		Success: true,
		Data:    map[string]string{"path": publicPath},
	})
}

func (h *AppearanceHandler) deleteHeaderImage(w http.ResponseWriter, r *http.Request) {
	assetsPath := filepath.Join(webRoot(), portalAssetsDir)
	for _, ext := range backgroundExts {
		os.Remove(filepath.Join(assetsPath, "header."+ext))
	}

	if err := h.setHeaderImage(""); err != nil {
		log.Printf("Error clearing header_image in config: %v", err)
		sendJSON(w, http.StatusInternalServerError, models.APIResponse{Success: false, Message: "Failed to update appearance config"})
		return
	}

	sendJSON(w, http.StatusOK, models.APIResponse{Success: true, Message: "Header image removed"})
}

func (h *AppearanceHandler) setHeaderImage(path string) error {
	cfg, err := h.loadAppearance()
	if err != nil {
		return err
	}

	cfg.HeaderImage = path
	cfg.UpdatedAt = ""
	doc, err := json.Marshal(cfg)
	if err != nil {
		return err
	}

	_, err = h.DB.Exec(`
		INSERT INTO system_settings (key, value, description, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (key) DO UPDATE SET value = $2, updated_at = NOW()
	`, appearanceSettingKey, string(doc), appearanceSettingDesc)
	return err
}
