// Package controller contains the planb reconcilers.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	planbv1 "github.com/sina-devel/planb/api/v1alpha1"
	"github.com/sina-devel/planb/internal/storage"
	"github.com/sina-devel/planb/internal/task"
)

// Options shared by the reconcilers.
type Options struct {
	// AgentImage runs backup/restore Jobs.
	AgentImage string
	// AgentImagePullPolicy for Jobs.
	AgentImagePullPolicy corev1.PullPolicy
	// DefaultTTL for Backups without spec.ttl.
	DefaultTTL time.Duration
	// JobTTLSecondsAfterFinished keeps finished Jobs (and their logs) around for this long.
	JobTTLSecondsAfterFinished int32
}

// SecretReader reads secrets without caching them in the manager.
type SecretReader interface {
	Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error
	List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error
}

func secretValue(ctx context.Context, r SecretReader, ns, name, key string) (string, error) {
	s := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, s); err != nil {
		return "", fmt.Errorf("reading secret %s/%s: %w", ns, name, err)
	}
	v, ok := s.Data[key]
	if !ok {
		return "", fmt.Errorf("secret %s/%s has no key %q", ns, name, key)
	}
	return string(v), nil
}

func storageConfig(s *planbv1.S3Spec) storage.Config {
	return storage.Config{
		Endpoint:      s.Endpoint,
		Bucket:        s.Bucket,
		Prefix:        s.Prefix,
		Region:        s.Region,
		Insecure:      s.Insecure,
		SkipTLSVerify: s.SkipTLSVerify,
	}
}

// storageClient builds an S3 client for a storage location, reading its credentials.
func storageClient(ctx context.Context, r SecretReader, bsl *planbv1.BackupStorageLocation) (*storage.Client, error) {
	cfg := storageConfig(&bsl.Spec.S3)
	if c := bsl.Spec.S3.Credentials; c != nil {
		var err error
		if cfg.AccessKey, err = secretValue(ctx, r, bsl.Namespace, c.Name, defaultStr(c.AccessKeyIDKey, "accessKey")); err != nil {
			return nil, err
		}
		if cfg.SecretKey, err = secretValue(ctx, r, bsl.Namespace, c.Name, defaultStr(c.SecretAccessKeyKey, "secretKey")); err != nil {
			return nil, err
		}
	}
	return storage.New(cfg)
}

func defaultStr(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// jobName returns a DNS-label-safe job name (max 63 chars).
func jobName(prefix, name string) string {
	n := prefix + "-" + name
	if len(n) <= 63 {
		return n
	}
	sum := sha256.Sum256([]byte(name))
	return n[:54] + "-" + hex.EncodeToString(sum[:])[:8]
}

func secretEnv(env, secret, key string) corev1.EnvVar {
	return corev1.EnvVar{Name: env, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key,
	}}}
}

func s3CredEnv(c *planbv1.S3Credentials, accessEnv, secretEnvName string) []corev1.EnvVar {
	if c == nil {
		return nil
	}
	return []corev1.EnvVar{
		secretEnv(accessEnv, c.Name, defaultStr(c.AccessKeyIDKey, "accessKey")),
		secretEnv(secretEnvName, c.Name, defaultStr(c.SecretAccessKeyKey, "secretKey")),
	}
}

func pgCredEnv(c planbv1.PostgresCredentials) []corev1.EnvVar {
	return []corev1.EnvVar{
		secretEnv(task.EnvPGUser, c.Name, defaultStr(c.UsernameKey, "username")),
		secretEnv(task.EnvPGPassword, c.Name, defaultStr(c.PasswordKey, "password")),
	}
}

// buildJob creates the Job running the agent for a task.
func (o *Options) buildJob(owner client.Object, name string, labels map[string]string, t *task.Task, env []corev1.EnvVar, opts *planbv1.JobOptions) (*batchv1.Job, error) {
	if opts == nil {
		opts = &planbv1.JobOptions{}
	}
	if rp := opts.Retry; rp != nil {
		t.Retry = task.Retry{Attempts: int(ptr.Deref(rp.Attempts, 0))}
		if rp.InitialBackoff != nil {
			t.Retry.InitialBackoff = rp.InitialBackoff.Duration
		}
		if rp.MaxBackoff != nil {
			t.Retry.MaxBackoff = rp.MaxBackoff.Duration
		}
	}
	encoded, err := t.Encode()
	if err != nil {
		return nil, err
	}
	image := o.AgentImage
	if opts.Image != "" {
		image = opts.Image
	}
	labels[planbv1.LabelManagedBy] = "planb"
	container := corev1.Container{
		Name:                     "agent",
		Image:                    image,
		ImagePullPolicy:          o.AgentImagePullPolicy,
		Args:                     []string{"agent"},
		Env:                      append([]corev1.EnvVar{{Name: task.EnvTask, Value: encoded}}, env...),
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		VolumeMounts:             []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			RunAsNonRoot:             ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
	if opts.Resources != nil {
		container.Resources = *opts.Resources
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: owner.GetNamespace(), Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(ptr.Deref(opts.BackoffLimit, 1)),
			ActiveDeadlineSeconds:   opts.ActiveDeadlineSeconds,
			TTLSecondsAfterFinished: ptr.To(o.JobTTLSecondsAfterFinished),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: opts.ServiceAccountName,
					NodeSelector:       opts.NodeSelector,
					Tolerations:        opts.Tolerations,
					Containers:         []corev1.Container{container},
					Volumes: []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					}}},
					SecurityContext: &corev1.PodSecurityContext{
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
				},
			},
		},
	}
	gvk := owner.GetObjectKind().GroupVersionKind()
	job.OwnerReferences = []metav1.OwnerReference{{
		APIVersion:         planbv1.GroupVersion.String(),
		Kind:               gvk.Kind,
		Name:               owner.GetName(),
		UID:                owner.GetUID(),
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}}
	return job, nil
}

type jobState int

const (
	jobRunning jobState = iota
	jobSucceeded
	jobFailed
)

func stateOf(job *batchv1.Job) (jobState, string) {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return jobSucceeded, ""
		case batchv1.JobFailed:
			return jobFailed, c.Reason + ": " + c.Message
		}
	}
	return jobRunning, ""
}

// jobResult reads the agent result from the most recent pod's termination message.
func jobResult(ctx context.Context, r SecretReader, job *batchv1.Job) (*task.Result, error) {
	sel, err := metav1.LabelSelectorAsSelector(job.Spec.Selector)
	if err != nil {
		return nil, err
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(job.Namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, err
	}
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[i].CreationTimestamp.After(pods.Items[j].CreationTimestamp.Time)
	})
	for _, p := range pods.Items {
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name != "agent" || cs.State.Terminated == nil {
				continue
			}
			msg := cs.State.Terminated.Message
			res := &task.Result{}
			if err := json.Unmarshal([]byte(msg), res); err != nil {
				// FallbackToLogsOnError: the message is the log tail.
				res.Error = tail(msg, 1024)
			}
			return res, nil
		}
	}
	return nil, fmt.Errorf("no terminated agent pod found for job %s", job.Name)
}

// emit records an event; notes are truncated to the events.k8s.io/v1 limit of 1KB.
func emit(rec events.EventRecorder, obj runtime.Object, eventType, reason, action, note string) {
	if len(note) > 1000 {
		note = note[:1000] + "..."
	}
	rec.Eventf(obj, nil, eventType, reason, action, "%s", note)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

func now() *metav1.Time {
	t := metav1.Now()
	return &t
}
