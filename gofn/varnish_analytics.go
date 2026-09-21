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

type MinuteSnapshot struct {
	Minute        time.Time
	Stream1Viewers int
	Stream2Viewers int
}

type VarnishWindowTracker struct {
	mu           sync.RWMutex
	sessions     map[string]*VarnishViewerSession // key = stream + ":" + ClientIP + ":" + SessionID
	history      []MinuteSnapshot
	lastSnapshot time.Time
}

var globalVarnishTracker = &VarnishWindowTracker{
	sessions: make(map[string]*VarnishViewerSession),
}

func (vt *VarnishWindowTracker) RecordMinuteSnapshot() {
	vt.mu.Lock()
	defer vt.mu.Unlock()

	now := time.Now().UTC().Truncate(time.Minute)
	if !vt.lastSnapshot.IsZero() && now.Equal(vt.lastSnapshot) {
		return // Already recorded for this minute
	}

	cutoff := time.Now().Add(-30 * time.Second)
	s1 := 0
	s2 := 0
	for _, sess := range vt.sessions {
		if sess.LastSeen.After(cutoff) {
			if sess.Stream == "stream" {
				s1++
			} else if sess.Stream == "stream2" {
				s2++
			}
		}
	}

	vt.history = append(vt.history, MinuteSnapshot{
		Minute:         now,
		Stream1Viewers: s1,
		Stream2Viewers: s2,
	})

	// Retain up to 1440 minutes (24 hours) of history
	if len(vt.history) > 1440 {
		vt.history = vt.history[len(vt.history)-1440:]
	}
	vt.lastSnapshot = now
}

func (vt *VarnishWindowTracker) GetHistory(requestedMinutes int) []map[string]any {
	vt.mu.RLock()
	defer vt.mu.RUnlock()

	if requestedMinutes < 1 {
		requestedMinutes = 30
	}
	if requestedMinutes > 1440 {
		requestedMinutes = 1440
	}

	cutoff := time.Now().UTC().Add(-time.Duration(requestedMinutes) * time.Minute)
	var result []map[string]any

	for _, snap := range vt.history {
		if snap.Minute.After(cutoff) || snap.Minute.Equal(cutoff) {
			minuteIso := snap.Minute.Format(time.RFC3339)
			result = append(result, map[string]any{
				"minute":  minuteIso,
				"stream":  "stream",
				"viewers": snap.Stream1Viewers,
			})
			result = append(result, map[string]any{
				"minute":  minuteIso,
				"stream":  "stream2",
				"viewers": snap.Stream2Viewers,
			})
		}
	}

	// If history has fewer than 2 snapshots (e.g. initial startup), pre-fill recent minutes with current counts
	if len(result) == 0 {
		now := time.Now().UTC().Truncate(time.Minute)
		s1 := 0
		s2 := 0
		cutoffSess := time.Now().Add(-30 * time.Second)
		for _, sess := range vt.sessions {
			if sess.LastSeen.After(cutoffSess) {
				if sess.Stream == "stream" {
					s1++
				} else if sess.Stream == "stream2" {
					s2++
				}
			}
		}
		for i := requestedMinutes - 1; i >= 0; i-- {
			t := now.Add(-time.Duration(i) * time.Minute)
			mIso := t.Format(time.RFC3339)
			result = append(result, map[string]any{
				"minute":  mIso,
				"stream":  "stream",
				"viewers": s1,
			})
			result = append(result, map[string]any{
				"minute":  mIso,
				"stream":  "stream2",
				"viewers": s2,
			})
		}
	}

	return result
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

func (vt *VarnishWindowTracker) GetActiveViewerIPs(windowDuration time.Duration) []string {
	vt.mu.RLock()
	defer vt.mu.RUnlock()

	cutoff := time.Now().Add(-windowDuration)
	ipMap := make(map[string]bool)
	for _, sess := range vt.sessions {
		if sess.LastSeen.After(cutoff) {
			if sess.ClientIP != "" && sess.ClientIP != "-" {
				ipMap[sess.ClientIP] = true
			}
		}
	}

	ips := make([]string, 0, len(ipMap))
	for ip := range ipMap {
		ips = append(ips, ip)
	}
	return ips
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
		"stream":               0,
		"stream2":              0,
		"app/stream/abr.m3u8":  0,
		"app/stream2/abr.m3u8": 0,
	}
	total := 0

	for _, sess := range vt.sessions {
		if sess.LastSeen.After(cutoff) {
			total++
			if sess.Stream == "stream" {
				streamCounts["stream"]++
				streamCounts["app/stream/abr.m3u8"]++
			} else if sess.Stream == "stream2" {
				streamCounts["stream2"]++
				streamCounts["app/stream2/abr.m3u8"]++
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
