package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	paymentProviderYo    = "YO"
	paymentStatusPending = "PENDING"
	paymentStatusDone    = "COMPLETED"
	paymentStatusFailed  = "FAILED"

	yoInstantLimitPerMin = 5
	yoStatusLimitPerMin  = 30
)

var nonDigits = regexp.MustCompile(`\D`)

type yoConfig struct {
	APIURL     string
	Username   string
	Password   string
	IPNURL     string
	FailureURL string
}

type yoAutoCreateRequest struct {
	XMLName xml.Name  `xml:"AutoCreate"`
	Request yoRequest `xml:"Request"`
}

type yoRequest struct {
	APIUsername                 string `xml:"APIUsername"`
	APIPassword                 string `xml:"APIPassword"`
	Method                      string `xml:"Method"`
	NonBlocking                 string `xml:"NonBlocking,omitempty"`
	Amount                      string `xml:"Amount,omitempty"`
	Account                     string `xml:"Account,omitempty"`
	Narrative                   string `xml:"Narrative,omitempty"`
	InstantNotificationUrl      string `xml:"InstantNotificationUrl,omitempty"`
	FailureNotificationUrl      string `xml:"FailureNotificationUrl,omitempty"`
	ProviderReferenceText       string `xml:"ProviderReferenceText,omitempty"`
	ExternalReference           string `xml:"ExternalReference,omitempty"`
	TransactionReference        string `xml:"TransactionReference,omitempty"`
	PrivateTransactionReference string `xml:"PrivateTransactionReference,omitempty"`
	DepositTransactionType      string `xml:"DepositTransactionType,omitempty"`
	Msisdn                      string `xml:"Msisdn,omitempty"`
}

type yoAutoCreateResponse struct {
	XMLName  xml.Name   `xml:"AutoCreate"`
	Response yoResponse `xml:"Response"`
}

type yoResponse struct {
	Status                      string `xml:"Status"`
	StatusCode                  string `xml:"StatusCode"`
	StatusMessage               string `xml:"StatusMessage"`
	TransactionReference        string `xml:"TransactionReference"`
	TransactionStatus           string `xml:"TransactionStatus"`
	PrivateTransactionReference string `xml:"PrivateTransactionReference"`
	Amount                      string `xml:"Amount"`
	AmountFormatted             string `xml:"AmountFormatted"`
	CurrencyCode                string `xml:"CurrencyCode"`
	TransactionInitiationDate   string `xml:"TransactionInitiationDate"`
	TransactionCompletionDate   string `xml:"TransactionCompletionDate"`
	IssuedReceiptNumber         string `xml:"IssuedReceiptNumber"`
	NetworkRef                  string `xml:"NetworkRef"`
}

