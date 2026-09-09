package redis

// Hermetic unit tests for the Redis-backed KVStore: everything here runs
// without a Redis server process. Behaviour that needs a real server lives
// in the integration tier (integration_test/kv_test.go); what belongs in
// this file is what is local to the store itself -- a nil client is a wiring
// error reported at construction, and a cancelled context fails every
// operation with the context's error before any command reaches the wire --
// plus, below, the store's full operation surface driven through a real
// go-redis client against miniredis, an independent in-process Redis
// implementation: genuine wire round trips, genuine server-side expiry on a
// millisecond clock, and genuine Lua execution of the two compare-and-swap
// scripts, with no external process.

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/pkgcore"
)

// TestNewKVStore_PanicsOnNilClient pins that a nil client is a wiring error
// reported at construction, not a failure deferred to the first operation.
func TestNewKVStore_PanicsOnNilClient(t *testing.T) {
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
// performing the operation, mirroring pkgcore's in-memory store for the
// distributed-mode store. The client points at a closed port (the
// 127.0.0.1:1 address this package's own example uses), so an operation that
// ignored the context and reached for the server would have to fail with a
// connection error, never the context's error this table asserts. go-redis
// honours a cancelled context before dialing too, so what the test pins is
// the store's early return as the public contract: a store that swapped in a
// context-less operation would turn it red.
func TestKVStore_CancelledContext(t *testing.T) {
	t.Parallel()

	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { client.Close() })
	store := NewKVStore(client)

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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.op(); !errors.Is(err, context.Canceled) {
				t.Errorf("%s on a cancelled context error = %v, want context.Canceled", tt.name, err)
			}
		})
	}
}

// redisClient builds a go-redis client pointed at a fresh miniredis server,
// cleaned up with the test.
func redisClient(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return mini, client
}

// TestKVStore_GetSetDelete_OverMiniredis drives the plain lifecycle over
// real wire round trips against an independent Redis implementation: an
// absent key is a miss, Set stores and replaces, Delete removes, deleting an
// absent key is not an error, and an overwrite clears a prior expiry.
func TestKVStore_GetSetDelete_OverMiniredis(t *testing.T) {
	_, client := redisClient(t)
	store := NewKVStore(client)
	ctx := context.Background()

	if _, ok, err := store.Get(ctx, "k"); err != nil || ok {
		t.Fatalf("Get on an absent key = (_, %v, %v), want (_, false, nil)", ok, err)
	}

	if err := store.Set(ctx, "k", []byte("v1"), 0); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}
	got, ok, err := store.Get(ctx, "k")
	if err != nil || !ok || string(got) != "v1" {
		t.Fatalf("Get after Set = (%q, %v, %v), want (\"v1\", true, nil)", got, ok, err)
	}

	if err := store.Set(ctx, "k", []byte{0x00, 0xff}, 0); err != nil {
		t.Fatalf("second Set() error = %v, want nil", err)
	}
	got, ok, err = store.Get(ctx, "k")
	if err != nil || !ok || !bytes.Equal(got, []byte{0x00, 0xff}) {
		t.Fatalf("Get after overwrite = (%v, %v, %v), want the binary replacement", got, ok, err)
	}

	if err := store.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete() error = %v, want nil", err)
	}
	if _, ok, err := store.Get(ctx, "k"); err != nil || ok {
		t.Errorf("Get after Delete = (_, %v, %v), want (_, false, nil)", ok, err)
	}
	if err := store.Delete(ctx, "k"); err != nil {
		t.Errorf("Delete of an absent key error = %v, want nil", err)
	}
}

// TestKVStore_TTL_OverMiniredis pins the server-clock expiry semantics: a
// key set with a positive ttl disappears once the server's own clock passes
// it (miniredis advances its clock deterministically via FastForward), a
// ttl-less key never does, and a Set without a ttl clears an earlier expiry.
func TestKVStore_TTL_OverMiniredis(t *testing.T) {
	mini, client := redisClient(t)
	store := NewKVStore(client)
	ctx := context.Background()

	if err := store.Set(ctx, "brief", []byte("v"), 25*time.Millisecond); err != nil {
		t.Fatalf("Set(brief) error = %v, want nil", err)
	}
	if got := mini.TTL("brief"); got <= 0 || got > 50*time.Millisecond {
		t.Errorf("TTL after a 25ms Set = %v, want roughly 25ms", got)
	}
	mini.FastForward(50 * time.Millisecond)
	if _, ok, err := store.Get(ctx, "brief"); err != nil || ok {
		t.Fatalf("Get(brief) after its expiry = (_, %v, %v), want (_, false, nil)", ok, err)
	}

	// Set without a ttl clears the expiry a previous Set attached.
	if err := store.Set(ctx, "brief", []byte("v"), 25*time.Millisecond); err != nil {
		t.Fatalf("Set(brief, ttl) error = %v, want nil", err)
	}
	if err := store.Set(ctx, "brief", []byte("v"), 0); err != nil {
		t.Fatalf("Set(brief, no ttl) error = %v, want nil", err)
	}
	if got := mini.TTL("brief"); got != 0 {
		t.Errorf("TTL after the clearing Set = %v, want 0 (no expiry)", got)
	}
	mini.FastForward(100 * time.Millisecond)
	if _, ok, err := store.Get(ctx, "brief"); err != nil || !ok {
		t.Errorf("Get(brief) after the clearing Set = (_, %v, %v), want (_, true, nil)", ok, err)
	}
}

