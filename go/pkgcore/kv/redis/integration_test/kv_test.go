//go:build integration

package redis_test

// Integration tests for kvredis.NewKVStore: each test drives the store
// through the public KVStore interface against a real Redis, asserting the
// exact semantics pkgcore's in-memory store's unit tests pin -- TTL expiry,
// IncrByFloat keeping a live key's expiry, CompareAndSwap as set-if-absent,
// expiry untouched by a swap -- plus the atomicity properties only a shared
// server can exercise. Where a semantic depends on the server's clock
// (expiry), the raw client doubles as an oracle for what the server holds.

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	kvredis "github.com/vislake/speed/go/pkgcore/kv/redis"
	"github.com/vislake/speed/go/pkgcore/kvstoretest"
)

func TestKVStore_SetGetDelete(t *testing.T) {
	ctx := context.Background()
	kv := kvredis.NewKVStore(startRedisClient(t, ctx))

	value, found, err := kv.Get(ctx, "billing:invoice:1042")
	if err != nil {
		t.Fatalf("Get() error = %v, want nil", err)
	}
	if found || value != nil {
		t.Errorf("Get() = (%q, %t), want (nil, false) for a missing key", value, found)
	}

	if err := kv.Set(ctx, "billing:invoice:1042", []byte("paid"), 0); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}
	value, found, err = kv.Get(ctx, "billing:invoice:1042")
	if err != nil || !found || string(value) != "paid" {
		t.Errorf("Get() = (%q, %t, %v), want (\"paid\", true, nil)", value, found, err)
	}

	if err := kv.Delete(ctx, "billing:invoice:1042"); err != nil {
		t.Fatalf("Delete() error = %v, want nil", err)
	}
	_, found, err = kv.Get(ctx, "billing:invoice:1042")
	if err != nil || found {
		t.Errorf("Get() after Delete = (%t, %v), want (false, nil)", found, err)
	}

	// Deleting a key the server does not hold is not an error.
	if err := kv.Delete(ctx, "never-stored"); err != nil {
		t.Errorf("Delete() of a missing key error = %v, want nil", err)
	}
}

func TestKVStore_SetWithTTL_ExpiresTheKey(t *testing.T) {
	ctx := context.Background()
	client := startRedisClient(t, ctx)
	kv := kvredis.NewKVStore(client)

	const key = "session:u-1"
	if err := kv.Set(ctx, key, []byte("token"), 200*time.Millisecond); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}
	ttl, err := client.PTTL(ctx, key).Result()
	if err != nil || ttl <= 0 {
		t.Fatalf("PTTL = %v (err %v), want a live expiry", ttl, err)
	}

	time.Sleep(500 * time.Millisecond)

	_, found, err := kv.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get() error = %v, want nil", err)
	}
	if found {
		t.Error("Get() found the key after its expiry, want it treated as absent")
	}
}

func TestKVStore_SetWithNonPositiveTTL_StoresForever_AndClearsAnExistingExpiry(t *testing.T) {
	ctx := context.Background()
	client := startRedisClient(t, ctx)
	kv := kvredis.NewKVStore(client)

	const key = "config:retry-limit"
	if err := kv.Set(ctx, key, []byte("3"), 200*time.Millisecond); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}

	// A ttl of zero replaces both value and expiry: the key must outlive the
	// expiry the first Set gave it.
	if err := kv.Set(ctx, key, []byte("5"), 0); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}
	ttl, err := client.PTTL(ctx, key).Result()
	if err != nil || ttl != -1 {
		t.Fatalf("PTTL = %v (err %v), want -1: the zero ttl must clear the expiry", ttl, err)
	}

	time.Sleep(400 * time.Millisecond)

	value, found, err := kv.Get(ctx, key)
	if err != nil || !found || string(value) != "5" {
		t.Errorf("Get() = (%q, %t, %v), want (\"5\", true, nil) after the old expiry passed", value, found, err)
	}
}

func TestKVStore_IncrByFloat_StartsMissingKeysAtZeroWithoutAnExpiry(t *testing.T) {
	ctx := context.Background()
	client := startRedisClient(t, ctx)
	kv := kvredis.NewKVStore(client)

	const key = "quota:acme:credits"
	result, err := kv.IncrByFloat(ctx, key, 2.5)
	if err != nil {
		t.Fatalf("IncrByFloat() error = %v, want nil", err)
	}
	if result != 2.5 {
		t.Errorf("IncrByFloat() = %v, want 2.5", result)
	}

	// A key born from an increment has no expiry, exactly like the in-memory
	// store's counter that starts at zero.
	ttl, err := client.PTTL(ctx, key).Result()
	if err != nil || ttl != -1 {
		t.Fatalf("PTTL = %v (err %v), want -1: a fresh counter must not expire", ttl, err)
	}

	// The server renders the number itself; parse, never compare as text.
	value, found, err := kv.Get(ctx, key)
	if err != nil || !found {
		t.Fatalf("Get() = (%t, %v), want (true, nil)", found, err)
	}
	parsed, err := strconv.ParseFloat(string(value), 64)
	if err != nil || parsed != 2.5 {
		t.Errorf("stored value %q does not parse to 2.5 (err %v)", value, err)
	}

	again, err := kv.IncrByFloat(ctx, key, 2.5)
	if err != nil || again != 5 {
		t.Errorf("IncrByFloat() = %v (err %v), want 5", again, err)
	}
}

