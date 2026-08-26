-- tsdb_003_radio.sql
-- Salt TV → TimescaleDB (ug/QNAP): radio analytics — BigQuery replacement for
-- functions/src/azuracast.js. Hypertables for the AzuraCast sync jobs.
-- Run on the TSDB instance via psql.

-- =============================================================
-- radio_history — AzuraCast song history (syncStationHistory)
-- =============================================================
create table if not exists public.radio_history (
  sh_id            bigint not null,
  station_id       integer not null,
  station_name     text,
  played_at        timestamptz not null,
  duration_seconds integer,
  playlist         text,
  streamer         text,
  is_request       boolean,
  song_id          text,
  song_artist      text,
  song_title       text,
  song_text        text,
  song_album       text,
  song_genre       text,
  listeners_start  integer,
  listeners_end    integer,
  delta_total      integer,
  is_visible       boolean,
  synced_at        timestamptz not null,
  primary key (sh_id, played_at)
);
select create_hypertable('public.radio_history', 'played_at', if_not_exists => true);
create index if not exists radio_history_ts_idx   on public.radio_history (played_at desc);
create index if not exists radio_history_title_idx on public.radio_history (lower(song_title), played_at desc);
create index if not exists radio_history_station_idx on public.radio_history (station_id, played_at desc);

-- =============================================================
-- radio_listeners — per-connection snapshots (snapshotAzuraCastListeners)
-- =============================================================
create table if not exists public.radio_listeners (
  id              bigint generated always as identity,
  snapshot_at     timestamptz not null,
  client_ip       text,
  ip_hash         text,
  mount_name      text,
  is_hls          boolean,
  connected_on    timestamptz,
  connected_until timestamptz,
  connected_time  integer,
  is_browser      boolean,
  is_mobile       boolean,
  is_bot          boolean,
  client          text,
  browser_family  text,
  os_family       text,
  country         text,
  synced_at       timestamptz not null,
  primary key (id, snapshot_at)
);
select create_hypertable('public.radio_listeners', 'snapshot_at', if_not_exists => true);
create index if not exists radio_listeners_ts_idx on public.radio_listeners (snapshot_at desc);
create index if not exists radio_listeners_country_idx on public.radio_listeners (country, snapshot_at desc);

-- =============================================================
-- radio_nowplaying — per-minute totals (snapshotNowPlayingTotals)
-- =============================================================
create table if not exists public.radio_nowplaying (
  id                bigint generated always as identity,
  snapshot_at       timestamptz not null,
  station_id        integer not null,
  listeners_total   integer,
  listeners_unique  integer,
  listeners_current integer,
  hls_listeners     integer,
  mount_listeners   integer,
  song_title        text,
  song_artist       text,
  is_live           boolean,
  streamer_name     text,
  primary key (id, snapshot_at)
);
select create_hypertable('public.radio_nowplaying', 'snapshot_at', if_not_exists => true);
create index if not exists radio_nowplaying_ts_idx on public.radio_nowplaying (snapshot_at desc);
create index if not exists radio_nowplaying_station_idx on public.radio_nowplaying (station_id, snapshot_at desc);

-- =============================================================
-- radio_daily / radio_hourly — AzuraCast overview charts
-- =============================================================
create table if not exists public.radio_daily (
  id          bigint generated always as identity,
  date        date not null,
  station_id  integer not null,
  listeners   integer,
  synced_at   timestamptz not null,
  primary key (id, date)
);
select create_hypertable('public.radio_daily', 'date', if_not_exists => true, chunk_time_interval => interval '1 year');
create unique index if not exists radio_daily_station_date_uniq on public.radio_daily (station_id, date);

create table if not exists public.radio_hourly (
  id          bigint generated always as identity,
  day_of_week integer not null,
  hour        integer not null,
  station_id  integer not null,
  listeners   integer,
  synced_at   timestamptz not null,
  primary key (id, synced_at)
);
select create_hypertable('public.radio_hourly', 'synced_at', if_not_exists => true);
create index if not exists radio_hourly_dow_idx on public.radio_hourly (day_of_week, hour);

-- =============================================================
-- radio_best_worst — most/least played (charts)
-- =============================================================
create table if not exists public.radio_best_worst (
  id          bigint generated always as identity,
  type        text not null,
  song_text   text,
  num_plays   integer,
  station_id  integer not null,
  synced_at   timestamptz not null,
  primary key (id, synced_at)
);
select create_hypertable('public.radio_best_worst', 'synced_at', if_not_exists => true);

-- =============================================================
-- radio_country / radio_browser / radio_client / radio_stream —
-- overview report snapshots (same shape: label/value/listeners/connected_seconds)
-- =============================================================
create table if not exists public.radio_country (
  id                bigint generated always as identity,
  synced_at         timestamptz not null,
  station_id        integer not null,
  label             text,
  value             text,
  listeners         integer,
  connected_seconds bigint,
  primary key (id, synced_at)
);
select create_hypertable('public.radio_country', 'synced_at', if_not_exists => true);
create index if not exists radio_country_sync_idx on public.radio_country (synced_at desc);

create table if not exists public.radio_browser (
  id                bigint generated always as identity,
  synced_at         timestamptz not null,
  station_id        integer not null,
  label             text,
  value             text,
  listeners         integer,
  connected_seconds bigint,
  primary key (id, synced_at)
);
select create_hypertable('public.radio_browser', 'synced_at', if_not_exists => true);
create index if not exists radio_browser_sync_idx on public.radio_browser (synced_at desc);

create table if not exists public.radio_client (
  id                bigint generated always as identity,
  synced_at         timestamptz not null,
  station_id        integer not null,
  label             text,
  value             text,
  listeners         integer,
  connected_seconds bigint,
  primary key (id, synced_at)
);
select create_hypertable('public.radio_client', 'synced_at', if_not_exists => true);
create index if not exists radio_client_sync_idx on public.radio_client (synced_at desc);

create table if not exists public.radio_stream (
  id                bigint generated always as identity,
  synced_at         timestamptz not null,
  station_id        integer not null,
  label             text,
  value             text,
  listeners         integer,
  connected_seconds bigint,
  primary key (id, synced_at)
);
select create_hypertable('public.radio_stream', 'synced_at', if_not_exists => true);
create index if not exists radio_stream_sync_idx on public.radio_stream (synced_at desc);

-- =============================================================
-- radio_listening_time — by-listening-time buckets
-- =============================================================
create table if not exists public.radio_listening_time (
  id          bigint generated always as identity,
  synced_at   timestamptz not null,
  station_id  integer not null,
  label       text,
  value       integer,
  primary key (id, synced_at)
);
select create_hypertable('public.radio_listening_time', 'synced_at', if_not_exists => true);
create index if not exists radio_listening_time_sync_idx on public.radio_listening_time (synced_at desc);
