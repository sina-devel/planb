// Package retry runs operations again with exponential backoff and jitter after transient failures.
package retry

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"
)

// Defaults used when a Policy field is zero.
const (
	DefaultAttempts       = 5
	DefaultInitialBackoff = 2 * time.Second
	DefaultMaxBackoff     = time.Minute
)

// Policy controls how often and how long an operation is retried.
type Policy struct {
	// Attempts is the total number of tries, including the first one.
	Attempts int
	// InitialBackoff is the wait before the second try. It doubles after each failure up to MaxBackoff.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

func (p Policy) withDefaults() Policy {
	if p.Attempts <= 0 {
		p.Attempts = DefaultAttempts
	}
	if p.InitialBackoff <= 0 {
		p.InitialBackoff = DefaultInitialBackoff
	}
	if p.MaxBackoff <= 0 {
		p.MaxBackoff = DefaultMaxBackoff
	}
	if p.InitialBackoff > p.MaxBackoff {
		p.InitialBackoff = p.MaxBackoff
	}
	return p
}

// Backoff returns the wait after the given failed attempt (0-based): half of the exponential delay is fixed
// and half is random, so concurrent workers spread out but always wait long enough for a rate limit to ease.
func (p Policy) Backoff(attempt int) time.Duration {
	p = p.withDefaults()
	d := p.MaxBackoff
	if attempt < 30 {
		if e := p.InitialBackoff << attempt; e > 0 && e < d {
			d = e
		}
	}
	half := d / 2
	return half + rand.N(d-half+1)
}

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent marks err so that Do returns it without retrying.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// IsPermanent reports whether err was marked with Permanent.
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

// Do calls fn until it succeeds, returns an error that retryable rejects or that is marked Permanent, the
// attempts are used up, or ctx is done. A nil retryable treats every error as transient.
func Do(ctx context.Context, p Policy, log *slog.Logger, what string, retryable func(error) bool, fn func() error) error {
	p = p.withDefaults()
	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		if IsPermanent(err) || ctx.Err() != nil || attempt+1 >= p.Attempts || (retryable != nil && !retryable(err)) {
			return err
		}
		wait := p.Backoff(attempt)
		if log != nil {
			log.Warn("retrying after error", "operation", what, "attempt", attempt+1, "of", p.Attempts,
				"wait", wait.Round(time.Millisecond), "error", err)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
	}
}
