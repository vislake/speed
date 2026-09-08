//go:build integration

package memcached_test

// Integration tests for kvmemcached.NewKVStore: each test drives the store
// through the public KVStore interface against a real Memcached, asserting
// the exact semantics pkgcore's in-memory store's unit tests and kv/redis's
// own integration tier pin -- TTL expiry (here enforced by this store's own
// logical envelope rather than Memcached's coarse whole-second native one,
// see the package doc comment), IncrByFloat keeping a live key's expiry,
// CompareAndSwap as set-if-absent, expiry untouched by a swap -- plus the
// atomicity properties only a real server's compare-and-swap primitives can
// exercise honestly: Memcached has no atomic float increment and no atomic
// whole-value compare at the protocol level, so both IncrByFloat and
// CompareAndSwap are compare-and-swap retry loops over Memcached's native
// gets/cas/add, and the two adversarial concurrency tests at the bottom of
// this file are the real proof that loop never loses an update or lets two
// racing set-if-absent calls both win.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bradfitz/gomemcache/memcache"

	"github.com/vislake/speed/go/pkgcore"
	kvmemcached "github.com/vislake/speed/go/pkgcore/kv/memcached"
	"github.com/vislake/speed/go/pkgcore/kvstoretest"
)

func TestKVStore_SetGetDelete(t *testing.T) {
	ctx := context.Background()
	kv := kvmemcached.NewKVStore(startMemcachedClient(t, ctx))

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

// TestKVStore_SetWithTTL_ExpiresTheKey pins the whole reason this store
// keeps its own logical expiry envelope: a ttl far below Memcached's
// whole-second native granularity must still expire on schedule, not a
// second-or-more later once the server's own coarse clock catches up.
func TestKVStore_SetWithTTL_ExpiresTheKey(t *testing.T) {
	ctx := context.Background()
	kv := kvmemcached.NewKVStore(startMemcachedClient(t, ctx))

	const key = "session:u-1"
	if err := kv.Set(ctx, key, []byte("token"), 100*time.Millisecond); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}

	if _, found, err := kv.Get(ctx, key); err != nil || !found {
		t.Fatalf("Get() immediately after Set = (found=%t, err=%v), want (true, nil)", found, err)
	}

	time.Sleep(500 * time.Millisecond)

	_, found, err := kv.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get() error = %v, want nil", err)
	}
	if found {
		t.Error("Get() found the key after its (sub-second) TTL elapsed, want it treated as absent")
	}
}

func TestKVStore_SetWithNonPositiveTTL_StoresForever_AndClearsAnExistingExpiry(t *testing.T) {
	ctx := context.Background()
	kv := kvmemcached.NewKVStore(startMemcachedClient(t, ctx))

	const key = "config:retry-limit"
	if err := kv.Set(ctx, key, []byte("3"), 100*time.Millisecond); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}

	// A ttl of zero replaces both value and expiry: the key must outlive the
	// expiry the first Set gave it.
	if err := kv.Set(ctx, key, []byte("5"), 0); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}

	time.Sleep(400 * time.Millisecond)

	value, found, err := kv.Get(ctx, key)
	if err != nil || !found || string(value) != "5" {
		t.Errorf("Get() = (%q, %t, %v), want (\"5\", true, nil) after the old expiry passed", value, found, err)
	}
}

func TestKVStore_IncrByFloat_StartsMissingKeysAtZeroWithoutAnExpiry(t *testing.T) {
	ctx := context.Background()
	kv := kvmemcached.NewKVStore(startMemcachedClient(t, ctx))

	const key = "quota:acme:credits"
	result, err := kv.IncrByFloat(ctx, key, 2.5)
	if err != nil {
		t.Fatalf("IncrByFloat() error = %v, want nil", err)
	}
	if result != 2.5 {
		t.Errorf("IncrByFloat() = %v, want 2.5", result)
	}

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
	kv := kvmemcached.NewKVStore(startMemcachedClient(t, ctx))

	const key = "quota:acme:monthly"
	if err := kv.Set(ctx, key, []byte("10"), 150*time.Millisecond); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}

	result, err := kv.IncrByFloat(ctx, key, 5.5)
	if err != nil || result != 15.5 {
		t.Fatalf("IncrByFloat() = %v (err %v), want 15.5", result, err)
	}

	// An increment is not a refresh: the counter must still die with its
	// rolling window, or a monthly quota would never roll over.
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
	kv := kvmemcached.NewKVStore(startMemcachedClient(t, ctx))

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
	kv := kvmemcached.NewKVStore(startMemcachedClient(t, ctx))

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
	kv := kvmemcached.NewKVStore(startMemcachedClient(t, ctx))

	const key = "lock:acme:lease"
	if err := kv.Set(ctx, key, []byte("holder-1"), 150*time.Millisecond); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
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
	kv := kvmemcached.NewKVStore(startMemcachedClient(t, ctx))

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

