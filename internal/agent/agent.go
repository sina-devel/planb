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

	"github.com/sina-devel/planb/internal/storage"
	"github.com/sina-devel/planb/internal/task"
)

// maxTerminationMessage is the Kubernetes limit for a container termination message.
const maxTerminationMessage = 4096

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
	if err := st.Check(ctx); err != nil {
		return nil, fmt.Errorf("backup storage: %w", err)
	}

	log := slog.With("op", t.Op, "name", t.Name, "location", st.URL(t.BackupKey))
	log.Info("starting")
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
	if waitErr != nil {
		return size, fmt.Errorf("%s failed: %w: %s", cmd.Args[0], waitErr, stderr.String())
	}
	return size, upErr
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
		return size, fmt.Errorf("%s failed: %w: %s", cmd.Args[0], err, stderr.String())
	}
	return size, nil
}
