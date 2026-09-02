package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"
)

// ──────────────── INBOUND ROUTER (SMS / WhatsApp → program chat) ────────────────
// Shared logic between the SMS agent webhook and the WhatsApp webhook: map an
// inbound message to the station + currently-airing chat room, then insert it
// into chat_messages as an external (non-app) message.

// channelRoute is a row from channel_routing (inbound number -> station).
type channelRoute struct {
	Channel          string `json:"channel"`
	InboundNumber    string `json:"inbound_number"`
	StationID        string `json:"station_id"`
	Kind             string `json:"kind"`
	WAPhoneNumberID  string `json:"wa_phone_number_id"`
}

// resolveChannelRoute looks up which station/kind an inbound number belongs to.
func (s *server) resolveChannelRoute(ctx context.Context, channel, inboundNumber string) (*channelRoute, error) {
	raw, _, err := s.doRest(ctx, "channel_routing", url.Values{
		"select":         {"channel,inbound_number,station_id,kind,wa_phone_number_id"},
		"channel":        {"eq." + channel},
		"inbound_number": {"eq." + inboundNumber},
		"active":         {"eq.true"},
	})
	if err != nil {
		return nil, err
	}
	var rows []channelRoute
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// resolveActiveRoom computes the chat_rooms.id that is airing NOW for a station,
// reusing the same epg_programs/activeProgramAt logic as the room managers.
// Returns (roomID, programName, ok). No active program -> ok=false.
func (s *server) resolveActiveRoom(ctx context.Context, route *channelRoute) (string, string, bool) {
	raw, _, err := s.doRest(ctx, "epg_programs", url.Values{
		"select":     {"program_name,start_time,end_time,days,tv_program_id"},
		"station_id": {"eq." + url.QueryEscape(route.StationID)},
	})
	if err != nil {
		log.Printf("[inbound] fetch epg_programs for %s failed: %v", route.StationID, err)
		return "", "", false
	}
	var programs []epgProgramRow
	if err := json.Unmarshal(raw, &programs); err != nil {
		log.Printf("[inbound] parse epg_programs for %s failed: %v", route.StationID, err)
		return "", "", false
	}
	active, ok := activeProgramAt(programs, time.Now().UTC())
	if !ok {
		return "", "", false
	}
	var roomID string
	if route.Kind == "tv" {
		roomID = active.TvProgramID
		if roomID == "" {
			roomID = fmt.Sprintf("%s_%s_%s",
				generateSlug(route.StationID), generateSlug(active.ProgramName),
				strings.ReplaceAll(active.StartTime, ":", ""))
		}
	} else {
		roomID = generateSlug(active.ProgramName)
	}
	return roomID, active.ProgramName, true
}

// inboundMessage is a normalized inbound message the insert helper accepts.
type inboundMessage struct {
	RoomID     string
	Channel    string // 'sms' | 'whatsapp'
	ExternalID string // msisdn / wa_id
	SenderName string
	Content    string
}

// insertInboundMessage inserts an external message into chat_messages via the
// service role. user_id stays NULL (not an app user); source + external id are
// recorded so presenters can reply. Returns the message id.
func (s *server) insertInboundMessage(ctx context.Context, msg inboundMessage) (string, error) {
	id, err := randomHex(16)
	if err != nil {
		return "", err
	}
	row := map[string]any{
		"id":                   id,
		"room_id":              msg.RoomID,
		"user_id":              externalUserID(msg.Channel, msg.ExternalID),
		"user_name":            msg.SenderName,
		"message_content":      msg.Content,
		"is_admin_message":     false,
		"is_lottie_emoji":      false,
		"is_expression":        false,
		"source":               msg.Channel,
		"sender_external_id":   msg.ExternalID,
		"sender_external_name": msg.SenderName,
		"created_at":           time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.restPostRow(ctx, "chat_messages", row); err != nil {
		return "", err
	}

	// Cache the external sender for reliable replies.
	_ = s.upsertChannelContact(ctx, msg.Channel, msg.ExternalID, msg.SenderName)
	return id, nil
}

// externalUserID builds a stable, non-null synthetic user id for inbound
// external (SMS/WhatsApp) senders so the app can group bubbles per sender and
// look them up (e.g. blocked-status) without crashing on a NULL user_id.
func externalUserID(channel, externalID string) string {
	clean := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, externalID)
	return "ext_" + channel + "_" + clean
}

func (s *server) upsertChannelContact(ctx context.Context, channel, externalID, name string) error {
	if externalID == "" {
		return nil
	}
	raw, _, err := s.doRest(ctx, "channel_contacts", url.Values{
		"select": {"external_id"}, "channel": {"eq." + channel}, "external_id": {"eq." + externalID},
	})
	if err == nil {
		var existing []struct {
			ExternalID string `json:"external_id"`
		}
		if json.Unmarshal(raw, &existing) == nil && len(existing) > 0 {
			return s.restPatch(ctx, "channel_contacts",
				"channel=eq."+url.QueryEscape(channel)+",external_id=eq."+url.QueryEscape(externalID),
				map[string]any{"display_name": name, "last_message_at": time.Now().UTC().Format(time.RFC3339)})
		}
	}
	return s.restPostRow(ctx, "channel_contacts", map[string]any{
		"channel": channel, "external_id": externalID, "display_name": name,
		"last_message_at": time.Now().UTC().Format(time.RFC3339),
	})
}
