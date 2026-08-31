-- tsdb_002_analytics.sql
-- Salt TV → TimescaleDB (ug/QNAP, 100.116.185.70:55439, db analytics)
-- Content analytics — BigQuery replacement for functions/src/analytics.js
--   (batchTrackContent*) and analyticsQueries.js (getAnalyticsMetrics).
-- Run on the TSDB instance via psql.

-- The smoke-trial demo hypertable used a different schema; move it aside so the
-- real ingestion schema owns the name.
alter table if exists public.content_views rename to content_views_demo;

-- =============================================================
-- content_views — batchTrackContentViews
-- =============================================================
create table if not exists public.content_views (
  id                        bigint generated always as identity,
  user_id                   text,
  content_id                text not null,
  content_type              text,
  content_name              text,
  creator_id                text,
  profile_id                text,
  country_code              text,
  program_duration_seconds  double precision,
  device_id                 text,
  device_type               text,
  browser                   text,
  os                        text,
  timestamp                 timestamptz not null,
  received_at               timestamptz not null,
  primary key (id, timestamp)
);
select create_hypertable('public.content_views', 'timestamp', if_not_exists => true);
create index if not exists content_views_ts_idx   on public.content_views (timestamp desc);
create index if not exists content_views_cid_idx  on public.content_views (content_id, timestamp desc);
create index if not exists content_views_user_idx on public.content_views (user_id, timestamp desc);

-- =============================================================
-- content_impressions — batchTrackContentImpressions
-- =============================================================
create table if not exists public.content_impressions (
  id            bigint generated always as identity,
  user_id       text,
  content_id    text not null,
  content_type  text,
  content_name  text,
  impression_type text not null,
  profile_id    text,
  country_code  text,
  device_id     text,
  device_type   text,
  browser       text,
  os            text,
  timestamp     timestamptz not null,
  received_at   timestamptz not null,
  primary key (id, timestamp)
);
select create_hypertable('public.content_impressions', 'timestamp', if_not_exists => true);
create index if not exists content_impressions_ts_idx  on public.content_impressions (timestamp desc);
create index if not exists content_impressions_cid_idx on public.content_impressions (content_id, timestamp desc);

-- =============================================================
-- watch_progress — batchTrackWatchProgress
-- =============================================================
create table if not exists public.watch_progress (
  id                        bigint generated always as identity,
  user_id                   text,
  content_id                text not null,
  content_type              text,
  creator_id                text,
  position                  double precision not null,
  duration                  double precision not null,
  delta                     double precision not null,
  program_duration_seconds  double precision,
  profile_id                text,
  country_code              text,
  device_id                 text,
  device_type               text,
  browser                   text,
  os                        text,
  timestamp                 timestamptz not null,
  received_at               timestamptz not null,
  primary key (id, timestamp)
);
select create_hypertable('public.watch_progress', 'timestamp', if_not_exists => true);
create index if not exists watch_progress_ts_idx   on public.watch_progress (timestamp desc);
create index if not exists watch_progress_cid_idx  on public.watch_progress (content_id, timestamp desc);

-- =============================================================
-- content_sessions — batchTrackContentSessions
-- =============================================================
create table if not exists public.content_sessions (
  id                        bigint generated always as identity,
  user_id                   text,
  content_id                text not null,
  content_type              text,
  session_start_time        timestamptz not null,
  total_watch_time_seconds  double precision not null,
  end_reason                text not null,
  program_duration_seconds  double precision,
  profile_id                text,
  country_code              text,
  device_id                 text,
  device_type               text,
  browser                   text,
  os                        text,
  timestamp                 timestamptz not null,
  received_at               timestamptz not null,
  primary key (id, timestamp)
);
select create_hypertable('public.content_sessions', 'timestamp', if_not_exists => true);
create index if not exists content_sessions_ts_idx   on public.content_sessions (timestamp desc);
create index if not exists content_sessions_cid_idx  on public.content_sessions (content_id, timestamp desc);

-- =============================================================
-- epg_metadata — station name + thumbnail lookup (was in BigQuery).
-- Populated by a feeder from the Supabase EPG tables; the analytics RPC
-- falls back to content_id-prefix station inference when no row exists.
-- =============================================================
create table if not exists public.epg_metadata (
  content_id   text primary key,
  content_name text,
  content_type text,
  station_name text,
  thumbnail_url text,
  last_updated timestamptz not null default now()
);
create index if not exists epg_metadata_station_idx on public.epg_metadata (station_name);

