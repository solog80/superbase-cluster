-- tsdb_006_radio_shows.sql
-- Show-mapping radio readers: getRadioShowAnalytics, getRadioShowSnapshots,
-- getRadioShowListenerDetails. EPG programs come from the epg_programs_radio
-- mirror (loaded from Supabase).

create or replace function public.get_radio_show_analytics(
  p_days  integer default 30,
  p_start text default null,
  p_end   text default null
) returns table (payload jsonb)
language plpgsql stable as $$
declare
  v_payload jsonb;
  v_ts_filter text;
  v_total bigint;
begin
  if p_start is not null and p_start <> '' and p_end is not null and p_end <> '' then
    v_ts_filter := format('played_at BETWEEN %L::timestamptz AND %L::timestamptz', p_start, p_end || ' 23:59:59');
  else
    v_ts_filter := format('played_at > now() - interval ''%s days''', p_days);
  end if;

  select count(*) into v_total
  from public.radio_history
  where song_title <> '' and song_title is not null
    and (p_start is null or p_start = '' or played_at between p_start::timestamptz and (p_end || ' 23:59:59')::timestamptz)
    and (p_start is null or p_start = '' or played_at > now() - (p_days || ' days')::interval);

  -- Map each played song to the show airing at that day+hour (EPG).
  -- day_num: BQ EXTRACT(DAYOFWEEK) = 1(Sun)..7(Sat); JS DAY_MAP same.
  select coalesce(jsonb_agg(row ORDER BY total_songs DESC), '[]'::jsonb) into v_payload from (
    with played as (
      select h.song_title, h.song_artist, h.listeners_start, h.delta_total, h.duration_seconds, h.played_at,
        extract(dow from h.played_at at time zone 'UTC') + 1 as day_num,
        extract(hour from h.played_at at time zone 'UTC')::int as hour
      from public.radio_history h
      where h.song_title <> '' and h.song_title is not null
        and (
          (p_start is not null and p_start <> '' and h.played_at between p_start::timestamptz and (p_end || ' 23:59:59')::timestamptz)
          or (p_start is null or p_start = '') and h.played_at > now() - (p_days || ' days')::interval
        )
    ),
    mapped as (
      select p.*, e.program_name, e.presenter, e.genre, e.type, e.start_time, e.end_time, e.days, e.image, e.thumbnail
      from played p
      left join lateral (
        select program_name, presenter, genre, type, start_time, end_time, days, image, thumbnail
        from public.epg_programs_radio e
        where utc_min(e.start_time) <= p.hour*60 and utc_min(e.end_time) > p.hour*60
          and e.days ~* ('(^|[,])' || to_char(p.played_at at time zone 'UTC','FMDay') || '([,]|$)')
        order by utc_min(e.start_time) desc
        limit 1
      ) e on true
    )
    select
      coalesce(program_name,'Unknown') as "programName",
      coalesce(presenter,'') as presenter,
      coalesce(genre,'') as genre,
      coalesce(type,'') as type,
      coalesce(start_time,'') as "startTime",
      coalesce(end_time,'') as "endTime",
      coalesce(days,'') as days,
      coalesce(image,'') as image,
      coalesce(thumbnail,'') as thumbnail,
      count(*)::int as total_songs,
      sum(duration_seconds)::int as total_airtime_seconds,
      round(avg(listeners_start))::int as avg_listeners,
      max(listeners_start)::int as peak_listeners,
      coalesce(sum(delta_total),0)::int as total_listener_delta
    from mapped
    group by 1,2,3,4,5,6,7,8,9
  ) row;

  return query select jsonb_build_object(
    'shows', v_payload,
    'totalHistoryEntries', v_total,
    'mappedEntries', v_total,
    'totalPrograms', (select count(*) from public.epg_programs_radio)
  );
end;
$$;

