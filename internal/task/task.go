// Package task defines the contract between the operator and the agent that runs inside backup/restore Jobs.
//
// The operator serialises a Task as JSON into the PLANB_TASK environment variable. Secrets are never part of
// the Task: they are injected as separate environment variables via secretKeyRef. The agent reports a Result
// as JSON in the container termination message.
package task

import (
	"encoding/json"
	"path"

	"github.com/sina-devel/planb/internal/storage"
)

const (
	EnvTask            = "PLANB_TASK"
	EnvStorageAccess   = "PLANB_STORAGE_ACCESS_KEY"
	EnvStorageSecret   = "PLANB_STORAGE_SECRET_KEY"
	EnvBucketAccess    = "PLANB_BUCKET_ACCESS_KEY"
	EnvBucketSecret    = "PLANB_BUCKET_SECRET_KEY"
	EnvPGUser          = "PGUSER"
	EnvPGPassword      = "PGPASSWORD"
	TerminationLogPath = "/dev/termination-log"

	// MetadataFile is stored next to the data of each completed backup.
	MetadataFile = "planb-backup.json"
	// BackupsDir is the directory below the storage location prefix holding all backups.
	BackupsDir = "backups"

	PostgresDir    = "postgres"
	BucketDir      = "bucket"
	ClusterDump    = "all.sql.gz"
	GlobalsDump    = "globals.sql.gz"
	DatabaseSuffix = ".dump"
)

type Op string

const (
	OpBackup  Op = "backup"
	OpRestore Op = "restore"
)

// Task is what the agent executes.
type Task struct {
	Op   Op     `json:"op"`
	Name string `json:"name"`
	// Storage is the backup storage location. Credentials come from EnvStorageAccess/EnvStorageSecret.
	Storage storage.Config `json:"storage"`
	// BackupKey is the full key prefix (ending in "/") of the backup inside the storage bucket.
	BackupKey string    `json:"backupKey"`
	Postgres  *Postgres `json:"postgres,omitempty"`
	Bucket    *Bucket   `json:"bucket,omitempty"`
}

// Postgres task options. Credentials come from PGUSER/PGPASSWORD.
type Postgres struct {
	Host      string   `json:"host"`
	Port      int32    `json:"port"`
	SSLMode   string   `json:"sslMode,omitempty"`
	Databases []string `json:"databases,omitempty"`
	ExtraArgs []string `json:"extraArgs,omitempty"`

	// backup only
	IncludeGlobals bool `json:"includeGlobals,omitempty"`

	// restore only
	DatabaseMapping map[string]string `json:"databaseMapping,omitempty"`
	Clean           bool              `json:"clean,omitempty"`
	CreateDatabase  bool              `json:"createDatabase,omitempty"`
	RestoreGlobals  bool              `json:"restoreGlobals,omitempty"`
}

// Bucket task options. For a backup S3 is the source; for a restore it is the target.
// Credentials come from EnvBucketAccess/EnvBucketSecret.
type Bucket struct {
	S3          storage.Config `json:"s3"`
	Overwrite   bool           `json:"overwrite,omitempty"`
	Concurrency int            `json:"concurrency,omitempty"`
}

// Result is reported by the agent through the termination message.
type Result struct {
	SizeBytes int64    `json:"sizeBytes"`
	Items     int64    `json:"items"`
	Files     []string `json:"files,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// BackupKey returns the key prefix of a backup within a storage location prefix.
func BackupKey(storagePrefix, backupName string) string {
	return storage.NormalizePrefix(path.Join(storage.NormalizePrefix(storagePrefix), BackupsDir, backupName))
}

// BackupsKey returns the key prefix holding all backups of a storage location.
func BackupsKey(storagePrefix string) string {
	return storage.NormalizePrefix(path.Join(storage.NormalizePrefix(storagePrefix), BackupsDir))
}

// Encode serialises the task.
func (t *Task) Encode() (string, error) {
	b, err := json.Marshal(t)
	return string(b), err
}

// Decode parses a task.
func Decode(s string) (*Task, error) {
	t := &Task{}
	return t, json.Unmarshal([]byte(s), t)
}
