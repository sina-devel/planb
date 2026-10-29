#!/usr/bin/env bash
# End-to-end test of an installed planb (operator, agent and UI) in a real cluster.
#
# It creates a throwaway namespace with PostgreSQL, a second empty PostgreSQL, an S3 endpoint (rclone serve s3)
# and a webhook receiver, then exercises every feature: storage locations, PostgreSQL and bucket backups and
# restores, prune, retries through an S3 outage, schedules with keepLast, TTL, deletion with and without
# retain-data, sync from storage, read-only locations, notifiers and the UI API. The namespace is deleted at
# the end unless KEEP=1.
#
#   KUBE_CONTEXT=k3s IMAGE_PREFIX=localhost:5000/ hack/e2e-k8s.sh
#
# IMAGE_PREFIX is prepended to the test images (postgres:18-alpine, rclone/rclone:1.75, node:22-alpine) for
# clusters that cannot pull from Docker Hub. The UI checks need the chart's UI enabled with basic auth.
set -euo pipefail
cd "$(dirname "$0")/.."

NS=${NS:-planb-e2e}
P=${IMAGE_PREFIX:-}
PLANB_NS=${PLANB_NS:-planb-system}
k() { kubectl ${KUBE_CONTEXT:+--context "$KUBE_CONTEXT"} "$@"; }
G=planb.sina-devel.github.io
PASSED=0

fail() { echo "FAIL: $*" >&2; k get "backups.$G,restores.$G,schedules.$G,backupstoragelocations.$G,notifiers.$G" -n "$NS" >&2 || true; exit 1; }
ok() { PASSED=$((PASSED + 1)); echo "    ok: $*"; }
cleanup() {
  [ -n "${PF_PID:-}" ] && kill "$PF_PID" 2>/dev/null || true
  if [ -n "${KEEP:-}" ]; then echo "kept namespace $NS"; else k delete ns "$NS" --wait=false >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT

# wait_for DESCRIPTION TIMEOUT_SECONDS CONDITION: evaluates the shell CONDITION until it succeeds.
wait_for() {
  local what=$1 timeout=$2 cond=$3
  local end=$((SECONDS + timeout))
  until eval "$cond" >/dev/null 2>&1; do
    [ $SECONDS -ge $end ] && fail "timed out after ${timeout}s waiting for $what"
    sleep 2
  done
}
phase() { k get "$1.$G" "$2" -n "$NS" -o jsonpath='{.status.phase}' 2>/dev/null; }
# wait_phase KIND NAME PHASE [TIMEOUT]: fails early when a different final phase is reached.
wait_phase() {
  local kind=$1 name=$2 want=$3 timeout=${4:-180} end=$((SECONDS + ${4:-180})) got
  while :; do
    got=$(phase "$kind" "$name")
    [ "$got" = "$want" ] && return 0
    case "$got" in Completed | Failed | FailedValidation)
      fail "$kind/$name is $got, want $want: $(k get "$kind.$G" "$name" -n "$NS" -o jsonpath='{.status.message}')" ;;
    esac
    [ $SECONDS -ge $end ] && fail "$kind/$name still '$got' after ${timeout}s, want $want"
    sleep 2
  done
}
field() { k get "$1.$G" "$2" -n "$NS" -o jsonpath="{$3}"; }
psql() { k exec -n "$NS" "deploy/${PGDEPLOY:-postgres}" -- psql -U postgres -tAq "$@"; }
# s3sh SCRIPT: runs a shell script in an rclone pod configured for the test S3 endpoint as remote "s". A
# short-lived pod sometimes fails to attach, so it is retried; it never reads stdin, which keeps retries safe.
s3sh() {
  local out try
  for try in 1 2 3; do
    if out=$(k run -n "$NS" "s3c-$RANDOM$RANDOM" --rm -i --restart=Never --quiet --image="${P}rclone/rclone:1.75" \
      --env=RCLONE_CONFIG_S_TYPE=s3 --env=RCLONE_CONFIG_S_PROVIDER=Other --env=RCLONE_CONFIG_S_ENDPOINT=http://s3:9000 \
      --env=RCLONE_CONFIG_S_ACCESS_KEY_ID=demo-access --env=RCLONE_CONFIG_S_SECRET_ACCESS_KEY=demo-secret-key \
      --env=RCLONE_LOG_LEVEL=ERROR --command -- sh -c "$1" </dev/null 2>/dev/null); then
      [ -n "$out" ] && printf '%s\n' "$out"
      return 0
    fi
    sleep 2
  done
  return 1
}
# s3 ARGS...: one rclone command, e.g. s3 cat s:bucket/key.
s3() { s3sh "rclone $*"; }
# s3_put CONTENT KEY: writes a small object.
s3_put() { s3sh "echo '$1' | rclone rcat '$2'"; }
# count_objects PATH: number of objects, or "error" when listing fails (so a failure never looks like 0).
count_objects() { local out; out=$(s3 lsf -R --files-only "$1") || { echo error; return; }; printf "%s" "$out" | grep -c . || true; }
hooks() { k logs -n "$NS" deploy/hook 2>/dev/null; }

