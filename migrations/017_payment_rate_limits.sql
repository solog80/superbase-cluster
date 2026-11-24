-- payment_rate_limits: ephemeral per-key counters for Yo! payment initiation.
-- Hot path lives on the Supabase primary (local to the mesh); NOT Upstash.
-- UNLOGGED is intentional: losing counters on a primary restart only relaxes
-- rate limiting briefly, and it avoids WAL pressure on the primary.

CREATE UNLOGGED TABLE IF NOT EXISTS public.rate_limits (
  key           text PRIMARY KEY,
  window_start  timestamptz NOT NULL DEFAULT now(),
  count         integer NOT NULL DEFAULT 0
);

CREATE OR REPLACE FUNCTION public.check_rate_limit(
  p_key text,
  p_limit int,
  p_window_seconds int
) RETURNS boolean
LANGUAGE plpgsql
AS $$
DECLARE
  v_cutoff timestamptz := now() - make_interval(secs => greatest(p_window_seconds, 1));
  v_count  int;
BEGIN
  INSERT INTO public.rate_limits (key, window_start, count)
  VALUES (p_key, now(), 1)
  ON CONFLICT (key) DO UPDATE SET
    count = CASE WHEN public.rate_limits.window_start < v_cutoff
                 THEN 1 ELSE public.rate_limits.count + 1 END,
    window_start = CASE WHEN public.rate_limits.window_start < v_cutoff
                        THEN now() ELSE public.rate_limits.window_start END
  RETURNING count INTO v_count;

  RETURN v_count <= greatest(p_limit, 1);
END;
$$;

ALTER TABLE public.rate_limits ENABLE ROW LEVEL SECURITY;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_policies
    WHERE schemaname = 'public' AND tablename = 'rate_limits' AND policyname = 'rate_limits_service'
  ) THEN
    CREATE POLICY rate_limits_service ON public.rate_limits
      FOR ALL TO service_role USING (true) WITH CHECK (true);
  END IF;
END $$;

NOTIFY pgrst, 'reload schema';
