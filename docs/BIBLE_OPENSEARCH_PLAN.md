# OpenSearch-backed Bible search — implementation plan

Status: **Planned** (Option B selected)
Owner: Saltmedia + superbase-cluster
Goal: Full-Bible verse-text search in the Saltmedia app, powered by OpenSearch, built as a reusable template for other content projects (Play it loud).

---

## Why OpenSearch

- Search-as-you-type (`phrase_prefix`), fuzzy (`fuzziness`), filtered faceting (`term` on version/book), pagination (`limit/offset`).
- Same pattern already running for **news articles** (`searchArticles`), **on-demand shows/episodes** (`searchOnDemand`), and **users** (`searchUsers`).
- ~155k short verse docs is trivial for the existing OpenSearch cluster (`srt-node`, `saltmedia-*` indices, `appsearch` role scoped to `saltmedia_search`).

## The reusable pipeline

```
Content → Postgres → gofn-indexer (replica read) → OpenSearch saltmedia-<entity>
                                                   ↓
Flutter UI ← MeshApiService (debounced) ← gofn mesh /api/v1/search<Entity> (osSearch)
```

Any entity in any project follows the same 4 steps. Play it loud just needs its content in a Postgres table.

---

## Part 1 — Data: Bible verses into Postgres, then indexed (Option B)

### 1a. Migration `010_bible.sql` (primary, mirrors `004_ondemand.sql`)

```sql
create table if not exists public.bible_verses (
  id           text primary key,                 -- '<version>|<book>|<chapter>|<verse>'
  version      text not null,                    -- en-kjv, en-web, en-bsb, en-asv, lg-olcb
  book         text not null,                    -- canonical id e.g. 'genesis'
  book_display text not null,                    -- display name e.g. 'Genesis' / 'Olubereberye'
  chapter      integer not null,
  verse        integer not null,
  text         text not null,
  unique (version, book, chapter, verse)
);
create index if not exists bible_verses_version_idx on public.bible_verses (version);

alter table public.bible_verses enable row level security;
create policy bible_verses_read on public.bible_verses for select to anon, service_role using (true);
grant select on public.bible_verses to anon, service_role;
```

### 1b. One-time backfill from the CDN

- Source: `https://cdn.jsdelivr.net/gh/wldeh/bible-api/bibles/<version>/books/<book>/chapters/<n>.json`
- Pull all ~1,189 chapters per version; **dedupe** the CDN's duplicated verses (reuse the `_dedupeChapter` logic already in `saltmedia/lib/services/BibleService.dart`).
- Insert ~155k rows (5 versions) — a small one-time load. Translations are static; no ongoing sync.
- **Side benefit:** once in Postgres, the reader can later be served from the mesh/PostgREST (geo-routed + Cloudflare-cached) and the jsdelivr origin can be dropped.

### 1c. Indexer (`gofn-indexer/main.go`)

Add one `indices[]` entry + mapping fields, then re-run the indexer once (systemd oneshot, 10-min timer):

```go
{index: "bible_verses", table: "bible_verses", selects: "id,version,book,book_display,chapter,verse,text"}
```

Mapping: `version`/`book`/`book_display` → `keyword`; `chapter`/`verse` → `integer`; `text` → `text`.

---

## Part 2 — Mesh endpoint (`superbase-cluster/gofn`)

1. Dispatch: `case "searchBible": s.handleSearchBible(w, r)` (anon-accessible, like `searchOnDemand`).
2. `handleSearchBible`: parse `q`, `version`, `book`, `limit`, `offset`; call `osSearchBible`.
3. `osSearchBible`: query `saltmedia-bible_verses` via the existing `osSearch` helper:
   - `multi_match` `type: phrase_prefix` on `text` + `book_display` (so "romans" or "for god so loved" works);
   - `term` filter on `version` (required) and optional `book`;
   - sort `_score`; paginate `limit/offset`.

Response contract:

```json
{
  "success": true,
  "data": {
    "verses": [
      {"id": "en-kjv|john|3|16", "version": "en-kjv", "book": "john",
       "book_display": "John", "chapter": 3, "verse": 16, "text": "For God so loved the world..."}
    ],
    "count": 123,
    "limit": 20,
    "offset": 0
  }
}
```

---

## Part 3 — App (`BiblePage.dart`)

1. `MeshApiService.searchBible(q, {version, book, limit, offset})` via the existing `_getApi` helper.
2. AppBar search icon → debounced (450ms) field — the news/ondemand pattern: screen stays intact, thin progress bar, results replace when they land, no-results state, Luganda strings.
3. Result tile: `Book Chapter:Verse — snippet` (+ translation name). Tap → `loadChapter(version, book, chapter)` then select + scroll to the verse.
4. Existing local book-name filter in the passage picker stays untouched.

---

## Part 4 — Deploy & verify

1. Apply migration + backfill → re-run indexer on edge-srt → `saltmedia-bible_verses` built.
2. Build + deploy `gofn` mesh to Edge (backup binary, `salt-gofn:new`, swap `supabase-api` container).
3. Verify live via anon key: `searchBible?q=love&version=en-kjv`, `q=for god so loved`, `q=romans`.
4. Build app APK → `adb install` → confirm search + tap-to-verse.

---

## How this transfers to Play it loud

- **Built once, reused everywhere:** the `osSearch` helper, one `indices[]` entry, one `search<Entity>` dispatch+handler template, the `{data:{entity,count}}` response contract, `MeshApiService._getApi`, and the Flutter debounced-search UI widget. After Bible, adding search to any Play it loud entity is ~30 minutes.
- **Proven options:** fuzzy (`fuzziness`), search-as-you-type (`phrase_prefix`), faceting (`term` on version/book), pagination, and Varnish caching of search responses (10s TTL, already configured for other search endpoints).
- **Known costs/limits:** OpenSearch is a separate cluster to run/secure (already done here: `appsearch` user, index pattern `saltmedia-*`); content must land in Postgres before it's searchable; re-index is a full rebuild every 10 min (fine at this scale, revisit past ~1M docs).

---

## Open decisions

1. **Versions to index:** all 5 (`en-kjv`, `en-web`, `en-bsb`, `en-asv`, `lg-olcb`) vs English + Luganda only. (All 5 is ~30k extra rows.)
2. **Search scope:** scoped to the currently selected version (recommended) vs across all versions (show translation per hit).