echo "==> infrastructure in namespace $NS"
k get crd "backups.$G" >/dev/null || fail "planb CRDs are not installed"
k delete ns "$NS" --ignore-not-found --wait=true >/dev/null
sed -e "s/namespace: planb-demo/namespace: $NS/; s/name: planb-demo/name: $NS/; s/s3\.planb-demo\.svc/s3.$NS.svc/" \
  -e "s#image: #image: $P#" -e '/^kind: BackupStorageLocation/,$d' hack/demo/infra.yaml | sed '$d' | k apply -f - >/dev/null
k apply -n "$NS" -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: { name: postgres2 }
spec:
  selector: { matchLabels: { app: postgres2 } }
  template:
    metadata: { labels: { app: postgres2 } }
    spec:
      containers:
        - name: postgres
          image: ${P}postgres:18-alpine
          env:
            - { name: POSTGRES_PASSWORD, value: demo-password }
          readinessProbe: { exec: { command: [pg_isready, -U, postgres] }, periodSeconds: 2 }
          volumeMounts: [{ name: data, mountPath: /var/lib/postgresql }]
      volumes: [{ name: data, emptyDir: {} }]
---
apiVersion: v1
kind: Service
metadata: { name: postgres2 }
spec: { selector: { app: postgres2 }, ports: [{ port: 5432 }] }
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: hook }
spec:
  selector: { matchLabels: { app: hook } }
  template:
    metadata: { labels: { app: hook } }
    spec:
      containers:
        - name: hook
          image: ${P}node:22-alpine
          command: [node, -e]
          args:
            - |
              require("http").createServer((req, res) => {
                let body = "";
                req.on("data", (c) => (body += c));
                req.on("end", () => { console.log("HOOK " + req.headers["x-test"] + " " + body); res.end("ok"); });
              }).listen(8080);
          readinessProbe: { tcpSocket: { port: 8080 }, periodSeconds: 2 }
---
apiVersion: v1
kind: Service
metadata: { name: hook }
spec: { selector: { app: hook }, ports: [{ port: 8080 }] }
EOF
k wait -n "$NS" --for=condition=Available deploy --all --timeout=180s >/dev/null || fail "test infrastructure did not start"
wait_for "seeded database" 60 'test "$(psql -d app -c "SELECT count(*) FROM customers" 2>/dev/null)" = 1000'
ok "postgres, postgres2, s3 and webhook receiver are up"

echo "==> storage locations"
k apply -n "$NS" -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: BackupStorageLocation
metadata: { name: default }
spec:
  s3: { endpoint: s3.$NS.svc:9000, bucket: backups, prefix: planb, insecure: true, credentials: { name: s3-credentials } }
  syncPeriod: 20s
---
apiVersion: $G/v1alpha1
kind: BackupStorageLocation
metadata: { name: readonly }
spec:
  s3: { endpoint: s3.$NS.svc:9000, bucket: backups, prefix: other, insecure: true, credentials: { name: s3-credentials } }
  accessMode: ReadOnly
