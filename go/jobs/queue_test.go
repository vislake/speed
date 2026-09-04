package jobs

import (
	"testing"
	"time"
)

func TestResolveEnqueueOptions_Defaults(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fallback := 90 * time.Second

	got := ResolveEnqueueOptions(now, fallback, nil)

	if got.Priority != PriorityNormal {
		t.Errorf("priority = %v, want %v", got.Priority, PriorityNormal)
	}
	if got.MaxRetries != DefaultMaxRetries {
		t.Errorf("maxRetries = %d, want %d", got.MaxRetries, DefaultMaxRetries)
	}
	if got.Timeout != fallback {
		t.Errorf("timeout = %v, want fallback %v", got.Timeout, fallback)
	}
	if !got.ScheduledAt.Equal(now) {
		t.Errorf("scheduledAt = %v, want now (%v) with no delay", got.ScheduledAt, now)
	}
}

func TestResolveEnqueueOptions_Overrides(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	got := ResolveEnqueueOptions(now, DefaultTimeout, []EnqueueOption{
		WithPriority(PriorityHigh),
		WithMaxRetries(7),
		WithTimeout(30 * time.Second),
		WithDelay(10 * time.Minute),
	})

	if got.Priority != PriorityHigh {
		t.Errorf("priority = %v, want %v", got.Priority, PriorityHigh)
	}
	if got.MaxRetries != 7 {
		t.Errorf("maxRetries = %d, want 7", got.MaxRetries)
	}
	if got.Timeout != 30*time.Second {
		t.Errorf("timeout = %v, want 30s", got.Timeout)
	}
	wantScheduledAt := now.Add(10 * time.Minute)
	if !got.ScheduledAt.Equal(wantScheduledAt) {
		t.Errorf("scheduledAt = %v, want %v", got.ScheduledAt, wantScheduledAt)
	}
}

func TestResolveEnqueueOptions_ScheduledAtLastWins(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	explicit := now.Add(48 * time.Hour)

	// WithScheduledAt applied after WithDelay: the absolute time wins.
	got := ResolveEnqueueOptions(now, DefaultTimeout, []EnqueueOption{
		WithDelay(1 * time.Minute),
		WithScheduledAt(explicit),
	})
	if !got.ScheduledAt.Equal(explicit) {
		t.Errorf("scheduledAt = %v, want explicit %v (WithScheduledAt applied last)", got.ScheduledAt, explicit)
	}

	// WithDelay applied after WithScheduledAt: the delay wins instead,
	// relative to now, not to the discarded explicit time.
	got = ResolveEnqueueOptions(now, DefaultTimeout, []EnqueueOption{
		WithScheduledAt(explicit),
		WithDelay(1 * time.Minute),
	})
	wantFromDelay := now.Add(1 * time.Minute)
	if !got.ScheduledAt.Equal(wantFromDelay) {
		t.Errorf("scheduledAt = %v, want %v (WithDelay applied last)", got.ScheduledAt, wantFromDelay)
	}
}

func TestResolveEnqueueOptions_ClampsOutOfRangeValues(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fallback := 45 * time.Second

	got := ResolveEnqueueOptions(now, fallback, []EnqueueOption{
		WithMaxRetries(-3),
		WithTimeout(0),
	})
	if got.MaxRetries != 0 {
		t.Errorf("maxRetries = %d, want 0 (negative clamped)", got.MaxRetries)
	}
	if got.Timeout != fallback {
		t.Errorf("timeout = %v, want fallback %v (non-positive WithTimeout ignored)", got.Timeout, fallback)
	}

	got = ResolveEnqueueOptions(now, fallback, []EnqueueOption{WithTimeout(-1 * time.Second)})
	if got.Timeout != fallback {
		t.Errorf("timeout = %v, want fallback %v (negative WithTimeout ignored)", got.Timeout, fallback)
	}
}
