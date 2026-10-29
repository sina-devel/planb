// Package v1alpha1 contains API Schema definitions for the planb v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=planb.sina-devel.github.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "planb.sina-devel.github.io", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme

	knownTypes []runtime.Object
)

// register records types to add to the scheme; called from each type's init().
func register(objs ...runtime.Object) {
	knownTypes = append(knownTypes, objs...)
}

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, knownTypes...)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

const (
	// FinalizerName is set on Backups so their data is removed from storage on deletion.
	FinalizerName = "planb.sina-devel.github.io/backup-data"

	LabelStorageLocation = "planb.sina-devel.github.io/storage-location"
	LabelSchedule        = "planb.sina-devel.github.io/schedule"
	LabelBackup          = "planb.sina-devel.github.io/backup"
	LabelRestore         = "planb.sina-devel.github.io/restore"
	LabelManagedBy       = "app.kubernetes.io/managed-by"

	// AnnotationSynced marks a Backup that was created by syncing from object storage.
	AnnotationSynced = "planb.sina-devel.github.io/synced-from-storage"
	// AnnotationRetainData keeps the backup data in storage when the Backup object is deleted.
	AnnotationRetainData = "planb.sina-devel.github.io/retain-data"
	// AnnotationNotified lists the Notifiers that were told about a finished Backup or Restore.
	AnnotationNotified = "planb.sina-devel.github.io/notified"
	// AnnotationTest on a Notifier sends a test message whenever its value changes.
	AnnotationTest = "planb.sina-devel.github.io/test"
)
