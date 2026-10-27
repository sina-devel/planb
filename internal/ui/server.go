// Package ui serves the optional planb web UI and its JSON API.
package ui

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	planbv1 "github.com/sina-devel/planb/api/v1alpha1"
	"github.com/sina-devel/planb/internal/controller"
)

//go:embed static
var static embed.FS

// Server is the UI HTTP server.
type Server struct {
	Client    client.Client
	Clientset kubernetes.Interface
	// Namespace restricts the UI to one namespace ("" = all namespaces).
	Namespace string
	ReadOnly  bool
	Username  string
	Password  string
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(static, "static")
	mux.Handle("GET /", http.FileServerFS(sub))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /api/config", s.config)
	mux.HandleFunc("GET /api/overview", s.overview)
	mux.HandleFunc("GET /api/logs/{ns}/{job}", s.logs)
	mux.HandleFunc("POST /api/backups", s.mutating(s.createBackup))
	mux.HandleFunc("DELETE /api/backups/{ns}/{name}", s.mutating(s.deleteBackup))
	mux.HandleFunc("POST /api/restores", s.mutating(s.createRestore))
	mux.HandleFunc("DELETE /api/restores/{ns}/{name}", s.mutating(s.deleteRestore))
	mux.HandleFunc("POST /api/schedules", s.mutating(s.createSchedule))
	mux.HandleFunc("DELETE /api/schedules/{ns}/{name}", s.mutating(s.deleteSchedule))
	mux.HandleFunc("POST /api/schedules/{ns}/{name}/{action}", s.mutating(s.toggleSchedule))
	mux.HandleFunc("POST /api/storagelocations", s.mutating(s.createStorageLocation))
	mux.HandleFunc("DELETE /api/storagelocations/{ns}/{name}", s.mutating(s.deleteStorageLocation))
	mux.HandleFunc("POST /api/notifiers", s.mutating(s.createNotifier))
	mux.HandleFunc("DELETE /api/notifiers/{ns}/{name}", s.mutating(s.deleteNotifier))
	mux.HandleFunc("POST /api/notifiers/{ns}/{name}/test", s.mutating(s.testNotifier))
	return s.auth(mux)
}

func (s *Server) auth(next http.Handler) http.Handler {
	if s.Username == "" && s.Password == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		u, p, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(s.Username)) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(s.Password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="planb"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) mutating(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.ReadOnly {
			writeErr(w, http.StatusForbidden, fmt.Errorf("the UI is in read-only mode"))
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// namespace resolves the namespace of a request, enforcing the configured restriction.
func (s *Server) namespace(ns string) (string, error) {
	if s.Namespace != "" {
		if ns != "" && ns != s.Namespace {
			return "", fmt.Errorf("namespace %q is not allowed", ns)
		}
		return s.Namespace, nil
	}
	return ns, nil
}

func (s *Server) config(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"readOnly": s.ReadOnly, "namespace": s.Namespace})
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ns, err := s.namespace(r.URL.Query().Get("namespace"))
	if err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	ctx := r.Context()
	opts := []client.ListOption{}
	if ns != "" {
		opts = append(opts, client.InNamespace(ns))
	}
	var (
		bsls      planbv1.BackupStorageLocationList
		backups   planbv1.BackupList
		schedules planbv1.ScheduleList
		restores  planbv1.RestoreList
		notifiers planbv1.NotifierList
	)
	for _, l := range []client.ObjectList{&bsls, &backups, &schedules, &restores, &notifiers} {
		if err := s.Client.List(ctx, l, opts...); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	sort.Slice(backups.Items, func(i, j int) bool {
		return backups.Items[i].CreationTimestamp.After(backups.Items[j].CreationTimestamp.Time)
	})
	sort.Slice(restores.Items, func(i, j int) bool {
		return restores.Items[i].CreationTimestamp.After(restores.Items[j].CreationTimestamp.Time)
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"storageLocations": bsls.Items,
		"backups":          backups.Items,
		"schedules":        schedules.Items,
		"restores":         restores.Items,
		"notifiers":        notifiers.Items,
	})
}

// secretRequest is a credentials Secret created together with the object that references it.
type secretRequest struct {
	Name string            `json:"name"`
	Data map[string]string `json:"data"`
}

// createMeta is common to all create requests.
type createMeta struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name,omitempty"`
	// Secrets are created before the object. Existing Secrets are never overwritten.
	Secrets []secretRequest `json:"secrets,omitempty"`
}

