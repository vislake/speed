package postgres

// Hermetic unit tests for the PostgreSQL-backed KVStore: everything here
// runs without a real PostgreSQL server. Behaviour that needs one lives in
// the integration tier (integration_test/kv_test.go); what belongs here is
// what is local to the store itself -- a nil pool is a wiring error
// reported at construction, a cancelled context fails every operation with
// the context's error before any statement reaches the wire, and the pure
// helper functions (normalizeValue, formatFloat, parseFloat, isNotNumericErr)
// are exercised directly, mirroring kv/redis/kv_test.go's identical layout
// and rationale.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestNewKVStore_PanicsOnNilPool pins that a nil pool is a wiring error
// reported at construction, not a failure deferred to the first operation.
func TestNewKVStore_PanicsOnNilPool(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewKVStore(nil) did not panic, want it to")
		}
	}()
	NewKVStore(nil)
}

// TestKVStore_CancelledContext pins the contract that no operation runs on a
// cancelled context: every method returns the context's error instead of
// performing the operation, mirroring pkgcore's in-memory store and
// kv/redis's identical contract for the distributed-mode store. The pool
// points at a closed port, so an operation that ignored the context and
// reached for the server would have to fail with a connection error, never
// the context's error this table asserts.
func TestKVStore_CancelledContext(t *testing.T) {
	t.Parallel()

	pool, err := pgxpool.New(context.Background(), "postgres://user:pass@127.0.0.1:1/db")
	if err != nil {
		t.Fatalf("pgxpool.New() error = %v, want nil", err)
	}
	t.Cleanup(pool.Close)
	store := NewKVStore(pool)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name string
		op   func() error
	}{
		{"Get", func() error {
			_, _, err := store.Get(ctx, "k")
			return err
		}},
		{"Set", func() error {
			return store.Set(ctx, "k", []byte("v"), 0)
		}},
		{"Delete", func() error {
			return store.Delete(ctx, "k")
		}},
		{"IncrByFloat", func() error {
			_, err := store.IncrByFloat(ctx, "k", 1)
			return err
		}},
		{"IncrByFloatWithTTL", func() error {
			_, err := store.IncrByFloatWithTTL(ctx, "k", 1, time.Hour)
			return err
		}},
		{"CompareAndSwap", func() error {
			_, err := store.CompareAndSwap(ctx, "k", nil, []byte("v"))
			return err
		}},
		{"Sweep", func() error {
			_, err := store.Sweep(ctx)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.op(); !errors.Is(err, context.Canceled) {
				t.Errorf("%s on a cancelled context error = %v, want context.Canceled", tt.name, err)
			}
		})
	}
}

// TestNormalizeValue pins that a nil slice becomes a non-nil, zero-length
// one, and every other slice passes through unchanged -- see
// normalizeValue's own doc comment for why the nil case matters to every
// SQL statement in this package.
func TestNormalizeValue(t *testing.T) {
	t.Parallel()

	if got := normalizeValue(nil); got == nil || len(got) != 0 {
		t.Errorf("normalizeValue(nil) = %#v, want a non-nil, zero-length slice", got)
	}
	want := []byte("payload")
	if got := normalizeValue(want); string(got) != string(want) {
		t.Errorf("normalizeValue(%q) = %q, want it unchanged", want, got)
	}
}

// TestFormatFloatParseFloat_RoundTrips pins that formatFloat's output
// parses back to the exact same float64 through parseFloat, the same
// round-trip guarantee pkgcore.memoryKVStore's own formatting relies on.
func TestFormatFloatParseFloat_RoundTrips(t *testing.T) {
	t.Parallel()

	for _, delta := range []float64{0, 1, -1, 2.5, 0.1, 1e10, -1e-10} {
		text := formatFloat(delta)
		parsed, err := parseFloat([]byte(text))
		if err != nil {
			t.Fatalf("parseFloat(%q) error = %v, want nil", text, err)
		}
		if parsed != delta {
			t.Errorf("formatFloat(%v) = %q, parseFloat back = %v, want %v", delta, text, parsed, delta)
		}
	}
}

// TestIsNotNumericErr pins the two PostgreSQL server error shapes
// incrByFloatSQL's convert_from/::numeric cast can raise, and that an
// unrelated error is not misclassified as one of them.
func TestIsNotNumericErr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "invalid numeric text",
			err:  errors.New(`ERROR: invalid input syntax for type numeric: "not a number" (SQLSTATE 22P02)`),
			want: true,
		},
		{
			name: "invalid utf8 bytes",
			err:  errors.New(`ERROR: invalid byte sequence for encoding "UTF8": 0xff (SQLSTATE 22021)`),
			want: true,
		},
		{
			name: "unrelated error",
			err:  errors.New("connection refused"),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNotNumericErr(tt.err); got != tt.want {
				t.Errorf("isNotNumericErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestExpiryInterval_NonPositiveTTLIsNil pins the zero-or-less boundary of
// the expiry parameter every expiry-writing statement binds: a ttl of zero
// or less must travel as an untyped nil (SQL NULL), which now() + NULL turns
// into a NULL expires_at -- "no expiry" -- rather than as a zero-length
// interval that now() + interval '00:00:00' would render as a non-NULL
// instant the read-side guards would treat as live forever.
func TestExpiryInterval_NonPositiveTTLIsNil(t *testing.T) {
	t.Parallel()

	for _, ttl := range []time.Duration{0, -time.Second} {
		if got := expiryInterval(ttl); got != nil {
			t.Errorf("expiryInterval(%v) = %#v, want nil", ttl, got)
		}
	}
}

// TestExpiryInterval_PositiveTTLIsTheDurationItself pins that a positive ttl
// travels as a time.Duration parameter -- the value pgx encodes as a
// PostgreSQL interval of exactly that length, which the database adds to its
// own now() (see expiryInterval's own doc comment). Returning the duration
// unchanged -- rather than an absolute instant computed with the application
// clock -- is the whole point of the one-clock fix WithClock's own doc
// comment describes.
func TestExpiryInterval_PositiveTTLIsTheDuration(t *testing.T) {
	t.Parallel()

	if got := expiryInterval(200 * time.Millisecond); got != 200*time.Millisecond {
		t.Errorf("expiryInterval(200ms) = %#v, want 200ms as a time.Duration", got)
	}
}

// TestWithClock_NilFunctionIsIgnored pins WithClock's nil guard, matching
// go/authn.WithClock's identical contract: a nil clock must leave whatever
// clock the store already holds in place rather than clearing it.
func TestWithClock_NilFunctionIsIgnored(t *testing.T) {
	t.Parallel()

	pool, err := pgxpool.New(context.Background(), "postgres://user:pass@127.0.0.1:1/db")
	if err != nil {
		t.Fatalf("pgxpool.New() error = %v, want nil", err)
	}
	t.Cleanup(pool.Close)

	store := NewKVStore(pool, WithClock(nil))
	if store.now == nil {
		t.Error("WithClock(nil) cleared the store's clock, want it left in place")
	}
}