---
apiVersion: $G/v1alpha1
kind: BackupStorageLocation
metadata: { name: broken }
spec:
  s3: { endpoint: s3.$NS.svc:9000, bucket: no-such-bucket, insecure: true, credentials: { name: s3-credentials } }
EOF
wait_for "location default Available" 90 'test "$(phase backupstoragelocation default)" = Available'
ok "location 'default' is Available"
wait_for "location broken Unavailable" 90 'test "$(phase backupstoragelocation broken)" = Unavailable'
ok "location with a missing bucket is Unavailable: $(field backupstoragelocation broken .status.message)"

echo "==> notifier (webhook, all events)"
k apply -n "$NS" -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: Notifier
metadata: { name: hook }
spec:
  events: [BackupCompleted, BackupFailed, RestoreCompleted, RestoreFailed]
  webhook: { url: "http://hook.$NS.svc:8080/", headers: { X-Test: e2e } }
EOF
k annotate -n "$NS" "notifier.$G/hook" "$G/test=t1" --overwrite >/dev/null
wait_for "notifier test" 60 'test "$(field notifier hook .status.lastTestRequest)" = t1'
wait_for "test message received" 30 'hooks | grep -q "HOOK e2e"'
[ "$(phase notifier hook)" = Ready ] || fail "notifier is $(phase notifier hook)"
ok "test message delivered with custom header, notifier Ready"

echo "==> postgres backups"
k apply -n "$NS" -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: Backup
metadata: { name: pg-1 }
spec:
  storageLocation: default
  postgres:
    host: postgres.$NS.svc
    sslMode: disable
    credentials: { name: postgres-credentials }
    databases: [app]
    includeGlobals: true
  job:
    retry: { attempts: 7, initialBackoff: 1s, maxBackoff: 20s }
---
apiVersion: $G/v1alpha1
kind: Backup
metadata: { name: pg-all }
spec:
  storageLocation: default
  postgres:
    host: postgres.$NS.svc
    sslMode: disable
    credentials: { name: postgres-credentials }
EOF
wait_phase backup pg-1 Completed
wait_phase backup pg-all Completed
[ "$(field backup pg-1 .status.items)" = 2 ] || fail "pg-1 items = $(field backup pg-1 .status.items)"
files=$(s3 lsf -R --files-only s:backups/planb/backups/pg-1/ | sort | tr '\n' ' ')
[ "$files" = "planb-backup.json postgres/app.dump postgres/globals.sql.gz " ] || fail "pg-1 files: $files"
ok "per-database backup: app.dump + globals.sql.gz + metadata"
s3 lsf -R s:backups/planb/backups/pg-all/ | grep -q postgres/all.sql.gz || fail "pg-all has no all.sql.gz"
ok "full-server backup: all.sql.gz"
task=$(k get job -n "$NS" "$(field backup pg-1 .status.jobName)" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="PLANB_TASK")].value}')
echo "$task" | grep -q '"retry":{"attempts":7,"initialBackoff":1000000000,"maxBackoff":20000000000}' || fail "job.retry not passed to the agent: $task"
ok "spec.job.retry reaches the agent"
[ -n "$(field backup pg-1 .status.expiration)" ] || fail "no expiration set"
ok "default TTL sets status.expiration=$(field backup pg-1 .status.expiration)"

echo "==> bucket backup"
k apply -n "$NS" -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: Backup
metadata: { name: media-1 }
spec:
  storageLocation: default
  bucket: { endpoint: s3.$NS.svc:9000, bucket: media, prefix: uploads/, insecure: true, credentials: { name: s3-credentials } }
EOF
wait_phase backup media-1 Completed
[ "$(field backup media-1 .status.items)" = 6 ] || fail "media-1 items = $(field backup media-1 .status.items)"
wait_for "expected 6 objects in the backup" 30 'test "$(count_objects s:backups/planb/backups/media-1/bucket/)" = 6'
ok "6 objects copied ($(field backup media-1 .status.sizeBytes) bytes)"

echo "==> postgres restores"
k apply -n "$NS" -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: Restore
metadata: { name: pg-mapped }
spec:
  backupName: pg-1
  postgres: { databaseMapping: { app: app_restored }, restoreGlobals: true }
