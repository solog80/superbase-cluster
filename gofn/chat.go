package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ──────────────── CHAT (Supabase master, Firebase backup — dual-write) ────────────────
// Mirrors functions/src/chatNotifications.js, chatRoomManager.js,
// scheduledChatManager.js, scheduledTvChatManager.js, updateChatParticipants.js
// into the mesh. Rooms/messages/participants live in Supabase (master); the app
// dual-writes to Firestore (backup) and calls processChatMessage here. FCM is
// sent from Go via the Firebase V1 HTTP API (helpers shared with notifications.go).
//
// During the migration window the legacy Firebase Firestore-triggered chat
// functions stay active, so both sides notify. This file is the Supabase-master
// path of that dual-write.

// epgProgramRow is a flattened EPG program used by the room managers.
type epgProgramRow struct {
	ProgramName string `json:"program_name"`
	StartTime   string `json:"start_time"`
	EndTime     string `json:"end_time"`
	Days        string `json:"days"`
	TvProgramID string `json:"tv_program_id"`
	DeleteChat  *bool  `json:"delete_chat,omitempty"`
}

// ──────────────── SCHEDULED ROOM MANAGERS ────────────────

// startChatScheduler runs the radio + TV chat room managers every minute,
// independent of TSDB/AzuraCast (they only need the Supabase connection).
func (s *server) startChatScheduler() {
	if s.serviceKey == "" {
		log.Println("chat scheduler: disabled (no SERVICE_ROLE_KEY)")
		return
	}
	log.Println("chat scheduler: starting radio + tv room managers")
	go s.runEvery(time.Minute, func(ctx context.Context) {
		if err := s.runRadioChatManager(ctx); err != nil {
			log.Printf("chat radio manager: %v", err)
		}
	})
	go s.runEvery(time.Minute, func(ctx context.Context) {
		if err := s.runTvChatManager(ctx); err != nil {
			log.Printf("chat tv manager: %v", err)
		}
	})
}

// activeProgramAt picks the program currently airing for a lineup at nowUtc.
func activeProgramAt(programs []epgProgramRow, nowUtc time.Time) (epgProgramRow, bool) {
	currentDay := nowUtc.UTC().Format("Monday")
	for _, p := range programs {
		if p.ProgramName == "" || p.StartTime == "" || p.EndTime == "" || p.Days == "" {
			continue
		}
		days := strings.Split(p.Days, ",")
		var hasDay bool
		for _, d := range days {
			if strings.TrimSpace(d) == currentDay {
				hasDay = true
				break
			}
		}
		if !hasDay {
			continue
		}
		var sh, sm, eh, em int
		if _, err := fmt.Sscanf(p.StartTime, "%d:%d", &sh, &sm); err != nil {
			continue
		}
		if _, err := fmt.Sscanf(p.EndTime, "%d:%d", &eh, &em); err != nil {
			continue
		}
		y, mo, d := nowUtc.UTC().Date()
		start := time.Date(y, mo, d, sh, sm, 0, 0, time.UTC)
		end := time.Date(y, mo, d, eh, em, 0, 0, time.UTC)
		if !end.After(start) {
			end = end.Add(24 * time.Hour) // crosses midnight
		}
		effStart := start
		if nowUtc.UTC().Before(start) {
			effStart = start.Add(-24 * time.Hour)
			end = end.Add(-24 * time.Hour)
		}
		if !nowUtc.UTC().Before(effStart) && nowUtc.UTC().Before(end) {
			return p, true
		}
	}
	return epgProgramRow{}, false
}

func (s *server) runRadioChatManager(ctx context.Context) error {
	// Read the radio lineup (Live_Radio station + its programs).
	raw, _, err := s.doRest(ctx, "epg_programs", url.Values{
		"select":     {"program_name,start_time,end_time,days,tv_program_id"},
		"station_id": {"eq.Live_Radio"},
	})
	if err != nil {
		return err
	}
	var programs []epgProgramRow
	if err := json.Unmarshal(raw, &programs); err != nil {
		return err
	}
	if len(programs) == 0 {
		return nil
	}

	nowUtc := time.Now().UTC()
	todayStr := nowUtc.Format("2006-01-02")

	active, ok := activeProgramAt(programs, nowUtc)
	var activeID string
	if ok {
		activeID = generateSlug(active.ProgramName)
		log.Printf("chat radio manager: active now = %s (%s)", activeID, active.ProgramName)
		if err := s.upsertChatRoom(ctx, chatRoomRow{
			ID: activeID, Kind: "radio", ProgramName: active.ProgramName, IsActive: true,
			LastCleanedAt: &todayStr,
		}); err != nil {
			return err
		}
		// Clean messages on a new day (delete_chat != false).
		if err := s.cleanRoomMessagesIfNewDay(ctx, activeID, todayStr); err != nil {
			return err
		}
	}

	// Deactivate other radio rooms.
	filter := "kind=eq.radio"
	if activeID != "" {
		filter = "kind=eq.radio,id=neq." + activeID
	}
	log.Printf("chat radio manager: deactivating (%s)", filter)
	return s.restPatch(ctx, "chat_rooms", filter, map[string]any{"is_active": false})
}

