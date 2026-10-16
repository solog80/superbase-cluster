-- tsdb_010_traffic_advertiser_rules.sql
-- StationPlaylist traffic → refine advertiser attribution for spots that fell
-- into the generic "Other Advert" bucket (run on TSDB: analytics @ 100.116.185.70:55439).
--
-- Why: spots dragged from F:\Adverts\ often arrive with an empty Studio Client
-- column. The resolver matches title+filename against public.traffic_client_rules
-- and, when nothing matches, labels the row "Other Advert". The Go ingest path
-- now reads this same table at runtime, so a rule added here takes effect
-- immediately (no redeploy) and this script fixes already-ingested rows.

-- =============================================================
-- 1. Advertiser rules (idempotent — safe to re-run).
--    Priority 100 = commercial; 80 = house/PSA style.
-- =============================================================
insert into public.traffic_client_rules (client_name, match_pattern, category, priority)
select v.client_name, v.match_pattern, v.category, v.priority
from (values
  -- Broadcast / print / government
  ('UPPC',                     'UPPC',                                   'Commercial', 100),
  ('UPDF',                     'UPDF',                                   'Commercial', 100),
  ('Ministry of Defence',      'MINISTRY OF DEFENCE',                    'Commercial', 100),
  ('Electoral Commission',     'ELECTORAL COMMISSION|EC ELECTIONS|EC WOMEN', 'Commercial', 100),
  ('ICPAU',                    'ICPAU',                                  'Commercial', 100),
  ('Rotary',                   'ROTARY',                                 'Commercial', 100),
  ('Buganda Royal',            'BUGANDA ROYAL',                          'Commercial', 100),
  ('Justice for All',          'JUSTICE FOR ALL',                        'Commercial', 100),
  ('Kwezi Concert',            'KWEZI CONCERT',                          'Commercial',  90),
  ('Nick Vujicic',             'NICK VUJICIC',                           'Commercial',  90),
  ('Bethel Covenant College',  'BETHEL COVENANT',                        'Commercial', 100),
  ('HPMI',                     'HPMI',                                   'Commercial', 100),
  -- Media / telecoms
  ('StarTimes',                'STARTIMES|STAR TIMES',                   'Commercial', 100),
  -- Beverages (brand-level, matching existing Minute Maid convention)
  ('Pepsi',                    'PEPSI',                                  'Commercial', 100),
  ('Mirinda',                  'MIRINDA',                                'Commercial', 100),
  ('Mountain Dew',             'MOUNTAIN DEW',                           'Commercial', 100),
  ('Evervess',                 'EVERVESS',                               'Commercial', 100),
  ('Aquafina',                 'AQUAFINA',                               'Commercial', 100),
  ('7Up',                      '7UP',                                    'Commercial', 100),
  -- Financial
  ('Finance Trust',            'FINANCE ?TRUST',                          'Commercial', 100),
  ('Malisu Apollo Investments','MALISU APOLLO',                           'Commercial', 100),
  -- Motors (Studio occasionally misspells KUBERA as KUBEERA)
  ('Kubera Motors',            'KUBE+RA',                                'Commercial', 100),
  -- Services / retail / personal bookings
  ('Dorah''s',                 'DORAH',                                  'Commercial',  90),
  ('St Elizabeth',             'ST ?ELIZABETH',                          'Commercial',  90),
  ('La Mistique',              'MISTIQUE',                               'Commercial',  90),
  ('Lillian',                  'LILLIAN',                                'Commercial',  90),
  ('Eng. Brian',               'ENG BRIAN',                              'Commercial',  90),
  ('Joseph Ngoma',             'JOSEPH NGOMA',                           'Commercial',  90),
  -- House
  ('Salt Media (House Promo)', 'SALT EKYUKA|PROGRAM CHANGE',             'House Promo', 80)
) as v(client_name, match_pattern, category, priority)
where not exists (
  select 1 from public.traffic_client_rules r
  where r.client_name = v.client_name and r.match_pattern = v.match_pattern
);

-- =============================================================
-- 2. Backfill: re-resolve generic buckets using the updated rules.
--    A one-time snapshot of the previous value is kept for rollback.
-- =============================================================
create table if not exists public.radio_spot_logs_client_backup_20261016 as
select played_at, log_hash, client as old_client
from public.radio_spot_logs
where client in ('Other Advert', 'Unknown', 'Unassigned')
  and public.resolve_traffic_client(title, filename, null) <> client;

update public.radio_spot_logs s
set client = public.resolve_traffic_client(s.title, s.filename, null)
where s.client in ('Other Advert', 'Unknown', 'Unassigned')
  and public.resolve_traffic_client(s.title, s.filename, null) <> s.client;

-- =============================================================
-- 3. Rollback (if ever needed):
--   update public.radio_spot_logs s
--   set client = b.old_client
--   from public.radio_spot_logs_client_backup_20261016 b
--   where s.played_at = b.played_at and s.log_hash = b.log_hash;
-- =============================================================
