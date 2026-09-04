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

// fetchBunnyVideo polls Bunny until the video is ready (status >= 4) and
// returns its metadata. Mirrors the old Firebase createEpisodeFromBunnyUpload
// wait-for-transcode behaviour, now run by the mesh.
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
	var last *bunnyVideo
	for i := 0; i < 12; i++ {
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return nil, fmt.Errorf("bunny fetch %s: %d %s", videoID, resp.StatusCode, string(b))
		}
		var v bunnyVideo
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, err
		}
		last = &v
		if v.Status >= 4 {
			return &v, nil
		}
		time.Sleep(3 * time.Second)
	}
	if last == nil {
		return nil, fmt.Errorf("bunny video %s gave no data", videoID)
	}
	return last, nil
}
