-- tsdb_005_radio_reports.sql
-- get_radio_reports — the big dashboard reader (10 queries + EPG current show).

-- utc_min helper: 'HH:MM' -> minutes (must exist before the RPC below)
create or replace function public.utc_min(t text) returns int
language sql immutable as $$
  select (split_part(t,':',1)::int * 60 + coalesce(split_part(t,':',2)::int,0));
$$;

create or replace function public.get_radio_reports(
  p_days  integer default 30,
  p_start text default null,
  p_end   text default null
) returns table (payload jsonb)
language plpgsql stable as $$
declare
  v_payload jsonb;
  v_date_filter text;
  v_ts_filter   text;
  v_daily jsonb;
  v_unique_daily jsonb;
  v_listener_unique jsonb;
  v_hourly jsonb;
  v_dow jsonb;
  v_top_songs jsonb;
  v_best jsonb;
  v_worst jsonb;
  v_today jsonb;
  v_by_browser jsonb;
  v_by_client jsonb;
  v_by_country jsonb;
  v_by_stream jsonb;
  v_by_listening_time jsonb;
  v_current jsonb;
  v_current_show jsonb;
  v_today_unique bigint;
  v_hourly_programs jsonb;
  v_today_hourly jsonb;
  v_compare_hourly jsonb;
  v_compare_date text;
  v_is_weekend boolean;
