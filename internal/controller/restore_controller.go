package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	planbv1 "github.com/sina-devel/planb/api/v1alpha1"
	"github.com/sina-devel/planb/internal/task"
)

// RestoreReconciler runs Restores as Jobs.
type RestoreReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	APIReader client.Reader
	Recorder  events.EventRecorder
	Options
}

func (r *RestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	rs := &planbv1.Restore{}
	if err := r.Get(ctx, req.NamespacedName, rs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	rs.SetGroupVersionKind(planbv1.GroupVersion.WithKind("Restore"))
	if !rs.DeletionTimestamp.IsZero() || rs.IsFinished() {
		return ctrl.Result{}, nil
	}
	switch rs.Status.Phase {
	case "", planbv1.RestorePhaseNew:
		return ctrl.Result{}, r.start(ctx, rs)
	case planbv1.RestorePhaseInProgress:
		return ctrl.Result{}, r.track(ctx, rs)
	}
	return ctrl.Result{}, nil
}

func (r *RestoreReconciler) finish(ctx context.Context, rs *planbv1.Restore, phase planbv1.RestorePhase, reason, msg string) error {
	evType := corev1.EventTypeWarning
	if phase == planbv1.RestorePhaseCompleted {
		evType = corev1.EventTypeNormal
	}
	emit(r.Recorder, rs, evType, reason, "Restore", msg)
	rs.Status.Phase = phase
	rs.Status.Message = msg
	rs.Status.CompletionTimestamp = now()
	return r.Status().Update(ctx, rs)
}

func (r *RestoreReconciler) invalid(ctx context.Context, rs *planbv1.Restore, format string, args ...any) error {
	return r.finish(ctx, rs, planbv1.RestorePhaseFailedValidation, "FailedValidation", fmt.Sprintf(format, args...))
}

func (r *RestoreReconciler) start(ctx context.Context, rs *planbv1.Restore) error {
	b := &planbv1.Backup{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: rs.Namespace, Name: rs.Spec.BackupName}, b); err != nil {
		if apierrors.IsNotFound(err) {
			return r.invalid(ctx, rs, "backup %q not found", rs.Spec.BackupName)
		}
		return err
	}
	if b.Status.Phase != planbv1.BackupPhaseCompleted {
		return r.invalid(ctx, rs, "backup %q is not completed (phase %q)", b.Name, b.Status.Phase)
	}
	bsl := &planbv1.BackupStorageLocation{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: rs.Namespace, Name: b.Spec.StorageLocation}, bsl); err != nil {
		if apierrors.IsNotFound(err) {
			return r.invalid(ctx, rs, "BackupStorageLocation %q not found", b.Spec.StorageLocation)
		}
		return err
	}

	t := &task.Task{
		Op:        task.OpRestore,
		Name:      rs.Name,
		Storage:   storageConfig(&bsl.Spec.S3),
		BackupKey: task.BackupKey(bsl.Spec.S3.Prefix, b.Name),
	}
	env := s3CredEnv(bsl.Spec.S3.Credentials, task.EnvStorageAccess, task.EnvStorageSecret)

	switch {
	case b.Spec.Postgres != nil:
		if rs.Spec.Bucket != nil {
			return r.invalid(ctx, rs, "backup %q is a postgres backup, but spec.bucket is set", b.Name)
		}
		opts := rs.Spec.Postgres
		if opts == nil {
			opts = &planbv1.PostgresRestoreOptions{}
		}
		target := &b.Spec.Postgres.PostgresConnection
		if opts.Target != nil {
			target = opts.Target
		}
		t.Postgres = &task.Postgres{
			Host:            target.Host,
			Port:            portOrDefault(target.Port),
			SSLMode:         target.SSLMode,
			Databases:       opts.Databases,
			ExtraArgs:       opts.ExtraArgs,
			DatabaseMapping: opts.DatabaseMapping,
			Clean:           opts.Clean,
			CreateDatabase:  ptr.Deref(opts.CreateDatabase, true),
			RestoreGlobals:  opts.RestoreGlobals,
		}
		env = append(env, pgCredEnv(target.Credentials)...)
	case b.Spec.Bucket != nil:
		if rs.Spec.Postgres != nil {
			return r.invalid(ctx, rs, "backup %q is a bucket backup, but spec.postgres is set", b.Name)
		}
		opts := rs.Spec.Bucket
		if opts == nil {
			opts = &planbv1.BucketRestoreOptions{}
		}
		target := &b.Spec.Bucket.S3Spec
		if opts.Target != nil {
			target = opts.Target
		}
		t.Bucket = &task.Bucket{S3: storageConfig(target), Overwrite: ptr.Deref(opts.Overwrite, true), Prune: opts.Prune}
		env = append(env, s3CredEnv(target.Credentials, task.EnvBucketAccess, task.EnvBucketSecret)...)
	default:
		return r.invalid(ctx, rs, "backup %q has no source", b.Name)
	}

	name := jobName("planb-restore", rs.Name)
	job, err := r.buildJob(rs, name, map[string]string{
		planbv1.LabelRestore: rs.Name,
		planbv1.LabelBackup:  b.Name,
	}, t, env, rs.Spec.Job)
	if err != nil {
		return err
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating job: %w", err)
	}
	emit(r.Recorder, rs, corev1.EventTypeNormal, "Started", "Restore", fmt.Sprintf("restore job %s created", name))
	rs.Status.Phase = planbv1.RestorePhaseInProgress
	rs.Status.JobName = name
	rs.Status.StartTimestamp = now()
	return r.Status().Update(ctx, rs)
}

func (r *RestoreReconciler) track(ctx context.Context, rs *planbv1.Restore) error {
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: rs.Namespace, Name: rs.Status.JobName}, job); err != nil {
		if apierrors.IsNotFound(err) {
			return r.finish(ctx, rs, planbv1.RestorePhaseFailed, "JobNotFound", fmt.Sprintf("job %s disappeared", rs.Status.JobName))
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
	rs.Status.SizeBytes = res.SizeBytes
	rs.Status.Items = res.Items
	rs.Status.DeletedItems = res.Deleted
	if state == jobFailed {
		msg := res.Error
		if msg == "" {
			msg = reason
		}
		return r.finish(ctx, rs, planbv1.RestorePhaseFailed, "RestoreFailed", msg)
	}
	msg := fmt.Sprintf("restore completed: %d items, %d bytes", res.Items, res.SizeBytes)
	if res.Deleted > 0 {
		msg += fmt.Sprintf(", %d extra objects deleted", res.Deleted)
	}
	return r.finish(ctx, rs, planbv1.RestorePhaseCompleted, "Completed", msg)
}

// SetupWithManager sets up the controller with the Manager.
func (r *RestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&planbv1.Restore{}).
		Owns(&batchv1.Job{}).
		Named("restore").
		Complete(r)
}
