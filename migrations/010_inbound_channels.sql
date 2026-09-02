-- 010_inbound_channels.sql
-- Inbound SMS + WhatsApp → program chat (two-way).
--   - channel_routing: which station/kind an inbound number belongs to.
--   - chat_messages: source + external sender identity columns.
--   - sms_outbox: presenter/station SMS replies drained by the TV-station agent.
-- Supabase stays the master; the mesh (service role) drives writes, the Go
-- agent polls sms_outbox. Run on the Supabase primary (us1) via psql.

-- =============================================================
-- channel_routing — inbound number -> station/channel
-- =============================================================
CREATE TABLE IF NOT EXISTS public.channel_routing (
  channel            text NOT NULL,            -- 'whatsapp' | 'sms'
  inbound_number     text NOT NULL,            -- WA phone-number-id / SMS SIM number
  station_id         text NOT NULL,            -- Live_Radio | Salt TV One | Salt TV Two | event
  kind               text NOT NULL,            -- 'radio' | 'tv'
  wa_phone_number_id text,                     -- for whatsapp send (null for sms)
  active             boolean NOT NULL DEFAULT true,
  created_at         timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (channel, inbound_number)
);
CREATE INDEX IF NOT EXISTS channel_routing_station_idx ON public.channel_routing (station_id);

-- =============================================================
-- chat_messages — add source + external sender identity
-- =============================================================
ALTER TABLE public.chat_messages ADD COLUMN IF NOT EXISTS source text NOT NULL DEFAULT 'app';
ALTER TABLE public.chat_messages ADD COLUMN IF NOT EXISTS sender_external_id text;
ALTER TABLE public.chat_messages ADD COLUMN IF NOT EXISTS sender_external_name text;

-- =============================================================
-- sms_outbox — presenter/station replies queue drained by the TV-station agent
-- =============================================================
CREATE TABLE IF NOT EXISTS public.sms_outbox (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  to_number  text NOT NULL,
  body       text NOT NULL,
  room_id    text,                          -- originating room (context/logs)
  status     text NOT NULL DEFAULT 'pending',  -- pending | sending | sent | failed
  error      text,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  sent_at    timestamptz
);
CREATE INDEX IF NOT EXISTS sms_outbox_pending_idx ON public.sms_outbox (status, created_at) WHERE status = 'pending';

-- =============================================================
-- channel_contacts — external sender cache for reliable replies
-- =============================================================
CREATE TABLE IF NOT EXISTS public.channel_contacts (
  channel         text NOT NULL,
  external_id     text NOT NULL,             -- wa_id / msisdn
  display_name    text,
  last_message_at timestamptz,
  PRIMARY KEY (channel, external_id)
);

-- =============================================================
-- RLS: mesh (service_role) drives all of this.
-- =============================================================
ALTER TABLE public.channel_routing  ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.sms_outbox       ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.channel_contacts ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS channel_routing_service ON public.channel_routing;
DROP POLICY IF EXISTS channel_contacts_service ON public.channel_contacts;
DROP POLICY IF EXISTS sms_outbox_service ON public.sms_outbox;
CREATE POLICY channel_routing_service  ON public.channel_routing  FOR ALL TO service_role USING (true) WITH CHECK (true);
CREATE POLICY channel_contacts_service ON public.channel_contacts FOR ALL TO service_role USING (true) WITH CHECK (true);
CREATE POLICY sms_outbox_service       ON public.sms_outbox       FOR ALL TO service_role USING (true) WITH CHECK (true);

-- chat_messages source/identity columns are covered by existing chat_messages RLS.

GRANT ALL ON public.channel_routing, public.channel_contacts, public.sms_outbox TO service_role;
