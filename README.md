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

## Secrets

Secrets are NOT in this repo. They live as env vars / container env / Cloudflare
worker secrets. See `docs/SUPABASE_PLAN.md` §15 for the SFX signing secret sync.