# replica-configs

Per-node configuration for the Salt TV Supabase mesh. These files were **lost during
the QNAP power outage** (Aug 2026) — keeping them here makes a wiped config a one-command
restore instead of an incident-time reconstruction. See `SUPABASE_PLAN.md` §12 + §14.

| Path | Node | What it is |
|---|---|---|
| `us1/99-realtime.conf` | us1 (Edge) | Postgres overrides (`max_replication_slots=10`, `max_wal_senders=20`) → `/etc/postgresql-custom/conf.d/99-realtime.conf` |
| `us1/envoy-cds.yaml` | us1 (Edge) | Envoy clusters (geo + failover priorities) → `/opt/supabase/supabase/docker/volumes/api/envoy/cds.yaml` |
| `us1/envoy-lds.template.yaml` | us1 (Edge) | Envoy routes (incl. `rest-v1-svc-read`) → `.../volumes/api/envoy/lds.template.yaml` |
| `us1/supabase-shield.vcl` | us1 (Edge) | Varnish origin shield (websocket pipe + caching) → `/home/customer/varnish/supabase-shield.vcl` |
| `eu1/wal-g.conf` | eu1 (contabo) | Postgres replica overrides → `/opt/supabase-replica/wal-g.conf` |
| `eu2/wal-g.conf` | eu2 (salt2) | Postgres replica overrides → `/opt/supabase-replica/wal-g.conf` |
| `us2/wal-g.conf` | us2 (Edge2) | Postgres replica overrides → `/opt/supabase-replica/wal-g.conf` |
| `ug/wal-g.conf` | ug (QNAP) | Postgres replica overrides → `/share/CACHEDEV1_DATA/supabase-replica/wal-g.conf` |
| `ug/varnish-ug.default.vcl` | ug (QNAP) | Read-API Varnish (`:5558`) → `/opt/varnish-ug/default.vcl` |

## Applying configs

- **Postgres overrides** — use `scripts/replica-config.sh` (writes all nodes in lockstep),
  then restart `supabase-db` (us1) + `supabase-replica` (each replica).
- **Envoy** — scp `envoy-cds.yaml` + `envoy-lds.template.yaml` to us1, then
  `docker restart supabase-envoy` (entrypoint regenerates `lds.yaml` from the template).
- **Varnish** — scp to the path, then `docker exec supabase-varnish varnishadm vcl.load <name> <file> && varnishadm vcl.use <name>` (or restart the container).

## Important rules (see SUPABASE_PLAN.md §6)

- Replica `max_wal_senders`/`max_replication_slots` must be **≥ the primary's** — change
  all nodes together or replicas crash-loop.
- After DDL on the primary, **restart `supabase-rest`** (us1) **and each replica's
  `supabase-rest-replica`** to refresh PostgREST schema caches.
- QNAP config files are `admin`-owned; write via a throwaway root container if `solofx`
  can't (see runbook in SUPABASE_PLAN.md §12).