EOF
wait_phase restore pg-mapped Completed
[ "$(psql -d app_restored -c 'SELECT count(*) FROM customers')" = 1000 ] || fail "app_restored row count"
ok "restore under a new name (createDatabase + databaseMapping + restoreGlobals): 1000 rows"
psql -d app_restored -c "DELETE FROM customers WHERE id > 10"
k apply -n "$NS" -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: Restore
metadata: { name: pg-clean }
spec:
  backupName: pg-1
  postgres: { databaseMapping: { app: app_restored }, clean: true }
EOF
wait_phase restore pg-clean Completed
[ "$(psql -d app_restored -c 'SELECT count(*) FROM customers')" = 1000 ] || fail "row count after clean restore"
ok "clean restore over modified data: back to 1000 rows"
k apply -n "$NS" -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: Restore
metadata: { name: pg-all-other-server }
spec:
  backupName: pg-all
  postgres:
    target: { host: postgres2.$NS.svc, sslMode: disable, credentials: { name: postgres-credentials } }
EOF
wait_phase restore pg-all-other-server Completed
[ "$(PGDEPLOY=postgres2 psql -d app -c 'SELECT count(*) FROM customers')" = 1000 ] || fail "row count on postgres2"
ok "full-server backup replayed into another, empty server"
k apply -n "$NS" -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: Restore
metadata: { name: pg-missing-db }
spec:
  backupName: pg-1
  postgres: { databases: [nope] }
EOF
wait_phase restore pg-missing-db Failed
ok "restore of a database that is not in the backup fails: $(field restore pg-missing-db .status.message | tail -c 60)"
k apply -n "$NS" -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: Restore
metadata: { name: wrong-kind }
spec:
  backupName: media-1
  postgres: { databases: [app] }
EOF
wait_phase restore wrong-kind FailedValidation
ok "postgres options on a bucket backup are rejected (FailedValidation)"

echo "==> bucket restores"
k apply -n "$NS" -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: Restore
metadata: { name: media-copy }
spec:
  backupName: media-1
  bucket:
    target: { endpoint: s3.$NS.svc:9000, bucket: media, prefix: restored/, insecure: true, credentials: { name: s3-credentials } }
EOF
wait_phase restore media-copy Completed
wait_for "expected 6 restored objects" 30 'test "$(count_objects s:media/restored/)" = 6'
ok "restore into another prefix: 6 objects"
s3_put changed s:media/restored/2026/file-1.txt
s3_put stale s:media/restored/stale/old.txt
s3_put outside s:media/outside.txt
k apply -n "$NS" -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: Restore
metadata: { name: media-keep }
spec:
  backupName: media-1
  bucket:
    overwrite: false
    target: { endpoint: s3.$NS.svc:9000, bucket: media, prefix: restored/, insecure: true, credentials: { name: s3-credentials } }
EOF
wait_phase restore media-keep Completed
[ "$(s3 cat s:media/restored/2026/file-1.txt)" = changed ] || fail "overwrite=false replaced an object"
ok "overwrite: false keeps existing objects"
k apply -n "$NS" -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: Restore
metadata: { name: media-prune }
spec:
  backupName: media-1
  bucket:
    prune: true
    target: { endpoint: s3.$NS.svc:9000, bucket: media, prefix: restored/, insecure: true, credentials: { name: s3-credentials } }
EOF
wait_phase restore media-prune Completed
[ "$(field restore media-prune .status.deletedItems)" = 1 ] || fail "deletedItems = $(field restore media-prune .status.deletedItems)"
wait_for "expected exactly 6 objects after prune" 30 'test "$(count_objects s:media/restored/)" = 6'
[ "$(s3 cat s:media/restored/2026/file-1.txt)" = "file 1" ] || fail "prune restore did not overwrite"
[ "$(s3 cat s:media/outside.txt)" = outside ] || fail "prune touched an object outside the target prefix"
ok "prune deletes the stale object only (deletedItems=1): $(field restore media-prune .status.message)"
k apply -n "$NS" -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: Restore
metadata: { name: media-into-storage }
spec:
  backupName: media-1
  bucket:
    prune: true
    target: { endpoint: s3.$NS.svc:9000, bucket: backups, prefix: "", insecure: true, credentials: { name: s3-credentials } }
