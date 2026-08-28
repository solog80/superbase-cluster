-- notifications: admin broadcast FCM sent-log + live status.
-- Added status/sent columns to track in-flight broadcasts from the admin UI.

CREATE TABLE IF NOT EXISTS notifications (
  id           text PRIMARY KEY,
  title        text NOT NULL,
  message      text NOT NULL DEFAULT '',
  type         text NOT NULL DEFAULT 'info',
  link         text,
  image_url    text,
  user_id      text,
  is_broadcast boolean NOT NULL DEFAULT false,
  recipients   integer NOT NULL DEFAULT 0,
  created_at   timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE notifications ADD COLUMN IF NOT EXISTS status text NOT NULL DEFAULT 'sent';
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS sent integer NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS notifications_created_idx ON notifications (created_at DESC);
CREATE INDEX IF NOT EXISTS notifications_status_idx ON notifications (status) WHERE status = 'sending';

ALTER TABLE notifications ENABLE ROW LEVEL SECURITY;
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = 'public' AND tablename = 'notifications' AND policyname = 'notifications_read') THEN
    CREATE POLICY notifications_read ON notifications FOR SELECT TO service_role USING (true);
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = 'public' AND tablename = 'notifications' AND policyname = 'notifications_write') THEN
    CREATE POLICY notifications_write ON notifications FOR ALL TO service_role USING (true) WITH CHECK (true);
  END IF;
END $$;