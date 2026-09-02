# Inbound SMS + WhatsApp → Program Chat (two-way) — Implementation Plan

**Status:** PLANNING (locked decisions below; providers/hardware on TV-station side)
**Repo / runtime:** `superbase-cluster/gofn` (mesh on Edge, `edge.solofx.net`)
**Related:** chat already lives on the mesh (`gofn/chat.go`); chat tables = `005_chat.sql`
(radio/tv `chat_rooms`, `chat_messages`, `chat_participants`).

The goal: **listeners/viewers send an SMS or a WhatsApp message to a published number and
that message lands in the program chat room currently airing** (radio + each TV station).
Presenters/admins can then **reply back** to that listener over the same channel (two-way).

Everything below rides the existing dual-write design (Supabase master, Firestore backup,
Firebase chat functions still active during migration).

---

## 0. Current-state recap (verified)

- Chat rooms are schedule-derived in `chat_rooms`:
  - radio id = `slug(program_name)` (e.g. `gambuze`, `lindirira`)
  - TV id = `slug(station)_slug(program)_starttime` (e.g. `salt_tv_one_worship_2100`),
    also persisted on `epg_programs.tv_program_id` by the room manager.
- Stations: **1 radio** (`Live_Radio`), **3 TV** (`Salt TV One`, `Salt TV Two`, `event`).
- App reads chat via Supabase realtime (`chat_messages`), Firestore fallback; sends via
  Firestore + dual-write to Supabase + `processChatMessage` on the mesh.
- Mesh room managers (`chat.go`) recompute the active room every minute from `epg_programs`
  (`activeProgramAt`).
- No SMS/WhatsApp code exists anywhere (Flutter app, admin, mesh, or Firebase functions).

---

## 1. Channels & transport (LOCKED)

### 1.1 SMS — custom Windows gateway at the TV station (LOCKED)

- **Hardware:** GSM modem connected to a **Windows PC at the TV station** (the modem lives
  at the broadcast site, NOT on the cluster). Modem talks standard GSM AT commands over a
  COM/USB serial port (the device class CodeSegment SMS Studio supports is the target —
  e.g. Wavecom/Telit/Sierra/Falcom-class AT modems).
