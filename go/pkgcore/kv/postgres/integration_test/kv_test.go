//go:build integration

// Integration tests for kvpostgres.NewKVStore: each test drives the store
// through the public KVStore interface against a real PostgreSQL, asserting
// the exact semantics pkgcore's in-memory store's unit tests pin -- TTL
// expiry, IncrByFloat keeping a live key's expiry, CompareAndSwap as
// set-if-absent, expiry untouched by a swap -- plus the atomicity properties
// only a shared server can genuinely exercise: this file's own
// ConcurrentIncrements and ConcurrentCompareAndSwap tests are the point of
// this integration tier -- a single-threaded
// AssertConforms pass alone does not prove IncrByFloat and CompareAndSwap
// are race-free under real concurrent callers.
package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vislake/speed/go/pkgcore"
	kvpostgres "github.com/vislake/speed/go/pkgcore/kv/postgres"
	"github.com/vislake/speed/go/pkgcore/kvstoretest"
)

func TestKVStore_SetGetDelete(t *testing.T) {
	ctx := context.Background()
	kv := kvpostgres.NewKVStore(startPostgresPool(t, ctx))

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
	kv := kvpostgres.NewKVStore(startPostgresPool(t, ctx))

	const key = "session:u-1"
	if err := kv.Set(ctx, key, []byte("token"), 200*time.Millisecond); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}

	if _, found, err := kv.Get(ctx, key); err != nil || !found {
		t.Fatalf("Get() immediately after Set = (found=%v, err=%v), want (true, nil)", found, err)
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
	kv := kvpostgres.NewKVStore(startPostgresPool(t, ctx))

	const key = "config:retry-limit"
	if err := kv.Set(ctx, key, []byte("3"), 200*time.Millisecond); err != nil {
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
	kv := kvpostgres.NewKVStore(startPostgresPool(t, ctx))

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

func TestKVStore_IncrByFloat_ResetsAnExpiredKeyRatherThanAddingToIt(t *testing.T) {
	ctx := context.Background()
	kv := kvpostgres.NewKVStore(startPostgresPool(t, ctx))

	const key = "quota:acme:expired-counter"
	if err := kv.Set(ctx, key, []byte("999"), 100*time.Millisecond); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}
	time.Sleep(300 * time.Millisecond)

	// The key is now expired -- IncrByFloat must treat it as absent (start
	// at delta, not 999+delta) and give it no expiry.
	result, err := kv.IncrByFloat(ctx, key, 2.5)
	if err != nil {
		t.Fatalf("IncrByFloat() on an expired key error = %v, want nil", err)
	}
	if result != 2.5 {
		t.Errorf("IncrByFloat() on an expired key = %v, want 2.5 (reset, not 999+2.5)", result)
	}

	// No expiry: the value must still be there well past the original TTL.
	time.Sleep(300 * time.Millisecond)
	value, found, err := kv.Get(ctx, key)
	if err != nil || !found || string(value) == "" {
		t.Errorf("Get() well after the reset = (%q, %t, %v), want the reset value still present", value, found, err)
	}
}

func TestKVStore_IncrByFloat_KeepsALiveKeysExpiry(t *testing.T) {
	ctx := context.Background()
	kv := kvpostgres.NewKVStore(startPostgresPool(t, ctx))

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
	kv := kvpostgres.NewKVStore(startPostgresPool(t, ctx))

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
	kv := kvpostgres.NewKVStore(startPostgresPool(t, ctx))

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

func TestKVStore_CompareAndSwap_ExpiredKeyMatchesOnlyEmptyOld(t *testing.T) {
	ctx := context.Background()
	kv := kvpostgres.NewKVStore(startPostgresPool(t, ctx))

	const key = "lock:acme:stale-lease"
	if err := kv.Set(ctx, key, []byte("holder-1"), 100*time.Millisecond); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}
	time.Sleep(300 * time.Millisecond)

	// The key is expired, so it is absent for matching purposes: a
	// non-empty old expecting the stale value must NOT match.
	swapped, err := kv.CompareAndSwap(ctx, key, []byte("holder-1"), []byte("holder-2"))
	if err != nil || swapped {
		t.Errorf("CompareAndSwap(non-empty old, expired key) = %t (err %v), want (false, nil)", swapped, err)
	}

	// An empty old matches the expired key (set-if-absent) and gives it a
	// fresh start with no expiry.
	swapped, err = kv.CompareAndSwap(ctx, key, nil, []byte("holder-3"))
	if err != nil || !swapped {
		t.Fatalf("CompareAndSwap(nil old, expired key) = %t (err %v), want (true, nil)", swapped, err)
	}
	value, found, err := kv.Get(ctx, key)
	if err != nil || !found || string(value) != "holder-3" {
		t.Errorf("Get() after resetting an expired key = (%q, %t, %v), want (\"holder-3\", true, nil)", value, found, err)
	}
}

func TestKVStore_CompareAndSwap_NeverChangesTheKeysExpiry(t *testing.T) {
	ctx := context.Background()
	kv := kvpostgres.NewKVStore(startPostgresPool(t, ctx))

	const key = "lock:acme:lease"
	if err := kv.Set(ctx, key, []byte("holder-1"), 250*time.Millisecond); err != nil {
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
	kv := kvpostgres.NewKVStore(startPostgresPool(t, ctx))

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

func TestKVStore_Sweep_RemovesOnlyExpiredRows(t *testing.T) {
	ctx := context.Background()
	kv := kvpostgres.NewKVStore(startPostgresPool(t, ctx))

	if err := kv.Set(ctx, "sweep:expiring", []byte("v"), 100*time.Millisecond); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}
	if err := kv.Set(ctx, "sweep:persistent", []byte("v"), 0); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}
	time.Sleep(300 * time.Millisecond)

	removed, err := kv.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep() error = %v, want nil", err)
	}
	if removed < 1 {
		t.Errorf("Sweep() removed = %d, want at least 1", removed)
	}

	// The no-TTL row must survive the sweep untouched.
	_, found, err := kv.Get(ctx, "sweep:persistent")
	if err != nil || !found {
		t.Errorf("Get() of the persistent key after Sweep = (%t, %v), want (true, nil)", found, err)
	}
}

