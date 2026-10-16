-- tsdb_013_traffic_advertiser_rules_round4.sql
-- Round 4: Health Tip spots and "Sting Fik 2026" belong to Haris
-- (run on TSDB: analytics @ 100.116.185.70:55439).

-- 1. Health tips → Haris (reuse the existing rule, keep the pattern).
update public.traffic_client_rules
set client_name = 'Haris'
where client_name = 'Health Tip (Sponsored)' and match_pattern = 'HEALTH TIP';

-- 2. Add "Sting Fik" to the Haris match patterns.
update public.traffic_client_rules
set match_pattern = 'BK 2026|ONER|BOOM|STING FIK'
where client_name = 'Haris' and match_pattern = 'BK 2026|ONER|BOOM';

-- 3. Snapshot for rollback, then relabel + backfill.
create table if not exists public.radio_spot_logs_client_backup_r4 as
select played_at, log_hash, client as old_client
from public.radio_spot_logs
where client = 'Health Tip (Sponsored)'
   or (client in ('Other Advert','Unknown','Unassigned')
       and public.resolve_traffic_client(title, filename, null) <> client);

update public.radio_spot_logs
set client = 'Haris'
where client = 'Health Tip (Sponsored)';

update public.radio_spot_logs s
set client = public.resolve_traffic_client(s.title, s.filename, null)
where s.client in ('Other Advert','Unknown','Unassigned')
  and public.resolve_traffic_client(s.title, s.filename, null) <> s.client;
