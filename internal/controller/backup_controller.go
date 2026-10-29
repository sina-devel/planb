package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	planbv1 "github.com/sina-devel/planb/api/v1alpha1"
	"github.com/sina-devel/planb/internal/task"
)

// BackupReconciler runs Backups as Jobs, tracks their completion and garbage-collects expired ones.
type BackupReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	APIReader client.Reader
	Recorder  events.EventRecorder
	Options
}

// +kubebuilder:rbac:groups=planb.sina-devel.github.io,resources=backups;restores;schedules;backupstoragelocations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=planb.sina-devel.github.io,resources=backups/status;restores/status;schedules/status;backupstoragelocations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=planb.sina-devel.github.io,resources=backups/finalizers;restores/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *BackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	b := &planbv1.Backup{}
	if err := r.Get(ctx, req.NamespacedName, b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	b.SetGroupVersionKind(planbv1.GroupVersion.WithKind("Backup"))

	if !b.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, b)
	}
	if !controllerutil.ContainsFinalizer(b, planbv1.FinalizerName) {
		controllerutil.AddFinalizer(b, planbv1.FinalizerName)
		if err := r.Update(ctx, b); err != nil {
			return ctrl.Result{}, err
		}
	}

	switch b.Status.Phase {
	case "", planbv1.BackupPhaseNew:
		if b.Annotations[planbv1.AnnotationSynced] == "true" {
			return ctrl.Result{}, nil // status is filled in by the storage location sync
		}
		return ctrl.Result{}, r.start(ctx, b)
	case planbv1.BackupPhaseInProgress:
		return ctrl.Result{}, r.track(ctx, b)
	case planbv1.BackupPhaseCompleted, planbv1.BackupPhaseFailed:
		if b.Status.Expiration == nil {
			return ctrl.Result{}, nil
		}
		left := time.Until(b.Status.Expiration.Time)
		if left > 0 {
			return ctrl.Result{RequeueAfter: left}, nil
		}
		bsl := &planbv1.BackupStorageLocation{}
		err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.StorageLocation}, bsl)
		if err == nil && bsl.Spec.AccessMode == planbv1.AccessModeReadOnly {
			return ctrl.Result{}, nil // never garbage-collect from a read-only location
		}
		log.Info("backup expired, deleting", "expiration", b.Status.Expiration)
		emit(r.Recorder, b, corev1.EventTypeNormal, "Expired", "Backup", "backup TTL expired")
		return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, b))
	}
	return ctrl.Result{}, nil
}

func (r *BackupReconciler) fail(ctx context.Context, b *planbv1.Backup, reason, msg string) error {
	emit(r.Recorder, b, corev1.EventTypeWarning, reason, "Backup", msg)
	b.Status.Phase = planbv1.BackupPhaseFailed
	b.Status.Message = msg
	b.Status.CompletionTimestamp = now()
	b.Status.Expiration = r.expiration(b)
	return r.Status().Update(ctx, b)
}

func (r *BackupReconciler) expiration(b *planbv1.Backup) *metav1.Time {
	ttl := r.DefaultTTL
	if b.Spec.TTL != nil {
		ttl = b.Spec.TTL.Duration
	}
	if ttl <= 0 {
		return nil
	}
	t := metav1.NewTime(b.Status.CompletionTimestamp.Add(ttl))
	return &t
}

