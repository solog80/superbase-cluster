-- tsdb_009_stationplaylist_traffic.sql
-- StationPlaylist Traffic & Billing Logs → TimescaleDB (ug/QNAP: analytics DB)
-- Ingests commercial/advertiser playback logs from StationPlaylist Studio (SLog-*.csv).
-- Provides automated reconciliation, advertiser affidavits, and traffic analytics.
-- Run on the TSDB instance (100.116.185.70:55439, db: analytics) via psql.

-- =============================================================
-- 1. traffic_client_rules — Pattern matching for advertiser resolution
-- When ads are dragged from cartwalls/folders (Category: <Manual Track>),
-- Studio leaves the Client column empty. This rule table auto-resolves
-- the true advertiser name from the ad Title or Filename.
-- =============================================================
create table if not exists public.traffic_client_rules (
  id             integer generated always as identity primary key,
  client_name    text not null,
  match_pattern  text not null,      -- case-insensitive pattern / regex
  category       text default 'Commercial',
  priority       integer not null default 0,
  created_at     timestamptz not null default now()
);

create index if not exists traffic_client_rules_prio_idx
  on public.traffic_client_rules (priority desc, id asc);

-- Seed initial rules based on StationPlaylist actual playout logs
insert into public.traffic_client_rules (client_name, match_pattern, category, priority) values
  ('Minute Maid',              'MINUTE MAID',           'Commercial', 100),
  ('MTN',                      'MTN',                   'Commercial', 100),
  ('Airtel',                   'AIRTEL',                'Commercial', 100),
  ('Plascon',                  'PLASCON',               'Commercial', 100),
  ('Syova Seeds',              'SYOVA',                 'Commercial', 100),
  ('Advanta Seeds',            'ADVANTA',               'Commercial', 100),
  ('Global Paints',            'GLOBAL PAINTS',         'Commercial', 100),
  ('Ebenezer Hardware',        'EBENEZER',              'Commercial', 100),
  ('Kubera Motors',            'KUBERA',                'Commercial', 100),
  ('URA',                      '(URA|EFRIS)',           'Commercial', 100),
  ('Levixone Concert',         'LEVIXONE',              'Promo',       90),
  ('Sky View',                 'SKY VIEW',              'Commercial',  90),
  ('Salt Media (House Promo)', '(SALT MEDIA|SALT APP)', 'House Promo', 80),
  ('Health Tip (Sponsored)',   'HEALTH TIP',            'Sponsorship', 70)
on conflict do nothing;

-- =============================================================
-- 2. radio_spot_logs — Hypertable for all broadcast commercials & spots
-- Partitioned by played_at for fast time-series filtering and rollup.
-- =============================================================
create table if not exists public.radio_spot_logs (
  id               bigint generated always as identity,
  station_id       integer not null default 307,
  played_at        timestamptz not null,
  scheduled_at     timestamptz,
  track_type       text not null default 'Spot',  -- 'Spot', 'Manual', 'Promo', 'Jingle', 'Song'
  client           text not null,                -- Advertiser / Sponsor name
  title            text not null,                -- Ad copy / creative title
  artist           text,                         -- Campaign / Sub-brand / Artist
  duration_seconds integer not null default 0,
  category         text,                         -- Spot Group or StationPlaylist Category
  filename         text not null,                -- Full audio path (e.g. F:\Adverts\...)
  user_dj          text,                         -- User/DJ logged into Studio
  listeners        integer default 0,            -- Online listener count at air time
  variance_seconds integer,                     -- played_at minus scheduled_at in seconds
  reconciled       boolean default false,
  log_hash         text not null,                -- MD5(station_id, played_at, filename, title) for deduplication
  synced_at        timestamptz not null default now(),
  primary key (id, played_at)
);

select create_hypertable('public.radio_spot_logs', 'played_at', if_not_exists => true);

-- Deduplication index: prevents duplicate rows when log files are re-read
create unique index if not exists radio_spot_logs_hash_uniq
  on public.radio_spot_logs (played_at, log_hash);

create index if not exists radio_spot_logs_client_idx
  on public.radio_spot_logs (lower(client), played_at desc);

create index if not exists radio_spot_logs_ts_idx
  on public.radio_spot_logs (played_at desc);

create index if not exists radio_spot_logs_station_idx
  on public.radio_spot_logs (station_id, played_at desc);

