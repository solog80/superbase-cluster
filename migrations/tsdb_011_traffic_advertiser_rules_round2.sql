-- tsdb_011_traffic_advertiser_rules_round2.sql
-- Round 2: advertiser mappings confirmed by the traffic officer for the
-- remaining "Other Advert" brands (run on TSDB: analytics @ 100.116.185.70:55439).

-- =============================================================
-- 1. Extra rules (idempotent).
-- =============================================================
insert into public.traffic_client_rules (client_name, match_pattern, category, priority)
select v.client_name, v.match_pattern, v.category, v.priority
from (values
  ('Haris',                  'BK 2026|ONER|BOOM',            'Commercial', 100),
  ('USAID',                   'VACCINATION',                  'Commercial', 100),
  ('URA',                     'WAIVER',                       'Commercial', 100),
  ('Damasco',                 'VERY WELLO',                   'Commercial', 100),
  ('Crown Beverages',         'NIVANA',                       'Commercial', 100),
  ('HPMI',                    'INDEPENDENCE NIGHT',           'Commercial', 100),
  ('PLU',                     'MK RUN',                       'Commercial', 100),
  ('LOFTY COM',               'LOFTY',                        'Commercial', 100),
  ('White Angels',            'WAHSK|MASAJJA|BULENGA',        'Commercial', 100),
  ('Deposit Protection Fund', 'DPF|DEPOSIT PROTECTION',       'Commercial', 100),
  ('Finance Trust Bank',      'FINANCE LUGANDA',              'Commercial', 100),
  ('Ministry of Defence',     'VOTE YKM',                     'Commercial', 100),
  ('Salt Media',              'BWE PYA|EKIRANGO',             'Commercial',  80),
  ('Sinapi',                  'GROW PROJECT',                 'Commercial', 100),
  ('NRM Secretariate',        'NRM 40 YEARS|ELECTIONS NRM',   'Commercial', 100)
) as v(client_name, match_pattern, category, priority)
where not exists (
  select 1 from public.traffic_client_rules r
  where r.client_name = v.client_name and r.match_pattern = v.match_pattern
);

-- =============================================================
-- 2. Broaden Electoral Commission to catch its announcement spots.
-- =============================================================
update public.traffic_client_rules
set match_pattern = 'ELECTORAL COMMISSION|EC ELECTIONS|EC WOMEN|EC ANNOUNCEMENT'
where client_name = 'Electoral Commission'
  and match_pattern = 'ELECTORAL COMMISSION|EC ELECTIONS|EC WOMEN';

-- =============================================================
-- 3. Rename Finance Trust → official advertiser "Finance Trust Bank".
-- =============================================================
update public.traffic_client_rules
set client_name = 'Finance Trust Bank'
where client_name = 'Finance Trust' and match_pattern = 'FINANCE ?TRUST';

-- =============================================================
-- 4. Backfill (snapshot for rollback).
-- =============================================================
create table if not exists public.radio_spot_logs_client_backup_r2 as
select played_at, log_hash, client as old_client
from public.radio_spot_logs
where (client in ('Other Advert','Unknown','Unassigned')
        and public.resolve_traffic_client(title, filename, null) <> client)
   or client = 'Finance Trust';

update public.radio_spot_logs s
set client = case
    when s.client = 'Finance Trust' then 'Finance Trust Bank'
    else public.resolve_traffic_client(s.title, s.filename, null)
  end
where (s.client in ('Other Advert','Unknown','Unassigned')
        and public.resolve_traffic_client(s.title, s.filename, null) <> s.client)
   or s.client = 'Finance Trust';

-- Rollback:
-- update public.radio_spot_logs s set client = b.old_client
-- from public.radio_spot_logs_client_backup_r2 b
-- where s.played_at = b.played_at and s.log_hash = b.log_hash;
