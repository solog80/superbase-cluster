#!/usr/bin/env bash
# replica-config.sh — push Postgres settings to the primary + all replicas in
# lockstep. Postgres requires every replica's max_wal_senders / max_replication_slots
# to be >= the primary's; changing them on us1 alone crash-loops the replicas
# (see SUPABASE_PLAN.md Gotcha #10).
#
# Works on macOS default bash 3.2 (no associative arrays).
# Usage:  scripts/replica-config.sh  [--dry-run]
set -euo pipefail

DRY=""
[ "${1:-}" = "--dry-run" ] && DRY="echo [dry-run]"

# us1 primary writes into its conf.d include dir (durable named volume).
PRIMARY="Edge"
PRIMARY_CFG="/etc/postgresql-custom/conf.d/99-realtime.conf"

# Replicas override via their mounted wal-g.conf (bind mount on each node).
# Format: node:ssh-host:config-path
REPLICAS=(
  "eu1:origin-contabo:/opt/supabase-replica/wal-g.conf"
  "eu2:origin2-salt2:/opt/supabase-replica/wal-g.conf"
  "us2:Edge2:/opt/supabase-replica/wal-g.conf"
  "ug:nas-ts:/share/CACHEDEV1_DATA/supabase-replica/wal-g.conf"
)

PRIMARY_VALUES=$'max_replication_slots = 10\nmax_wal_senders = 20\nwal_keep_size = 8GB'
REPLICA_VALUES=$'hot_standby = on\nmax_wal_senders = 20\nmax_replication_slots = 10'

echo "==> primary (${PRIMARY})"
$DRY ssh "${PRIMARY}" "printf '%s\n' '${PRIMARY_VALUES}' > ${PRIMARY_CFG} && cat ${PRIMARY_CFG}"
echo "    -> ${PRIMARY_CFG} (restart supabase-db to apply)"

for entry in "${REPLICAS[@]}"; do
  node="${entry%%:*}"
  rest="${entry#*:}"
  host="${rest%%:*}"
  path="${rest#*:}"
  echo "==> replica ${node} (${host}:${path})"
  if [ "$node" = "ug" ]; then
    # ug config is admin-owned; write via a throwaway root container
    $DRY ssh "${host}" "D=/share/CACHEDEV1_DATA/.qpkg/container-station/bin/docker; \
      printf '%s\n' '${REPLICA_VALUES}' > /tmp/wal-g.conf && \
      \$D run --rm -v /tmp/wal-g.conf:/tmp/in.vcl -v ${path}:/v/out alpine:3.20 sh -c 'rm -f /v/out && cp /tmp/in.vcl /v/out' && cat ${path}"
  else
    $DRY ssh "${host}" "printf '%s\n' '${REPLICA_VALUES}' > ${path} && cat ${path}"
  fi
  echo "    -> restart supabase-replica on ${node} to apply"
done

echo
echo "NOTE: after writing configs, restart supabase-db (primary) + supabase-replica (each replica)."