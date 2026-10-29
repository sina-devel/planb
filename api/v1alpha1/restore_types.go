package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PostgresRestoreOptions control a PostgreSQL restore.
type PostgresRestoreOptions struct {
	// Target server. Defaults to the server the backup was taken from.
	// +optional
	Target *PostgresConnection `json:"target,omitempty"`
	// Databases to restore (default: all databases in the backup).
	// +optional
	Databases []string `json:"databases,omitempty"`
	// DatabaseMapping restores a database under a different name: {source: target}.
	// +optional
	DatabaseMapping map[string]string `json:"databaseMapping,omitempty"`
	// Clean drops existing objects before recreating them (pg_restore --clean --if-exists).
	// +optional
	Clean bool `json:"clean,omitempty"`
	// CreateDatabase creates the target database if it does not exist.
	// +kubebuilder:default=true
	// +optional
	CreateDatabase *bool `json:"createDatabase,omitempty"`
	// RestoreGlobals also replays roles/tablespaces when the backup contains them.
	// +optional
	RestoreGlobals bool `json:"restoreGlobals,omitempty"`
	// ExtraArgs appended to pg_restore / psql.
	// +optional
	ExtraArgs []string `json:"extraArgs,omitempty"`
}

// BucketRestoreOptions control a bucket restore.
type BucketRestoreOptions struct {
	// Target bucket. Defaults to the bucket (and prefix) the backup was taken from.
	// +optional
	Target *S3Spec `json:"target,omitempty"`
	// Overwrite existing objects in the target. When false, existing keys are skipped.
	// +kubebuilder:default=true
	// +optional
	Overwrite *bool `json:"overwrite,omitempty"`
	// Prune deletes objects under the target prefix that are not in the backup, so the target matches the
	// backup exactly. It runs only after every object was copied. Backups of planb itself are never deleted,
	// even when the target is the storage location's bucket.
	// +optional
	Prune bool `json:"prune,omitempty"`
}

// RestoreSpec defines the desired state of Restore.
type RestoreSpec struct {
	// BackupName is the Backup (in the same namespace) to restore from.
	// +kubebuilder:validation:MinLength=1
	BackupName string `json:"backupName"`
	// +optional
	Postgres *PostgresRestoreOptions `json:"postgres,omitempty"`
	// +optional
	Bucket *BucketRestoreOptions `json:"bucket,omitempty"`
	// +optional
	Job *JobOptions `json:"job,omitempty"`
}

type RestorePhase string

const (
	RestorePhaseNew              RestorePhase = "New"
	RestorePhaseInProgress       RestorePhase = "InProgress"
	RestorePhaseCompleted        RestorePhase = "Completed"
	RestorePhaseFailed           RestorePhase = "Failed"
	RestorePhaseFailedValidation RestorePhase = "FailedValidation"
)

// RestoreStatus defines the observed state of Restore.
type RestoreStatus struct {
	// +optional
	Phase RestorePhase `json:"phase,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	StartTimestamp *metav1.Time `json:"startTimestamp,omitempty"`
	// +optional
	CompletionTimestamp *metav1.Time `json:"completionTimestamp,omitempty"`
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// +optional
	Items int64 `json:"items,omitempty"`
	// DeletedItems is the number of objects removed by spec.bucket.prune.
	// +optional
	DeletedItems int64 `json:"deletedItems,omitempty"`
	// +optional
	JobName string `json:"jobName,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Backup",type=string,JSONPath=`.spec.backupName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Completed",type=date,JSONPath=`.status.completionTimestamp`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Restore restores a Backup into a PostgreSQL server or a bucket.
type Restore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RestoreSpec   `json:"spec,omitempty"`
	Status RestoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RestoreList contains a list of Restore.
type RestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Restore `json:"items"`
}

func init() {
	register(&Restore{}, &RestoreList{})
}

// IsFinished reports whether the restore reached a terminal phase.
func (r *Restore) IsFinished() bool {
	switch r.Status.Phase {
	case RestorePhaseCompleted, RestorePhaseFailed, RestorePhaseFailedValidation:
		return true
	}
	return false
}