func (m *createMeta) meta() *createMeta { return m }

// decodeCreate decodes a create request, resolves its namespace and creates the requested Secrets.
func (s *Server) decodeCreate(w http.ResponseWriter, r *http.Request, req interface{ meta() *createMeta }) (string, bool) {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return "", false
	}
	m := req.meta()
	ns, err := s.namespace(m.Namespace)
	if err != nil || ns == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("a valid namespace is required"))
		return "", false
	}
	for _, sr := range m.Secrets {
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: sr.Name,
				Labels: map[string]string{planbv1.LabelManagedBy: "planb-ui"}},
			StringData: sr.Data,
		}
		if err := s.Client.Create(r.Context(), sec); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("creating secret %s: %w", sr.Name, err))
			return "", false
		}
	}
	return ns, true
}

type createBackupRequest struct {
	createMeta
	// Schedule creates the backup from a schedule's template ("run now").
	Schedule string              `json:"schedule,omitempty"`
	Spec     *planbv1.BackupSpec `json:"spec,omitempty"`
}

func (s *Server) createBackup(w http.ResponseWriter, r *http.Request) {
	req := &createBackupRequest{}
	ns, ok := s.decodeCreate(w, r, req)
	if !ok {
		return
	}
	var b *planbv1.Backup
	switch {
	case req.Schedule != "":
		sched := &planbv1.Schedule{}
		if err := s.Client.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: req.Schedule}, sched); err != nil {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		b = controller.NewBackupFromSchedule(sched, time.Now())
	case req.Spec != nil:
		b = &planbv1.Backup{Spec: *req.Spec}
		b.Namespace = ns
	default:
		writeErr(w, http.StatusBadRequest, fmt.Errorf("either schedule or spec is required"))
		return
	}
	if req.Name != "" {
		b.Name = req.Name
	}
	if b.Name == "" {
		b.GenerateName = "manual-"
	}
	if err := s.Client.Create(r.Context(), b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request) {
	s.deleteObject(w, r, &planbv1.Backup{})
}

func (s *Server) deleteRestore(w http.ResponseWriter, r *http.Request) {
	s.deleteObject(w, r, &planbv1.Restore{})
}

func (s *Server) deleteObject(w http.ResponseWriter, r *http.Request, obj client.Object) {
	ns, err := s.namespace(r.PathValue("ns"))
	if err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	obj.SetNamespace(ns)
	obj.SetName(r.PathValue("name"))
	if err := s.Client.Delete(r.Context(), obj); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type createRestoreRequest struct {
	createMeta
	Spec planbv1.RestoreSpec `json:"spec"`
}

func (s *Server) createRestore(w http.ResponseWriter, r *http.Request) {
	req := &createRestoreRequest{}
	ns, ok := s.decodeCreate(w, r, req)
	if !ok {
		return
	}
	rs := &planbv1.Restore{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: req.Name}, Spec: req.Spec}
	if rs.Name == "" {
		rs.GenerateName = req.Spec.BackupName + "-"
	}
	if err := s.Client.Create(r.Context(), rs); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, rs)
}

type createScheduleRequest struct {
	createMeta
	Spec planbv1.ScheduleSpec `json:"spec"`
}