create index if not exists radio_spot_logs_title_idx
  on public.radio_spot_logs (lower(title), played_at desc);

-- =============================================================
-- 3. Helper: resolve_traffic_client
-- Matches explicit client first; if empty, matches against traffic_client_rules
-- =============================================================
create or replace function public.resolve_traffic_client(
  p_title           text,
  p_filename        text,
  p_explicit_client text default null
) returns text
language plpgsql stable as $$
declare
  v_client text;
  v_target text;
begin
  if p_explicit_client is not null and trim(p_explicit_client) <> '' then
    return trim(p_explicit_client);
  end if;

  v_target := coalesce(p_title, '') || ' ' || coalesce(p_filename, '');

  select client_name into v_client
  from public.traffic_client_rules
  where v_target ~* match_pattern
  order by priority desc, id asc
  limit 1;

  if v_client is not null then
    return v_client;
  end if;

  if v_target ~* 'adverts' or v_target ~* 'commercial' then
    return 'Other Advert';
  end if;

  return 'Unknown';
end;
$$;

-- =============================================================
-- 4. RPC: get_traffic_advertiser_summary
-- Summarizes total spots, airtime, and creative titles per advertiser.
-- Used by the Traffic Officer dashboard.
-- =============================================================
create or replace function public.get_traffic_advertiser_summary(
  p_start   timestamptz,
  p_end     timestamptz,
  p_station integer default 307
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
    from public.radio_spot_logs
    where played_at between p_start and p_end
      and station_id = p_station
    group by client
  ) row;

  return query select v_payload;
end;
$$;

-- =============================================================
-- 5. RPC: get_traffic_affidavit
-- Itemized Proof of Performance for a specific advertiser / date range.
-- Generates certified airtime log entries for client invoicing.
-- =============================================================
create or replace function public.get_traffic_affidavit(
  p_client  text,
  p_start   timestamptz,
  p_end     timestamptz,
  p_station integer default 307
) returns table (payload jsonb)
language plpgsql stable as $$
declare
  v_payload jsonb;
  v_total_spots integer;
  v_total_airtime integer;
begin
  select
    count(*)::integer,
    coalesce(sum(duration_seconds)::integer, 0)
  into v_total_spots, v_total_airtime
  from public.radio_spot_logs
  where station_id = p_station
    and played_at between p_start and p_end
    and (p_client is null or p_client = '' or lower(client) = lower(p_client));

  select coalesce(jsonb_agg(row order by played_at asc), '[]'::jsonb) into v_payload
  from (
    select
      s.id,
      s.client,
      s.title,
      s.artist,
      to_char(s.played_at at time zone 'Africa/Kampala', 'YYYY-MM-DD HH24:MI:SS') as played_at_local,
      s.played_at,
      s.scheduled_at,
      s.duration_seconds,
      to_char(interval '1 second' * s.duration_seconds, 'MI:SS') as duration_formatted,
      s.category,
      s.user_dj,
      s.variance_seconds,
      s.filename,
      coalesce(e.program_name, 'Live Playout') as program_name
    from public.radio_spot_logs s
    left join lateral (
      select program_name
      from public.epg_programs_radio e
      where utc_min(e.start_time) <= extract(hour from s.played_at at time zone 'UTC')::int * 60
        and utc_min(e.end_time) > extract(hour from s.played_at at time zone 'UTC')::int * 60
        and e.days ~* ('(^|[,])' || to_char(s.played_at at time zone 'UTC', 'FMDay') || '([,]|$)')
      order by utc_min(e.start_time) desc
      limit 1
    ) e on true
    where s.station_id = p_station
      and s.played_at between p_start and p_end
      and (p_client is null or p_client = '' or lower(s.client) = lower(p_client))
  ) row;

  return query select jsonb_build_object(
    'client', coalesce(p_client, 'ALL'),
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
-- 6. RPC: get_traffic_clients
-- Returns list of all advertisers and their latest spot run time.
-- =============================================================
create or replace function public.get_traffic_clients(
  p_start   timestamptz default null,
  p_end     timestamptz default null,
  p_station integer default 307
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
    from public.radio_spot_logs
    where station_id = p_station
      and played_at between v_start and v_end
    group by client
  ) row;

  return query select v_payload;
end;
$$;
