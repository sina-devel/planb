package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	k8sretry "k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	planbv1 "github.com/sina-devel/planb/api/v1alpha1"
	"github.com/sina-devel/planb/internal/storage"
	"github.com/sina-devel/planb/internal/task"
)

const defaultValidationPeriod = time.Minute

// StorageLocationReconciler validates BackupStorageLocations and syncs backups found in storage into the cluster.
type StorageLocationReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	APIReader client.Reader
	Recorder  events.EventRecorder
}

func (r *StorageLocationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	bsl := &planbv1.BackupStorageLocation{}
	if err := r.Get(ctx, req.NamespacedName, bsl); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	period := defaultValidationPeriod
	if bsl.Spec.SyncPeriod != nil && bsl.Spec.SyncPeriod.Duration > 0 {
		period = max(bsl.Spec.SyncPeriod.Duration, 10*time.Second)
	}

	st, err := storageClient(ctx, r.APIReader, bsl)
	if err == nil {
		err = st.Check(ctx)
	}
	bsl.Status.LastValidationTime = now()
	if err != nil {
		if bsl.Status.Phase != planbv1.StorageLocationUnavailable {
			emit(r.Recorder, bsl, corev1.EventTypeWarning, "Unavailable", "Sync", err.Error())
		}
		bsl.Status.Phase = planbv1.StorageLocationUnavailable
		bsl.Status.Message = err.Error()
		return ctrl.Result{RequeueAfter: period}, r.Status().Update(ctx, bsl)
	}
	bsl.Status.Phase = planbv1.StorageLocationAvailable
	bsl.Status.Message = ""

	if bsl.Spec.SyncPeriod == nil || bsl.Spec.SyncPeriod.Duration > 0 {
		if err := r.sync(ctx, bsl, st); err != nil {
			log.Error(err, "syncing backups from storage")
			bsl.Status.Message = "sync failed: " + err.Error()
		} else {
			bsl.Status.LastSyncTime = now()
		}
	}
	return ctrl.Result{RequeueAfter: period}, r.Status().Update(ctx, bsl)
}

// sync creates Backup objects for completed backups that exist in storage but not in the cluster.
func (r *StorageLocationReconciler) sync(ctx context.Context, bsl *planbv1.BackupStorageLocation, st *storage.Client) error {
	log := logf.FromContext(ctx)
	names, err := st.ListDirs(ctx, task.BackupsKey(bsl.Spec.S3.Prefix))
	if err != nil {
		return err
	}
	existing := &planbv1.BackupList{}
	if err := r.List(ctx, existing, client.InNamespace(bsl.Namespace)); err != nil {
		return err
	}
	inCluster := map[string]*planbv1.Backup{}
	for i := range existing.Items {
		inCluster[existing.Items[i].Name] = &existing.Items[i]
	}

	for _, name := range names {
		b, ok := inCluster[name]
		if ok && b.Status.Phase != "" {
			continue
		}
		data, err := st.ReadAll(ctx, task.BackupKey(bsl.Spec.S3.Prefix, name)+task.MetadataFile)
		if err != nil {
			if storage.IsNotFound(err) {
				continue // in progress elsewhere, or not a planb backup
			}
			return err
		}
		meta := &planbv1.Backup{}
		if err := json.Unmarshal(data, meta); err != nil {
			log.Error(err, "invalid backup metadata", "backup", name)
			continue
		}
		if meta.Status.Phase != planbv1.BackupPhaseCompleted {
			continue
		}
		if meta.Status.Expiration != nil && meta.Status.Expiration.Time.Before(time.Now()) &&
			bsl.Spec.AccessMode != planbv1.AccessModeReadOnly {
			continue // expired: would be garbage-collected right away
		}
		if !ok {
			b = &planbv1.Backup{}
			b.Name = name
			b.Namespace = bsl.Namespace
			b.Labels = meta.Labels
			if b.Labels == nil {
				b.Labels = map[string]string{}
			}
			b.Labels[planbv1.LabelStorageLocation] = bsl.Name
			b.Annotations = map[string]string{planbv1.AnnotationSynced: "true"}
			b.Spec = meta.Spec
			b.Spec.StorageLocation = bsl.Name
			if err := r.Create(ctx, b); err != nil {
				if apierrors.IsAlreadyExists(err) {
					continue
				}
				return err
			}
		} else if b.Annotations[planbv1.AnnotationSynced] != "true" {
			continue // a local backup that has not started yet: not ours to touch
		}
		// The backup controller adds its finalizer right after the Backup is created, so the copy in hand may be
		// stale: re-read it from the API server and retry on conflict.
		err = k8sretry.RetryOnConflict(k8sretry.DefaultRetry, func() error {
			if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(b), b); err != nil {
				return err
			}
			b.Status = meta.Status
			return r.Status().Update(ctx, b)
		})
		if err != nil {
			return err
		}
		log.Info("synced backup from storage", "backup", name)
		emit(r.Recorder, bsl, corev1.EventTypeNormal, "BackupSynced", "Sync", fmt.Sprintf("synced backup %s from storage", name))
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *StorageLocationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&planbv1.BackupStorageLocation{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("backupstoragelocation").
		Complete(r)
}
