package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const momoProvider = "MTNUG_MOMO"

type momoConfig struct {
	BaseURL           string
	TargetEnvironment string
	SubscriptionKey   string
	APIUser           string
	APIKey            string
	CallbackURL       string
}

func momoConfigFromEnv() (momoConfig, error) {
	env := strings.TrimSpace(getenv("MTN_MOMO_ENV", "sandbox"))
	base := "https://proxy.momoapi.mtn.com"
	if strings.EqualFold(env, "sandbox") {
		base = "https://sandbox.momodeveloper.mtn.com"
	}
	cfg := momoConfig{
		BaseURL:           base,
		TargetEnvironment: env,
		SubscriptionKey:   strings.TrimSpace(getenv("MTN_MOMO_SUBSCRIPTION_KEY", "")),
		APIUser:           strings.TrimSpace(getenv("MTN_MOMO_API_USER", "")),
		APIKey:            strings.TrimSpace(getenv("MTN_MOMO_API_KEY", "")),
		CallbackURL:       strings.TrimSpace(getenv("MOMO_CALLBACK_URL", "")),
	}
	if cfg.SubscriptionKey == "" || cfg.APIUser == "" || cfg.APIKey == "" {
		return cfg, fmt.Errorf("missing MTN MoMo configuration (MTN_MOMO_SUBSCRIPTION_KEY, MTN_MOMO_API_USER, MTN_MOMO_API_KEY)")
	}
	return cfg, nil
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (s *server) momoAccessToken(ctx context.Context, cfg momoConfig) (string, error) {
	s.momoMu.Lock()
	if s.momoToken != "" && time.Now().Add(60*time.Second).Before(s.momoTokenExpiry) {
		token := s.momoToken
		s.momoMu.Unlock()
		return token, nil
	}
	s.momoMu.Unlock()

	auth := base64.StdEncoding.EncodeToString([]byte(cfg.APIUser + ":" + cfg.APIKey))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL+"/collection/token/", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Basic "+auth)
	req.Header.Set("X-Reference-Id", newUUID())
	req.Header.Set("Ocp-Apim-Subscription-Key", cfg.SubscriptionKey)
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("MTN token: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("MTN token parse: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return "", fmt.Errorf("MTN token response missing access_token")
	}
	if tokenResp.ExpiresIn <= 0 {
		tokenResp.ExpiresIn = 3600
	}
	s.momoMu.Lock()
	s.momoToken = tokenResp.AccessToken
	s.momoTokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn-300) * time.Second)
	s.momoMu.Unlock()
	return tokenResp.AccessToken, nil
}

