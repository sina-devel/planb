#!/usr/bin/env bash
# End-to-end test of the agent (the part that moves data) against a throwaway local PostgreSQL
# server and an S3 endpoint served by `rclone serve s3` over a local directory.
# No docker or registry access needed. Run inside `nix develop` (needs postgresql, rclone, go).
set -euo pipefail
cd "$(dirname "$0")/.."

PGPORT_E2E=${PGPORT_E2E:-54329}
S3PORT=${S3PORT:-9099}
KEY=planbkey SECRET=planbsecret
T=$(mktemp -d "${E2E_TMP:-${TMPDIR:-/tmp}}/planb-e2e.XXXXXX")
S3PID=

cleanup() {
  pg_ctl -D "$T/pg" stop -m immediate >/dev/null 2>&1 || true
  [ -n "$S3PID" ] && kill "$S3PID" 2>/dev/null || true
  [ -n "${KEEP:-}" ] && echo "kept $T" || rm -rf "$T"
}
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

echo "==> building agent"
go build -o bin/planb ./cmd/planb

echo "==> starting postgres on :$PGPORT_E2E and s3 on :$S3PORT ($T)"
echo secret >"$T/pwfile"
initdb -D "$T/pg" -U postgres --auth=scram-sha-256 --pwfile="$T/pwfile" >/dev/null
pg_ctl -D "$T/pg" -l "$T/pg.log" -w start \
  -o "-p $PGPORT_E2E -c listen_addresses=127.0.0.1 -c unix_socket_directories=''" >/dev/null || { cat "$T/pg.log"; fail "postgres did not start"; }
mkdir -p "$T/s3/backups" "$T/s3/src" "$T/s3/restored"
start_s3() {
  rclone serve s3 --dir-cache-time 1s --addr "127.0.0.1:$S3PORT" --auth-key "$KEY,$SECRET" "$T/s3" >>"$T/s3.log" 2>&1 &
  S3PID=$!
  for _ in $(seq 50); do curl -s -o /dev/null "http://127.0.0.1:$S3PORT/" && break; sleep 0.2; done
}
start_s3

export PGPASSWORD=secret
psql() { command psql -h 127.0.0.1 -p "$PGPORT_E2E" -U postgres -v ON_ERROR_STOP=1 -tAq "$@"; }
psql -c "CREATE DATABASE app"
psql -d app -c "CREATE TABLE t(id int primary key, v text); INSERT INTO t SELECT g, md5(g::text) FROM generate_series(1,5000) g"

# agent TASK_JSON
agent() {
  PLANB_TASK="$1" PLANB_STORAGE_ACCESS_KEY=$KEY PLANB_STORAGE_SECRET_KEY=$SECRET \
    PLANB_BUCKET_ACCESS_KEY=$KEY PLANB_BUCKET_SECRET_KEY=$SECRET PGUSER=postgres \
    ./bin/planb agent 2>&1 | sed 's/^/    /'
  return "${PIPESTATUS[0]}"
}
S3="\"endpoint\":\"127.0.0.1:$S3PORT\",\"insecure\":true"
STORAGE="\"storage\":{$S3,\"bucket\":\"backups\",\"prefix\":\"planb\"}"
PG="\"host\":\"127.0.0.1\",\"port\":$PGPORT_E2E,\"sslMode\":\"disable\""

echo "==> postgres backup (per database + globals)"
agent '{"op":"backup","name":"pg1",'"$STORAGE"',"backupKey":"planb/backups/pg1/","postgres":{'"$PG"',"databases":["app"],"includeGlobals":true}}'
[ -s "$T/s3/backups/planb/backups/pg1/postgres/app.dump" ] || fail "app.dump missing"
[ -s "$T/s3/backups/planb/backups/pg1/postgres/globals.sql.gz" ] || fail "globals.sql.gz missing"

echo "==> postgres restore into app_restored (createDatabase + mapping)"
agent '{"op":"restore","name":"r1",'"$STORAGE"',"backupKey":"planb/backups/pg1/","postgres":{'"$PG"',"databaseMapping":{"app":"app_restored"},"createDatabase":true}}'
[ "$(psql -d app_restored -c 'SELECT count(*) FROM t')" = 5000 ] || fail "row count after restore"

echo "==> postgres restore with clean over modified data"
psql -d app_restored -c "DELETE FROM t WHERE id > 10"
agent '{"op":"restore","name":"r2",'"$STORAGE"',"backupKey":"planb/backups/pg1/","postgres":{'"$PG"',"databaseMapping":{"app":"app_restored"},"clean":true}}'
[ "$(psql -d app_restored -c 'SELECT count(*) FROM t')" = 5000 ] || fail "row count after clean restore"

echo "==> restore of a database that is not in the backup must fail"
if agent '{"op":"restore","name":"r-bad",'"$STORAGE"',"backupKey":"planb/backups/pg1/","postgres":{'"$PG"',"databases":["nope"]}}' >/dev/null; then
  fail "expected failure for unknown database"
