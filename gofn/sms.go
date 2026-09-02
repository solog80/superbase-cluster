package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ──────────────── SMS — TV-station Windows gateway agent endpoints ────────────────
// The GSM modem lives on a Windows PC at the TV station. A custom agent drives
// it over AT commands. Because the mesh cannot reach INTO that PC:
//
//	inbound = agent POSTs received SMS here (smsInbound)
//	outbound = agent polls sms_outbox (smsOutboxPoll) → sends → reports (smsOutboxReport)
//
// The agent authenticates with X-Agent-Key (SMS_AGENT_KEY), distinct from the
// Supabase service key. These functions are NOT for end-user traffic.

// isAgentRequest reports whether the request carries the configured agent key.
func (s *server) isAgentRequest(r *http.Request) bool {
	if s.smsAgentKey == "" {
		return false
	}
	return r.Header.Get("X-Agent-Key") == s.smsAgentKey
}

// agentEndpoints lists functions that must be reachable from the TV-station
// agent WITHOUT a Supabase apikey (they carry X-Agent-Key instead).
func agentEndpoints() map[string]bool {
	return map[string]bool{
		"smsInbound": true, "smsOutboxPoll": true, "smsOutboxReport": true,
	}
}

// handleSMSInbound receives a listener SMS pushed by the agent.
func (s *server) handleSMSInbound(w http.ResponseWriter, r *http.Request) {
	if !s.isAgentRequest(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid agent key"})
		return
	}
	var body struct {
		From string `json:"from"` // msisdn of the listener
		To   string `json:"to"`   // the SIM number the SMS was sent to (channel_routing key)
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	body.From = strings.TrimSpace(body.From)
	body.Text = strings.TrimSpace(body.Text)
	if body.From == "" || body.To == "" || body.Text == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "from, to, and text are required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	route, err := s.resolveChannelRoute(ctx, "sms", body.To)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if route == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no channel_routing for this number"})
		return
	}

	roomID, programName, ok := s.resolveActiveRoom(ctx, route)
	if !ok {
		// No program airing right now — drop + log (per plan).
		log.Printf("[smsInbound] %s: no active program for station %s; dropped", body.From, route.StationID)
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "reason": "no active program"})
		return
	}

	name := body.From
	msgID, err := s.insertInboundMessage(ctx, inboundMessage{
		RoomID: roomID, Channel: "sms", ExternalID: body.From,
		SenderName: name, Content: body.Text,
	})
	if err != nil {
		log.Printf("[smsInbound] insert failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// Notify presenters/admins watching this room (lightweight, targeted).
	s.notifyRoomInbound(ctx, roomID, programName, "SMS", name, body.Text)

	writeJSON(w, http.StatusOK, map[string]any{"success": true, "messageId": msgID, "roomId": roomID})
}

// handleSMSOutboxPoll returns a batch of pending outbox rows for the agent to send.
func (s *server) handleSMSOutboxPoll(w http.ResponseWriter, r *http.Request) {
	if !s.isAgentRequest(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid agent key"})
		return
	}
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		if n := atoiSafe(v); n > 0 && n <= 100 {
			limit = n
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	raw, _, err := s.doRest(ctx, "sms_outbox", url.Values{
		"select": {"id,to_number,body,room_id"},
		"status": {"eq.pending"},
		"order":  {"created_at.asc"},
		"limit":  {fmt.Sprintf("%d", limit)},
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	var rows []struct {
		ID       string `json:"id"`
		ToNumber string `json:"to_number"`
		Body     string `json:"body"`
		RoomID   string `json:"room_id"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "bad outbox response"})
		return
	}

	// Claim the batch as 'sending' so another poll doesn't double-send.
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	if len(ids) > 0 {
		_ = s.restPatch(ctx, "sms_outbox",
			"status=eq.pending,id=in.("+strings.Join(ids, ",")+")",
			map[string]any{"status": "sending", "updated_at": time.Now().UTC().Format(time.RFC3339)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": rows})
}

// handleSMSOutboxReport marks outbox rows sent/failed after the agent attempts them.
func (s *server) handleSMSOutboxReport(w http.ResponseWriter, r *http.Request) {
	if !s.isAgentRequest(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid agent key"})
		return
	}
	var body struct {
		Results []struct {
			ID      string `json:"id"`
			Success bool   `json:"success"`
			Error   string `json:"error,omitempty"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body"})
		return
	}
	if len(body.Results) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no results"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	for _, res := range body.Results {
		if res.ID == "" {
			continue
		}
		updates := map[string]any{
			"updated_at": time.Now().UTC().Format(time.RFC3339),
		}
		if res.Success {
			updates["status"] = "sent"
			updates["sent_at"] = time.Now().UTC().Format(time.RFC3339)
		} else {
			updates["status"] = "failed"
			if res.Error != "" {
				updates["error"] = res.Error
			}
		}
		if err := s.restPatch(ctx, "sms_outbox", "id=eq."+url.QueryEscape(res.ID), updates); err != nil {
			log.Printf("[smsOutboxReport] update %s failed: %v", res.ID, err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// enqueueSmsReply is the admin/presenter entry point (service-key gated in the
// dispatcher): it queues an outbound SMS to a listener. Called by the chat UI.
func (s *server) enqueueSmsReply(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SenderExternalID string `json:"senderExternalId"`
		Content          string `json:"content"`
		RoomID           string `json:"roomId,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body"})
		return
	}
	if body.SenderExternalID == "" || body.Content == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "senderExternalId and content are required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	row := map[string]any{
		"to_number":  body.SenderExternalID,
		"body":       body.Content,
		"room_id":    body.RoomID,
		"status":     "pending",
		"created_at": time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.restPostRow(ctx, "sms_outbox", row); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "SMS queued"})
}

// notifyRoomInbound pushes a targeted notice that a new external message arrived
// in a room (in-app push to admins/presenters). Kept lightweight — see plan §4.5.
func (s *server) notifyRoomInbound(ctx context.Context, roomID, programName, channel, senderName, content string) {
	if roomID == "" {
		return
	}
	title := fmt.Sprintf("New %s in %s", channel, programName)
	body := fmt.Sprintf("%s: %s", senderName, truncate(content, 120))
	log.Printf("[inbound-notify] room=%s title=%q", roomID, title)

	// Resolve recipient FCM tokens: room admins/presenters (is_admin users).
	raw, _, err := s.doRest(ctx, "users", url.Values{
		"select": {"id"}, "is_admin": {"eq.true"}, "limit": {"50"},
	})
	if err != nil {
		return
	}
	var admins []struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &admins) != nil {
		return
	}
	for _, a := range admins {
		tokens, err := s.userFCMTokens(ctx, a.ID)
		if err != nil {
			continue
		}
		data := map[string]string{
			"type": "chat_inbound", "programId": roomID, "roomId": roomID,
			"channel": channel, "click_action": "FLUTTER_NOTIFICATION_CLICK",
		}
		s.sendFCMMany(ctx, tokens, title, body, "", data)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func atoiSafe(v string) int {
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
