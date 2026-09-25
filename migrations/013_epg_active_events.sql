-- Update get_today_epg RPC to inject active special events from the events table as interruptive programs.
CREATE OR REPLACE FUNCTION public.get_today_epg()
 RETURNS jsonb
 LANGUAGE sql
 STABLE
 SET search_path TO 'public'
AS $function$
  with day as (
    select
      trim(to_char((now() at time zone 'utc')::date, 'Day')) as today_name,
      trim(to_char(((now() at time zone 'utc')::date - 1), 'Day')) as yesterday_name
  ),
  active_events as (
    select
      e.id,
      e.title as program_name,
      coalesce(e.presenter, '') as presenter,
      coalesce(e.image_url, '') as image_url,
      coalesce(e.platform, 'both') as platform,
      coalesce(e.stations, '[]'::jsonb) as stations,
      coalesce(e.enable_chat, true) as enable_chat
    from public.events e
    where
      (e.enable_chat is null or e.enable_chat = true)
      and (now() at time zone 'utc') >= e.start_date
      and (now() at time zone 'utc') <= e.end_date
  ),
  filtered as (
    select p.*
    from public.epg_programs p, day
    where
      -- airs today
      position(day.today_name in coalesce(p.days, '')) > 0
      or
      -- crosses midnight from yesterday (end hour < start hour) AND airs yesterday
      (
        position(day.yesterday_name in coalesce(p.days, '')) > 0
        and split_part(p.end_time, ':', 1)::int < split_part(p.start_time, ':', 1)::int
      )
  ),
  radio_active_events as (
    select coalesce(
      jsonb_agg(
        jsonb_build_object(
          'tvProgramId', ae.id,
          'programName', ae.program_name,
          'presenter', ae.presenter,
          'genre', 'Special Event',
          'details', '',
          'language', 'English',
          'startTime', '00:00',
          'endTime', '23:59',
          'days', day.today_name,
          'type', 'interruptive',
          'isInterruptive', true,
          'shouldShow', true,
          'image', ae.image_url,
          'thumbnail', ae.image_url,
          'enableChat', ae.enable_chat
        )
      ),
      '[]'::jsonb
    ) as ev_programs
    from active_events ae, day
    where ae.platform in ('radio', 'both')
  ),
  radio_programs as (
    select (
      (select ev_programs from radio_active_events) ||
      coalesce(
        jsonb_agg(
          jsonb_build_object(
            'tvProgramId', coalesce(nullif(f.tv_program_id, ''), regexp_replace(lower(f.station_id), '[^a-z0-9]+', '_', 'g') || '_' || regexp_replace(lower(f.program_name), '[^a-z0-9]+', '_', 'g') || '_' || replace(f.start_time, ':', '')),
            'programName', f.program_name,
            'presenter', f.presenter,
            'genre', f.genre,
            'details', f.details,
            'language', f.language,
            'startTime', f.start_time,
            'endTime', f.end_time,
            'days', coalesce(f.days, ''),
            'type', f.type,
            'image', f.image,
            'thumbnail', f.thumbnail,
            'enableChat', true
          )
          order by f.start_time
        ),
        '[]'::jsonb
      )
    ) as programs
    from filtered f
    join public.epg_stations st on st.id = f.station_id
    where st.lineup_type = 'radio' and st.is_visible = true
  ),
  tv_stations as (
    select
      st.id as station_id,
      st.station_url,
      st.is_pay_per_view,
      st.price,
      st.currency,
      st.is_live,
      (
        coalesce(
          (
            select jsonb_agg(
              jsonb_build_object(
                'tvProgramId', ae.id,
                'programName', ae.program_name,
                'presenter', ae.presenter,
                'genre', 'Special Event',
                'details', '',
                'language', 'English',
                'startTime', '00:00',
                'endTime', '23:59',
                'days', day.today_name,
                'type', 'interruptive',
                'isInterruptive', true,
                'shouldShow', true,
                'image', ae.image_url,
                'thumbnail', ae.image_url,
                'enableChat', ae.enable_chat
              )
            )
            from active_events ae, day
            where ae.platform in ('tv', 'both')
              and (jsonb_array_length(ae.stations) = 0 or ae.stations @> jsonb_build_array(st.id))
          ),
          '[]'::jsonb
        ) ||
        coalesce(
          (
            select jsonb_agg(
              jsonb_build_object(
                'tvProgramId', coalesce(nullif(f.tv_program_id, ''), regexp_replace(lower(f.station_id), '[^a-z0-9]+', '_', 'g') || '_' || regexp_replace(lower(f.program_name), '[^a-z0-9]+', '_', 'g') || '_' || replace(f.start_time, ':', '')),
                'programName', f.program_name,
                'presenter', f.presenter,
                'genre', f.genre,
                'details', f.details,
                'language', f.language,
                'startTime', f.start_time,
                'endTime', f.end_time,
                'days', coalesce(f.days, ''),
                'type', f.type,
                'image', f.image,
                'thumbnail', f.thumbnail,
                'enableChat', true
              )
              order by f.start_time
            )
            from filtered f
            where f.station_id = st.id
          ),
          '[]'::jsonb
        )
      ) as programs
    from public.epg_stations st
    where st.lineup_type = 'tv' and st.is_visible = true
  ),
  tv_agg as (
    select jsonb_object_agg(
      ts.station_id,
      jsonb_build_object(
        'stationImageUrl', null,
        'stationUrl', coalesce(ts.station_url, ''),
        'isPayPerView', ts.is_pay_per_view,
        'price', coalesce(ts.price::text, ''),
        'currency', coalesce(ts.currency, ''),
        'isLive', ts.is_live,
        'programs', ts.programs
      )
    ) as tv
    from tv_stations ts
    where jsonb_array_length(ts.programs) > 0
  )
  select jsonb_build_object(
    'data', jsonb_build_object(
      'tv', coalesce((select tv from tv_agg), '{}'::jsonb),
      'radio', coalesce((select jsonb_build_object(
        'stationUrl', coalesce(st.station_url, ''),
        'stationImageUrl', null,
        'isPayPerView', st.is_pay_per_view,
        'price', coalesce(st.price::text, ''),
        'currency', coalesce(st.currency, ''),
        'isLive', st.is_live,
        'programs', coalesce((select programs from radio_programs), '[]'::jsonb)
      ) from public.epg_stations st where st.lineup_type = 'radio' and st.is_visible = true limit 1), '{}'::jsonb)
    )
  );
$function$;
