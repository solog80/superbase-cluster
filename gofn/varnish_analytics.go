package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type VarnishViewerSession struct {
	Stream    string    // "stream" or "stream2"
	ClientIP  string    // e.g. "154.72.217.98"
	SessionID string    // e.g. "75_" or "105_oIXQSGmJ"
	LastSeen  time.Time
}

type VarnishWindowTracker struct {
	mu       sync.RWMutex
	sessions map[string]*VarnishViewerSession // key = stream + ":" + ClientIP + ":" + SessionID
}

var globalVarnishTracker = &VarnishWindowTracker{
	sessions: make(map[string]*VarnishViewerSession),
}

// RecordHit processes a single log line (or IP + raw URI tuple).
func (vt *VarnishWindowTracker) RecordHit(ip, rawURI string) {
	if ip == "" || ip == "-" {
		return
	}
	// Sanitize IP if port or proxy prefix attached
	if idx := strings.Index(ip, ":"); idx != -1 && !strings.Contains(ip, "]") && strings.Count(ip, ":") == 1 {
		ip = ip[:idx]
	}
	ip = strings.TrimSpace(ip)

	// Identify stream name from request path
	var stream string
	if strings.Contains(rawURI, "/app/stream2/") {
		stream = "stream2"
	} else if strings.Contains(rawURI, "/app/stream/") {
		stream = "stream"
	} else {
		return // Ignore non-live-stream traffic
	}

	// Extract session ID from query parameters if present
	sessionID := ""
	if qIdx := strings.Index(rawURI, "?"); qIdx != -1 {
		qs := rawURI[qIdx+1:]
		if sIdx := strings.Index(qs, "session="); sIdx != -1 {
			sub := qs[sIdx+8:]
			if amIdx := strings.Index(sub, "&"); amIdx != -1 {
				sessionID = sub[:amIdx]
			} else if spIdx := strings.Index(sub, " "); spIdx != -1 {
				sessionID = sub[:spIdx]
			} else {
				sessionID = sub
			}
		}
	}
	sessionID = strings.TrimSpace(sessionID)

	key := fmt.Sprintf("%s:%s:%s", stream, ip, sessionID)

	vt.mu.Lock()
	defer vt.mu.Unlock()
	if sess, ok := vt.sessions[key]; ok {
		sess.LastSeen = time.Now()
	} else {
		vt.sessions[key] = &VarnishViewerSession{
			Stream:    stream,
			ClientIP:  ip,
			SessionID: sessionID,
			LastSeen:  time.Now(),
		}
	}
}

// CleanupExpired prunes sessions older than windowDuration.
func (vt *VarnishWindowTracker) CleanupExpired(windowDuration time.Duration) {
	vt.mu.Lock()
	defer vt.mu.Unlock()
	cutoff := time.Now().Add(-windowDuration)
	for k, sess := range vt.sessions {
		if sess.LastSeen.Before(cutoff) {
			delete(vt.sessions, k)
		}
	}
}

func (vt *VarnishWindowTracker) StartCleanupLoop(interval, windowDuration time.Duration) {
	ticker := time.NewTicker(interval)
	for range ticker.C {
		vt.CleanupExpired(windowDuration)
	}
}

type LiveStatsSummary struct {
	TotalViewers  int            `json:"viewers"`
	StreamCounts  map[string]int `json:"streams"`
	WindowSeconds int            `json:"window_seconds"`
}

func (vt *VarnishWindowTracker) GetStats(windowDuration time.Duration) LiveStatsSummary {
	vt.mu.RLock()
	defer vt.mu.RUnlock()

	cutoff := time.Now().Add(-windowDuration)
	streamCounts := map[string]int{
		"stream":  0,
		"stream2": 0,
	}
	total := 0

	for _, sess := range vt.sessions {
		if sess.LastSeen.After(cutoff) {
			total++
			if sess.Stream == "stream" {
				streamCounts["stream"]++
			} else if sess.Stream == "stream2" {
				streamCounts["stream2"]++
			}
		}
	}

	return LiveStatsSummary{
		TotalViewers:  total,
		StreamCounts:  streamCounts,
		WindowSeconds: int(windowDuration.Seconds()),
	}
}

// startVarnishUDPListener opens a UDP socket to receive real-time Varnish log lines.
func startVarnishUDPListener(port string) {
	pc, err := net.ListenPacket("udp", ":"+port)
	if err != nil {
		log.Printf("[VarnishUDP] Failed to listen on :%s: %v", port, err)
		return
	}
	defer pc.Close()
	log.Printf("[VarnishUDP] Listening for Varnish logs on UDP port :%s", port)

	buf := make([]byte, 65535)
	for {
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			log.Printf("[VarnishUDP] Read error: %v", err)
			time.Sleep(1 * time.Second)
			continue
		}
		lines := strings.Split(string(buf[:n]), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, " ", 2)
			if len(parts) == 2 {
				globalVarnishTracker.RecordHit(parts[0], parts[1])
			}
		}
	}
}

// handleIngestVarnishLog enables HTTP POST batch ingestion of Varnish log lines.
func (s *server) handleIngestVarnishLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	lines := strings.Split(string(body), "\n")
	count := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) == 2 {
			globalVarnishTracker.RecordHit(parts[0], parts[1])
			count++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "ingested": count})
}
