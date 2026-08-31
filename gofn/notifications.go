package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ──────────────── ADMIN BROADCAST NOTIFICATIONS ────────────────
// The admin composes + sends FCM push notifications (broadcast to all users,
// or to one user). Every send is recorded in Supabase `notifications` as the
// sent-history log. Firebase Auth/FCM stays the push channel; Supabase is the
// master for the log.

// sendFCMMany sends to many tokens with a small worker pool.
func (s *server) sendFCMMany(ctx context.Context, tokens []string, title, body, image string, data map[string]string) int {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 20)
	var mu sync.Mutex
	success := 0
	for _, tok := range tokens {
		wg.Add(1)
		sem <- struct{}{}
		go func(t string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := s.fcmSend(ctx, t, title, body, image, data); err == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}(tok)
	}
	wg.Wait()
	return success
}

func (s *server) allFCMTokens(ctx context.Context) ([]string, error) {
	var tokens []string
	offset := 0
	for {
		raw, _, err := s.doRest(ctx, "users", url.Values{
			"select":     {"fcm_tokens"},
			"fcm_tokens": {"neq.[]"},
			"limit":      {"1000"}, "offset": {strconv.Itoa(offset)},
		})
		if err != nil {
			return nil, err
		}
		var rows []struct {
			FCMTokens json.RawMessage `json:"fcm_tokens"`
		}
		if err := json.Unmarshal(raw, &rows); err != nil {
			return nil, err
		}
		for _, r := range rows {
			var t []string
			if json.Unmarshal(r.FCMTokens, &t) == nil && len(t) > 0 {
				tokens = append(tokens, t...)
			}
		}
		if len(rows) < 1000 {
			break
		}
		offset += 1000
	}
	return tokens, nil
}

func (s *server) userFCMTokens(ctx context.Context, uid string) ([]string, error) {
	raw, _, err := s.doRest(ctx, "users", url.Values{
		"select": {"fcm_tokens"}, "id": {"eq." + url.QueryEscape(uid)},
	})
	if err != nil {
		return nil, err
	}
	var rows []struct {
		FCMTokens json.RawMessage `json:"fcm_tokens"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	var t []string
	_ = json.Unmarshal(rows[0].FCMTokens, &t)
	return t, nil
}

// notificationSend carries the fields for a push send task.
type notificationSend struct {
	Title       string
	Message     string
	Type        string
	Link        string
	ImageURL    string
	UserID      string
	IsBroadcast bool
	SentBy      string
}

// runNotificationSend executes a send outside of the HTTP request lifecycle so
// large broadcasts survive client/Vercel disconnects. Broadcasts take minutes
// (fetching ~15k tokens + FCM fan-out) and are serialized via broadcastMu.
// It writes a "sending" log row first (so the UI shows live status) and flips
// it to "sent"/"failed" when done. Returns recipient count and sends succeeded.
func (s *server) runNotificationSend(task notificationSend) (int, int) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	id, _ := randomHex(8)
	now := time.Now().UTC().Format(time.RFC3339)
	logRow := map[string]any{
		"id":           id,
		"title":        task.Title,
		"message":      task.Message,
		"type":         orStr(task.Type, "info"),
		"link":         orStr(task.Link, ""),
		"image_url":    orStr(task.ImageURL, ""),
		"is_broadcast": task.IsBroadcast,
		"recipients":   0,
		"created_at":   now,
		"status":       "sending",
		"sent_by":      task.SentBy,
	}
	if !task.IsBroadcast {
		logRow["user_id"] = task.UserID
	}
	if err := s.restPostRow(ctx, "notifications", logRow); err != nil {
		log.Printf("[sendNotification] log insert failed: %v", err)
	}

	finish := func(status string, recipients, sent int) {
		if err := s.restPatch(ctx, "notifications", "id=eq."+url.QueryEscape(id), map[string]any{
			"status": status, "recipients": recipients, "sent": sent,
		}); err != nil {
			log.Printf("[sendNotification] status update failed: %v", err)
		}
	}

	var tokens []string
	var err error
	if task.IsBroadcast {
		tokens, err = s.allFCMTokens(ctx)
	} else {
		tokens, err = s.userFCMTokens(ctx, task.UserID)
	}
	if err != nil {
		log.Printf("[sendNotification] fetch tokens failed: %v", err)
		finish("failed", 0, 0)
		return 0, 0
	}
	if len(tokens) == 0 {
		log.Printf("[sendNotification] no tokens for broadcast=%v", task.IsBroadcast)
		finish("failed", 0, 0)
		return 0, 0
	}

	data := map[string]string{
		"type":         orStr(task.Type, "info"),
		"title":        task.Title,
		"message":      task.Message,
		"link":         task.Link,
		"imageUrl":     task.ImageURL,
		"click_action": "FLUTTER_NOTIFICATION_CLICK",
	}
	sent := s.sendFCMMany(ctx, tokens, task.Title, task.Message, task.ImageURL, data)

	finish("sent", len(tokens), sent)
	log.Printf("[sendNotification] done broadcast=%v recipients=%d sent=%d", task.IsBroadcast, len(tokens), sent)
	return len(tokens), sent
}

// handleSendNotification sends a broadcast (or single-user) FCM push and logs it.
// Individual sends run synchronously (fast); broadcasts run in the background so
// the HTTP request returns immediately (Vercel functions time out otherwise).
func (s *server) handleSendNotification(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title    string `json:"title"`
		Message  string `json:"message"`
		Type     string `json:"type"`
		Link     string `json:"link"`
		ImageURL string `json:"imageUrl"`
		UserID   string `json:"userId"`
		SentBy   string `json:"sentBy"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	if strings.TrimSpace(body.Title) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "title is required"})
		return
	}

	isBroadcast := body.UserID == ""
	task := notificationSend{
		Title:       body.Title,
		Message:     body.Message,
		Type:        body.Type,
		Link:        body.Link,
		ImageURL:    body.ImageURL,
		UserID:      body.UserID,
		IsBroadcast: isBroadcast,
		SentBy:      strings.TrimSpace(body.SentBy),
	}

	if isBroadcast {
		if !s.broadcastMu.TryLock() {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "A broadcast is already in progress. Try again in a few minutes."})
			return
		}
		go func() {
			defer s.broadcastMu.Unlock()
			s.runNotificationSend(task)
		}()
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true, "queued": true,
			"message": "Broadcast queued — delivering in the background",
		})
		return
	}

	// Individual sends are fast; run synchronously so the UI gets live counts.
	recipients, sent := s.runNotificationSend(task)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "recipients": recipients, "sent": sent,
		"message": "Notification sent",
	})
}

