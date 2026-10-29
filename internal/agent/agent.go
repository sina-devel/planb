// Package agent executes backup and restore tasks inside Kubernetes Jobs.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/sina-devel/planb/internal/retry"
	"github.com/sina-devel/planb/internal/storage"
	"github.com/sina-devel/planb/internal/task"
)

// maxTerminationMessage is the Kubernetes limit for a container termination message.
const maxTerminationMessage = 4096

// Backoff between retries of a single S3 request inside minio-go (10 tries). Operations that still fail are
// retried by the task's retry policy on top.
const (
	requestRetryUnit = 500 * time.Millisecond
	requestRetryCap  = 15 * time.Second
)

// retryPolicy returns the task's retry policy with defaults filled in.
func retryPolicy(t *task.Task) retry.Policy {
	p := retry.Policy{Attempts: t.Retry.Attempts, InitialBackoff: t.Retry.InitialBackoff, MaxBackoff: t.Retry.MaxBackoff}
	if p.Attempts <= 0 {
		p.Attempts = retry.DefaultAttempts
	}
	if p.InitialBackoff <= 0 {
		p.InitialBackoff = retry.DefaultInitialBackoff
	}
	if p.MaxBackoff <= 0 {
		p.MaxBackoff = retry.DefaultMaxBackoff
	}
	return p
}

// retryOp runs fn under the task's retry policy.
func retryOp(ctx context.Context, t *task.Task, log *slog.Logger, what string, retryable func(error) bool, fn func() error) error {
	return retry.Do(ctx, retryPolicy(t), log, what, retryable, fn)
}

// toolError is a failed run of a PostgreSQL client tool.
type toolError struct {
	tool, stderr string
	err          error
}

func (e *toolError) Error() string { return fmt.Sprintf("%s failed: %v: %s", e.tool, e.err, e.stderr) }
func (e *toolError) Unwrap() error { return e.err }

// PostgreSQL client messages (lower case) for failures to open a connection. Nothing has been changed on the
// server yet, so the step can always run again.
var pgConnectErrors = []string{
	"connection refused", "connection timed out", "timeout expired", "connection reset",
	"could not translate host name", "name or service not known", "temporary failure in name resolution",
	"no route to host", "network is unreachable",
	"the database system is starting up", "the database system is shutting down",
	"the database system is in recovery mode", "the database system is not yet accepting connections",
	"too many clients", "remaining connection slots are reserved",
}

// PostgreSQL client messages (lower case) for a connection lost part way through.
var pgSessionErrors = []string{
	"server closed the connection unexpectedly", "connection reset by peer", "could not receive data from server",
	"could not send data to server", "ssl syscall error", "ssl connection has been closed unexpectedly",
	"terminating connection due to administrator command", "no connection to the server",
	"canceling statement due to conflict with recovery",
}

