-- tsdb_015_traffic_title_overrides.sql
-- Persistent per-creative title overrides. The StationPlaylist Title field for
-- some spots is not the creative name (e.g. "Sting Fik 2026 - Lug.mp3" logs its
-- title as "Lug"). This lets us correct the displayed/affidavit creative title
-- scoped to a file/title pattern, and it applies to future airings too.
-- Run on TSDB: analytics @ 100.116.185.70:55439.

create table if not exists public.traffic_title_overrides (
  id             integer generated always as identity primary key,
  match_pattern  text not null,      -- case-insensitive regex over title+filename
  title_override text not null,
  priority       integer not null default 0,
  created_at     timestamptz not null default now()
);

create index if not exists traffic_title_overrides_prio_idx
  on public.traffic_title_overrides (priority desc, id asc);

-- Helper: resolve the creative title (override wins, else the original).
create or replace function public.resolve_traffic_title(
  p_title    text,
  p_filename text
) returns text
language sql stable as $$
  select coalesce(
    (select title_override
       from public.traffic_title_overrides
      where (coalesce(p_title, '') || ' ' || coalesce(p_filename, '')) ~* match_pattern
      order by priority desc, id asc
      limit 1),
    p_title
  );
$$;

-- This specific creative should display as "Sting".
insert into public.traffic_title_overrides (match_pattern, title_override, priority)
select 'STING FIK', 'Sting', 100
where not exists (
  select 1 from public.traffic_title_overrides where match_pattern = 'STING FIK'
);

-- Backfill existing rows (with a rollback snapshot).
create table if not exists public.radio_spot_logs_title_backup_20261016 as
select played_at, log_hash, title as old_title
from public.radio_spot_logs
where public.resolve_traffic_title(title, filename) is distinct from title;

update public.radio_spot_logs s
set title = public.resolve_traffic_title(s.title, s.filename)
where public.resolve_traffic_title(s.title, s.filename) is distinct from s.title;

-- Rollback:
-- update public.radio_spot_logs s set title = b.old_title
-- from public.radio_spot_logs_title_backup_20261016 b
-- where s.played_at = b.played_at and s.log_hash = b.log_hash;
