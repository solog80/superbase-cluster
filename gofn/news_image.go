package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	deepteamswebp "github.com/deepteams/webp"
	"golang.org/x/image/draw"
	xwebp "golang.org/x/image/webp"
)

const newsImageBucketPath = "epg-images"

// newsImageMaxWidth is the target width for web-optimized article images.
const newsImageMaxWidth = 1280

// newsImageTargetBytes caps the output WebP size. The encoder picks the highest
// quality that fits under this budget (best middle-ground compression).
const newsImageTargetBytes = 150 * 1024

// newsImageQualitySteps are tried highest-to-lowest until the size budget is met.
var newsImageQualitySteps = []int{82, 78, 74, 70, 66, 62, 58, 54, 50}

// storageUpload pushes raw bytes into the mesh Supabase Storage bucket and
// returns the public URL.
func (s *server) storageUpload(ctx context.Context, path, mime string, raw []byte) (string, error) {
	storageURL := s.restURL
	if i := strings.Index(storageURL, "/rest/v1"); i >= 0 {
		storageURL = storageURL[:i]
	}
	u := storageURL + "/storage/v1/object/" + newsImageBucketPath + "/" + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("apikey", s.serviceKey)
	req.Header.Set("Authorization", "Bearer "+s.serviceKey)
	req.Header.Set("Content-Type", mime)
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("storage upload %d: %s", resp.StatusCode, string(body))
	}
	publicBase := getenv("PUBLIC_URL", "https://edge.solofx.net")
	return fmt.Sprintf("%s/storage/v1/object/public/%s/%s", publicBase, newsImageBucketPath, path), nil
}

// handleUploadNewsImage accepts a multipart image upload, resizes it for the
// web, converts it to WebP, and stores it on the mesh storage under
// news/<folder>/<id>.webp (folder defaults to <year>/<month>). The returned
// public URL is what admin posts into Joomla.
func (s *server) handleUploadNewsImage(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(25 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid multipart form: " + err.Error()})
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "file is required"})
		return
	}
	defer file.Close()
	folder := sanitizeFolder(r.FormValue("folder"))
	if folder == "" {
		folder = fmt.Sprintf("%d/%02d", time.Now().Year(), time.Now().Month())
	}
	src, _, err := image.Decode(file)
	if err != nil {
		// Fall back to WebP (not registered with image.Decode).
		if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "file is required"})
			return
		}
		src, err = xwebp.Decode(file)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported image format"})
			return
		}
	}
	b := src.Bounds()
	scale := 1.0
	if b.Dx() > newsImageMaxWidth {
		scale = float64(newsImageMaxWidth) / float64(b.Dx())
	}
	dstW := int(float64(b.Dx()) * scale)
	dstH := int(float64(b.Dy()) * scale)
	if dstW < 1 || dstH < 1 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "image too small"})
		return
	}
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)

	var out []byte
	for _, q := range newsImageQualitySteps {
		var buf bytes.Buffer
		if err := deepteamswebp.Encode(&buf, dst, &deepteamswebp.EncoderOptions{Quality: float32(q)}); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "webp encode failed: " + err.Error()})
			return
		}
		out = buf.Bytes()
		if len(out) <= newsImageTargetBytes {
			break
		}
	}

	id, err := randomHex(16)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	path := "news/" + folder + "/" + id + ".webp"
	ctx := r.Context()
	url, err := s.storageUpload(ctx, path, "image/webp", out)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("[uploadNewsImage] %dx%d -> %s", dstW, dstH, url)
	writeJSON(w, http.StatusOK, map[string]any{"url": url})
}

// sanitizeFolder normalizes a user-supplied folder path so it stays inside the
// news root and cannot traverse elsewhere.
func sanitizeFolder(f string) string {
	f = strings.Trim(f, " /")
	f = strings.ReplaceAll(f, "\\", "/")
	if f == "" || f == "." {
		return ""
	}
	parts := strings.Split(f, "/")
	var clean []string
	for _, p := range parts {
		switch p {
		case "", ".":
			continue
		case "..":
			return ""
		}
		clean = append(clean, p)
	}
	return strings.Join(clean, "/")
}

