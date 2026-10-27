package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AccessMode of a BackupStorageLocation.
// +kubebuilder:validation:Enum=ReadWrite;ReadOnly
type AccessMode string

const (
	AccessModeReadWrite AccessMode = "ReadWrite"
	// AccessModeReadOnly allows restores only: no new backups and no data deletion.
	AccessModeReadOnly AccessMode = "ReadOnly"
)

// BackupStorageLocationSpec defines where backups are stored.
type BackupStorageLocationSpec struct {
	// S3 bucket that stores backups.
	S3 S3Spec `json:"s3"`
	// +kubebuilder:default=ReadWrite
	// +optional
	AccessMode AccessMode `json:"accessMode,omitempty"`
	// SyncPeriod is how often backups found in storage are synced into the cluster. 0 disables sync.
	// +kubebuilder:default="1m"
	// +optional
	SyncPeriod *metav1.Duration `json:"syncPeriod,omitempty"`
}

type StorageLocationPhase string

const (
	StorageLocationAvailable   StorageLocationPhase = "Available"
	StorageLocationUnavailable StorageLocationPhase = "Unavailable"
)

// BackupStorageLocationStatus defines the observed state of BackupStorageLocation.
type BackupStorageLocationStatus struct {
	// +optional
	Phase StorageLocationPhase `json:"phase,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	LastValidationTime *metav1.Time `json:"lastValidationTime,omitempty"`
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=bsl
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.spec.s3.endpoint`
// +kubebuilder:printcolumn:name="Bucket",type=string,JSONPath=`.spec.s3.bucket`
// +kubebuilder:printcolumn:name="Access",type=string,JSONPath=`.spec.accessMode`
// +kubebuilder:printcolumn:name="Last Validated",type=date,JSONPath=`.status.lastValidationTime`

// BackupStorageLocation is an S3-compatible bucket where backups are stored.
type BackupStorageLocation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BackupStorageLocationSpec   `json:"spec,omitempty"`
	Status BackupStorageLocationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BackupStorageLocationList contains a list of BackupStorageLocation.
type BackupStorageLocationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BackupStorageLocation `json:"items"`
}

func init() {
	register(&BackupStorageLocation{}, &BackupStorageLocationList{})
}
