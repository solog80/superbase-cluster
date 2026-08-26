-- 001_ads.sql
-- Salt TV → Supabase: ads module (migrated from functions/src/ads.js)
-- Tables: ads (creative inventory). Ad-event analytics live on the dedicated
-- TimescaleDB instance (migrations/tsdb_001_ad_events.sql), NOT here.
-- View: ads_api (exact camelCase shape the Flutter Ad.fromJson expects)
-- Requires PG15+ (security_invoker). Run on the primary (us1) via psql.

-- =============================================================
-- ads — creative inventory (Firestore collection `ads`)
-- =============================================================
create table if not exists public.ads (
  id                     text primary key default gen_random_uuid()::text,
  ad_name                text not null,
  ad_type                text not null default 'manual',      -- 'manual' | 'vast'
  status                 text not null default 'inactive',    -- 'active' | 'inactive' | 'pending'
  placement_type         text[] not null default '{}',        -- ['pre-roll','mid-roll','banner']
  creative_url           text,
  creative_type          text,                                 -- 'image' | 'video'
  landing_page_url       text,
  vast_tag_url           text,
  vast_wrapper_limit     integer,
  duration_seconds       integer,
  thumbnail_url          text,
  mid_roll_trigger_type  text,                                 -- 'percentage' | 'timestamp'
  mid_roll_trigger_value integer,
  priority               integer not null default 0,
  frequency_cap          jsonb not null default '{}'::jsonb,
  targeting_rules        jsonb not null default '{}'::jsonb,
  start_date             timestamptz,
  end_date               timestamptz,
  created_at             timestamptz not null default now(),
  updated_at             timestamptz not null default now()
);

create index if not exists ads_status_priority_idx on public.ads (status, priority desc);

alter table public.ads enable row level security;

-- anon: can only read active ads (direct /rest/v1/ads_api reads are safe)
create policy ads_anon_read on public.ads
  for select to anon
  using (status = 'active');

-- service_role: full access (the Go service / admin tools)
create policy ads_svc_all on public.ads
  for all to service_role
  using (true) with check (true);

grant select, insert, update, delete on public.ads to service_role;
grant select on public.ads to anon;

-- =============================================================
-- ads_api — camelCase view matching the Flutter Ad model exactly
-- (id, adName, adType, status, placementType, creativeUrl, ...).
-- security_invoker: RLS of ads still applies for anon.
-- =============================================================
drop view if exists public.ads_api;
create view public.ads_api
with (security_invoker = on) as
select
  id,
  ad_name                as "adName",
  ad_type                as "adType",
  status,
  placement_type         as "placementType",
  creative_url           as "creativeUrl",
  creative_type          as "creativeType",
  landing_page_url       as "landingPageUrl",
  vast_tag_url           as "vastTagUrl",
  vast_wrapper_limit     as "vastWrapperLimit",
  duration_seconds       as "durationSeconds",
  thumbnail_url          as "thumbnailUrl",
  mid_roll_trigger_type  as "midRollTriggerType",
  mid_roll_trigger_value as "midRollTriggerValue",
  priority,
  frequency_cap          as "frequencyCap",
  targeting_rules        as "targetingRules",
  start_date             as "startDate",
  end_date               as "endDate",
  created_at             as "createdAt",
  updated_at             as "updatedAt"
from public.ads;

-- =============================================================
-- ad_events + get_ad_analytics live on TimescaleDB (ug/QNAP),
-- see migrations/tsdb_001_ad_events.sql. Nothing else is needed here.
-- =============================================================

grant select on public.ads_api to anon, service_role;