type paymentRecord struct {
	PaymentUID          string          `json:"payment_uid"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
	Provider            string          `json:"provider"`
	ExternalID          string          `json:"external_id"`
	ExternalRef         string          `json:"external_ref,omitempty"`
	PaymentID           string          `json:"payment_id,omitempty"`
	PhoneNumber         string          `json:"phone_number,omitempty"`
	PayerName           string          `json:"payer_name,omitempty"`
	PayerEmail          string          `json:"payer_email,omitempty"`
	Amount              float64         `json:"amount"`
	Currency            string          `json:"currency"`
	Status              string          `json:"status"`
	StatusReason        string          `json:"status_reason,omitempty"`
	Narrative           string          `json:"narrative,omitempty"`
	TransactionID       string          `json:"transaction_id,omitempty"`
	IssuedReceiptNumber string          `json:"issued_receipt_number,omitempty"`
	NetworkRef          string          `json:"network_ref,omitempty"`
	Source              string          `json:"source,omitempty"`
	Raw                 json.RawMessage `json:"raw,omitempty"`
}

type kycResult struct {
	Success         bool   `json:"success"`
	Registered      bool   `json:"registered"`
	Blacklisted     bool   `json:"blacklisted"`
	LookupFailed    bool   `json:"lookupFailed"`
	AccessDenied    bool   `json:"accessDenied"`
	Status          string `json:"status"`
	Code            string `json:"code"`
	Message         string `json:"message"`
	Name            string `json:"name"`
	FirstName       string `json:"firstName"`
	MiddleName      string `json:"middleName"`
	Surname         string `json:"surname"`
	Gender          string `json:"gender"`
	DateOfBirth     string `json:"dateOfBirth"`
	BlacklistStatus string `json:"blacklistStatus"`
}

func (r kycResult) Verified() bool {
	return r.Registered && !r.Blacklisted && !r.LookupFailed && !r.AccessDenied
}

type kycCacheEntry struct {
	result    kycResult
	expiresAt time.Time
}

type initiateYoDepositBody struct {
	Amount      string `json:"amount"`
	PhoneNumber string `json:"phoneNumber"`
	ExternalRef string `json:"externalRef"`
	Narrative   string `json:"narrative"`
	PayerName   string `json:"payerName"`
	PayerEmail  string `json:"payerEmail"`
}

type checkYoStatusBody struct {
	TransactionReference        string `json:"transactionReference"`
	PrivateTransactionReference string `json:"privateTransactionReference"`
	ExternalRef                 string `json:"externalRef"`
	Amount                      string `json:"amount"`
	PhoneNumber                 string `json:"phoneNumber"`
	Narrative                   string `json:"narrative"`
	PayerName                   string `json:"payerName"`
	PayerEmail                  string `json:"payerEmail"`
	StatusReason                string `json:"statusReason"`
}

func yoConfigFromEnv() (yoConfig, error) {
	cfg := yoConfig{
		APIURL:     strings.TrimSpace(os.Getenv("YO_API_URL")),
		Username:   strings.TrimSpace(os.Getenv("YO_API_USERNAME")),
		Password:   strings.TrimSpace(os.Getenv("YO_API_PASSWORD")),
		IPNURL:     strings.TrimSpace(os.Getenv("YO_IPN_URL")),
		FailureURL: strings.TrimSpace(os.Getenv("YO_FAILURE_URL")),
	}
	if cfg.APIURL == "" || cfg.Username == "" || cfg.Password == "" {
		return cfg, errors.New("missing Yo! configuration (YO_API_URL, YO_API_USERNAME, YO_API_PASSWORD)")
	}
	return cfg, nil
}

// yoKycConfigFromEnv uses the dedicated Yo! KYC API/credentials when provided.
// Yo! support directs KYC calls to the sandbox KYC API even for production
// payments; payments/status keep using the production YO_API_* values.
func yoKycConfigFromEnv() (yoConfig, error) {
	cfg := yoConfig{
		APIURL:   strings.TrimSpace(os.Getenv("YO_KYC_API_URL")),
		Username: strings.TrimSpace(os.Getenv("YO_KYC_API_USERNAME")),
		Password: strings.TrimSpace(os.Getenv("YO_KYC_API_PASSWORD")),
	}
	if cfg.APIURL == "" || cfg.Username == "" || cfg.Password == "" {
		return yoConfigFromEnv()
	}
	return cfg, nil
}

func (s *server) callYoXML(ctx context.Context, cfg yoConfig, req yoRequest) ([]byte, error) {
	payload := yoAutoCreateRequest{Request: req}
	body, err := xml.Marshal(payload)
	if err != nil {
		return nil, err
	}
	body = append([]byte(xml.Header), body...)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.APIURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "text/xml")
	httpReq.Header.Set("Content-transfer-encoding", "text")

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("Yo! HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

func (s *server) callYo(ctx context.Context, cfg yoConfig, req yoRequest) (yoResponse, error) {
	raw, err := s.callYoXML(ctx, cfg, req)
	if err != nil {
		return yoResponse{}, err
	}
	var parsed yoAutoCreateResponse
	if err := xml.Unmarshal(raw, &parsed); err != nil {
		return yoResponse{}, fmt.Errorf("parse Yo! response: %w", err)
	}
	return parsed.Response, nil
}

func (s *server) handleInitiateYoDeposit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	if !s.isServiceKey(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "service key required"})
		return
	}
	var body initiateYoDepositBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}

	amount, err := parseAmount(body.Amount)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	phone, ok := normalizeYoPhone(body.PhoneNumber)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported phone number"})
		return
	}
	if allowed, err := s.checkPaymentRateLimit(r.Context(), "yo:initiate:"+clientIP(r), yoInstantLimitPerMin); err != nil {
		log.Printf("[payments] rate limit check failed (allowing): %v", err)
	} else if !allowed {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"status": "Error", "reason": "Too many attempts. Please wait."})
		return
	}

	// Server-side KYC gate: do not create a Yo! deposit for an unverified or
	// blocked number, even if the client bypasses the app's KYC step.
	kyc, err := s.lookupMsisdnKyc(r.Context(), phone)
	if err != nil {
		log.Printf("[payments] prepayment KYC failed: %v", err)
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
	if strings.TrimSpace(body.PayerName) == "" {
		body.PayerName = kyc.Name
	}

	cfg, err := yoConfigFromEnv()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		return
	}
	narrative := strings.TrimSpace(body.Narrative)
	if len(narrative) > 200 {
		narrative = narrative[:200]
	}
	referenceText := narrative
	if referenceText == "" {
		referenceText = "contribution"
	}
	externalRef := strings.TrimSpace(body.ExternalRef)

	resp, err := s.callYo(r.Context(), cfg, yoRequest{
		APIUsername:            cfg.Username,
		APIPassword:            cfg.Password,
		Method:                 "acdepositfunds",
		NonBlocking:            "TRUE",
		Amount:                 formatAmount(amount),
		Account:                phone,
		Narrative:              narrative,
		InstantNotificationUrl: cfg.IPNURL,
		FailureNotificationUrl: cfg.FailureURL,
		ProviderReferenceText:  "Your " + referenceText + " has been received. Thanks for your contribution towards GOD's ministry",
		ExternalReference:      externalRef,
	})
	if err != nil {
		log.Printf("[payments] Yo initiate error: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "Deposit failed", "details": err.Error()})
		return
	}
	if strings.EqualFold(resp.Status, "ERROR") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": resp.StatusMessage, "code": resp.StatusCode})
		return
	}
	txRef := strings.TrimSpace(resp.TransactionReference)
	if txRef == "" {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "Yo! did not return a transaction reference"})
		return
	}

	now := time.Now().UTC()
	p := paymentRecord{
		PaymentUID:    paymentUID(paymentProviderYo, txRef),
		CreatedAt:     now,
		UpdatedAt:     now,
		Provider:      paymentProviderYo,
		ExternalID:    txRef,
		ExternalRef:   externalRef,
		PaymentID:     txRef,
		PhoneNumber:   phone,
		PayerName:     sanitizeName(body.PayerName),
		PayerEmail:    strings.TrimSpace(body.PayerEmail),
		Amount:        amount,
		Currency:      "UGX",
		Status:        normalizePaymentStatus(resp.TransactionStatus, resp.Status),
		StatusReason:  resp.StatusMessage,
		Narrative:     narrative,
		TransactionID: txRef,
		NetworkRef:    resp.NetworkRef,
		Source:        "mesh",
		Raw:           mustJSON(resp),
	}
	if err := s.storePayment(r.Context(), p); err != nil {
		log.Printf("[payments] store initiate failed: %v", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"transactionRef": txRef,
		"status":         p.Status,
	})
}

func (s *server) handleCheckYoTransactionStatus(w http.ResponseWriter, r *http.Request) {
	s.handleYoStatusLookup(w, r, true)
}

// handleRecordYoPayment is the service-only bridge for the legacy Firebase
// functions (IPN/failure/status polling). It re-queries Yo! itself so the mesh
// never trusts an IPN payload.
func (s *server) handleRecordYoPayment(w http.ResponseWriter, r *http.Request) {
	s.handleYoStatusLookup(w, r, false)
}

func (s *server) handleYoStatusLookup(w http.ResponseWriter, r *http.Request, rateLimited bool) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	if !s.isServiceKey(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "service key required"})
		return
	}
	var body checkYoStatusBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	body.TransactionReference = strings.TrimSpace(body.TransactionReference)
	body.PrivateTransactionReference = strings.TrimSpace(body.PrivateTransactionReference)
	body.ExternalRef = strings.TrimSpace(body.ExternalRef)
	if body.PrivateTransactionReference == "" {
		body.PrivateTransactionReference = body.ExternalRef
	}
	if body.TransactionReference == "" && body.PrivateTransactionReference == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "transactionReference or privateTransactionReference is required"})
		return
	}
	if rateLimited {
		if allowed, err := s.checkPaymentRateLimit(r.Context(), "yo:status:"+clientIP(r), yoStatusLimitPerMin); err != nil {
			log.Printf("[payments] status rate limit check failed (allowing): %v", err)
		} else if !allowed {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"status": "Error", "reason": "Too many attempts. Please wait."})
			return
		}
	}

	resp, err := s.lookupYoStatus(r.Context(), body)
	if err != nil {
		log.Printf("[payments] Yo status error: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "Transaction status check failed", "details": err.Error()})
		return
	}
	if strings.EqualFold(resp.Status, "ERROR") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": resp.StatusMessage, "code": resp.StatusCode})
		return
	}
	s.storeYoStatus(r.Context(), body, resp)

	writeJSON(w, http.StatusOK, map[string]any{
		"status":                      resp.Status,
		"transactionStatus":           resp.TransactionStatus,
		"statusMessage":               resp.StatusMessage,
		"transactionReference":        resp.TransactionReference,
		"privateTransactionReference": resp.PrivateTransactionReference,
		"amount":                      resp.Amount,
		"amountFormatted":             resp.AmountFormatted,
		"currencyCode":                resp.CurrencyCode,
		"transactionInitiationDate":   resp.TransactionInitiationDate,
		"transactionCompletionDate":   resp.TransactionCompletionDate,
		"issuedReceiptNumber":         resp.IssuedReceiptNumber,
	})
}

func (s *server) lookupYoStatus(ctx context.Context, body checkYoStatusBody) (yoResponse, error) {
	cfg, err := yoConfigFromEnv()
	if err != nil {
		return yoResponse{}, err
	}
	return s.callYo(ctx, cfg, yoRequest{
		APIUsername:                 cfg.Username,
		APIPassword:                 cfg.Password,
		Method:                      "actransactioncheckstatus",
		TransactionReference:        body.TransactionReference,
		PrivateTransactionReference: body.PrivateTransactionReference,
	})
}

func (s *server) storeYoStatus(ctx context.Context, body checkYoStatusBody, resp yoResponse) paymentRecord {
	externalID := strings.TrimSpace(resp.TransactionReference)
	if externalID == "" {
		externalID = strings.TrimSpace(body.TransactionReference)
	}
	if externalID == "" {
		externalID = strings.TrimSpace(body.PrivateTransactionReference)
	}
	now := time.Now().UTC()
	reason := strings.TrimSpace(resp.StatusMessage)
	if reason == "" {
		reason = strings.TrimSpace(body.StatusReason)
	}
	p := paymentRecord{
		PaymentUID:          paymentUID(paymentProviderYo, externalID),
		CreatedAt:           parseYoDate(resp.TransactionInitiationDate, now),
		UpdatedAt:           now,
		Provider:            paymentProviderYo,
		ExternalID:          externalID,
		ExternalRef:         strings.TrimSpace(body.PrivateTransactionReference),
		PaymentID:           externalID,
		PhoneNumber:         cleanPhoneDigits(body.PhoneNumber),
		PayerName:           sanitizeName(body.PayerName),
		PayerEmail:          strings.TrimSpace(body.PayerEmail),
		Amount:              parseAmountQuiet(resp.Amount),
		Currency:            defaultString(resp.CurrencyCode, "UGX"),
		Status:              normalizePaymentStatus(resp.TransactionStatus, resp.Status),
		StatusReason:        reason,
		Narrative:           strings.TrimSpace(body.Narrative),
		TransactionID:       externalID,
		IssuedReceiptNumber: resp.IssuedReceiptNumber,
		NetworkRef:          resp.NetworkRef,
		Source:              "mesh",
		Raw:                 mustJSON(resp),
	}
	if p.Amount <= 0 {
		p.Amount = parseAmountQuiet(body.Amount)
	}
	if err := s.storePayment(ctx, p); err != nil {
		log.Printf("[payments] store status failed: %v", err)
	}
	return p
}

// handleYoWebhook is the future IPN/failure target, kept ready so switching
// the Yo! notification URLs is a config change. It is public (Yo! cannot send
// an API key) but always re-queries Yo! before writing.
func (s *server) handleYoWebhook(w http.ResponseWriter, r *http.Request, failure bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if allowed, err := s.checkPaymentRateLimit(r.Context(), "yo:ipn:"+clientIP(r), 120); err != nil {
		log.Printf("[payments] ipn rate limit check failed (allowing): %v", err)
	} else if !allowed {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	_ = r.ParseForm()
	redacted := url.Values{}
	sensitiveFormKeys := map[string]bool{
		"msisdn": true, "payer_email": true, "payer_names": true,
		"signature": true, "external_ref": true, "network_ref": true,
	}
	for key, values := range r.PostForm {
		for _, value := range values {
			if sensitiveFormKeys[key] {
				redacted.Add(key, "[redacted]")
			} else {
				redacted.Add(key, value)
			}
		}
	}
	kind := "ipn"
	if failure {
		kind = "failure"
	}
	log.Printf("[payments] yo %s fields: %s", kind, redacted.Encode())
	body := checkYoStatusBody{
		TransactionReference:        firstFormValue(r, "transaction_reference", "transactionReference"),
		PrivateTransactionReference: firstFormValue(r, "private_transaction_reference", "privateTransactionReference"),
		ExternalRef:                 firstFormValue(r, "external_ref", "externalRef"),
		Amount:                      firstFormValue(r, "amount"),
		PhoneNumber:                 firstFormValue(r, "msisdn", "phone_number", "phoneNumber"),
		Narrative:                   firstFormValue(r, "narrative"),
		PayerName:                   firstFormValue(r, "payer_names", "payerName"),
		PayerEmail:                  firstFormValue(r, "payer_email", "payerEmail"),
	}
	if failure && body.TransactionReference == "" {
		body.TransactionReference = firstFormValue(r, "failed_transaction_reference", "failedTransactionReference")
	}
	if body.PrivateTransactionReference == "" {
		body.PrivateTransactionReference = body.ExternalRef
	}
	if body.TransactionReference == "" && body.PrivateTransactionReference != "" {
		if ref := s.lookupPaymentTransactionRef(r.Context(), body.PrivateTransactionReference); ref != "" {
			body.TransactionReference = ref
			body.PrivateTransactionReference = ""
		}
	}
	if body.TransactionReference == "" && body.PrivateTransactionReference == "" {
		http.Error(w, "missing transaction reference", http.StatusBadRequest)
		return
	}
	resp, err := s.lookupYoStatus(r.Context(), body)
	if err != nil {
		log.Printf("[payments] Yo webhook lookup error: %v", err)
		http.Error(w, "status lookup failed", http.StatusInternalServerError)
		return
	}
	if strings.EqualFold(resp.Status, "ERROR") {
		http.Error(w, resp.StatusMessage, http.StatusBadRequest)
		return
	}
	s.storeYoStatus(r.Context(), body, resp)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if failure {
		_, _ = w.Write([]byte("OK"))
		return
	}
	_, _ = w.Write([]byte("narrative=Payment+received+successfully"))
}

func firstFormValue(r *http.Request, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(r.FormValue(key)); value != "" {
			return value
		}
	}
	return ""
}

// lookupPaymentTransactionRef maps an IPN external reference to the Yo!
// TransactionReference recorded at initiate. Future mesh IPNs can then verify
// the transaction without relying on Yo! accepting the external ref as a
// PrivateTransactionReference.
func (s *server) lookupPaymentTransactionRef(ctx context.Context, externalRef string) string {
	externalRef = strings.TrimSpace(externalRef)
	if externalRef == "" {
		return ""
	}
	db, err := s.tsdbDB(ctx)
	if err != nil {
		return ""
	}
	var ref string
	if err := db.QueryRowContext(ctx, `
		SELECT COALESCE(NULLIF(transaction_id, ''), external_id)
		FROM public.payments
		WHERE provider = 'YO' AND external_ref = $1
		ORDER BY created_at DESC
		LIMIT 1`, externalRef).Scan(&ref); err != nil {
		return ""
	}
	return strings.TrimSpace(ref)
}

type msisdnKycBody struct {
	PhoneNumber string `json:"phoneNumber"`
}

// handleGetMsisdnKycInfo wraps Yo! acgetmsisdnkycinfo (section 14 of API 3.48).
// The method is access-controlled by Yo!: this account currently gets
// StatusCode -9999 ("not permitted"), so the app-side call stays commented out
// until Yo enables it. The endpoint is safe to leave live for that cutover.
func (s *server) handleGetMsisdnKycInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	if !s.isServiceKey(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "service key required"})
		return
	}
	var body msisdnKycBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	phone, ok := normalizeUgandaMsisdn(body.PhoneNumber)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid phone number"})
		return
	}
	if allowed, err := s.checkPaymentRateLimit(r.Context(), "yo:kyc:"+clientIP(r), 10); err != nil {
		log.Printf("[payments] kyc rate limit check failed (allowing): %v", err)
	} else if !allowed {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"status": "Error", "message": "Too many verification attempts. Please wait."})
		return
	}
	result, err := s.lookupMsisdnKyc(r.Context(), phone)
	if err != nil {
		log.Printf("[payments] Yo KYC error: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "KYC lookup failed", "details": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// lookupMsisdnKyc calls Yo! KYC once and caches the result briefly. The app's
// pre-payment KYC call populates this cache; the initiate endpoint reuses it so
// a verified number does not hit Yo! twice. A caller that bypasses the app
// still gets a server-side KYC check here.
func (s *server) lookupMsisdnKyc(ctx context.Context, phone string) (kycResult, error) {
	phone = strings.TrimSpace(phone)
	ttl := time.Duration(getenvInt64("YO_KYC_CACHE_TTL_SECONDS", 600)) * time.Second
	now := time.Now()

	s.kycMu.Lock()
	if entry, ok := s.kycCache[phone]; ok && now.Before(entry.expiresAt) {
		s.kycMu.Unlock()
		return entry.result, nil
	}
	s.kycMu.Unlock()

	cfg, err := yoKycConfigFromEnv()
	if err != nil {
		return kycResult{}, err
	}
	raw, err := s.callYoXML(ctx, cfg, yoRequest{
		APIUsername: cfg.Username,
		APIPassword: cfg.Password,
		Method:      "acgetmsisdnkycinfo",
		Msisdn:      phone,
	})
	if err != nil {
		return kycResult{}, err
	}
	result := parseKycResult(raw)
	if ttl > 0 {
		s.kycMu.Lock()
		if s.kycCache == nil {
			s.kycCache = map[string]kycCacheEntry{}
		}
		s.kycCache[phone] = kycCacheEntry{result: result, expiresAt: now.Add(ttl)}
		s.kycMu.Unlock()
	}
	return result, nil
}

func parseKycResult(raw []byte) kycResult {
	status := xmlTagValue(raw, "Status")
	code := xmlTagValue(raw, "StatusCode")
	message := xmlTagValue(raw, "StatusMessage")
	firstName := xmlTagValue(raw, "FirstName")
	middleName := xmlTagValue(raw, "MiddleName")
	surname := xmlTagValue(raw, "Surname")
	gender := xmlTagValue(raw, "Gender")
	dob := xmlTagValue(raw, "Date")
	blacklist := xmlTagValue(raw, "BlacklistStatus")
	name := strings.TrimSpace(strings.Join(nonEmptyStrings(firstName, middleName, surname), " "))
	registered := strings.EqualFold(status, "OK") &&
		(name != "" || strings.Contains(string(raw), "AccountInformation"))
	lowerMessage := strings.ToLower(message)
	blacklisted := strings.EqualFold(blacklist, "BLOCKED")
	accessDenied := code == "-9999" &&
		(strings.Contains(lowerMessage, "not permitted") ||
			strings.Contains(lowerMessage, "access to"))
	lookupFailed := !strings.EqualFold(status, "OK") && !accessDenied

	return kycResult{
		Success:         strings.EqualFold(status, "OK"),
		Registered:      registered,
		Blacklisted:     blacklisted,
		LookupFailed:    lookupFailed,
		AccessDenied:    accessDenied,
		Status:          status,
		Code:            code,
		Message:         message,
		Name:            name,
		FirstName:       firstName,
		MiddleName:      middleName,
		Surname:         surname,
		Gender:          gender,
		DateOfBirth:     dob,
		BlacklistStatus: blacklist,
	}
}

func (s *server) cleanupKycCache() {
	s.kycMu.Lock()
	defer s.kycMu.Unlock()
	now := time.Now()
	for key, entry := range s.kycCache {
		if now.After(entry.expiresAt) {
			delete(s.kycCache, key)
		}
	}
}

func xmlTagValue(raw []byte, tag string) string {
	pattern := `(?s)<` + regexp.QuoteMeta(tag) + `[^>]*>(.*?)</` + regexp.QuoteMeta(tag) + `>`
	match := regexp.MustCompile(pattern).FindSubmatch(raw)
	if len(match) < 2 {
		return ""
	}
	value := regexp.MustCompile(`<[^>]+>`).ReplaceAllString(string(match[1]), "")
	return strings.TrimSpace(html.UnescapeString(value))
}

func nonEmptyStrings(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, strings.TrimSpace(value))
		}
	}
	return out
}

func (s *server) handleGetPaymentMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	if !s.isServiceKey(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "service key required"})
		return
	}
	days := atoiDefault(r.URL.Query().Get("days"), 30)
	if days < 1 {
		days = 30
	}
	if days > 366 {
		days = 366
	}

	db, err := s.tsdbDB(r.Context())
	if err != nil {
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "payment analytics unavailable"})
		return
	}

	daily, err := queryMaps(db, `
		SELECT to_char(time_bucket('1 day', created_at), 'YYYY-MM-DD') AS date,
		       count(*)::bigint AS count,
		       coalesce(sum(amount), 0)::double precision AS total,
		       count(*) FILTER (WHERE status = 'COMPLETED')::bigint AS completed,
		       count(*) FILTER (WHERE status = 'FAILED')::bigint AS failed,
		       count(*) FILTER (WHERE status = 'PENDING')::bigint AS pending
		FROM public.payments
		WHERE created_at > now() - make_interval(days => $1)
		GROUP BY 1 ORDER BY 1 DESC`, days)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	byProvider, err := queryMaps(db, `
		SELECT provider, status,
		       count(*)::bigint AS count,
		       coalesce(sum(amount), 0)::double precision AS total
		FROM public.payments
		WHERE created_at > now() - make_interval(days => $1)
		GROUP BY 1, 2 ORDER BY total DESC`, days)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	topContributors, err := queryMaps(db, `
		SELECT coalesce(payer_name, '') AS payer_name,
		       coalesce(phone_number, '') AS phone_number,
		       count(*)::bigint AS contributions,
		       coalesce(sum(amount), 0)::double precision AS total_given,
		       max(created_at) AS last_contribution
		FROM public.payments
		WHERE status = 'COMPLETED' AND created_at > now() - make_interval(days => $1)
		  AND (coalesce(phone_number, '') <> '' OR coalesce(payer_name, '') <> '')
		GROUP BY 1, 2 ORDER BY total_given DESC LIMIT 100`, days)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	summaryRows, err := queryMaps(db, `
		SELECT count(*)::bigint AS total_transactions,
		       count(*) FILTER (WHERE status = 'COMPLETED')::bigint AS successful,
		       count(*) FILTER (WHERE status = 'FAILED')::bigint AS failed,
		       count(*) FILTER (WHERE status = 'PENDING')::bigint AS pending,
		       coalesce(sum(CASE WHEN status = 'COMPLETED' THEN amount ELSE 0 END), 0)::double precision AS total_revenue,
		       coalesce(avg(CASE WHEN status = 'COMPLETED' THEN amount END), 0)::double precision AS avg_contribution,
		       count(DISTINCT NULLIF(phone_number, ''))::bigint AS unique_contributors
		FROM public.payments
		WHERE created_at > now() - make_interval(days => $1)`, days)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	summary := map[string]any{}
	if len(summaryRows) > 0 {
		summary = summaryRows[0]
	}

	byNarrative, err := queryMaps(db, `
		SELECT coalesce(narrative, '') AS narrative,
		       count(*)::bigint AS count,
		       coalesce(sum(amount), 0)::double precision AS total
		FROM public.payments
		WHERE status = 'COMPLETED' AND created_at > now() - make_interval(days => $1)
		GROUP BY 1 ORDER BY total DESC`, days)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"daily":           nonNilMaps(daily),
		"byProvider":      nonNilMaps(byProvider),
		"topContributors": nonNilMaps(topContributors),
		"summary":         summary,
		"byNarrative":     nonNilMaps(byNarrative),
	})
}

// storePayment writes to Timescale; when it is unreachable the durable outbox
// keeps the payment for replay so a QNAP outage never loses the row.
func (s *server) storePayment(ctx context.Context, p paymentRecord) error {
	if p.PaymentUID == "" {
		p.PaymentUID = paymentUID(p.Provider, p.ExternalID)
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = p.CreatedAt
	}
	if p.Currency == "" {
		p.Currency = "UGX"
	}
	if p.Status == "" {
		p.Status = paymentStatusPending
	}

	err := s.upsertPaymentTSDB(ctx, p)
	if err == nil {
		return nil
	}
	if errors.Is(err, errTsdbUnavailable) && s.outbox != nil {
		payload := paymentJob{Payment: p, ReceivedAt: time.Now().UTC()}
		if _, qerr := s.outbox.Enqueue(outboxKindPayment, paymentDedupeKey(p), payload, payload.ReceivedAt); qerr == nil {
			log.Printf("[payments] queued payment %s in outbox (tsdb unavailable)", p.PaymentUID)
			return nil
		} else {
			return fmt.Errorf("tsdb unavailable; outbox enqueue failed: %w", qerr)
		}
	}
	return err
}

func (s *server) upsertPaymentTSDB(ctx context.Context, p paymentRecord) error {
	db, err := s.tsdbDB(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", errTsdbUnavailable, err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", errTsdbUnavailable, err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`, p.PaymentUID); err != nil {
		return err
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE public.payments SET
			updated_at = now(),
			external_ref = COALESCE(NULLIF($2, ''), external_ref),
			payment_id = COALESCE(NULLIF($3, ''), payment_id),
			phone_number = COALESCE(NULLIF($4, ''), phone_number),
			payer_name = COALESCE(NULLIF($5, ''), payer_name),
			amount = CASE WHEN $6 > 0 THEN $6 ELSE amount END,
			currency = COALESCE(NULLIF($7, ''), currency),
			status = $8,
			status_reason = COALESCE(NULLIF($9, ''), status_reason),
			narrative = COALESCE(NULLIF($10, ''), narrative),
			transaction_id = COALESCE(NULLIF($11, ''), transaction_id),
			issued_receipt_number = COALESCE(NULLIF($12, ''), issued_receipt_number),
			network_ref = COALESCE(NULLIF($13, ''), network_ref),
			source = COALESCE(NULLIF($14, ''), source),
			payer_email = COALESCE(NULLIF($15, ''), payer_email),
			raw = COALESCE($16, raw)
		WHERE payment_uid = $1`,
		p.PaymentUID,
		nullIfEmpty(p.ExternalRef),
		nullIfEmpty(p.PaymentID),
		nullIfEmpty(p.PhoneNumber),
		nullIfEmpty(p.PayerName),
		p.Amount,
		p.Currency,
		p.Status,
		nullIfEmpty(p.StatusReason),
		nullIfEmpty(p.Narrative),
		nullIfEmpty(p.TransactionID),
		nullIfEmpty(p.IssuedReceiptNumber),
		nullIfEmpty(p.NetworkRef),
		nullIfEmpty(p.Source),
		nullIfEmpty(p.PayerEmail),
		rawOrNil(p.Raw),
	)
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected > 0 {
		return tx.Commit()
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO public.payments (
			payment_uid, created_at, updated_at, provider, external_id, external_ref,
			payment_id, phone_number, payer_name, payer_email, amount, currency, status,
			status_reason, narrative, transaction_id, issued_receipt_number,
			network_ref, source, raw
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
		p.PaymentUID,
		p.CreatedAt.UTC(),
		p.UpdatedAt.UTC(),
		p.Provider,
		p.ExternalID,
		nullIfEmpty(p.ExternalRef),
		nullIfEmpty(p.PaymentID),
		nullIfEmpty(p.PhoneNumber),
		nullIfEmpty(p.PayerName),
		nullIfEmpty(p.PayerEmail),
		p.Amount,
		p.Currency,
		p.Status,
		nullIfEmpty(p.StatusReason),
		nullIfEmpty(p.Narrative),
		nullIfEmpty(p.TransactionID),
		nullIfEmpty(p.IssuedReceiptNumber),
		nullIfEmpty(p.NetworkRef),
		nullIfEmpty(p.Source),
		rawOrNil(p.Raw),
	)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *server) checkPaymentRateLimit(ctx context.Context, key string, limit int) (bool, error) {
	payload, _ := json.Marshal(map[string]any{
		"p_key":            key,
		"p_limit":          limit,
		"p_window_seconds": 60,
	})
	body, _, err := s.restRead(ctx, http.MethodPost, "rpc/check_rate_limit", "", payload, "return=representation")
	if err != nil {
		return true, err
	}
	var allowed bool
	if err := json.Unmarshal(body, &allowed); err != nil {
		return true, err
	}
	return allowed, nil
}

func (s *server) cleanupPaymentRateLimits(ctx context.Context) error {
	cutoff := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	return s.restDelete(ctx, "rate_limits", "window_start=lt."+url.QueryEscape(cutoff))
}

func (s *server) startPaymentMaintenance() {
	go s.runEvery(10*time.Minute, func(ctx context.Context) {
		s.cleanupKycCache()
		if err := s.cleanupPaymentRateLimits(ctx); err != nil {
			log.Printf("[payments] rate-limit cleanup: %v", err)
		}
	})
}

func queryMaps(db *sql.DB, query string, args ...any) ([]map[string]any, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0)
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			row[col] = normalizeSQLValue(values[i])
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func normalizeSQLValue(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	case []byte:
		return string(t)
	default:
		return t
	}
}

func nonNilMaps(rows []map[string]any) []map[string]any {
	if rows == nil {
		return []map[string]any{}
	}
	return rows
}

func normalizeYoPhone(value string) (string, bool) {
	digits := cleanPhoneDigits(value)
	if len(digits) != 12 || !strings.HasPrefix(digits, "256") {
		return "", false
	}
	switch digits[3:5] {
	case "70", "74", "75":
		return digits, true
	}
	return "", false
}

// normalizeUgandaMsisdn accepts any 256-prefixed mobile number; KYC is used
// for all contribution numbers (MTN MoMo and Yo!), not only Yo! prefixes.
func normalizeUgandaMsisdn(value string) (string, bool) {
	digits := cleanPhoneDigits(value)
	if len(digits) != 12 || !strings.HasPrefix(digits, "256") {
		return "", false
	}
	return digits, true
}

func cleanPhoneDigits(value string) string {
	digits := nonDigits.ReplaceAllString(value, "")
	if strings.HasPrefix(digits, "0") {
		digits = "256" + digits[1:]
	}
	if len(digits) == 9 {
		digits = "256" + digits
	}
	return digits
}

func normalizePaymentStatus(transactionStatus, status string) string {
	switch strings.ToUpper(strings.TrimSpace(transactionStatus)) {
	case "SUCCEEDED", "SUCCESSFUL", "SUCCESS", "COMPLETED":
		return paymentStatusDone
	case "FAILED", "REJECTED", "CANCELLED", "CANCELED":
		return paymentStatusFailed
	case "PENDING", "PROCESSING":
		return paymentStatusPending
	}
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "ERROR":
		return paymentStatusFailed
	case "OK", "SUCCESS":
		return paymentStatusPending
	}
	return paymentStatusPending
}

func parseAmount(value string) (float64, error) {
	amount, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || amount <= 0 {
		return 0, errors.New("invalid amount")
	}
	if amount > 10_000_000 {
		return 0, errors.New("amount exceeds the maximum")
	}
	return amount, nil
}

func parseAmountQuiet(value string) float64 {
	amount, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || amount < 0 {
		return 0
	}
	return amount
}

func formatAmount(amount float64) string {
	if amount == math.Trunc(amount) {
		return strconv.FormatInt(int64(amount), 10)
	}
	return strconv.FormatFloat(amount, 'f', 2, 64)
}

func parseYoDate(value string, fallback time.Time) time.Time {
	value = strings.TrimSpace(value)
	for _, layout := range []string{
		"2006-01-02 15:04:05",
		time.RFC3339,
		"2006-01-02T15:04:05",
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC()
		}
	}
	return fallback
}

func paymentUID(provider, externalID string) string {
	return provider + ":" + strings.TrimSpace(externalID)
}

func paymentDedupeKey(p paymentRecord) string {
	stamp := p.UpdatedAt
	if stamp.IsZero() {
		stamp = p.CreatedAt
	}
	return fmt.Sprintf("payment:%s:%s:%s", p.PaymentUID, p.Status, stamp.UTC().Format(time.RFC3339Nano))
}

func clientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("X-Client-IP")); ip != "" {
		return ip
	}
	if ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); ip != "" {
		return ip
	}
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		return strings.TrimSpace(strings.Split(forwarded, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func sanitizeName(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "_", " ")
	if len(value) > 100 {
		value = value[:100]
	}
	return value
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func nullIfEmpty(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func rawOrNil(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return string(raw)
}

func mustJSON(v any) json.RawMessage {
	body, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return body
}
