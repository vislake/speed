package postgres

// Hermetic unit tests for the PostgreSQL-backed KVStore: everything here
// runs without a real PostgreSQL server. Behaviour that needs one lives in
// the integration tier (integration_test/kv_test.go); what belongs here is
// what is local to the store itself -- a nil pool is a wiring error
// reported at construction, a cancelled context fails every operation with
// the context's error before any statement reaches the wire, the pure
// helper functions (normalizeValue, formatFloat, parseFloat, isNotNumericErr)
// are exercised directly, and the store's operations run against a scripted
// pgxmock double of the pool (the dbQueries seam Store.pool is typed on),
// so every statement's execution, row scanning and error mapping is driven
// hermetically with no server process -- mirroring kv/redis/kv_test.go's
// identical layout and rationale, with the miniredis tier standing in for
// the scripted double here.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pashagolub/pgxmock/v3"

	"github.com/vislake/speed/go/pkgcore"
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

// scriptedStore builds a Store over a pgxmock pool with no expectations
// queued, for the per-statement tests below. The matcher is string
// equality: the store's statements are fixed SQL constants full of regex
// metacharacters (parentheses, stars), and the point of the expectation is
// "this exact statement ran", never a pattern.
func scriptedStore(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v, want nil", err)
	}
	t.Cleanup(func() { mock.Close() })
	return &Store{pool: mock, now: time.Now}, mock
}

// wantMockErr asserts the returned error is non-nil and carries the
// operation prefix, pinning the store's wrapping.
func wantMockErr(t *testing.T, err error, prefix string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), prefix) {
		t.Fatalf("error = %v, want a wrapped error carrying %q", err, prefix)
	}
}

