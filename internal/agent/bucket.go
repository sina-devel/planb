package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/sina-devel/planb/internal/storage"
	"github.com/sina-devel/planb/internal/task"
)

// deleteBatch is the number of keys removed per DeleteObjects request (the S3 maximum).
const deleteBatch = 1000

func bucketClient(t *task.Task, log *slog.Logger) (*storage.Client, error) {
	cfg := t.Bucket.S3
	cfg.AccessKey = os.Getenv(task.EnvBucketAccess)
	cfg.SecretKey = os.Getenv(task.EnvBucketSecret)
	c, err := storage.New(cfg)
	if err != nil {
		return nil, err
	}
	p := retryPolicy(t)
	c.Retry, c.Log = &p, log
	return c, nil
}

// ownBackups returns the key prefix holding planb's backups when c is the storage location's bucket, so that
// bucket backups and prunes leave them alone. It returns "" for any other bucket.
func ownBackups(st, c *storage.Client) string {
	if !c.SameBucket(st) {
		return ""
	}
	return task.BackupsKey(st.Prefix())
}

func backupBucket(ctx context.Context, log *slog.Logger, st *storage.Client, t *task.Task) (*task.Result, error) {
	src, err := bucketClient(t, log)
	if err != nil {
		return nil, err
	}
	if err := src.Check(ctx); err != nil {
		return nil, fmt.Errorf("source bucket: %w", err)
	}
	log.Info("copying objects", "from", src.URL(src.Prefix()))
	c := &copier{
		log: log, t: t, workers: t.Bucket.Concurrency, overwrite: true,
		src: src, srcPrefix: src.Prefix(), skip: ownBackups(st, src),
		dst: st, dstPrefix: t.BackupKey + task.BucketDir + "/",
	}
	return c.run(ctx)
}

func restoreBucket(ctx context.Context, log *slog.Logger, st *storage.Client, t *task.Task) (*task.Result, error) {
	dst, err := bucketClient(t, log)
	if err != nil {
		return nil, err
	}
	if err := dst.Check(ctx); err != nil {
		return nil, fmt.Errorf("target bucket: %w", err)
	}
	log.Info("copying objects", "to", dst.URL(dst.Prefix()))
	c := &copier{
		log: log, t: t, workers: t.Bucket.Concurrency, overwrite: t.Bucket.Overwrite,
		src: st, srcPrefix: t.BackupKey + task.BucketDir + "/",
		dst: dst, dstPrefix: dst.Prefix(),
	}
	if t.Bucket.Prune {
		c.keep = map[string]struct{}{}
	}
	res, err := c.run(ctx)
	if err != nil || !t.Bucket.Prune {
		return res, err
	}
	log.Info("pruning objects that are not in the backup", "prefix", dst.URL(dst.Prefix()))
	res.Deleted, err = prune(ctx, dst, dst.Prefix(), c.keep, ownBackups(st, dst))
	if res.Deleted > 0 {
		log.Info("pruned objects", "count", res.Deleted)
	}
	return res, err
}

// copier streams every object below srcPrefix to dstPrefix, keeping relative keys.
type copier struct {
	log                  *slog.Logger
	t                    *task.Task
	workers              int
	overwrite            bool
	src, dst             *storage.Client
	srcPrefix, dstPrefix string
	// skip, if set, is a source key prefix that is not copied.
	skip string
	// keep, if not nil, collects the relative key of every source object.
	keep map[string]struct{}
}

func (c *copier) run(ctx context.Context) (*task.Result, error) {
	workers := c.workers
	if workers <= 0 {
		workers = 4
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		items, size, skipped atomic.Int64
		once                 sync.Once
		firstErr             error
		wg                   sync.WaitGroup
	)
	fail := func(err error) {
		once.Do(func() { firstErr = err; cancel() })
	}
	jobs := make(chan storage.Object)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for o := range jobs {
				dstKey := c.dstPrefix + strings.TrimPrefix(o.Key, c.srcPrefix)
				if !c.overwrite {
					exists, err := c.dst.Exists(ctx, dstKey)
					if err != nil {
						fail(err)
						continue
					}
					if exists {
						skipped.Add(1)
						continue
					}
				}
				err := retryOp(ctx, c.t, c.log, "copy "+c.src.URL(o.Key), storage.IsRetryable, func() error {
					return copyOne(ctx, c.src, o.Key, c.dst, dstKey)
				})
				if err != nil {
					fail(err)
					continue
				}
				n := items.Add(1)
				size.Add(o.Size)
				if n%1000 == 0 {
					c.log.Info("progress", "objects", n, "bytes", size.Load())
				}
			}
		}()
	}
	listErr := c.src.List(ctx, c.srcPrefix, func(o storage.Object) error {
		if c.skip != "" && strings.HasPrefix(o.Key, c.skip) {
			return nil
		}
		if c.keep != nil {
			c.keep[strings.TrimPrefix(o.Key, c.srcPrefix)] = struct{}{}
		}
		select {
		case jobs <- o:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	close(jobs)
	wg.Wait()
	res := &task.Result{Items: items.Load(), SizeBytes: size.Load()}
	if skipped.Load() > 0 {
		c.log.Info("skipped existing objects", "count", skipped.Load())
	}
	if firstErr != nil {
		return res, firstErr
	}
	return res, listErr
}

func copyOne(ctx context.Context, src *storage.Client, srcKey string, dst *storage.Client, dstKey string) error {
	r, size, ct, err := src.Get(ctx, srcKey)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	_, err = dst.Put(ctx, dstKey, r, size, ct)
	return err
}

// prune deletes objects below prefix whose relative key is not in keep, leaving keys below protect alone.
func prune(ctx context.Context, c *storage.Client, prefix string, keep map[string]struct{}, protect string) (int64, error) {
	var (
		deleted int64
		batch   []string
	)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := c.DeleteKeys(ctx, batch); err != nil {
			return err
		}
		deleted += int64(len(batch))
		batch = batch[:0]
		return nil
	}
	err := c.List(ctx, prefix, func(o storage.Object) error {
		if protect != "" && strings.HasPrefix(o.Key, protect) {
			return nil
		}
		if _, ok := keep[strings.TrimPrefix(o.Key, prefix)]; ok {
			return nil
		}
		batch = append(batch, o.Key)
		if len(batch) == deleteBatch {
			return flush()
		}
		return nil
	})
	if err == nil {
		err = flush()
	}
	return deleted, err
}
