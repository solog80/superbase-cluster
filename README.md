# Salt TV — Supabase Cluster

Self-hosted Supabase mesh infrastructure for Salt TV (5 nodes: us1/Edge primary,
eu1/contabo, eu2/salt2, us2/Edge2, ug/QNAP). Single source of truth for the
cluster config, Go service, migrations, and runbooks.

## Layout

```
gofn/               Go service (salt-gofn) — the /api/v1 mesh API
migrations/         Postgres DDL for the Supabase primary (and TSDB analytics)
replica-configs/    Per-node configs (VCL, wal-g.conf, envoy, postgres overrides)
scripts/            Ops helpers (replica-config.sh lockstep Postgres settings)
docs/               SUPABASE_PLAN.md — architecture, runbooks, post-mortems
```

## Nodes

| Node | SSH host | Role |
|---|---|---|
| us1 | Edge | PRIMARY — full Supabase stack + Go service |
| eu1 | origin-contabo | READ REPLICA + read API (Europe) |
| eu2 | origin2-salt2 | READ REPLICA + read API (Europe #2) |
| us2 | Edge2 | READ REPLICA + read API (US #2) |
| ug | nas-ts (QNAP) | READ REPLICA + read API + QuObjects/Swift video origin |

## Key docs

- `docs/SUPABASE_PLAN.md` — architecture, read-path migration, cross-region failover,
  QNAP outage post-mortem + runbooks, realtime, SFX video delivery (§15).

## Quick commands

```sh
# Apply Postgres settings to all nodes in lockstep
scripts/replica-config.sh            # or --dry-run

# Deploy the Go service (cross-compile + push + rebuild image)
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/salt-gofn ./gofn
scp /tmp/salt-gofn Edge:/opt/supabase-gofn/salt-gofn
# then on Edge: docker build -t salt-gofn:new /opt/supabase-gofn && recreate supabase-api
```

## Edge outbox (TV ad logs)

TV/radio traffic, the StationPlaylist webhook, content/ad analytics, and Varnish
snapshots can write to a durable on-disk queue before acknowledging the client.
When TimescaleDB (QNAP) is unreachable the queue keeps the batches; a background
drainer replays them oldest first when the database returns. Clients are
unchanged: the edge still returns HTTP 200 with the same response fields.
TV/radio traffic replay is idempotent (`played_at, log_hash`). Analytics replay
is at-least-once: a crash after the TSDB commit but before the outbox ack can
duplicate that batch. EPG/bulk replace jobs are not queued; they rebuild from
source on their normal schedule.

Enable it on `supabase-api` by adding these to the existing `docker run`:

```sh
-v /opt/supabase-gofn/spool:/var/lib/salt-gofn \
-e OUTBOX_ENABLED=1 \
-e OUTBOX_PATH=/var/lib/salt-gofn/outbox.db \
-e OUTBOX_DRAIN=1
```

Use `OUTBOX_DRAIN=0` for store-only mode during a QNAP outage, then restart with
`OUTBOX_DRAIN=1` after the database returns. Optional tuning:
`OUTBOX_MAX_BYTES` (default 5 GiB, logical queued bytes),
`OUTBOX_MAX_ATTEMPTS` (0 = retry until success; default 0), `OUTBOX_DEDUPE_TTL_HOURS` (168),
`OUTBOX_DRAIN_INTERVAL` (5s), `OUTBOX_RETRY_BASE` (5s), `OUTBOX_RETRY_MAX` (15m).

Check queue depth and the last drain error at
`https://edge.solofx.net/api/v1/health` under `data.outbox`. A full queue returns
HTTP 503 with `Retry-After`, so the vMix agent keeps its watermark and retries.
A non-zero `dead` count means a batch hit a permanent error (or you configured a
retry limit). After fixing the cause, move dead jobs back to the queue:

```sh
curl -X POST -H "apikey: $SERVICE_ROLE_KEY" \
  "https://edge.solofx.net/api/v1/replayOutboxDead?limit=1000"
```

This endpoint is service-role only. `OUTBOX_MAX_ATTEMPTS=0` (the default) retries
transient failures forever, so a long QNAP outage does not dead-letter traffic.
The bbolt file is single-writer: keep `supabase-api` at one container instance
while this outbox is enabled. Make sure the container user can write
`/opt/supabase-gofn/spool` on the host.

## Secrets

Secrets are NOT in this repo. They live as env vars / container env / Cloudflare
worker secrets. See `docs/SUPABASE_PLAN.md` §15 for the SFX signing secret sync.