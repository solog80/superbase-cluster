-- system_emails: idempotency + delivery log for server-side transactional mail.
-- event_key examples: welcome:<user_id>, contribution:<payment_id>.

CREATE TABLE IF NOT EXISTS system_emails (
  event_key  text PRIMARY KEY,
  kind       text NOT NULL,
  recipient  text NOT NULL DEFAULT '',
  status     text NOT NULL DEFAULT 'sending',
  error      text,
  created_at timestamptz NOT NULL DEFAULT now(),
  sent_at    timestamptz
);

CREATE INDEX IF NOT EXISTS system_emails_kind_idx ON system_emails (kind, created_at DESC);
CREATE INDEX IF NOT EXISTS system_emails_status_idx ON system_emails (status) WHERE status IN ('sending', 'failed');

ALTER TABLE system_emails ENABLE ROW LEVEL SECURITY;
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = 'public' AND tablename = 'system_emails' AND policyname = 'system_emails_read') THEN
    CREATE POLICY system_emails_read ON system_emails FOR SELECT TO service_role USING (true);
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = 'public' AND tablename = 'system_emails' AND policyname = 'system_emails_write') THEN
    CREATE POLICY system_emails_write ON system_emails FOR ALL TO service_role USING (true) WITH CHECK (true);
  END IF;
END $$;

NOTIFY pgrst, 'reload schema';