// TestKVStore_ConcurrentIncrementsLoseNoUpdates is the adversarial
// concurrency proof for IncrByFloat: many goroutines race an increment on
// the very same key, and the final stored value must be exactly the sum of
// every delta -- proof that incrByFloatSQL's single database-arbitrated
// statement is genuinely atomic, not merely correct when called one at a
// time (which a plain AssertConforms pass, run sequentially, could never
// distinguish from a broken read-modify-write).
func TestKVStore_ConcurrentIncrementsLoseNoUpdates(t *testing.T) {
	ctx := context.Background()
	kv := kvpostgres.NewKVStore(startPostgresPool(t, ctx))

	const (
		key        = "quota:acme:concurrent-counter"
		goroutines = 50
		increments = 20
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

// TestKVStore_ConcurrentCompareAndSwapSetIfAbsent_ExactlyOneWinner is the
// adversarial concurrency proof for CompareAndSwap: many goroutines race a
// set-if-absent CompareAndSwap (old=nil) against the very same
// never-before-seen key, and exactly one must report swapped=true -- proof
// that compareAndSwapSQL's UPDATE-then-INSERT-ON-CONFLICT-DO-NOTHING design
// resolves the concurrent-insert race without ever raising an error to a
// losing caller (a raised unique-violation would be a design failure this
// test would catch: swapped=false, err=nil is the only acceptable way to
// lose).
func TestKVStore_ConcurrentCompareAndSwapSetIfAbsent_ExactlyOneWinner(t *testing.T) {
	ctx := context.Background()
	kv := kvpostgres.NewKVStore(startPostgresPool(t, ctx))

	const (
		key        = "lock:acme:concurrent-acquire"
		goroutines = 50
	)

	var wins int64
	var wg sync.WaitGroup
	winningValues := make([]string, goroutines)
	errs := make([]error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			candidate := fmt.Sprintf("holder-%d", g)
			swapped, err := kv.CompareAndSwap(ctx, key, nil, []byte(candidate))
			errs[g] = err
			if swapped {
				atomic.AddInt64(&wins, 1)
				winningValues[g] = candidate
			}
		}(g)
	}
	wg.Wait()

	for g, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: CompareAndSwap() error = %v, want nil (a loser must report swapped=false, err=nil, never an error)", g, err)
		}
	}
	if wins != 1 {
		t.Fatalf("CompareAndSwap() set-if-absent winners = %d, want exactly 1", wins)
	}

	var winningValue string
	for _, v := range winningValues {
		if v != "" {
			winningValue = v
		}
	}
	value, found, err := kv.Get(ctx, key)
	if err != nil || !found {
		t.Fatalf("Get() = (%t, %v), want (true, nil)", found, err)
	}
	if string(value) != winningValue {
		t.Errorf("stored value = %q, want the single winner's value %q", value, winningValue)
	}
}

