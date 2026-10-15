package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
