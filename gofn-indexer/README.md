# salt-gofn-indexer

Catalog search indexer for the Salt TV mesh. Reads catalog data from the
**geo-routed read replicas** (never the write primary) over the Tailscale mesh
and indexes it into OpenSearch on `srt-node` (`100.127.244.33:9200`).

A search index is a derived cache — so the indexer keeps working even with the
primary down, and it never adds read load to the write path.

## Index layout

`<INDEX_PREFIX>-<table>`, one index per collection:

- `saltmedia-tv_shows` — on-demand catalog
- `saltmedia-epg_programs` — EPG guide
- `saltmedia-events` — events
- `saltmedia-ads` — ads
- `saltmedia-users` — app users (non-empty name only)

Each run does a full rebuild (delete index → recreate → bulk load) — correct
and idempotent at this scale. Large tables are paginated (1000/page) until a
short page or the `Content-Range` total is reached; replicas return HTTP 206
for paginated reads, which the indexer treats as success.

## Build

```sh
GOOS=linux GOARCH=amd64 go build -o salt-gofn-indexer-linux .
scp salt-gofn-indexer-linux edge-srt:/opt/opensearch-indexer/
```

## Run

```sh
# one full sync, then exit (used by the systemd timer)
INDEXER_MODE=once ./salt-gofn-indexer-linux

# stay running, re-index every N seconds
INDEXER_MODE=watch INDEXER_INTERVAL=300 ./salt-gofn-indexer-linux
```

### Env

| Var | Default | Notes |
|---|---|---|
| `OPENSEARCH_URL` | — (required) | `https://100.127.244.33:9200` |
| `OPENSEARCH_USER` | `admin` | |
| `OPENSEARCH_PASSWORD` | — (required) | |
| `OPENSEARCH_INSECURE` | unset | set `1` to skip TLS verify (self-signed demo cert) |
| `ANON_KEY` | — (required) | Supabase anon key for replica reads |
| `INDEX_PREFIX` | `saltmedia` | index name prefix |
| `REPLICAS` | us2 | comma-separated **full base URLs** in failover order, each including its path prefix |
| `INDEXER_MODE` | `once` | `once` or `watch` |
| `INDEXER_INTERVAL` | `300` | seconds between watch re-indexes |

### Replica base URLs (path conventions differ per node)

```sh
us2: http://100.82.159.75:5557              # bare paths
eu1: http://100.116.100.32:5557/rest/v1    # nginx strips /rest/v1
eu2: http://100.99.30.100:5557/rest/v1
```

The table is appended directly to the base URL. If a replica is down, the
indexer logs the failure and tries the next in the list (15 s per-replica
timeout).

## Production deployment (srt-node)

systemd oneshot + timer, runs every 10 min:

```sh
# /etc/systemd/system/opensearch-indexer.service  (Type=oneshot, env as above)
# /etc/systemd/system/opensearch-indexer.timer     (OnUnitActiveSec=10min)
systemctl enable --now opensearch-indexer.timer
```

Logs: `journalctl -u opensearch-indexer.service`.

## Search examples

```sh
# fuzzy typo tolerance
curl -sk -u admin:PASS https://100.127.244.33:9200/saltmedia-tv_shows/_search \
  -H 'Content-Type: application/json' \
  -d '{"query":{"match":{"title":{"query":"Sportz","fuzziness":"AUTO"}}}}'

# multi-field
curl -sk -u admin:PASS https://100.127.244.33:9200/saltmedia-tv_shows/_search \
  -H 'Content-Type: application/json' \
  -d '{"query":{"multi_match":{"query":"highlights","fields":["title","description"]}}}'
```

> Note: the initial admin password (`OPENSEARCH_INITIAL_ADMIN_PASSWORD`) should
> be rotated for production, and app queries should use scoped RBAC roles, not
> the `admin` account.
