package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// handleGetEpisodePlaybackUrl mirrors getEpisodePlaybackUrl: returns a
// freshly-signed playback URL for an SFX episode (minted at play time).
func (s *server) handleGetEpisodePlaybackUrl(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ShowID    string `json:"showId"`
		SeasonID  string `json:"seasonId"`
		EpisodeID string `json:"episodeId"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	if body.ShowID == "" || body.SeasonID == "" || body.EpisodeID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "showId, seasonId, and episodeId are required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	raw, _, err := s.doRest(ctx, "episodes", url.Values{
		"select": {"video_url"}, "id": {"eq." + url.QueryEscape(body.EpisodeID)},
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	var rows []struct {
		VideoURL *string `json:"video_url"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if len(rows) == 0 || rows[0].VideoURL == nil || *rows[0].VideoURL == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "Episode " + body.EpisodeID + " not found"})
		return
	}

	videoURL := *rows[0].VideoURL
	signed, err := s.signSfxUrl(ctx, videoURL)
	if err != nil {
		// Non-fatal: return the raw URL on signing failure.
		signed = videoURL
	}

	out := map[string]any{
		"success": true,
		"url":     signed,
	}
	// For SFX URLs the token expires shortly after playback; signal that the
	// client should mint fresh per play (matches the source function).
	if !strings.Contains(videoURL, "objects.solofx.net") {
		out["expiresIn"] = nil
	}
	writeJSON(w, http.StatusOK, out)
}

// signSfxUrl signs an SFX (objects.solofx.net) media URL. When
// SFX_SIGN_SECRET is set, signs natively with the same HMAC-SHA256 / base64url
// scheme the Cloudflare sfx-signed-urls worker uses (so tokens are identical).
// Otherwise falls back to the gate's /_sign endpoint (legacy). Non-SFX URLs are
// returned unchanged.
func (s *server) signSfxUrl(ctx context.Context, videoURL string) (string, error) {
	if !strings.Contains(videoURL, "objects.solofx.net") {
		return videoURL, nil
	}
	signKey := os.Getenv("SFX_SIGN_KEY")
	secret := os.Getenv("SFX_SIGN_SECRET")

	u, err := url.Parse(videoURL)
	if err != nil {
		return videoURL, err
	}

	if secret != "" {
		// Native signing: token = base64url(HMAC-SHA256(secret, "${path}:${exp}")).
		now := time.Now().Unix()
		exp := now + 4*3600 + 300 // DEFAULT_TTL (4h) + 300s buffer
		token := sfxHMAC(u.Path, exp, secret)
		signed := *u
		q := signed.Query()
		q.Set("token", token)
		q.Set("exp", fmt.Sprintf("%d", exp))
		signed.RawQuery = q.Encode()
		return signed.String(), nil
	}

	if signKey == "" {
		return videoURL, fmt.Errorf("SFX_SIGN_KEY not configured")
	}

	signURL := "https://objects.solofx.net/_sign?path=" + url.QueryEscape(u.Path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, signURL, nil)
	if err != nil {
		return videoURL, err
	}
	req.Header.Set("X-Sign-Key", signKey)
	resp, err := s.client.Do(req)
	if err != nil {
		return videoURL, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return videoURL, fmt.Errorf("_sign failed with status %d", resp.StatusCode)
	}
	var data struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &data); err != nil || data.URL == "" {
		return videoURL, fmt.Errorf("_sign returned no url")
	}
	return data.URL, nil
}

// sfxHMAC reproduces the Cloudflare sfx-signed-urls worker's sign():
//   token = base64url( HMAC-SHA256(key=secret, data="${path}:${exp}") )
// This lets the Go mesh mint byte-identical tokens to the worker, so clients
// (and any worker still validating) accept them.
func sfxHMAC(path string, exp int64, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s:%d", path, exp)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// refreshOndemandCache rebuilds the in-memory on-demand payload (mirrors
// scheduleOnDemandCacheRefresh, which ran every 6 hours).
func (s *server) refreshOndemandCache(ctx context.Context) error {
	_, _, _, err := s.ondemandPayload(ctx)
	return err
}

// handleUploadShowPoster mirrors uploadShowPoster: uploads a base64 poster
// image to Supabase Storage (epg-images/posters/<showId>_<aspect>.jpg) and
// returns the public URL.
func (s *server) handleUploadShowPoster(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ShowID      string `json:"showId"`
		Aspect      string `json:"aspect"`
		ImageBase64 string `json:"imageBase64"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	if body.ShowID == "" || body.Aspect == "" || body.ImageBase64 == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "showId, aspect, and imageBase64 are required"})
		return
	}
	if body.Aspect != "16x9" && body.Aspect != "2x3" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "aspect must be 16x9 or 2x3"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	url, err := s.uploadShowPoster(ctx, body.ShowID, body.Aspect, body.ImageBase64)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "poster upload failed: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"success": true, "url": url})
}

// uploadShowPoster uploads a base64 poster to Supabase Storage under
// epg-images/posters/<showId>_<aspect>.jpg and returns the public URL.
func (s *server) uploadShowPoster(ctx context.Context, showID, aspect, base64Data string) (string, error) {
	clean := strings.TrimPrefix(base64Data, "data:image/jpeg;base64,")
	clean = strings.TrimPrefix(clean, "data:image/png;base64,")
	clean = strings.TrimPrefix(clean, "data:image/webp;base64,")
	raw, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return "", err
	}

	path := fmt.Sprintf("posters/%s_%s.jpg", showID, aspect)
	storageURL := s.restURL
	if i := strings.Index(storageURL, "/rest/v1"); i >= 0 {
		storageURL = storageURL[:i]
	}

	// Delete any existing poster first so re-uploads overwrite.
	delURL := storageURL + "/storage/v1/object/epg-images/" + path
	if req, err := http.NewRequestWithContext(ctx, http.MethodDelete, delURL, nil); err == nil {
		req.Header.Set("apikey", s.serviceKey)
		req.Header.Set("Authorization", "Bearer "+s.serviceKey)
		if resp, err := s.client.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}

	u := storageURL + "/storage/v1/object/epg-images/" + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("apikey", s.serviceKey)
	req.Header.Set("Authorization", "Bearer "+s.serviceKey)
	req.Header.Set("Content-Type", "image/jpeg")
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
	return fmt.Sprintf("%s/storage/v1/object/public/epg-images/%s", publicBase, path), nil
}
