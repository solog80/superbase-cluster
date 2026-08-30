-- users search: pg_trgm GIN indexes for fast ILIKE '%term%' lookups on
-- the 100k-row users table (name + email). Used by getUsersPaginated
-- (or(name.ilike.*t*,email.ilike.*t*)).

CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE INDEX IF NOT EXISTS users_name_trgm_idx ON users USING gin (name gin_trgm_ops);
CREATE INDEX IF NOT EXISTS users_email_trgm_idx ON users USING gin (email gin_trgm_ops);