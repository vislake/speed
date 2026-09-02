package jobs

import (
	"testing"
	"time"
)

func TestResolveEnqueueOptions_Defaults(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fallback := 90 * time.Second

	got := resolveEnqueueOptions(now, fallback, nil)

	if got.priority != PriorityNormal {
		t.Errorf("priority = %v, want %v", got.priority, PriorityNormal)
	}
	if got.maxRetries != DefaultMaxRetries {
		t.Errorf("maxRetries = %d, want %d", got.maxRetries, DefaultMaxRetries)
	}
	if got.timeout != fallback {
		t.Errorf("timeout = %v, want fallback %v", got.timeout, fallback)
	}
	if !got.scheduledAt.Equal(now) {
		t.Errorf("scheduledAt = %v, want now (%v) with no delay", got.scheduledAt, now)
	}
}

func TestResolveEnqueueOptions_Overrides(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	got := resolveEnqueueOptions(now, DefaultTimeout, []EnqueueOption{
		WithPriority(PriorityHigh),
		WithMaxRetries(7),
		WithTimeout(30 * time.Second),
		WithDelay(10 * time.Minute),
	})

	if got.priority != PriorityHigh {
		t.Errorf("priority = %v, want %v", got.priority, PriorityHigh)
	}
	if got.maxRetries != 7 {
		t.Errorf("maxRetries = %d, want 7", got.maxRetries)
	}
	if got.timeout != 30*time.Second {
		t.Errorf("timeout = %v, want 30s", got.timeout)
	}
	wantScheduledAt := now.Add(10 * time.Minute)
	if !got.scheduledAt.Equal(wantScheduledAt) {
		t.Errorf("scheduledAt = %v, want %v", got.scheduledAt, wantScheduledAt)
	}
}

func TestResolveEnqueueOptions_ScheduledAtLastWins(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	explicit := now.Add(48 * time.Hour)

	// WithScheduledAt applied after WithDelay: the absolute time wins.
	got := resolveEnqueueOptions(now, DefaultTimeout, []EnqueueOption{
		WithDelay(1 * time.Minute),
		WithScheduledAt(explicit),
	})
	if !got.scheduledAt.Equal(explicit) {
		t.Errorf("scheduledAt = %v, want explicit %v (WithScheduledAt applied last)", got.scheduledAt, explicit)
	}

	// WithDelay applied after WithScheduledAt: the delay wins instead,
	// relative to now, not to the discarded explicit time.
	got = resolveEnqueueOptions(now, DefaultTimeout, []EnqueueOption{
		WithScheduledAt(explicit),
		WithDelay(1 * time.Minute),
	})
	wantFromDelay := now.Add(1 * time.Minute)
	if !got.scheduledAt.Equal(wantFromDelay) {
		t.Errorf("scheduledAt = %v, want %v (WithDelay applied last)", got.scheduledAt, wantFromDelay)
	}
}

func TestResolveEnqueueOptions_ClampsOutOfRangeValues(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fallback := 45 * time.Second

	got := resolveEnqueueOptions(now, fallback, []EnqueueOption{
		WithMaxRetries(-3),
		WithTimeout(0),
	})
	if got.maxRetries != 0 {
		t.Errorf("maxRetries = %d, want 0 (negative clamped)", got.maxRetries)
	}
	if got.timeout != fallback {
		t.Errorf("timeout = %v, want fallback %v (non-positive WithTimeout ignored)", got.timeout, fallback)
	}

	got = resolveEnqueueOptions(now, fallback, []EnqueueOption{WithTimeout(-1 * time.Second)})
	if got.timeout != fallback {
		t.Errorf("timeout = %v, want fallback %v (negative WithTimeout ignored)", got.timeout, fallback)
	}
}
