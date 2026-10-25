-- 014_live_ad_settings.sql
-- Global Live TV ad pacing. Read by the mobile apps; written from the admin
-- dashboard through the Next.js service-role proxy.
--
-- Live ads are client-side overlays only: they never rewrite the HLS manifest
-- and do not require OME Enterprise/SSAI.
--
-- Run on the Supabase primary (us1) via psql.

create table if not exists public.ad_settings (
  id                            text primary key default 'global',
  live_ads_enabled              boolean not null default true,
  live_interval_minutes         integer not null default 30,
  live_max_per_session          integer not null default 2,
  live_skip_after_seconds       integer not null default 5,
  live_skip_min_duration_seconds integer not null default 12,
  updated_at                    timestamptz not null default now()
);

insert into public.ad_settings (id)
values ('global')
on conflict (id) do nothing;

-- Read-only for devices; writes are service-role only.
alter table public.ad_settings enable row level security;

drop policy if exists ad_settings_read on public.ad_settings;
create policy ad_settings_read on public.ad_settings
  for select to anon, service_role using (true);

grant select on public.ad_settings to anon;
grant all on public.ad_settings to service_role;

-- Make sure PostgREST sees the new table immediately on the primary.
notify pgrst, 'reload schema';

-- Keep updated_at fresh.
create or replace function public.ad_settings_touch_updated_at()
returns trigger
language plpgsql
as $$
begin
  new.updated_at := now();
  return new;
end;
$$;

drop trigger if exists ad_settings_touch on public.ad_settings;
create trigger ad_settings_touch
before update on public.ad_settings
for each row execute function public.ad_settings_touch_updated_at();