- **Software:** **full custom Windows software** (CodeSegment SMS Studio is expired and we
  must be independent). No Gammu, no third-party SMS aggregator (Africa's Talking, etc.).
- **SIM:** 1 SIM in the modem. **Mostly inbound** (listeners texting in) but the **same SIM
  can also send outbound** (presenter replies / station messages).
- **SMS only.** WhatsApp is a separate channel (see §1.2).

```
[GSM modem + SIM @ TV-station Windows PC]
        │  custom Windows SMS gateway (AT commands over COM/USB)
        │
        ├─ inbound: SIM receives → agent POSTs to mesh /api/v1/smsInbound   (push)
        └─ outbound: agent polls mesh /api/v1/smsOutbox:pending → sends via modem
                     → reports status back to mesh                          (pull)
        │
        ▼ HTTPS (edge.solofx.net)
   mesh (superbase-cluster/gofn)  →  channel_routing → active room → chat_messages
```

**Key constraint:** the mesh cannot reach INTO the TV-station PC (NAT/broadcast site), so:
- inbound = **agent pushes** to the mesh;
- outbound = **agent pulls** a mesh-side outbox queue, sends, then reports status.

### 1.2 WhatsApp — Meta WhatsApp Business Cloud API (LOCKED)

- Official `graph.facebook.com` API; WABA + phone number + system-user token; webhook
  (`X-Hub-Signature-256`) for inbound; token send for outbound.
- Runs entirely cloud-side (no TV-station hardware).
- Requires an approved template for the first outbound message to a new user (free-form only
  within the 24h customer-service window).

### Procurement / setup checklist
- [ ] WhatsApp: WABA ID = `____`; phone number = `____`; phone-number-id = `____`;
      system-user token = `____`; app secret = `____`; verify token = `____`
- [ ] WhatsApp: business profile + approved template text = `____`
- [ ] TV station: Windows PC always-on + stable internet (confirmed yes)
- [ ] TV station: GSM modem model = `____`; COM port = `____`; baud = `____`; SIM number = `____`

---

## 2. Architecture / routing model

**Per-channel → current show** (LOCKED):
- Each published number belongs to ONE channel (`whatsapp` or `sms`) and ONE station
  (`Live_Radio` / `Salt TV One` / `Salt TV Two` / `event`).
- On inbound, resolve the station from the number (`channel_routing`), then resolve the
  **currently airing** room at receive time (reuse `activeProgramAt` against `epg_programs`).
  Insert into `chat_messages`.
- If no program is actively airing at that moment, **drop + log** (don't invent a room).
- Two-way: presenter/admin replies from the chat UI go back over the original channel to the
  original sender's number/wa_id.

---

## 3. Database migration

New file: `superbase-cluster/migrations/010_inbound_channels.sql` (proposed; refine)

```sql
-- channel_routing: inbound number -> station/channel
create table if not exists public.channel_routing (
  channel          text not null,            -- 'whatsapp' | 'sms'
  inbound_number   text not null,            -- WA phone-number-id / SMS SIM number
  station_id       text not null,            -- Live_Radio | Salt TV One | Salt TV Two | event
  kind             text not null,            -- 'radio' | 'tv'
  wa_phone_number_id text,                   -- for whatsapp send (null for sms)
  active           boolean not null default true,
  created_at       timestamptz not null default now(),
  primary key (channel, inbound_number)
);

-- chat_messages: add source + external sender identity
alter table public.chat_messages add column if not exists source text not null default 'app';
alter table public.chat_messages add column if not exists sender_external_id text;   -- wa_id / msisdn
alter table public.chat_messages add column if not exists sender_external_name text; -- display name / masked phone

-- SMS outbox: presenter/station replies queue drained by the TV-station agent
create table if not exists public.sms_outbox (
  id            uuid primary key default gen_random_uuid(),
  to_number     text not null,
  body          text not null,
  room_id       text,                        -- originating room (for context/logs)
  status        text not null default 'pending',  -- pending | sending | sent | failed
  error         text,
  created_at    timestamptz not null default now(),
  updated_at    timestamptz not null default now(),
  sent_at       timestamptz
);
create index if not exists sms_outbox_pending_idx on public.sms_outbox (status, created_at);

-- optional: cache of external senders for reliable replies
create table if not exists public.channel_contacts (
  channel      text not null,
  external_id  text not null,   -- wa_id / msisdn
  display_name text,
  last_message_at timestamptz,
  primary key (channel, external_id)
);
```

---

## 4. Mesh Go changes (`superbase-cluster/gofn/`)

### 4.1 `inbound_router.go` (shared helper)
- `resolveStation(channel, inboundNumber)` → reads `channel_routing`.
- `resolveActiveRoom(stationID)` → queries `epg_programs`, runs `activeProgramAt(now)`;
  returns room id (radio: slug(program); TV: `tv_program_id` or slug-concat).
- `insertInboundMessage(...)` → service-role insert into `chat_messages`
  (`user_id=NULL`, `source`, `sender_external_id`, `sender_external_name`, `user_name`).
- Sends a lightweight presenter/admin notice (see §4.5).

### 4.2 `sms.go` (TV-station agent endpoints)
- `smsInbound` (POST, agent → mesh): HMAC-authenticated (shared secret); body
  `{from, text, receivedAt?}`. → `insertInboundMessage` (source=sms).
- `smsOutboxPoll` (GET, agent → mesh): returns oldest `pending` rows (bounded batch) for the
  agent to send; marks them `sending`.
- `smsOutboxReport` (POST, agent → mesh): `{ids[], success, error?}` → set `sent`/`failed`.
- These need agent auth distinct from service key (see §4.4).

### 4.3 `whatsapp.go`
- `waWebhook`: GET → hub.challenge; POST → verify `X-Hub-Signature-256`, parse messages.
- `waSend(to, body)` → `POST /v21.0/{phone_number_id}/messages` (bearer system-user token).
- Inbound → `insertInboundMessage` (source=whatsapp).

### 4.4 Auth model
- Existing mesh dispatch gates by `apikey` (service or anon). Webhooks/agent calls can't send
  the service key from outside.
- **Allowlist** for `waWebhook`/`smsInbound`/`smsOutboxPoll`/`smsOutboxReport` that bypasses
  the apikey gate **only** when:
  - WhatsApp: provider signature validates (Meta `X-Hub-Signature-256`).
  - SMS agent: an `X-Agent-Key` header matches a long-lived per-agent token (env) — the mesh
    never trusts the URL alone.
- New **service-key-gated** admin/presenter endpoints (called by the chat UI):
  - `sendWhatsAppReply` — `{messageId or senderExternalId, content}`
  - `enqueueSmsReply` — `{messageId or senderExternalId, content}` → inserts into `sms_outbox`
    (agent drains it). Optional `replyNow` path that also adds a visible app-side row.

### 4.5 Presenter/admin notice on inbound
- Do NOT blast every admin (avoids the FCM storm). Send a targeted "new SMS/WhatsApp in
  <show>" notice to the presenter/admin room watchers. Define payload `type` + `programId`
  so tapping opens the right room. (Open question on exact recipients — see §7.)

### 4.6 Env vars (add to `env-current.txt`; keep out of git)
```
WA_APP_SECRET=
WA_VERIFY_TOKEN=
WA_SYSTEM_USER_TOKEN=
SMS_AGENT_KEY=          # shared secret between the TV-station agent and the mesh
```

---

## 5. TV-station Windows SMS gateway (custom software)

**Status: scaffolded** in a separate project — `myApps/sms-agent-windows`
(.NET 8 console, runs under NSSM). See its README for build/install/config.

Small always-on Windows service/agent (separate deliverable from the mesh). Responsibilities:

1. **Modem I/O (AT commands)** over COM/USB:
   - init/AT readiness + PIN unlock
   - receive SMS (Text or PDU mode; or polling `+CMGL`) → parse sender + text
   - send SMS (`+CMGS`) + delivery-report handling
   - signal-strength/modem health → status endpoint
2. **Push inbound**: on each received SMS → `POST https://edge.solofx.net/api/v1/smsInbound`
   (`X-Agent-Key`, JSON `{from, text}`). Store locally / retry on failure.
3. **Pull outbound**: poll
   `GET https://edge.solofx.net/api/v1/smsOutboxPoll?limit=N` (e.g. every 2–5 s or long-poll)
   → send each via modem → `POST smsOutboxReport`.
4. **Health/logging**: local file log; periodic `POST` heartbeat so the mesh knows the agent
   is alive (optional alerting).
5. **Config**: number ↔ station mapping comes FROM the mesh (`channel_routing`) — the agent
   just forwards the SIM number / is told its station id at install time.

Suggested shape: **.NET 8 Windows Service** (or a small Topshelf/console run under NSSM),
targeting the same modem class CodeSegment supported (Wavecom/Telit/Sierra/Falcom-style AT
modems). Hardware-specific bits (COM port, baud, model quirks) live in a config file.

---

## 6. Flutter app changes
- Program chat reads `chat_messages`; confirm rows with `user_id = NULL` + `source` render
  correctly (badge: name + optional SMS/WhatsApp source tag).
- Presenter/admin **reply affordance**: select an SMS/WhatsApp bubble → reply box → calls
  mesh reply endpoints (`sendWhatsAppReply` / `enqueueSmsReply`).
- Decide which UI ships first (mobile `ProgramChatWidget` vs web `radio-chatroom`).

---

## 7. Open questions (resolve before/while building)
- [ ] If no program is active at inbound time → confirm **drop + log** is acceptable.
- [ ] Reply UI: mobile `ProgramChatWidget` and/or web `radio-chatroom` — which first?
- [ ] Inbound notice recipients: which presenter/admin IDs per room (role `is_admin`? a
      `moderator` set per show?) + payload `type`/`programId`.
- [ ] Should presenter replies from chat UI also appear in the thread as a visible
      "presenter replied" message? Which `source` value?
- [ ] WhatsApp media (image/voice) — show text-only first?
- [ ] SMS agent heartbeat → mesh: interval + alerting (if any).
- [ ] Outbound SMS from the same SIM shares the SMS bundle / rate; confirm acceptable volumes.
- [ ] Confirm modem model + Windows version for the agent (for COM/AT specifics).

---

## 8. Suggested implementation order
1. Migration `010_inbound_channels.sql` + `channel_routing` seed rows.
2. Mesh `inbound_router.go` (station/room resolution) — test against live `epg_programs`.
3. Mesh `sms.go` agent endpoints (`smsInbound`, `smsOutboxPoll`, `smsOutboxReport`).
4. Mesh `whatsapp.go` inbound webhook (challenge + signature) → insert into chat.
5. Mesh reply endpoints (`sendWhatsAppReply`, `enqueueSmsReply`).
6. Windows SMS gateway agent (modem I/O + push/poll) — separate deliverable/repo.
7. Flutter render tags + reply affordance.
8. Presenter notice payload/type.
9. End-to-end test: TV-station agent ⇄ mesh ⇄ chat room ⇄ reply back.
