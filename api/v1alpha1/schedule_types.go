package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ScheduleSpec defines the desired state of Schedule.
type ScheduleSpec struct {
	// Schedule in cron format (5 fields). "CRON_TZ=Europe/Berlin 0 3 * * *" and
	// descriptors such as "@daily" or "@every 6h" are supported.
	// +kubebuilder:validation:MinLength=1
	Schedule string `json:"schedule"`
	// Paused stops new backups from being created.
	// +optional
	Paused bool `json:"paused,omitempty"`
	// SkipIfRunning does not create a new backup while a previous one from this schedule is still running.
	// +kubebuilder:default=true
	// +optional
	SkipIfRunning *bool `json:"skipIfRunning,omitempty"`
	// KeepLast deletes the oldest completed backups of this schedule beyond this count (0 = rely on TTL only).
	// +kubebuilder:validation:Minimum=0
	// +optional
	KeepLast int32 `json:"keepLast,omitempty"`
	// Template of the Backups created by this schedule.
	Template BackupSpec `json:"template"`
}

type SchedulePhase string

const (
	SchedulePhaseEnabled          SchedulePhase = "Enabled"
	SchedulePhasePaused           SchedulePhase = "Paused"
	SchedulePhaseFailedValidation SchedulePhase = "FailedValidation"
)

// ScheduleStatus defines the observed state of Schedule.
type ScheduleStatus struct {
	// +optional
	Phase SchedulePhase `json:"phase,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	LastBackup string `json:"lastBackup,omitempty"`
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`
	// +optional
	NextScheduleTime *metav1.Time `json:"nextScheduleTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Last Backup",type=string,JSONPath=`.status.lastBackup`
// +kubebuilder:printcolumn:name="Next",type=date,JSONPath=`.status.nextScheduleTime`

// Schedule creates Backups periodically.
type Schedule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ScheduleSpec   `json:"spec,omitempty"`
	Status ScheduleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ScheduleList contains a list of Schedule.
type ScheduleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Schedule `json:"items"`
}

func init() {
	register(&Schedule{}, &ScheduleList{})
}