fi

echo "==> full-server backup (pg_dumpall)"
agent '{"op":"backup","name":"pgall",'"$STORAGE"',"backupKey":"planb/backups/pgall/","postgres":{'"$PG"'}}'
gzip -dc "$T/s3/backups/planb/backups/pgall/postgres/all.sql.gz" | grep "CREATE DATABASE app" >/dev/null || fail "pg_dumpall content"

echo "==> bucket backup/restore"
for i in $(seq 1 25); do mkdir -p "$T/s3/src/data/dir$((i % 3))"; echo "file $i" >"$T/s3/src/data/dir$((i % 3))/f$i.txt"; done
echo skip >"$T/s3/src/other.txt"
agent '{"op":"backup","name":"b1",'"$STORAGE"',"backupKey":"planb/backups/b1/","bucket":{"s3":{'"$S3"',"bucket":"src","prefix":"data"}}}'
[ "$(find "$T/s3/backups/planb/backups/b1/bucket" -type f | wc -l)" = 25 ] || fail "expected 25 backed-up objects"
agent '{"op":"restore","name":"r3",'"$STORAGE"',"backupKey":"planb/backups/b1/","bucket":{"s3":{'"$S3"',"bucket":"restored","prefix":"copy/"},"overwrite":true}}'
[ "$(find "$T/s3/restored/copy" -type f | wc -l)" = 25 ] || fail "expected 25 restored objects"
[ "$(cat "$T/s3/restored/copy/dir1/f1.txt")" = "file 1" ] || fail "restored content"

echo "==> bucket restore with overwrite=false keeps existing objects"
echo changed >"$T/s3/restored/copy/dir1/f1.txt"
agent '{"op":"restore","name":"r4",'"$STORAGE"',"backupKey":"planb/backups/b1/","bucket":{"s3":{'"$S3"',"bucket":"restored","prefix":"copy/"},"overwrite":false}}'
[ "$(cat "$T/s3/restored/copy/dir1/f1.txt")" = "changed" ] || fail "overwrite=false replaced an object"

echo "==> bucket restore with prune removes objects that are not in the backup"
mkdir -p "$T/s3/restored/copy/extra"
echo stale >"$T/s3/restored/copy/extra/stale.txt"
echo outside >"$T/s3/restored/outside.txt"
sleep 1.5 # let rclone notice files written behind its back
agent '{"op":"restore","name":"r5",'"$STORAGE"',"backupKey":"planb/backups/b1/","bucket":{"s3":{'"$S3"',"bucket":"restored","prefix":"copy/"},"overwrite":true,"prune":true}}'
[ ! -e "$T/s3/restored/copy/extra/stale.txt" ] || fail "prune kept an object that is not in the backup"
[ -e "$T/s3/restored/outside.txt" ] || fail "prune deleted an object outside the target prefix"
[ "$(find "$T/s3/restored/copy" -type f | wc -l)" = 25 ] || fail "expected exactly 25 objects after prune"
[ "$(cat "$T/s3/restored/copy/dir1/f1.txt")" = "file 1" ] || fail "prune restore content"

echo "==> bucket restore with prune into the storage bucket keeps planb's backups"
echo stale >"$T/s3/backups/stale.txt"
sleep 1.5
agent '{"op":"restore","name":"r6",'"$STORAGE"',"backupKey":"planb/backups/b1/","bucket":{"s3":{'"$S3"',"bucket":"backups"},"prune":true}}'
[ ! -e "$T/s3/backups/stale.txt" ] || fail "prune kept a stale object in the storage bucket"
[ -s "$T/s3/backups/planb/backups/pg1/postgres/app.dump" ] || fail "prune deleted planb's own backups"
[ -e "$T/s3/backups/dir1/f1.txt" ] || fail "prune restore into the storage bucket"

echo "==> backup survives an S3 outage (retries with backoff)"
kill "$S3PID"; wait "$S3PID" 2>/dev/null || true
RETRY='"retry":{"attempts":8,"initialBackoff":1000000000,"maxBackoff":4000000000}'
agent '{"op":"backup","name":"b-outage",'"$STORAGE"',"backupKey":"planb/backups/b-outage/",'"$RETRY"',"bucket":{"s3":{'"$S3"',"bucket":"src","prefix":"data"}}}' >"$T/outage.log" 2>&1 &
APID=$!
sleep 4
start_s3
wait "$APID" || { cat "$T/outage.log"; fail "backup did not recover after the outage"; }
grep -q "retrying after error" "$T/outage.log" || { cat "$T/outage.log"; fail "expected retries in the agent log"; }
[ "$(find "$T/s3/backups/planb/backups/b-outage/bucket" -type f | wc -l)" = 25 ] || fail "expected 25 objects after the outage"

echo "==> all e2e checks passed"