// startNotificationStaleCleanup marks any in-flight "sending" rows as failed
// shortly after startup — a mesh restart mid-broadcast would otherwise leave a
// log entry stuck in the "sending" state forever.
func (s *server) startNotificationStaleCleanup() {
	go func() {
		time.Sleep(5 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.restPatch(ctx, "notifications", "status=eq.sending", map[string]any{"status": "failed"}); err != nil {
			log.Printf("[notifications] stale cleanup failed: %v", err)
		} else {
			log.Printf("[notifications] stale sending rows marked failed")
		}
	}()
}

// handleGetSentNotifications returns the sent-history log (newest first).
func (s *server) handleGetSentNotifications(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 10
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 && v <= 200 {
		limit = v
	}
	page := 1
	if v, err := strconv.Atoi(q.Get("page")); err == nil && v > 0 {
		page = v
	}
	offset := (page - 1) * limit

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	total, _ := s.countTableRows(ctx, "notifications", nil)

	vals := url.Values{
		"select": {"*"}, "order": {"created_at.desc"},
		"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)},
	}
	raw, _, err := s.doRest(ctx, "notifications", vals)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "bad response"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"notifications": rows, "total": total,
	})
}

// handleDeleteNotification deletes a single sent-history log row by id.
func (s *server) handleDeleteNotification(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id is required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := s.restDelete(ctx, "notifications", "id=eq."+url.QueryEscape(id)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// handleClearSentNotifications deletes the entire sent-history log.
func (s *server) handleClearSentNotifications(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	// PostgREST requires a WHERE clause; created_at is NOT NULL on every row,
	// so this matches the whole table.
	if err := s.restDelete(ctx, "notifications", "created_at=not.is.null"); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "Sent history cleared"})
}

// handleDeleteNotifications deletes multiple sent-history rows by ids (POST with {ids: [...]}).
func (s *server) handleDeleteNotifications(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.IDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "ids array is required"})
		return
	}
	if len(body.IDs) > 200 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "max 200 ids per request"})
		return
	}
	quoted := make([]string, len(body.IDs))
	for i, id := range body.IDs {
		quoted[i] = url.QueryEscape(id)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.restDelete(ctx, "notifications", "id=in.("+strings.Join(quoted, ",")+")"); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "deleted": len(body.IDs)})
}