-- =============================================================
-- get_analytics_metrics — replaces getAnalyticsMetrics (9 BigQuery queries).
-- Returns the exact JSON payload the admin dashboard expects.
-- =============================================================
create or replace function public.get_analytics_metrics()
returns table (payload jsonb)
language plpgsql stable as $$
declare
  v_payload jsonb;
begin
  with views_total as (
    select count(*)::bigint as total_views,
           count(distinct user_id)::bigint as unique_users
    from public.content_views
  ),
  watch_time as (
    select
      coalesce(sum(delta) filter (where coalesce(content_type,'tv') in ('tv','ondemand')), 0) as total_watch_time,
      coalesce(sum(delta) filter (where content_type = 'radio'), 0) as total_listening_time
    from public.watch_progress
  ),
  station_perf as (
    select
      coalesce(m.station_name, case
        when s.content_id like 'salt_tv_one%' then 'Salt TV One'
        when s.content_id like 'salt_tv_two%' then 'Salt TV Two'
        when s.content_type = 'radio' then 'Salt FM'
        else 'Other' end) as station,
      count(*)::bigint as views,
      coalesce(sum(s.total_watch_time_seconds), 0) as total_time
    from public.content_sessions s
    left join public.epg_metadata m on m.content_id = s.content_id
    where s.content_type in ('tv','radio')
    group by 1
  ),
  station_arr as (
    select coalesce(jsonb_agg(jsonb_build_object(
      'name', station, 'views', views, 'time', total_time) order by views desc), '[]'::jsonb) as val
    from station_perf
  ),
  top as (
    with view_stats as (
      select content_id,
             (array_agg(content_name) filter (where content_name is not null))[1] as content_name,
             coalesce((array_agg(content_type) filter (where content_type is not null))[1], 'tv') as content_type,
             count(*)::bigint as views
      from public.content_views
      group by content_id
    ),
    progress_stats as (
      select content_id, sum(delta) as total_watch_time
      from public.watch_progress
      group by content_id
    ),
    metadata as (
      select distinct on (content_id) content_id, station_name, thumbnail_url
      from public.epg_metadata
      order by content_id, last_updated desc
    ),
    ranked as (
      select
        vs.content_id,
        vs.content_name,
        vs.content_type,
        coalesce(ps.total_watch_time, 0) as total_watch_time,
        coalesce(m.station_name, case
          when vs.content_id like 'salt_tv_one%' then 'Salt TV One'
          when vs.content_id like 'salt_tv_two%' then 'Salt TV Two'
          when vs.content_type = 'radio' then 'Salt FM'
          else 'Other' end) as station,
        coalesce(m.thumbnail_url, '') as thumbnail_url,
        vs.views,
        row_number() over (order by vs.views desc) as global_rank,
        row_number() over (partition by vs.content_type order by vs.views desc) as type_rank
      from view_stats vs
      left join progress_stats ps on ps.content_id = vs.content_id
      left join metadata m on m.content_id = vs.content_id
    )
    select content_id, content_name, content_type, total_watch_time, station, thumbnail_url, views
    from ranked
    where global_rank <= 20 or type_rank <= 5
    order by views desc
  ),
  top_arr as (
    select coalesce(jsonb_agg(jsonb_build_object(
      'contentId', content_id, 'contentName', coalesce(content_name,'Unknown'),
      'contentType', coalesce(content_type,'unknown'), 'station', station,
      'thumbnailUrl', thumbnail_url, 'views', views, 'totalWatchTime', total_watch_time)
      order by views desc), '[]'::jsonb) as val
    from top
  ),
  reasons as (
    select end_reason, count(*)::bigint as count
    from public.content_sessions
    where timestamp > now() - interval '30 days'
    group by end_reason
  ),
  reasons_arr as (
    select coalesce(jsonb_agg(jsonb_build_object('reason', coalesce(end_reason,'unknown'), 'count', count) order by count desc), '[]'::jsonb) as val
    from reasons
  ),
  reasons_map as (
    select coalesce(jsonb_object_agg(coalesce(end_reason,'unknown'), count), '{}'::jsonb) as val
    from reasons
  ),
  avg_watch as (
    select coalesce(avg(total_watch_time_seconds), 0) as avg_time
    from public.content_sessions
    where timestamp > now() - interval '30 days'
  ),
  completion as (
    with session_stats as (
      select content_id,
             avg(case when program_duration_seconds > 0
                 then (total_watch_time_seconds / program_duration_seconds) * 100 end) as completion_rate,
             count(*)::bigint as session_count
      from public.content_sessions
      where program_duration_seconds > 0
        and timestamp > now() - interval '30 days'
      group by content_id
    ),
    content_names as (
      select content_id, (array_agg(content_name) filter (where content_name is not null))[1] as content_name
      from public.content_views
      group by content_id
    ),
    metadata as (
      select content_id, (array_agg(station_name) filter (where station_name is not null))[1] as station_name,
             (array_agg(thumbnail_url) filter (where thumbnail_url is not null))[1] as thumbnail_url
      from public.epg_metadata
      group by content_id
    )
    select
      ss.content_id,
      cn.content_name,
      ss.completion_rate,
      ss.session_count,
      coalesce(m.station_name, 'Other') as station_name,
      coalesce(m.thumbnail_url, '') as thumbnail_url
    from session_stats ss
    left join content_names cn on cn.content_id = ss.content_id
    left join metadata m on m.content_id = ss.content_id
    where ss.session_count > 1
    order by ss.completion_rate desc nulls last
    limit 10
  ),
  completion_arr as (
    select coalesce(jsonb_agg(jsonb_build_object(
      'contentId', content_id, 'contentName', coalesce(content_name,'Unknown'),
      'completionRate', round(coalesce(completion_rate,0))::int, 'sessionCount', session_count,
      'station', station_name, 'thumbnailUrl', thumbnail_url)
      order by completion_rate desc nulls last), '[]'::jsonb) as val
    from completion
  ),
  peak_hours as (
    select extract(hour from timestamp at time zone 'UTC')::int as hour_of_day,
           count(*)::bigint as view_count
    from public.content_views
    where timestamp > now() - interval '30 days'
    group by 1 order by 1
  ),
  peak_arr as (
    select coalesce(jsonb_agg(jsonb_build_object('hour', hour_of_day, 'views', view_count) order by hour_of_day), '[]'::jsonb) as val
    from peak_hours
  ),
  recent as (
    with latest_sessions as (
      select user_id, content_id, coalesce(content_type,'tv') as content_type,
             timestamp, total_watch_time_seconds, end_reason
      from public.content_sessions
      order by timestamp desc
      limit 20
    ),
    content_names as (
      select content_id, (array_agg(content_name) filter (where content_name is not null))[1] as content_name
      from public.content_views
      group by content_id
    ),
    metadata as (
      select content_id, (array_agg(thumbnail_url) filter (where thumbnail_url is not null))[1] as thumbnail_url
      from public.epg_metadata
      group by content_id
    )
    select
      s.user_id, cn.content_name, s.content_type, coalesce(m.thumbnail_url,'') as thumbnail_url,
      s.total_watch_time_seconds, s.end_reason, s.timestamp
    from latest_sessions s
    left join content_names cn on cn.content_id = s.content_id
    left join metadata m on m.content_id = s.content_id
    order by s.timestamp desc
  ),
  recent_arr as (
    select coalesce(jsonb_agg(jsonb_build_object(
      'userId', case when user_id is null then 'Unknown' else left(user_id,8) || '...' end,
      'contentName', coalesce(content_name,'Unknown'), 'contentType', coalesce(content_type,'unknown'),
      'thumbnailUrl', thumbnail_url, 'watchTime', round(coalesce(total_watch_time_seconds,0))::int,
      'endReason', end_reason, 'timestamp', to_char(timestamp at time zone 'UTC','HH24:MI:SS'))
      order by timestamp desc), '[]'::jsonb) as val
    from recent
  )
  select jsonb_build_object(
    'totalViews', (select total_views from views_total),
    'uniqueUsers', (select unique_users from views_total),
    'totalWatchTimeSeconds', (select round(total_watch_time) from watch_time),
    'totalListeningTimeSeconds', (select round(total_listening_time) from watch_time),
    'stationPerformance', (select val from station_arr),
    'topContent', (select val from top_arr),
    'topCompletionRates', (select val from completion_arr),
    'peakViewingHours', (select val from peak_arr),
    'recentActivity', (select val from recent_arr),
    'sessionEndReasons', (select val from reasons_map),
    'sessionEndReasonsDetailed', (select val from reasons_arr),
    'averageWatchTimeSeconds', round((select avg_time from avg_watch)),
    'timestamp', to_char(now() at time zone 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'),
    'is_crash_resilient', true
  ) into v_payload;

  return query select v_payload;
end;
$$;

grant execute on function public.get_analytics_metrics() to postgres;
