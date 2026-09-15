package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AudioHandler manages upload/delete/serve for the three captive-portal
// audio slots: insertcoin, coindrop, donepaying. Files live on disk at
// <audioDir>/<slot>.<ext>; metadata lives in system_settings key
// 'portal_audio' as a JSON blob.
type AudioHandler struct {
	DB *sql.DB
}

const audioSettingKey = "portal_audio"
const audioSettingDesc = "Captive portal audio slots (insertcoin, coindrop, donepaying) metadata as JSON"

// audioDir returns the directory holding uploaded audio files.
// Override with AUDIO_DIR env var for local development.
func audioDir() string {
	if v := os.Getenv("AUDIO_DIR"); v != "" {
		return v
	}
	return "/var/lib/pisowifi/audio"
}

// maxAudioBytes caps audio uploads at 5 MB.
const maxAudioBytes = 5 << 20

// validAudioSlots lists the accepted slot names (case-insensitive on input).
var validAudioSlots = map[string]bool{
	"insertcoin": true,
	"coindrop":   true,
	"donepaying": true,
}

// audioExtByMIME maps accepted audio content types to file extensions.
var audioExtByMIME = map[string]string{
	"audio/mpeg":   "mp3",
	"audio/wav":    "wav",
	"audio/x-wav":  "wav",
	"audio/ogg":    "ogg",
	"audio/mp4":    "m4a",
	"audio/x-m4a":  "m4a",
	"audio/aac":    "m4a",
	"audio/webm":   "webm",
	"audio/flac":   "flac",
	"audio/x-flac": "flac",
}

// audioExts lists every extension a stored audio file can have (used
// when deleting a previous upload with a different extension).
var audioExts = []string{"mp3", "wav", "ogg", "m4a", "webm", "flac", "bin"}

// AudioSlotMeta is the per-slot metadata stored in system_settings.
type AudioSlotMeta struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	UploadedAt  string `json:"uploaded_at"`
}

// portalAudioDoc is the full JSON document stored under portal_audio.
type portalAudioDoc map[string]*AudioSlotMeta

// EnsureAudioDir creates the audio storage directory if it doesn't exist.
// Called once at startup from main.go.
func EnsureAudioDir() error {
	dir := audioDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create audio directory %s: %w", dir, err)
	}
	log.Printf("Audio directory ready: %s", dir)
	return nil
}

// AdminDispatch routes POST /api/admin/audio/<slot> and DELETE
// /api/admin/audio/<slot> to the right handler based on method.
func (h *AudioHandler) AdminDispatch(w http.ResponseWriter, r *http.Request) {
	// Extract slot name from path: /api/admin/audio/<slot>
	path := strings.TrimPrefix(r.URL.Path, "/api/admin/audio/")
	if path == "" || strings.Contains(path, "/") {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false, "message": "invalid audio slot path",
		})
		return
	}
	slot := strings.ToLower(path)
	if !validAudioSlots[slot] {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false, "message": fmt.Sprintf("unknown audio slot %q (valid: insertcoin, coindrop, donepaying)", path),
		})
		return
	}

	switch r.Method {
	case http.MethodPost:
		h.upload(w, r, slot)
	case http.MethodDelete:
		h.delete(w, r, slot)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// AdminList handles GET /api/admin/audio — returns the full portal_audio
// metadata document.
func (h *AudioHandler) AdminList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	doc, err := h.loadAudioDoc()
	if err != nil {
		log.Printf("Error loading audio metadata: %v", err)
		sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false, "message": "failed to load audio metadata",
		})
		return
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success": true, "data": doc,
	})
}

