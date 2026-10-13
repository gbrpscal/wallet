package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/domain"
)

var t0 = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

func within(d, want time.Duration) bool {
	return d >= want-want/5 && d <= want+want/5
}

func TestReferencePolicyBackoff(t *testing.T) {
	p := app.DefaultReferencePolicy
	s := p.Initial(t0)
	if !within(s.NextAttemptAt.Sub(t0), time.Second) || !s.ExpiresAt.Equal(t0.Add(10*time.Minute)) || s.Attempts != 0 {
		t.Fatalf("initial schedule = %+v", s)
	}
	// Esperas esperadas após cada tentativa sem sucesso: 2, 4, ..., teto de 60s.
	wants := []time.Duration{2, 4, 8, 16, 32, 60, 60, 60}
	now := t0
	for i, want := range wants {
		s = p.Next(s, now)
		if s.Attempts != i+1 {
			t.Fatalf("attempts = %d, want %d", s.Attempts, i+1)
		}
		if got := s.NextAttemptAt.Sub(now); !within(got, want*time.Second) {
			t.Errorf("delay after attempt %d = %v, want %v ±20%%", i+1, got, want*time.Second)
		}
		now = s.NextAttemptAt
	}
}

func TestReferencePolicyNeverSchedulesPastExpiry(t *testing.T) {
	p := app.DefaultReferencePolicy
	s := p.Initial(t0)
	almost := s.ExpiresAt.Add(-time.Second)
	if next := p.Next(s, almost); !next.NextAttemptAt.Equal(s.ExpiresAt) {
		t.Errorf("next attempt %v is after expiry %v", next.NextAttemptAt, s.ExpiresAt)
	}
}

func TestReferencePolicyExhaustion(t *testing.T) {
	p := app.DefaultReferencePolicy
	s := p.Initial(t0)
	if p.Exhausted(s, t0) {
		t.Fatal("fresh schedule must not be exhausted")
	}
	s.Attempts = p.MaxAttempts
	if !p.Exhausted(s, t0) {
		t.Error("max attempts must exhaust")
	}
	s.Attempts = 0
	if !p.Exhausted(s, t0.Add(p.TTL)) {
		t.Error("reaching the TTL must exhaust")
	}
}

func TestRetryPolicy(t *testing.T) {
	p := app.RetryPolicy{Retries: 3, BaseDelay: time.Millisecond}
	cases := []struct {
		name  string
		err   error
		calls int
	}{
		{"transient is retried", &app.TransientError{Err: errors.New("deadlock")}, 4},
		{"stale version is retried", app.ErrConcurrentUpdate, 4},
		{"duplicate is retried", &app.DuplicateError{Constraint: "x"}, 4},
		{"business rejection is not retried", domain.ErrInsufficientFunds, 1},
		{"validation is not retried", domain.ErrInvalidAmount, 1},
		{"unknown error is not retried", errors.New("boom"), 1},
	}
	for _, c := range cases {
		calls := 0
		err := p.Do(context.Background(), func() error { calls++; return c.err })
		if calls != c.calls || !errors.Is(err, c.err) {
			t.Errorf("%s: calls = %d (want %d), err = %v", c.name, calls, c.calls, err)
		}
	}

	calls := 0
	err := p.Do(context.Background(), func() error {
		calls++
		if calls < 3 {
			return app.ErrConcurrentUpdate
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Errorf("success after retries: calls = %d, err = %v", calls, err)
	}
}

func TestRetryStopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	_ = app.RetryPolicy{Retries: 3, BaseDelay: time.Hour}.Do(ctx, func() error { calls++; return app.ErrConcurrentUpdate })
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestCursor(t *testing.T) {
	for _, v := range []int64{0, 1, 42, 1 << 40} {
		got, err := app.DecodeCursor(app.EncodeCursor(v))
		if err != nil || got != v {
			t.Errorf("round trip %d = %d, %v", v, got, err)
		}
	}
	if after, err := app.DecodeCursor(""); err != nil || after != 0 {
		t.Errorf("empty cursor = %d, %v", after, err)
	}
	for _, bad := range []string{"%%%", "e30", "eyJ2IjoyLCJhZnRlciI6MX0", "eyJ2IjoxLCJhZnRlciI6LTF9", "bm90IGpzb24", "42"} {
		if _, err := app.DecodeCursor(bad); !errors.Is(err, domain.ErrInvalidRequest) {
			t.Errorf("cursor %q: error = %v, want INVALID_REQUEST", bad, err)
		}
	}
}
