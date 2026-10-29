// Package storage is a thin wrapper around minio-go for S3-compatible object storage.
package storage

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/sina-devel/planb/internal/retry"
)

// DefaultPartSize is used for streaming uploads of unknown size. Max object size is 10000 * PartSize.
const DefaultPartSize = 64 << 20

// Config describes how to connect to a bucket.
type Config struct {
	Endpoint      string `json:"endpoint"`
	Bucket        string `json:"bucket"`
	Prefix        string `json:"prefix,omitempty"`
	Region        string `json:"region,omitempty"`
	Insecure      bool   `json:"insecure,omitempty"`
	SkipTLSVerify bool   `json:"skipTLSVerify,omitempty"`
	AccessKey     string `json:"-"`
	SecretKey     string `json:"-"`
}

// Client is a bucket-scoped S3 client.
type Client struct {
	mc       *minio.Client
	cfg      Config
	PartSize uint64
	// Retry, when set, retries transient failures of whole operations: Check, Exists, listings and deletes are
	// repeated, and interrupted downloads resume where they stopped. minio-go also retries every HTTP request
	// on its own. Put is not retried here because a stream cannot be rewound; callers retry it.
	Retry *retry.Policy
	// Log receives retry messages. Defaults to slog.Default().
	Log *slog.Logger
}

// Object is a listed object.
type Object struct {
	Key  string
	Size int64
}

// SetRequestBackoff changes how long minio-go waits between retries of a single HTTP request (process-wide).
// The minio-go defaults (200ms, at most 1s) give up within seconds, which is too short for provider rate limits.
func SetRequestBackoff(unit, maxWait time.Duration) {
	minio.DefaultRetryUnit = unit
	minio.DefaultRetryCap = maxWait
}

// New creates a Client.
func New(cfg Config) (*Client, error) {
	endpoint := strings.TrimPrefix(strings.TrimPrefix(cfg.Endpoint, "https://"), "http://")
	var creds *credentials.Credentials
	if cfg.AccessKey != "" {
		creds = credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, "")
	} else {
		creds = credentials.NewChainCredentials([]credentials.Provider{
			&credentials.EnvAWS{},
			&credentials.IAM{Client: &http.Client{Transport: http.DefaultTransport}},
		})
	}
	opts := &minio.Options{
		Creds:  creds,
		Secure: !cfg.Insecure,
		Region: cfg.Region,
	}
	if cfg.SkipTLSVerify {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicitly requested by the user
		opts.Transport = tr
	}
	mc, err := minio.New(endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("creating s3 client for %s: %w", endpoint, err)
	}
	return &Client{mc: mc, cfg: cfg, PartSize: DefaultPartSize}, nil
}

// Bucket returns the bucket name.
func (c *Client) Bucket() string { return c.cfg.Bucket }

// Prefix returns the normalised prefix ("" or "something/").
func (c *Client) Prefix() string { return NormalizePrefix(c.cfg.Prefix) }

// Key joins parts below the configured prefix.
func (c *Client) Key(parts ...string) string {
	return c.Prefix() + strings.TrimPrefix(path.Join(parts...), "/")
}

// URL returns an s3:// URL for a key.
func (c *Client) URL(key string) string { return "s3://" + c.cfg.Bucket + "/" + key }

// SameBucket reports whether other points at the same endpoint and bucket.
func (c *Client) SameBucket(other *Client) bool {
	return c.mc.EndpointURL().Host == other.mc.EndpointURL().Host && c.cfg.Bucket == other.cfg.Bucket
}

