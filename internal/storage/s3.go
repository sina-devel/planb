// Package storage is a thin wrapper around minio-go for S3-compatible object storage.
package storage

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
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
}

// Object is a listed object.
type Object struct {
	Key  string
	Size int64
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

// NormalizePrefix strips leading slashes and ensures a trailing slash on non-empty prefixes.
func NormalizePrefix(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

// Check verifies the bucket exists and is reachable.
func (c *Client) Check(ctx context.Context) error {
	ok, err := c.mc.BucketExists(ctx, c.cfg.Bucket)
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
	_, err := c.Put(ctx, key, strings.NewReader(string(data)), int64(len(data)), contentType)
	return err
}

// Get opens an object for reading and returns its size and content type.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, int64, string, error) {
	obj, err := c.mc.GetObject(ctx, c.cfg.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, "", fmt.Errorf("reading %s: %w", c.URL(key), err)
	}
	st, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		return nil, 0, "", fmt.Errorf("reading %s: %w", c.URL(key), err)
	}
	return obj, st.Size, st.ContentType, nil
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
	_, err := c.mc.StatObject(ctx, c.cfg.Bucket, key, minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	if IsNotFound(err) {
		return false, nil
	}
	return false, err
}

// IsNotFound reports whether err is a missing key/bucket error.
func IsNotFound(err error) bool {
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		return resp.Code == "NoSuchKey" || resp.Code == "NoSuchBucket" || resp.StatusCode == http.StatusNotFound
	}
	return false
}

// List calls fn for every object under prefix (recursive).
func (c *Client) List(ctx context.Context, prefix string, fn func(Object) error) error {
	for obj := range c.mc.ListObjects(ctx, c.cfg.Bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return fmt.Errorf("listing %s: %w", c.URL(prefix), obj.Err)
		}
		if strings.HasSuffix(obj.Key, "/") && obj.Size == 0 {
			continue // directory marker
		}
		if err := fn(Object{Key: obj.Key, Size: obj.Size}); err != nil {
			return err
		}
	}
	return nil
}

// ListDirs returns the immediate "sub-directories" (common prefixes) below prefix.
func (c *Client) ListDirs(ctx context.Context, prefix string) ([]string, error) {
	var dirs []string
	for obj := range c.mc.ListObjects(ctx, c.cfg.Bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: false}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("listing %s: %w", c.URL(prefix), obj.Err)
		}
		if strings.HasSuffix(obj.Key, "/") {
			dirs = append(dirs, strings.TrimSuffix(strings.TrimPrefix(obj.Key, prefix), "/"))
		}
	}
	return dirs, nil
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
