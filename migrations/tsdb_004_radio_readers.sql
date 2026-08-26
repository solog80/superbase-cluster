-- tsdb_004_radio_readers.sql
-- Salt TV → TimescaleDB: radio reader RPCs — replaces the getRadio* functions
-- in functions/src/azuracast.js. Each returns the exact JSON payload the admin
-- dashboard expects. EPG show resolution reads from Supabase epg_programs via
-- postgres_fdw-free approach: the RPCs accept EPG programs passed by the Go
-- layer is NOT used — instead they query a local epg mirror table.
-- NOTE: this relies on an `epg_programs` mirror on TSDB (see load step).

-- =============================================================
-- get_radio_history — getRadioHistory
-- =============================================================
create or replace function public.get_radio_history(
  p_days     integer default 7,
  p_limit    integer default 100,
  p_offset   integer default 0,
  p_search   text default null,
  p_sort     text default 'played_at',
  p_sort_dir text default 'DESC'
) returns table (payload jsonb)
language plpgsql stable as $$
declare
  v_payload jsonb;
  v_where   text := '';
  v_ts      timestamptz := now() - (p_days || ' days')::interval;
  v_sort    text;
  v_total   bigint;
begin
  if p_search is not null and p_search <> '' then
    v_where := format(' AND (lower(coalesce(song_title,'''')) LIKE %L OR lower(coalesce(song_artist,'''')) LIKE %L OR lower(coalesce(song_text,'''')) LIKE %L OR lower(coalesce(playlist,'''')) LIKE %L)',
      '%' || lower(p_search) || '%', '%' || lower(p_search) || '%', '%' || lower(p_search) || '%', '%' || lower(p_search) || '%');
  end if;

  p_sort := coalesce(p_sort, 'played_at');
  p_sort_dir := coalesce(upper(p_sort_dir), 'DESC');
  case p_sort
    when 'song_title' then v_sort := 'song_title';
    when 'song_artist' then v_sort := 'song_artist';
    when 'listeners_start' then v_sort := 'listeners_start';
    when 'listeners_end' then v_sort := 'listeners_end';
    when 'delta_total' then v_sort := 'delta_total';
    when 'duration_seconds' then v_sort := 'duration_seconds';
    else v_sort := 'played_at';
  end case;
  if p_sort_dir <> 'ASC' then p_sort_dir := 'DESC'; end if;

  execute 'SELECT count(*) FROM public.radio_history WHERE played_at > $1' || v_where
    into v_total using v_ts;

  execute 'SELECT coalesce(jsonb_agg(row ORDER BY ' || v_sort || ' ' || p_sort_dir || ', sh_id DESC), ''[]''::jsonb) FROM (' ||
    'SELECT sh_id, station_id, station_name, played_at, duration_seconds, playlist, streamer, is_request,' ||
    ' song_id, song_artist, song_title, song_text, song_album, song_genre,' ||
    ' listeners_start, listeners_end, delta_total, is_visible' ||
    ' FROM public.radio_history WHERE played_at > $1' || v_where ||
    ' ORDER BY ' || v_sort || ' ' || p_sort_dir || ', sh_id DESC LIMIT ' || p_limit || ' OFFSET ' || p_offset || ') row'
    into v_payload using v_ts;

  return query select jsonb_build_object(
    'data', v_payload,
    'total', v_total,
    'limit', p_limit,
    'offset', p_offset
  );
end;
$$;

-- =============================================================
-- get_radio_country_details — getRadioCountryDetails
-- =============================================================
create or replace function public.get_radio_country_details(
  p_country text default null,
  p_limit   integer default 20
) returns table (payload jsonb)
language plpgsql stable as $$
declare
  v_total bigint;
  v_by_stream jsonb;
  v_by_browser jsonb;
  v_by_os jsonb;
  v_by_duration jsonb;
begin
  if p_country is null or p_country = '' then
    return query select jsonb_build_object('error', 'country param required');
    return;
  end if;

  select count(*) into v_total from public.radio_listeners where country = p_country;

  select coalesce(jsonb_agg(row ORDER BY count desc), '[]'::jsonb) into v_by_stream
    from (select is_hls, count(*) as count from public.radio_listeners where country = p_country group by is_hls) row;

  select coalesce(jsonb_agg(row ORDER BY count desc), '[]'::jsonb) into v_by_browser
    from (select browser_family, count(*) as count from public.radio_listeners where country = p_country and browser_family <> '' group by browser_family) row;

  select coalesce(jsonb_agg(row ORDER BY count desc), '[]'::jsonb) into v_by_os
    from (select os_family, count(*) as count from public.radio_listeners where country = p_country and os_family <> '' group by os_family) row;

  select coalesce(jsonb_agg(row ORDER BY min_ct asc), '[]'::jsonb) into v_by_duration
    from (
      select case
        when connected_time < 60 then '<1m'
        when connected_time < 300 then '1-5m'
        when connected_time < 900 then '5-15m'
        when connected_time < 3600 then '15-60m'
        else '60m+' end as duration,
        count(*) as count, min(connected_time) as min_ct
      from public.radio_listeners
      where country = p_country
      group by duration
    ) row;

  return query select jsonb_build_object(
    'country', p_country,
    'total', v_total,
    'byStream', v_by_stream,
    'byBrowser', v_by_browser,
    'byOS', v_by_os,
    'byDuration', v_by_duration
  );
end;
$$;

grant execute on function public.get_radio_history(integer, integer, integer, text, text, text) to postgres;
grant execute on function public.get_radio_country_details(text, integer) to postgres;
