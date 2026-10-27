package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PostgresBackupSource backs up databases from a PostgreSQL server.
type PostgresBackupSource struct {
	PostgresConnection `json:",inline"`
	// Databases to dump with pg_dump (custom format). When empty, the whole
	// cluster is dumped with pg_dumpall (plain SQL, gzipped).
	// +optional
	Databases []string `json:"databases,omitempty"`
	// IncludeGlobals also dumps roles and tablespaces (pg_dumpall --globals-only)
	// when specific databases are listed.
	// +optional
	IncludeGlobals bool `json:"includeGlobals,omitempty"`
	// ExtraArgs appended to the pg_dump / pg_dumpall command line.
	// +optional
	ExtraArgs []string `json:"extraArgs,omitempty"`
}

// BucketBackupSource copies all objects of a bucket (optionally under a prefix).
type BucketBackupSource struct {
	S3Spec `json:",inline"`
}

// BackupSpec defines the desired state of Backup.
// +kubebuilder:validation:XValidation:rule="has(self.postgres) != has(self.bucket)",message="exactly one of postgres or bucket must be set"
type BackupSpec struct {
	// StorageLocation is the name of a BackupStorageLocation in the same namespace.
	// +kubebuilder:validation:MinLength=1
	StorageLocation string `json:"storageLocation"`
	// TTL after which the backup and its data are deleted. Defaults to 720h (30 days).
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`
	// +optional
	Postgres *PostgresBackupSource `json:"postgres,omitempty"`
	// +optional
	Bucket *BucketBackupSource `json:"bucket,omitempty"`
	// +optional
	Job *JobOptions `json:"job,omitempty"`
}

// BackupPhase is the lifecycle phase of a Backup.
type BackupPhase string

const (
	BackupPhaseNew        BackupPhase = "New"
	BackupPhaseInProgress BackupPhase = "InProgress"
	BackupPhaseCompleted  BackupPhase = "Completed"
	BackupPhaseFailed     BackupPhase = "Failed"
	BackupPhaseDeleting   BackupPhase = "Deleting"
)

// BackupStatus defines the observed state of Backup.
type BackupStatus struct {
	// +optional
	Phase BackupPhase `json:"phase,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// Location of the backup data, e.g. s3://bucket/prefix/backups/name/
	// +optional
	Location string `json:"location,omitempty"`
	// +optional
	StartTimestamp *metav1.Time `json:"startTimestamp,omitempty"`
	// +optional
	CompletionTimestamp *metav1.Time `json:"completionTimestamp,omitempty"`
	// +optional
	Expiration *metav1.Time `json:"expiration,omitempty"`
	// SizeBytes is the total size of the uploaded data.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// Items is the number of dump files (postgres) or objects (bucket) stored.
	// +optional
	Items int64 `json:"items,omitempty"`
	// Files stored for this backup, relative to Location.
	// +optional
	Files []string `json:"files,omitempty"`
	// JobName of the Job running the backup.
	// +optional
	JobName string `json:"jobName,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Location",type=string,JSONPath=`.spec.storageLocation`
// +kubebuilder:printcolumn:name="Size",type=integer,JSONPath=`.status.sizeBytes`
// +kubebuilder:printcolumn:name="Completed",type=date,JSONPath=`.status.completionTimestamp`
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.status.expiration`

// Backup is a one-off backup of a PostgreSQL server or a bucket.
type Backup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BackupSpec   `json:"spec,omitempty"`
	Status BackupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BackupList contains a list of Backup.
type BackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Backup `json:"items"`
}

func init() {
	register(&Backup{}, &BackupList{})
}

// IsFinished reports whether the backup reached a terminal phase.
func (b *Backup) IsFinished() bool {
	return b.Status.Phase == BackupPhaseCompleted || b.Status.Phase == BackupPhaseFailed
}

// Kind returns "postgres" or "bucket".
func (s *BackupSpec) Kind() string {
	if s.Postgres != nil {
		return "postgres"
	}
	return "bucket"
}
