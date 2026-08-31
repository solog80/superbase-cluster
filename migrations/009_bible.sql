-- 009_bible.sql
-- Salt TV → Supabase: full Bible text, one row per verse, for OpenSearch search.
-- Content is static (public-domain translations); loaded once via a backfill
-- from the wldeh/bible-api CDN. Runs on the Supabase primary (us1) via psql.

-- =============================================================
-- bible_verses — one row per verse per translation
-- =============================================================
create table if not exists public.bible_verses (
  id           text primary key,   -- '<version>|<book>|<chapter>|<verse>'
  version      text not null,      -- en-kjv, en-web, en-bsb, en-asv, lg-olcb
  book         text not null,      -- canonical id e.g. 'genesis'
  book_display text not null,      -- display name e.g. 'Genesis' / 'Olubereberye'
  chapter      integer not null,
  verse        integer not null,
  text         text not null,
  unique (version, book, chapter, verse)
);
create index if not exists bible_verses_version_idx on public.bible_verses (version);

-- =============================================================
-- RLS: anon + service_role read (public content); service_role owns writes
-- (backfill/loads). Mirrors seasons/episodes.
-- =============================================================
alter table public.bible_verses enable row level security;

create policy bible_verses_read on public.bible_verses for select to anon, service_role using (true);
create policy bible_verses_admin_all on public.bible_verses for all to service_role using (true) with check (true);

grant select on public.bible_verses to anon, service_role;
grant insert, update, delete on public.bible_verses to service_role;
