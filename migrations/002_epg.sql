-- 002_epg.sql
-- Salt TV → Supabase: EPG program lineups (from Firebase fm_program_lineup).
-- Normalized so the Go radio/TV readers can resolve show names without Firestore.
-- Run on the Supabase primary (us1) via psql.

-- =============================================================
-- epg_stations — one row per lineup doc/station key
-- =============================================================
create table if not exists public.epg_stations (
  id          text primary key,          -- 'Live_Radio' | 'Live_TV' | 'Salt TV One' | ...
  lineup_type text not null default 'radio',  -- 'radio' | 'tv'
  station_url text,
  is_live     boolean default false,
  is_visible  boolean default true,
  is_pay_per_view boolean default false,
  price       double precision,
  currency    text,
  enable_chat boolean default true,
  updated_at  timestamptz not null default now()
);

-- =============================================================
-- epg_programs — flattened program lineups (denormalized)
-- =============================================================
create table if not exists public.epg_programs (
  id          bigint generated always as identity primary key,
  station_id  text not null references public.epg_stations(id) on delete cascade,
  program_name text not null,
  presenter   text,
  genre       text,
  details     text,
  language    text,
  start_time  text not null,             -- 'HH:MM' UTC
  end_time    text not null,
  days        text,                      -- 'Monday,Tuesday,...' (comma list)
  type        text,                      -- radio program type (news/music/...)
  image       text,
  thumbnail   text,
  target_audience text,
  tv_program_id text,
  updated_at  timestamptz not null default now()
);
create index if not exists epg_programs_station_idx on public.epg_programs (station_id);
create index if not exists epg_programs_name_idx  on public.epg_programs (program_name);

-- =============================================================
-- RLS: anon + service_role can read EPG (public data)
-- =============================================================
alter table public.epg_stations enable row level security;
alter table public.epg_programs enable row level security;

create policy epg_stations_read on public.epg_stations for select to anon, service_role using (true);
create policy epg_programs_read on public.epg_programs for select to anon, service_role using (true);

grant select on public.epg_stations to anon, service_role;
grant select on public.epg_programs to anon, service_role;