// transient returns the retry check for a step. S3 errors are judged by storage.IsRetryable. For PostgreSQL,
// connectOnly limits retries to failures to connect, for steps that are not safe to repeat once they started.
func transient(connectOnly bool) func(error) bool {
	return func(err error) bool {
		var te *toolError
		if !errors.As(err, &te) {
			return storage.IsRetryable(err)
		}
		s := strings.ToLower(te.stderr)
		connecting := strings.Contains(s, "could not connect to server") || strings.Contains(s, "could not translate host name") ||
			(strings.Contains(s, "connection to server") && strings.Contains(s, "failed:"))
		if connecting && containsAny(s, pgConnectErrors) {
			return true
		}
		return !connectOnly && containsAny(s, pgSessionErrors)
	}
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// Run reads the task from the environment, executes it and reports the result.
func Run(ctx context.Context) error {
	raw := os.Getenv(task.EnvTask)
	if raw == "" {
		return fmt.Errorf("%s is not set", task.EnvTask)
	}
	t, err := task.Decode(raw)
	if err != nil {
		return fmt.Errorf("decoding task: %w", err)
	}
	res, err := Execute(ctx, t)
	if res == nil {
		res = &task.Result{}
	}
	if err != nil {
		res.Error = err.Error()
	}
	report(res)
	return err
}

// Execute runs a task.
func Execute(ctx context.Context, t *task.Task) (*task.Result, error) {
	t.Storage.AccessKey = os.Getenv(task.EnvStorageAccess)
	t.Storage.SecretKey = os.Getenv(task.EnvStorageSecret)
	st, err := storage.New(t.Storage)
	if err != nil {
		return nil, err
	}
	if v, _ := strconv.Atoi(os.Getenv("PLANB_PART_SIZE_MB")); v > 0 {
		st.PartSize = uint64(v) << 20
	}
	log := slog.With("op", t.Op, "name", t.Name, "location", st.URL(t.BackupKey))
	storage.SetRequestBackoff(requestRetryUnit, requestRetryCap)
	p := retryPolicy(t)
	st.Retry, st.Log = &p, log
	if err := st.Check(ctx); err != nil {
		return nil, fmt.Errorf("backup storage: %w", err)
	}

	log.Info("starting", "retryAttempts", p.Attempts, "maxBackoff", p.MaxBackoff)
	var res *task.Result
	switch {
	case t.Postgres != nil && t.Op == task.OpBackup:
		res, err = backupPostgres(ctx, log, st, t)
	case t.Postgres != nil && t.Op == task.OpRestore:
		res, err = restorePostgres(ctx, log, st, t)
	case t.Bucket != nil && t.Op == task.OpBackup:
		res, err = backupBucket(ctx, log, st, t)
	case t.Bucket != nil && t.Op == task.OpRestore:
		res, err = restoreBucket(ctx, log, st, t)
	default:
		return nil, errors.New("task has neither postgres nor bucket section")
	}
	if err != nil {
		log.Error("failed", "error", err)
		return res, err
	}
	log.Info("finished", "items", res.Items, "bytes", res.SizeBytes)
	return res, nil
}

func report(res *task.Result) {
	b, _ := json.Marshal(res)
	for len(b) > maxTerminationMessage && len(res.Files) > 0 {
		res.Files = res.Files[:len(res.Files)/2]
		b, _ = json.Marshal(res)
	}
	if len(b) > maxTerminationMessage && len(res.Error) > 1024 {
		res.Error = res.Error[:1024] + "..."
		b, _ = json.Marshal(res)
	}
	fmt.Println("RESULT " + string(b))
	// Kubernetes creates the file; outside a pod there is nothing to write to.
	f, err := os.OpenFile(task.TerminationLogPath, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("could not write termination log", "error", err)
		}
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(b); err != nil {
		slog.Warn("could not write termination log", "error", err)
	}
}

// tailBuffer keeps the last n bytes written to it (used to capture stderr of child processes).
type tailBuffer struct {
	n   int
	buf bytes.Buffer
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	_, _ = os.Stderr.Write(p)
	t.buf.Write(p)
	if over := t.buf.Len() - t.n; over > 0 {
		t.buf.Next(over)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return strings.TrimSpace(t.buf.String()) }

// command builds an exec.Cmd for a PostgreSQL client tool with connection settings in the environment.
func command(ctx context.Context, pg *task.Postgres, name string, args ...string) (*exec.Cmd, *tailBuffer) {
	cmd := exec.CommandContext(ctx, name, args...)
	env := append(os.Environ(),
		"PGHOST="+pg.Host,
		"PGPORT="+strconv.Itoa(int(pg.Port)),
		"PGCONNECT_TIMEOUT=30",
	)
	if pg.SSLMode != "" {
		env = append(env, "PGSSLMODE="+pg.SSLMode)
	}
	cmd.Env = env
	stderr := &tailBuffer{n: 2048}
	cmd.Stderr = stderr
	return cmd, stderr
}

// streamOut runs cmd and uploads its stdout to key.
func streamOut(ctx context.Context, st *storage.Client, key string, cmd *exec.Cmd, stderr *tailBuffer, gz bool) (int64, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("starting %s: %w", cmd.Path, err)
	}
	var r io.Reader = stdout
	if gz {
		r = gzipReader(stdout)
	}
	size, upErr := st.Put(ctx, key, r, -1, "application/octet-stream")
	if upErr != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if upErr != nil {
		return size, upErr // the tool was killed because the upload failed
	}
	if waitErr != nil {
		return size, &toolError{tool: cmd.Args[0], stderr: stderr.String(), err: waitErr}
	}
	return size, nil
}

// streamIn downloads key and pipes it into cmd's stdin.
func streamIn(ctx context.Context, st *storage.Client, key string, cmd *exec.Cmd, stderr *tailBuffer, gz bool) (int64, error) {
	obj, size, _, err := st.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	defer func() { _ = obj.Close() }()
	var r io.Reader = obj
	if gz {
		gr, err := gunzipReader(obj)
		if err != nil {
			return 0, fmt.Errorf("decompressing %s: %w", key, err)
		}
		defer func() { _ = gr.Close() }()
		r = gr
	}
	cmd.Stdin = r
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if err := cmd.Run(); err != nil {
		return size, &toolError{tool: cmd.Args[0], stderr: stderr.String(), err: err}
	}
	return size, nil
}