EOF
wait_phase restore media-into-storage Completed
wait_for "prune into the storage bucket deleted planb's backups" 30 'test "$(count_objects s:backups/planb/backups/pg-1/)" = 3'
ok "prune into the storage bucket keeps planb's own backups"

echo "==> retries through an S3 outage"
k apply -n "$NS" -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: Backup
metadata: { name: outage-1 }
spec:
  storageLocation: default
  bucket: { endpoint: s3.$NS.svc:9000, bucket: media, prefix: uploads/, insecure: true, credentials: { name: s3-credentials } }
  job:
    retry: { attempts: 10, initialBackoff: 2s, maxBackoff: 10s }
EOF
wait_for "outage-1 InProgress" 60 'test "$(phase backup outage-1)" = InProgress'
k patch svc s3 -n "$NS" -p '{"spec":{"selector":{"app":"nowhere"}}}' >/dev/null
sleep 25
k patch svc s3 -n "$NS" -p '{"spec":{"selector":{"app":"s3"}}}' >/dev/null
wait_phase backup outage-1 Completed 240
retries=$(k logs -n "$NS" "job/$(field backup outage-1 .status.jobName)" 2>/dev/null | grep -c "retrying after error" || true)
[ "$retries" -gt 0 ] || fail "expected retries in the agent log"
[ "$(field backup outage-1 .status.items)" = 6 ] || fail "outage-1 items"
ok "S3 unreachable for 25s mid-backup: $retries retries, then Completed with 6 objects"

echo "==> read-only location"
k apply -n "$NS" -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: Backup
metadata: { name: to-readonly }
spec:
  storageLocation: readonly
  postgres: { host: postgres.$NS.svc, sslMode: disable, credentials: { name: postgres-credentials }, databases: [app] }
EOF
wait_for "backup to read-only location rejected" 60 'test "$(phase backup to-readonly)" = Failed'
ok "backup to a ReadOnly location fails: $(field backup to-readonly .status.message)"

echo "==> deletion, retain-data and sync from storage"
k delete "backup.$G" media-1 -n "$NS" --wait=true >/dev/null
wait_for "media-1 data deleted" 60 'test "$(count_objects s:backups/planb/backups/media-1/)" = 0'
ok "deleting a Backup deletes its data"
k annotate "backup.$G" pg-all -n "$NS" "$G/retain-data=true" >/dev/null
k delete "backup.$G" pg-all -n "$NS" --wait=true >/dev/null
wait_for "retain-data did not keep the data" 30 'test "$(count_objects s:backups/planb/backups/pg-all/)" = 2'
ok "retain-data keeps the data"
wait_for "pg-all re-imported by sync" 90 'test "$(phase backup pg-all)" = Completed'
[ "$(k get "backup.$G" pg-all -n "$NS" -o jsonpath="{.metadata.annotations.planb\.sina-devel\.github\.io/synced-from-storage}")" != "" ] || fail "synced backup not annotated"
ok "sync from storage re-imports the backup"
k apply -n "$NS" -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: Restore
metadata: { name: from-synced }
spec:
  backupName: pg-all
  postgres:
    target: { host: postgres2.$NS.svc, sslMode: disable, credentials: { name: postgres-credentials } }
EOF
wait_phase restore from-synced Completed
ok "restore from a synced backup works"

echo "==> TTL"
k apply -n "$NS" -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: Backup
metadata: { name: short-lived }
spec:
  storageLocation: default
  ttl: 15s
  postgres: { host: postgres.$NS.svc, sslMode: disable, credentials: { name: postgres-credentials }, databases: [app] }
EOF
wait_phase backup short-lived Completed
wait_for "short-lived expired" 90 '! k get "backup.$G" short-lived -n "$NS"'
wait_for "expired backup's data was kept" 30 'test "$(count_objects s:backups/planb/backups/short-lived/)" = 0'
ok "expired backup and its data are deleted"