// upload handles POST /api/admin/audio/<slot> — multipart file upload.
func (h *AudioHandler) upload(w http.ResponseWriter, r *http.Request, slot string) {
	// Hard cap: 5 MB file + a little headroom for multipart framing.
	r.Body = http.MaxBytesReader(w, r.Body, maxAudioBytes+64<<10)
	if err := r.ParseMultipartForm(maxAudioBytes + 64<<10); err != nil {
		sendJSON(w, http.StatusRequestEntityTooLarge, map[string]interface{}{
			"success": false, "message": "file too large (max 5 MB)",
		})
		return
	}
	defer r.MultipartForm.RemoveAll()

	file, header, err := r.FormFile("file")
	if err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false, "message": "missing 'file' field in multipart form",
		})
		return
	}
	defer file.Close()

	if header.Size > maxAudioBytes {
		sendJSON(w, http.StatusRequestEntityTooLarge, map[string]interface{}{
			"success": false, "message": "file too large (max 5 MB)",
		})
		return
	}

	// Determine content type: prefer the browser-supplied type when it
	// starts with audio/; otherwise sniff the first 512 bytes.
	contentType := header.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "audio/") && contentType != "application/octet-stream" {
		// Try sniffing
		head := make([]byte, 512)
		n, _ := file.Read(head)
		contentType = http.DetectContentType(head[:n])
		// Seek back so we can still copy from the start
		if seeker, ok := file.(io.Seeker); ok {
			seeker.Seek(0, io.SeekStart)
		} else {
			// Cannot seek — re-open via FormFile again is not possible;
			// just prepend the head bytes.
			sendJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false, "message": fmt.Sprintf("unsupported audio content type %q", contentType),
			})
			return
		}
	}

	// Derive extension from content type
	ext := extFromContentType(contentType, header.Filename)

	dir := audioDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("Error creating audio dir: %v", err)
		sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false, "message": "failed to create audio directory",
		})
		return
	}

	// Delete any existing file for this slot (all possible extensions)
	h.removeSlotFiles(dir, slot)

	// Write atomically: temp file then rename
	tmp, err := os.CreateTemp(dir, ".audio-"+slot+"-*")
	if err != nil {
		log.Printf("Error creating temp audio file: %v", err)
		sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false, "message": "failed to save audio file",
		})
		return
	}
	tmpName := tmp.Name()

	written, err := io.Copy(tmp, file)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmpName)
		log.Printf("Error writing audio file: %v", err)
		sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false, "message": "failed to write audio file",
		})
		return
	}

	finalName := slot + "." + ext
	finalPath := filepath.Join(dir, finalName)
	os.Chmod(tmpName, 0644)
	if err := os.Rename(tmpName, finalPath); err != nil {
		os.Remove(tmpName)
		log.Printf("Error renaming audio file: %v", err)
		sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false, "message": "failed to save audio file",
		})
		return
	}

	uploadedAt := time.Now().UTC().Format(time.RFC3339)

	// Update metadata
	doc, err := h.loadAudioDoc()
	if err != nil {
		log.Printf("Error loading audio doc (will create fresh): %v", err)
		doc = make(portalAudioDoc)
	}
	doc[slot] = &AudioSlotMeta{
		Filename:    finalName,
		ContentType: contentType,
		Size:        written,
		UploadedAt:  uploadedAt,
	}
	if err := h.saveAudioDoc(doc); err != nil {
		log.Printf("Error saving audio metadata: %v", err)
		sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false, "message": "failed to save audio metadata",
		})
		return
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"data": map[string]interface{}{
			"slot":         slot,
			"filename":     finalName,
			"content_type": contentType,
			"size":         written,
			"uploaded_at":  uploadedAt,
			"url":          "/audio/" + slot,
		},
	})
}

// delete handles DELETE /api/admin/audio/<slot>.
func (h *AudioHandler) delete(w http.ResponseWriter, r *http.Request, slot string) {
	dir := audioDir()
	h.removeSlotFiles(dir, slot)

	doc, err := h.loadAudioDoc()
	if err != nil {
		log.Printf("Error loading audio doc for delete: %v", err)
		doc = make(portalAudioDoc)
	}
	delete(doc, slot)
	if err := h.saveAudioDoc(doc); err != nil {
		log.Printf("Error saving audio metadata after delete: %v", err)
		sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false, "message": "failed to update audio metadata",
		})
		return
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success": true, "deleted": true, "slot": slot,
	})
}

