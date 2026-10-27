// Package v1alpha1 contains API Schema definitions for the planb v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=planb.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "planb.io", Version: "v1alpha1"}

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
	FinalizerName = "planb.io/backup-data"

	LabelStorageLocation = "planb.io/storage-location"
	LabelSchedule        = "planb.io/schedule"
	LabelBackup          = "planb.io/backup"
	LabelRestore         = "planb.io/restore"
	LabelManagedBy       = "app.kubernetes.io/managed-by"

	// AnnotationSynced marks a Backup that was created by syncing from object storage.
	AnnotationSynced = "planb.io/synced-from-storage"
	// AnnotationRetainData keeps the backup data in storage when the Backup object is deleted.
	AnnotationRetainData = "planb.io/retain-data"
)