func TestKVStore_IncrByFloat_KeepsALiveKeysExpiry(t *testing.T) {
	ctx := context.Background()
	client := startRedisClient(t, ctx)
	kv := kvredis.NewKVStore(client)

	const key = "quota:acme:monthly"
	if err := kv.Set(ctx, key, []byte("10"), 250*time.Millisecond); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}

	result, err := kv.IncrByFloat(ctx, key, 5.5)
	if err != nil || result != 15.5 {
		t.Fatalf("IncrByFloat() = %v (err %v), want 15.5", result, err)
	}

	// An increment is not a refresh: the counter must still die with its
	// rolling window, or a monthly quota would never roll over.
	ttl, err := client.PTTL(ctx, key).Result()
	if err != nil || ttl <= 0 {
		t.Fatalf("PTTL = %v (err %v), want the key to keep its live expiry", ttl, err)
	}

	time.Sleep(600 * time.Millisecond)

	_, found, err := kv.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get() error = %v, want nil", err)
	}
	if found {
		t.Error("Get() found the counter after its window expired, want it gone")
	}
}

func TestKVStore_IncrByFloat_NonNumericValueFailsAndStaysUntouched(t *testing.T) {
	ctx := context.Background()
	kv := kvredis.NewKVStore(startRedisClient(t, ctx))

	tests := []struct {
		name  string
		value []byte
	}{
		{"plain text", []byte("not a number")},
		{"an empty value", []byte{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := "kv-it-" + t.Name()
			if err := kv.Set(ctx, key, tt.value, 0); err != nil {
				t.Fatalf("Set() error = %v, want nil", err)
			}

			result, err := kv.IncrByFloat(ctx, key, 1)
			if !errors.Is(err, pkgcore.ErrNotNumeric) {
				t.Fatalf("IncrByFloat() error = %v, want ErrNotNumeric", err)
			}
			if result != 0 {
				t.Errorf("IncrByFloat() = %v on failure, want 0", result)
			}

			value, found, err := kv.Get(ctx, key)
			if err != nil || !found {
				t.Fatalf("Get() = (%t, %v), want (true, nil)", found, err)
			}
			if string(value) != string(tt.value) {
				t.Errorf("value after the failed increment = %q, want %q untouched", value, tt.value)
			}
		})
	}
}

func TestKVStore_CompareAndSwap(t *testing.T) {
	ctx := context.Background()
	kv := kvredis.NewKVStore(startRedisClient(t, ctx))

	// Set-if-absent: a missing key matches the empty expectation, whether it
	// is nil or a zero-length slice.
	swapped, err := kv.CompareAndSwap(ctx, "lock:acme:import", nil, []byte("held"))
	if err != nil || !swapped {
		t.Fatalf("CompareAndSwap(nil old) = %t (err %v), want (true, nil) as set-if-absent", swapped, err)
	}
	value, found, err := kv.Get(ctx, "lock:acme:import")
	if err != nil || !found || string(value) != "held" {
		t.Fatalf("Get() = (%q, %t, %v), want (\"held\", true, nil)", value, found, err)
	}

	swapped, err = kv.CompareAndSwap(ctx, "lock:acme:export", []byte{}, []byte("held"))
	if err != nil || !swapped {
		t.Errorf("CompareAndSwap(empty old) = %t (err %v), want (true, nil) as set-if-absent", swapped, err)
	}

	// An absent key with a non-empty expectation is a mismatch, not an error.
	swapped, err = kv.CompareAndSwap(ctx, "lock:acme:never", []byte("someone else"), []byte("held"))
	if err != nil || swapped {
		t.Errorf("CompareAndSwap(non-empty old, absent key) = %t (err %v), want (false, nil)", swapped, err)
	}
	_, found, err = kv.Get(ctx, "lock:acme:never")
	if err != nil || found {
		t.Errorf("Get() after the refused swap = (%t, %v), want (false, nil)", found, err)
	}

	// A mismatch on a present key leaves the value alone.
	swapped, err = kv.CompareAndSwap(ctx, "lock:acme:import", []byte("stale"), []byte("nobody"))
	if err != nil || swapped {
		t.Fatalf("CompareAndSwap(mismatch) = %t (err %v), want (false, nil)", swapped, err)
	}
	value, _, err = kv.Get(ctx, "lock:acme:import")
	if err != nil || string(value) != "held" {
		t.Errorf("value after the mismatch = %q (err %v), want \"held\"", value, err)
	}

	// An exact match swaps the value.
	swapped, err = kv.CompareAndSwap(ctx, "lock:acme:import", []byte("held"), []byte("released"))
	if err != nil || !swapped {
		t.Fatalf("CompareAndSwap(match) = %t (err %v), want (true, nil)", swapped, err)
	}
	value, _, err = kv.Get(ctx, "lock:acme:import")
	if err != nil || string(value) != "released" {
		t.Errorf("value after the swap = %q (err %v), want \"released\"", value, err)
	}
}