// TestKVStore_IncrByFloat_OverMiniredis drives the increment contract over
// the server's native INCRBYFLOAT: a fresh key starts at zero plus delta
// with no expiry, a live key accumulates and keeps its own expiry, and a
// non-numeric value fails with pkgcore.ErrNotNumeric untouched.
func TestKVStore_IncrByFloat_OverMiniredis(t *testing.T) {
	mini, client := redisClient(t)
	store := NewKVStore(client)
	ctx := context.Background()

	result, err := store.IncrByFloat(ctx, "n", 2.5)
	if err != nil {
		t.Fatalf("IncrByFloat on a fresh key error = %v, want nil", err)
	}
	if result != 2.5 {
		t.Fatalf("IncrByFloat on a fresh key = %v, want 2.5", result)
	}
	if got := mini.TTL("n"); got != 0 {
		t.Errorf("fresh-key TTL = %v, want 0 (no expiry)", got)
	}

	if err := store.Set(ctx, "n", []byte("10"), 45*time.Minute); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}
	if result, err := store.IncrByFloat(ctx, "n", 5); err != nil || result != 15 {
		t.Fatalf("IncrByFloat on a live key = (%v, %v), want (15, nil)", result, err)
	}
	if ttl := mini.TTL("n"); ttl <= 44*time.Minute || ttl > 45*time.Minute {
		t.Errorf("TTL after a live increment = %v, want the key's own expiry preserved", ttl)
	}
	got, ok, err := store.Get(ctx, "n")
	if err != nil || !ok || string(got) != "15" {
		t.Errorf("Get after increments = (%q, %v, %v), want (\"15\", true, nil)", got, ok, err)
	}

	if err := store.Set(ctx, "bad", []byte("not-a-number"), 0); err != nil {
		t.Fatalf("Set(bad) error = %v, want nil", err)
	}
	if _, err := store.IncrByFloat(ctx, "bad", 1); !errors.Is(err, pkgcore.ErrNotNumeric) {
		t.Errorf("IncrByFloat on a non-numeric value error = %v, want pkgcore.ErrNotNumeric", err)
	}
	if got, _ := mini.Get("bad"); got != "not-a-number" {
		t.Errorf("non-numeric value after the failed increment = %q, want it untouched", got)
	}
}

// TestKVStore_IncrByFloatWithTTL_OverMiniredis drives the ttl-attaching
// sibling through its Lua script: a fresh key gets the ttl attached on the
// creating call, and a live key's own expiry is carried through untouched --
// the script must never let the ttl argument shorten it.
func TestKVStore_IncrByFloatWithTTL_OverMiniredis(t *testing.T) {
	mini, client := redisClient(t)
	store := NewKVStore(client)
	ctx := context.Background()

	ttl := 45 * time.Minute
	if _, err := store.IncrByFloatWithTTL(ctx, "n", 3, ttl); err != nil {
		t.Fatalf("IncrByFloatWithTTL on a fresh key error = %v, want nil", err)
	}
	if ttl := mini.TTL("n"); ttl <= 44*time.Minute || ttl > 45*time.Minute {
		t.Errorf("fresh-key TTL = %v, want roughly 45m", ttl)
	}

	if err := store.Set(ctx, "n", []byte("7"), 45*time.Minute); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}
	if result, err := store.IncrByFloatWithTTL(ctx, "n", 1, 10*time.Second); err != nil || result != 8 {
		t.Fatalf("IncrByFloatWithTTL on a live key = (%v, %v), want (8, nil)", result, err)
	}
	if ttl := mini.TTL("n"); ttl <= 44*time.Minute || ttl > 45*time.Minute {
		t.Errorf("TTL after a ttl-argued increment of a live key = %v, want the key's own expiry, never the ttl argument", ttl)
	}

	if err := store.Set(ctx, "bad", []byte("not-a-number"), 0); err != nil {
		t.Fatalf("Set(bad) error = %v, want nil", err)
	}
	if _, err := store.IncrByFloatWithTTL(ctx, "bad", 1, ttl); !errors.Is(err, pkgcore.ErrNotNumeric) {
		t.Errorf("IncrByFloatWithTTL on a non-numeric value error = %v, want pkgcore.ErrNotNumeric", err)
	}
}

