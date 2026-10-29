package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	planbv1 "github.com/sina-devel/planb/api/v1alpha1"
	"github.com/sina-devel/planb/internal/notify"
)

// notifyRetryWindow is how long failed deliveries are retried after a Backup or Restore finished.
const notifyRetryWindow = time.Hour

// +kubebuilder:rbac:groups=planb.sina-devel.github.io,resources=notifiers,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=planb.sina-devel.github.io,resources=notifiers/status,verbs=get;update;patch

// NotifierReconciler validates Notifiers and sends test messages.
type NotifierReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	APIReader   client.Reader
	Recorder    events.EventRecorder
	ClusterName string
}

func (r *NotifierReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	n := &planbv1.Notifier{}
	if err := r.Get(ctx, req.NamespacedName, n); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := n.DeepCopy()
	n.Status.Type = n.Spec.Type()

	// A configuration error is reported until it is fixed; a delivery error until the next successful delivery.
	sender, err := buildSender(ctx, r.APIReader, n)
	switch {
	case err != nil:
		n.Status.Phase, n.Status.Message = planbv1.NotifierPhaseFailed, err.Error()
	case n.Status.Phase == "" || strings.HasPrefix(n.Status.Message, errInvalidConfig):
		n.Status.Phase, n.Status.Message = planbv1.NotifierPhaseReady, ""
	}

	if test := n.Annotations[planbv1.AnnotationTest]; test != "" && test != n.Status.LastTestRequest {
		n.Status.LastTestRequest = test
		if sender != nil {
			msg := &notify.Message{Event: "Test", Cluster: r.ClusterName, Kind: "Notifier", Namespace: n.Namespace, Name: n.Name}
			recordDelivery(r.Recorder, n, sender.Send(ctx, msg), "test message")
		}
	}
	if equalNotifierStatus(&orig.Status, &n.Status) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, r.Status().Patch(ctx, n, client.MergeFrom(orig))
}

func equalNotifierStatus(a, b *planbv1.NotifierStatus) bool {
	return a.Type == b.Type && a.Phase == b.Phase && a.Message == b.Message && a.LastTestRequest == b.LastTestRequest &&
		a.LastSentTime.Equal(b.LastSentTime) && a.LastFailureTime.Equal(b.LastFailureTime)
}

// recordDelivery updates the Notifier status (not saved) and emits an event for a delivery attempt.
func recordDelivery(rec events.EventRecorder, n *planbv1.Notifier, err error, what string) {
	if err != nil {
		n.Status.Phase, n.Status.Message, n.Status.LastFailureTime = planbv1.NotifierPhaseFailed, err.Error(), now()
		emit(rec, n, corev1.EventTypeWarning, "SendFailed", "Notify", fmt.Sprintf("sending %s: %v", what, err))
		return
	}
	n.Status.Phase, n.Status.Message, n.Status.LastSentTime = planbv1.NotifierPhaseReady, "", now()
	emit(rec, n, corev1.EventTypeNormal, "Sent", "Notify", "sent "+what)
}

// buildSender reads the Notifier's secrets and returns its channel. Errors are configuration problems.
func buildSender(ctx context.Context, r SecretReader, n *planbv1.Notifier) (notify.Sender, error) {
	s := &n.Spec
	get := func(ref planbv1.SecretKeyRef) (string, error) {
		return secretValue(ctx, r, n.Namespace, ref.Name, ref.Key)
	}
	var (
		sender notify.Sender
		err    error
	)
	switch {
	case s.Webhook != nil:
		w := &notify.Webhook{URL: s.Webhook.URL, Headers: s.Webhook.Headers}
		if s.Webhook.URLSecret != nil {
			w.URL, err = get(*s.Webhook.URLSecret)
		}
		sender = w
	case s.Slack != nil:
		sl := &notify.Slack{Channel: s.Slack.Channel}
		sl.WebhookURL, err = get(s.Slack.WebhookURL)
		sender = sl
	case s.Telegram != nil:
		t := &notify.Telegram{APIURL: s.Telegram.APIURL, ChatID: s.Telegram.ChatID}
		t.BotToken, err = get(s.Telegram.BotToken)
		sender = t
	case s.Email != nil:
		e := &notify.Email{Host: s.Email.Host, Port: int(portOr(s.Email.Port, 587)), TLS: string(s.Email.TLS),
			SkipTLSVerify: s.Email.SkipTLSVerify, From: s.Email.From, To: s.Email.To}
		if c := s.Email.Credentials; c != nil {
			if e.Username, err = secretValue(ctx, r, n.Namespace, c.Name, defaultStr(c.UsernameKey, "username")); err == nil {
				e.Password, err = secretValue(ctx, r, n.Namespace, c.Name, defaultStr(c.PasswordKey, "password"))
			}
		}
		sender = e
	default:
		err = fmt.Errorf("no channel configured")
	}
	if err != nil {
		return nil, fmt.Errorf("%s%w", errInvalidConfig, err)
	}
	return sender, nil
}