// NormalizePrefix strips leading slashes and ensures a trailing slash on non-empty prefixes.
func NormalizePrefix(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

func (c *Client) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

// do runs fn, retrying transient failures when c.Retry is set.
func (c *Client) do(ctx context.Context, what string, fn func() error) error {
	if c.Retry == nil {
		return fn()
	}
	return retry.Do(ctx, *c.Retry, c.log(), what, IsRetryable, fn)
}

// Check verifies the bucket exists and is reachable.
func (c *Client) Check(ctx context.Context) error {
	var ok bool
	err := c.do(ctx, "check "+c.URL(""), func() (err error) {
		ok, err = c.mc.BucketExists(ctx, c.cfg.Bucket)
		return err
	})
	if err != nil {
		return fmt.Errorf("checking bucket %q: %w", c.cfg.Bucket, err)
	}
	if !ok {
		return fmt.Errorf("bucket %q does not exist", c.cfg.Bucket)
	}
	return nil
}

// Put uploads r to key. size may be -1 for streams of unknown length.
func (c *Client) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) (int64, error) {
	opts := minio.PutObjectOptions{ContentType: contentType}
	if size < 0 {
		opts.PartSize = c.PartSize
	}
	info, err := c.mc.PutObject(ctx, c.cfg.Bucket, key, r, size, opts)
	if err != nil {
		return 0, fmt.Errorf("uploading %s: %w", c.URL(key), err)
	}
	return info.Size, nil
}

// PutBytes uploads a small in-memory object.
func (c *Client) PutBytes(ctx context.Context, key string, data []byte, contentType string) error {
	return c.do(ctx, "upload "+c.URL(key), func() error {
		_, err := c.Put(ctx, key, strings.NewReader(string(data)), int64(len(data)), contentType)
		return err
	})
}

// Get opens an object for reading and returns its size and content type. With c.Retry set, a download
// interrupted by a transient error continues from the last byte read, as long as the object is unchanged.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, int64, string, error) {
	var (
		obj *minio.Object
		st  minio.ObjectInfo
	)
	err := c.do(ctx, "open "+c.URL(key), func() (err error) {
		obj, st, err = c.open(ctx, key, 0, "")
		return err
	})
	if err != nil {
		return nil, 0, "", fmt.Errorf("reading %s: %w", c.URL(key), err)
	}
	if c.Retry == nil {
		return obj, st.Size, st.ContentType, nil
	}
	r := &resumableReader{ctx: ctx, c: c, key: key, etag: st.ETag, size: st.Size, rc: obj}
	return r, st.Size, st.ContentType, nil
}

// open starts a download at offset. A non-empty etag makes it fail if the object has changed since.
func (c *Client) open(ctx context.Context, key string, offset int64, etag string) (*minio.Object, minio.ObjectInfo, error) {
	opts := minio.GetObjectOptions{}
	if offset > 0 {
		if err := opts.SetRange(offset, 0); err != nil {
			return nil, minio.ObjectInfo{}, err
		}
	}
	if etag != "" {
		if err := opts.SetMatchETag(etag); err != nil {
			return nil, minio.ObjectInfo{}, err
		}
	}
	obj, err := c.mc.GetObject(ctx, c.cfg.Bucket, key, opts)
	if err != nil {
		return nil, minio.ObjectInfo{}, err
	}
	st, err := obj.Stat() // GetObject is lazy: Stat sends the request
	if err != nil {
		_ = obj.Close()
		return nil, minio.ObjectInfo{}, err
	}
	return obj, st, nil
}

// resumableReader reopens the object at the current offset after a transient read error.
type resumableReader struct {
	ctx       context.Context
	c         *Client
	key, etag string
	size, off int64
	rc        io.ReadCloser
	failures  int // consecutive failures without progress
}

