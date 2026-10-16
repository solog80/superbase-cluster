-- tsdb_012_traffic_advertiser_rules_round3.sql
-- Round 3: obvious remaining variants (run on TSDB: analytics @ 100.116.185.70:55439).

insert into public.traffic_client_rules (client_name, match_pattern, category, priority)
select v.client_name, v.match_pattern, v.category, v.priority
from (values
  ('NRM Secretariate', 'NRM VOTE|NRM LOCAL|NRM LC|NRM 40|ELECTIONS NRM', 'Commercial', 100),
  ('Salt Media',       'BWE PYA|EKIRANGO|ENKYUKAKYUKA|KIRANGO',           'Commercial',  80)
) as v(client_name, match_pattern, category, priority)
where not exists (
  select 1 from public.traffic_client_rules r
  where r.client_name = v.client_name and r.match_pattern = v.match_pattern
);

create table if not exists public.radio_spot_logs_client_backup_r3 as
select played_at, log_hash, client as old_client
from public.radio_spot_logs
where client in ('Other Advert','Unknown','Unassigned')
  and public.resolve_traffic_client(title, filename, null) <> client;

update public.radio_spot_logs s
set client = public.resolve_traffic_client(s.title, s.filename, null)
where s.client in ('Other Advert','Unknown','Unassigned')
  and public.resolve_traffic_client(s.title, s.filename, null) <> s.client;
