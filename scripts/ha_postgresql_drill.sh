#!/usr/bin/env bash
set -Eeuo pipefail

# Destructive operations are confined to a freshly-created temporary directory
# and loopback ports. Set XIRANG_HA_KEEP=1 to retain logs after a failure.

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
PG_BIN=${XIRANG_HA_PG_BIN:-/usr/lib/postgresql/16/bin}
PG_PORT=${XIRANG_HA_PG_PORT:-55432}
HTTPS_PORT=${XIRANG_HA_HTTPS_PORT:-18999}
GRPC_PORT=${XIRANG_HA_GRPC_PORT:-18444}
if [[ -n "${XIRANG_HA_WORK_DIR:-}" ]]; then
  WORK_DIR=$XIRANG_HA_WORK_DIR
  rm -rf "$WORK_DIR"
  mkdir -p "$WORK_DIR"
else
  WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/xirang-ha.XXXXXX")
fi
PG_DATA="$WORK_DIR/postgres"
PG_SOCKET="$WORK_DIR/socket"
ACTIVE_DATA="$WORK_DIR/controller-active"
STANDBY_DATA="$WORK_DIR/controller-standby"
RECOVERY_DATA="$WORK_DIR/controller-recovery"
RESTORE_DATA="$WORK_DIR/controller-restored"
NAMESPACE="ha-drill-$(date +%s)-$$"
DB_NAME=xirang_ha
RESTORE_DB=xirang_ha_restore
DSN="postgresql://$(id -un)@127.0.0.1:${PG_PORT}/${DB_NAME}?sslmode=disable"
RESTORE_DSN="postgresql://$(id -un)@127.0.0.1:${PG_PORT}/${RESTORE_DB}?sslmode=disable"
TOKEN=$(openssl rand -hex 32)
CONTROLLER_PID=
STANDBY_PID=
PG_STARTED=0

log() { printf '[ha-drill] %s\n' "$*"; }
fail() { printf '[ha-drill] ERROR: %s\n' "$*" >&2; exit 1; }

stop_pid() {
  local pid=${1:-}
  if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
    kill "$pid" 2>/dev/null || true
    for _ in {1..50}; do
      kill -0 "$pid" 2>/dev/null || return 0
      sleep 0.1
    done
    kill -KILL "$pid" 2>/dev/null || true
  fi
}

cleanup() {
  local status=$?
  stop_pid "$CONTROLLER_PID"
  stop_pid "$STANDBY_PID"
  if [[ "$PG_STARTED" == 1 ]]; then
    "$PG_BIN/pg_ctl" -D "$PG_DATA" -m immediate stop >/dev/null 2>&1 || true
  fi
  if [[ "${XIRANG_HA_KEEP:-0}" == 1 || $status -ne 0 ]]; then
    log "artifacts retained at $WORK_DIR"
  else
    rm -rf "$WORK_DIR"
  fi
  exit "$status"
}
trap cleanup EXIT INT TERM

for command in initdb pg_ctl createdb psql pg_dump pg_restore; do
  [[ -x "$PG_BIN/$command" ]] || fail "missing $PG_BIN/$command"
done
for command in curl openssl python3; do
  command -v "$command" >/dev/null || fail "missing command: $command"
done
[[ -x "$ROOT_DIR/bin/xirangd" ]] || fail "build bin/xirangd first (make build)"
[[ -x "$ROOT_DIR/bin/xirang-maint" ]] || fail "build bin/xirang-maint first (make build)"

mkdir -p "$PG_SOCKET" "$ACTIVE_DATA" "$STANDBY_DATA" "$RECOVERY_DATA" "$RESTORE_DATA"
printf '%s\n' "$TOKEN" | tee \
  "$ACTIVE_DATA/admin.token" "$STANDBY_DATA/admin.token" \
  "$RECOVERY_DATA/admin.token" "$RESTORE_DATA/admin.token" >/dev/null
chmod 0600 "$ACTIVE_DATA/admin.token" "$STANDBY_DATA/admin.token" \
  "$RECOVERY_DATA/admin.token" "$RESTORE_DATA/admin.token"

