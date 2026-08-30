-- 008_search_catalog_rpc.sql
-- Salt TV → Supabase: app-facing catalog search exposed as a PostgREST RPC so it
-- can be read via /rest/v1 (CDN-cached + geo-routed to replicas), matching the
-- app's read path. Uses pg_trgm similarity ranking on tv_shows (title, description)
-- and epg_programs (program_name, presenter). Returns the same row shape the app
-- already parses for the catalog.
--
-- App calls: GET /rest/v1/rpc/search_catalog?q=<term>&type=<opt>&limit=20&offset=0
--
-- Run on the Supabase primary (us1). DDL replicates to all replicas; restart
-- supabase-rest-replica on ug/eu1/eu2/us2 afterward (Gotcha #7).

create extension if not exists pg_trgm;

create or replace function public.search_catalog(
  q text default '',
  vtype text default null,
  vlimit int default 20,
  voffset int default 0
)
returns jsonb
language plpgsql
stable
security invoker
set search_path = public
as $$
declare
  term text := coalesce(nullif(trim(q), ''), '');
  lim int  := greatest(1, least(coalesce(vlimit, 20), 100));
  off int  := greatest(0, coalesce(voffset, 0));
  result jsonb;
begin
  if term = '' then
    -- No query: return published shows, optionally type-filtered, newest first.
    select jsonb_agg(row_to_json(t)) into result
    from (
      select id, title, type, description, thumbnail,
             poster_url_16x9 as "posterUrl16x9",
             poster_url_2x3  as "posterUrl2x3",
             season_count    as "seasonCount",
             published
      from tv_shows
      where published = true
        and (vtype is null or type = vtype)
      order by created_at desc
      limit lim offset off
    ) t;
  else
    -- Search: rank by trigram similarity on title/description; fall back to
    -- ilike containment so every term still matches something.
    select jsonb_agg(row_to_json(t)) into result
    from (
      with scored as (
        select s.id, s.title, s.type, s.description, s.thumbnail,
               s.poster_url_16x9 as "posterUrl16x9",
               s.poster_url_2x3  as "posterUrl2x3",
               s.season_count    as "seasonCount",
               s.published,
               greatest(
                 similarity(s.title, term),
                 similarity(coalesce(s.description, ''), term)
               ) as sim,
               (s.title ilike '%' || term || '%' or
                coalesce(s.description, '') ilike '%' || term || '%') as contains
        from tv_shows s
        where s.published = true
          and (vtype is null or s.type = vtype)
          and (
                similarity(s.title, term) > 0.2
             or similarity(coalesce(s.description, ''), term) > 0.2
             or s.title ilike '%' || term || '%'
             or coalesce(s.description, '') ilike '%' || term || '%'
          )
      )
      select id, title, type, description, thumbnail,
             "posterUrl16x9", "posterUrl2x3", "seasonCount", published
      from scored
      order by contains desc, sim desc, title asc
      limit lim offset off
    ) t;
  end if;

  return coalesce(result, jsonb_build_array());
end;
$$;