// Serve handles GET /audio/<slot> (PUBLIC — no auth).
// Streams the audio file with proper Content-Type, Accept-Ranges, and
// Cache-Control headers. Supports HTTP Range requests for seeking.
func (h *AudioHandler) Serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract slot from path: /audio/<slot>
	path := strings.TrimPrefix(r.URL.Path, "/audio/")
	if path == "" || strings.Contains(path, "/") {
		sendJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false, "message": "audio not uploaded",
		})
		return
	}
	slot := strings.ToLower(path)
	if !validAudioSlots[slot] {
		sendJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false, "message": "audio not uploaded",
		})
		return
	}

	// Look up metadata to find the content type and confirm the file exists
	doc, err := h.loadAudioDoc()
	if err != nil {
		log.Printf("Error loading audio doc for serve: %v", err)
	}
	meta := doc[slot]
	if meta == nil {
		sendJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false, "message": "audio not uploaded",
		})
		return
	}

	filePath := filepath.Join(audioDir(), meta.Filename)
	f, err := os.Open(filePath)
	if err != nil {
		log.Printf("Audio file missing on disk: %s — %v", filePath, err)
		sendJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false, "message": "audio not uploaded",
		})
		return
	}
	defer f.Close()

	contentType := meta.ContentType
	if contentType == "" {
		contentType = "audio/mpeg"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "public, max-age=3600")

	// http.ServeContent handles Range requests, If-Modified-Since, and
	// Content-Length automatically. It reads the file size via Stat().
	http.ServeContent(w, r, meta.Filename, time.Time{}, f)
}

// ---------- helpers ----------

// extFromContentType returns a file extension for the given content type.
// Falls back to the filename extension if the MIME type is generic, then
// to "bin" if nothing else matches.
func extFromContentType(contentType, filename string) string {
	// Try MIME → ext map first
	if ext, ok := audioExtByMIME[strings.ToLower(contentType)]; ok {
		return ext
	}
	// Try the mime package
	exts, _ := mime.ExtensionsByType(contentType)
	if len(exts) > 0 {
		e := strings.TrimPrefix(exts[0], ".")
		if e != "" {
			return e
		}
	}
	// Try filename extension for octet-stream
	if filename != "" {
		fext := strings.TrimPrefix(filepath.Ext(filename), ".")
		fext = strings.ToLower(fext)
		for _, allowed := range audioExts {
			if fext == allowed {
				return fext
			}
		}
	}
	return "bin"
}

// removeSlotFiles deletes any existing audio file for the given slot
// regardless of extension.
func (h *AudioHandler) removeSlotFiles(dir, slot string) {
	for _, ext := range audioExts {
		os.Remove(filepath.Join(dir, slot+"."+ext))
	}
}

// loadAudioDoc reads the portal_audio JSON from system_settings.
// Returns an empty doc when the row doesn't exist.
func (h *AudioHandler) loadAudioDoc() (portalAudioDoc, error) {
	doc := make(portalAudioDoc)
	var value string
	err := h.DB.QueryRow(
		"SELECT value FROM system_settings WHERE key = $1",
		audioSettingKey,
	).Scan(&value)
	if err == sql.ErrNoRows {
		return doc, nil
	}
	if err != nil {
		return doc, err
	}
	if err := json.Unmarshal([]byte(value), &doc); err != nil {
		return make(portalAudioDoc), nil
	}
	return doc, nil
}

// saveAudioDoc persists the portal_audio JSON to system_settings using
// ON CONFLICT (key) DO UPDATE.
func (h *AudioHandler) saveAudioDoc(doc portalAudioDoc) error {
	data, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	_, err = h.DB.Exec(`
		INSERT INTO system_settings (key, value, description, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (key) DO UPDATE SET value = $2, updated_at = NOW()
	`, audioSettingKey, string(data), audioSettingDesc)
	return err
}
