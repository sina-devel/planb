package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

var fast = Policy{Attempts: 4, InitialBackoff: time.Millisecond, MaxBackoff: 4 * time.Millisecond}

func TestDoRetriesUntilSuccess(t *testing.T) {
	calls := 0
	err := Do(context.Background(), fast, nil, "op", nil, func() error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("err=%v calls=%d, want nil and 3", err, calls)
	}
}

func TestDoStopsAfterAttempts(t *testing.T) {
	calls := 0
	err := Do(context.Background(), fast, nil, "op", nil, func() error { calls++; return errors.New("down") })
	if err == nil || calls != 4 {
		t.Fatalf("err=%v calls=%d, want error and 4", err, calls)
	}
}

func TestDoPermanent(t *testing.T) {
	calls := 0
	base := errors.New("denied")
	err := Do(context.Background(), fast, nil, "op", nil, func() error { calls++; return Permanent(base) })
	if !errors.Is(err, base) || calls != 1 {
		t.Fatalf("err=%v calls=%d, want denied and 1", err, calls)
	}
	calls = 0
	_ = Do(context.Background(), fast, nil, "op", func(error) bool { return false }, func() error { calls++; return base })
	if calls != 1 {
		t.Fatalf("retryable=false: calls=%d, want 1", calls)
	}
}

func TestDoContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := Do(ctx, Policy{Attempts: 10, InitialBackoff: time.Hour, MaxBackoff: time.Hour}, nil, "op", nil, func() error {
		calls++
		cancel()
		return errors.New("x")
	})
	if err == nil || calls != 1 {
		t.Fatalf("err=%v calls=%d, want error and 1", err, calls)
	}
}

func TestBackoffBounds(t *testing.T) {
	p := Policy{Attempts: 10, InitialBackoff: time.Second, MaxBackoff: 10 * time.Second}
	for attempt, full := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second} {
		for range 50 {
			if d := p.Backoff(attempt); d < full/2 || d > full {
				t.Fatalf("Backoff(%d) = %v, want within [%v, %v]", attempt, d, full/2, full)
			}
		}
	}
	if d := p.Backoff(200); d < 5*time.Second || d > 10*time.Second {
		t.Fatalf("Backoff(200) = %v", d)
	}
}