func (s *server) runTvChatManager(ctx context.Context) error {
	// Read TV stations + their programs.
	raw, _, err := s.doRest(ctx, "epg_stations", url.Values{
		"select":      {"id"},
		"lineup_type": {"eq.tv"},
	})
	if err != nil {
		return err
	}
	var stations []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &stations); err != nil {
		return err
	}

	nowUtc := time.Now().UTC()
	todayStr := nowUtc.Format("2006-01-02")
	activeIDs := map[string]bool{}

	for _, st := range stations {
		raw, _, err := s.doRest(ctx, "epg_programs", url.Values{
			"select":     {"program_name,start_time,end_time,days,tv_program_id"},
			"station_id": {"eq." + st.ID},
		})
		if err != nil {
			continue
		}
		var programs []epgProgramRow
		if err := json.Unmarshal(raw, &programs); err != nil {
			continue
		}
		active, ok := activeProgramAt(programs, nowUtc)
		if !ok {
			continue
		}
		tvProgramID := fmt.Sprintf("%s_%s_%s",
			generateSlug(st.ID), generateSlug(active.ProgramName), strings.ReplaceAll(active.StartTime, ":", ""))
		activeIDs[tvProgramID] = true

		if err := s.upsertChatRoom(ctx, chatRoomRow{
			ID: tvProgramID, Kind: "tv", StationName: st.ID,
			ProgramID: tvProgramID, ProgramName: active.ProgramName, IsActive: true,
			LastCleanedAt: &todayStr,
		}); err != nil {
			return err
		}
		// Persist tv_program_id on the EPG program so the app can use it.
		if active.TvProgramID != tvProgramID {
			_ = s.restPatch(ctx, "epg_programs",
				"station_id=eq."+url.QueryEscape(st.ID)+",program_name=eq."+url.QueryEscape(active.ProgramName)+",start_time=eq."+active.StartTime,
				map[string]any{"tv_program_id": tvProgramID})
		}
		if err := s.cleanRoomMessagesIfNewDay(ctx, tvProgramID, todayStr); err != nil {
			return err
		}
	}

	// Deactivate TV rooms not currently active. PostgREST PATCH does not apply
	// reliably to `not.in(...)` filters (returns 204 with no rows changed), so
	// resolve the explicit id list first, then PATCH by id=in.(...).
	filter := "kind=eq.tv,is_active=eq.true"
	if len(activeIDs) > 0 {
		raw, _, err := s.doRest(ctx, "chat_rooms", url.Values{
			"select": {"id"}, "kind": {"eq.tv"}, "is_active": {"eq.true"},
			"limit": {"200"},
		})
		if err != nil {
			return err
		}
		var rows []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &rows); err != nil {
			return err
		}
		toDeactivate := make([]string, 0, len(rows))
		for _, r := range rows {
			if !activeIDs[r.ID] {
				toDeactivate = append(toDeactivate, r.ID)
			}
		}
		if len(toDeactivate) == 0 {
			log.Printf("chat tv manager: nothing to deactivate")
			return nil
		}
		// PostgREST in-list — split into batches to stay within URL limits.
		for i := 0; i < len(toDeactivate); i += 50 {
			end := i + 50
			if end > len(toDeactivate) {
				end = len(toDeactivate)
			}
			ids := strings.Join(toDeactivate[i:end], ",")
			log.Printf("chat tv manager: deactivating %d room(s)", end-i)
			if err := s.restPatch(ctx, "chat_rooms", "kind=eq.tv,id=in.("+ids+")", map[string]any{"is_active": false}); err != nil {
				return err
			}
		}
		return nil
	}
	return s.restPatch(ctx, "chat_rooms", filter, map[string]any{"is_active": false})
}

