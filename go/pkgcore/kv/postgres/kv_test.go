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
