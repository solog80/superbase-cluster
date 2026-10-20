-- tsdb_016_vmix_traffic.sql
-- vMix TV ad-playback logs -> TimescaleDB (analytics DB)
-- Separate from radio_spot_logs so radio billing is untouched. Reuses
-- public.traffic_client_rules for advertiser resolution (same pattern table).
-- Run on the TSDB instance (100.116.185.70:55439, db: analytics) via psql:
--   psql -h 100.116.185.70 -p 55439 -U postgres -d analytics -f tsdb_016_vmix_traffic.sql

-- =============================================================
-- 1. video_spot_logs — hypertable for TV ad airings (Program + overlays)
-- =============================================================
create table if not exists public.video_spot_logs (
  id               bigint generated always as identity,
  channel_id       integer not null,                 -- 401 Salt TV One, 402 Salt TV Two, 403 event
  channel_name     text,
  played_at        timestamptz not null,
  monitor          text not null default 'Program',  -- 'Program' | 'Overlay N'
  monitor_number   integer,                          -- overlay number, null/0 for program
  track_type       text not null default 'Spot',
  client           text not null,
  title            text not null,
  artist           text,
  duration_seconds integer not null default 0,
  category         text,
  filename         text not null,
  input_number     integer,
  input_key        text,
  log_hash         text not null,
  synced_at        timestamptz not null default now(),
  primary key (id, played_at)
);

select create_hypertable('public.video_spot_logs', 'played_at', if_not_exists => true);

-- Idempotent: re-pushing a VLog file never duplicates a spot
create unique index if not exists video_spot_logs_hash_uniq
  on public.video_spot_logs (played_at, log_hash);

create index if not exists video_spot_logs_ts_idx
  on public.video_spot_logs (played_at desc);
create index if not exists video_spot_logs_channel_idx
  on public.video_spot_logs (channel_id, played_at desc);
create index if not exists video_spot_logs_client_idx
  on public.video_spot_logs (lower(client), played_at desc);
create index if not exists video_spot_logs_title_idx
  on public.video_spot_logs (lower(title), played_at desc);

-- =============================================================
-- 2. RPC: get_video_traffic_advertiser_summary
-- =============================================================
create or replace function public.get_video_traffic_advertiser_summary(
  p_start   timestamptz,
  p_end     timestamptz,
  p_channel integer default 0          -- 0 = all channels
) returns table (payload jsonb)
language plpgsql stable as $$
declare
  v_payload jsonb;
begin
  select coalesce(jsonb_agg(row order by total_spots desc), '[]'::jsonb) into v_payload
  from (
    select
      client,
      count(*)::integer as total_spots,
      sum(duration_seconds)::integer as total_airtime_seconds,
      round(sum(duration_seconds) / 60.0, 1) as total_airtime_minutes,
      min(played_at) as first_aired,
      max(played_at) as last_aired,
      jsonb_agg(distinct title) as creatives,
      count(distinct title)::integer as creative_count
    from public.video_spot_logs
    where played_at between p_start and p_end
      and (p_channel = 0 or channel_id = p_channel)
    group by client
  ) row;

  return query select v_payload;
end;
$$;

-- =============================================================
-- 3. RPC: get_video_traffic_affidavit
-- Itemized Proof of Performance for a client / channel / date range.
-- =============================================================
create or replace function public.get_video_traffic_affidavit(
  p_client  text,
  p_start   timestamptz,
  p_end     timestamptz,
  p_channel integer default 0
) returns table (payload jsonb)
language plpgsql stable as $$
declare
  v_payload      jsonb;
  v_total_spots  integer;
  v_total_airtime integer;
  v_channel_name text;
begin
  select
    count(*)::integer,
    coalesce(sum(duration_seconds)::integer, 0),
    coalesce(max(nullif(channel_name, '')), '')
  into v_total_spots, v_total_airtime, v_channel_name
  from public.video_spot_logs
  where played_at between p_start and p_end
    and (p_channel = 0 or channel_id = p_channel)
    and (p_client is null or p_client = '' or lower(client) = lower(p_client));

  select coalesce(jsonb_agg(row order by played_at asc), '[]'::jsonb) into v_payload
  from (
    select
      s.id,
      s.channel_id,
      coalesce(nullif(s.channel_name, ''), 'Channel ' || s.channel_id::text) as channel_name,
      s.monitor,
      s.client,
      s.title,
      s.artist,
      to_char(s.played_at at time zone 'Africa/Kampala', 'YYYY-MM-DD HH24:MI:SS') as played_at_local,
      s.played_at,
      s.duration_seconds,
      to_char(interval '1 second' * s.duration_seconds, 'MI:SS') as duration_formatted,
      s.category,
      s.filename,
      s.input_number,
      s.input_key,
      'Live Playout'::text as program_name,
      ''::text as operator
    from public.video_spot_logs s
    where s.played_at between p_start and p_end
      and (p_channel = 0 or s.channel_id = p_channel)
      and (p_client is null or p_client = '' or lower(s.client) = lower(p_client))
  ) row;

  return query select jsonb_build_object(
    'client', coalesce(p_client, 'ALL'),
    'channel', p_channel,
    'channel_name', v_channel_name,
    'start_date', p_start,
    'end_date', p_end,
    'total_spots', v_total_spots,
    'total_airtime_seconds', v_total_airtime,
    'total_airtime_minutes', round(v_total_airtime / 60.0, 1),
    'spots', v_payload
  );
end;
$$;

-- =============================================================
-- 4. RPC: get_video_traffic_clients
-- =============================================================
create or replace function public.get_video_traffic_clients(
  p_start   timestamptz default null,
  p_end     timestamptz default null,
  p_channel integer default 0
) returns table (payload jsonb)
language plpgsql stable as $$
declare
  v_payload jsonb;
  v_start timestamptz := coalesce(p_start, now() - interval '30 days');
  v_end   timestamptz := coalesce(p_end, now());
begin
  select coalesce(jsonb_agg(row order by total_spots desc), '[]'::jsonb) into v_payload
  from (
    select
      client,
      count(*)::integer as total_spots,
      sum(duration_seconds)::integer as total_airtime_seconds,
      max(played_at) as last_played_at
    from public.video_spot_logs
    where played_at between v_start and v_end
      and (p_channel = 0 or channel_id = p_channel)
    group by client
  ) row;

  return query select v_payload;
end;
$$;

-- =============================================================
-- 5. Optional: TV advertiser resolution rules (reuses traffic_client_rules).
-- Add real brands as they are contracted; uncomment/adjust.
-- =============================================================
-- insert into public.traffic_client_rules (client_name, match_pattern, category, priority) values
--   ('Stanbic Bank', 'STANBIC', 'Commercial', 100)
-- on conflict do nothing;
