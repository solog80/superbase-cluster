-- 003_events.sql
-- Salt TV → Supabase: events (from Firebase `events` collection, epg.js CRUD).
-- Run on the Supabase primary (us1) via psql.

create table if not exists public.events (
  id         text primary key default gen_random_uuid()::text,
  title      text not null,
  image_url  text,
  presenter  text,
  start_date timestamptz not null,
  end_date   timestamptz not null,
  platform   text not null default 'both',   -- 'tv' | 'radio' | 'both'
  stations   jsonb not null default '[]'::jsonb,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

create index if not exists events_platform_idx on public.events (platform, start_date desc);

alter table public.events enable row level security;

-- anon: can read events (public)
create policy events_anon_read on public.events for select to anon using (true);

-- service_role: full access
create policy events_svc_all on public.events for all to service_role using (true) with check (true);

grant select on public.events to anon, service_role;
grant insert, update, delete on public.events to service_role;
