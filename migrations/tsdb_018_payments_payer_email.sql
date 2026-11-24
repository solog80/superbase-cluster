-- tsdb_018_payments_payer_email.sql
-- Store the app user's registered email on the payment ledger so contribution
-- receipts can be sent without relying on a phone/subscription lookup.
-- Anonymous users have no email and are skipped by the receipt poller.

ALTER TABLE public.payments ADD COLUMN IF NOT EXISTS payer_email text;

CREATE INDEX IF NOT EXISTS payments_payer_email_idx
  ON public.payments (payer_email)
  WHERE payer_email IS NOT NULL;