type chatRoomRow struct {
	ID            string
	Kind          string
	StationName   string
	ProgramID     string
	ProgramName   string
	IsActive      bool
	LastCleanedAt *string
}

// upsertChatRoom inserts or updates a room row by id.
func (s *server) upsertChatRoom(ctx context.Context, r chatRoomRow) error {
	row := map[string]any{
		"id":              r.ID, "kind": r.Kind, "is_active": r.IsActive,
		"program_name":    r.ProgramName,
		"last_cleaned_at": r.LastCleanedAt,
		"created_at":      time.Now().UTC().Format(time.RFC3339),
	}
	if r.StationName != "" {
		row["station_name"] = r.StationName
	}
	if r.ProgramID != "" {
		row["program_id"] = r.ProgramID
	}
	// Check existence.
	raw, _, err := s.doRest(ctx, "chat_rooms", url.Values{"select": {"id"}, "id": {"eq." + url.QueryEscape(r.ID)}})
	if err != nil {
		return err
	}
	var existing []struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &existing) == nil && len(existing) > 0 {
		updates := map[string]any{
			"kind": r.Kind, "is_active": r.IsActive, "program_name": r.ProgramName,
			"last_cleaned_at": r.LastCleanedAt,
		}
		if r.StationName != "" {
			updates["station_name"] = r.StationName
		}
		if r.ProgramID != "" {
			updates["program_id"] = r.ProgramID
		}
		return s.restPatch(ctx, "chat_rooms", "id=eq."+url.QueryEscape(r.ID), updates)
	}
	return s.restPostRow(ctx, "chat_rooms", row)
}

// cleanRoomMessagesIfNewDay deletes a room's messages once per day.
func (s *server) cleanRoomMessagesIfNewDay(ctx context.Context, roomID, todayStr string) error {
	raw, _, err := s.doRest(ctx, "chat_rooms", url.Values{"select": {"last_cleaned_at"}, "id": {"eq." + url.QueryEscape(roomID)}})
	if err != nil {
		return err
	}
	var rows []struct {
		LastCleanedAt *string `json:"last_cleaned_at"`
	}
	if json.Unmarshal(raw, &rows) == nil && len(rows) == 1 && rows[0].LastCleanedAt != nil && *rows[0].LastCleanedAt == todayStr {
		return nil // already cleaned today
	}
	return s.restDelete(ctx, "chat_messages", "room_id=eq."+url.QueryEscape(roomID))
}

// ──────────────── PROCESS MESSAGE (participant + notifications) ────────────────

