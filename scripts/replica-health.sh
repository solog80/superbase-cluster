#!/usr/bin/env bash
# Replica / archiver / disk health check for the SaltTV Supabase primary (us1/Edge).
# Exits non-zero and prints an ALERT line when something needs attention.
# Optional webhook: set ALERT_WEBHOOK_URL in /etc/default/replica-health.
#
# Checks:
#   - root disk usage
#   - replication slot state (lost / unreserved / inactive)
#   - replication streaming state + large lag
#   - new WAL-G archive failures since the previous run
set -uo pipefail

DB_CONTAINER=${DB_CONTAINER:-supabase-db}
STATE_FILE=${STATE_FILE:-/var/tmp/replica-health.archiver}

psqlq() {
  docker exec -i "$DB_CONTAINER" psql -U postgres -d postgres -tAF, -c "$1" 2>/dev/null
}

alerts=()
warn() { alerts+=("$1"); }

# --- disk ---
disk=$(df -P / | awk 'NR==2{gsub("%","",$5); print $5}')
if [ -n "${disk:-}" ] && [ "$disk" -ge 85 ]; then
  warn "disk:/=${disk}%"
fi

# --- slots ---
while IFS=, read -r name active status behind; do
  [ -z "${name:-}" ] && continue
  case "$status" in
    lost) warn "slot:${name}=lost" ;;
    unreserved) warn "slot:${name}=unreserved" ;;
  esac
  if [ "$active" = "f" ]; then
    warn "slot:${name}=inactive(${behind})"
  fi
done < <(psqlq "select slot_name, active, coalesce(wal_status,'-'), coalesce(pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)),'-') from pg_replication_slots where slot_name not like 'supabase_realtime%'")

# --- replication lag ---
while IFS=, read -r app state lag; do
  [ -z "${app:-}" ] && continue
  if [ "$state" != "streaming" ]; then
    warn "repl:${app}=${state}"
  fi
  case "$lag" in
    *GB|*TB) warn "lag:${app}=${lag}" ;;
  esac
done < <(psqlq "select application_name, state, coalesce(pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn)),'-') from pg_stat_replication")

# --- archiver failures (delta) ---
fail=$(psqlq "select failed_count from pg_stat_archiver" | head -1)
fail=${fail:-0}
prev=$(cat "$STATE_FILE" 2>/dev/null || echo "$fail")
if [ "$fail" -gt "$prev" ]; then
  warn "archiver:failed_count+$((fail - prev))(total=${fail})"
fi
mkdir -p "$(dirname "$STATE_FILE")"
echo "$fail" > "$STATE_FILE" 2>/dev/null || true

if [ ${#alerts[@]} -gt 0 ]; then
  msg="ALERT: ${alerts[*]}"
  echo "$msg"
  echo "Hint: lost slots cannot stream — re-clone the affected replica."
  if [ -n "${ALERT_WEBHOOK_URL:-}" ]; then
    curl -fsS -m 5 -H 'Content-Type: application/json' \
      -d "{\"text\":\"$msg\"}" "$ALERT_WEBHOOK_URL" >/dev/null 2>&1 || true
  fi
  exit 1
fi

echo "OK: replicas healthy (disk ${disk}%, archiver failed=${fail})"
