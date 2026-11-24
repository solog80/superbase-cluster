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
drainer replays them when the database returns, draining TV/radio traffic ahead
of analytics snapshots. Clients are unchanged: the edge still returns HTTP 200
with the same response fields.
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

## System email (welcome + contribution receipts)

`gofn` sends transactional mail over SMTP and stores an idempotency/audit row in
Supabase `system_emails` (migration `015_system_emails.sql`). Two pollers run in
the background:

- **Welcome** — new `users` rows with a real email get a welcome message.
  Anonymous users are skipped.
- **Contribution receipt** — completed rows in the Timescale `analytics.payments`
  ledger (`status = 'COMPLETED'`) are matched back to an app account
  (subscription `transactionId`, then subscription/user phone) and the
  registered email gets a receipt. Idempotency is the Supabase `system_emails`
  key `contribution:<payment_uid>`. If no registered email is found the row is
  logged as `skipped` and no mail is sent. No Directus dependency.

Apply the migration on the primary, then recreate `supabase-api` with the SMTP
env file mounted read-only. Keep the file outside the Docker build context
(`/opt/supabase-gofn/`) so it can never be baked into an image layer:

```sh
install -d -m 700 /opt/salt-secrets
install -m 600 /dev/null /opt/salt-secrets/email.env
# write SMTP_USER / SMTP_PASS / SMTP_FROM into the file (e.g. `read -s`)
chown root:root /opt/salt-secrets/email.env
```

`email.env` contains:

```sh
SMTP_HOST=mail.saltmedia.ug
SMTP_PORT=587
SMTP_USER=no-reply@saltmedia.ug
SMTP_PASS=...
SMTP_FROM=no-reply@saltmedia.ug
SMTP_FROM_NAME=Salt Media
EMAIL_SUPPORT_ADDRESS=info@saltmedia.ug
```

Then add these to the `supabase-api` `docker run`:

```sh
-v /opt/salt-secrets/email.env:/app/email.env:ro \
-e ENV_FILE=/app/email.env
```

The service loads it at startup (`loadDotEnv`) and none of the SMTP values appear
in shell history or `docker inspect` — only the path. The file is read at
startup, so rotating any value needs a container restart. `SMTP_PASS_FILE` is
also supported if you prefer a password-only file; `SMTP_PASS` remains available
for local runs.

`SMTP_PORT=465` can be used with `SMTP_TLS=implicit`; 587 uses STARTTLS.
Optional tuning: `EMAIL_DRY_RUN=1` (log instead of send), `EMAIL_REDIRECT_TO`
(send every message to one address, original recipient in the subject — use this
for the first live test), `EMAIL_SUPPORT_ADDRESS`, `WELCOME_EMAIL_ENABLED`,
`WELCOME_EMAIL_INTERVAL_SECONDS` (60), `WELCOME_EMAIL_LOOKBACK_MINUTES` (15),
`WELCOME_EMAIL_SINCE` (RFC3339 floor for the first deploy),
`CONTRIBUTION_EMAIL_ENABLED`, `CONTRIBUTION_EMAIL_INTERVAL_SECONDS` (30),
`CONTRIBUTION_EMAIL_LOOKBACK_HOURS` (24), `CONTRIBUTION_EMAIL_SINCE` (RFC3339
floor for the first deploy), `CONTRIBUTION_EMAIL_LIMIT` (200),
`CONTRIBUTION_FALLBACK_EMAIL` (internal inbox for payments that cannot be
matched to an app account).

> **Contribution receipt caveat.** `ConditionalPaymentForm.dart` only submits a
> payer name and phone number — no user id or email. General contributions
> therefore cannot be linked to an app account unless a Firestore
> `subscriptions` document exists for that transaction/phone (subscription
> purchases) or a Firestore `users` document stores that phone. In practice,
> recent general contributions matched **0** accounts, so they are logged as
> `skipped` unless `CONTRIBUTION_FALLBACK_EMAIL` is set. Sending a receipt to a
> contributor requires capturing an email/user id in the payment flow.

Service-role endpoint for ad-hoc mail:

```sh
curl -X POST -H "apikey: $SERVICE_ROLE_KEY" -H "Content-Type: application/json" \
  https://edge.solofx.net/api/v1/sendSystemEmail \
  -d '{"to":"you@saltmedia.ug","subject":"Test","text":"Hello from the mesh"}'
```

## Payments (Yo! → TimescaleDB + receipts)

The new app pays via the Dart BFF (no API key in the client) → Go mesh → Yo!.
The mesh writes a single payment ledger to **TimescaleDB** (`analytics.payments`,
`migrations/tsdb_017_payments.sql`) for the admin dashboard (up to ~1 year).
Supabase Postgres keeps only the ephemeral rate-limit counters
(`migrations/017_payment_rate_limits.sql`); Upstash is not used on this path.

- **Legacy compatibility:** the Firebase `initiateYoDeposit`,
  `checkYoTransactionStatus`, and `yoWebhooks` functions stay deployed and their
  IPN/failure URLs are unchanged. They call `POST /api/v1/recordYoPayment`
  (service key) best-effort, so post-cutover legacy payments also appear in the
  new ledger. No historical backfill is performed.
- **IPNs:** the Firebase `yoWebhooks` IPN/failure URLs stay unchanged for old
  app builds. The mesh already exposes `POST /api/v1/yoIpn` and
  `/api/v1/yoFailure` (public, verify-via-Yo!) so switching the URLs after the
  migration is a config-only change.
- **Email receipts:** the existing SMTP mailer sends contribution receipts from
  the Timescale ledger (`status = 'COMPLETED'`) with `system_emails` idempotency
  (`contribution:<payment_uid>`). The app sends the signed-in user's registered
  email (`payer_email`); anonymous users have no email and are skipped. The Yo!
  IPN `payer_email` is also captured when present. Old-app MoMo (Directus-only)
  is not included in this Timescale receipt poller. See the **System email**
  section above.
- **KYC:** the mesh exposes `POST /api/v1/getMsisdnKycInfo` (Yo!
  `acgetmsisdnkycinfo`) and the BFF proxies it at
  `/api/v1/bff/payments/yo/kyc`. Per Yo! support, KYC uses the dedicated
  sandbox `YO_KYC_*` URL/credentials while payments keep using the production
  `YO_API_*`. The Flutter form verifies both MTN and Yo! numbers before payment,
  and the mesh **re-verifies on `initiateYoDeposit`** so a client cannot bypass
  it. Unverified/blocked/blacklisted/lookup-failed numbers are rejected;
  KYC outage/access errors fail closed. Results are cached
  (`YO_KYC_CACHE_TTL_SECONDS`, default 600) and rate-limited to avoid hammering
  Yo!/the mesh. This prevents unregistered/bot submissions and over-sending.
  The legacy Firebase path and the current Firebase MoMo proxy are not
  server-enforced yet; the new app UI still gates them.
- **Durability:** if Timescale is unreachable, payment rows are queued in the
  existing bbolt outbox (`payment.upsert` jobs) and replayed. Status reads never
  depend on Timescale — the mesh asks Yo! directly.

### Mesh env (us1 `supabase-api`)

Keep these in **`superbase-cluster/.env`** (gitignored, local source of truth),
then deploy that file to Edge as `/opt/salt-secrets/payments.env` and pass it
with `--env-file /opt/salt-secrets/payments.env` (secrets stay out of the image
and `docker inspect`):

```sh
YO_API_URL=https://paymentsapi2.yo.co.ug/ybs/task.php
YO_API_USERNAME=...
YO_API_PASSWORD=...
YO_KYC_API_URL=https://paymentsdev1.yo.co.ug/yopaytest/task.php
YO_KYC_API_USERNAME=...
YO_KYC_API_PASSWORD=...
YO_IPN_URL=https://us-central1-salt-media-app1.cloudfunctions.net/yoWebhooks/yo-ipn
YO_FAILURE_URL=https://us-central1-salt-media-app1.cloudfunctions.net/yoWebhooks/yo-failure
```