log "initializing isolated PostgreSQL on 127.0.0.1:$PG_PORT"
"$PG_BIN/initdb" -D "$PG_DATA" -U "$(id -un)" --auth-local=trust --auth-host=trust --no-locale >/dev/null
{
  printf "listen_addresses = '127.0.0.1'\n"
  printf 'port = %s\n' "$PG_PORT"
  printf "unix_socket_directories = '%s'\n" "$PG_SOCKET"
  printf 'fsync = on\nfull_page_writes = on\n'
} >> "$PG_DATA/postgresql.conf"
"$PG_BIN/pg_ctl" -D "$PG_DATA" -l "$WORK_DIR/postgresql.log" start >/dev/null
PG_STARTED=1
"$PG_BIN/createdb" -h 127.0.0.1 -p "$PG_PORT" "$DB_NAME"

start_controller() {
  local data=$1 dsn=$2 log_file=$3 mode=${4:-active}
  local standby_args=()
  [[ "$mode" == standby ]] && standby_args=(-standby)
  "$ROOT_DIR/bin/xirangd" \
    -listen "127.0.0.1:$HTTPS_PORT" \
    -agent-listen "127.0.0.1:$GRPC_PORT" \
    -data-dir "$data" -web-root= \
    -tls-host 127.0.0.1 \
    -external-state-dsn "$dsn" \
    -external-state-namespace "$NAMESPACE" \
    -remote-build-enabled=false \
    -job-artifact-root "$WORK_DIR/artifacts" \
    -build-source-root "$WORK_DIR/sources" \
    -build-workspace-root "$WORK_DIR/build-work" \
    -build-artifact-root "$WORK_DIR/build-artifacts" \
    "${standby_args[@]}" >"$log_file" 2>&1 &
  CONTROLLER_PID=$!
}

wait_https() {
  for _ in {1..150}; do
    if curl --silent --fail --insecure "https://127.0.0.1:$HTTPS_PORT/api/v1/version" >/dev/null; then
      return 0
    fi
    sleep 0.1
  done
  return 1
}

api() {
  local method=$1 path=$2 data=${3:-}
  local args=(--silent --show-error --insecure --request "$method" \
    -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json')
  [[ -n "$data" ]] && args+=(--data "$data")
  curl "${args[@]}" "https://127.0.0.1:$HTTPS_PORT$path"
}

queue_exists() {
  local name=$1
  api GET /api/v1/queues | python3 -c \
    'import json,sys; name=sys.argv[1]; data=json.load(sys.stdin); raise SystemExit(0 if any(x.get("name")==name for x in data.get("items",[])) else 1)' "$name"
}

log "starting active and passive controllers against one advisory-lock namespace"
start_controller "$ACTIVE_DATA" "$DSN" "$WORK_DIR/active.log"
wait_https || fail "active controller did not become ready"
ACTIVE_PID=$CONTROLLER_PID
start_controller "$STANDBY_DATA" "$DSN" "$WORK_DIR/standby.log" standby
STANDBY_PID=$CONTROLLER_PID
CONTROLLER_PID=$ACTIVE_PID
sleep 1
kill -0 "$STANDBY_PID" 2>/dev/null || fail "standby exited instead of waiting for leadership"
grep -q 'waiting for PostgreSQL leadership' "$WORK_DIR/standby.log" || fail "standby did not report lock contention"

MARKER_QUEUE="ha-marker-$$"
api PUT "/api/v1/queues/$MARKER_QUEUE" '{"weight":7,"backfill":true}' >"$WORK_DIR/queue-created.json"
queue_exists "$MARKER_QUEUE" || fail "durable marker was not written"

log "simulating abrupt active-controller failure"
kill -KILL "$ACTIVE_PID"
wait "$ACTIVE_PID" 2>/dev/null || true
CONTROLLER_PID=$STANDBY_PID
STANDBY_PID=
wait_https || fail "standby did not acquire leadership and bind the service port"
queue_exists "$MARKER_QUEUE" || fail "standby did not restore the committed control state"

log "creating and cryptographically verifying an online XIRANG recovery point"
BACKUP_JSON=$(api POST /api/v1/system/backups)
BACKUP_ID=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])' <<<"$BACKUP_JSON")
api POST "/api/v1/system/backups/$BACKUP_ID/verify" >"$WORK_DIR/backup-verified.json"
"$ROOT_DIR/bin/xirang-maint" verify "$STANDBY_DATA/backups/$BACKUP_ID" >"$WORK_DIR/maintenance-verify.log"