// TestKVStore_CompareAndSwap_OverMiniredis drives the compare-and-swap
// script's shapes: set-if-absent for a missing key with an empty old,
// immediate false for a missing key with a non-empty old, a matched live
// swap that preserves the key's expiry, and a mismatched live swap that
// changes nothing.
func TestKVStore_CompareAndSwap_OverMiniredis(t *testing.T) {
	mini, client := redisClient(t)
	store := NewKVStore(client)
	ctx := context.Background()

	swapped, err := store.CompareAndSwap(ctx, "k", nil, []byte("new"))
	if err != nil || !swapped {
		t.Fatalf("CompareAndSwap(set-if-absent) = (%v, %v), want (true, nil)", swapped, err)
	}
	if got, _ := mini.Get("k"); got != "new" {
		t.Errorf("set-if-absent stored %q, want \"new\"", got)
	}
	if got := mini.TTL("k"); got != 0 {
		t.Errorf("set-if-absent TTL = %v, want 0 (no expiry)", got)
	}

	swapped, err = store.CompareAndSwap(ctx, "absent", []byte("old"), []byte("new"))
	if err != nil || swapped {
		t.Fatalf("CompareAndSwap on an absent key with a non-empty old = (%v, %v), want (false, nil)", swapped, err)
	}
	if mini.Exists("absent") {
		t.Error("a mismatched set-if-absent created the key")
	}

	if err := store.Set(ctx, "k", []byte("old"), 45*time.Minute); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}
	before := mini.TTL("k")
	swapped, err = store.CompareAndSwap(ctx, "k", []byte("old"), []byte("new"))
	if err != nil || !swapped {
		t.Fatalf("matched live swap = (%v, %v), want (true, nil)", swapped, err)
	}
	if got, _ := mini.Get("k"); got != "new" {
		t.Errorf("matched swap stored %q, want \"new\"", got)
	}
	if after := mini.TTL("k"); after <= 44*time.Minute || after > before+time.Second {
		t.Errorf("TTL after a matched swap = %v (was %v), want it preserved", after, before)
	}

	swapped, err = store.CompareAndSwap(ctx, "k", []byte("stale"), []byte("other"))
	if err != nil || swapped {
		t.Fatalf("mismatched live swap = (%v, %v), want (false, nil)", swapped, err)
	}
	if got, _ := mini.Get("k"); got != "new" {
		t.Errorf("mismatched swap changed the stored value to %q, want it untouched", got)
	}
}

// TestKVStore_ConcurrentIncrements_OverMiniredis pins the atomicity the
// server's own INCRBYFLOAT provides: concurrent increments of one key
// converge on exactly their sum -- no lost update under real concurrency.
func TestKVStore_ConcurrentIncrements_OverMiniredis(t *testing.T) {
	_, client := redisClient(t)
	store := NewKVStore(client)
	ctx := context.Background()

	const goroutines = 8
	const incrementsPerGoroutine = 25
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < incrementsPerGoroutine; i++ {
				if _, err := store.IncrByFloat(ctx, "counter", 1); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent increment error = %v, want nil", err)
	}

	got, ok, err := store.Get(ctx, "counter")
	const want = goroutines * incrementsPerGoroutine
	if err != nil || !ok || string(got) != strconv.Itoa(want) {
		t.Errorf("final counter = (%q, %v, %v), want (%d, true, nil)", got, ok, err, want)
	}
}

// TestKVStore_CompareAndSwap_SetIfAbsent_SingleWinner pins set-if-absent
// exclusivity under real concurrency: concurrent creators of one key agree
// on exactly one winner, through the script's own atomicity.
func TestKVStore_CompareAndSwap_SetIfAbsent_SingleWinner(t *testing.T) {
	mini, client := redisClient(t)
	store := NewKVStore(client)
	ctx := context.Background()

	const goroutines = 8
	var wg sync.WaitGroup
	wins := make(chan bool, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			swapped, err := store.CompareAndSwap(ctx, "lock", nil, []byte("holder"))
			if err != nil {
				wins <- false
				t.Errorf("CompareAndSwap error = %v, want nil", err)
				return
			}
			wins <- swapped
		}()
	}
	wg.Wait()
	close(wins)
	winners := 0
	for w := range wins {
		if w {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("set-if-absent winners = %d, want exactly 1", winners)
	}
	if got, _ := mini.Get("lock"); got != "holder" {
		t.Errorf("final value = %q, want \"holder\"", got)
	}
}