`YO_IPN_URL` / `YO_FAILURE_URL` intentionally stay on Firebase so legacy app
builds keep working. Mesh-initiated deposits inherit the same URLs; after the
migration, point them at `https://edge.solofx.net/api/v1/yoIpn` and
`https://edge.solofx.net/api/v1/yoFailure`.

### Endpoints (Go mesh)

Public (Yo! cannot send an API key, so each re-queries Yo! first):
- `POST /api/v1/yoIpn`
- `POST /api/v1/yoFailure`

Service-role only:
- `POST /api/v1/initiateYoDeposit` — `{amount, phoneNumber, narrative, payerName, externalRef}`
- `POST /api/v1/checkYoTransactionStatus` — `{transactionReference | privateTransactionReference}`
- `POST /api/v1/recordYoPayment` — legacy Firebase bridge; re-queries Yo! and upserts
- `GET  /api/v1/getPaymentMetrics?days=365` — dashboard metrics (Timescale)

The BFF calls the first two service-only routes; the admin dashboard calls
`getPaymentMetrics`; Firebase calls `recordYoPayment`.

### Deploy order

1. Apply `migrations/017_payment_rate_limits.sql` on the Supabase primary.
2. Apply `migrations/tsdb_017_payments.sql` on the TimescaleDB instance.
3. Rebuild/redeploy `supabase-api` with `payments.env` (Yo! + SMTP env).
4. Rebuild/redeploy `saltmedia-bff` on us1 with `GO_API_URL` + `MESH_SERVICE_KEY`.
5. Deploy the app (hard switch to `/api/v1/bff/payments/yo/*`).
6. Deploy the updated Firebase functions (best-effort `recordYoPayment`).
7. Deploy the admin app (payments route now calls the mesh).

## MoMo (new app via mesh; old app stays Directus)

- **New app:** BFF `/api/v1/bff/payments/momo/{initiate,status}` → mesh
  `initiateMomoPayment` / `checkMomoPaymentStatus` → MTN MoMo RequestToPay/status
  → Timescale `payments` with `provider = MTNUG_MOMO`. No Directus.
- **Callback/IPN:** the mesh sends `X-Callback-Url` =
  `MOMO_CALLBACK_URL?reference=<ref>` and exposes public
  `POST /api/v1/momoCallback`. **The active callback is the mesh URL.** The
  existing Directus flow URL is commented out for the legacy/cutover path.
  New-app MoMo no longer creates Directus rows; old-app MoMo still uses the
  Directus flow. The MTN dashboard default callback can remain the CMS URL
  until cutover because the per-request mesh callback overrides it — switching
  the dashboard default is a config change, no app update.
- **Old app:** Firebase `proxyMomoPayment` → Directus flow is unchanged; old-app
  MoMo records stay in Directus (no Timescale sync, by decision). They are also
  not included in the Timescale dashboard or receipt poller. Keep the Directus
  callback untouched.
- **BFF auto-update:** pushes touching `saltmedia/packages/saltmedia_bff/**` on
  `backup-play-pause-fix` build and push
  `ghcr.io/solog80/saltmedia-bff:latest`; Watchtower updates Edge/eu1/ug. Every
  BFF container must have `GO_API_URL` and `MESH_SERVICE_KEY`; Edge must carry
  the `com.centurylinklabs.watchtower.enable=true` label.
- **KYC:** the mesh re-verifies the number server-side on initiate
  (`lookupMsisdnKyc`); the app UI gates too. Mobile-money enablement still
  depends on MTN RequestToPay (validity permission pending).
- **Env:** `MTN_MOMO_ENV`, `MTN_MOMO_SUBSCRIPTION_KEY`, `MTN_MOMO_API_USER`,
  `MTN_MOMO_API_KEY`, `MOMO_CALLBACK_URL` (in `superbase-cluster/.env`, synced to
  `/opt/salt-secrets/email.env`).
- **Service-only endpoints:** `initiateMomoPayment`,
  `checkMomoPaymentStatus`; **public:** `momoCallback`.

## Secrets

Secrets are NOT in this repo. They live as env vars / container env / Cloudflare
worker secrets. See `docs/SUPABASE_PLAN.md` §15 for the SFX signing secret sync.