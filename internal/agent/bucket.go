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

func bucketClient(t *task.Task) (*storage.Client, error) {
	cfg := t.Bucket.S3
	cfg.AccessKey = os.Getenv(task.EnvBucketAccess)
	cfg.SecretKey = os.Getenv(task.EnvBucketSecret)
	return storage.New(cfg)
}

func backupBucket(ctx context.Context, log *slog.Logger, st *storage.Client, t *task.Task) (*task.Result, error) {
	src, err := bucketClient(t)
	if err != nil {
		return nil, err
	}
	if err := src.Check(ctx); err != nil {
		return nil, fmt.Errorf("source bucket: %w", err)
	}
	log.Info("copying objects", "from", src.URL(src.Prefix()))
	return copyObjects(ctx, t.Bucket.Concurrency, src, src.Prefix(), st, t.BackupKey+task.BucketDir+"/", true)
}

func restoreBucket(ctx context.Context, log *slog.Logger, st *storage.Client, t *task.Task) (*task.Result, error) {
	dst, err := bucketClient(t)
	if err != nil {
		return nil, err
	}
	if err := dst.Check(ctx); err != nil {
		return nil, fmt.Errorf("target bucket: %w", err)
	}
	log.Info("copying objects", "to", dst.URL(dst.Prefix()))
	return copyObjects(ctx, t.Bucket.Concurrency, st, t.BackupKey+task.BucketDir+"/", dst, dst.Prefix(), t.Bucket.Overwrite)
}

// copyObjects streams every object below srcPrefix to dstPrefix, keeping relative keys.
func copyObjects(ctx context.Context, workers int, src *storage.Client, srcPrefix string, dst *storage.Client, dstPrefix string, overwrite bool) (*task.Result, error) {
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
				dstKey := dstPrefix + strings.TrimPrefix(o.Key, srcPrefix)
				if !overwrite {
					exists, err := dst.Exists(ctx, dstKey)
					if err != nil {
						fail(err)
						continue
					}
					if exists {
						skipped.Add(1)
						continue
					}
				}
				if err := copyOne(ctx, src, o.Key, dst, dstKey); err != nil {
					fail(err)
					continue
				}
				n := items.Add(1)
				size.Add(o.Size)
				if n%1000 == 0 {
					slog.Info("progress", "objects", n, "bytes", size.Load())
				}
			}
		}()
	}
	listErr := src.List(ctx, srcPrefix, func(o storage.Object) error {
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
		slog.Info("skipped existing objects", "count", skipped.Load())
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