// handleGetLinkMetadata fetches a URL's <title>/description/og:image for the
// notification form's auto-fill.
func (s *server) handleGetLinkMetadata(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body"})
		return
	}
	if !strings.HasPrefix(body.URL, "http://") && !strings.HasPrefix(body.URL, "https://") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid url"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, body.URL, nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; SaltAdmin/1.0)")
	resp, err := s.client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to fetch url"})
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	html := string(raw)

	meta := func(prop string) string {
		re := regexp.MustCompile(`(?is)<meta[^>]+(?:property|name)="` + regexp.QuoteMeta(prop) + `"[^>]+content="([^"]+)"`)
		if m := re.FindStringSubmatch(html); len(m) == 2 {
			return strings.TrimSpace(m[1])
		}
		return ""
	}
	title := meta("og:title")
	if title == "" {
		title = meta("twitter:title")
	}
	if title == "" {
		if m := regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`).FindStringSubmatch(html); len(m) == 2 {
			title = strings.TrimSpace(m[1])
		}
	}
	desc := meta("og:description")
	if desc == "" {
		desc = meta("description")
	}
	image := meta("og:image")
	if image == "" {
		image = meta("twitter:image")
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"metadata": map[string]string{
			"title": title, "description": desc, "image": image,
		},
	})
}

// countTableRows returns an exact row count for a table using Prefer: count=exact.
func (s *server) countTableRows(ctx context.Context, table string, filters url.Values) (int, error) {
	u := s.restURL + "/" + table + "?select=id"
	if e := filters.Encode(); e != "" {
		u += "&" + e
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("apikey", s.serviceKey)
	req.Header.Set("Authorization", "Bearer "+s.serviceKey)
	req.Header.Set("Prefer", "count=exact")
	req.Header.Set("Range", "0-0")
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	cr := resp.Header.Get("Content-Range")
	if i := strings.LastIndex(cr, "/"); i >= 0 {
		if n, err := strconv.Atoi(strings.TrimSpace(cr[i+1:])); err == nil {
			return n, nil
		}
	}
	return 0, nil
}

// ──────────────── FCM (Firebase V1 HTTP API, no SDK) ────────────────

type fcmServiceAccount struct {
	ProjectID   string `json:"project_id"`
	PrivateKey  string `json:"private_key"`
	ClientEmail string `json:"client_email"`
	TokenURI    string `json:"token_uri"`
}

func loadFCMSA() (*fcmServiceAccount, error) {
	raw := os.Getenv("FCM_SERVICE_ACCOUNT")
	if raw == "" {
		return nil, errors.New("FCM_SERVICE_ACCOUNT not set")
	}
	var sa fcmServiceAccount
	if err := json.Unmarshal([]byte(raw), &sa); err != nil {
		return nil, err
	}
	if sa.PrivateKey == "" || sa.ClientEmail == "" {
		return nil, errors.New("FCM_SERVICE_ACCOUNT missing private_key/client_email")
	}
	return &sa, nil
}

func (s *server) serviceAccountToken(ctx context.Context, sa *fcmServiceAccount, scope string) (string, error) {
	now := time.Now().Unix()
	headerJSON, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT"})
	claimsJSON, _ := json.Marshal(map[string]any{
		"iss": sa.ClientEmail, "scope": scope,
		"aud": sa.TokenURI, "iat": now, "exp": now + 3600,
	})
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return "", errors.New("invalid private key pem")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", err
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return "", errors.New("not an RSA key")
	}
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	jwt := signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)

	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {jwt}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sa.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("token exchange %d: %s", resp.StatusCode, string(b))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(b, &tok); err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

func (s *server) fcmSend(ctx context.Context, token, title, body, image string, data map[string]string) error {
	sa, err := loadFCMSA()
	if err != nil {
		return err
	}
	access, err := s.serviceAccountToken(ctx, sa, "https://www.googleapis.com/auth/firebase.messaging")
	if err != nil {
		return err
	}
	notification := map[string]any{"title": title, "body": body}
	message := map[string]any{
		"token":        token,
		"notification": notification,
		"data": func() map[string]string {
			d := map[string]string{"click_action": "FLUTTER_NOTIFICATION_CLICK"}
			for k, v := range data {
				d[k] = v
			}
			return d
		}(),
	}
	if image != "" {
		notification["image"] = image
		message["apns"] = map[string]any{
			"fcm_options": map[string]any{"image": image},
			"payload": map[string]any{
				"aps": map[string]any{"mutable-content": 1},
			},
		}
	}
	msg := map[string]any{"message": message}
	payload, _ := json.Marshal(msg)
	u := "https://fcm.googleapis.com/v1/projects/" + url.PathEscape(sa.ProjectID) + "/messages:send"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("fcm %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