// TestKVStore_Get_OverScriptedPool drives Get's three answers: a live row is
// returned with ok true, a row the database does not hold (pgx.ErrNoRows)
// is a miss with ok false, and a failing statement surfaces as a wrapped
// error.
func TestKVStore_Get_OverScriptedPool(t *testing.T) {
	ctx := context.Background()

	t.Run("row found", func(t *testing.T) {
		store, mock := scriptedStore(t)
		mock.ExpectQuery(getSQL).WithArgs("k").WillReturnRows(
			pgxmock.NewRows([]string{"value"}).AddRow([]byte("hello")))
		value, ok, err := store.Get(ctx, "k")
		if err != nil || !ok || string(value) != "hello" {
			t.Fatalf("Get() = (%q, %v, %v), want (\"hello\", true, nil)", value, ok, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet pool expectations: %v", err)
		}
	})

	t.Run("row absent", func(t *testing.T) {
		store, mock := scriptedStore(t)
		mock.ExpectQuery(getSQL).WithArgs("k").WillReturnError(pgx.ErrNoRows)
		value, ok, err := store.Get(ctx, "k")
		if err != nil || ok || value != nil {
			t.Fatalf("Get() = (%q, %v, %v), want (nil, false, nil)", value, ok, err)
		}
	})

	t.Run("statement failure is wrapped", func(t *testing.T) {
		store, mock := scriptedStore(t)
		mock.ExpectQuery(getSQL).WithArgs("k").WillReturnError(errors.New("connection lost"))
		_, _, err := store.Get(ctx, "k")
		wantMockErr(t, err, "pkgcore/kv/postgres: get:")
	})
}

// TestKVStore_SetAndDelete_OverScriptedPool drives the two Exec-only
// operations over their success and failure answers.
func TestKVStore_SetAndDelete_OverScriptedPool(t *testing.T) {
	ctx := context.Background()

	t.Run("set succeeds and binds the normalized value", func(t *testing.T) {
		store, mock := scriptedStore(t)
		mock.ExpectExec(setSQL).WithArgs("k", []byte{}, nil).WillReturnResult(pgxmock.NewResult("INSERT", 1))
		if err := store.Set(ctx, "k", nil, 0); err != nil {
			t.Fatalf("Set() error = %v, want nil", err)
		}
	})

	t.Run("set failure is wrapped", func(t *testing.T) {
		store, mock := scriptedStore(t)
		mock.ExpectExec(setSQL).WithArgs("k", []byte{}, nil).WillReturnError(errors.New("connection lost"))
		wantMockErr(t, store.Set(ctx, "k", nil, 0), "pkgcore/kv/postgres: set:")
	})

	t.Run("delete succeeds", func(t *testing.T) {
		store, mock := scriptedStore(t)
		mock.ExpectExec(deleteSQL).WithArgs("k").WillReturnResult(pgxmock.NewResult("DELETE", 1))
		if err := store.Delete(ctx, "k"); err != nil {
			t.Fatalf("Delete() error = %v, want nil", err)
		}
	})

	t.Run("delete failure is wrapped", func(t *testing.T) {
		store, mock := scriptedStore(t)
		mock.ExpectExec(deleteSQL).WithArgs("k").WillReturnError(errors.New("connection lost"))
		wantMockErr(t, store.Delete(ctx, "k"), "pkgcore/kv/postgres: delete:")
	})
}

// TestKVStore_IncrByFloat_OverScriptedPool drives the increment's answers:
// a live numeric row yields the parsed result, the server's
// not-a-number answer maps to pkgcore.ErrNotNumeric, and a plain statement
// failure is wrapped.
func TestKVStore_IncrByFloat_OverScriptedPool(t *testing.T) {
	ctx := context.Background()

	t.Run("numeric row parses", func(t *testing.T) {
		store, mock := scriptedStore(t)
		mock.ExpectQuery(incrByFloatSQL).WithArgs("n", "2.5").WillReturnRows(
			pgxmock.NewRows([]string{"result"}).AddRow([]byte("15")))
		result, err := store.IncrByFloat(ctx, "n", 2.5)
		if err != nil || result != 15 {
			t.Fatalf("IncrByFloat() = (%v, %v), want (15, nil)", result, err)
		}
	})

	t.Run("non-numeric value maps to ErrNotNumeric", func(t *testing.T) {
		store, mock := scriptedStore(t)
		mock.ExpectQuery(incrByFloatSQL).WithArgs("n", "1").WillReturnError(
			errors.New("invalid input syntax for type numeric"))
		_, err := store.IncrByFloat(ctx, "n", 1)
		if !errors.Is(err, pkgcore.ErrNotNumeric) {
			t.Fatalf("IncrByFloat() error = %v, want pkgcore.ErrNotNumeric", err)
		}
	})

	t.Run("statement failure is wrapped", func(t *testing.T) {
		store, mock := scriptedStore(t)
		mock.ExpectQuery(incrByFloatSQL).WithArgs("n", "1").WillReturnError(errors.New("connection lost"))
		_, err := store.IncrByFloat(ctx, "n", 1)
		wantMockErr(t, err, "pkgcore/kv/postgres: incr:")
	})
}

// TestKVStore_IncrByFloatWithTTL_OverScriptedPool drives the ttl-attaching
// sibling's scripted answers, with the interval parameter bound exactly as
// the store computes it.
func TestKVStore_IncrByFloatWithTTL_OverScriptedPool(t *testing.T) {
	ctx := context.Background()

	t.Run("numeric row parses", func(t *testing.T) {
		store, mock := scriptedStore(t)
		mock.ExpectQuery(incrByFloatWithTTLSQL).WithArgs("n", "3", 45*time.Minute).WillReturnRows(
			pgxmock.NewRows([]string{"result"}).AddRow([]byte("18")))
		result, err := store.IncrByFloatWithTTL(ctx, "n", 3, 45*time.Minute)
		if err != nil || result != 18 {
			t.Fatalf("IncrByFloatWithTTL() = (%v, %v), want (18, nil)", result, err)
		}
	})

	t.Run("non-numeric value maps to ErrNotNumeric", func(t *testing.T) {
		store, mock := scriptedStore(t)
		mock.ExpectQuery(incrByFloatWithTTLSQL).WithArgs("n", "1", time.Hour).WillReturnError(
			errors.New("invalid byte sequence for encoding"))
		_, err := store.IncrByFloatWithTTL(ctx, "n", 1, time.Hour)
		if !errors.Is(err, pkgcore.ErrNotNumeric) {
			t.Fatalf("IncrByFloatWithTTL() error = %v, want pkgcore.ErrNotNumeric", err)
		}
	})

	t.Run("statement failure is wrapped", func(t *testing.T) {
		store, mock := scriptedStore(t)
		mock.ExpectQuery(incrByFloatWithTTLSQL).WithArgs("n", "1", time.Hour).WillReturnError(errors.New("connection lost"))
		_, err := store.IncrByFloatWithTTL(ctx, "n", 1, time.Hour)
		wantMockErr(t, err, "pkgcore/kv/postgres: incr with ttl:")
	})
}

// TestKVStore_CompareAndSwap_OverScriptedPool drives the swap's boolean
// answer (scanned 1 or 0) and its failure wrap.
func TestKVStore_CompareAndSwap_OverScriptedPool(t *testing.T) {
	ctx := context.Background()

	t.Run("swapped", func(t *testing.T) {
		store, mock := scriptedStore(t)
		mock.ExpectQuery(compareAndSwapSQL).WithArgs("k", []byte{}, []byte("v")).WillReturnRows(
			pgxmock.NewRows([]string{"swapped"}).AddRow(int64(1)))
		swapped, err := store.CompareAndSwap(ctx, "k", nil, []byte("v"))
		if err != nil || !swapped {
			t.Fatalf("CompareAndSwap() = (%v, %v), want (true, nil)", swapped, err)
		}
	})

	t.Run("not swapped", func(t *testing.T) {
		store, mock := scriptedStore(t)
		mock.ExpectQuery(compareAndSwapSQL).WithArgs("k", []byte("old"), []byte("new")).WillReturnRows(
			pgxmock.NewRows([]string{"swapped"}).AddRow(int64(0)))
		swapped, err := store.CompareAndSwap(ctx, "k", []byte("old"), []byte("new"))
		if err != nil || swapped {
			t.Fatalf("CompareAndSwap() = (%v, %v), want (false, nil)", swapped, err)
		}
	})

	t.Run("statement failure is wrapped", func(t *testing.T) {
		store, mock := scriptedStore(t)
		mock.ExpectQuery(compareAndSwapSQL).WithArgs("k", []byte{}, []byte("v")).WillReturnError(errors.New("connection lost"))
		_, err := store.CompareAndSwap(ctx, "k", nil, []byte("v"))
		wantMockErr(t, err, "pkgcore/kv/postgres: cas:")
	})
}

// TestKVStore_Sweep_OverScriptedPool drives the sweep's row count from the
// statement's own command tag and its failure wrap.
func TestKVStore_Sweep_OverScriptedPool(t *testing.T) {
	ctx := context.Background()

	t.Run("reports the deleted row count", func(t *testing.T) {
		store, mock := scriptedStore(t)
		mock.ExpectExec(sweepSQL).WillReturnResult(pgxmock.NewResult("DELETE", 3))
		n, err := store.Sweep(ctx)
		if err != nil || n != 3 {
			t.Fatalf("Sweep() = (%v, %v), want (3, nil)", n, err)
		}
	})

	t.Run("statement failure is wrapped", func(t *testing.T) {
		store, mock := scriptedStore(t)
		mock.ExpectExec(sweepSQL).WillReturnError(errors.New("connection lost"))
		_, err := store.Sweep(ctx)
		wantMockErr(t, err, "pkgcore/kv/postgres: sweep:")
	})
}
