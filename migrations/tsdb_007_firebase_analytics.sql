-- tsdb_007_firebase_analytics.sql
-- User & Device analytics (was functions/src/firebaseAnalytics.js — 13 BigQuery
-- queries over analytics.content_sessions). Ported to a single TimescaleDB RPC
-- so the admin dashboard reads the same data fast (and cached by the mesh).
-- Run on the TSDB instance via psql.

create or replace function public.get_firebase_analytics(p_start date, p_end date)
returns table (payload jsonb)
language plpgsql stable as $$
declare
  v_payload jsonb;
begin
  with
  dau as (
    select count(distinct user_id)::bigint as n
    from public.content_sessions
    where timestamp::date = p_end
  ),
  mau as (
    select count(distinct user_id)::bigint as n
    from public.content_sessions
    where timestamp::date >= p_start
  ),
  sessions as (
    select count(*)::bigint as total_sessions,
           count(distinct user_id)::bigint as unique_users,
           coalesce(avg(total_watch_time_seconds), 0) as avg_duration
    from public.content_sessions
    where timestamp::date >= p_start
  ),
  devices as (
    select coalesce(device_type, 'Unknown') as device,
           count(distinct user_id)::bigint as users,
           count(*)::bigint as sessions,
           coalesce(avg(total_watch_time_seconds), 0) as avg_watch_time,
           round(count(*) * 100.0 / nullif(sum(count(*)) over (), 0), 2) as percentage
    from public.content_sessions
    where timestamp::date >= p_start
    group by 1
    order by sessions desc
  ),
  devices_arr as (
    select coalesce(jsonb_agg(jsonb_build_object(
      'device', device, 'users', users, 'sessions', sessions,
      'avgSessionDuration', round(avg_watch_time)::int, 'percentage', percentage)
      order by sessions desc), '[]'::jsonb) as val from devices
  ),
  os_br as (
    select coalesce(os, 'Unknown') as os,
           count(distinct user_id)::bigint as users,
           count(*)::bigint as sessions,
           coalesce(avg(total_watch_time_seconds), 0) as avg_watch_time,
           round(count(*) * 100.0 / nullif(sum(count(*)) over (), 0), 2) as percentage
    from public.content_sessions
    where timestamp::date >= p_start and os is not null
    group by 1
    order by sessions desc
    limit 10
  ),
  os_arr as (
    select coalesce(jsonb_agg(jsonb_build_object(
      'os', os, 'users', users, 'sessions', sessions,
      'avgSessionDuration', round(avg_watch_time)::int, 'percentage', percentage)
      order by sessions desc), '[]'::jsonb) as val from os_br
  ),
  browser_br as (
    select coalesce(browser, 'Unknown') as browser,
           count(distinct user_id)::bigint as users,
           count(*)::bigint as sessions,
           coalesce(avg(total_watch_time_seconds), 0) as avg_watch_time,
           round(count(*) * 100.0 / nullif(sum(count(*)) over (), 0), 2) as percentage
    from public.content_sessions
    where timestamp::date >= p_start and browser is not null
    group by 1
    order by sessions desc
    limit 10
  ),
  browser_arr as (
    select coalesce(jsonb_agg(jsonb_build_object(
      'browser', browser, 'users', users, 'sessions', sessions,
      'avgSessionDuration', round(avg_watch_time)::int, 'percentage', percentage)
      order by sessions desc), '[]'::jsonb) as val from browser_br
  ),
  geo_br as (
    select coalesce(country_code, 'Unknown') as country,
           count(distinct user_id)::bigint as users,
           count(*)::bigint as sessions,
           coalesce(avg(total_watch_time_seconds), 0) as avg_watch_time,
           round(count(*) * 100.0 / nullif(sum(count(*)) over (), 0), 2) as percentage
    from public.content_sessions
    where timestamp::date >= p_start and country_code is not null
    group by 1
    order by users desc
    limit 15
  ),
  geo_arr as (
    select coalesce(jsonb_agg(jsonb_build_object(
      'country', country, 'users', users, 'sessions', sessions,
      'avgSessionDuration', round(avg_watch_time)::int, 'percentage', percentage)
      order by users desc), '[]'::jsonb) as val from geo_br
  ),
  first_session as (
    select user_id, min(timestamp::date) as first_date
    from public.content_sessions
    where timestamp::date >= p_start
    group by user_id
  ),
  subsequent as (
    select distinct user_id, timestamp::date as session_date
    from public.content_sessions
    where timestamp::date >= p_start
  ),
  retention as (
    select (ss.session_date - fs.first_date) as days_since_first,
           count(distinct ss.user_id)::bigint as returning_users
    from first_session fs
    join subsequent ss on ss.user_id = fs.user_id
    where ss.session_date > fs.first_date
    group by 1
  ),
  retention_data as (
    select
      coalesce((select returning_users from retention where days_since_first = 1), 0) as day1,
      coalesce((select returning_users from retention where days_since_first = 7), 0) as day7,
      coalesce((select returning_users from retention where days_since_first = 30), 0) as day30
  ),
  all_first_sessions as (
    select user_id, min(timestamp::date) as first_date
    from public.content_sessions
    group by user_id
  ),
  new_users as (
    select count(*)::bigint as n from all_first_sessions where first_date >= p_start
  ),
  engagement as (
    select coalesce(avg(total_watch_time_seconds), 0) as avg_duration,
           coalesce(max(total_watch_time_seconds), 0) as max_duration,
           case when count(distinct user_id) > 0
                then sum(total_watch_time_seconds) / count(distinct user_id) else 0 end as avg_time_per_user
    from public.content_sessions
    where timestamp::date >= p_start
  ),
  churn as (
    select coalesce(end_reason, 'Unknown') as reason,
           count(*)::bigint as count,
           count(distinct user_id)::bigint as users,
           round(count(*) * 100.0 / nullif(sum(count(*)) over (), 0), 2) as percentage
    from public.content_sessions
    where timestamp::date >= p_start and end_reason is not null
    group by 1
    order by count desc
  ),
  churn_arr as (
    select coalesce(jsonb_agg(jsonb_build_object(
      'reason', reason, 'count', count, 'users', users, 'percentage', percentage)
      order by count desc), '[]'::jsonb) as val from churn
  ),
  dau_trend as (
    select to_char(timestamp::date, 'YYYY-MM-DD') as date,
           count(distinct user_id)::bigint as dau,
           count(*)::bigint as sessions
    from public.content_sessions
    where timestamp::date >= p_start
    group by timestamp::date
    order by timestamp::date
  ),
  dau_trend_arr as (
    select coalesce(jsonb_agg(jsonb_build_object(
      'date', date, 'dau', dau, 'sessions', sessions)), '[]'::jsonb) as val from dau_trend
  ),
  ct_eng as (
    select coalesce(content_type, 'Unknown') as type,
           count(*)::bigint as sessions,
           count(distinct user_id)::bigint as users,
           coalesce(avg(total_watch_time_seconds), 0) as avg_watch_time,
           coalesce(sum(total_watch_time_seconds), 0) as total_watch_time
    from public.content_sessions
    where timestamp::date >= p_start
    group by 1
    order by sessions desc
  ),
  ct_arr as (
    select coalesce(jsonb_agg(jsonb_build_object(
      'type', type, 'sessions', sessions, 'users', users,
      'avgWatchTime', round(avg_watch_time)::int, 'totalWatchTime', round(total_watch_time)::int)
      order by sessions desc), '[]'::jsonb) as val from ct_eng
  )
  select jsonb_build_object(
    'period', 'custom',
    'startDate', to_char(p_start, 'YYYY-MM-DD'),
    'endDate', to_char(p_end, 'YYYY-MM-DD'),
    'overview', jsonb_build_object(
      'dau', (select n from dau),
      'mau', (select n from mau),
      'newUsers', (select n from new_users),
      'totalSessions', (select total_sessions from sessions),
      'uniqueUsers', (select unique_users from sessions),
      'avgSessionDuration', round((select avg_duration from sessions))::int
    ),
    'devices', jsonb_build_object('breakdown', (select val from devices_arr)),
    'operatingSystems', jsonb_build_object('breakdown', (select val from os_arr)),
    'browsers', jsonb_build_object('breakdown', (select val from browser_arr)),
    'geographic', jsonb_build_object('breakdown', (select val from geo_arr)),
    'retention', jsonb_build_object(
      'day1', (select day1 from retention_data),
      'day7', (select day7 from retention_data),
      'day30', (select day30 from retention_data)
    ),
    'engagement', jsonb_build_object(
      'avgSessionDuration', round((select avg_duration from engagement))::int,
      'maxSessionDuration', round((select max_duration from engagement))::int,
      'avgTimePerUser', round((select avg_time_per_user from engagement))::int
    ),
    'churnReasons', (select val from churn_arr),
    'dauTrend', (select val from dau_trend_arr),
    'contentTypeEngagement', (select val from ct_arr),
    'timestamp', to_char(now() at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
  ) into v_payload;

  return query select v_payload;
end;
$$;

grant execute on function public.get_firebase_analytics(date, date) to postgres;