const errInvalidConfig = "invalid configuration: "

func portOr(p, d int32) int32 {
	if p == 0 {
		return d
	}
	return p
}

// SetupWithManager sets up the controller with the Manager.
func (r *NotifierReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&planbv1.Notifier{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Named("notifier").
		Complete(r)
}

// NotificationReconciler tells the matching Notifiers about finished Backups or Restores (one reconciler per kind).
// Delivered Notifiers are recorded in the planb.sina-devel.github.io/notified annotation, so each is told at least once.
type NotificationReconciler struct {
	client.Client
	APIReader   client.Reader
	Recorder    events.EventRecorder
	ClusterName string
	// Restores selects the kind: false for Backups, true for Restores.
	Restores bool
}

func (r *NotificationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	obj, msg, err := r.message(ctx, req.NamespacedName)
	if err != nil || msg == nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	notifiers := &planbv1.NotifierList{}
	if err := r.List(ctx, notifiers, client.InNamespace(req.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	finished := obj.GetCreationTimestamp().Time
	if msg.CompletionTime != nil {
		finished = *msg.CompletionTime
	}
	done := notifiedSet(obj)
	var failed []string
	for i := range notifiers.Items {
		n := &notifiers.Items[i]
		if done[n.Name] || !wants(n, planbv1.NotificationEvent(msg.Event), obj.GetLabels()) ||
			finished.Before(n.CreationTimestamp.Time) { // never notify about history
			continue
		}
		sender, err := buildSender(ctx, r.APIReader, n)
		if err == nil {
			err = sender.Send(ctx, msg)
		}
		orig := n.DeepCopy()
		recordDelivery(r.Recorder, n, err, fmt.Sprintf("%s for %s %s", msg.Event, msg.Kind, obj.GetName()))
		if perr := r.Status().Patch(ctx, n, client.MergeFrom(orig)); perr != nil {
			log.Error(perr, "updating notifier status", "notifier", n.Name)
		}
		if err != nil {
			failed = append(failed, n.Name)
			continue
		}
		done[n.Name] = true
	}

	if len(done) > len(notifiedSet(obj)) {
		names := make([]string, 0, len(done))
		for k := range done {
			names = append(names, k)
		}
		slices.Sort(names)
		patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
		ann := obj.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		ann[planbv1.AnnotationNotified] = strings.Join(names, ",")
		obj.SetAnnotations(ann)
		if err := r.Patch(ctx, obj, patch); err != nil {
			return ctrl.Result{}, err
		}
	}
	if len(failed) > 0 && time.Since(finished) < notifyRetryWindow {
		log.Info("notification failed, will retry", "notifiers", failed)
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	return ctrl.Result{}, nil
}

// message returns the object and its notification, or a nil message if it is not finished or must not be notified.
func (r *NotificationReconciler) message(ctx context.Context, key types.NamespacedName) (client.Object, *notify.Message, error) {
	msg := &notify.Message{Cluster: r.ClusterName, Namespace: key.Namespace, Name: key.Name}
	if !r.Restores {
		b := &planbv1.Backup{}
		if err := r.Get(ctx, key, b); err != nil {
			return nil, nil, err
		}
		if !b.DeletionTimestamp.IsZero() || b.Annotations[planbv1.AnnotationSynced] == "true" {
			return b, nil, nil
		}
		switch b.Status.Phase {
		case planbv1.BackupPhaseCompleted:
			msg.Event = string(planbv1.EventBackupCompleted)
		case planbv1.BackupPhaseFailed:
			msg.Event = string(planbv1.EventBackupFailed)
		default:
			return b, nil, nil
		}
		msg.Kind, msg.Phase, msg.Message = "Backup", string(b.Status.Phase), b.Status.Message
		msg.Source, msg.StorageLocation = describeSource(&b.Spec), b.Spec.StorageLocation
		msg.Schedule = b.Labels[planbv1.LabelSchedule]
		msg.SizeBytes, msg.Items = b.Status.SizeBytes, b.Status.Items
		msg.StartTime, msg.CompletionTime = timeOf(b.Status.StartTimestamp), timeOf(b.Status.CompletionTimestamp)
		return b, msg, nil
	}
	rs := &planbv1.Restore{}
	if err := r.Get(ctx, key, rs); err != nil {
		return nil, nil, err
	}
	if !rs.DeletionTimestamp.IsZero() {
		return rs, nil, nil
	}
	switch rs.Status.Phase {
	case planbv1.RestorePhaseCompleted:
		msg.Event = string(planbv1.EventRestoreCompleted)
	case planbv1.RestorePhaseFailed, planbv1.RestorePhaseFailedValidation:
		msg.Event = string(planbv1.EventRestoreFailed)
	default:
		return rs, nil, nil
	}
	msg.Kind, msg.Phase, msg.Message = "Restore", string(rs.Status.Phase), rs.Status.Message
	msg.Backup = rs.Spec.BackupName
	b := &planbv1.Backup{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: rs.Spec.BackupName}, b); err == nil {
		msg.Source, msg.StorageLocation = describeSource(&b.Spec), b.Spec.StorageLocation
	}
	msg.SizeBytes, msg.Items = rs.Status.SizeBytes, rs.Status.Items
	msg.StartTime, msg.CompletionTime = timeOf(rs.Status.StartTimestamp), timeOf(rs.Status.CompletionTimestamp)
	return rs, msg, nil
}

func notifiedSet(obj client.Object) map[string]bool {
	set := map[string]bool{}
	for _, n := range strings.Split(obj.GetAnnotations()[planbv1.AnnotationNotified], ",") {
		if n != "" {
			set[n] = true
		}
	}
	return set
}

// wants reports whether a Notifier is interested in an event about an object with these labels.
func wants(n *planbv1.Notifier, ev planbv1.NotificationEvent, objLabels map[string]string) bool {
	if n.Spec.Suspend || !n.DeletionTimestamp.IsZero() {
		return false
	}
	evs := n.Spec.Events
	if len(evs) == 0 {
		evs = []planbv1.NotificationEvent{planbv1.EventBackupFailed, planbv1.EventRestoreFailed}
	}
	if !slices.Contains(evs, ev) {
		return false
	}
	if n.Spec.Selector == nil {
		return true
	}
	sel, err := metav1.LabelSelectorAsSelector(n.Spec.Selector)
	return err == nil && sel.Matches(labels.Set(objLabels))
}

// describeSource summarises what a backup reads from, e.g. "postgres · db.svc / app".
func describeSource(s *planbv1.BackupSpec) string {
	switch {
	case s.Postgres != nil:
		dbs := "(all databases)"
		if len(s.Postgres.Databases) > 0 {
			dbs = strings.Join(s.Postgres.Databases, ", ")
		}
		return fmt.Sprintf("postgres · %s / %s", s.Postgres.Host, dbs)
	case s.Bucket != nil:
		return fmt.Sprintf("bucket · %s/%s/%s", s.Bucket.Endpoint, s.Bucket.Bucket, s.Bucket.Prefix)
	}
	return ""
}

func timeOf(t *metav1.Time) *time.Time {
	if t == nil {
		return nil
	}
	return &t.Time
}

// SetupWithManager sets up the controller with the Manager.
func (r *NotificationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr)
	if r.Restores {
		b = b.For(&planbv1.Restore{}).Named("notify-restore")
	} else {
		b = b.For(&planbv1.Backup{}).Named("notify-backup")
	}
	return b.Complete(r)
}
