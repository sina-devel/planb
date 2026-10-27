package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/robfig/cron/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	planbv1 "github.com/sina-devel/planb/api/v1alpha1"
)

// ScheduleReconciler creates Backups from Schedules.
type ScheduleReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	// Now is overridable in tests.
	Now func() time.Time
}

// ParseSchedule parses a cron expression (standard 5 fields, descriptors, CRON_TZ= prefix).
func ParseSchedule(s string) (cron.Schedule, error) {
	return cron.ParseStandard(s)
}

// lastSlot returns the most recent schedule slot after `from` that is not after `now`, or zero.
func lastSlot(sched cron.Schedule, from, now time.Time) time.Time {
	var slot time.Time
	for t, i := sched.Next(from), 0; !t.After(now) && !t.IsZero() && i < 100000; t, i = sched.Next(t), i+1 {
		slot = t
	}
	return slot
}

func (r *ScheduleReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *ScheduleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	s := &planbv1.Schedule{}
	if err := r.Get(ctx, req.NamespacedName, s); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := s.Status.DeepCopy()

	sched, err := ParseSchedule(s.Spec.Schedule)
	if err != nil {
		s.Status.Phase = planbv1.SchedulePhaseFailedValidation
		s.Status.Message = fmt.Sprintf("invalid schedule %q: %v", s.Spec.Schedule, err)
		s.Status.NextScheduleTime = nil
		return ctrl.Result{}, r.updateStatus(ctx, s, orig)
	}
	if s.Spec.Paused {
		s.Status.Phase = planbv1.SchedulePhasePaused
		s.Status.Message = ""
		s.Status.NextScheduleTime = nil
		return ctrl.Result{}, r.updateStatus(ctx, s, orig)
	}
	s.Status.Phase = planbv1.SchedulePhaseEnabled

	now := r.now()
	from := s.CreationTimestamp.Time
	if s.Status.LastScheduleTime != nil {
		from = s.Status.LastScheduleTime.Time
	}
	if slot := lastSlot(sched, from, now); !slot.IsZero() {
		running, err := r.hasRunningBackup(ctx, s)
		if err != nil {
			return ctrl.Result{}, err
		}
		if running && ptr.Deref(s.Spec.SkipIfRunning, true) {
			s.Status.Message = fmt.Sprintf("skipped run at %s: previous backup still running", slot.UTC().Format(time.RFC3339))
			emit(r.Recorder, s, corev1.EventTypeNormal, "Skipped", "Schedule", s.Status.Message)
		} else {
			name, err := r.createBackup(ctx, s, slot)
			if err != nil {
				return ctrl.Result{}, err
			}
			log.Info("created backup", "backup", name)
			s.Status.LastBackup = name
			s.Status.Message = ""
		}
		s.Status.LastScheduleTime = &metav1.Time{Time: slot}
		from = slot
	}

	if err := r.prune(ctx, s); err != nil {
		log.Error(err, "pruning old backups")
	}

	next := sched.Next(from)
	s.Status.NextScheduleTime = &metav1.Time{Time: next}
	if err := r.updateStatus(ctx, s, orig); err != nil {
		return ctrl.Result{}, err
	}
	wait := time.Until(next)
	if wait < time.Second {
		wait = time.Second
	}
	return ctrl.Result{RequeueAfter: wait}, nil
}

func (r *ScheduleReconciler) updateStatus(ctx context.Context, s *planbv1.Schedule, orig *planbv1.ScheduleStatus) error {
	if equalStatus(orig, &s.Status) {
		return nil
	}
	return r.Status().Update(ctx, s)
}

func equalStatus(a, b *planbv1.ScheduleStatus) bool {
	return a.Phase == b.Phase && a.Message == b.Message && a.LastBackup == b.LastBackup &&
		a.LastScheduleTime.Equal(b.LastScheduleTime) && a.NextScheduleTime.Equal(b.NextScheduleTime)
}

func (r *ScheduleReconciler) scheduleBackups(ctx context.Context, s *planbv1.Schedule) ([]planbv1.Backup, error) {
	list := &planbv1.BackupList{}
	err := r.List(ctx, list, client.InNamespace(s.Namespace), client.MatchingLabels{planbv1.LabelSchedule: s.Name})
	return list.Items, err
}

func (r *ScheduleReconciler) hasRunningBackup(ctx context.Context, s *planbv1.Schedule) (bool, error) {
	items, err := r.scheduleBackups(ctx, s)
	if err != nil {
		return false, err
	}
	for i := range items {
		if !items[i].IsFinished() && items[i].DeletionTimestamp.IsZero() {
			return true, nil
		}
	}
	return false, nil
}

// NewBackupFromSchedule builds the Backup a schedule creates for a given slot.
func NewBackupFromSchedule(s *planbv1.Schedule, slot time.Time) *planbv1.Backup {
	labels := map[string]string{planbv1.LabelSchedule: s.Name}
	return &planbv1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", s.Name, slot.UTC().Format("20060102150405")),
			Namespace: s.Namespace,
			Labels:    labels,
		},
		Spec: *s.Spec.Template.DeepCopy(),
	}
}

func (r *ScheduleReconciler) createBackup(ctx context.Context, s *planbv1.Schedule, slot time.Time) (string, error) {
	b := NewBackupFromSchedule(s, slot)
	if err := r.Create(ctx, b); err != nil && !apierrors.IsAlreadyExists(err) {
		emit(r.Recorder, s, corev1.EventTypeWarning, "BackupCreateFailed", "Schedule", err.Error())
		return "", err
	}
	emit(r.Recorder, s, corev1.EventTypeNormal, "BackupCreated", "Schedule", fmt.Sprintf("created backup %s", b.Name))
	return b.Name, nil
}

// prune deletes the oldest completed backups beyond spec.keepLast.
func (r *ScheduleReconciler) prune(ctx context.Context, s *planbv1.Schedule) error {
	if s.Spec.KeepLast <= 0 {
		return nil
	}
	items, err := r.scheduleBackups(ctx, s)
	if err != nil {
		return err
	}
	var done []planbv1.Backup
	for _, b := range items {
		if b.Status.Phase == planbv1.BackupPhaseCompleted && b.DeletionTimestamp.IsZero() {
			done = append(done, b)
		}
	}
	sort.Slice(done, func(i, j int) bool {
		return done[i].CreationTimestamp.After(done[j].CreationTimestamp.Time)
	})
	for i := int(s.Spec.KeepLast); i < len(done); i++ {
		if err := r.Delete(ctx, &done[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
		emit(r.Recorder, s, corev1.EventTypeNormal, "BackupPruned", "Schedule", fmt.Sprintf("deleted backup %s (keepLast=%d)", done[i].Name, s.Spec.KeepLast))
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ScheduleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&planbv1.Schedule{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// Re-evaluate retention whenever one of the schedule's backups changes.
		Watches(&planbv1.Backup{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
			name := o.GetLabels()[planbv1.LabelSchedule]
			if name == "" {
				return nil
			}
			return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: o.GetNamespace(), Name: name}}}
		})).
		Named("schedule").
		Complete(r)
}
