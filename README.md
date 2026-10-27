# planb

A Kubernetes operator for backup and restore, inspired by [Velero](https://velero.io). Velero backs up
Kubernetes resources and volumes. planb backs up **data services**:

- **PostgreSQL**: `pg_dump` (custom format) per database, or the whole server with `pg_dumpall`
- **S3 buckets**: every object of any S3-compatible bucket, optionally limited to a prefix

Backups go to any S3-compatible object store (AWS S3, MinIO, Ceph RGW, ...). There is also an optional web UI.

## Concepts

| Kind | Velero equivalent | Purpose |
|---|---|---|
| `BackupStorageLocation` (`bsl`) | BackupStorageLocation | Bucket where backups are written; validated periodically |
| `Backup` | Backup | One-off backup of a PostgreSQL server *or* a bucket |
| `Schedule` | Schedule | Cron that creates `Backup`s from a template; supports `keepLast` |
| `Restore` | Restore | Restores a `Backup` to its original source or a different target |

All resources are namespaced. A Backup refers to a storage location, and all credential Secrets live in the
same namespace.

### How it works

```
Backup CR ──> operator ──> Job (planb agent) ──stream──> s3://bucket/<prefix>/backups/<name>/
                 │                                        ├── postgres/app.dump
                 │ <── result via termination message     ├── postgres/globals.sql.gz
                 └── writes planb-backup.json ──────────> └── planb-backup.json
```

- Each backup or restore runs as a **Kubernetes Job** in the resource's namespace. The Job uses the same image in
  `agent` mode, and that image contains the PostgreSQL client tools.
- Data is **streamed**: `pg_dump | upload`, or `download | pg_restore`. Nothing is written to local disk.
- Credentials reach the Job only through `secretKeyRef` environment variables. They never appear in a CR or in
  Job args.
- The agent reports its size and item count through the container termination message. The operator then writes
  `planb-backup.json` next to the data.

### Velero-style behaviour

- **TTL**: `spec.ttl` (default `720h`, set by `--default-backup-ttl`). When it expires, the Backup *and its data* are
  deleted.
- **Deleting a Backup deletes its data.** A finalizer handles this. Data is **kept** when:
  - the Backup has the annotation `planb.io/retain-data: "true"`
  - the namespace is being deleted (protects against an accidental `kubectl delete ns`)
  - the storage location is `ReadOnly`, missing, or being deleted
- **Sync from storage**: every `syncPeriod`, each storage location imports completed backups it finds in the
  bucket that are not in the cluster yet. To restore in a new cluster (DR):
  1. Install planb.
  2. Create the same `BackupStorageLocation`, preferably with `accessMode: ReadOnly`.
  3. Wait for the backups to show up.
  4. Create a `Restore`.
- **ReadOnly locations** accept restores only: no new backups, no TTL garbage collection, no data deletion.

## Install

Images for `linux/amd64` and `linux/arm64` are published to `ghcr.io/sina-devel/planb` by GitHub Actions
(`.github/workflows/image.yaml`): `main` for the main branch, `sha-<commit>` for each commit, and `X.Y.Z` / `X.Y` for
`vX.Y.Z` tags.

```bash
helm upgrade --install planb charts/planb -n planb-system --create-namespace \
  --set image.tag=main \
  --set ui.enabled=true --set ui.auth.username=admin --set ui.auth.password='change-me'
```

To build your own image: `make docker-build docker-push IMG=registry.example.com/planb:dev`.

CRDs are under `charts/planb/crds/` and Helm installs them. Regenerate them with `make generate` after changing
`api/`.

## Usage

See [`examples/`](examples/):

```bash
kubectl apply -f examples/01-storage-location.yaml
kubectl apply -f examples/02-postgres-backup.yaml
kubectl get backups -n db -w
kubectl apply -f examples/05-restore.yaml
kubectl get restores -n db
kubectl logs -n db job/planb-backup-app-manual-1     # agent logs
```

### PostgreSQL

| Backup option | Effect |
|---|---|
| `databases: [a, b]` | One `pg_dump --format=custom` file per database |
| *(no databases)* | `pg_dumpall` of the whole server as gzipped SQL |
| `includeGlobals: true` | Also dumps roles and tablespaces (`pg_dumpall --globals-only`) |
| `extraArgs` | Appended to `pg_dump` / `pg_dumpall` |

| Restore option | Effect |
|---|---|
| `target` | Restore to another server (default: the backup's source) |
| `databases` | Restore only some of the databases |
| `databaseMapping: {app: app_copy}` | Restore under a different name |
| `createDatabase` (default `true`) | Create the target database if it is missing |
| `clean` | `pg_restore --clean --if-exists`: drop objects before recreating them |
| `restoreGlobals` | Replay roles and tablespaces first |

> A full-server (`pg_dumpall`) backup is replayed with `psql` as-is. Restore it into an **empty** server. For
> selective or in-place restores, back up specific `databases` instead.
>
> The image ships the PostgreSQL 18 client, which can dump every server version from 9.2 on. To use another client
> version, set `spec.job.image` (or `manager.agentImage` in the chart).

### Buckets

A bucket backup copies objects under `spec.bucket.prefix` into `<backup>/bucket/`, keeping relative keys. A restore
copies them back to the original bucket and prefix, or to `spec.bucket.target`. With `overwrite: false`, existing
keys are skipped.

### Job tuning

`spec.job` on a Backup, a Schedule template, or a Restore accepts `resources`, `nodeSelector`, `tolerations`,
`serviceAccountName`, `activeDeadlineSeconds`, `backoffLimit` and `image`. Set `PLANB_PART_SIZE_MB` in the image
environment to raise the 64 MiB multipart chunk size. The largest streamed object is 10,000 × part size, so about
640 GiB by default.

## Web UI (optional)

Enable it with `--set ui.enabled=true`. Locally, run `make run-ui` and open http://localhost:8090.

The UI lists storage locations, backups, schedules and restores, and refreshes every 5 seconds. From the UI you can:

- start a backup with **Run now** on a schedule, or create one from a JSON spec
- **Restore** a backup, with target, database mapping and clean options
- view Job logs
- pause or resume schedules
- delete backups and restores

Settings:

- `ui.readOnly`: hides mutations; the UI's RBAC becomes read-only too
- `ui.namespace`: limits the UI to one namespace
- `ui.auth.*`: basic auth. **Set it before exposing the UI through `ui.ingress`.**

The UI is a single embedded HTML file (`internal/ui/static/index.html`) with no build step. Its JSON API is under
`/api/*`.

## Development

```bash
nix develop            # or nix-shell / direnv: go, controller-gen, helm, kind, postgresql, rclone
make tidy generate     # resolve deps, generate deepcopy + CRDs (needs network)
make test              # unit tests
make e2e               # agent end-to-end: throwaway PostgreSQL 18 + `rclone serve s3` (no docker)
make install run       # CRDs into current cluster, run the operator locally
```

Layout:

```
api/v1alpha1/        CRD types
cmd/planb/           single binary: manager | agent | ui
internal/controller/ reconcilers (backup, restore, schedule, storage location)
internal/agent/      data movers run inside Jobs (pg_dump/pg_restore, bucket copy)
internal/storage/    S3 client (minio-go)
internal/task/       operator <-> agent contract
internal/ui/         UI server + embedded page
charts/planb/        Helm chart (CRDs in crds/)
```

## Roadmap

- Client-side encryption of backups (age/AES) and checksums in metadata
- More databases (MySQL/MariaDB, MongoDB, Redis)
- Point-in-time recovery for PostgreSQL (WAL archiving)
- Prometheus metrics for backup age, size and failures