log "injecting recovery-point corruption; verification must reject it"
CORRUPT_BACKUP="$WORK_DIR/corrupt-backup"
cp -a "$STANDBY_DATA/backups/$BACKUP_ID" "$CORRUPT_BACKUP"
CORRUPT_FILE=$(find "$CORRUPT_BACKUP" -type f ! -name manifest.json -print -quit)
[[ -n "$CORRUPT_FILE" ]] || fail "recovery point contains no payload to corrupt"
printf '\001corrupt' >>"$CORRUPT_FILE"
if "$ROOT_DIR/bin/xirang-maint" verify "$CORRUPT_BACKUP" >"$WORK_DIR/corrupt-verify.log" 2>&1; then
  fail "corrupted recovery point passed verification"
fi

log "simulating PostgreSQL outage; writes must fail closed"
"$PG_BIN/pg_ctl" -D "$PG_DATA" -m fast stop >/dev/null
PG_STARTED=0
HTTP_CODE=$(curl --silent --insecure --output "$WORK_DIR/outage-write.json" --write-out '%{http_code}' \
  --request PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  --data '{"weight":9,"backfill":true}' \
  "https://127.0.0.1:$HTTPS_PORT/api/v1/queues/ha-outage-rejected") || true
[[ "$HTTP_CODE" == 500 ]] || fail "database outage write returned HTTP $HTTP_CODE instead of 500"

"$PG_BIN/pg_ctl" -D "$PG_DATA" -l "$WORK_DIR/postgresql-restarted.log" start >/dev/null
PG_STARTED=1
stop_pid "$CONTROLLER_PID"
CONTROLLER_PID=
start_controller "$RECOVERY_DATA" "$DSN" "$WORK_DIR/recovery.log"
wait_https || fail "controller did not recover after PostgreSQL restart"
queue_exists "$MARKER_QUEUE" || fail "committed marker was lost after database restart"
if queue_exists ha-outage-rejected; then
  fail "failed write leaked into durable state"
fi

log "dumping and restoring the PostgreSQL control-state database"
"$PG_BIN/pg_dump" -h 127.0.0.1 -p "$PG_PORT" --format=custom --file "$WORK_DIR/control-state.dump" "$DB_NAME"
sha256sum "$WORK_DIR/control-state.dump" >"$WORK_DIR/control-state.dump.sha256"
cp "$WORK_DIR/control-state.dump" "$WORK_DIR/control-state-corrupt.dump"
truncate -s 128 "$WORK_DIR/control-state-corrupt.dump"
if "$PG_BIN/pg_restore" --list "$WORK_DIR/control-state-corrupt.dump" >"$WORK_DIR/corrupt-dump.log" 2>&1; then
  fail "truncated PostgreSQL dump passed structural verification"
fi
"$PG_BIN/createdb" -h 127.0.0.1 -p "$PG_PORT" "$RESTORE_DB"
"$PG_BIN/pg_restore" -h 127.0.0.1 -p "$PG_PORT" --dbname "$RESTORE_DB" "$WORK_DIR/control-state.dump"
stop_pid "$CONTROLLER_PID"
CONTROLLER_PID=
start_controller "$RESTORE_DATA" "$RESTORE_DSN" "$WORK_DIR/restored.log"
wait_https || fail "controller did not start from restored PostgreSQL database"
queue_exists "$MARKER_QUEUE" || fail "restored database does not contain the committed marker"

log "PASS: lock exclusion, failover, fail-closed outage handling, corruption rejection, recovery-point verification, and PostgreSQL restore"
