package main

import (
	"context"
	"encoding/json"
	"fmt"
	stdhtml "html"
	"io"
	"log"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

func (s *server) startSystemEmailPollers() {
	if s.mail == nil {
		log.Printf("[system-email] SMTP is not configured; email pollers disabled")
		return
	}
	if !getenvBool("SMTP_ENABLED", true) {
		log.Printf("[system-email] SMTP_ENABLED=false; email pollers disabled")
		return
	}
	s.startWelcomeEmailPoller()
	s.startPaymentReceiptEmailPoller()
}

func (s *server) startWelcomeEmailPoller() {
	if !getenvBool("WELCOME_EMAIL_ENABLED", true) {
		log.Printf("[system-email] welcome emails disabled")
		return
	}
	interval := time.Duration(getenvInt64("WELCOME_EMAIL_INTERVAL_SECONDS", 60)) * time.Second
	lookback := time.Duration(getenvInt64("WELCOME_EMAIL_LOOKBACK_MINUTES", 15)) * time.Minute
	go s.runEvery(interval, func(ctx context.Context) {
		s.processWelcomeEmails(ctx, lookback)
	})
}

func (s *server) startPaymentReceiptEmailPoller() {
	if s.tsdb == nil {
		log.Printf("[system-email] payment receipt emails disabled (no TSDB config)")
		return
	}
	if !getenvBool("CONTRIBUTION_EMAIL_ENABLED", true) {
		log.Printf("[system-email] contribution emails disabled")
		return
	}
	interval := time.Duration(getenvInt64("CONTRIBUTION_EMAIL_INTERVAL_SECONDS", 30)) * time.Second
	lookback := time.Duration(getenvInt64("CONTRIBUTION_EMAIL_LOOKBACK_HOURS", 24)) * time.Hour
	go s.runEvery(interval, func(ctx context.Context) {
		s.processPaymentReceiptEmails(ctx, lookback)
	})
}

type systemUser struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	Name        string `json:"name"`
	Role        string `json:"role"`
	IsAdmin     bool   `json:"is_admin"`
	IsAnonymous bool   `json:"is_anonymous"`
	IsBlocked   bool   `json:"is_blocked"`
}

func (s *server) processWelcomeEmails(ctx context.Context, lookback time.Duration) {
	cutoff := time.Now().UTC().Add(-lookback)
	if since := strings.TrimSpace(getenv("WELCOME_EMAIL_SINCE", "")); since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil && t.After(cutoff) {
			cutoff = t
		}
	}
	raw, _, err := s.doRest(ctx, "users", url.Values{
		"select":       {"id,email,name,role,is_admin,is_anonymous,is_blocked,created_at"},
		"email":        {"not.is.null"},
		"is_anonymous": {"not.is.true"},
		"created_at":   {"gte." + cutoff.UTC().Format(time.RFC3339)},
		"order":        {"created_at.desc"},
		"limit":        {"200"},
	})
	if err != nil {
		log.Printf("[system-email] welcome query failed: %v", err)
		return
	}
	var users []systemUser
	if err := json.Unmarshal(raw, &users); err != nil {
		log.Printf("[system-email] welcome decode failed: %v", err)
		return
	}
	for _, u := range users {
		emailAddr := strings.TrimSpace(u.Email)
		if emailAddr == "" || u.IsAnonymous || u.IsBlocked || u.IsAdmin {
			continue
		}
		key := "welcome:" + u.ID
		claimed, err := s.claimSystemEmail(ctx, key, "welcome", emailAddr)
		if err != nil {
			if s.noteSystemEmailTableError(err) {
				return
			}
			log.Printf("[system-email] welcome claim %s failed: %v", key, err)
			continue
		}
		if !claimed {
			continue
		}
		name := prettyName(u.Name)
		if name == "" {
			name = "there"
		}
		text, body := renderWelcomeEmail(name, emailAddr, s.mail.support)
		if err := s.mail.send(emailAddr, "Welcome to Salt Media", text, body); err != nil {
			log.Printf("[system-email] welcome send to %s failed: %v", emailAddr, err)
			s.releaseSystemEmail(ctx, key)
			continue
		}
		s.finishSystemEmail(ctx, key, "sent", emailAddr, "")
		log.Printf("[system-email] welcome sent to %s", emailAddr)
	}
}