func (r *BackupReconciler) start(ctx context.Context, b *planbv1.Backup) error {
	bsl := &planbv1.BackupStorageLocation{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.StorageLocation}, bsl); err != nil {
		if apierrors.IsNotFound(err) {
			return r.fail(ctx, b, "StorageLocationNotFound", fmt.Sprintf("BackupStorageLocation %q not found", b.Spec.StorageLocation))
		}
		return err
	}
	if bsl.Spec.AccessMode == planbv1.AccessModeReadOnly {
		return r.fail(ctx, b, "StorageLocationReadOnly", fmt.Sprintf("BackupStorageLocation %q is read-only", bsl.Name))
	}
	if b.Spec.Postgres == nil && b.Spec.Bucket == nil {
		return r.fail(ctx, b, "InvalidSpec", "exactly one of postgres or bucket must be set")
	}

	t := &task.Task{
		Op:        task.OpBackup,
		Name:      b.Name,
		Storage:   storageConfig(&bsl.Spec.S3),
		BackupKey: task.BackupKey(bsl.Spec.S3.Prefix, b.Name),
	}
	env := s3CredEnv(bsl.Spec.S3.Credentials, task.EnvStorageAccess, task.EnvStorageSecret)
	if pg := b.Spec.Postgres; pg != nil {
		t.Postgres = &task.Postgres{
			Host:           pg.Host,
			Port:           portOrDefault(pg.Port),
			SSLMode:        pg.SSLMode,
			Databases:      pg.Databases,
			IncludeGlobals: pg.IncludeGlobals,
			ExtraArgs:      pg.ExtraArgs,
		}
		env = append(env, pgCredEnv(pg.Credentials)...)
	} else {
		t.Bucket = &task.Bucket{S3: storageConfig(&b.Spec.Bucket.S3Spec)}
		env = append(env, s3CredEnv(b.Spec.Bucket.Credentials, task.EnvBucketAccess, task.EnvBucketSecret)...)
	}

	name := jobName("planb-backup", b.Name)
	job, err := r.buildJob(b, name, map[string]string{
		planbv1.LabelBackup:          b.Name,
		planbv1.LabelStorageLocation: bsl.Name,
	}, t, env, b.Spec.Job)
	if err != nil {
		return err
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating job: %w", err)
	}

	if b.Labels == nil {
		b.Labels = map[string]string{}
	}
	if b.Labels[planbv1.LabelStorageLocation] != bsl.Name {
		b.Labels[planbv1.LabelStorageLocation] = bsl.Name
		if err := r.Update(ctx, b); err != nil {
			return err
		}
	}

	emit(r.Recorder, b, corev1.EventTypeNormal, "Started", "Backup", fmt.Sprintf("backup job %s created", name))
	b.Status.Phase = planbv1.BackupPhaseInProgress
	b.Status.JobName = name
	b.Status.StartTimestamp = now()
	b.Status.Location = fmt.Sprintf("s3://%s/%s", bsl.Spec.S3.Bucket, t.BackupKey)
	b.Status.Message = ""
	return r.Status().Update(ctx, b)
}

func (r *BackupReconciler) track(ctx context.Context, b *planbv1.Backup) error {
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Status.JobName}, job); err != nil {
		if apierrors.IsNotFound(err) {
			return r.fail(ctx, b, "JobNotFound", fmt.Sprintf("job %s disappeared", b.Status.JobName))
		}
		return err
	}
	state, reason := stateOf(job)
	if state == jobRunning {
		return nil
	}
	res, err := jobResult(ctx, r.APIReader, job)
	if err != nil {
		res = &task.Result{Error: err.Error()}
	}
	if state == jobFailed {
		msg := res.Error
		if msg == "" {
			msg = reason
		}
		return r.fail(ctx, b, "BackupFailed", msg)
	}

	b.Status.Phase = planbv1.BackupPhaseCompleted
	b.Status.CompletionTimestamp = now()
	b.Status.Expiration = r.expiration(b)
	b.Status.SizeBytes = res.SizeBytes
	b.Status.Items = res.Items
	b.Status.Files = res.Files
	b.Status.Message = ""

	if err := r.writeMetadata(ctx, b); err != nil {
		// The data is safe; only the sync to other clusters is affected.
		b.Status.Message = "backup completed but metadata upload failed: " + err.Error()
		emit(r.Recorder, b, corev1.EventTypeWarning, "MetadataUploadFailed", "Backup", err.Error())
	}
	emit(r.Recorder, b, corev1.EventTypeNormal, "Completed", "Backup", fmt.Sprintf("backup completed: %d items, %d bytes", res.Items, res.SizeBytes))
	return r.Status().Update(ctx, b)
}

