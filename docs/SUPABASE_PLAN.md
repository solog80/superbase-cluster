# SALT TV — Multi-Region Supabase on 5 Nodes: Build Plan

Successor to `BUILD_PLAN.md` (Appwrite). We are replacing Appwrite with **self-hosted
Supabase**. This is the going-forward source of truth for the database/API/backup layer.

**Decisions (LOCKED):**
- **Write primary = us1 / Edge** (Supabase already deployed & verified live).
- **Redundancy pattern = Standby + Backups** (single writable primary + one async
  streaming standby for failover + WAL-G backups to an off-region target).
- No per-region read/write split (Supabase's API layer is write-coupled to one Postgres;
  a streaming replica is read-only — the Appwrite pgpool model does not translate).
- Geo-routing + CDN for read speed (single API still round-trips to primary, but public
  GETs are CDN-cached).
- **Canonical domain: `edge.solofx.net`** (DNS record = mirror of `us1-edge.solofx.net`;
  Supabase public URLs set to `https://edge.solofx.net`).

---

## 1. Node Roles

| Node | SSH host | Public IP | Tailscale | Role |
|---|---|---|---|---|
| **us1** | `Edge` | 198.204.224.170 | 100.74.77.39 | **PRIMARY** — full Supabase stack |
| **eu1** | `origin-contabo` | 158.220.126.145 | 100.116.100.32 | **READ REPLICA + read-only REST API (Europe)** |
| **ug** | `nas-ts` (QNAP) | 192.168.0.112 / TS | 100.116.185.70 | **READ REPLICA + read-only REST API** + MinIO backup store |
| **us2** | `Edge2` | 142.54.173.186 | 100.82.159.75 | **READ REPLICA + read-only REST API (US #2)** |
| **eu2** | `origin2-salt2` | 75.119.149.43 | 100.99.30.100 | **READ REPLICA + read-only REST API (Europe #2)** |

All server-to-server traffic (replication + backups) flows over **Tailscale** — no
firewall holes, no public exposure of Postgres.

---

## 2. Current State (what's done)

| Piece | Status |
|---|---|
| **Supabase on us1/Edge** (full stack) | ✅ live & verified end-to-end |
| Envoy API gateway `:8555` (Studio + REST + Auth + Storage + Realtime) | ✅ verified (read via REST 200) |
| Supavisor pooler `:55432` (DB access), tenant `supabase` | ✅ verified (CREATE/INSERT/COMMIT) |
| WAL-G bundled in `supabase/postgres` image (`/home/wal-g`) | ✅ present |
| Replication readiness | ✅ **`max_wal_senders=20`, `max_replication_slots=10` on us1 primary** + all 4 replicas (`wal-g.conf` on each) |
| Raw Postgres host-port exposure for `pg_basebackup` | ✅ `100.74.77.39:55433 → 55432` (tailnet only) |
| **QNAP read replica** (`supabase-replica` on ug) | ✅ **re-cloned Aug 2026 after outage** — streaming, caught up (≤5s lag), `100.116.185.70:55432` |
| **QNAP read API** (`varnish-ug` `:5558`) | ✅ rebuilt (VCL was wiped) — serves `/rest/v1/` → local PostgREST |
| WAL-G continuous archiving → QNAP MinIO | ⚠️ configured (backup-push **still deferred**) — **TODO: enable so replicas can recover without re-clone** |
| Tailscale tailnet (all 5 nodes joined) | ✅ per BUILD_PLAN Phase A |
| **Supabase Realtime** (postgres_changes) | ✅ **enabled** — `epg_programs/epg_stations/tv_shows/seasons/episodes/ads/events` published to `supabase_realtime`; websocket through Varnish fixed (`return pipe` + `vcl_pipe`) |
| **App read path** | ✅ **moved to `/rest/v1` RPCs** (CDN-cached + geo-routed) — see §2.5 |
| **Cross-region failover** | ✅ **cascading priorities** in envoy geo clusters — see §2.6 |
| **SFX video delivery** | ✅ **signing on the Go mesh + CF edge cache** for `objects.solofx.net` — see §15 |
| **us1/Edge security hardening** | ✅ **firewall + SSH + fail2ban** — see §16. DB/admin ports now Tailscale-only; public web/streaming/payment untouched. ❗ **Ufw NOT enabled** (would flush Docker's FORWARD chain) |

### Replication plumbing (us1 → ug)
- Replicator role: `replicator` / password in `.env` (`REPLICATOR_PASSWORD`).
- `pg_hba.conf` (mounted at `/etc/postgresql/pg_hba.conf`): replication allowed from tailnet `100.64.0.0/10` and docker bridge `172.16.0.0/12`.
- Replication slot: `qnap_replica_slot` (physical).
- QNAP replica: `supabase/postgres:17.6.1.136`, data at `/share/CACHEDEV1_DATA/supabase-replica/pgdata`, `hot_standby=on` via mounted `wal-g.conf` override, listens on tailnet `100.116.185.70:55432`.

### eu1 read replica + read API (Europe) ✅
- `supabase-replica` on eu1: streaming replica from us1 (slot `eu1_replica_slot`), hot_standby=on, `100.116.100.32:55432`.
- `supabase-rest-replica` (PostgREST → eu1 replica, `127.0.0.1:5555`) + `supabase-rest-proxy` (nginx `/rest/v1/` strip, `127.0.0.1:5556`).
- Envoy geo-routing on us1: `CF-IPCountry` EU set (`DE FR GB IT ES NL BE AT CH SE NO DK FI PL CZ IE PT GR HU RO BG HR SK SI LT LV EE UA`) → cluster `eu1_rest` (`100.116.100.32:5556`).

### eu2 read replica + read API (Europe #2) ✅
- `supabase-replica` on eu2: streaming replica from us1 (slot `eu2_replica_slot`), hot_standby=on, `100.99.30.100:55432`.
- `supabase-rest-replica` (PostgREST → eu2 replica, `127.0.0.1:5555`) + `supabase-rest-proxy` (nginx `/rest/v1/`, `127.0.0.1:5556`).
  - Note: eu2 containers couldn't reach the tailnet IP (bridge→host routing), so PostgREST points at the replica container IP `172.17.0.10:5432` directly.
- **EU redundancy**: `eu1_rest` cluster now has **two endpoints** (eu1 + eu2, round-robin + health failover). EU reads split across both.

### us2 read replica + read API (US #2) ✅
- `supabase-replica` on us2: streaming replica (slot `us2_replica_slot`), `100.82.159.75:55432`.
- `supabase-rest-replica` (PostgREST → us2 replica, `127.0.0.1:5555`) + `supabase-rest-proxy` (nginx, `127.0.0.1:5556`).
- **US redundancy**: us2 added as 2nd endpoint in the Envoy `rest` cluster → US reads round-robin us1 (local) + us2, health-checked. us2 nginx proxies all paths (the `rest` route does `prefix_rewrite: /`, so it receives `/hello` not `/rest/v1/hello`). Writes (POST/PUT/PATCH/DELETE) are pinned to the **`us1_rest`** cluster (primary only) via the `rest-v1-write` route — never to replicas.

### Geo-routing summary (verified)
| CF-IPCountry | Read backend |
|---|---|
| Africa (UG KE TZ ...) | ug (QNAP) |
| Europe (DE FR GB ...) | eu1 **+ eu2** (load-balanced, fails over) |
| US/rest | us1 **+ us2** (load-balanced, fails over) |

### Read path migration (Aug 2026) ✅ — app reads via /rest/v1
The Flutter app's catalog reads (EPG, on-demand, ads, events) now hit **`/rest/v1`** RPC functions instead of the Go service `/api/v1`, so they flow through **CF cache → Varnish → geo-routed replica**, offloading us1. The Go service remains for writes/admin/logic.

- **RPC functions on the primary** (`functions/superbase/migrations/006_read_views.sql`):
  - `get_today_epg()` → `{data:{tv:{<station>:{...}}, radio:{...}}}` (today-only + midnight-crossover, replicates Go `buildEPGPayload`)
  - `get_on_demand_catalog()` → `{data:{shows:[{...,seasons:[{...,episodes:[]}]}]}}` (published-only at every level)
  - `get_events_payload()` → `{events:[...]}`
  - Ads use the existing `ads_api` view (`/rest/v1/ads_api?status=eq.active&order=priority.desc`)
- **App side**: `lib/services/mesh_api_service.dart` reads `/rest/v1/rpc/*` + `ads_api` with the same JSON shapes the app already parses (no provider changes).
- **Why RPCs, not views**: PostgREST v14 does **not** expose newly-created views through the Supavisor pooler (`PGRST205` / not in schema cache), while RPC functions appear after `NOTIFY pgrst, 'reload schema'`. Use RPCs for new read objects.
- **Replicas need PostgREST restart after DDL** (schema cache) — see Gotcha #7; the RPCs must exist on every replica's PostgREST (they replicate via WAL, then `supabase-rest-replica` restart).
- **Read-your-writes**: envoy `rest-v1-svc-read` route pins **service-role** reads to `us1_rest` (primary only), so admin edits → reads stay consistent regardless of replica lag.

### Cross-region failover (Aug 2026) ✅ — cascading priorities
Envoy geo clusters cascade to **any surviving region** via `priority` levels. Envoy only moves up a priority when all lower-priority endpoints are unhealthy.

| Cluster | prio 0 | prio 1 | prio 2 |
|---|---|---|---|
| **US (`rest`)** | us1 (`rest:3000`) + us2 (`100.82.159.75:5557`) | eu1 + eu2 | ug |
| **EU (`eu1_rest`)** | eu1 (`100.116.100.32:5557`) + eu2 (`100.99.30.100:5557`) | ug | us1 + us2 |
| **UG (`ug_rest`)** | ug (`100.116.185.70:5558`) | eu1 + eu2 | us1 + us2 |

- Live-verified: stopping QNAP's `varnish-ug` → UG traffic still 200 (failed over to EU); restart → served locally again.
- Health checks: `http_health_check /` (US), `tcp_health_check` (EU/UG). Config in `volumes/api/envoy/cds.yaml`.

### QNAP read-only REST API (geo-routing) ✅
- `supabase-rest-replica`: `postgrest/postgrest:v14.12` → local replica (`127.0.0.1:5555`), same JWT_SECRET + anon key as us1. Reads only (replica is read-only).
- **Read API served by `varnish-ug`** (container, `-a 0.0.0.0:5558`, VCL at `/opt/varnish-ug/default.vcl`): strips `/rest/v1/` and proxies to PostgREST (`127.0.0.1:5555`), caches anon reads 60s. Envoy `ug_rest` targets `100.116.185.70:5558`.
  - ⚠️ The VCL was **wiped during the Aug 2026 outage** (path became an empty dir). Restored; **keep a copy in the repo** (`replica-configs/ug/default.vcl`).
- **Studio on ug** ✅: `supabase-postgres-meta` (`supabase-meta-ug`) + `supabase/studio` (`supabase-studio-ug`, `127.0.0.1:5557`) pointed at the replica; nginx serves `/` → Studio with basic auth (admin/dashboard pw), `/rest/v1/` → read API.
- **Geo-routing** (Tailscale-native, single CF origin at us1): Envoy on us1 routes `GET/HEAD /rest/v1/*` with `CF-IPCountry` ∈ EU/Africa → cluster `ug_rest` (`100.116.185.70:5558`, over the Tailscale mesh). All **writes/auth/storage/realtime → us1** (replica is read-only; storage files live on us1). `edge.solofx.net` → us1 only; no ug tunnel required for geo-routing.
  - Files: `volumes/api/envoy/cds.yaml` (cluster `ug_rest`) + `lds.template.yaml` (route `rest-v1-geo-ug`, added to Lua `PROTECTED_ROUTES`).

### Cloudflare entry + caching (free tier) ✅
- **Tunnel `edge-us1`** on us1 (cloudflared systemd service): `edge.solofx.net` → `http://localhost:8555`. DNS: `edge.solofx.net` CNAME → `e30a6a5c-69d1-4ba6-8e09-2392f300df95.cfargotunnel.com` (proxied).
- **Cache Worker `edge-cache`** on `edge.solofx.net/*` (Cache API, `caches.default`): caches **anon-role** `GET/HEAD /rest/v1/*` (JWT `role=anon`) with 60s TTL; passes through Studio/basic-auth, writes, authenticated reads, storage, realtime. Forwards `CF-IPCountry` so geo-routing survives the proxy.
- **Relay Worker `origin-relay`** on `origin-relay.solog80.workers.dev`: forwards to `https://us1-edge.solofx.net` (Workers on the same zone can't fetch a same-zone proxied hostname — 1003 loop protection — so the cache worker goes via the workers.dev relay; raw origin IPs are also 1003-blocked).
- Origin Rules are **paid** on this plan; Cache Rules are **paid** too → the Worker path is the free caching option. Verified: anon read MISS→HIT, Studio 307, writes pass-through, `cf-cache-status` stays DYNAMIC (app-level cache).

---

## 3. Architecture (target)

```
        GeoDNS / Cloudflare (geo-routes + CDN for GET)
                         │
        ┌────────────────┼─────────────────────────────┐
     nearest region API  │                             │
   (single API today: us1)                             │
                         ▼                             │
   ┌─────────── us1 PRIMARY ───────────┐               │
   │  Envoy :8555 → Studio/REST/Auth    │               │
   │  /Storage/Realtime/Functions       │               │
   │  Postgres (supabase-db) :55432     │               │
   │        │  streaming WAL ─────┐     │               │
   └────────┼─────────────────────┼─────┘               │
            │                     │                     │
            ▼                     ▼                     │
   ┌── eu1 STANDBY ──┐   ┌─ ug BACKUP ──┐              │
   │ supabase-db      │   │ MinIO        │◄─ WAL-G      │
   │ async replica    │   │ (off-region) │   (PITR)     │
   │ (failover target)│   └──────────────┘              │
   └─────────────────┘                                  │
   us2: Observability (Grafana/Loki)                    │
   eu2: optional 2nd standby                            │
```

- **Writes:** always → us1 primary.
- **Reads:** API + CDN; on failover → eu1.
- **DR:** WAL-G PITR backups to QNAP MinIO, independent of us1.

---

## 4. Build Steps

### Phase 0 — Baseline ✅ (done)
Full Supabase on us1/Edge verified end-to-end (see BUILD_PLAN §2).

### Phase 1 — Replication plumbing (us1)
- [ ] Expose raw Postgres over **Tailscale only** for `pg_basebackup`:
      add host port to the `db` service → `100.74.77.39:55433:55432`
      (55433 avoids the litellm `0.0.0.0:5432` binding; container internal port is 55432).
- [ ] Add `pg_hba.conf` entry allowing `replication` + tailnet CIDR `100.64.0.0/10`
      (via `/etc/postgresql-custom` / the `db-config` volume; authenticate with a password,
      do not trust).
- [ ] Create a dedicated `replicator` role (`LOGIN REPLICATION`) with a strong password,
      and store the password in the standby's config.
- [ ] Recreate only the `db` container to apply the port + conf (`docker-compose up -d db`);
      verify data persists (it lives in `./volumes/db/data`).
- [ ] Verify from eu1: `pg_basebackup --host=100.74.77.39 --port=55433` connects.

### Phase 2 — Backups: WAL-G → QNAP MinIO (us1 + ug)
- [ ] On **ug/QNAP**: create a dedicated MinIO bucket (e.g. `supabase-backups`) +
      an access key (via existing MinIO on QNAP; the BUILD_PLAN already targets QNAP MinIO).
- [ ] On **us1** Supabase db: configure WAL-G continuous archiving via
      `/etc/postgresql-custom/wal-g.conf` + postgres config:
      - `archive_mode = on`
      - `archive_command = wal-g wal-push %p`
      - WAL-G env: `WALG_S3_PREFIX=s3://supabase-backups/us1`,
        `WALG_S3_ENDPOINT=<qnap-minio-tailscale>:9000`,
        `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`,
        `WALG_S3_FORCE_PATH_STYLE=true`, `WALG_LIBSODIUM_KEY=<random>` (encryption).
- [ ] Schedule periodic full backups: `wal-g backup-push $PGDATA`
      (cron/host job or a small sidecar container) — daily full + continuous WAL = PITR.
- [ ] **Test a restore** to a throwaway container on us2 from QNAP MinIO
      (`wal-g backup-fetch` + replay WAL) — prove DR works before relying on it.

### Phase 3 — Async streaming standby (eu1)
- [ ] On **eu1**: run the **same** `supabase/postgres:17.6.1.136` image (so extensions/roles
      match on promotion), internal port 55432, data in a persistent volume.
- [ ] Initial clone from us1 over Tailscale:
      `pg_basebackup -h 100.74.77.39 -p 55433 -U replicator -D <data> -R -S eu1_slot -X stream`
      (`-R` writes `standby.signal` + primary_conninfo; `-S` creates `eu1_slot`).
- [ ] Point `primary_conninfo` at the Tailscale IP; confirm `pg_is_in_recovery = t` and
      replay lag ≈ 0.
- [ ] **Pre-stage the full compose** on eu1 (services stopped or `profiles`-gated) so that
      failover = promote DB + `docker-compose up -d` + repoint DNS (minutes, not hours).
- [ ] Health check: WAL lag alert (e.g. `pg_stat_replication` / Prometheus exporter on us2).

### Phase 4 — Failover runbook (us1 → eu1)
Documented, tested procedure:
1. Confirm us1 is down (or maintenance window).
2. On eu1 standby: promote → `SELECT pg_promote();` (or `pg_ctl promote`).
3. Bring up the full Supabase stack on eu1 pointing at the promoted DB.
4. Repoint: Cloudflare/geo-DNS `*.solofx.net` API + Studio + Supabase client base URL →
   eu1's API port; update Supavisor/pooler + app `NEXT_PUBLIC_SUPABASE_URL`/keys if needed.
5. Point WAL-G archiving at QNAP MinIO under a `eu1` prefix.
6. Rebuild a new standby (us2/eu2) from the new primary; note old us1 as the new standby.
- [ ] Document failback (reverse) once eu1 is primary and us1 is repaired.

### Phase 5 — Read speed / geo-routing (all)
- [ ] Cloudflare geo-routing to nearest region API (currently all traffic → us1; a single
      DNS record `supabase.solofx.net` → us1 with `proxied` CDN).
- [ ] CDN-cache public **GET** endpoints (catalog, metadata, public storage reads) —
      mirror the Varnish pattern from BUILD_PLAN (cache GETs, pass non-GET/Set-Cookie).
- [ ] Optional future: if per-region read latency matters, add **read-only** replicas +
      a separate read PostgREST per region — but only as a later optimization (not stock,
      adds complexity). Not in scope now.

---

## 5. Definition of Done

- One writable Supabase primary (us1) serving all writes; single API base URL.
- eu1 async standby caught up; failover to eu1 tested end-to-end (DB + full API) ≤ 15 min.
- WAL-G PITR backups flowing to QNAP MinIO daily; **restore tested**.
- us2 observing replication lag + service health (alert on lag > N and on standby loss).
- No single point of failure for data: primary loss → eu1 promotes; primary+eu1 loss →
  PITR restore from QNAP MinIO.

---

## 6. Gotchas / Notes

1. **Supabase ≠ Appwrite at the replication layer.** Clients talk to PostgREST/auth/storage
   over HTTP, all write-coupled to one Postgres. Streaming replicas are **read-only**.
   The Appwrite "local replica + pgpool read/write split" does not carry over.
2. **Replicate via raw Postgres, not Supavisor.** The pooler (`:55432`) is a connection
   pooler and cannot stream WAL. Expose the real `supabase-db` on a separate Tailscale
   port (`55433`) for `pg_basebackup`.
3. **Same image on the standby.** Use `supabase/postgres:17.6.1.136` on eu1 so extensions
   (pgsodium, etc.), roles, and config match — a clean `pg_basebackup` inherits them.
4. **Port conflict on us1:** litellm owns `0.0.0.0:5432`; bind replication to a unique
   port on the Tailscale IP (`100.74.77.39:55433`), never `0.0.0.0`.
5. **Backup target must be off-region** (QNAP MinIO) so losing us1 doesn't lose backups.
   MinIO also runs on us1 but that's same-host — not DR.
6. **Test the restore.** A backup you've never restored is not a backup.
7. **Replica PostgREST schema caches go stale after DDL.** Replicas are read-only, so
   PostgREST can't `LISTEN` on the `pgrst` channel (logs spam "session is read-only") and
   never sees new tables. After creating tables on the primary, **restart
   `supabase-rest-replica`** on ug/eu1/eu2/us2 to refresh their schema caches.
8. **Writes must never hit the read-only replicas.** The `rest` cluster includes us2 for
   read scaling, but it also catches POST/PUT/PATCH/DELETE → random "read-only" errors.
   Fixed with a dedicated `us1_rest` cluster (primary only) + `rest-v1-write` route that
   matches `POST|PUT|PATCH|DELETE /rest/v1/*` **before** the catch-all `rest-v1-protected`
   route. Keep this ordering if you touch the Envoy routes.
9. **us2's read API uses bare paths, not `/rest/v1/`.** us2 nginx proxies `location /`
    (Envoy does the `prefix_rewrite` before it arrives), so hitting `us2:5556/rest/v1/x`
    directly returns `PGRST125`. eu1/eu2/ug strip `/rest/v1/` in nginx. Different convention
    per node — through the Envoy mesh both work identically.
10. **Replica config must match (or exceed) the primary — always.** Postgres requires the
    replica's `max_wal_senders`/`max_replication_slots` to be ≥ the primary's. Raising them
    on us1 **without the replicas crash-loops every replica** (`recovery aborted because of
    insufficient parameter settings`). Change these on **all nodes together** — see
    `scripts/replica-config.sh` (TODO). This bit us during the Aug 2026 outage.
11. **PostgREST doesn't expose newly-created views through the pooler.** A fresh view gets
    `PGRST205` ("not in schema cache") and `NOTIFY pgrst` may not pick it up via Supavisor.
    **Use RPC functions** (they appear after `NOTIFY pgrst, 'reload schema'`) for new read
    objects — see §2.5. After any DDL, restart `supabase-rest` + each replica's
    `supabase-rest-replica`.
12. **WebSockets (Realtime) must be piped through Varnish.** Varnish strips the hop-by-hop
    `Upgrade`/`Connection` headers on `return (pass)`, so realtime websockets return `400`.
    Fix: detect `req.http.Upgrade ~ "(?i)websocket"` → `return (pipe)` in `vcl_recv`, and
    re-add the upgrade headers in a `vcl_pipe` block. Applied to us1's `supabase-varnish`.
13. **A replica that lags past WAL retention needs a full re-clone.** If the primary recycles
    the WAL the replica still needs (`requested WAL segment ... already removed`), streaming
    can't resume — run `pg_basebackup` fresh (§ QNAP re-clone runbook). Enable WAL-G
    archiving + `wal_keep_size` (TODO) so long outages recover without re-cloning.
14. **Container config bind-mounts can be wiped by a power outage.** QNAP's `varnish-ug`
    VCL path became an empty directory after the outage, taking the read API down. Keep
    copies of every container config (VCL, nginx, postgres overrides) in the repo
    (`replica-configs/`) so a wipe is a one-command restore.
15. **QuObjects/Swift is open (no auth on GET object reads).** Verified: segments return 200
    with or without tokens/headers. The **only gate is the HMAC token** the worker/Go mesh puts
    on the *playlist*. If you drop per-segment validation, segment paths are effectively public
    once cached at the CF edge. Do not rely on the origin for auth.
16. **Cloudflare worker secrets are write-only.** You cannot read back `SIGN_SECRET` via
    wrangler/API — the dashboard shows it encrypted. To share it with the Go mesh, **rotate**
    it: set a new value on the worker (`wrangler secret put`) and the same value as
    `SFX_SIGN_SECRET` on the Go service. Keep both in sync (see §15).

---

## 7. Smoke Trial (Aug 2026) ✅

End-to-end verification of the full read/write path. Result: **all 6 layers pass**.

| Layer | What was tested | Result |
|---|---|---|
| Schema + RLS | `public.tv_shows` (27 rows) + `public.hero_banners` (3 rows), anon-read RLS policies | ✅ |
| Write path | POST `/rest/v1/*` (service role) → us1 primary | ✅ 201 |
| Replication | Rows visible on ug/eu1/eu2/us2, slots 0 lag | ✅ 27/3 everywhere |
| Per-region reads | Each replica read API (direct tailnet) | ✅ (us2 = bare path) |
| Geo-routing | Spoofed `CF-IPCountry` against Envoy, proved via nginx access logs | ✅ Africa→ug, EU→eu1+eu2, US→us1+us2 |
| CDN cache | anon GET on `edge.solofx.net` | ✅ MISS→HIT |

- Trial data loaded from the Firebase exports (`exports/tv_shows.ndjson`, `hero_banners.ndjson`),
  transformed to snake_case via `jq`, inserted through the public REST API.
- **Bugs found & fixed:** (1) writes round-robining into read-only replicas → `us1_rest`
  cluster + `rest-v1-write` route; (2) stale replica PostgREST schema caches → restart
  required after DDL (see Gotchas #7/#8).
- **Ops improvement:** `access_log /dev/stdout;` added to the 4 replica nginx proxies
  (ug/eu1/eu2/us2) so geo-routed reads are visible in `docker logs`.
- MCP on `/mcp` is **deny-by-default** (see earlier MCP hardening); the IP allow-list was
  reverted because Docker NAT makes every connection appear as the bridge gateway
  (`172.27.0.1`) — allowing it effectively opened MCP to everyone.

---

## 8. Go Functions (api service) ✅

Supabase Edge Functions are **Deno-only**, so "functions in Go" = a Go HTTP microservice
exposed through the same Envoy gateway, mimicking the edge-function contract (same URL
scheme + `apikey`/`Authorization: Bearer <jwt>` auth) so clients don't change.

### Architecture
```
client → edge.solofx.net/api/v1/<name>   (public, via CF + worker)
       → Envoy :8555  route "api-v1-all" (prefix /api/v1/, prefix_rewrite: /) → cluster "api"
       → supabase-api container  (Go 1.25, stdlib only, 17MB, zero deps)
       → PostgREST via mesh  (http://api-gw:8000/rest/v1, service-role key)
```
- Container `supabase-api` runs on the `supabase_default` network on us1; image `salt-gofn`
  built from `/opt/supabase-gofn/` (binary + Dockerfile). Runs **alongside** the Deno
  edge-runtime (`/functions/v1/*` untouched) — zero risk to existing routes.
- Code: `salt-media-migration/supabase-gofn/main.go` (stdlib `net/http`, Go 1.22+ mux,
  catch-all dispatcher that extracts the function name from the last path segment).
- Envoy: cluster `api` in `cds.yaml` (STRICT_DNS → `supabase-api:8080`, health
  `/api/v1/health`); route `api-v1-all` in `lds.template.yaml` (basic_auth disabled,
  RBAC allow_all — auth is app-level). Not in Lua `PROTECTED_ROUTES` → passes through.
- CORS: headers added in Go (and Envoy's own CORS filter covers the public path).

### API contract (two layers)
```
/rest/v1/*   = DATA  — PostgREST: direct table/view access, RLS-guarded, anon/service key.
                        Read-heavy path: CDN-cached + geo-routed to nearest region. Keep public.
/api/v1/*    = LOGIC — Go service: business logic, auth decisions, external integrations.
                        If it needs a decision or touches an external service, it goes here.
```
- **Rule of thumb:** `/rest/v1/` reads/writes your data; `/api/v1/` runs your business.
  Don't rename `/rest/v1/` (Supabase platform standard; the cache + geo-routing depend on it).
- To "hide" the raw data API later: route everything through `/api/v1/` and keep PostgREST
  internal-only — but that forfeits the free CDN+geo read path; do it only if schema exposure
  becomes a real concern.

### Functions implemented
| Function | Route | Auth | What it does |
|---|---|---|---|
| `health` | `GET /api/v1/health` | none | status + live DB ping (tv_shows count via Content-Range) |
| `catalog` | `GET /api/v1/catalog?q=&type=` | anon/service key | search `tv_shows` by title (`ilike`) + type filter via PostgREST |

### Adding a function
1. Add a `case "name": s.handleName(w, r)` in `dispatch` (auth enforced for all but `health`).
2. `GOOS=linux GOARCH=amd64 go build -o salt-gofn-linux .`
3. `scp salt-gofn-linux Edge:/opt/supabase-gofn/` → `docker build` → `docker rm -f supabase-api && docker run -d --name supabase-api --network supabase_default -e PG_REST_URL=http://api-gw:8000/rest/v1 -e SERVICE_ROLE_KEY=... -e ANON_KEY=... salt-gofn`
4. Test via `curl http://127.0.0.1:8555/api/v1/<name>`.

### Verification (Aug 2026)
- `health` → 200 `db_status:ok`; `catalog?q=Con` → `Connected` (live data).
- No/invalid key → **401**. Public path `edge.solofx.net/api/v1/health` → **200** (CORS OK).
- Test UI: `salt-media-migration/react-app` (Vite) — health check + catalog search
  wired to `/api/v1/*` with the embedded anon key.

### Migration note
Firebase `onCall` functions are the same contract: JSON request → JSON response, auth via
token. Porting to Go means rewriting against the new Postgres schema — the old JS is the
spec. Read-heavy catalog functions often become PostgREST views/RPCs with **no function
at all**; only "logic + DB" and external-integration functions need the Go service.

---

## 9. Observability (Grafana + Prometheus on Linode) ✅

- **grafana VPS** (139.162.162.37, Linode, tailnet `100.98.214.99`) joined the tailnet; 2GB swap added.
- **Prometheus** (docker, `100.98.214.99:9090`) scraping all 5 nodes: `node_exporter` (`:9100`) + `postgres_exporter` (`:9187`) — 10 targets up.
- **Grafana v13** (docker, `100.98.214.99:3000`) replaced the broken native install. Admin set, Prometheus datasource added, imported **Node Exporter Full** (#1860) + **PostgreSQL** (#9628) dashboards.
- **Friendly instance labels**: Prometheus relabels `instance` → `us1/ug/eu1/eu2/us2` (was raw tailnet IPs) so all dashboards read cleanly.
- **Custom dashboard "Supabase Mesh Overview"** (`/d/supabase-mesh/supabase-mesh-overview`): all 5 nodes on one screen — CPU/mem/disk/load/network + Postgres connections/transactions/cache-hit + **replication lag** + primary/replica role, with an instance dropdown.
- Access: `http://100.98.214.99:3000` (tailnet only). Config on grafana VPS: `/opt/monitoring/`.
- Note: old native Grafana (and its Loki datasources) removed. Loki/promtail still run on us1 — re-add a Loki datasource if log dashboards are wanted.
- Suggested alerts to add later: node down, disk > 85%, replication lag, postgres up/down.

---

## 10. Multi-App on the Mesh — Option C (future)

For hosting several products (e.g. Salt Media, PlayItLoud) on the 5-node mesh with
**full isolation**. Each app = its own Supabase project = **its own primary node + its
own read replicas across the mesh**. NOT built yet — this is the forward target once an
app outgrows schema-sharing (Option A).

### Concept
```
app.solofx.net ──► Cloudflare/Envoy ──► app's primary ──► app's replicas
                  (Host-header routing)   (us1|us2|…)      (2× across mesh)
```

### Node assignment (proposal)
| App | Primary | Replicas |
|---|---|---|
| Salt Media (existing) | us1 (live) | ug + eu1 + eu2 + us2 (live) |
| PlayItLoud (new) | us2 | eu1 + eu2 + ug (us1 stays Salt primary) |

- us2 currently runs a Salt **replica**; to become PlayItLoud's primary it must be
  **unsubscribed from Salt** (`pg_drop_replication_slot` on us1), then re-cloned as
  PlayItLoud's primary (or a fresh `pg_basebackup` from a new PlayItLoud primary).
- Each app keeps its own: `postgres` database/primary, Supavisor tenants, Envoy gateway
  port or Host-header route, keys (`JWT_SECRET`, anon/service-role), backup prefix.

### Routing & identity
- **Cloudflare**: one zone (`solofx.net`), one CNAME per app
  (`playitloud.solofx.net` → tunnel/worker); the worker relays to the right origin.
- **Envoy**: either a second gateway port per app OR one gateway keyed by Host header
  (route `virtual_hosts` per app). Go service dispatches per app prefix or Host.
- **No cross-app data**: separate Postgres clusters → physical isolation by default.

### Moving an app from Option A → Option C
1. In the shared project, `pg_dump --schema=<app_schema>` (schema + data) on us1.
2. Stand up the new app's primary on us2 (fresh `supabase/postgres:17.6.1.136`, load dump).
3. Clone replicas (`pg_basebackup`, one slot per standby) from the new primary.
4. Point the app's Envoy route + DNS at the new cluster; update app keys/base URL.
5. Remove the app's schema + RLS from the shared project once traffic is cut over.
   - The schema/RLS work is reusable — no redesign needed.

### Ops implications (per app, multiplied)
- N × WAL-G backup streams (each app → QNAP MinIO under its own prefix).
- N × failover runbooks (promote standby → repoint DNS/keys) — each app independent.
- N × monitoring: Prometheus targets + per-app dashboard filters; Grafana already
  relabels by node, add per-project labels.
- Resource budget per node must cover the apps assigned to it (each app's primary does
  the write work; each replica adds read/CPU).

### When to use this vs Option A
- **Option A (one project, schema-per-app + `app_id` JWT RLS)** — while apps are small,
  learning, or sharing infra cost. Zero extra ops.
- **Option C** — when an app needs hard isolation, independent scaling/upgrades, or its
  own failover guarantees. The migration path is `pg_dump` + re-clone, so A→C is safe.

---

## 11. Analytics — TimescaleDB (self-hosted) ✅

BigQuery replacement for self-reliance. TimescaleDB = Postgres + time-series (hypertables,
`time_bucket`, retention). Runs as a **separate instance**, NOT on the Supabase primary.

### Deployment (on ug/QNAP)
- Container `timescale-db` (`timescale/timescaledb:latest-pg17`, **2.29.2**),
  `--restart unless-stopped`, **x86_64**.
- Data volume: `/share/CACHEDEV1_DATA/timescale/data` (3.2 TB free on the NAS — bulk
  time-series belongs here, not on us1's 129 GB). Creds in
  `/share/CACHEDEV1_DATA/timescale/.env` (`TIMESCALE_PASSWORD`).
- Database `analytics`, extension `timescaledb` enabled.
- Access: tailnet only `100.116.185.70:55439` (also reachable from us1's Go service over
  the tailnet). Password in the `.env` above.
- Sample hypertable: `public.content_views` (content_id, view_time, region, user_id,
  duration_s, ad_impressions) — 182 seeded rows; verified `time_bucket` queries.
- **Note:** ug is a busy NAS (transcode workers, varnish, replica). Fine for an
  idle-until-queried analytics store — just avoid heavy reporting during transcodes.
  If the time-series ever outgrows it, chunk-partition by region or add a dedicated node.

### Why a separate instance (not the primary)
- Analytics workload (aggregates, retention) must not compete with OLTP writes.
- The Supabase `supabase/postgres` image doesn't ship TimescaleDB; a dedicated
  `timescale/timescaledb` image keeps the primary stock/upgradeable.
- It lives on the QNAP because time-series + backup data is bulk — the NAS (3.2 TB free)
  is its natural home, and it stays off the OLTP primaries.

### How it slots into the stack (replaces BigQuery)
- **Writer:** the Go service (`supabase-api`) pushes aggregates → TimescaleDB hypertables
  (via `pgx` or `lib/pq`), replacing the `sync*ToBigQuery` triggers + BigQuery syncs.
- **Reader:** the ~35 analytics functions (revenue, retention, views, payouts, geographic,
  device) read hypertables instead of BigQuery. Report reads are non-critical → fine on ug.
- **Scheduled aggregation:** `pg_cron`/Go workers feed it (the `pollsHourlyAggregation` /
  `refreshCache` pattern already exists).
- **Scaling later:** hypertables can move to eu2 (664GB free disk) or a dedicated node
  when the time-series grows; chunks can be partitioned by region.

### Admin
```sh
ssh nas-ts 'D=/share/CACHEDEV1_DATA/.qpkg/container-station/bin/docker; $D exec -it timescale-db psql -U postgres -d analytics'
# or from a Mac on the tailnet (psql required):
PGPASSWORD=$(cat /tmp/tspw.txt) psql -h 100.116.185.70 -p 55439 -U postgres -d analytics
```

---

## 12. QNAP Outage Post-Mortem (Aug 2026)

**Event:** Power outage at the QNAP (ug). On return, the ug replica crashed, the read API
was down, and UG users saw 503s until failover was added.

### Timeline & root causes
1. **Replica crash-loop on reboot** — primary had `max_wal_senders=20` but QNAP still had
   `10` (raised on us1 earlier, never propagated). Postgres refuses to start recovery when
   the replica setting is lower than the primary's (Gotcha #10).
2. **WAL purged during the outage** — QNAP was ~8h behind; the primary recycled the WAL
   segments it needed (`requested WAL segment ... already removed`). Streaming couldn't
   resume → required a **full `pg_basebackup` re-clone** (Gotcha #13).
3. **`varnish-ug` VCL wiped** — the bind-mounted VCL path became an empty directory,
   so the read API (`:5558`) was down (Gotcha #14).
4. **No failover** — UG geo-routed to `ug_rest` (QNAP only); with QNAP down it 503'd.
   Fixed by adding cascading priorities (§2.6).

### Outcome / fixes applied
- Re-cloned the QNAP Postgres replica (fresh `pg_basebackup` + `qnap_replica_slot`), caught up ≤5s.
- Rebuilt `varnish-ug` VCL on `:5558` (video varnish `varnish-cache` untouched).
- Bumped `max_wal_senders=20`/`max_replication_slots=10` on all 4 replicas.
- Added cascading cross-region failover in envoy (§2.6); live-verified UG→EU→US.
- App read path moved to `/rest/v1` RPCs (CDN + Varnish + geo) so a region being down
  no longer hammers us1 (§2.5).

### Open TODOs (prevention)
- [ ] **WAL-G continuous archiving → MinIO** (currently deferred) so replicas recover from
      long outages without a full re-clone.
- [ ] Set `wal_keep_size` on the primary as cheap insurance.
- [ ] Commit all container configs (VCL, nginx, postgres overrides) to the repo
      (`replica-configs/`) — see §14.
- [ ] `scripts/replica-config.sh` to apply Postgres settings to all nodes in lockstep.
- [ ] Enable QNAP snapshots on the replica data/config volume for instant rollback.
- [ ] Prometheus alerts: replica down / replay lag > 5min / crash-loop.

### QNAP replica re-clone runbook (when WAL is purged)
```sh
# On QNAP: stop + remove the broken replica (data is a read-only copy)
ssh nas-ts 'D=/share/CACHEDEV1_DATA/.qpkg/container-station/bin/docker; \
  $D stop supabase-replica; $D rm supabase-replica'

# Fresh basebackup from us1 primary over the tailnet (replicator creds from primary_conninfo)
ssh nas-ts "D=/share/CACHEDEV1_DATA/.qpkg/container-station/bin/docker; \
  setsid \$D run --rm -v supabase-replica-pgdata:/var/lib/postgresql/data \
  -e PGPASSWORD=<replicator_password> supabase/postgres:17.6.1.136 sh -c \
  'chown -R postgres:postgres /var/lib/postgresql/data && su postgres -c \
  \"pg_basebackup -h 100.74.77.39 -p 55433 -U replicator -D /var/lib/postgresql/data \
  -R -S qnap_replica_slot -X stream -P\"'"  # -R writes standby.signal + primary_conninfo

# Recreate the container (port 55432, config mount, restart unless-stopped)
ssh nas-ts 'D=/share/CACHEDEV1_DATA/.qpkg/container-station/bin/docker; \
  $D run -d --name supabase-replica --restart unless-stopped \
  -v supabase-replica-pgdata:/var/lib/postgresql/data \
  -v /share/CACHEDEV1_DATA/supabase-replica/wal-g.conf:/etc/postgresql-custom/wal-g.conf \
  -p 100.116.185.70:55432:5432 supabase/postgres:17.6.1.136 -c config_file=/etc/postgresql/postgresql.conf'

# Restart PostgREST so its schema cache sees new RPCs (Gotcha #7)
ssh nas-ts 'D=...; $D restart supabase-rest-replica'
```
Verify: `pg_is_in_recovery=t`, replay lag ≤ a few seconds, and the UG geo route returns 200.

### QNAP config-fix workaround (admin-owned files)
Config files under `/share/CACHEDEV1_DATA/...` are owned by `admin`; the ops account
(`solofx`) can't write them and there's no passwordless sudo. Write via a throwaway root
container:
```sh
ssh nas-ts 'D=/share/CACHEDEV1_DATA/.qpkg/container-station/bin/docker; \
  $D run --rm -v /path/to/file:/v/out alpine:3.20 sh -c "cp /tmp/in /v/out"'
```
Prefer fixing ownership (`chown solofx`) so this isn't needed — TODO.

---

## 13. Realtime (Supabase postgres_changes) ✅

See §2.5 for the read path. Realtime specifics:

- **Publication:** `supabase_realtime` contains `epg_programs, epg_stations, tv_shows,
  seasons, episodes, ads, events` (INSERT/UPDATE/DELETE).
- **App side:** `supabase_flutter` + `SupabaseClient` (url from `MESH_API_URL`, anon key).
  `TVRepository`, `RadioRepository`, `OndemandPlaylistsNotifier`, `AdService` each subscribe
  to postgres_changes on their tables and force-refresh on change. Firestore listeners kept
  as a backup path.
- **Self-host caveats:**
  - Realtime needs `max_replication_slots` ≥ realtime's slots (it uses 2 logical slots).
    If it can't create its slot it crash-loops with `ReplicationMaxWalSendersReached`.
  - The realtime websocket must pass through Varnish → `return (pipe)` (Gotcha #12).
  - Supabase client protocol **v2** (`vsn=2.0.0`) is used by default; older self-host
    realtime spoke v1 — if joins time out, check protocol support.

---

## 14. Repo layout for cluster configs (TODO)

Track the per-node config files that were lost during the outage so recovery is a copy:

```
replica-configs/
  ug/
    varnish-ug.default.vcl      # :5558 read API VCL
    wal-g.conf                  # max_wal_senders=20 etc.
  eu1/ eu2/ us2/ us1/
    wal-g.conf
    envoy/cds.yaml              # canonical (also in supabase docker volumes)
    envoy/lds.template.yaml
```

---

## 15. SFX Video Delivery — signing on the Go mesh, CF edge cache ✅

On-demand video (`objects.solofx.net`, OpenStack Swift / "QuObjects" on QNAP) is served
through a multi-layer cache. Signing was moved from the Cloudflare worker onto the **Go mesh**
(part of the Supabase cluster) to cut worker cost and get geo-latency.

### Chain
```
app → Go mesh (replicas) signSfxUrl → HMAC-signs the master playlist natively (SFX_SIGN_SECRET)
    → player fetches m3u8 from objects.solofx.net (worker accepts the Go token)
    → CF edge caches video segments by path (Cache Rule "sfx-s3-cache", query-string ignored)
    → on CF MISS: fill directly from QuObjects (origin is open — no token required)
    → QuObjects varnish (varnish-cache :8082 on ug) caches segments 24h
```

### Layers verified (live)
| Layer | What it caches | Status |
|---|---|---|
| Cloudflare Cache Rule (`sfx-s3-cache`) | Public objects (posters) 4h; segments 24h | ✅ `cf-cache-status: HIT` (query ignored) |
| QuObjects Varnish (`varnish-cache` :8082, ug) | Video segments `.m4s`/`.ts` 24h | ✅ `X-Cache: VARNISH-HIT` |
| Cloudflare + us1 Varnish | Catalog/EPG RPCs 60s | ✅ `x-cache: HIT` |
| Go mesh | HMAC signing (SFX_SIGN_SECRET) | ✅ native, byte-identical to worker |

### Key facts
- **QuObjects (Swift) is open** — GET object reads require no auth (verified: token/header all return 200).
  The only real gate is the worker's HMAC token check on the playlist.
- **Signing algorithm** (must match the worker exactly):
  `token = base64.RawURLEncoding( HMAC-SHA256(SFX_SIGN_SECRET, "${path}:${exp}") )`, `exp = now + 4h + 300s`.
  Implemented in Go as `sfxHMAC()` in `functions/superbase/ondemand_misc.go`; verified byte-identical.
- **Secrets**: `SFX_SIGN_KEY` (X-Sign-Key header) and `SFX_SIGN_SECRET` (HMAC key) both live in
  the Go service env. The worker's `SIGN_SECRET` was **rotated** to match (Cloudflare secrets are
  write-only — cannot be read back; rotate to set the same value on both).
- **Playlists** (`.m3u8`) are `no-store` / pass-through (they change during transcoding + carry
  per-session tokens). Only **segments** are cached at the edge.

### Cost / bandwidth
- Cloudflare **bandwidth is free** (all plans). The cost driver is **request/worker invocations**.
- Signing on the Go mesh (replicas) removes the worker from the signing path → worker only does
  playlist rewriting (~2-3 req/view) → ~$600+/mo → under $10/mo at 1M MAU.

### Open TODOs (playback repo — separate git)
- [ ] Move **playlist rewriting** to the Go mesh (fetch + rewrite m3u8 so segments point at plain
      CF-cached paths, no per-segment tokens) — currently the worker rewrites segment URIs.
- [ ] Point the app's `sfx_signing.dart` at the Go mesh instead of `objects.solofx.net/_sign`.
- [ ] Decide if per-segment token validation is wanted at all (origin is open; the playlist is the gate).

---

## 16. Security Hardening — us1/Edge (Supabase primary) ✅

Audit (Aug 2026) found the **write primary** was the exposed node: **no firewall**, SSH
**password auth on**, **no fail2ban**, no malware scanner, and Docker publishing 30+ ports
to `0.0.0.0` — including Postgres `5432`, Supabase pooler/Supavisor `55432/6543`, Redis
`6379`, Prometheus `9090`, Portainer `8000/9443/9001`, Loki `3100`, OpenWebUI `3000`, and
Directus (payment CMS admin) `8055`. The other 4 nodes were already correctly firewalled.

> ✅ **A firewall IS active.** It is enforced with **plain `iptables`**, not the `ufw`
> wrapper — `ufw` being "inactive" does not mean no firewall. `iptables` (the actual packet
> filter) has `INPUT policy DROP` + 10 explicit ACCEPT rules, and the `DOCKER-USER` chain has
> 11 DROP rules, all live and persisted via `edge-firewall.service`.
>
> ❗ **Why not `ufw`:** `ufw enable` flushes and rebuilds Docker's `FORWARD` chain, which
> would break **all** container networking on this production primary until Docker restarts.
> So UFW is kept as a config reference only and **never enabled**; the same rules are applied
> directly with `iptables` (zero outage risk).

### 16.1 Network containment ✅

**Plain-language summary of what the firewall does:**

| Traffic | Policy |
|---|---|
| Host itself (host INPUT) | **DENY by default** (`INPUT policy DROP`) — only the rules below are allowed |
| SSH `22`, web `80/443/81` (npm) | ✅ allowed (public) |
| Tailscale mesh `100.64.0.0/10` + `tailscale0` iface + WireGuard UDP `41641` | ✅ allowed (replication, backups, admin, exporters) |
| varnish shield `8556`, varnish video `8081` (from Docker bridge) | ✅ allowed (public + npm→varnish) |
| Docker-published web/streaming/payment ports (Envoy `8555`, owncast `1935/8080`, playitloud `3005/3010`, DRM `3001`, stats `8099`, Directus `8055`) | ✅ public (kept by decision) |
| Postgres `5432`, pooler `55432`, Supavisor `6543`, Redis `6379`, Prometheus `9090`, Portainer `8000/9443/9001`, Loki `3100`, video-downloader `3002` | 🔒 **Tailscale-only** (DOCKER-USER DROP from public) |
| fail2ban banned IPs | 🚫 dropped (`f2b-sshd` chain) |

**Implementation details:**

- **Host `INPUT`** (`/home/customer/host-input-firewall.sh`): default **DROP**. Allows
  loopback + established/related, the **Tailscale `ts-input` hook** (WireGuard UDP 41641 +
  CGNAT — preserved), Tailscale `100.64.0.0/10`, `22/80/443/81`, host-network varnish
  `8556` (public shield), and Docker bridge `172.16.0.0/12 → 8081/8556` (npm→varnish).
- **`DOCKER-USER`** (`/home/customer/docker-user-firewall.sh`): ACCEPT Tailscale `100.64.0.0/10`
  + Docker bridges `172.16.0.0/12` + loopback; then **DROP** public access to the restricted
  containers. Everything else (`RETURN`) stays public.
- **Corrected for Docker DNAT:** Docker rewrites the published→internal port *before*
  `DOCKER-USER` sees the packet (e.g. `55432→5432`, `3000→8080`, `8555→8000`). Rules therefore
  match **container destination IP + internal port**, not the published port — this was the bug
  that initially left OpenWebUI exposed and briefly blocked streaming-web/enovy. Container IPs
  are pinned in the script; **re-run the script if a container is recreated with a new IP.**
- **Persistence:** `edge-firewall.service` (oneshot, `After=docker.service tailscaled.service`)
  re-applies both scripts on boot; `RemainAfterExit=yes`. Also a copy of the pre-change
  iptables state is saved in `/home/customer/iptables-backup-*.rules`.

**Public-keep list (unchanged):** `80/443/81` (npm), `8555` (Envoy API), `8556` (varnish
shield), `1935`/`8080` (owncast), `3005`/`3010` (playitloud web/admin), `3001` (DRM), `8099`
(viewer stats), and `8055` (Directus CMS / payment gateway — kept public by decision).

**Tailscale-only now:** `5432`, `55432`, `6543`, `6379`, `9090`, `3000`, `8000`, `9443`,
`9001`, `3100`, `3002`. Replication, geo reads, and restic backups are **unaffected** — they
all travel over Tailscale (`100.64.0.0/10` accepted) and were verified live: 5 replicas
streaming with 0 lag, geo-routed anon reads 200 (US + spoofed `CF-IPCountry: UG` → ug), restic
snapshots reachable.

### 16.2 SSH + brute-force ✅
- `PasswordAuthentication no`, `MaxAuthTries 3` — set in `sshd_config` **and** the
  cloud-init drop-in `/etc/ssh/sshd_config.d/50-cloud-init.conf` (it overrode the main config).
  Verified: `Permission denied (publickey)` for password attempts; key auth still works.
- **fail2ban** (`/etc/fail2ban/jail.local`): `[sshd]` jail, `maxretry 5 / findtime 10m /
  bantime 10m`, `iptables-multiport`. Banned 4 scanning IPs within the first minutes of
  activation.

### 16.3 Malware scanning ⏳ (deferred)
- ClamAV + rkhunter + Lynis **install aborted** — `apt-get install` hung (killed cleanly,
  `dpkg --configure -a` completed with no errors, apt healthy, nothing half-installed).
  **TODO:** retry the install, then add a weekly scan cron (Sun 02:00) + daily on-demand
  ClamAV of `/media/data` (payment DB) and `restic check` in the backup cron.

### 16.4 Directus / payment layer (deferred by decision)
- Directus `docker-compose.yml` hardcodes `ADMIN_PASSWORD: "d1r3ctu5"` and
  `SECRET: "et45yehrffgdt4jshr"` in plaintext; `api_keys` table stores the Yo! Payments
  password in plaintext. **Not rotated** (user chose "backup only").
- **Directus DB backup is live** (`backup-directus-db.sh`, daily 03:15, separate restic repo
  `edge-directus`): SQLite online-backup → gzip → restic. 2 GB DB → ~148 MB stored; restore
  verified (151,894 `general_collections` rows + 629,068 `directus_activity` rows queryable).
- **TODO:** rotate Directus SECRET/ADMIN_PASSWORD + Yo/MTN keys; investigate IP
  `146.70.186.116` (21k activity events incl. `directus_flows` edits, no recorded login).

### 16.5 Streaming note
- OvenMediaEngine (`ovenmediaengine`, host-network) has been **exited 10 days** —
  `live.solofx.net` → 525. Left as-is by decision; not caused by the firewall (it was down
  before). Live streaming remains an open item outside this hardening scope.

---

## 17. OpenSearch — dedicated search node on the mesh ✅

New cluster node **`srt-node`** (`139.144.77.47`, SSH alias `edge-srt`, Debian 11, 2 cores /
3.8 GB) — repurposed SRT box now doubling as the mesh's **search node**. Tailnet IP
`100.127.244.33`; added via `tailscale up` (srt-node joins the existing tailnet — the new
firewall on us1 already ACCEPTs `100.64.0.0/10`, so no mesh changes needed).

### Node cleanup (memory)
- Was 3.4 GB used / 192 MB free. Root cause: **440 `docker-proxy` processes** spawned by
  `srt_proxy_pro` (402 published UDP ports `20000-20400`) + `restreamer`'s 24/7 ffmpeg.
- Stopped + `--restart=no`: `affectionate_pare` (srt_proxy_pro, 0 connections) and
  `restreamer`. Kept: `NginX` (npm :81/443), `Teradek_Sputnik` (SRT :554), `nimble`,
  portainer. **Result: 3.4 GiB → 788 MiB used (2.8 GiB free).**

### OpenSearch deployment
- **Container** `opensearchproject/opensearch:2.19.0`, `--restart unless-stopped`,
  `discovery.type=single-node`, `OPENSEARCH_JAVA_OPTS=-Xms1g -Xmx1g`.
- **Tailscale-only**: bound `100.127.244.33:9200` (HTTP) + `:9600` (metrics) — **not
  public** (verified blocked from the internet).
- `vm.max_map_count=262144` (persisted in `/etc/sysctl.conf`) — Lucene mmap requirement.
- Data persisted at `/opt/opensearch/data`. Security plugin **enabled** (TLS + auth; initial
  admin password set via `OPENSEARCH_INITIAL_ADMIN_PASSWORD`).
- Verified: cluster GREEN, full-text `match` + **fuzzy** search OK over the tailnet from us1
  and grafana; index/doc create/delete OK. (First search can miss a just-indexed doc until
  the 1s `refresh_interval` — use `?refresh=true` or allow 1s.)

### Indexer reads from REPLICAS — not the primary
A search index is a **derived cache**, so it must NOT depend on the write primary. The
connector reads catalog data from the **geo-routed read replicas** (tailnet, verified live
from srt-node):

| Replica | Endpoint (verified 200) | Path convention |
|---|---|---|
| us2 | `100.82.159.75:5557` | bare path `/tv_shows` |
| eu1 | `100.116.100.32:5557` | `/rest/v1/tv_shows` (nginx strips prefix) |
| eu2 | `100.99.30.100:5557` | `/rest/v1/tv_shows` (nginx strips prefix) |

- **Failover order:** nearest-region replica → any other replica → primary only as last
  resort. Index stays fresh even with the primary down (replicas keep serving reads).
- **Sync:** interval/cron re-index (RPCs on replicas) and/or Supabase Realtime
  `postgres_changes` (publication already exists) for incremental updates.
- **Source tables:** `tv_shows`, `epg_programs`, `events`, `ads`, `users` — Directus content
  (`articles` etc.) comes from the Directus SQLite DB (separate source), not the replicas.

### Sync connector ✅ (Go indexer, `gofn-indexer/`)
- **`salt-gofn-indexer`** — stdlib-only Go binary that reads catalog data from the
  **read replicas** over Tailscale and bulk-loads into OpenSearch. Deployed to srt-node
  (`/opt/opensearch-indexer/`), runs via systemd timer every **10 min** (oneshot, full
  idempotent rebuild per run).
- **Failover verified live:** dead replica first → times out (15 s) → next replica serves.
  Order: us2 (`:5557` bare) → eu1 (`:5557/rest/v1`) → eu2 (`:5557/rest/v1`). Primary is
  deliberately **not** in the list. Replica base URLs must include each node's path
  convention (us2 bare, eu1/eu2 `/rest/v1` nginx-strip).
- **Indexes:** `saltmedia-tv_shows`, `saltmedia-epg_programs`, `saltmedia-events`,
  `saltmedia-ads`, `saltmedia-users` (7,959 docs live: 8 shows, 145 EPG, 1 event, 7,805
  users with names). Users are paginated (1000/page; replicas return HTTP 206 for
  paginated reads, treated as success) and filtered to non-null names.
- **Verified:** full-text `match`, **fuzzy** typo search (`Sportz`→`Sports`), and
  multi-field search (title+description; user name+email) all return correct hits over
  the tailnet.

### App catalog search wired to OpenSearch ✅
- `supabase-api` (Go service, `/api/v1/catalog`) now queries **OpenSearch first** with
  fuzzy multi-field match on `saltmedia-tv_shows`, **falling back to PostgREST `ilike`**
  if OpenSearch is unreachable. Verified both paths:
  - OpenSearch: `q=Sportz` → `Sports` (response `search: opensearch`)
  - Fallback: stopping OpenSearch → same query returns via PostgREST (`search: postgrest`),
    and auto-recovers when OpenSearch returns.
- **App-facing `/rest/v1` search:** `search_catalog` PostgREST RPC (migration
  `008_search_catalog_rpc.sql`) runs on the replicas and is geo-routed/CDN-cached —
  the app calls `GET /rest/v1/rpc/search_catalog?q=...&type=...&limit=&offset=`. Uses
  pg_trgm similarity ranking over `tv_shows` (title/description). Verified 200 through
  `edge.solofx.net/rest/v1/rpc/search_catalog` for all geo regions.
- **Admin user search** (`/api/v1`): `getUsersPaginated` (and `searchUsers`) in the Go
  service back the admin dashboard's user-management page via OpenSearch `saltmedia-users`.
  Response shape matches the dashboard's `GetUsersPaginatedResponse`. Search ~0.13s,
  fuzzy typo tolerant (`masara`→`masera`/`Masaba Ronald`). No-search lists page via
  PostgREST. The dashboard needed **no frontend changes** (its `/api/users` route already
  proxies to `getUsersPaginated`, which previously 404'd).
- **Varnish search caching** ✅: the us1 origin shield (`supabase-varnish` on :8556,
  VCL `/home/customer/varnish/supabase-shield.vcl`) now caches **search responses** so
  repeated queries don't hit the Go service/OpenSearch:
  - `/api/v1/getUsersPaginated` + `/api/v1/searchUsers` → `X-Cache-Rule=search`, **10s TTL**
    (URL keyed: searchTerm/page/limit are in the query string; response is admin-shared,
    not per-user).
  - `/rest/v1/rpc/search_catalog` (app) already cached as anon `/rest/v1/*` read (60s).
  - Verified through `edge.solofx.net` (tunnel → :8556 shield): admin search and app
    `search_catalog` both MISS → HIT. Reload = `docker restart supabase-varnish`;
    pre-change VCL backed up as `supabase-shield.vcl.bak.<date>`.
- Enabled via `OPENSEARCH_URL` / `OPENSEARCH_USER` / `OPENSEARCH_PASSWORD` /
  `OPENSEARCH_INSECURE` env on the `supabase-api` container (recreated with
  `salt-gofn:new`).
- **OpenSearch credentials:** production uses the **`appsearch`** internal user
  (password generated + rotated at setup) scoped to role `saltmedia_search`
  (index pattern `saltmedia-*` only — verified 403 on system indices). The default
  `admin` user is reserved and kept as break-glass; the demo password from first boot
  is superseded — **do not re-use `OPENSEARCH_INITIAL_ADMIN_PASSWORD` in app config**.
- Build/deploy: `gofn-indexer/README.md` (env vars + examples).

### Joomla articles indexed + Joomla API proxied through the mesh ✅
- **Indexer source:** a small PHP exporter on the Joomla host (`/sfx-articles-export.php`,
  queries the local MySQL, returns published `state=1` articles as JSON, protected by an
  `X-Export-Key` header). The indexer pulls it **directly from the origin IP**
  (`https://65.181.111.128` + `Host: saltmedia.ug`) every 10 min, bypassing Cloudflare.
  5,122 published articles → `saltmedia-joomla_articles` index (title, body(stripped
  HTML), category, created, publish_up, featured, state, hits, created_by, author_name,
  images, etc.). Used for **fast catalog search** (`searchArticles`).
- **Dashboard/app article CRUD via the Joomla API** (`gofn/joomla.go`): the Go mesh
  (`supabase-api` on Edge) proxies the six dashboard functions to the **Joomla REST API**
  (`JOOMLA_API_URL`, Basic auth `saltapi`) — `getNewsArticles`, `getNewsArticle`,
  `createJoomlaArticle`, `updateJoomlaArticle`, `deleteJoomlaArticle`,
  `getJoomlaReference` (categories/authors/tags). This is the same path the origin access
  logs show working (Go-http-client from Edge, 200s). JSON:API passthrough, so the
  dashboard parses exactly what Joomla returns (titles, thumbnails, authors, state, hits).
- **Why the mesh (not direct-from-dashboard):** Cloudflare challenges Basic-auth requests
  to `/api/index.php/v1/` from arbitrary external IPs, but **Edge's** requests pass
  (trusted origin of traffic + Go-http-client UA). The dashboard's Next.js routes proxy to
  the mesh, which calls Joomla from Edge — reliable. The dashboard routes needed **no
  changes** (they already proxy to these mesh functions from commit `be2918e`).
- **Client details:** `joomlaClient` forces **IPv4** (`tcp4`) because the container DNS
  resolves `saltmedia.ug` to an IPv6 AAAA that hangs on the Docker bridge. `JOOMLA_API_URL`
  already includes `/api/index.php/v1` (paths appended directly).
- **Cloudflare note:** `browser_check` restored to **on**; the only WAF custom rule is the
  export-path block (`/sfx-articles-export.php`, 403 for non-`139.144.77.47`) as
  defense-in-depth. The mesh→Joomla API path works with browser_check on (verified).
- **Verified through `edge.solofx.net`:** articles list (state/hits/author/thumbnails),
  single article, categories (21), authors (33) all return correct Joomla JSON:API.

### News dashboard search = OpenSearch (fuzzy), indexed via the Joomla API ✅
- **Indexer pulls from the Joomla API (not a MySQL export)** for consistency: the indexer
  calls the mesh `getNewsArticles` endpoint (which proxies the Joomla API from Edge) in
  pages of 500 → `saltmedia-joomla_articles`. Single source of truth = the same API the
  dashboard reads/writes. 2,024 API-visible published articles indexed (matches the
  dashboard exactly; the API exposes fewer than the raw 5,122 DB rows due to its own
  filtering).
- **Search wiring:** `getNewsArticles?search=X` now queries **OpenSearch first** (fuzzy
  multi-field on title/body, optional category filter), mapping results back to the
  JSON:API shape the dashboard parses (titles, thumbnails, state, hits, category).
  **Falls back to the Joomla API** if OpenSearch is unavailable (verified live).
  List/pagination (no search term) still proxies the Joomla API unchanged.
- **Verified:** typo `sironk` → finds "Sironko Leaders…", `tooro` → 3 results with
  thumbnails, ~1.0-1.5s (faster than the ~3s Joomla API search), fallback works when
  OpenSearch is stopped. No dashboard code change needed (it calls `getNewsArticles?search=`).
- Indexer config: `MESH_BASE_URL` + `MESH_SERVICE_KEY` (replaces the old export-endpoint
  env). The PHP export endpoint (`/sfx-articles-export.php`) is retained but no longer used
  by the indexer; its CF block rule remains harmless.

### Open TODOs
- [ ] RBAC: scoped roles per index (`saltmedia_*` etc.) instead of admin account; rotate the
      initial admin password.
- [ ] Optional: Supabase Realtime `postgres_changes` incremental updates (the 10-min full
      rebuild is sufficient today).
- [ ] OpenSearch-Dashboards for attack-log analysis (§16.4 forensics) — bind tailnet-only.
- [ ] Decide: keep on this box (SRT co-tenant) vs move to a dedicated 8 GB VPS if search
      volume grows.

---

## 18. Joomla site — login lockdown + Cloudflare cache fix (saltmedia.ug) ✅

Debugging a frontend login failure (`/en/component/users/?task=user.login` redirect loop)
uncovered two separate root causes on the **Joomla site** (cPanel host `65.181.111.128`,
separate from the Supabase mesh):

### 18.1 Root cause 1 — Cloudflare was caching the login page
- The zone had a **site-wide cache rule** (`edge_ttl override_origin`, 86400s / 24h) that
  cached **all** Joomla pages, including `/component/users/` (login). A cached login page
  carries **no session cookie**, so the form CSRF token could never validate → login always
  failed (and a GET to `task=user.login` 303-looped).
- **Fix:** removed the aggressive site-cache rule; added a **login bypass** cache rule
  (`cache: false` for `/component/users/`, `/login`, `/edit/`) and a **static-asset cache**
  rule (JS/CSS/images/fonts → `override_origin`, 86400s). Pages now `respect_origin`
  (Joomla sends `no-store`, so they're not edge-cached; login is never cached).
- Also added `.htaccess` `Cache-Control: no-store` for login paths (belt-and-suspenders).
- **Still-open edge limitation:** Cloudflare **strips `Set-Cookie`** for `/component/users/`
  even with `cache: false` on this zone's Free plan (`cache_level: aggressive` not settable
  via API). Frontend login remains impractical through CF; **use the admin backend** for
  editing SP Page Builder pages. Fixing fully needs the dashboard cache_level change or a
  paid plan.

### 18.2 Root cause 2 — password auth was wide open / then too locked
- After earlier hardening, core `plg_authentication_joomla` (password for everyone) was
  **disabled** and a custom **`plg_authentication_soloonly`** plugin (only user `solo`,
  id 438) handled password auth. During debugging the core plugin was re-enabled (open),
  then **re-locked** to `soloonly`-only (verified: solo logs in, `saltapi` rejected).
- **Current auth state:** `joomla=0` (core password off), `soloonly=1` (solo-only password),
  `cookie=1`, WebAuthn passkey enabled. Solo has re-added MFA (TOTP) and changed password.
- **Plugin files** on the host: `plugins/authentication/soloonly/{soloonly.xml, src/, services/}`
  (extension_id 393). TOTP/MFA tables: `#__user_mfa`, `#__user_profiles`.

### 18.3 Operational notes
- **Editing SPP pages:** use the **admin backend** (`/administrator/` → Components → SP
  Page Builder). The frontend `/en/edit/6.html` editor requires frontend auth, which is
  blocked by the CF Set-Cookie limitation above.
- **Frontend login** for visitors is broken by the CF cookie limitation; admin login is fine.
- Cache rules live in zone `http_request_cache_settings` (order: login-bypass → static-assets
  → respect-origin default). Managed via the same CF API token used for the saltmedia.ug zone.

---