func (s *server) processPaymentReceiptEmails(ctx context.Context, lookback time.Duration) {
	cutoff := time.Now().UTC().Add(-lookback)
	if since := strings.TrimSpace(getenv("CONTRIBUTION_EMAIL_SINCE", "")); since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil && t.After(cutoff) {
			cutoff = t
		}
	}
	limit := getenvInt64("CONTRIBUTION_EMAIL_LIMIT", 200)
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	db, err := s.tsdbDB(ctx)
	if err != nil {
		log.Printf("[system-email] payment receipt query unavailable: %v", err)
		return
	}
	rows, err := queryMaps(db, `
		SELECT payment_uid, provider, external_id, external_ref, payment_id,
		       phone_number, payer_name, payer_email, amount, currency, status,
		       status_reason, narrative, transaction_id, issued_receipt_number,
		       created_at, updated_at
		FROM public.payments
		WHERE status = 'COMPLETED' AND updated_at > $1
		ORDER BY updated_at DESC
		LIMIT $2`, cutoff, limit)
	if err != nil {
		log.Printf("[system-email] payment receipt query failed: %v", err)
		return
	}
	for _, row := range rows {
		paymentUID := stringifyValue(row["payment_uid"])
		if paymentUID == "" {
			continue
		}
		key := "contribution:" + paymentUID
		claimed, err := s.claimSystemEmail(ctx, key, "contribution", "")
		if err != nil {
			if s.noteSystemEmailTableError(err) {
				return
			}
			log.Printf("[system-email] contribution claim %s failed: %v", key, err)
			continue
		}
		if !claimed {
			continue
		}
		if err := s.dispatchPaymentReceiptEmail(ctx, key, row); err != nil {
			log.Printf("[system-email] contribution %s failed: %v", paymentUID, err)
			s.releaseSystemEmail(ctx, key)
		}
	}
}