// handleProcessChatMessage mirrors updateChatParticipants + chatNotifications:
// called by the app right after it writes a message to Supabase (and Firestore).
func (s *server) handleProcessChatMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MessageID      string `json:"messageId"`
		RoomID         string `json:"roomId"`
		UserID         string `json:"userId"`
		UserName       string `json:"userName"`
		MessageContent string `json:"messageContent"`
		IsAdminMessage bool   `json:"isAdminMessage"`
		IsLottieEmoji  bool   `json:"isLottieEmoji"`
		IsExpression   bool   `json:"isExpression"`
		CreatedAt      string `json:"createdAt"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	if body.RoomID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "roomId is required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// 1. Add the sender as a participant.
	if body.UserID != "" {
		profileImg := ""
		if raw, _, err := s.doRest(ctx, "users", url.Values{
			"select": {"profile_image_url"}, "id": {"eq." + url.QueryEscape(body.UserID)},
		}); err == nil {
			var ps []struct {
				ProfileImageURL *string `json:"profile_image_url"`
			}
			if json.Unmarshal(raw, &ps) == nil && len(ps) == 1 && ps[0].ProfileImageURL != nil {
				profileImg = *ps[0].ProfileImageURL
			}
		}
		_ = s.upsertChatParticipant(ctx, body.RoomID, body.UserID, orStr(body.UserName, "Anonymous"), profileImg)
	}

	// 2. Notifications (skip admin/system/lottie/expression messages).
	if !body.IsAdminMessage && !body.IsLottieEmoji && !body.IsExpression {
		s.sendChatNotifications(ctx, body.RoomID, body.MessageID, body.UserID, orStr(body.UserName, "Anonymous"), body.MessageContent)
	}

	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (s *server) upsertChatParticipant(ctx context.Context, roomID, userID, userName, profileImg string) error {
	// Only insert if not already a participant.
	raw, _, err := s.doRest(ctx, "chat_participants", url.Values{
		"select": {"user_id"}, "room_id": {"eq." + url.QueryEscape(roomID)}, "user_id": {"eq." + url.QueryEscape(userID)},
	})
	if err == nil {
		var existing []struct {
			UserID string `json:"user_id"`
		}
		if json.Unmarshal(raw, &existing) == nil && len(existing) > 0 {
			return nil
		}
	}
	row := map[string]any{
		"room_id": roomID, "user_id": userID, "user_name": userName,
		"profile_image_url": profileImg, "first_joined_at": time.Now().UTC().Format(time.RFC3339),
	}
	return s.restPostRow(ctx, "chat_participants", row)
}

// sendChatNotifications resolves @mentions + admins and sends FCM pushes.
func (s *server) sendChatNotifications(ctx context.Context, roomID, messageID, senderID, senderName, content string) {
	recipients := map[string]bool{}
	mentionedIDs := map[string]bool{}

	// @mentions → user_profiles by user_name.
	mentionRe := regexp.MustCompile(`@(\w+)`)
	for _, m := range mentionRe.FindAllStringSubmatch(content, -1) {
		if len(m) < 2 {
			continue
		}
		raw, _, err := s.doRest(ctx, "users", url.Values{
			"select": {"id"}, "user_name": {"eq." + url.QueryEscape(m[1])}, "limit": {"1"},
		})
		if err != nil {
			continue
		}
		var rows []struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &rows) == nil && len(rows) == 1 && rows[0].ID != senderID {
			mentionedIDs[rows[0].ID] = true
			recipients[rows[0].ID] = true
		}
	}

	// Admins.
	adminIDs := map[string]bool{}
	if raw, _, err := s.doRest(ctx, "users", url.Values{
		"select": {"id"}, "is_admin": {"eq.true"},
	}); err == nil {
		var rows []struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &rows) == nil {
			for _, r := range rows {
				if r.ID == senderID {
					continue
				}
				adminIDs[r.ID] = true
				recipients[r.ID] = true
			}
		}
	}

	if len(recipients) == 0 {
		return
	}

	// Room name for the title.
	roomName := "Chat"
	if raw, _, err := s.doRest(ctx, "chat_rooms", url.Values{"select": {"program_name"}, "id": {"eq." + url.QueryEscape(roomID)}}); err == nil {
		var rows []struct {
			ProgramName *string `json:"program_name"`
		}
		if json.Unmarshal(raw, &rows) == nil && len(rows) == 1 && rows[0].ProgramName != nil {
			roomName = *rows[0].ProgramName
		}
	}

	// Build per-user notification type: mention wins, else admin_alert.
	userNotification := map[string]string{}
	for uid := range mentionedIDs {
		userNotification[uid] = "mention"
	}
	for uid := range adminIDs {
		if _, ok := userNotification[uid]; !ok {
			userNotification[uid] = "admin_alert"
		}
	}

	// Fetch FCM tokens for all recipients.
	for uid, ntype := range userNotification {
		raw, _, err := s.doRest(ctx, "users", url.Values{
			"select": {"fcm_tokens"}, "id": {"eq." + url.QueryEscape(uid)},
		})
		if err != nil {
			continue
		}
		var rows []struct {
			FCMTokens json.RawMessage `json:"fcm_tokens"`
		}
		if json.Unmarshal(raw, &rows) != nil || len(rows) != 1 {
			continue
		}
		var tokens []string
		if json.Unmarshal(rows[0].FCMTokens, &tokens) != nil {
			continue
		}
		for _, tok := range tokens {
			if tok == "" {
				continue
			}
			var title, body string
			var ntypeOut string
			if ntype == "mention" {
				title = fmt.Sprintf("New mention in %s", roomName)
				body = fmt.Sprintf("%s mentioned you: \"%s\"", senderName, content)
				ntypeOut = "mention"
			} else {
				title = fmt.Sprintf("New User Message in %s", roomName)
				body = fmt.Sprintf("%s: \"%s\"", senderName, content)
				ntypeOut = "admin_alert"
			}
			data := map[string]string{
				"type": ntypeOut, "programId": roomID, "messageId": messageID,
				"senderId": senderID, "senderName": senderName, "click_action": "FLUTTER_NOTIFICATION_CLICK",
			}
			if err := s.fcmSend(ctx, tok, title, body, "", data); err != nil {
				log.Printf("[chat notify] fcm send to %s failed: %v", uid, err)
			}
		}
	}
}