// handleListNewsImages lists objects under the news/ prefix (folders included),
// so admin can browse the media library and reuse existing images.
func (s *server) handleListNewsImages(w http.ResponseWriter, r *http.Request) {
	folder := sanitizeFolder(r.URL.Query().Get("folder"))
	prefix := "news"
	if folder != "" {
		prefix = "news/" + folder + "/"
	} else {
		prefix = "news/"
	}
	offset := 0
	fmt.Sscanf(r.URL.Query().Get("offset"), "%d", &offset)
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		var l int
		fmt.Sscanf(v, "%d", &l)
		if l > 0 && l <= 500 {
			limit = l
		}
	}

	objs, err := s.storageList(r.Context(), prefix, offset, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	publicBase := getenv("PUBLIC_URL", "https://edge.solofx.net")
	images := make([]map[string]any, 0, len(objs))
	for _, o := range objs {
		name, _ := o["name"].(string)
		if name == "" {
			continue
		}
		path := prefix + name
		isFolder := strings.HasSuffix(name, "/") || o["metadata"] == nil
		if isFolder {
			// Folders come back as "2026" or "2026/"; normalize to a slash form.
			name = strings.TrimSuffix(name, "/")
			path = "news/" + folder + "/" + name + "/"
			if folder == "" {
				path = "news/" + name + "/"
			}
		}
		entry := map[string]any{
			"name":     name,
			"path":     path,
			"isFolder": isFolder,
		}
		if !isFolder {
			entry["url"] = fmt.Sprintf("%s/storage/v1/object/public/epg-images/%s", publicBase, path)
			entry["sizeBytes"] = o["size"]
			entry["updatedAt"] = o["updated_at"]
		}
		images = append(images, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"prefix": prefix, "folder": folder, "images": images, "offset": offset + len(objs)})
}

// handleCreateNewsFolder creates an empty folder under news/ by uploading a
// zero-byte placeholder object. Supabase Storage materializes a folder as soon
// as an object exists beneath it.
func (s *server) handleCreateNewsFolder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Folder string `json:"folder"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body"})
		return
	}
	folder := sanitizeFolder(body.Folder)
	if folder == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "folder is required"})
		return
	}
	path := "news/" + folder + "/.keep"
	if _, err := s.storageUpload(r.Context(), path, "application/octet-stream", nil); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("[createNewsFolder] %s", folder)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "folder": folder})
}

// handleDeleteNewsImage deletes an image object from the news media library.
func (s *server) handleDeleteNewsImage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body"})
		return
	}
	path := strings.Trim(body.Path, " /")
	path = strings.ReplaceAll(path, "\\", "/")
	if !strings.HasPrefix(path, "news/") {
		path = "news/" + path
	}
	parts := strings.Split(path, "/")
	var clean []string
	for _, p := range parts {
		switch p {
		case "", ".":
			continue
		case "..":
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid path"})
			return
		}
		clean = append(clean, p)
	}
	if len(clean) < 2 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "path must include news/<file> or news/<folder>/<file>"})
		return
	}
	path = strings.Join(clean, "/")

	storageURL := s.restURL
	if i := strings.Index(storageURL, "/rest/v1"); i >= 0 {
		storageURL = storageURL[:i]
	}
	u := storageURL + "/storage/v1/object/" + newsImageBucketPath + "/" + path
	req, err := http.NewRequestWithContext(r.Context(), http.MethodDelete, u, nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	req.Header.Set("apikey", s.serviceKey)
	req.Header.Set("Authorization", "Bearer "+s.serviceKey)
	resp, err := s.client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("storage delete %d: %s", resp.StatusCode, string(b))})
		return
	}
	log.Printf("[deleteNewsImage] %s", path)
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// storageList lists objects in the mesh storage bucket under a prefix.
func (s *server) storageList(ctx context.Context, prefix string, offset, limit int) ([]map[string]any, error) {
	storageURL := s.restURL
	if i := strings.Index(storageURL, "/rest/v1"); i >= 0 {
		storageURL = storageURL[:i]
	}
	u := storageURL + "/storage/v1/object/list/" + newsImageBucketPath
	body, _ := json.Marshal(map[string]any{
		"prefix": prefix, "limit": limit, "offset": offset,
		"sortBy": map[string]any{"column": "name", "order": "asc"},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("apikey", s.serviceKey)
	req.Header.Set("Authorization", "Bearer "+s.serviceKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("storage list %d: %s", resp.StatusCode, string(raw))
	}
	var objs []map[string]any
	if err := json.Unmarshal(raw, &objs); err != nil {
		return nil, fmt.Errorf("bad storage list response: %v", err)
	}
	return objs, nil
}