echo "==> schedule with keepLast (takes about 3 minutes)"
k apply -n "$NS" -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: Schedule
metadata: { name: every-minute }
spec:
  schedule: "* * * * *"
  keepLast: 2
  template:
    storageLocation: default
    postgres: { host: postgres.$NS.svc, sslMode: disable, credentials: { name: postgres-credentials }, databases: [app] }
EOF
scheduled() { k get "backups.$G" -n "$NS" -l "$G/schedule=every-minute" -o name | grep -c . || true; }
seen=""
end=$((SECONDS + 240))
while [ $SECONDS -lt $end ]; do
  seen=$(printf '%s\n%s\n' "$seen" "$(k get "backups.$G" -n "$NS" -l "$G/schedule=every-minute" -o name)" | sort -u | grep . || true)
  [ "$(echo "$seen" | grep -c .)" -ge 3 ] && break
  sleep 5
done
[ "$(echo "$seen" | grep -c .)" -ge 3 ] || fail "schedule created $(echo "$seen" | grep -c .) backups in 4 minutes"
wait_for "keepLast pruning" 90 'test "$(scheduled)" -le 2'
ok "schedule created $(echo "$seen" | grep -c .) backups and keeps the last 2"
k patch "schedule.$G" every-minute -n "$NS" --type=merge -p '{"spec":{"paused":true}}' >/dev/null
wait_for "schedule paused" 30 'test "$(phase schedule every-minute)" = Paused'
ok "schedule can be paused"

echo "==> notifications"
for ev in BackupCompleted RestoreCompleted RestoreFailed; do
  wait_for "$ev notification" 60 'hooks | grep -q "\"event\":\"$ev\""'
done
hooks | grep '"name":"pg-missing-db"' | grep -q RestoreFailed || fail "no RestoreFailed for pg-missing-db"
[ -n "$(k get "restore.$G" pg-mapped -n "$NS" -o jsonpath="{.metadata.annotations.planb\.sina-devel\.github\.io/notified}")" ] || fail "notified annotation missing"
ok "webhook received BackupCompleted, RestoreCompleted and RestoreFailed ($(hooks | grep -c '"event"') messages)"

echo "==> UI API"
if user=$(k get secret -n "$PLANB_NS" planb-ui-auth -o jsonpath='{.data.username}' 2>/dev/null | base64 -d) && [ -n "$user" ]; then
  pass=$(k get secret -n "$PLANB_NS" planb-ui-auth -o jsonpath='{.data.password}' | base64 -d)
  k port-forward -n "$PLANB_NS" svc/planb-ui 18090:80 >/dev/null 2>&1 &
  PF_PID=$!
  wait_for "UI port-forward" 30 'curl -sf -o /dev/null http://127.0.0.1:18090/healthz'
  [ "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18090/api/overview)" = 401 ] || fail "UI API without auth is not 401"
  ok "UI API requires auth"
  curl -sf -u "$user:$pass" http://127.0.0.1:18090/api/overview | grep -q '"pg-1"' || fail "UI overview misses pg-1"
  ok "UI overview lists the backups"
  curl -sf -u "$user:$pass" -H 'Content-Type: application/json' -X POST http://127.0.0.1:18090/api/restores \
    -d '{"namespace":"'"$NS"'","name":"ui-restore","spec":{"backupName":"pg-1","postgres":{"databaseMapping":{"app":"app_ui"}}}}' >/dev/null ||
    fail "UI could not create a restore"
  wait_phase restore ui-restore Completed
  [ "$(psql -d app_ui -c 'SELECT count(*) FROM customers')" = 1000 ] || fail "UI restore row count"
  ok "restore created through the UI API"
  curl -sf -u "$user:$pass" -X POST "http://127.0.0.1:18090/api/notifiers/$NS/hook/test" >/dev/null || fail "UI notifier test"
  wait_for "UI test message" 60 'test "$(hooks | grep -c "\"event\":\"Test\"")" -ge 2'
  ok "notifier test from the UI API"
else
  echo "    skipped: UI with basic auth is not enabled"
fi

echo "==> all $PASSED cluster checks passed"
