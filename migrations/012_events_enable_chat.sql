-- 012_events_enable_chat.sql
-- Add enable_chat column to public.events table

ALTER TABLE public.events ADD COLUMN IF NOT EXISTS enable_chat BOOLEAN NOT NULL DEFAULT true;

-- Reload PostgREST schema cache
NOTIFY pgrst, 'reload schema';