func (s *server) momoRequestToPay(ctx context.Context, cfg momoConfig, reference, phone, amount, currency, payerName, narrative string) error {
	token, err := s.momoAccessToken(ctx, cfg)
	if err != nil {
		return err
	}
	callback := cfg.CallbackURL
	if callback != "" {
		sep := "?"
		if strings.Contains(callback, "?") {
			sep = "&"
		}
		callback = callback + sep + "reference=" + url.QueryEscape(reference)
	}
	payload := map[string]any{
		"amount":       amount,
		"currency":     currency,
		"externalId":   reference,
		"payer":        map[string]string{"partyIdType": "MSISDN", "partyId": phone},
		"payerMessage": narrative,
		"payeeNote":    "Contribution",
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL+"/collection/v1_0/requesttopay", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Reference-Id", reference)
	req.Header.Set("X-Target-Environment", cfg.TargetEnvironment)
	req.Header.Set("Ocp-Apim-Subscription-Key", cfg.SubscriptionKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if callback != "" {
		req.Header.Set("X-Callback-Url", callback)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("MTN RequestToPay: %d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

type momoStatusResponse struct {
	Amount                 string          `json:"amount"`
	Currency               string          `json:"currency"`
	FinancialTransactionID string          `json:"financialTransactionId"`
	ExternalID             string          `json:"externalId"`
	Status                 string          `json:"status"`
	Reason                 json.RawMessage `json:"reason"`
	PayerMessage           string          `json:"payerMessage"`
	PayeeNote              string          `json:"payeeNote"`
}

func (s *server) momoStatus(ctx context.Context, cfg momoConfig, reference string) (momoStatusResponse, error) {
	token, err := s.momoAccessToken(ctx, cfg)
	if err != nil {
		return momoStatusResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+"/collection/v1_0/requesttopay/"+url.PathEscape(reference), nil)
	if err != nil {
		return momoStatusResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Target-Environment", cfg.TargetEnvironment)
	req.Header.Set("Ocp-Apim-Subscription-Key", cfg.SubscriptionKey)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return momoStatusResponse{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return momoStatusResponse{}, fmt.Errorf("MTN status: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var status momoStatusResponse
	if err := json.Unmarshal(body, &status); err != nil {
		return momoStatusResponse{}, fmt.Errorf("MTN status parse: %w", err)
	}
	return status, nil
}

func normalizeMomoStatus(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "SUCCESSFUL", "SUCCESS", "COMPLETED":
		return paymentStatusDone
	case "FAILED", "REJECTED", "TIMEOUT", "CANCELLED", "CANCELED":
		return paymentStatusFailed
	default:
		return paymentStatusPending
	}
}

func momoReason(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) == nil {
		if msg, ok := obj["message"].(string); ok && strings.TrimSpace(msg) != "" {
			return strings.TrimSpace(msg)
		}
		if code, ok := obj["code"].(string); ok && strings.TrimSpace(code) != "" {
			return strings.TrimSpace(code)
		}
	}
	return strings.TrimSpace(string(raw))
}

func normalizeMomoPhone(value string) (string, bool) {
	digits := cleanPhoneDigits(value)
	if len(digits) != 12 || !strings.HasPrefix(digits, "256") {
		return "", false
	}
	switch digits[3:5] {
	case "77", "78", "31", "39", "76", "79":
		return digits, true
	}
	return "", false
}

type initiateMomoBody struct {
	Amount      string `json:"amount"`
	Currency    string `json:"currency"`
	PhoneNumber string `json:"phoneNumber"`
	PayerName   string `json:"payerName"`
	PayerEmail  string `json:"payerEmail"`
	Narrative   string `json:"narrative"`
}

type momoStatusBody struct {
	TransactionID string `json:"transactionId"`
}

func (s *server) handleInitiateMomoPayment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	if !s.isServiceKey(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "service key required"})
		return
	}
	var body initiateMomoBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	amount, err := parseAmount(body.Amount)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	phone, ok := normalizeMomoPhone(body.PhoneNumber)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported MTN MoMo number"})
		return
	}
	if allowed, err := s.checkPaymentRateLimit(r.Context(), "momo:initiate:"+clientIP(r), 5); err != nil {
		log.Printf("[momo] rate limit check failed (allowing): %v", err)
	} else if !allowed {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "Too many attempts. Please wait."})
		return
	}
	kyc, err := s.lookupMsisdnKyc(r.Context(), phone)
	if err != nil {
		log.Printf("[momo] prepayment KYC failed: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "Unable to verify this number right now. Please try again."})
		return
	}
	if !kyc.Verified() {
		msg := "This number is not verified for payments."
		switch {
		case kyc.Blacklisted:
			msg = "This number is blocked for payments."
		case kyc.LookupFailed:
			msg = "This number is not registered."
		case kyc.AccessDenied:
			msg = "Unable to verify this number right now. Please try again."
		case !kyc.Registered:
			msg = "This number is not registered."
		}
		writeJSON(w, http.StatusForbidden, map[string]any{"error": msg})
		return
	}
	cfg, err := momoConfigFromEnv()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		return
	}
	if cfg.CallbackURL == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "MOMO_CALLBACK_URL is not configured"})
		return
	}
	currency := strings.TrimSpace(body.Currency)
	if currency == "" {
		currency = "UGX"
	}
	narrative := strings.TrimSpace(body.Narrative)
	if narrative == "" {
		narrative = "Contribution"
	}
	payerName := sanitizeName(body.PayerName)
	if payerName == "" {
		payerName = kyc.Name
	}
	reference := newUUID()
	if err := s.momoRequestToPay(r.Context(), cfg, reference, phone, formatAmount(amount), currency, payerName, narrative); err != nil {
		log.Printf("[momo] RequestToPay failed: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "Mobile money request failed", "details": err.Error()})
		return
	}
	now := time.Now().UTC()
	p := paymentRecord{
		PaymentUID:    paymentUID(momoProvider, reference),
		CreatedAt:     now,
		UpdatedAt:     now,
		Provider:      momoProvider,
		ExternalID:    reference,
		PaymentID:     reference,
		PhoneNumber:   phone,
		PayerName:     payerName,
		PayerEmail:    strings.TrimSpace(body.PayerEmail),
		Amount:        amount,
		Currency:      currency,
		Status:        paymentStatusPending,
		Narrative:     narrative,
		TransactionID: reference,
		Source:        "mesh-momo",
		Raw:           mustJSON(map[string]any{"reference": reference, "status": "PENDING"}),
	}
	if err := s.storePayment(r.Context(), p); err != nil {
		log.Printf("[momo] store initiate failed: %v", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"transactionId": reference,
		"status":        paymentStatusPending,
	})
}

func (s *server) handleCheckMomoPaymentStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	if !s.isServiceKey(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "service key required"})
		return
	}
	var body momoStatusBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	reference := strings.TrimSpace(body.TransactionID)
	if reference == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "transactionId is required"})
		return
	}
	if allowed, err := s.checkPaymentRateLimit(r.Context(), "momo:status:"+clientIP(r), 30); err != nil {
		log.Printf("[momo] status rate limit check failed (allowing): %v", err)
	} else if !allowed {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "Too many attempts. Please wait."})
		return
	}
	cfg, err := momoConfigFromEnv()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		return
	}
	status, err := s.momoStatus(r.Context(), cfg, reference)
	if err != nil {
		log.Printf("[momo] status failed: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "Failed to check payment status", "details": err.Error()})
		return
	}
	normalized := normalizeMomoStatus(status.Status)
	reason := momoReason(status.Reason)
	now := time.Now().UTC()
	p := paymentRecord{
		PaymentUID:    paymentUID(momoProvider, reference),
		CreatedAt:     now,
		UpdatedAt:     now,
		Provider:      momoProvider,
		ExternalID:    reference,
		PaymentID:     reference,
		Amount:        parseAmountQuiet(status.Amount),
		Currency:      defaultString(status.Currency, "UGX"),
		Status:        normalized,
		StatusReason:  reason,
		TransactionID: reference,
		NetworkRef:    status.FinancialTransactionID,
		Source:        "mesh-momo",
		Raw:           mustJSON(status),
	}
	if err := s.storePayment(r.Context(), p); err != nil {
		log.Printf("[momo] store status failed: %v", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"transactionId":          reference,
		"status":                 status.Status,
		"transactionStatus":      status.Status,
		"paymentStatus":          normalized,
		"reason":                 reason,
		"financialTransactionId": status.FinancialTransactionID,
		"amount":                 status.Amount,
		"currency":               status.Currency,
	})
}

func momoCallbackReference(r *http.Request) string {
	for _, key := range []string{"reference", "ref", "externalId", "transactionId"} {
		if value := strings.TrimSpace(r.URL.Query().Get(key)); value != "" {
			return value
		}
	}
	_ = r.ParseForm()
	for _, key := range []string{"externalId", "reference", "transactionId"} {
		if value := strings.TrimSpace(r.FormValue(key)); value != "" {
			return value
		}
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if len(raw) > 0 {
		var body map[string]any
		if json.Unmarshal(raw, &body) == nil {
			for _, key := range []string{"externalId", "reference", "transactionId"} {
				if value, ok := body[key].(string); ok && strings.TrimSpace(value) != "" {
					return strings.TrimSpace(value)
				}
			}
		}
	}
	return ""
}

func (s *server) handleMomoCallback(w http.ResponseWriter, r *http.Request) {
	if allowed, err := s.checkPaymentRateLimit(r.Context(), "momo:callback:"+clientIP(r), 120); err != nil {
		log.Printf("[momo] callback rate limit check failed (allowing): %v", err)
	} else if !allowed {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	reference := momoCallbackReference(r)
	if reference == "" {
		log.Printf("[momo] callback without reference (%s %s)", r.Method, r.URL.Path)
		_, _ = w.Write([]byte("OK"))
		return
	}
	cfg, err := momoConfigFromEnv()
	if err != nil {
		log.Printf("[momo] callback config error: %v", err)
		_, _ = w.Write([]byte("OK"))
		return
	}
	status, err := s.momoStatus(r.Context(), cfg, reference)
	if err != nil {
		log.Printf("[momo] callback status lookup failed for %s: %v", reference, err)
		_, _ = w.Write([]byte("OK"))
		return
	}
	now := time.Now().UTC()
	reason := momoReason(status.Reason)
	p := paymentRecord{
		PaymentUID:    paymentUID(momoProvider, reference),
		CreatedAt:     now,
		UpdatedAt:     now,
		Provider:      momoProvider,
		ExternalID:    reference,
		PaymentID:     reference,
		Amount:        parseAmountQuiet(status.Amount),
		Currency:      defaultString(status.Currency, "UGX"),
		Status:        normalizeMomoStatus(status.Status),
		StatusReason:  reason,
		TransactionID: reference,
		NetworkRef:    status.FinancialTransactionID,
		Source:        "mesh-momo-callback",
		Raw:           mustJSON(status),
	}
	if err := s.storePayment(r.Context(), p); err != nil {
		log.Printf("[momo] callback store failed: %v", err)
	}
	log.Printf("[momo] callback %s status=%s", reference, status.Status)
	_, _ = w.Write([]byte("OK"))
}
