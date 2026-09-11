package ratelimit

import (
	"math"
	"testing"
	"time"
)

// TestRetryAfterSeconds pins RetryAfterSeconds' boundary matrix across the
// inputs a caller can actually pass: sub-second remainders -- the ordinary
// tail of every exhausted window -- round up to 1, whole seconds pass
// through, and a remainder of zero or less floors at 0 (retry now is true
// once nothing remains; a negative count would be ungrammatical in the
// delay-seconds vocabulary the conversion feeds). The top clamp, reachable
// only through the float core retryAfterSeconds with a value no
// time.Duration can carry on a 64-bit int platform, is pinned separately
// below the table. See RetryAfterSeconds' own doc comment for the shape's
// rationale.
func TestRetryAfterSeconds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		remaining time.Duration
		want      int
	}{
		{name: "sub-second window tail rounds up", remaining: 900 * time.Millisecond, want: 1},
		{name: "one millisecond rounds up", remaining: time.Millisecond, want: 1},
		{name: "exactly one second passes through", remaining: time.Second, want: 1},
		{name: "one second and a bit rounds up", remaining: 1100 * time.Millisecond, want: 2},
		{name: "window just reset floors at zero", remaining: 0, want: 0},
		{name: "elapsed remainder floors at zero", remaining: -3 * time.Second, want: 0},
		{name: "negative sub-second floors at zero", remaining: -500 * time.Millisecond, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RetryAfterSeconds(tt.remaining); got != tt.want {
				t.Fatalf("RetryAfterSeconds(%v) = %d, want %d", tt.remaining, got, tt.want)
			}
		})
	}

	// The float core's top clamp: a value int cannot represent answers
	// math.MaxInt, never an implementation-defined conversion. 2^63 -- the
	// nearest float64 to math.MaxInt -- is the smallest such value.
	if got := retryAfterSeconds(2 * float64(math.MaxInt)); got != math.MaxInt {
		t.Errorf("retryAfterSeconds(past int's range) = %d, want math.MaxInt", got)
	}
	if got := retryAfterSeconds(float64(math.MaxInt)); got != math.MaxInt {
		t.Errorf("retryAfterSeconds(at the clamp threshold) = %d, want math.MaxInt", got)
	}
	if got := retryAfterSeconds(1e15); got != 1_000_000_000_000_000 {
		t.Errorf("retryAfterSeconds(1e15) = %d, want 1e15 -- a large representable value converts directly", got)
	}
}