func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request) {
	req := &createScheduleRequest{}
	ns, ok := s.decodeCreate(w, r, req)
	if !ok {
		return
	}
	if _, err := controller.ParseSchedule(req.Spec.Schedule); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid schedule: %w", err))
		return
	}
	sched := &planbv1.Schedule{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: req.Name}, Spec: req.Spec}
	if err := s.Client.Create(r.Context(), sched); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, sched)
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	s.deleteObject(w, r, &planbv1.Schedule{})
}

type createStorageLocationRequest struct {
	createMeta
	Spec planbv1.BackupStorageLocationSpec `json:"spec"`
}

func (s *Server) createStorageLocation(w http.ResponseWriter, r *http.Request) {
	req := &createStorageLocationRequest{}
	ns, ok := s.decodeCreate(w, r, req)
	if !ok {
		return
	}
	bsl := &planbv1.BackupStorageLocation{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: req.Name}, Spec: req.Spec}
	if err := s.Client.Create(r.Context(), bsl); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, bsl)
}

func (s *Server) deleteStorageLocation(w http.ResponseWriter, r *http.Request) {
	s.deleteObject(w, r, &planbv1.BackupStorageLocation{})
}

type createNotifierRequest struct {
	createMeta
	Spec planbv1.NotifierSpec `json:"spec"`
}

func (s *Server) createNotifier(w http.ResponseWriter, r *http.Request) {
	req := &createNotifierRequest{}
	ns, ok := s.decodeCreate(w, r, req)
	if !ok {
		return
	}
	n := &planbv1.Notifier{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: req.Name}, Spec: req.Spec}
	if err := s.Client.Create(r.Context(), n); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, n)
}

func (s *Server) deleteNotifier(w http.ResponseWriter, r *http.Request) {
	s.deleteObject(w, r, &planbv1.Notifier{})
}

// testNotifier asks the operator to send a test message by changing the planb.io/test annotation.
func (s *Server) testNotifier(w http.ResponseWriter, r *http.Request) {
	ns, err := s.namespace(r.PathValue("ns"))
	if err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	n := &planbv1.Notifier{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: r.PathValue("name")}}
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, planbv1.AnnotationTest, time.Now().UTC().Format(time.RFC3339Nano))
	if err := s.Client.Patch(r.Context(), n, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, n)
}

func (s *Server) toggleSchedule(w http.ResponseWriter, r *http.Request) {
	ns, err := s.namespace(r.PathValue("ns"))
	if err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	var paused bool
	switch r.PathValue("action") {
	case "pause":
		paused = true
	case "resume":
	default:
		writeErr(w, http.StatusNotFound, fmt.Errorf("unknown action"))
		return
	}
	sched := &planbv1.Schedule{}
	if err := s.Client.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: r.PathValue("name")}, sched); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	patch := client.MergeFrom(sched.DeepCopy())
	sched.Spec.Paused = paused
	if err := s.Client.Patch(r.Context(), sched, patch); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, sched)
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	ns, err := s.namespace(r.PathValue("ns"))
	if err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	text, err := s.jobLogs(r.Context(), ns, r.PathValue("job"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, text)
}

func (s *Server) jobLogs(ctx context.Context, ns, name string) (string, error) {
	job := &batchv1.Job{}
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, job); err != nil {
		return "", err
	}
	if job.Labels[planbv1.LabelManagedBy] != "planb" {
		return "", fmt.Errorf("job %s is not managed by planb", name)
	}
	sel, err := metav1.LabelSelectorAsSelector(job.Spec.Selector)
	if err != nil {
		return "", err
	}
	pods := &corev1.PodList{}
	if err := s.Client.List(ctx, pods, client.InNamespace(ns), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no pods found for job %s (it may have been cleaned up)", name)
	}
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[i].CreationTimestamp.After(pods.Items[j].CreationTimestamp.Time)
	})
	raw, err := s.Clientset.CoreV1().Pods(ns).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{
		Container: "agent", TailLines: ptr.To[int64](2000),
	}).DoRaw(ctx)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// ListenAndServe runs the server until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	slog.Info("ui listening", "addr", addr, "readOnly", s.ReadOnly, "namespace", s.Namespace)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