func (r *resumableReader) Read(p []byte) (int, error) {
	for {
		if r.rc == nil {
			if err := r.reopen(); err != nil {
				return 0, fmt.Errorf("reading %s at byte %d: %w", r.c.URL(r.key), r.off, err)
			}
		}
		n, err := r.rc.Read(p)
		r.off += int64(n)
		if n > 0 {
			r.failures = 0
		}
		if err == io.EOF && r.off < r.size {
			err = io.ErrUnexpectedEOF
		}
		if err == nil || err == io.EOF {
			return n, err
		}
		if r.off >= r.size {
			return n, io.EOF // every byte arrived; the connection failed afterwards
		}
		if !IsRetryable(err) {
			return n, fmt.Errorf("reading %s: %w", r.c.URL(r.key), err)
		}
		_ = r.rc.Close()
		r.rc = nil
		r.failures++
		if r.failures >= r.c.Retry.Attempts && r.c.Retry.Attempts > 0 {
			return n, fmt.Errorf("reading %s at byte %d: %w", r.c.URL(r.key), r.off, err)
		}
		r.c.log().Warn("download interrupted, resuming", "object", r.c.URL(r.key), "offset", r.off, "error", err)
		if n > 0 {
			return n, nil
		}
	}
}

func (r *resumableReader) reopen() error {
	if err := sleep(r.ctx, r.c.Retry.Backoff(r.failures-1)); err != nil {
		return err
	}
	return r.c.do(r.ctx, "resume "+r.c.URL(r.key), func() error {
		obj, _, err := r.c.open(r.ctx, r.key, r.off, r.etag)
		if err != nil {
			return err
		}
		r.rc = obj
		return nil
	})
}

func (r *resumableReader) Close() error {
	if r.rc == nil {
		return nil
	}
	return r.rc.Close()
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ReadAll reads a whole (small) object.
func (c *Client) ReadAll(ctx context.Context, key string) ([]byte, error) {
	r, _, _, err := c.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}

// Exists reports whether key exists.
func (c *Client) Exists(ctx context.Context, key string) (bool, error) {
	exists := false
	err := c.do(ctx, "stat "+c.URL(key), func() error {
		_, err := c.mc.StatObject(ctx, c.cfg.Bucket, key, minio.StatObjectOptions{})
		switch {
		case err == nil:
			exists = true
		case IsNotFound(err):
			return nil
		}
		return err
	})
	return exists, err
}

// IsNotFound reports whether err is a missing key/bucket error.
func IsNotFound(err error) bool {
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		return resp.Code == "NoSuchKey" || resp.Code == "NoSuchBucket" || resp.StatusCode == http.StatusNotFound
	}
	return false
}

// IsRetryable reports whether err is likely transient: a network failure, a timeout, a 5xx response or a rate
// limit. Authentication, permission, missing bucket/key and other client errors are not retryable.
func IsRetryable(err error) bool {
	// A deadline error is usually a per-request timeout; retry.Do stops on its own once the job context is done.
	if err == nil || errors.Is(err, context.Canceled) || retry.IsPermanent(err) {
		return false
	}
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		switch resp.Code {
		case "RequestTimeout", "RequestError", "InternalError", "ServiceUnavailable", "SlowDown", "SlowDownRead",
			"SlowDownWrite", "Throttling", "ThrottlingException", "RequestLimitExceeded", "RequestThrottled",
			"ExpiredToken", "ExpiredTokenException":
			return true
		}
		switch resp.StatusCode {
		case 0, http.StatusRequestTimeout, http.StatusTooManyRequests, 499:
			return true
		}
		return resp.StatusCode >= 500
	}
	var (
		unknownAuthority x509.UnknownAuthorityError
		invalidCert      x509.CertificateInvalidError
		hostname         x509.HostnameError
	)
	if errors.As(err, &unknownAuthority) || errors.As(err, &invalidCert) || errors.As(err, &hostname) {
		return false
	}
	return true // connection refused or reset, DNS failures, unexpected EOF, ...
}

