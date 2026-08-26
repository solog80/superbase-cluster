-- 006_read_views.sql
-- Salt TV → Supabase: read-path RPC functions so the app can read via /rest/v1
-- (CDN-cached, geo-routed to replicas) instead of /api/v1 (Go service on us1).
-- Each function returns the EXACT camelCase shape the Flutter app already parses,
-- so the app only needs to change where it fetches from, not how it parses.
-- Run on the Supabase primary (us1) via psql. DDL replicates to all replicas.
-- After applying, restart supabase-rest-replica on ug/eu1/eu2/us2 to refresh
-- their PostgREST schema caches (see SUPABASE_PLAN.md Gotcha #7).
--
-- Implemented as RPC functions (not views) because PostgREST v14 does not expose
-- newly-created views through the pooler without a restart, while RPC functions
-- appear in the schema cache immediately.

-- =============================================================
-- ON-DEMAND CATALOG — returns { data: { shows: [ ... ] } }
-- Published-only at every level (matches buildOnDemandPayload publishedOnly).
-- =============================================================
create or replace function public.get_on_demand_catalog()
returns jsonb
language sql
stable
security invoker
set search_path = public
as $$
  select jsonb_build_object(
    'data',
    jsonb_build_object(
      'shows',
      coalesce(
        (
          select jsonb_agg(
            jsonb_build_object(
              'id', s.id,
              'title', s.title,
              'type', s.type,
              'description', s.description,
              'thumbnail', s.thumbnail,
              'posterUrl16x9', s.poster_url_16x9,
              'posterUrl2x3', s.poster_url_2x3,
              'seasonCount', s.season_count,
              'createdAt', s.created_at,
              'bunnyGuid', s.bunny_guid,
              'published', s.published,
              'seasons', (
                select coalesce(
                  jsonb_agg(
                    jsonb_build_object(
                      'id', se.id,
                      'title', se.title,
                      'order', se.ord,
                      'episodeCount', (
                        select count(*) from public.episodes e
                        where e.season_id = se.id and e.published = true
                      ),
                      'published', se.published,
                      'episodes', (
                        select coalesce(
                          jsonb_agg(
                            jsonb_build_object(
                              'id', e.id,
                              'title', e.title,
                              'description', e.description,
                              'duration', e.duration,
                              'thumbnail', e.thumbnail,
                              'videoUrl', e.video_url,
                              'dateUploaded', e.date_uploaded,
                              'airDate', e.air_date,
                              'published', e.published,
                              'processing', e.processing,
                              'sfxJobName', e.sfx_job_name
                            ) order by e.air_date
                          ),
                          '[]'::jsonb
                        )
                        from public.episodes e
                        where e.season_id = se.id and e.published = true
                      )
                    ) order by se.ord
                  ),
                  '[]'::jsonb
                )
                from public.seasons se
                where se.show_id = s.id and se.published = true
              )
            ) order by s.created_at
          )
          from public.tv_shows s
          where s.published = true
            -- A show only appears when it has at least one published
            -- episode. Unpublishing all episodes hides the show entirely.
            and exists (
              select 1
              from public.seasons se
              join public.episodes e
                on e.season_id = se.id and e.published = true
              where se.show_id = s.id and se.published = true
            )
        ),
        '[]'::jsonb
      )
    )
  );
$$;

grant execute on function public.get_on_demand_catalog() to anon, service_role;

-- =============================================================
-- EVENTS — returns { events: [ ... ] } ordered start_date desc
-- =============================================================
create or replace function public.get_events_payload()
returns jsonb
language sql
stable
security invoker
set search_path = public
as $$
  select jsonb_build_object(
    'events',
    coalesce(
      (
        select jsonb_agg(jsonb_build_object(
          'id', e.id,
          'title', e.title,
          'imageUrl', e.image_url,
          'presenter', e.presenter,
          'startDate', e.start_date,
          'endDate', e.end_date,
          'platform', e.platform,
          'stations', e.stations
        ) order by e.start_date desc)
        from public.events e
      ),
      '[]'::jsonb
    )
  );
$$;

grant execute on function public.get_events_payload() to anon, service_role;

-- =============================================================
-- TODAY'S EPG — returns { data: { tv, radio } }.
-- Replicates buildEPGPayload: visible stations only, TV stations only when they
-- have today's programs, radio as a single object. Today filter includes
-- midnight-crossover programs from yesterday (end < start).
-- =============================================================
create or replace function public.get_today_epg()
returns jsonb
language sql
stable
security invoker
set search_path = public
as $$
  with day as (
    select
      to_char((now() at time zone 'utc')::date, 'FMDay') as today_name,
      to_char(((now() at time zone 'utc')::date - 1), 'FMDay') as yesterday_name
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
  radio_programs as (
    select jsonb_agg(
      jsonb_build_object(
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
        'thumbnail', f.thumbnail
      )
      order by f.start_time
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
      coalesce(
        (
          select jsonb_agg(
            jsonb_build_object(
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
              'thumbnail', f.thumbnail
            )
            order by f.start_time
          )
          from filtered f
          where f.station_id = st.id
        ),
        '[]'::jsonb
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
    'data',
    jsonb_build_object(
      'tv', coalesce((select tv from tv_agg), '{}'::jsonb),
      'radio',
      jsonb_build_object(
        'stationUrl', coalesce((select station_url from public.epg_stations where lineup_type = 'radio' and is_visible = true limit 1), ''),
        'stationImageUrl', null,
        'isPayPerView', coalesce((select is_pay_per_view from public.epg_stations where lineup_type = 'radio' and is_visible = true limit 1), false),
        'price', coalesce((select price::text from public.epg_stations where lineup_type = 'radio' and is_visible = true limit 1), ''),
        'currency', coalesce((select currency from public.epg_stations where lineup_type = 'radio' and is_visible = true limit 1), ''),
        'isLive', coalesce((select is_live from public.epg_stations where lineup_type = 'radio' and is_visible = true limit 1), false),
        'programs', coalesce((select programs from radio_programs), '[]'::jsonb)
      )
    )
  );
$$;

grant execute on function public.get_today_epg() to anon, service_role;