// TestKVStore_ConformsToKVStoreContract proves kvmemcached.NewKVStore
// satisfies the shared contract kvstoretest.AssertConforms checks -- the
// same suite go/pkgcore's own kv_conformance_test.go runs against
// pkgcore.NewMemoryKVStore and kv/redis's own integration tier runs against
// a real Redis -- against a real Memcached, so drift between the three
// registered KVStore implementations is caught here once instead of
// pairwise. Every pair of stores AssertConforms's subtests build sits on
// two independent gomemcache clients to one container (one container per
// test file): the suite's cross-instance assertions -- a value set through
// one instance must be visible through the other -- are the contract-suite
// form of verifying the MultiReplicaSafe bit this implementation declares
// when it registers, and they need genuinely independent connections to
// mean anything. NewKVStore holds no per-instance state of its own -- it is
// a thin wrapper over its client, unlike EventBus, which is why no per-store
// cleanup is needed here.
func TestKVStore_ConformsToKVStoreContract(t *testing.T) {
	ctx := context.Background()
	clientA, clientB := startMemcachedClientPair(t, ctx)

	// The caps argument is this implementation's declaration — register.go's
	// init declares MultiReplicaSafe alone (this package's register_test.go
	// pins the registry to return exactly that bit, never SurvivesRestart) —
	// so the capability-gated suite runs the cross-instance assertions for
	// the MultiReplicaSafe half of that declaration against the independent
	// dual-client pair below, and runs no restart protocol: the bit that
	// would demand one is honestly absent, and its absence is pinned by the
	// does-not-survive protocol in the test after this one.
	kvstoretest.AssertConforms(t, pkgcore.MultiReplicaSafe, func() (pkgcore.KVStore, pkgcore.KVStore) {
		return kvmemcached.NewKVStore(clientA), kvmemcached.NewKVStore(clientB)
	})
}

// TestKVStore_DataDoesNotSurviveRestart_ConsistentWithItsHonestDeclaration
// runs the mirror of the shared survives-restart protocol
// (kvstoretest.AssertDoesNotSurviveRestart) against this implementation's
// real backend: register.go honestly declares no SurvivesRestart --
// Memcached is a pure in-memory cache with no persistence mechanism of any
// kind -- and this test verifies the factual basis of that non-declaration:
// a value written before a genuine restart of the Memcached container is
// gone afterwards. A future change that declared SurvivesRestart over this
// backend would contradict a verified loss rather than an unexamined
// assumption.
func TestKVStore_DataDoesNotSurviveRestart_ConsistentWithItsHonestDeclaration(t *testing.T) {
	ctx := context.Background()
	container, hostPort := startMemcachedPersistent(t, ctx)

	// The caps argument carries the declaration whose absence this protocol
	// pins (the same MultiReplicaSafe-only bits register.go's init declares
	// and register_test.go checks); the protocol refuses a call whose caps
	// declare SurvivesRestart, so this run is the negative half of the
	// declaration's verification — the honest non-declaration's factual
	// basis, proven against a genuine restart of the Memcached container
	// itself.
	kvstoretest.AssertDoesNotSurviveRestart(t, pkgcore.MultiReplicaSafe,
		func() pkgcore.KVStore {
			return kvmemcached.NewKVStore(memcache.New(hostPort))
		},
		func() {
			if err := container.Stop(ctx, nil); err != nil {
				t.Fatalf("stop memcached container: %v", err)
			}
			if err := container.Start(ctx); err != nil {
				t.Fatalf("restart memcached container: %v", err)
			}
			waitForMemcachedReady(t, ctx, hostPort)
		})
}