// List calls fn for every object under prefix (recursive). With c.Retry set, a listing that fails part way
// continues after the last object it delivered.
func (c *Client) List(ctx context.Context, prefix string, fn func(Object) error) error {
	startAfter := ""
	failures := 0
	for {
		listErr, fnErr := c.listFrom(ctx, prefix, startAfter, func(o Object) error {
			startAfter = o.Key
			failures = 0
			return fn(o)
		})
		switch {
		case fnErr != nil:
			return fnErr
		case listErr == nil:
			return nil
		case c.Retry == nil || !IsRetryable(listErr) || ctx.Err() != nil:
			return fmt.Errorf("listing %s: %w", c.URL(prefix), listErr)
		}
		failures++
		if c.Retry.Attempts > 0 && failures >= c.Retry.Attempts {
			return fmt.Errorf("listing %s: %w", c.URL(prefix), listErr)
		}
		wait := c.Retry.Backoff(failures - 1)
		c.log().Warn("listing interrupted, resuming", "prefix", c.URL(prefix), "after", startAfter, "wait", wait, "error", listErr)
		if err := sleep(ctx, wait); err != nil {
			return fmt.Errorf("listing %s: %w", c.URL(prefix), listErr)
		}
	}
}

func (c *Client) listFrom(ctx context.Context, prefix, startAfter string, fn func(Object) error) (listErr, fnErr error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // stops minio's listing goroutine when fn fails
	opts := minio.ListObjectsOptions{Prefix: prefix, Recursive: true, StartAfter: startAfter}
	for obj := range c.mc.ListObjects(ctx, c.cfg.Bucket, opts) {
		if obj.Err != nil {
			return obj.Err, nil
		}
		if strings.HasSuffix(obj.Key, "/") && obj.Size == 0 {
			continue // directory marker
		}
		if err := fn(Object{Key: obj.Key, Size: obj.Size}); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// ListDirs returns the immediate "sub-directories" (common prefixes) below prefix.
func (c *Client) ListDirs(ctx context.Context, prefix string) ([]string, error) {
	var dirs []string
	err := c.do(ctx, "list "+c.URL(prefix), func() error {
		dirs = nil
		for obj := range c.mc.ListObjects(ctx, c.cfg.Bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: false}) {
			if obj.Err != nil {
				return obj.Err
			}
			if strings.HasSuffix(obj.Key, "/") {
				dirs = append(dirs, strings.TrimSuffix(strings.TrimPrefix(obj.Key, prefix), "/"))
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", c.URL(prefix), err)
	}
	return dirs, nil
}

// DeleteKeys removes the given objects. Keys that no longer exist are not an error.
func (c *Client) DeleteKeys(ctx context.Context, keys []string) error {
	return c.do(ctx, fmt.Sprintf("delete %d objects", len(keys)), func() error {
		objs := make(chan minio.ObjectInfo, len(keys))
		for _, k := range keys {
			objs <- minio.ObjectInfo{Key: k}
		}
		close(objs)
		var firstErr error
		for e := range c.mc.RemoveObjects(ctx, c.cfg.Bucket, objs, minio.RemoveObjectsOptions{}) {
			if firstErr == nil && !IsNotFound(e.Err) {
				firstErr = fmt.Errorf("deleting %s: %w", c.URL(e.ObjectName), e.Err)
			}
		}
		return firstErr
	})
}

// DeletePrefix removes every object under prefix. An empty prefix is refused.
func (c *Client) DeletePrefix(ctx context.Context, prefix string) error {
	if strings.Trim(prefix, "/") == "" {
		return errors.New("refusing to delete an empty prefix")
	}
	objs := make(chan minio.ObjectInfo)
	listErr := make(chan error, 1)
	go func() {
		defer close(objs)
		listErr <- c.List(ctx, prefix, func(o Object) error {
			select {
			case objs <- minio.ObjectInfo{Key: o.Key}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	var firstErr error
	for e := range c.mc.RemoveObjects(ctx, c.cfg.Bucket, objs, minio.RemoveObjectsOptions{}) {
		if firstErr == nil {
			firstErr = fmt.Errorf("deleting %s: %w", c.URL(e.ObjectName), e.Err)
		}
	}
	if err := <-listErr; err != nil {
		return err
	}
	return firstErr
}