// TestKVStore_ConformsToKVStoreContract proves kvpostgres.NewKVStore
// satisfies the shared contract kvstoretest.AssertConforms checks -- the
// same suite go/pkgcore's own kv_conformance_test.go runs against
// pkgcore.NewMemoryKVStore, and kv/redis's own integration tier runs
// against a real Redis -- against a real PostgreSQL, so drift between the
// three KVStore implementations under the deployment-composition retrofit's
// N registered implementations per seam is caught here once instead of
// pairwise. Every pair of stores AssertConforms's subtests build sits on
// two independent connection pools to one container (one container per test
// file): the suite's cross-instance assertions -- a value set through one
// instance must be visible through the other -- are the contract-suite form
// of verifying the MultiReplicaSafe bit this implementation declares when
// it registers, and they need genuinely independent connections to mean
// anything. NewKVStore holds no per-instance state of its own -- it is a
// thin wrapper over its pool -- and every subtest derives its own key from
// its own test name (kvstoretest.conformKey), so no two subtests ever
// collide on a row.
func TestKVStore_ConformsToKVStoreContract(t *testing.T) {
	ctx := context.Background()
	poolA := startPostgresPool(t, ctx)
	poolB, err := pgxpool.NewWithConfig(ctx, poolA.Config())
	if err != nil {
		t.Fatalf("pgxpool.NewWithConfig: %v", err)
	}
	t.Cleanup(poolB.Close)

	// The caps argument is this implementation's declaration — the component
	// descriptor (component.go) declares MultiReplicaSafe | SurvivesRestart,
	// and this package's own component_test.go pins the descriptor's
	// declaration to the exported Capabilities constant —
	// so the capability-gated suite runs the cross-instance assertions for
	// the MultiReplicaSafe half of that declaration against the independent
	// pool pair below.
	kvstoretest.AssertConforms(t, pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart, func() (pkgcore.KVStore, pkgcore.KVStore) {
		return kvpostgres.NewKVStore(poolA), kvpostgres.NewKVStore(poolB)
	})
}

// TestKVStore_DeclaredSurvivesRestart_ProvenAgainstContainerRestart runs
// the shared survives-restart protocol (kvstoretest.AssertSurvivesRestart)
// against this implementation's real backend: the component descriptor
// (component.go) declares pkgcore.SurvivesRestart, and this test is the
// verification that the declaration is real -- a value written before a
// genuine restart of the PostgreSQL container must be readable afterwards.
// The protocol drives the restart itself; the closures supply the pieces
// only this backend can: a store over the persistent fixture's pool (which
// redials the fixed advertised address on its own), and a restart that
// stops and starts the container and waits until the pool answers a Ping
// again -- so the post-restart read fails only if the data is genuinely
// gone.
func TestKVStore_DeclaredSurvivesRestart_ProvenAgainstContainerRestart(t *testing.T) {
	ctx := context.Background()
	container, pool := startPostgresPersistent(t, ctx)

	// The caps argument carries the declaration this protocol verifies (the
	// same bits the component descriptor declares and component_test.go pins
	// to the exported Capabilities constant); the
	// protocol refuses a call whose caps do not declare SurvivesRestart, so
	// this run is the SurvivesRestart half of the declaration's
	// verification, its container restart the state-holding-service restart
	// pkgcore.Capability's doc comment names.
	kvstoretest.AssertSurvivesRestart(t, pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart,
		func() pkgcore.KVStore {
			return kvpostgres.NewKVStore(pool)
		},
		func() {
			if err := container.Stop(ctx, nil); err != nil {
				t.Fatalf("stop postgres container: %v", err)
			}
			if err := container.Start(ctx); err != nil {
				t.Fatalf("restart postgres container: %v", err)
			}
			waitForPostgresReady(t, ctx, pool)
		})
}

