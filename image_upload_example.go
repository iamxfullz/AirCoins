// This is an example of how the image upload API endpoint would be implemented
// Based on the existing audio.go and appearance.go patterns in the AirCoins API

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

// ImageHandler manages upload/delete/serve for portal user images
// Files would live in a directory like /var/lib/pisowifi/user-images/
// Metadata would be stored in a database table or system_settings
type ImageHandler struct {
	DB *sql.DB
}

const imageSettingKey = "portal_user_images"
const imageSettingDesc = "Portal user uploaded images metadata as JSON"

// imageDir returns the directory holding uploaded user images
func imageDir() string {
	if v := os.Getenv("USER_IMAGES_DIR"); v != "" {
		return v
	}
	return "/var/lib/pisowifi/user-images"
}

// maxImageBytes caps image uploads at 5 MB
const maxImageBytes = 5 << 20

// validImageTypes lists accepted image MIME types
var validImageTypes = map[string]bool{
	"image/jpeg": true,
	"image/jpg":  true,
	"image/png":  true,
}

// imageExtByMIME maps accepted image content types to file extensions
var imageExtByMIME = map[string]string{
	"image/jpeg": "jpg",
	"image/jpg":  "jpg",
	"image/png":  "png",
}

// ImageMeta is the per-image metadata
type ImageMeta struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	UploadedAt  string `json:"uploaded_at"`
	SessionID   string `json:"session_id"`  // Associated session token
	PublicURL   string `json:"public_url"`  // Publicly accessible URL
}

// UploadImage handles POST /api/portal/upload/image
// This would be a public endpoint for portal users to upload images
func (h *ImageHandler) UploadImage(w http.ResponseWriter, r *http.Request) {
	// Hard cap: 5 MB file + a little headroom for multipart framing
	r.Body = http.MaxBytesReader(w, r.Body, maxImageBytes+64<<10)
	if err := r.ParseMultipartForm(maxImageBytes + 64<<10); err != nil {
		sendJSON(w, http.StatusRequestEntityTooLarge, map[string]interface{}{
			"success": false, "message": "File too large (max 5 MB)",
		})
		return
	}
	defer r.MultipartForm.RemoveAll()

	file, header, err := r.FormFile("file")
	if err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false, "message": "Missing 'file' field in multipart form",
		})
		return
	}
	defer file.Close()

	if header.Size > maxImageBytes {
		sendJSON(w, http.StatusRequestEntityTooLarge, map[string]interface{}{
			"success": false, "message": "File too large (max 5 MB)",
		})
		return
	}

	// Get session token from header or form
	sessionToken := r.Header.Get("X-Session-Token")
	if sessionToken == "" {
		sessionToken = r.FormValue("session_token")
	}

	// Validate file type
	contentType := header.Header.Get("Content-Type")
	if !validImageTypes[contentType] {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false, "message": "Only JPG, JPEG, and PNG images are allowed",
		})
		return
	}

	// Derive extension from content type
	ext := imageExtByMIME[contentType]
	if ext == "" {
		ext = "jpg" // default
	}

	dir := imageDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("Error creating image dir: %v", err)
		sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false, "message": "Failed to create image directory",
		})
		return
	}

	// Generate unique filename
	uniqueID := fmt.Sprintf("%d", time.Now().UnixNano())
	filename := fmt.Sprintf("%s_%s.%s", sessionToken, uniqueID, ext)
	finalPath := filepath.Join(dir, filename)

	// Write file
	f, err := os.Create(finalPath)
	if err != nil {
		log.Printf("Error creating image file: %v", err)
		sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false, "message": "Failed to save image file",
		})
		return
	}
	defer f.Close()

	written, err := io.Copy(f, file)
	if err != nil {
		os.Remove(finalPath)
		log.Printf("Error writing image file: %v", err)
		sendJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false, "message": "Failed to write image file",
		})
		return
	}

	// Set proper permissions
	os.Chmod(finalPath, 0644)

	// Create metadata
	uploadedAt := time.Now().UTC().Format(time.RFC3339)
	publicURL := fmt.Sprintf("/user-images/%s", filename)

	meta := ImageMeta{
		Filename:    filename,
		ContentType: contentType,
		Size:        written,
		UploadedAt:  uploadedAt,
		SessionID:   sessionToken,
		PublicURL:   publicURL,
	}

	// Save metadata to database (simplified example)
	// In real implementation, you would save to a database table

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"data": map[string]interface{}{
			"filename":     filename,
			"content_type": contentType,
			"size":         written,
			"uploaded_at":  uploadedAt,
			"session_id":   sessionToken,
			"url":          publicURL,
			"message":      "Image uploaded successfully",
		},
	})
}

// ServeImage handles GET /user-images/<filename> (PUBLIC)
func (h *ImageHandler) ServeImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract filename from path: /user-images/<filename>
	filename := strings.TrimPrefix(r.URL.Path, "/user-images/")
	if filename == "" || strings.Contains(filename, "/") {
		sendJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false, "message": "Image not found",
		})
		return
	}

	filePath := filepath.Join(imageDir(), filename)
	f, err := os.Open(filePath)
	if err != nil {
		log.Printf("Image file missing: %s — %v", filePath, err)
		sendJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false, "message": "Image not found",
		})
		return
	}
	defer f.Close()

	// Try to detect content type from file extension
	contentType := mime.TypeByExtension(filepath.Ext(filename))
	if contentType == "" {
		contentType = "image/jpeg" // default
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=3600")

	// Serve the file
	http.ServeContent(w, r, filename, time.Time{}, f)
}

// Helper function for JSON responses
func sendJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}