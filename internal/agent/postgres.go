package agent

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"

	"github.com/sina-devel/planb/internal/storage"
	"github.com/sina-devel/planb/internal/task"
)

func backupPostgres(ctx context.Context, log *slog.Logger, st *storage.Client, t *task.Task) (*task.Result, error) {
	pg := t.Postgres
	res := &task.Result{}
	upload := func(file string, gz bool, name string, args ...string) error {
		key := t.BackupKey + path.Join(task.PostgresDir, file)
		log.Info("dumping", "tool", name, "file", file)
		cmd, stderr := command(ctx, pg, name, args...)
		size, err := streamOut(ctx, st, key, cmd, stderr, gz)
		if err != nil {
			return err
		}
		res.SizeBytes += size
		res.Items++
		res.Files = append(res.Files, path.Join(task.PostgresDir, file))
		log.Info("uploaded", "file", file, "bytes", size)
		return nil
	}

	if len(pg.Databases) == 0 {
		return res, upload(task.ClusterDump, true, "pg_dumpall", pg.ExtraArgs...)
	}
	if pg.IncludeGlobals {
		if err := upload(task.GlobalsDump, true, "pg_dumpall", "--globals-only"); err != nil {
			return res, err
		}
	}
	for _, db := range pg.Databases {
		args := append([]string{"--format=custom", "--dbname=" + db}, pg.ExtraArgs...)
		if err := upload(db+task.DatabaseSuffix, false, "pg_dump", args...); err != nil {
			return res, err
		}
	}
	return res, nil
}

func restorePostgres(ctx context.Context, log *slog.Logger, st *storage.Client, t *task.Task) (*task.Result, error) {
	pg := t.Postgres
	res := &task.Result{}
	prefix := t.BackupKey + task.PostgresDir + "/"

	var files []string
	if err := st.List(ctx, prefix, func(o storage.Object) error {
		files = append(files, strings.TrimPrefix(o.Key, prefix))
		return nil
	}); err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("backup contains no postgres dumps under %s", st.URL(prefix))
	}
	sort.Strings(files)
	has := func(f string) bool {
		for _, x := range files {
			if x == f {
				return true
			}
		}
		return false
	}
	psqlFile := func(file string) error {
		log.Info("replaying SQL", "file", file)
		args := append([]string{"--dbname=postgres", "--quiet"}, pg.ExtraArgs...)
		cmd, stderr := command(ctx, pg, "psql", args...)
		size, err := streamIn(ctx, st, prefix+file, cmd, stderr, true)
		if err != nil {
			return err
		}
		res.SizeBytes += size
		res.Items++
		return nil
	}

	// Whole-cluster dump: replay it as-is.
	if has(task.ClusterDump) {
		if len(pg.Databases) > 0 || len(pg.DatabaseMapping) > 0 {
			return nil, fmt.Errorf("backup is a full pg_dumpall dump: databases/databaseMapping are not supported")
		}
		return res, psqlFile(task.ClusterDump)
	}

	if pg.RestoreGlobals && has(task.GlobalsDump) {
		if err := psqlFile(task.GlobalsDump); err != nil {
			return res, err
		}
	}

	wanted := map[string]bool{}
	for _, db := range pg.Databases {
		wanted[db] = true
	}
	restored := 0
	for _, f := range files {
		if !strings.HasSuffix(f, task.DatabaseSuffix) {
			continue
		}
		src := strings.TrimSuffix(f, task.DatabaseSuffix)
		if len(wanted) > 0 && !wanted[src] {
			continue
		}
		delete(wanted, src)
		target := src
		if m, ok := pg.DatabaseMapping[src]; ok && m != "" {
			target = m
		}
		if pg.CreateDatabase {
			if err := ensureDatabase(ctx, log, pg, target); err != nil {
				return res, err
			}
		}
		log.Info("restoring database", "source", src, "target", target)
		args := []string{"--dbname=" + target, "--exit-on-error"}
		if pg.Clean {
			args = append(args, "--clean", "--if-exists")
		}
		args = append(args, pg.ExtraArgs...)
		cmd, stderr := command(ctx, pg, "pg_restore", args...)
		size, err := streamIn(ctx, st, prefix+f, cmd, stderr, false)
		if err != nil {
			return res, err
		}
		res.SizeBytes += size
		res.Items++
		res.Files = append(res.Files, target)
		restored++
	}
	if len(wanted) > 0 {
		missing := make([]string, 0, len(wanted))
		for db := range wanted {
			missing = append(missing, db)
		}
		sort.Strings(missing)
		return res, fmt.Errorf("databases not found in backup: %s", strings.Join(missing, ", "))
	}
	if restored == 0 && res.Items == 0 {
		return res, fmt.Errorf("nothing to restore")
	}
	return res, nil
}

func ensureDatabase(ctx context.Context, log *slog.Logger, pg *task.Postgres, name string) error {
	cmd, stderr := command(ctx, pg, "psql", "--dbname=postgres", "--tuples-only", "--no-align",
		"--command=SELECT 1 FROM pg_database WHERE datname = "+quoteLiteral(name))
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("checking database %q: %w: %s", name, err, stderr.String())
	}
	if strings.TrimSpace(string(out)) == "1" {
		return nil
	}
	log.Info("creating database", "name", name)
	cmd, stderr = command(ctx, pg, "psql", "--dbname=postgres", "--command=CREATE DATABASE "+quoteIdent(name))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("creating database %q: %w: %s", name, err, stderr.String())
	}
	return nil
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func quoteLiteral(s string) string { return `'` + strings.ReplaceAll(s, `'`, `''`) + `'` }