func (s *server) dispatchPaymentReceiptEmail(ctx context.Context, key string, row map[string]any) error {
	paymentUID := stringifyValue(row["payment_uid"])
	txID := firstNonEmptyString(
		stringifyValue(row["transaction_id"]),
		stringifyValue(row["external_id"]),
		paymentUID,
	)

	uid := ""
	emailAddr := ""
	name := prettyName(stringifyValue(row["payer_name"]))
	if directEmail := strings.TrimSpace(stringifyValue(row["payer_email"])); validEmail(directEmail) {
		// Registered app user's email supplied by the client at initiate (or
		// payer_email from the Yo! IPN). Anonymous users have no email here.
		emailAddr = directEmail
	} else {
		resolvedUID, resolvedEmail, resolvedName, err := s.resolveContributionUser(ctx, map[string]any{
			"name":           stringifyValue(row["payer_name"]),
			"phone_number":   stringifyValue(row["phone_number"]),
			"transaction_id": txID,
		})
		if err != nil {
			return err
		}
		uid = resolvedUID
		emailAddr = resolvedEmail
		if resolvedName != "" {
			name = resolvedName
		}
	}
	if name == "" {
		name = "friend"
	}
	amount := formatUGX(row["amount"])
	reason := prettyName(stringifyValue(row["narrative"]))
	if reason == "" {
		reason = "Salt Media Ministry"
	}
	if txID == "" {
		txID = "N/A"
	}
	date := formatEmailDate(stringifyValue(row["created_at"]))
	phone := stringifyValue(row["phone_number"])
	receiptNo := strings.TrimSpace(stringifyValue(row["issued_receipt_number"]))
	reference := shortReference(txID)
	if receiptNo == "" {
		receiptNo = reference
		reference = ""
	}
	data := contributionEmailData{
		Name:          name,
		Amount:        amount,
		Reason:        reason,
		TransactionID: txID,
		ReceiptNo:     receiptNo,
		Reference:     reference,
		Date:          date,
		Phone:         phone,
		Site:          "https://saltmedia.ug",
		Support:       s.mail.support,
	}
	if emailAddr == "" {
		fallback := strings.TrimSpace(getenv("CONTRIBUTION_FALLBACK_EMAIL", ""))
		if fallback == "" {
			reasonText := "no registered email for payer"
			if uid != "" {
				reasonText = "registered user has no usable email"
			}
			s.finishSystemEmail(ctx, key, "skipped", "", reasonText)
			return nil
		}
		text, body := renderContributionAdminEmail(data)
		if err := s.mail.send(fallback, "Contribution received - "+amount+" (no linked account)", text, body); err != nil {
			return err
		}
		s.finishSystemEmail(ctx, key, "sent", fallback, "")
		log.Printf("[system-email] contribution alert sent to %s (payment %s)", fallback, paymentUID)
		return nil
	}
	text, body := renderContributionEmail(data)
	subject := "Contribution received - " + amount
	if err := s.mail.send(emailAddr, subject, text, body); err != nil {
		return err
	}
	s.finishSystemEmail(ctx, key, "sent", emailAddr, "")
	log.Printf("[system-email] contribution receipt sent to %s (payment %s)", emailAddr, paymentUID)
	return nil
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (s *server) resolveContributionUser(ctx context.Context, row map[string]any) (string, string, string, error) {
	payer := prettyName(stringifyValue(row["name"]))
	phone := stringifyValue(row["phone_number"])
	txID := stringifyValue(row["transaction_id"])
	uid := ""
	token, projectID, tokenErr := s.firebaseAdminToken(ctx)
	if tokenErr != nil {
		log.Printf("[system-email] firestore token unavailable: %v", tokenErr)
	}
	if tokenErr == nil && txID != "" {
		_, fields, err := s.firestoreFindDocument(ctx, token, projectID, "subscriptions", "transactionId", txID)
		if err != nil {
			log.Printf("[system-email] firestore subscription lookup by tx %s: %v", txID, err)
		}
		uid = firestoreUserID(fields)
	}
	if tokenErr == nil && uid == "" && phone != "" {
		for _, v := range phoneVariants(phone) {
			_, fields, err := s.firestoreFindDocument(ctx, token, projectID, "subscriptions", "phoneNumber", v)
			if err != nil {
				continue
			}
			if uid = firestoreUserID(fields); uid != "" {
				break
			}
		}
	}
	if tokenErr == nil && uid == "" && phone != "" {
		for _, v := range phoneVariants(phone) {
			docID, _, err := s.firestoreFindDocument(ctx, token, projectID, "users", "phoneNumber", v)
			if err != nil {
				continue
			}
			if docID != "" {
				uid = docID
				break
			}
		}
	}
	if uid == "" {
		return "", "", payer, nil
	}
	u, err := s.supabaseUserByID(ctx, uid)
	if err != nil {
		return uid, "", payer, err
	}
	if u == nil {
		return uid, "", payer, nil
	}
	if u.Name != "" {
		payer = prettyName(u.Name)
	}
	if strings.TrimSpace(u.Email) == "" || u.IsAnonymous || u.IsBlocked {
		return uid, "", payer, nil
	}
	return uid, strings.TrimSpace(u.Email), payer, nil
}

func (s *server) supabaseUserByID(ctx context.Context, uid string) (*systemUser, error) {
	raw, _, err := s.doRest(ctx, "users", url.Values{
		"select": {"id,email,name,role,is_admin,is_anonymous,is_blocked"},
		"id":     {"eq." + uid},
		"limit":  {"1"},
	})
	if err != nil {
		return nil, err
	}
	var users []systemUser
	if err := json.Unmarshal(raw, &users); err != nil {
		return nil, err
	}
	if len(users) == 0 {
		return nil, nil
	}
	return &users[0], nil
}

func (s *server) firestoreFindDocument(ctx context.Context, token, projectID, collection, field, value string) (string, map[string]any, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil, nil
	}
	payload, _ := json.Marshal(map[string]any{
		"structuredQuery": map[string]any{
			"from": []any{map[string]any{"collectionId": collection}},
			"where": map[string]any{
				"fieldFilter": map[string]any{
					"field": map[string]any{"fieldPath": field},
					"op":    "EQUAL",
					"value": map[string]any{"stringValue": value},
				},
			},
			"limit": 1,
		},
	})
	endpoint := "https://firestore.googleapis.com/v1/projects/" + url.PathEscape(projectID) + "/databases/(default)/documents:runQuery"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return "", nil, fmt.Errorf("firestore query %s.%s: %d %s", collection, field, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var results []struct {
		Document *struct {
			Name   string         `json:"name"`
			Fields map[string]any `json:"fields"`
		} `json:"document"`
	}
	if err := json.Unmarshal(body, &results); err != nil {
		return "", nil, err
	}
	for _, r := range results {
		if r.Document == nil {
			continue
		}
		parts := strings.Split(r.Document.Name, "/")
		return parts[len(parts)-1], r.Document.Fields, nil
	}
	return "", nil, nil
}

func firestoreUserID(fields map[string]any) string {
	if fields == nil {
		return ""
	}
	if v := firestoreString(fields, "userId"); v != "" {
		return v
	}
	if v := firestoreString(fields, "lastActivatedUser"); v != "" {
		return v
	}
	for _, v := range firestoreStrings(fields, "linkedUsers") {
		if v != "" {
			return v
		}
	}
	return ""
}

func firestoreString(fields map[string]any, key string) string {
	v, ok := fields[key].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := v["stringValue"].(string)
	return s
}

func firestoreStrings(fields map[string]any, key string) []string {
	v, ok := fields[key].(map[string]any)
	if !ok {
		return nil
	}
	arr, ok := v["arrayValue"].(map[string]any)
	if !ok {
		return nil
	}
	values, ok := arr["values"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, item := range values {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		s, _ := m["stringValue"].(string)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func phoneVariants(raw string) []string {
	digits := regexp.MustCompile(`\D`).ReplaceAllString(raw, "")
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	add(strings.TrimSpace(raw))
	add(digits)
	if strings.HasPrefix(digits, "256") && len(digits) > 9 {
		local := digits[3:]
		add("+" + digits)
		add("0" + local)
		add(local)
		add("256" + local)
	}
	if strings.HasPrefix(digits, "0") && len(digits) > 9 {
		local := digits[1:]
		add("+256" + local)
		add("256" + local)
		add(local)
	}
	return out
}

func (s *server) claimSystemEmail(ctx context.Context, key, kind, recipient string) (bool, error) {
	payload, _ := json.Marshal([]map[string]any{{
		"event_key": key,
		"kind":      kind,
		"recipient": recipient,
		"status":    "sending",
	}})
	body, _, err := s.restRead(ctx, http.MethodPost, "system_emails", "", payload, "resolution=ignore-duplicates,return=representation")
	if err != nil {
		return false, err
	}
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}

func (s *server) finishSystemEmail(ctx context.Context, key, status, recipient, errMsg string) {
	row := map[string]any{"status": status}
	if recipient != "" {
		row["recipient"] = recipient
	}
	if errMsg != "" {
		row["error"] = errMsg
	}
	if status == "sent" {
		row["sent_at"] = time.Now().UTC().Format(time.RFC3339)
	}
	if err := s.restPatch(ctx, "system_emails", "event_key=eq."+url.QueryEscape(key), row); err != nil {
		log.Printf("[system-email] finish %s failed: %v", key, err)
	}
}

func (s *server) noteSystemEmailTableError(err error) bool {
	msg := err.Error()
	if !strings.Contains(msg, "system_emails") && !strings.Contains(msg, "42P01") && !strings.Contains(msg, "does not exist") {
		return false
	}
	s.emailTableWarnOnce.Do(func() {
		log.Printf("[system-email] system_emails is unavailable; apply migrations/015_system_emails.sql (%v)", err)
	})
	return true
}

func (s *server) releaseSystemEmail(ctx context.Context, key string) {
	if err := s.restDelete(ctx, "system_emails", "event_key=eq."+url.QueryEscape(key)); err != nil {
		log.Printf("[system-email] release %s failed: %v", key, err)
	}
}

func (s *server) handleSendSystemEmail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	if s.mail == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "SMTP is not configured"})
		return
	}
	var body struct {
		To      string `json:"to"`
		Subject string `json:"subject"`
		Text    string `json:"text"`
		HTML    string `json:"html"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	if strings.TrimSpace(body.To) == "" || strings.TrimSpace(body.Subject) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "to and subject are required"})
		return
	}
	text := body.Text
	if text == "" {
		text = stdhtml.UnescapeString(body.HTML)
	}
	htmlBody := body.HTML
	if htmlBody == "" {
		htmlBody = renderEmailHTML(body.Subject, body.Subject, "<p>"+stdhtml.EscapeString(text)+"</p>")
	}
	if err := s.mail.send(body.To, body.Subject, text, htmlBody); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func stringifyValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case json.Number:
		return t.String()
	default:
		return strings.TrimSpace(fmt.Sprint(t))
	}
}

func validEmail(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	parsed, err := mail.ParseAddress(value)
	return err == nil && strings.Contains(parsed.Address, "@")
}

func shortReference(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 14 {
		return value
	}
	return value[:8] + "…" + value[len(value)-6:]
}

func formatEmailDate(raw string) string {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return raw
	}
	return t.UTC().Format("2 Jan 2006, 15:04 UTC")
}

func prettyName(s string) string {
	s = strings.Join(strings.Fields(strings.ReplaceAll(strings.TrimSpace(s), "_", " ")), " ")
	if s == "" {
		return ""
	}
	parts := strings.Fields(s)
	for i, p := range parts {
		r := []rune(p)
		if len(r) > 0 {
			r[0] = unicode.ToUpper(r[0])
		}
		parts[i] = string(r)
	}
	return strings.Join(parts, " ")
}
