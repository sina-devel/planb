// Command planb is a Kubernetes backup & restore operator for PostgreSQL and S3 buckets.
//
//	planb manager   run the operator
//	planb agent     run a backup/restore task (used inside Jobs)
//	planb ui        serve the optional web UI
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	planbv1 "github.com/sina-devel/planb/api/v1alpha1"
	"github.com/sina-devel/planb/internal/agent"
	"github.com/sina-devel/planb/internal/controller"
	"github.com/sina-devel/planb/internal/ui"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "manager":
		err = runManager(os.Args[2:])
	case "agent":
		err = runAgent()
	case "ui":
		err = runUI(os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		slog.Error("exiting", "error", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: planb <manager|agent|ui|version> [flags]")
}

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(planbv1.AddToScheme(s))
	return s
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func runManager(args []string) error {
	fs := flag.NewFlagSet("manager", flag.ExitOnError)
	metricsAddr := fs.String("metrics-bind-address", ":8080", "metrics endpoint address (0 disables)")
	probeAddr := fs.String("health-probe-bind-address", ":8081", "health probe address")
	leaderElect := fs.Bool("leader-elect", false, "enable leader election")
	watchNamespace := fs.String("watch-namespace", os.Getenv("WATCH_NAMESPACE"), "only watch this namespace (default: all)")
	agentImage := fs.String("agent-image", os.Getenv("PLANB_AGENT_IMAGE"), "image used for backup/restore jobs")
	pullPolicy := fs.String("agent-image-pull-policy", envOr("PLANB_AGENT_IMAGE_PULL_POLICY", "IfNotPresent"), "pull policy for job images")
	defaultTTL := fs.Duration("default-backup-ttl", 720*time.Hour, "TTL for backups without spec.ttl (0 = keep forever)")
	jobTTL := fs.Int("job-ttl-seconds", 86400, "keep finished jobs for this many seconds")
	clusterName := fs.String("cluster-name", os.Getenv("PLANB_CLUSTER_NAME"), "cluster name shown in notifications")
	opts := zap.Options{}
	opts.BindFlags(fs)
	_ = fs.Parse(args)
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if *agentImage == "" {
		return fmt.Errorf("--agent-image (or PLANB_AGENT_IMAGE) is required")
	}

	scheme := newScheme()
	cacheOpts := cache.Options{
		// Only cache the Jobs we create.
		ByObject: map[client.Object]cache.ByObject{
			&batchv1.Job{}: {Label: labels.SelectorFromSet(labels.Set{planbv1.LabelManagedBy: "planb"})},
		},
	}
	if *watchNamespace != "" {
		cacheOpts.DefaultNamespaces = map[string]cache.Config{*watchNamespace: {}}
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress: *probeAddr,
		LeaderElection:         *leaderElect,
		LeaderElectionID:       "planb.planb.io",
		Cache:                  cacheOpts,
	})
	if err != nil {
		return err
	}

	o := controller.Options{
		AgentImage:                 *agentImage,
		AgentImagePullPolicy:       corev1.PullPolicy(*pullPolicy),
		DefaultTTL:                 *defaultTTL,
		JobTTLSecondsAfterFinished: int32(*jobTTL),
	}
	if err := (&controller.BackupReconciler{
		Client: mgr.GetClient(), Scheme: scheme, APIReader: mgr.GetAPIReader(),
		Recorder: mgr.GetEventRecorder("planb-backup"), Options: o,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := (&controller.RestoreReconciler{
		Client: mgr.GetClient(), Scheme: scheme, APIReader: mgr.GetAPIReader(),
		Recorder: mgr.GetEventRecorder("planb-restore"), Options: o,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := (&controller.ScheduleReconciler{
		Client: mgr.GetClient(), Scheme: scheme, Recorder: mgr.GetEventRecorder("planb-schedule"),
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := (&controller.StorageLocationReconciler{
		Client: mgr.GetClient(), Scheme: scheme, APIReader: mgr.GetAPIReader(),
		Recorder: mgr.GetEventRecorder("planb-storagelocation"),
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := (&controller.NotifierReconciler{
		Client: mgr.GetClient(), Scheme: scheme, APIReader: mgr.GetAPIReader(),
		Recorder: mgr.GetEventRecorder("planb-notifier"), ClusterName: *clusterName,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	for _, restores := range []bool{false, true} {
		if err := (&controller.NotificationReconciler{
			Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(),
			Recorder: mgr.GetEventRecorder("planb-notifier"), ClusterName: *clusterName, Restores: restores,
		}).SetupWithManager(mgr); err != nil {
			return err
		}
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	ctrl.Log.Info("starting manager", "version", version, "agentImage", *agentImage, "watchNamespace", *watchNamespace)
	return mgr.Start(ctrl.SetupSignalHandler())
}

func runAgent() error {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return agent.Run(ctx)
}

func runUI(args []string) error {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	addr := fs.String("bind-address", envOr("PLANB_UI_ADDR", ":8090"), "listen address")
	ns := fs.String("namespace", os.Getenv("PLANB_UI_NAMESPACE"), "restrict the UI to one namespace")
	readOnly := fs.Bool("read-only", os.Getenv("PLANB_UI_READ_ONLY") == "true", "disable all mutating actions")
	_ = fs.Parse(args)

	cfg := ctrl.GetConfigOrDie()
	c, err := client.New(cfg, client.Options{Scheme: newScheme()})
	if err != nil {
		return err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	srv := &ui.Server{
		Client:    c,
		Clientset: cs,
		Namespace: *ns,
		ReadOnly:  *readOnly,
		Username:  os.Getenv("PLANB_UI_USERNAME"),
		Password:  os.Getenv("PLANB_UI_PASSWORD"),
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return srv.ListenAndServe(ctx, *addr)
}
