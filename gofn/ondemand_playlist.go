package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ──────────────── SFX PLAYLIST REWRITE ────────────────

// signUri returns `abs?token=<sig>&exp=<exp>` for a playlist URI, resolving
// relative URIs against the playlist directory and stripping any existing query.
// Mirrors the Cloudflare worker's signUri() so tokens are byte-identical.
func (s *server) signUri(uri, dir string, exp int64, secret string) string {
	// Drop any existing query (e.g. a token already attached by a prior hop).
	if i := strings.IndexByte(uri, '?'); i >= 0 {
		uri = uri[:i]
	}
	abs := uri
	if !strings.HasPrefix(uri, "/") {
		abs = dir + uri
	}
	token := sfxHMAC(abs, exp, secret)
	return fmt.Sprintf("%s?token=%s&exp=%d", abs, token, exp)
}

// rewritePlaylist re-signs every URI in an m3u8/mpd (variants + segments) with
// a fresh token. Mirrors the worker's rewritePlaylist() so a Go-served playlist
// is indistinguishable from a worker-rewritten one. The token lives in the query
// string, which the Cloudflare "sfx-s3-cache" rule ignores for the cache key —
// so segments still share one edge-cache entry while being gated by the playlist.
func (s *server) rewritePlaylist(text, playlistPath string, exp int64, secret string) string {
	dir := playlistPath[:strings.LastIndex(playlistPath, "/")+1]
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			out = append(out, "")
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			if strings.Contains(trimmed, `URI="`) {
				out = append(out, s.replaceURIs(trimmed, dir, exp, secret))
			} else {
				out = append(out, trimmed)
			}
			continue
		}
		out = append(out, s.signUri(trimmed, dir, exp, secret))
	}
	return strings.Join(out, "\n")
}

// replaceURIs re-signs URIs inside `#EXT-X-MEDIA:...URI="..."` lines.
func (s *server) replaceURIs(line, dir string, exp int64, secret string) string {
	parts := strings.Split(line, `URI="`)
	if len(parts) < 2 {
		return line
	}
	result := parts[0]
	for i := 1; i < len(parts); i++ {
		rest := parts[i]
		end := strings.Index(rest, `"`)
		if end < 0 {
			result += `URI="` + rest
			continue
		}
		uri := rest[:end]
		result += `URI="` + s.signUri(uri, dir, exp, secret) + rest[end:]
	}
	return result
}

// handleGetSignedPlaylist rewrites and returns an SFX HLS/DASH playlist.
// Body: { url: "<signed master playlist URL>" }.
// The playlist is fetched from the QuObjects origin (direct, bypassing the
// Cloudflare worker so we get the plain playlist), every variant/segment URI is
// re-signed with a fresh token, and the rewritten playlist is returned.
func (s *server) handleGetSignedPlaylist(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	if body.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "url required"})
		return
	}

	u, err := url.Parse(body.URL)
	if err != nil || u.Path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad url"})
		return
	}

	secret := os.Getenv("SFX_SIGN_SECRET")

	// Fetch from the QuObjects origin directly (plain playlist, no worker
	// rewrite). Port 8010 = Swift proxy (HTTPS); 8090 = nginx TLS-relay (HTTP).
	origin := os.Getenv("SFX_ORIGIN")
	if origin == "" {
		origin = "http://100.116.185.70:8090"
	}
	fetchURL := origin + u.Path

	ctx := r.Context()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	resp, err := s.client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "origin fetch failed: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": fmt.Sprintf("origin status %d", resp.StatusCode)})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	isPlaylist := strings.HasSuffix(u.Path, ".m3u8") || strings.HasSuffix(u.Path, ".mpd")
	if !isPlaylist || secret == "" {
		// Non-playlist or no signing secret: proxy through unchanged.
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.Write(raw)
		return
	}

	// Mint a fresh expiry for the rewritten URIs (4h default + 300s buffer).
	now := time.Now().Unix()
	exp := now + 4*3600 + 300
	rewritten := s.rewritePlaylist(string(raw), u.Path, exp, secret)

	ct := "application/vnd.apple.mpegurl"
	if strings.HasSuffix(u.Path, ".mpd") {
		ct = "application/dash+xml"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store") // playlists change; never cache
	w.Write([]byte(rewritten))
}