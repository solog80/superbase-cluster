-- 004_ondemand.sql
-- Salt TV → Supabase: on-demand catalog (tv_shows → seasons → episodes).
-- Normalized so the Go on-demand readers can serve the full nested payload
-- without Firestore. Run on the Supabase primary (us1) via psql.

-- =============================================================
-- Cleanup: the smoke-trial load flattened season docs into tv_shows
-- (27 rows = 7 real shows + 20 season rows). Delete the season rows
-- so tv_shows only contains shows; seasons/episodes get their own tables.
-- Keeps only the 7 real shows that exist in Firestore's tv_shows doc.
-- =============================================================
delete from public.tv_shows
 where id not in (
   'connected_c9bc07','dj_jemo_3e95fa','nyumiza_e4d5b3',
   'pati_africa_gang_7806d8','sports_cea70037','sway_8d811e','the_gang_bf8c6b'
 );

-- =============================================================
-- seasons — one row per show sub-collection doc
-- =============================================================
create table if not exists public.seasons (
  id           text primary key,          -- 'connected_2026-05', 'the_gang_bf8c6b_s01'
  show_id      text not null references public.tv_shows(id) on delete cascade,
  title        text not null,
  ord          integer not null default 1,
  episode_count integer not null default 0,
  published    boolean not null default true,
  created_at   timestamptz not null default now(),
  updated_at   timestamptz not null default now()
);
create index if not exists seasons_show_idx on public.seasons (show_id);

-- =============================================================
-- episodes — flattened season sub-collection docs
-- =============================================================
create table if not exists public.episodes (
  id           text primary key,          -- 'connected_2026-05_ep01'
  season_id    text not null references public.seasons(id) on delete cascade,
  show_id      text not null references public.tv_shows(id) on delete cascade,
  title        text not null,
  description  text not null default '',
  duration     integer not null default 0,
  thumbnail    text,
  video_url    text,
  date_uploaded timestamptz,
  air_date     timestamptz,
  published    boolean not null default true,
  processing   boolean not null default false,
  sfx_job_name text,
  server       text,                      -- 'sfx' | 'bunny'
  bunny_guid   text,
  created_at   timestamptz not null default now(),
  updated_at   timestamptz not null default now()
);
create index if not exists episodes_season_idx on public.episodes (season_id);
create index if not exists episodes_show_idx   on public.episodes (show_id);

-- =============================================================
-- RLS: anon + service_role read on-demand (public read path via CDN);
-- service_role owns writes (admin). Mirrors tv_shows anon-read.
-- =============================================================
alter table public.seasons  enable row level security;
alter table public.episodes enable row level security;

create policy seasons_read on public.seasons for select to anon, service_role using (true);
create policy episodes_read on public.episodes for select to anon, service_role using (true);
create policy seasons_admin_all on public.seasons for all to service_role using (true) with check (true);
create policy episodes_admin_all on public.episodes for all to service_role using (true) with check (true);

grant select on public.seasons, public.episodes to anon, service_role;
grant insert, update, delete on public.seasons, public.episodes to service_role;
