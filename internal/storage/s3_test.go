package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/sina-devel/planb/internal/retry"
)

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{minio.ErrorResponse{Code: "SlowDown", StatusCode: http.StatusServiceUnavailable}, true},
		{minio.ErrorResponse{Code: "Whatever", StatusCode: http.StatusTooManyRequests}, true},
		{minio.ErrorResponse{Code: "InternalError", StatusCode: http.StatusInternalServerError}, true},
		{minio.ErrorResponse{Code: "AccessDenied", StatusCode: http.StatusForbidden}, false},
		{minio.ErrorResponse{Code: "NoSuchBucket", StatusCode: http.StatusNotFound}, false},
		{minio.ErrorResponse{Code: "PreconditionFailed", StatusCode: http.StatusPreconditionFailed}, false},
		{&net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		{io.ErrUnexpectedEOF, true},
		{fmt.Errorf("wrapped: %w", context.Canceled), false},
		{fmt.Errorf("request timeout: %w", context.DeadlineExceeded), true},
		{retry.Permanent(io.ErrUnexpectedEOF), false},
	}
	for _, c := range cases {
		if got := IsRetryable(c.err); got != c.want {
			t.Errorf("IsRetryable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// flakyObjectServer serves one object for GET requests, honouring "Range: bytes=N-" and If-Match. The first
// cuts responses are closed after half of the requested bytes.
func flakyObjectServer(t *testing.T, data []byte, cuts int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		n := int32(0)
		if r.Method == http.MethodGet {
			n = requests.Add(1)
		}
		if m := r.Header.Get("If-Match"); m != "" && strings.Trim(m, `"`) != "etag1" {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		start := 0
		if rg := r.Header.Get("Range"); rg != "" {
			start, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"))
		}
		body := data[start:]
		h := w.Header()
		h.Set("ETag", `"etag1"`)
		h.Set("Content-Type", "application/test")
		h.Set("Last-Modified", time.Unix(0, 0).UTC().Format(http.TimeFormat))
		h.Set("Content-Length", strconv.Itoa(len(body)))
		if start > 0 {
			h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(data)-1, len(data)))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		if r.Method == http.MethodHead {
			return
		}
		if n <= cuts {
			_, _ = w.Write(body[:len(body)/2])
			w.(http.Flusher).Flush()
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

func testClient(t *testing.T, srv *httptest.Server, p *retry.Policy) *Client {
	t.Helper()
	c, err := New(Config{Endpoint: srv.URL, Bucket: "bucket", Insecure: true, Region: "us-east-1", AccessKey: "a", SecretKey: "b"})
	if err != nil {
		t.Fatal(err)
	}
	c.Retry = p
	return c
}

func TestGetResumesInterruptedDownload(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), 64<<10) // 1 MiB
	srv, requests := flakyObjectServer(t, data, 2)
	c := testClient(t, srv, &retry.Policy{Attempts: 5, InitialBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond})

	r, size, ct, err := c.Get(context.Background(), "obj")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if size != int64(len(data)) || ct != "application/test" || !bytes.Equal(got, data) {
		t.Fatalf("got %d bytes (size %d, type %q), want %d identical bytes", len(got), size, ct, len(data))
	}
	if n := requests.Load(); n != 3 {
		t.Fatalf("requests = %d, want 3 (two interrupted, one resumed to the end)", n)
	}
}

func TestGetWithoutRetryFailsOnInterruption(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 1<<20)
	srv, _ := flakyObjectServer(t, data, 1)
	c := testClient(t, srv, nil)
	r, _, _, err := c.Get(context.Background(), "obj")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("expected a read error without retries")
	}
}