begin
  if p_start is not null and p_start <> '' and p_end is not null and p_end <> '' then
    v_date_filter := format('d.date BETWEEN %L::date AND %L::date', p_start, p_end);
    v_ts_filter := format('played_at BETWEEN %L::timestamptz AND %L::timestamptz', p_start, p_end || ' 23:59:59');
  else
    v_date_filter := format('d.date > CURRENT_DATE - interval ''%s days''', p_days);
    v_ts_filter := format('played_at > now() - interval ''%s days''', p_days);
  end if;

  execute 'select coalesce(jsonb_agg(row ORDER BY date ASC), ''[]''::jsonb) from (' ||
    'select to_char(d.date,''YYYY-MM-DD'') as date, d.listeners as avg_listeners,' ||
    ' coalesce(np.unique_listeners, lst.unique_listeners) as unique_listeners' ||
    ' from public.radio_daily d' ||
    ' left join (select (snapshot_at at time zone ''UTC'')::date as date, max(listeners_unique) as unique_listeners' ||
    '   from public.radio_nowplaying where snapshot_at > now() - interval ''14 days'' group by 1) np on np.date = d.date' ||
    ' left join (select (snapshot_at at time zone ''UTC'')::date as date, count(distinct client_ip) as unique_listeners' ||
    '   from public.radio_listeners where snapshot_at > now() - interval ''14 days'' group by 1) lst on lst.date = d.date' ||
    ' where ' || v_date_filter || ') row'
    into v_daily;

  execute 'select coalesce(jsonb_agg(row ORDER BY date ASC), ''[]''::jsonb) from (' ||
    'select to_char(date,''YYYY-MM-DD'') as date, unique_listeners from (' ||
    '  select (snapshot_at at time zone ''UTC'')::date as date, max(listeners_unique) as unique_listeners' ||
    '  from public.radio_nowplaying where snapshot_at > now() - interval ''14 days'' group by 1) t) row'
    into v_unique_daily;

  execute 'select coalesce(jsonb_agg(row ORDER BY date ASC), ''[]''::jsonb) from (' ||
    'select to_char(date,''YYYY-MM-DD'') as date, unique_listeners from (' ||
    '  select (snapshot_at at time zone ''UTC'')::date as date, count(distinct client_ip) as unique_listeners' ||
    '  from public.radio_listeners where snapshot_at > now() - interval ''14 days'' group by 1) t) row'
    into v_listener_unique;

  execute 'select coalesce(jsonb_agg(row ORDER BY date ASC, hour ASC), ''[]''::jsonb) from (' ||
    'select extract(hour from snapshot_at at time zone ''UTC'')::int as hour,' ||
    ' to_char(date_trunc(''day'', snapshot_at at time zone ''UTC''),''YYYY-MM-DD'') as date,' ||
    ' max(listeners_total) as peak_listeners' ||
    ' from public.radio_nowplaying where snapshot_at > now() - interval ''14 days'' group by 1,2) row'
    into v_hourly;

  execute 'select coalesce(jsonb_agg(row ORDER BY date ASC), ''[]''::jsonb) from (' ||
    'select to_char(d.date,''YYYY-MM-DD'') as date, d.listeners from public.radio_daily d where ' || v_date_filter || ' order by d.date asc) row'
    into v_dow;

  execute 'select coalesce(jsonb_agg(row), ''[]''::jsonb) from (' ||
    'select song_title, song_artist, count(*) as plays, sum(duration_seconds) as total_airtime_seconds' ||
    ' from public.radio_history where ' || v_ts_filter ||
    ' and song_title <> '''' and song_title is not null group by song_title, song_artist order by plays desc limit 20) row'
    into v_top_songs;

  execute 'select coalesce(jsonb_agg(row), ''[]''::jsonb) from (' ||
    'select song_title, song_artist, delta_total, listeners_start, listeners_end, played_at' ||
    ' from public.radio_history where ' || v_ts_filter ||
    ' and song_title <> '''' and song_title is not null order by delta_total desc limit 10) row'
    into v_best;

  execute 'select coalesce(jsonb_agg(row), ''[]''::jsonb) from (' ||
    'select song_title, song_artist, delta_total, listeners_start, listeners_end, played_at' ||
    ' from public.radio_history where ' || v_ts_filter ||
    ' and song_title <> '''' and song_title is not null order by delta_total asc limit 10) row'
    into v_worst;

  -- today aggregate + unique listeners
  execute 'select coalesce(jsonb_build_object(''songs_played'', t.songs_played, ''total_airtime_seconds'', t.total_airtime_seconds,' ||
    ' ''avg_listeners'', t.avg_listeners, ''peak_listeners'', t.peak_listeners,' ||
    ' ''unique_listeners'', t.unique_listeners), ''null''::jsonb) from (' ||
    'select count(*)::int as songs_played, sum(duration_seconds) as total_airtime_seconds,' ||
    ' round(avg(listeners_start))::int as avg_listeners, max(listeners_start)::int as peak_listeners,' ||
    ' (select max(listeners_unique) from public.radio_nowplaying where snapshot_at > date_trunc(''day'', now())) as unique_listeners' ||
    ' from public.radio_history where played_at > date_trunc(''day'', now())) t'
    into v_today;

  select jsonb_agg(row ORDER BY listeners DESC) into v_by_browser from (
    select label as browser, listeners, connected_seconds from public.radio_browser
    where synced_at = (select max(synced_at) from public.radio_browser) order by listeners desc) row;

  select jsonb_agg(row ORDER BY listeners DESC) into v_by_client from (
    select label as client_raw, value as client, listeners, connected_seconds from public.radio_client
    where synced_at = (select max(synced_at) from public.radio_client) order by listeners desc) row;

  select jsonb_agg(row ORDER BY listeners DESC) into v_by_country from (
    select label as country_code, value as country, listeners, connected_seconds from public.radio_country
    where synced_at = (select max(synced_at) from public.radio_country) order by listeners desc) row;

  select jsonb_agg(row ORDER BY listeners DESC) into v_by_stream from (
    select label as stream_id, value as stream, listeners, connected_seconds from public.radio_stream
    where synced_at = (select max(synced_at) from public.radio_stream) order by listeners desc) row;

  select coalesce(jsonb_agg(row ORDER BY value DESC), '[]'::jsonb) into v_by_listening_time from (
    select label, value from public.radio_listening_time
    where synced_at = (select max(synced_at) from public.radio_listening_time)) row;

  -- current show from EPG (UTC now). imageLandscape: radio EPG has none → null.
  select jsonb_build_object('programName', program_name, 'presenter', coalesce(presenter,''),
    'genre', coalesce(genre,''), 'startTime', start_time, 'endTime', end_time,
    'image', image, 'imageLandscape', null)
  into v_current_show
  from public.epg_programs_radio e
  where e.days ~* (trim(to_char(now() at time zone 'UTC', 'Day')))
    and utc_min(start_time) <= utc_min(to_char(now() at time zone 'UTC','HH24:MI'))
    and utc_min(end_time) > utc_min(to_char(now() at time zone 'UTC','HH24:MI'))
  order by utc_min(start_time) desc
  limit 1;

  -- hourly program map for today (jsonb_build_object to preserve camelCase key)
  select coalesce(jsonb_agg(jsonb_build_object(
    'hour', h,
    'programName', (
      select program_name from public.epg_programs_radio p
      where p.days ~* (trim(to_char(now() at time zone 'UTC', 'Day')))
        and utc_min(p.start_time) <= h*60 and utc_min(p.end_time) > h*60
      order by utc_min(p.start_time) desc limit 1
    )
  ) ORDER BY h), '[]'::jsonb) into v_hourly_programs
  from generate_series(0,23) h;

  -- today vs compare hourly
  v_is_weekend := extract(dow from now()) in (0,6);
  v_compare_date := to_char(now() - (case when v_is_weekend then interval '7 days' else interval '1 day' end), 'YYYY-MM-DD');

  select coalesce(jsonb_agg(row ORDER BY hour), '[]'::jsonb) into v_today_hourly from (
    select hour, peak_listeners from jsonb_to_recordset(v_hourly) as x(hour int, date text, peak_listeners int)
    where date = to_char(now() at time zone 'UTC','YYYY-MM-DD')) row;

  select coalesce(jsonb_agg(row ORDER BY hour), '[]'::jsonb) into v_compare_hourly from (
    select hour, peak_listeners from jsonb_to_recordset(v_hourly) as x(hour int, date text, peak_listeners int)
    where date = v_compare_date) row;

  v_current := null::jsonb;

  select jsonb_build_object(
    'current', v_current,
    'currentShow', v_current_show,
    'daily', v_daily,
    'hourlyToday', v_today_hourly,
    'hourlyCompare', v_compare_hourly,
    'hourlyPrograms', v_hourly_programs,
    'dayOfWeek', v_dow,
    'topSongs', v_top_songs,
    'best', v_best,
    'worst', v_worst,
    'today', v_today,
    'byBrowser', coalesce(v_by_browser, '[]'::jsonb),
    'byClient', coalesce(v_by_client, '[]'::jsonb),
    'byCountry', coalesce(v_by_country, '[]'::jsonb),
    'byStream', coalesce(v_by_stream, '[]'::jsonb),
    'byListeningTime', coalesce(v_by_listening_time, '[]'::jsonb)
  ) into v_payload;

  return query select v_payload;
end;
$$;

grant execute on function public.get_radio_reports(integer, text, text) to postgres;