// TestKVStore_ConcurrentIncrementsLoseNoUpdates is the first mandatory
// adversarial proof: many goroutines racing IncrByFloat on the same key
// through Memcached's native gets/cas -- the protocol has no atomic float
// increment at all -- must still land the exact sum with no update lost to
// a missed race, and the bounded compare-and-swap retry loop must not starve
// under this many concurrent goroutines.
func TestKVStore_ConcurrentIncrementsLoseNoUpdates(t *testing.T) {
	ctx := context.Background()
	kv := kvmemcached.NewKVStore(startMemcachedClient(t, ctx))

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

// TestKVStore_ConcurrentCompareAndSwapSetIfAbsent_ExactlyOneWins is the
// second mandatory adversarial proof: many goroutines racing
// CompareAndSwap's set-if-absent form on the same key -- backed by
// Memcached's native "add", which the server itself guarantees succeeds for
// only one caller -- must let exactly one of them win, never zero (every
// attempt spuriously losing to phantom contention) and never more than one
// (two callers both believing they created the key).
func TestKVStore_ConcurrentCompareAndSwapSetIfAbsent_ExactlyOneWins(t *testing.T) {
	ctx := context.Background()
	kv := kvmemcached.NewKVStore(startMemcachedClient(t, ctx))

	const (
		key        = "lock:acme:migration"
		goroutines = 30
	)

	var wins int64
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			winner := []byte("winner-" + strconv.Itoa(g))
			swapped, err := kv.CompareAndSwap(ctx, key, nil, winner)
			if err != nil {
				t.Errorf("CompareAndSwap() error = %v, want nil", err)
				return
			}
			if swapped {
				atomic.AddInt64(&wins, 1)
			}
		}()
	}
	wg.Wait()

	if wins != 1 {
		t.Errorf("winners = %d, want exactly 1", wins)
	}

	if _, found, err := kv.Get(ctx, key); err != nil || !found {
		t.Errorf("Get() after the race = (%t, %v), want (true, nil): exactly one goroutine's write must have landed", found, err)
	}
}

// TestKVStore_ReviveRaceInTheTTLFlipWindow_LosesNoIncrement pins the
// revive rule of the TTL-flip window: when a key sits in that window --
// logically expired by its envelope, but still physically present because
// Memcached's own eviction runs on the whole-second exptime the write
// scheduled -- readers must not delete the expired row unconditionally. A
// Delete issued from a stale read could land after a concurrent writer has
// already revived the key with its fresh value, silently destroying that
// writer's increment: two (or more) concurrent IncrByFloatWithTTL reviving
// the key in the same window would report their own private "1"s and the
// stored total would lose every increment but one.
//
// Every revive is routed through Memcached's own compare-and-swap at
// the token the stale read returned, so a concurrent writer who revived the
// key between the read and the revive simply wins the race and the loser
// retries from a fresh read -- no write is ever clobbered. This test drives
// the exact window: each round seeds a key that expires in a few tens of
// milliseconds, waits until it is logically dead but physically present, and
// releases workers simultaneously to revive it with IncrByFloatWithTTL. The
// final stored value must equal the number of workers, every round; the
// whole flip window is roughly a second wide (the physical exptime rounds up
// to a whole second at write time), so hundreds of rounds race inside it in
// a few seconds.
func TestKVStore_ReviveRaceInTheTTLFlipWindow_LosesNoIncrement(t *testing.T) {
	ctx := context.Background()
	kv := kvmemcached.NewKVStore(startMemcachedClient(t, ctx))

	const (
		workers  = 8
		rounds   = 150
		flipTTL  = 40 * time.Millisecond
		flipWait = 55 * time.Millisecond
		// reviveTTL is what the racing revivers attach to the fresh key.
		// It must comfortably outlive the whole round: a reviver whose retry
		// lands after the key it is reviving has flipped again is -- by the
		// KVStore contract -- resetting an *expired* key, which is not the
		// lost-update shape under test. The flip under test is the one the
		// seed's own flipTTL creates, which the round waits out before the
		// revivers start.
		reviveTTL = 2 * time.Second
	)

	for round := 0; round < rounds; round++ {
		key := fmt.Sprintf("flip-window:%d", round)
		if err := kv.Set(ctx, key, []byte("0"), flipTTL); err != nil {
			t.Fatalf("round %d: Set() error = %v, want nil", round, err)
		}
		// Past the logical expiry, well inside the physical-eviction window.
		time.Sleep(flipWait)

		start := make(chan struct{})
		errs := make([]error, workers)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				<-start
				_, errs[w] = kv.IncrByFloatWithTTL(ctx, key, 1, reviveTTL)
			}(w)
		}
		close(start)
		wg.Wait()

		for w, err := range errs {
			if err != nil {
				t.Fatalf("round %d: worker %d IncrByFloatWithTTL() error = %v, want nil", round, w, err)
			}
		}

		value, found, err := kv.Get(ctx, key)
		if err != nil {
			t.Fatalf("round %d: Get() error = %v, want nil", round, err)
		}
		if !found {
			t.Fatalf("round %d: Get() reports the revived key absent", round)
		}
		parsed, err := strconv.ParseFloat(string(value), 64)
		if err != nil {
			t.Fatalf("round %d: stored value %q does not parse: %v", round, value, err)
		}
		if parsed != float64(workers) {
			t.Fatalf("round %d: counter = %v, want %d: a concurrent revive in the TTL-flip window lost an increment", round, parsed, workers)
		}
	}
}
