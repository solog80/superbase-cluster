-- tsdb_017_payments.sql
-- Salt TV payments ledger for dashboard analytics (up to ~1 year).
-- Runs on the TimescaleDB analytics instance (ug/QNAP, 100.116.185.70:55439,
-- db analytics) — NOT the Supabase OLTP primary.
--
-- One mutable row per payment (status changes update the row), partitioned by
-- created_at for cheap time-range scans. Status updates never compress this
-- hypertable, so no compression policy is enabled.
--
-- payment_uid is deterministic: '<PROVIDER>:<external_id>'
--   YO -> external_id = Yo! TransactionReference
-- The ledger is populated by the Go mesh (new app + legacy IPN bridge).
-- No Directus dependency.
--
-- Upserts are update-first under a per-payment advisory lock (see
-- gofn/payments.go), so created_at stays stable and retries never duplicate.

CREATE TABLE IF NOT EXISTS public.payments (
  payment_uid           text NOT NULL,
  created_at            timestamptz NOT NULL,
  updated_at            timestamptz NOT NULL DEFAULT now(),
  provider              text NOT NULL,
  external_id           text NOT NULL,
  external_ref          text,
  payment_id            text,
  phone_number          text,
  payer_name            text,
  amount                numeric(14,2) NOT NULL DEFAULT 0,
  currency              text NOT NULL DEFAULT 'UGX',
  status                text NOT NULL DEFAULT 'PENDING',
  status_reason         text,
  narrative             text,
  transaction_id        text,
  issued_receipt_number text,
  network_ref           text,
  source                text,
  raw                   jsonb,
  PRIMARY KEY (payment_uid, created_at)
);

SELECT create_hypertable('public.payments', 'created_at', if_not_exists => true);

CREATE INDEX IF NOT EXISTS payments_created_idx
  ON public.payments (created_at DESC);
CREATE INDEX IF NOT EXISTS payments_provider_status_idx
  ON public.payments (provider, status, created_at DESC);
CREATE INDEX IF NOT EXISTS payments_status_updated_idx
  ON public.payments (status, updated_at DESC);
CREATE INDEX IF NOT EXISTS payments_phone_idx
  ON public.payments (phone_number, created_at DESC);
CREATE INDEX IF NOT EXISTS payments_uid_idx
  ON public.payments (payment_uid);
CREATE INDEX IF NOT EXISTS payments_external_ref_idx
  ON public.payments (external_ref)
  WHERE external_ref IS NOT NULL;

-- Current-state read (status polling / IPN upserts) under an advisory lock.
-- Kept in Go so the same logic can enqueue a durable outbox job when TSDB is
-- unavailable; no SQL function needed here.
