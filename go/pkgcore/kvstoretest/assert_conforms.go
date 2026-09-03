// Package kvstoretest verifies that a pkgcore.KVStore implementation upholds
// the contract KVStore's own doc comment describes, independent of which
// backend implements it. It plays the same role for KVStore that
// go/tenancy/tenancytest.AssertIsolated plays for dbkit.Repository[T] and
// go/pkgcore/eventbustest.AssertConforms plays for EventBus: one suite every
// implementation — built-in or host-supplied through pkgcore.WithKVStore —
// must pass, so drift between implementations is caught here once instead of
// pairwise (see docs/internal/03-deployment-modes.md).
package kvstoretest

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// conformShortTTL and conformExpiryWait size the expiry check: long enough
// to survive ordinary scheduling jitter under -race, short enough to keep
// the suite fast, with the same 5x margin go/pkgcore's own kv_test.go uses
// for the identical reason.
const (
	conformShortTTL   = 25 * time.Millisecond
	conformExpiryWait = 5 * conformShortTTL
)

// AssertConforms verifies that the KVStore factory returns satisfies the
// contract documented on pkgcore.KVStore. Each subtest calls factory to get
// its own store instance and operates on keys it derives from the subtest
// name (see conformKey), so subtests sharing a long-lived backing store (as
// the Redis integration leg does, one container per test file) never
// collide on key names even though AssertConforms does not require factory
// to return an empty store.
//
// What AssertConforms checks, in order: Get on a key that was never set
// reports a miss, not an error; Set followed by Get round-trips the exact
// bytes; Delete removes a key, after which Get reports a miss again,
// deleting an absent key is not an error; a key set with a short TTL is
// visible until the TTL elapses and reports a miss afterward, while a key
// set with no TTL (zero) survives past the same wait; IncrByFloat on a
// missing key starts from zero and accumulates across calls; IncrByFloat on
// a key holding a non-numeric value fails with pkgcore.ErrNotNumeric and
// leaves the value unchanged; CompareAndSwap on a missing key succeeds only
// when old is empty (set-if-absent), and on an existing key succeeds only
// when old matches the stored value, leaving the value untouched on a
// mismatch; and a call made with an already-cancelled context fails with
// that context's error instead of performing the operation.
func AssertConforms(t *testing.T, factory func() pkgcore.KVStore) {
	t.Helper()

	t.Run("get_on_a_never_set_key_reports_a_miss_not_an_error", func(t *testing.T) {
		t.Helper()
		store := factory()
		key := conformKey(t, "never-set")

		val, found, err := store.Get(context.Background(), key)
		if err != nil {
			t.Fatalf("Get() error = %v, want nil", err)
		}
		if found {
			t.Errorf("Get() found = true, want false for a key never set")
		}
		if val != nil {
			t.Errorf("Get() value = %v, want nil for a key never set", val)
		}
	})

	t.Run("set_then_get_round_trips_the_exact_bytes", func(t *testing.T) {
		t.Helper()
		store := factory()
		key := conformKey(t, "round-trip")
		want := []byte("kvstoretest payload")

		if err := store.Set(context.Background(), key, want, 0); err != nil {
			t.Fatalf("Set() error = %v, want nil", err)
		}
		got, found, err := store.Get(context.Background(), key)
		if err != nil {
			t.Fatalf("Get() error = %v, want nil", err)
		}
		if !found {
			t.Fatal("Get() found = false, want true right after Set")
		}
		if !bytes.Equal(got, want) {
			t.Errorf("Get() value = %q, want %q", got, want)
		}
	})

	t.Run("delete_removes_the_key_and_is_a_no_op_on_an_absent_one", func(t *testing.T) {
		t.Helper()
		store := factory()
		key := conformKey(t, "delete")

		if err := store.Set(context.Background(), key, []byte("gone soon"), 0); err != nil {
			t.Fatalf("Set() error = %v, want nil", err)
		}
		if err := store.Delete(context.Background(), key); err != nil {
			t.Fatalf("Delete() error = %v, want nil", err)
		}
		if _, found, err := store.Get(context.Background(), key); err != nil || found {
			t.Errorf("Get() after Delete = (found=%v, err=%v), want (false, nil)", found, err)
		}
		// Deleting an already-absent key is not an error.
		if err := store.Delete(context.Background(), key); err != nil {
			t.Errorf("Delete() on an absent key error = %v, want nil", err)
		}
	})

	t.Run("a_short_ttl_expires_while_no_ttl_survives_the_same_wait", func(t *testing.T) {
		t.Helper()
		store := factory()
		expiring := conformKey(t, "expiring")
		persistent := conformKey(t, "persistent")

		if err := store.Set(context.Background(), expiring, []byte("v"), conformShortTTL); err != nil {
			t.Fatalf("Set() with a TTL error = %v, want nil", err)
		}
		if err := store.Set(context.Background(), persistent, []byte("v"), 0); err != nil {
			t.Fatalf("Set() with no TTL error = %v, want nil", err)
		}

		if _, found, err := store.Get(context.Background(), expiring); err != nil || !found {
			t.Fatalf("Get() immediately after Set with a TTL = (found=%v, err=%v), want (true, nil)", found, err)
		}

		time.Sleep(conformExpiryWait)

		if _, found, err := store.Get(context.Background(), expiring); err != nil || found {
			t.Errorf("Get() after the TTL elapsed = (found=%v, err=%v), want (false, nil)", found, err)
		}
		if _, found, err := store.Get(context.Background(), persistent); err != nil || !found {
			t.Errorf("Get() of a no-TTL key after the same wait = (found=%v, err=%v), want (true, nil)", found, err)
		}
	})

	t.Run("incr_by_float_starts_from_zero_and_accumulates", func(t *testing.T) {
		t.Helper()
		store := factory()
		key := conformKey(t, "counter")

		got, err := store.IncrByFloat(context.Background(), key, 2.5)
		if err != nil {
			t.Fatalf("IncrByFloat() first call error = %v, want nil", err)
		}
		if got != 2.5 {
			t.Errorf("IncrByFloat() first call = %v, want 2.5", got)
		}

		got, err = store.IncrByFloat(context.Background(), key, 1.5)
		if err != nil {
			t.Fatalf("IncrByFloat() second call error = %v, want nil", err)
		}
		if got != 4 {
			t.Errorf("IncrByFloat() second call = %v, want 4", got)
		}
	})

	t.Run("incr_by_float_on_a_non_numeric_value_fails_and_leaves_it_unchanged", func(t *testing.T) {
		t.Helper()
		store := factory()
		key := conformKey(t, "non-numeric")
		want := []byte("not a number")

		if err := store.Set(context.Background(), key, want, 0); err != nil {
			t.Fatalf("Set() error = %v, want nil", err)
		}

		if _, err := store.IncrByFloat(context.Background(), key, 1); !errors.Is(err, pkgcore.ErrNotNumeric) {
			t.Errorf("IncrByFloat() on a non-numeric value error = %v, want errors.Is(err, pkgcore.ErrNotNumeric)", err)
		}

		got, found, err := store.Get(context.Background(), key)
		if err != nil || !found {
			t.Fatalf("Get() after a failed IncrByFloat = (found=%v, err=%v), want (true, nil)", found, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("value after a failed IncrByFloat = %q, want unchanged %q", got, want)
		}
	})

	t.Run("compare_and_swap_set_if_absent_then_matched_swap_then_mismatch_leaves_it_untouched", func(t *testing.T) {
		t.Helper()
		store := factory()
		key := conformKey(t, "cas")

		// A missing key matches only an empty expectation: set-if-absent.
		swapped, err := store.CompareAndSwap(context.Background(), key, nil, []byte("v1"))
		if err != nil {
			t.Fatalf("CompareAndSwap() set-if-absent error = %v, want nil", err)
		}
		if !swapped {
			t.Fatal("CompareAndSwap() set-if-absent = false, want true for a missing key with an empty old value")
		}

		// A mismatched old leaves the value untouched.
		swapped, err = store.CompareAndSwap(context.Background(), key, []byte("wrong"), []byte("v2"))
		if err != nil {
			t.Fatalf("CompareAndSwap() mismatch error = %v, want nil", err)
		}
		if swapped {
			t.Error("CompareAndSwap() mismatch = true, want false")
		}
		if got, _, _ := store.Get(context.Background(), key); !bytes.Equal(got, []byte("v1")) {
			t.Errorf("value after a mismatched CompareAndSwap = %q, want unchanged %q", got, "v1")
		}

		// A matched old swaps.
		swapped, err = store.CompareAndSwap(context.Background(), key, []byte("v1"), []byte("v2"))
		if err != nil {
			t.Fatalf("CompareAndSwap() matched swap error = %v, want nil", err)
		}
		if !swapped {
			t.Fatal("CompareAndSwap() matched swap = false, want true")
		}
		if got, _, _ := store.Get(context.Background(), key); !bytes.Equal(got, []byte("v2")) {
			t.Errorf("value after a matched CompareAndSwap = %q, want %q", got, "v2")
		}
	})

	t.Run("a_cancelled_context_fails_without_performing_the_operation", func(t *testing.T) {
		t.Helper()
		store := factory()
		key := conformKey(t, "cancelled")

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := store.Set(ctx, key, []byte("v"), 0); !errors.Is(err, context.Canceled) {
			t.Errorf("Set() with a cancelled context error = %v, want context.Canceled", err)
		}
		if _, found, err := store.Get(ctx, key); !errors.Is(err, context.Canceled) {
			t.Errorf("Get() with a cancelled context error = %v, want context.Canceled", err)
		} else if found {
			t.Error("Get() with a cancelled context found = true, want the operation not to have run")
		}
		// Confirm the cancelled Set above never actually wrote the key: read
		// it back with a live context.
		if _, found, err := store.Get(context.Background(), key); err != nil || found {
			t.Errorf("Get() with a live context after a cancelled Set = (found=%v, err=%v), want (false, nil)", found, err)
		}
	})
}

// conformKey derives a key from t's name and suffix, so subtests sharing a
// long-lived backing store (the Redis integration leg starts one container
// per test file, not per case) never collide on key names.
func conformKey(t *testing.T, suffix string) string {
	t.Helper()
	return "kvstoretest:" + t.Name() + ":" + suffix
}
