package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"
)

// ──────────────── BUNNY (video.bunnycdn.com) helpers ────────────────
// Used by mesh-first on-demand episode creation. The mesh is authoritative;
// Firebase stays a mirror. Mesh env must carry BUNNY_API_KEY/BUNNY_LIBRARY_ID
// (and optionally BUNNY_CDN_HOSTNAME).

func bunnyLibraryID() string { return os.Getenv("BUNNY_LIBRARY_ID") }
func bunnyAPIKey() string    { return os.Getenv("BUNNY_API_KEY") }

func bunnyCDNHostname() string {
	if h := os.Getenv("BUNNY_CDN_HOSTNAME"); h != "" {
		return h
	}
	return "vz-13b87e04-41b.b-cdn.net"
}

// bunnyVideo mirrors the subset of the Bunny video object we need.
type bunnyVideo struct {
	GUID             string  `json:"guid"`
	Title            string  `json:"title"`
	Length           float64 `json:"length"`
	Status           int     `json:"status"`
	ThumbnailFileName string `json:"thumbnailFileName"`
	Hostname         string  `json:"hostname"`
}

// fetchBunnyVideo fetches Bunny video metadata immediately. If the video is still
// transcoding (status < 4), it returns the metadata immediately with duration if
// available, avoiding long HTTP blocking that causes gateway timeouts.
func fetchBunnyVideo(client *http.Client, videoID string) (*bunnyVideo, error) {
	if bunnyAPIKey() == "" || bunnyLibraryID() == "" {
		return nil, fmt.Errorf("BUNNY_API_KEY / BUNNY_LIBRARY_ID not set")
	}
	url := fmt.Sprintf("https://video.bunnycdn.com/library/%s/videos/%s", bunnyLibraryID(), videoID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("AccessKey", bunnyAPIKey())

	// Up to 3 attempts with 1s pause in case the newly created video is propagating
	for i := 0; i < 3; i++ {
		resp, err := client.Do(req)
		if err != nil {
			if i == 2 {
				return nil, err
			}
			time.Sleep(1 * time.Second)
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var v bunnyVideo
			if err := json.Unmarshal(b, &v); err != nil {
				return nil, err
			}
			return &v, nil
		}
		if resp.StatusCode != http.StatusNotFound || i == 2 {
			return nil, fmt.Errorf("bunny fetch %s: %d %s", videoID, resp.StatusCode, string(b))
		}
		time.Sleep(1 * time.Second)
	}
	return nil, fmt.Errorf("bunny video %s not found", videoID)
}

// handleBunnyWebhook processes asynchronous video status updates from Bunny Stream webhooks.
// Payload: { "VideoLibraryId": 307069, "VideoGuid": "...", "Status": 3 }
// Status list: 0=Queued, 1=Processing, 2=Encoding, 3=Finished, 4=Resolution finished, 5=Failed.
func (s *server) handleBunnyWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read body"})
		return
	}

	// Signature verification (HMAC-SHA256 with library Read-Only API key if configured)
	if secret := os.Getenv("BUNNY_WEBHOOK_KEY"); secret != "" {
		sig := r.Header.Get("X-BunnyStream-Signature")
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(bodyBytes)
		expected := hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(sig), []byte(expected)) {
			log.Printf("[bunnyWebhook] Signature mismatch: got %s, expected %s", sig, expected)
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid signature"})
			return
		}
	}

	var payload struct {
		VideoLibraryID int    `json:"VideoLibraryId"`
		VideoGUID      string `json:"VideoGuid"`
		Status         int    `json:"Status"`
	}
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}

	if payload.VideoGUID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing VideoGuid"})
		return
	}

	log.Printf("[bunnyWebhook] Event received for video %s: status=%d (library=%d)", payload.VideoGUID, payload.Status, payload.VideoLibraryID)

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	now := time.Now().UTC().Format(time.RFC3339)

	switch payload.Status {
	case 3, 4:
		// Video Finished (3) or Resolution finished (4 - playable)
		v, err := fetchBunnyVideo(s.client, payload.VideoGUID)
		if err != nil {
			log.Printf("[bunnyWebhook] Failed to fetch video %s from Bunny: %v", payload.VideoGUID, err)
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}

		host := bunnyCDNHostname()
		if v.Hostname != "" {
			host = v.Hostname
		}
		thumbFile := v.ThumbnailFileName
		if thumbFile == "" {
			thumbFile = "thumbnail.jpg"
		}
		thumbURL := fmt.Sprintf("https://%s/%s/%s", host, v.GUID, thumbFile)
		videoURL := fmt.Sprintf("https://%s/%s/playlist.m3u8", host, v.GUID)

		patch := map[string]any{
			"processing": false,
			"duration":   int(v.Length),
			"thumbnail":  thumbURL,
			"video_url":  videoURL,
			"updated_at": now,
		}

		// Try updating by bunny_guid first
		affected, err := s.restPatchCount(ctx, "episodes", "bunny_guid=eq."+url.QueryEscape(payload.VideoGUID), patch)
		if err != nil || affected == 0 {
			// Fallback: match by video_url containing the guid
			affected, err = s.restPatchCount(ctx, "episodes", "video_url=like.*"+url.QueryEscape(payload.VideoGUID)+"*", patch)
		}

		s.clearOndemandCache()
		log.Printf("[bunnyWebhook] Video %s updated in DB: duration=%vs, rows_updated=%d", payload.VideoGUID, v.Length, affected)
		writeJSON(w, http.StatusOK, map[string]any{
			"success":     true,
			"videoGuid":   payload.VideoGUID,
			"status":      payload.Status,
			"duration":    v.Length,
			"rowsUpdated": affected,
		})
		return

	case 5:
		// Failed
		patch := map[string]any{
			"processing": false,
			"updated_at": now,
		}
		affected, _ := s.restPatchCount(ctx, "episodes", "bunny_guid=eq."+url.QueryEscape(payload.VideoGUID), patch)
		s.clearOndemandCache()
		log.Printf("[bunnyWebhook] Video %s failed transcoding: rows_updated=%d", payload.VideoGUID, affected)
		writeJSON(w, http.StatusOK, map[string]any{
			"success":     true,
			"videoGuid":   payload.VideoGUID,
			"status":      "failed",
			"rowsUpdated": affected,
		})
		return

	default:
		// 0=Queued, 1=Processing, 2=Encoding, 6/7/8=Upload states, etc.
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "status": payload.Status})
		return
	}
}