-- =============================================================
-- get_radio_show_snapshots — getRadioShowSnapshots
-- Matches radio_listeners + radio_nowplaying to EPG shows (5-min buckets).
-- Supports pagination (p_limit/p_offset) so the shows table can lazy-load.
-- When p_show is provided, returns only that show's aggregate + timeline
-- (used by the timeline modal on demand).
-- =============================================================
create or replace function public.get_radio_show_snapshots(
  p_start     text default null,
  p_end       text default null,
  p_limit     integer default null,
  p_offset    integer default 0,
  p_show      text default null
) returns table (payload jsonb)
language plpgsql stable as $$
declare
  v_payload jsonb;
  v_total_shows int;
  v_ts_filter text;
begin
  if p_start is not null and p_start <> '' and p_end is not null and p_end <> '' then
    v_ts_filter := format('snapshot_at BETWEEN %L::timestamptz AND %L::timestamptz', p_start, p_end || ' 23:59:59');
  else
    v_ts_filter := 'snapshot_at > date_trunc(''day'', now())';
  end if;

  select coalesce(jsonb_agg(row ORDER BY (row->>'avgListeners')::int DESC NULLS LAST), '[]'::jsonb) into v_payload from (
    with np as (
      select n.snapshot_at, n.listeners_total, n.listeners_unique,
        extract(dow from n.snapshot_at at time zone 'UTC') + 1 as day_num,
        extract(hour from n.snapshot_at at time zone 'UTC')::int as hour,
        (floor(extract(minute from n.snapshot_at at time zone 'UTC') / 5) * 5)::int as bucket
      from public.radio_nowplaying n
      where (
        (p_start is not null and p_start <> '' and n.snapshot_at between p_start::timestamptz and (p_end || ' 23:59:59')::timestamptz)
        or (p_start is null or p_start = '') and n.snapshot_at > date_trunc('day', now())
      )
    ),
    lst as (
      select l.snapshot_at, l.is_hls, l.connected_time,
        extract(dow from l.snapshot_at at time zone 'UTC') + 1 as day_num,
        extract(hour from l.snapshot_at at time zone 'UTC')::int as hour,
        (floor(extract(minute from l.snapshot_at at time zone 'UTC') / 5) * 5)::int as bucket
      from public.radio_listeners l
      where (
        (p_start is not null and p_start <> '' and l.snapshot_at between p_start::timestamptz and (p_end || ' 23:59:59')::timestamptz)
        or (p_start is null or p_start = '') and l.snapshot_at > date_trunc('day', now())
      )
    ),
    np_mapped as (
      select n.*, e.program_name, e.thumbnail, e.presenter, e.genre, e.type, e.start_time, e.end_time, e.days, e.image
      from np n
      left join lateral (
        select program_name, thumbnail, presenter, genre, type, start_time, end_time, days, image
        from public.epg_programs_radio e
        where utc_min(e.start_time) <= n.hour*60 + n.bucket and utc_min(e.end_time) > n.hour*60 + n.bucket
          and e.days ~* ('(^|[,])' || to_char(n.snapshot_at at time zone 'UTC','FMDay') || '([,]|$)')
        order by utc_min(e.start_time) desc
        limit 1
      ) e on true
    ),
    lst_mapped as (
      select l.*, e.program_name, e.thumbnail
      from lst l
      left join lateral (
        select program_name, thumbnail from public.epg_programs_radio e
        where utc_min(e.start_time) <= l.hour*60 + l.bucket and utc_min(e.end_time) > l.hour*60 + l.bucket
          and e.days ~* ('(^|[,])' || to_char(l.snapshot_at at time zone 'UTC','FMDay') || '([,]|$)')
        order by utc_min(e.start_time) desc
        limit 1
      ) e on true
    ),
    agg_np as (
      select program_name, thumbnail, presenter, genre, type, start_time, end_time, days, image,
        count(*) as np_count,
        round(avg(listeners_total))::int as avg_total,
        max(listeners_total)::int as peak_total,
        round(avg(listeners_unique))::int as avg_unique,
        case when (p_show is not null and p_show <> '') then
          jsonb_agg(jsonb_build_object('t', to_char(snapshot_at at time zone 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'), 'v', listeners_total) ORDER BY snapshot_at)
        else '[]'::jsonb end as timeline
      from np_mapped
      where program_name is not null
      group by 1,2,3,4,5,6,7,8,9
    ),
    agg_lst as (
      select program_name, thumbnail,
        count(*) filter (where is_hls) as hls_count,
        count(*) filter (where not is_hls) as local_count,
        round(avg(connected_time))::int as avg_connected
      from lst_mapped
      where program_name is not null
      group by 1,2
    ),
    -- totalListeners = sum of each airing's daily peak (max concurrent audience
    -- across every time the show aired in the selected range).
    daily_peaks as (
      select program_name, (snapshot_at at time zone 'UTC')::date as date,
        max(listeners_total) as daily_peak
      from np_mapped
      where program_name is not null
      group by 1,2
    ),
    total_peaks as (
      select program_name, sum(daily_peak)::int as total_listeners,
        count(*)::int as airings
      from daily_peaks
      group by 1
    )
    select
      jsonb_build_object(
        'programName', a.program_name,
        'presenter', coalesce(a.presenter,''),
        'genre', coalesce(a.genre,''),
        'type', coalesce(a.type,''),
        'startTime', coalesce(a.start_time,''),
        'endTime', coalesce(a.end_time,''),
        'days', coalesce(a.days,''),
        'image', coalesce(a.image,''),
        'thumbnail', coalesce(a.thumbnail,''),
        'snapshots', a.np_count,
        'airings', coalesce(tp.airings, 0),
        'totalListeners', coalesce(tp.total_listeners, 0),
        'avgListeners', a.avg_total,
        'peakListeners', a.peak_total,
        'uniqueListeners', a.avg_unique,
        'hlsListeners', case when a.np_count > 0 and coalesce(l.hls_count,0)+coalesce(l.local_count,0) > 0
          then round((coalesce(l.hls_count,0)::numeric / (coalesce(l.hls_count,0)+coalesce(l.local_count,0))) * a.avg_total)::int
          else 0 end,
        'localListeners', case when a.np_count > 0 and coalesce(l.hls_count,0)+coalesce(l.local_count,0) > 0
          then round((coalesce(l.local_count,0)::numeric / (coalesce(l.hls_count,0)+coalesce(l.local_count,0))) * a.avg_total)::int
          else 0 end,
        'avgConnectedSeconds', coalesce(l.avg_connected,0),
        'timeline', coalesce(a.timeline, '[]'::jsonb)
      ) as row
    from agg_np a
    left join agg_lst l on l.program_name = a.program_name and l.thumbnail = a.thumbnail
    left join total_peaks tp on tp.program_name = a.program_name
    where (p_show is null or p_show = '' or a.program_name = p_show)
    order by a.avg_total desc nulls last
    limit coalesce(p_limit, 1000) offset coalesce(p_offset, 0)
  ) row;

  select count(*) into v_total_shows from (
    select distinct e.program_name
    from public.radio_nowplaying n
    left join lateral (
      select program_name from public.epg_programs_radio e
      where utc_min(e.start_time) <= (extract(hour from n.snapshot_at at time zone 'UTC'))*60
        + (floor(extract(minute from n.snapshot_at at time zone 'UTC') / 5) * 5)::int
        and utc_min(e.end_time) > (extract(hour from n.snapshot_at at time zone 'UTC'))*60
        + (floor(extract(minute from n.snapshot_at at time zone 'UTC') / 5) * 5)::int
        and e.days ~* ('(^|[,])' || to_char(n.snapshot_at at time zone 'UTC','FMDay') || '([,]|$)')
      order by utc_min(e.start_time) desc limit 1
    ) e on true
    where e.program_name is not null
      and n.snapshot_at between p_start::timestamptz and (p_end || ' 23:59:59')::timestamptz
  ) t;

  return query select jsonb_build_object(
    'shows', v_payload,
    'totalShows', v_total_shows,
    'totalPrograms', (select count(*) from public.epg_programs_radio)
  );
end;
$$;

-- =============================================================
-- get_radio_show_listener_details — getRadioShowListenerDetails
-- =============================================================
create or replace function public.get_radio_show_listener_details(
  p_start     text default null,
  p_end       text default null,
  p_show      text default null,
  p_page      integer default 1,
  p_page_size integer default 25
) returns table (payload jsonb)
language plpgsql volatile as $$
declare
  v_payload jsonb;
  v_ts_filter text;
  v_total bigint;
  v_listeners jsonb;
  v_program_id bigint;
begin
  if p_show is null or p_show = '' then
    return query select jsonb_build_object('error', 'showName param required');
    return;
  end if;
  if p_start is not null and p_start <> '' and p_end is not null and p_end <> '' then
    v_ts_filter := format('snapshot_at BETWEEN %L::timestamptz AND %L::timestamptz', p_start, p_end || ' 23:59:59');
  else
    v_ts_filter := 'snapshot_at > date_trunc(''day'', now())';
  end if;

  select id into v_program_id from public.epg_programs_radio where program_name = p_show limit 1;
  if v_program_id is null then
    return query select jsonb_build_object('showName', p_show, 'listeners', '[]'::jsonb, 'total', 0, 'page', p_page, 'pageSize', p_page_size, 'totalPages', 0);
    return;
  end if;

  -- Materialize matched listener rows (CTE is statement-scoped, so use a temp table).
  create temp table tmp_radio_matched on commit drop as
  with lst as (
    select l.snapshot_at, l.client_ip, l.is_hls, l.mount_name, l.connected_on, l.connected_time, l.browser_family, l.client, l.country, l.os_family
    from public.radio_listeners l
    where (
      (p_start is not null and p_start <> '' and l.snapshot_at between p_start::timestamptz and (p_end || ' 23:59:59')::timestamptz)
      or (p_start is null or p_start = '') and l.snapshot_at > date_trunc('day', now())
    )
  ),
  matched as (
    select l.* from lst l
    where (select program_name from public.epg_programs_radio e2
      where utc_min(e2.start_time) <= (extract(hour from l.snapshot_at at time zone 'UTC'))::int*60 + (floor(extract(minute from l.snapshot_at at time zone 'UTC') / 5) * 5)::int
        and utc_min(e2.end_time) > (extract(hour from l.snapshot_at at time zone 'UTC'))::int*60 + (floor(extract(minute from l.snapshot_at at time zone 'UTC') / 5) * 5)::int
        and e2.days ~* ('(^|[,])' || to_char(l.snapshot_at at time zone 'UTC','FMDay') || '([,]|$)')
      order by utc_min(e2.start_time) desc limit 1) = p_show
  )
  select * from matched;

  select count(*) into v_total from tmp_radio_matched;

  select coalesce(jsonb_agg(row ORDER BY time DESC), '[]'::jsonb) into v_listeners from (
    select
      coalesce(client_ip,'') as ip,
      coalesce(to_char(coalesce(connected_on, snapshot_at) at time zone 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'), '') as time,
      coalesce(nullif(browser_family,''), client, '') as user_agent,
      coalesce(nullif(mount_name,''), case when is_hls then 'HLS' else 'Local' end, '') as stream,
      coalesce(country,'') as location
    from tmp_radio_matched
    order by snapshot_at desc
    limit p_page_size offset (p_page - 1) * p_page_size
  ) row;

  drop table tmp_radio_matched;

  return query select jsonb_build_object(
    'showName', p_show,
    'listeners', v_listeners,
    'total', v_total,
    'page', p_page,
    'pageSize', p_page_size,
    'totalPages', ceil(v_total::numeric / nullif(p_page_size,0))
  );
end;
$$;

grant execute on function public.get_radio_show_analytics(integer, text, text) to postgres;
grant execute on function public.get_radio_show_snapshots(text, text) to postgres;
grant execute on function public.get_radio_show_listener_details(text, text, text, integer, integer) to postgres;