// writeMetadata stores the Backup object next to its data so other clusters can sync it.
func (r *BackupReconciler) writeMetadata(ctx context.Context, b *planbv1.Backup) error {
	bsl := &planbv1.BackupStorageLocation{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.StorageLocation}, bsl); err != nil {
		return err
	}
	st, err := storageClient(ctx, r.APIReader, bsl)
	if err != nil {
		return err
	}
	meta := planbv1.Backup{
		TypeMeta: metav1.TypeMeta{APIVersion: planbv1.GroupVersion.String(), Kind: "Backup"},
		ObjectMeta: metav1.ObjectMeta{
			Name:              b.Name,
			Namespace:         b.Namespace,
			Labels:            b.Labels,
			CreationTimestamp: b.CreationTimestamp,
		},
		Spec:   b.Spec,
		Status: b.Status,
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return st.PutBytes(ctx, task.BackupKey(bsl.Spec.S3.Prefix, b.Name)+task.MetadataFile, data, "application/json")
}

func (r *BackupReconciler) finalize(ctx context.Context, b *planbv1.Backup) error {
	log := logf.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(b, planbv1.FinalizerName) {
		return nil
	}
	if reason := r.keepDataReason(ctx, b); reason != "" {
		log.Info("keeping backup data", "reason", reason)
	} else {
		if b.Status.Phase != planbv1.BackupPhaseDeleting {
			b.Status.Phase = planbv1.BackupPhaseDeleting
			if err := r.Status().Update(ctx, b); err != nil {
				return err
			}
		}
		bsl := &planbv1.BackupStorageLocation{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.StorageLocation}, bsl); err != nil {
			return err
		}
		st, err := storageClient(ctx, r.APIReader, bsl)
		if err != nil {
			return err
		}
		key := task.BackupKey(bsl.Spec.S3.Prefix, b.Name)
		log.Info("deleting backup data", "location", st.URL(key))
		if err := st.DeletePrefix(ctx, key); err != nil {
			emit(r.Recorder, b, corev1.EventTypeWarning, "DeleteFailed", "Backup", err.Error())
			return err
		}
	}
	controllerutil.RemoveFinalizer(b, planbv1.FinalizerName)
	return client.IgnoreNotFound(r.Update(ctx, b))
}

// keepDataReason returns why the data of a deleted Backup must stay in storage, or "".
func (r *BackupReconciler) keepDataReason(ctx context.Context, b *planbv1.Backup) string {
	if b.Annotations[planbv1.AnnotationRetainData] == "true" {
		return "annotation " + planbv1.AnnotationRetainData
	}
	ns := &corev1.Namespace{}
	if err := r.APIReader.Get(ctx, types.NamespacedName{Name: b.Namespace}, ns); err == nil && !ns.DeletionTimestamp.IsZero() {
		return "namespace is being deleted"
	}
	bsl := &planbv1.BackupStorageLocation{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.StorageLocation}, bsl); err != nil {
		if apierrors.IsNotFound(err) {
			return "storage location not found"
		}
		return "" // transient: retry deletion
	}
	if !bsl.DeletionTimestamp.IsZero() {
		return "storage location is being deleted"
	}
	if bsl.Spec.AccessMode == planbv1.AccessModeReadOnly {
		return "storage location is read-only"
	}
	if b.Status.Location == "" && b.Annotations[planbv1.AnnotationSynced] != "true" {
		return "backup never started"
	}
	return ""
}

func portOrDefault(p int32) int32 {
	if p == 0 {
		return 5432
	}
	return p
}

// SetupWithManager sets up the controller with the Manager.
func (r *BackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&planbv1.Backup{}).
		Owns(&batchv1.Job{}).
		Named("backup").
		Complete(r)
}
