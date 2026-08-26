-- tsdb_001_ad_events.sql
-- Salt TV → TimescaleDB (ug/QNAP, 100.116.185.70:55439, db analytics)
-- Ad-event analytics — the BigQuery replacement for ads.js
--   batchTrackAdEvents  → INSERT into public.ad_events (hypertable)
--   getAdAnalytics      → public.get_ad_analytics() RPC
-- Run on the TSDB instance via psql.

-- =============================================================
-- ad_events — hypertable (time-series), matches the ad_events
-- rows the app sent to BigQuery + the adAnalytics Firestore fallback.
-- =============================================================
create table if not exists public.ad_events (
  id         bigint generated always as identity,
  ad_id      text,
  event_type text not null,   -- impression|click|video_start|video_complete|video_skip|video_q1..q3
  timestamp  timestamptz not null default now(),
  user_id    text not null default 'anonymous',
  watch_time integer,
  metadata   jsonb,
  primary key (id, timestamp)
);

select create_hypertable('public.ad_events', 'timestamp', if_not_exists => true);

create index if not exists ad_events_ts_idx    on public.ad_events (timestamp desc);
create index if not exists ad_events_ad_idx    on public.ad_events (ad_id, event_type, timestamp desc);
create index if not exists ad_events_event_idx on public.ad_events (event_type, timestamp desc);

-- =============================================================
-- get_ad_analytics — replaces BigQuery aggregations in ads.js
-- getAdAnalytics. Returns the exact JSON payload shape.
-- =============================================================
create or replace function public.get_ad_analytics(
  p_start timestamptz,
  p_end   timestamptz,
  p_ad_id text default null
) returns table (payload jsonb)
language plpgsql stable as $$
declare
  v_payload jsonb;
begin
  with rows_agg as (
    select
      ad_id,
      event_type,
      count(*)::integer                       as count,
      count(distinct user_id)::integer        as unique_users,
      avg(watch_time)                         as avg_watch_time
    from public.ad_events
    where timestamp between p_start and p_end
      and (p_ad_id is null or ad_id = p_ad_id)
    group by ad_id, event_type
  ),
  per_ad as (
    select
      ad_id,
      coalesce(sum(count) filter (where event_type = 'impression'), 0)::integer as impressions,
      coalesce(sum(count) filter (where event_type = 'click'), 0)::integer      as clicks,
      coalesce(sum(count) filter (where event_type = 'video_start'), 0)::integer as starts,
      coalesce(sum(count) filter (where event_type = 'video_complete'), 0)::integer as completes,
      coalesce(sum(count) filter (where event_type = 'video_skip'), 0)::integer    as skips,
      coalesce(max(avg_watch_time), 0)        as avg_watch_time,
      coalesce(max(unique_users), 0)::integer as unique_users
    from rows_agg
    group by ad_id
  ),
  daily as (
    select
      (timestamp at time zone 'UTC')::date as date,
      event_type,
      count(*)::integer as count
    from public.ad_events
    where timestamp between p_start and p_end
    group by 1, 2
    order by 1 desc
  ),
  daily_metrics as (
    select
      to_char(date, 'YYYY-MM-DD') as date,
      coalesce(sum(count) filter (where event_type = 'impression'), 0)::integer as impressions,
      coalesce(sum(count) filter (where event_type = 'click'), 0)::integer      as clicks
    from daily
    group by date
  ),
  top_ads as (
    select
      ad_id,
      count(*) filter (where event_type = 'impression')::integer as impressions,
      count(*) filter (where event_type = 'click')::integer      as clicks,
      count(*) filter (where event_type = 'video_complete')::integer as completes
    from public.ad_events
    where timestamp between p_start and p_end
    group by ad_id
    order by impressions desc
    limit 5
  ),
  totals as (
    select
      coalesce(sum(impressions), 0)::integer as total_impressions,
      coalesce(sum(clicks), 0)::integer      as total_clicks,
      coalesce(sum(starts), 0)::integer      as total_starts,
      coalesce(sum(completes), 0)::integer   as total_completes,
      coalesce(sum(skips), 0)::integer       as total_skips
    from per_ad
  ),
  funnel(stage, users, percentage) as (
    select 'Impressions', t.total_impressions, 100::numeric
    from totals t
    union all
    select 'Starts', t.total_starts,
           case when t.total_impressions > 0
                then round((t.total_starts::numeric / t.total_impressions) * 100, 2)
                else 0 end
    from totals t
    union all
    select 'Completes', t.total_completes,
           case when t.total_impressions > 0
                then round((t.total_completes::numeric / t.total_impressions) * 100, 2)
                else 0 end
    from totals t
    union all
    select 'Clicks', t.total_clicks,
           case when t.total_impressions > 0
                then round((t.total_clicks::numeric / t.total_impressions) * 100, 2)
                else 0 end
    from totals t
  ),
  ad_metrics_arr as (
    select coalesce(jsonb_agg(jsonb_build_object(
      'adId', ad_id,
      'impressions', impressions,
      'clicks', clicks,
      'starts', starts,
      'completes', completes,
      'skips', skips,
      'avgWatchTime', round(coalesce(avg_watch_time, 0)::numeric, 2),
      'uniqueUsers', unique_users
    ) order by impressions desc), '[]'::jsonb) as val
    from per_ad
  ),
  daily_arr as (
    select coalesce(jsonb_agg(jsonb_build_object(
      'date', date,
      'impressions', impressions,
      'clicks', clicks
    ) order by date asc), '[]'::jsonb) as val
    from daily_metrics
  ),
  top_arr as (
    select coalesce(jsonb_agg(jsonb_build_object(
      'adId', ad_id,
      'impressions', impressions,
      'clicks', clicks,
      'completes', completes,
      'ctr', round(case when impressions > 0 then (clicks::numeric / impressions) * 100 else 0 end, 2)
    ) order by impressions desc), '[]'::jsonb) as val
    from top_ads
  ),
  funnel_arr as (
    select jsonb_agg(jsonb_build_object('stage', stage, 'users', users, 'percentage', percentage)
      order by array_position(array['Impressions','Starts','Completes','Clicks'], stage)) as val
    from funnel
  )
  select jsonb_build_object(
    'success', true,
    'totalImpressions', (select total_impressions from totals),
    'totalClicks', (select total_clicks from totals),
    'avgCTR', round(case when (select total_impressions from totals) > 0
                 then ((select total_clicks from totals)::numeric / (select total_impressions from totals)) * 100
                 else 0 end, 2),
    'avgCompletionRate', round(case when (select total_starts from totals) > 0
                 then ((select total_completes from totals)::numeric / (select total_starts from totals)) * 100
                 else 0 end, 2),
    'impressionTrend', 0,
    'clickTrend', 0,
    'impressionTrends', (select val from daily_arr),
    'engagementFunnel', (select val from funnel_arr),
    'topAds', (select val from top_arr),
    'adMetrics', (select val from ad_metrics_arr),
    'dateRange', jsonb_build_object('start', p_start, 'end', p_end)
  ) into v_payload;

  return query select v_payload;
end;
$$;

grant usage on schema public to postgres;
grant execute on function public.get_ad_analytics(timestamptz, timestamptz, text) to postgres;