func TestKVStore_CompareAndSwap_NeverChangesTheKeysExpiry(t *testing.T) {
	ctx := context.Background()
	client := startRedisClient(t, ctx)
	kv := kvredis.NewKVStore(client)

	const key = "lock:acme:lease"
	if err := kv.Set(ctx, key, []byte("holder-1"), 250*time.Millisecond); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}
	before, err := client.PTTL(ctx, key).Result()
	if err != nil || before <= 0 {
		t.Fatalf("PTTL = %v (err %v), want a live expiry", before, err)
	}

	swapped, err := kv.CompareAndSwap(ctx, key, []byte("holder-1"), []byte("holder-2"))
	if err != nil || !swapped {
		t.Fatalf("CompareAndSwap() = %t (err %v), want (true, nil)", swapped, err)
	}

	// The swap is not a refresh: the lease must still run out, or a holder
	// could extend its lease forever by swapping in place.
	time.Sleep(600 * time.Millisecond)

	_, found, err := kv.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get() error = %v, want nil", err)
	}
	if found {
		t.Error("Get() found the lease after its expiry, want the swap to have left the expiry alone")
	}
}

func TestKVStore_CarriesBinaryValues(t *testing.T) {
	ctx := context.Background()
	kv := kvredis.NewKVStore(startRedisClient(t, ctx))

	const key = "crypto:blob"
	blob := []byte{0x00, 0x01, 0xff, 0xfe, 0x00}
	if err := kv.Set(ctx, key, blob, 0); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}
	value, found, err := kv.Get(ctx, key)
	if err != nil || !found {
		t.Fatalf("Get() = (%t, %v), want (true, nil)", found, err)
	}
	if string(value) != string(blob) {
		t.Errorf("Get() = %v, want the stored bytes %v back verbatim", value, blob)
	}

	// CompareAndSwap compares whole values, embedded NUL bytes included.
	swapped, err := kv.CompareAndSwap(ctx, key, blob, []byte("text now"))
	if err != nil || !swapped {
		t.Fatalf("CompareAndSwap(binary old) = %t (err %v), want (true, nil)", swapped, err)
	}
}

func TestKVStore_ConcurrentIncrementsLoseNoUpdates(t *testing.T) {
	ctx := context.Background()
	kv := kvredis.NewKVStore(startRedisClient(t, ctx))

	const (
		key        = "quota:acme:counter"
		goroutines = 20
		increments = 10
	)

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < increments; i++ {
				if _, err := kv.IncrByFloat(ctx, key, 1); err != nil {
					t.Errorf("IncrByFloat() error = %v, want nil", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	value, found, err := kv.Get(ctx, key)
	if err != nil || !found {
		t.Fatalf("Get() = (%t, %v), want (true, nil)", found, err)
	}
	parsed, err := strconv.ParseFloat(string(value), 64)
	if err != nil {
		t.Fatalf("stored value %q does not parse: %v", value, err)
	}
	if parsed != goroutines*increments {
		t.Errorf("counter = %v, want %d: a concurrent increment was lost", parsed, goroutines*increments)
	}
}

// TestKVStore_ConformsToKVStoreContract proves kvredis.NewKVStore satisfies
// the shared contract kvstoretest.AssertConforms checks -- the same suite
// go/pkgcore's own kv_conformance_test.go runs against
// pkgcore.NewMemoryKVStore -- against a real Redis, so drift between the
// two KVStore implementations under the deployment-composition retrofit's N
// registered implementations per seam is caught here once instead of
// pairwise. Every store AssertConforms's subtests build shares one Redis
// container and client (one container per test file), which is safe because
// NewKVStore holds no per-instance state of its own -- it is a thin wrapper
// over the shared client, unlike EventBus, which is why no per-store cleanup
// is needed here.
func TestKVStore_ConformsToKVStoreContract(t *testing.T) {
	ctx := context.Background()
	client := startRedisClient(t, ctx)

	kvstoretest.AssertConforms(t, func() pkgcore.KVStore {
		return kvredis.NewKVStore(client)
	})
}