// TestKVStore_TTLJudgedByTheDatabaseClockNotTheApplicationClock pins the
// single-clock rule of this store's TTL arithmetic: every expiry is
// computed inside PostgreSQL as now() + ttl and judged against
// PostgreSQL's own now(), never written as an absolute instant computed on
// the application clock.
//
// The store under test is deliberately built with an application clock ten
// minutes behind the database's (kvpostgres.WithClock supplies the seam).
// A store that computed the absolute expiry its statements store from that
// clock would see every written expiry already past by the database's
// reckoning: the key would vanish instantly, a security control (a
// rate-limit window, a lockout) failing silently early. A Set with a live
// ttl must therefore still be visible to a plain Get, and the stored row
// must additionally carry an expires_at about ttl after the database's own
// now() -- the assertion that keeps this test protective even against a
// regression that stops consulting the injected clock and goes back to
// time.Now() directly: that code's expiry would trail the database clock
// by the full ten-minute skew and this second check would fail.
//
// The whole test is timing-robust by construction: the skew (ten minutes) is
// enormous next to the ttl (two seconds), so no scheduling jitter can blur
// the distinction between "expired because the skewed write clock said so"
// and "expired because the ttl genuinely elapsed on the database clock".
func TestKVStore_TTLJudgedByTheDatabaseClockNotTheApplicationClock(t *testing.T) {
	ctx := context.Background()
	pool := startPostgresPool(t, ctx)

	var dbNow time.Time
	if err := pool.QueryRow(ctx, "SELECT now()").Scan(&dbNow); err != nil {
		t.Fatalf("read database clock: %v", err)
	}

	const skew = 10 * time.Minute
	skewed := kvpostgres.NewKVStore(pool, kvpostgres.WithClock(func() time.Time {
		return dbNow.Add(-skew)
	}))
	plain := kvpostgres.NewKVStore(pool)

	const ttl = 2 * time.Second

	// Set on the skewed store: before the one-clock fix the expiry it stored
	// was (dbNow - 10min) + ttl, already past when the database judged it.
	const setKey = "ratelimit:acme:lockout"
	if err := skewed.Set(ctx, setKey, []byte("locked"), ttl); err != nil {
		t.Fatalf("Set() on the skewed store error = %v, want nil", err)
	}
	value, found, err := plain.Get(ctx, setKey)
	if err != nil {
		t.Fatalf("Get() error = %v, want nil", err)
	}
	if !found || string(value) != "locked" {
		t.Errorf("Get() = (%q, %t, %v), want (\"locked\", true, nil): a Set with a live ttl must stay visible until the ttl genuinely elapses on the database clock, whatever the application clock says", value, found, err)
	}
	assertRemainingTTL(t, ctx, pool, setKey, ttl)

	// IncrByFloatWithTTL creating a fresh key on the skewed store: the same
	// one-clock property on the expiry-attaching increment path.
	const counterKey = "ratelimit:acme:login-attempts"
	if _, err := skewed.IncrByFloatWithTTL(ctx, counterKey, 1, ttl); err != nil {
		t.Fatalf("IncrByFloatWithTTL() on the skewed store error = %v, want nil", err)
	}
	value, found, err = plain.Get(ctx, counterKey)
	if err != nil {
		t.Fatalf("Get() error = %v, want nil", err)
	}
	if !found || string(value) != "1" {
		t.Errorf("Get() = (%q, %t, %v), want (\"1\", true, nil): IncrByFloatWithTTL's fresh-key expiry must be anchored to the database clock too", value, found, err)
	}
	assertRemainingTTL(t, ctx, pool, counterKey, ttl)

	// And the key genuinely dies once the ttl has elapsed on the database
	// clock -- neither longer (the skew direction that would keep a security
	// lockout alive past its window) nor shorter.
	time.Sleep(ttl + 500*time.Millisecond)
	for _, key := range []string{setKey, counterKey} {
		if _, found, err := plain.Get(ctx, key); err != nil {
			t.Fatalf("Get(%q) error = %v, want nil", key, err)
		} else if found {
			t.Errorf("Get(%q) found the key after ttl+500ms elapsed on the database clock, want it treated as absent", key)
		}
	}
}

// assertRemainingTTL pins that key's stored expires_at is about ttl ahead of
// the database's own now() -- the second, DB-visible half of the one-clock
// property (see the regression test above for why it is load-bearing on its
// own).
func assertRemainingTTL(t *testing.T, ctx context.Context, pool *pgxpool.Pool, key string, ttl time.Duration) {
	t.Helper()

	var remaining time.Duration
	err := pool.QueryRow(ctx,
		`SELECT expires_at - now() FROM pkgcore_kv_entries WHERE key = $1`, key).Scan(&remaining)
	if err != nil {
		t.Fatalf("read remaining ttl of %q: %v", key, err)
	}
	if remaining <= 0 || remaining > ttl {
		t.Errorf("row %q expires_at - now() = %v, want in (0, %v]: the expiry must be anchored to the database clock, not an application-clock instant (a skewed one would land far outside this window)", key, remaining, ttl)
	}
}
