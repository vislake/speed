//go:build integration

package nats_test

// Integration tests for kvnats.NewKVStore: each test drives the store
// through the public KVStore interface against a real, JetStream-enabled
// NATS server, asserting the exact semantics pkgcore's in-memory store's and
// kv/redis's own unit/integration tests pin -- TTL expiry, IncrByFloat
// keeping a live key's expiry, CompareAndSwap as set-if-absent, expiry
// untouched by a swap -- plus the atomicity properties only a shared server
// can exercise, and the two adversarial concurrency proofs this
// implementation's own hard part (no native atomic increment, no native
// compare-by-value) specifically demands: many goroutines racing
// IncrByFloat on one key losing no update, and many goroutines racing
// CompareAndSwap set-if-absent on one key with exactly one winner.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/vislake/speed/go/pkgcore"
	kvnats "github.com/vislake/speed/go/pkgcore/kv/nats"
	"github.com/vislake/speed/go/pkgcore/kvstoretest"
)

func TestKVStore_SetGetDelete(t *testing.T) {
	ctx := context.Background()
	kv := newStore(t, ctx)

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
	kv := newStore(t, ctx)

	const key = "session:u-1"
	if err := kv.Set(ctx, key, []byte("token"), 200*time.Millisecond); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}
	if _, found, err := kv.Get(ctx, key); err != nil || !found {
		t.Fatalf("Get() immediately after Set = (%t, %v), want (true, nil)", found, err)
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
	kv := newStore(t, ctx)

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
	kv := newStore(t, ctx)

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
	kv := newStore(t, ctx)

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
	kv := newStore(t, ctx)

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
	kv := newStore(t, ctx)

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
	kv := newStore(t, ctx)

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

func TestKVStore_CompareAndSwap_SetIfAbsentAfterExpiry_UsesUpdateNotCreate(t *testing.T) {
	// This pins the specific edge this implementation must get right: a key
	// whose own encoded expiry has passed is logically absent, but the
	// NATS-level message is still live -- the write that resurrects it must
	// go through Update at the still-live revision, never Create, which
	// would fail against a message JetStream itself never considered
	// deleted.
	ctx := context.Background()
	kv := newStore(t, ctx)

	const key = "cache:acme:session-count"
	if err := kv.Set(ctx, key, []byte("stale"), 50*time.Millisecond); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}
	time.Sleep(250 * time.Millisecond)

	swapped, err := kv.CompareAndSwap(ctx, key, nil, []byte("fresh"))
	if err != nil || !swapped {
		t.Fatalf("CompareAndSwap(nil old) after expiry = %t (err %v), want (true, nil)", swapped, err)
	}
	value, found, err := kv.Get(ctx, key)
	if err != nil || !found || string(value) != "fresh" {
		t.Errorf("Get() = (%q, %t, %v), want (\"fresh\", true, nil)", value, found, err)
	}
}

func TestKVStore_IncrByFloat_AfterExpiry_UsesUpdateNotCreate(t *testing.T) {
	// The IncrByFloat twin of the CompareAndSwap test above: a logically
	// expired key must still resolve through Update, since Create against a
	// message JetStream still holds fails ErrKeyExists.
	ctx := context.Background()
	kv := newStore(t, ctx)

	const key = "quota:acme:expired-counter"
	if err := kv.Set(ctx, key, []byte("99"), 50*time.Millisecond); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}
	time.Sleep(250 * time.Millisecond)

	result, err := kv.IncrByFloat(ctx, key, 3)
	if err != nil {
		t.Fatalf("IncrByFloat() after expiry error = %v, want nil", err)
	}
	if result != 3 {
		t.Errorf("IncrByFloat() after expiry = %v, want 3 (starting fresh, not 99+3)", result)
	}
}

func TestKVStore_CarriesBinaryValues(t *testing.T) {
	ctx := context.Background()
	kv := newStore(t, ctx)

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

	swapped, err := kv.CompareAndSwap(ctx, key, blob, []byte("text now"))
	if err != nil || !swapped {
		t.Fatalf("CompareAndSwap(binary old) = %t (err %v), want (true, nil)", swapped, err)
	}
}

// TestKVStore_ConcurrentIncrementsLoseNoUpdates is the first of this
// implementation's two mandatory adversarial concurrency proofs: many
// goroutines racing IncrByFloat's compare-and-swap retry loop on the very
// same key, none of them losing an update, and none of them exhausting the
// retry budget under this realistic contention -- if any goroutine's
// IncrByFloat call returned an error, the test would have already failed
// above the final sum assertion.
func TestKVStore_ConcurrentIncrementsLoseNoUpdates(t *testing.T) {
	ctx := context.Background()
	kv := newStore(t, ctx)

	const (
		key        = "quota:acme:counter"
		goroutines = 40
		increments = 15
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
// second mandatory adversarial concurrency proof: many goroutines racing
// CompareAndSwap's set-if-absent path on a key none of them has set yet,
// exactly one of which may report swapped=true, with the stored value
// afterward matching exactly that one winner's own write -- the property
// CompareAndSwap's revision-guarded Create is what actually has to
// guarantee, since the read that precedes it is not itself atomic.
func TestKVStore_ConcurrentCompareAndSwapSetIfAbsent_ExactlyOneWins(t *testing.T) {
	ctx := context.Background()
	kv := newStore(t, ctx)

	const (
		key        = "lock:acme:import-race"
		goroutines = 30
	)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
	)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			candidate := []byte(fmt.Sprintf("holder-%d", id))
			swapped, err := kv.CompareAndSwap(ctx, key, nil, candidate)
			if err != nil {
				t.Errorf("CompareAndSwap() error = %v, want nil", err)
				return
			}
			if swapped {
				mu.Lock()
				winners = append(winners, string(candidate))
				mu.Unlock()
			}
		}(g)
	}
	wg.Wait()

	if len(winners) != 1 {
		t.Fatalf("winners = %v, want exactly one goroutine to win the set-if-absent race", winners)
	}

	value, found, err := kv.Get(ctx, key)
	if err != nil || !found {
		t.Fatalf("Get() = (%t, %v), want (true, nil)", found, err)
	}
	if string(value) != winners[0] {
		t.Errorf("stored value = %q, want the winner's own value %q", value, winners[0])
	}
}

// TestKVStore_DeclaredSurvivesRestart_RunsTheSharedVerification proves this
// package's declared pkgcore.SurvivesRestart capability is real by running
// the shared survives-restart protocol (kvstoretest.AssertSurvivesRestart):
// a value written before the server restarts must still be readable after
// it comes back (JetStream's default File storage persists across a
// stop/start of the same container, which never removes its filesystem the
// way Terminate would), and the client -- built with nats.Connect's own
// default reconnect behaviour, untouched -- resumes ordinary operation once
// reconnected, without this package's own code doing anything to notice or
// recover from the disconnect. The protocol drives the restart itself; the
// closures supply the pieces only this backend can: a store over the
// restart-aware fixture's connection, and a restart that stops and starts
// the container and blocks until the connection has reconnected, so the
// protocol's post-restart read happens against a live connection.
func TestKVStore_DeclaredSurvivesRestart_RunsTheSharedVerification(t *testing.T) {
	ctx := context.Background()
	container, conn := startNATSConnWithContainer(t, ctx)

	// The caps argument carries the declaration this protocol verifies (the
	// same bits register.go's init declares and this file's
	// TestInit_RegistersKVNatsOnTheSharedRegistry_WithCapabilities pins);
	// the protocol refuses a call whose caps do not declare SurvivesRestart,
	// so this run is the SurvivesRestart half of the declaration's
	// verification, its container restart the state-holding-service restart
	// pkgcore.Capability's doc comment names — the restart that would expose
	// a MemoryStorage-adopted bucket, which NewKVStore's own refusal (below)
	// makes unreachable through this store.
	kvstoretest.AssertSurvivesRestart(t, pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart,
		func() pkgcore.KVStore {
			kv, err := kvnats.NewKVStore(ctx, conn, "survives-restart-it")
			if err != nil {
				t.Fatalf("NewKVStore() error = %v, want nil", err)
			}
			return kv
		},
		func() {
			restartNATSContainer(t, ctx, container, conn)
		})
}

// TestInit_RegistersKVNatsOnTheSharedRegistry_WithCapabilities is the
// integration-tier counterpart of the package's own unit-level registration
// test: with a real server reachable, Build actually succeeds, so this is
// the one place the exact declared Capability value can be pinned end to
// end, the way kv/redis's own register_test.go pins it without needing a
// real server at all (go-redis's lazy client lets that test succeed with
// nothing listening).
func TestNewKVStore_ConstructsAgainstARealServer_WithCapabilities(t *testing.T) {
	ctx := context.Background()
	conn := startNATSConn(t, ctx)
	defer conn.Close()

	impl, err := kvnats.NewKVStore(ctx, conn, "registry-check")
	if err != nil {
		t.Fatalf("NewKVStore error = %v, want nil", err)
	}
	if impl == nil {
		t.Error("NewKVStore returned a nil KVStore")
	}
	if want := pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart; kvnats.Capabilities != want {
		t.Errorf("Capabilities = %v, want %v", kvnats.Capabilities, want)
	}
}

// TestKVStore_ConformsToKVStoreContract proves kvnats.NewKVStore satisfies
// the shared contract kvstoretest.AssertConforms checks -- the same suite
// go/pkgcore's own kv_conformance_test.go runs against
// pkgcore.NewMemoryKVStore and kv/redis's own integration tier runs against
// a real Redis -- against a real, JetStream-enabled NATS, so drift between
// the three registered KVStore implementations is caught here once instead
// of pairwise. Every pair of stores AssertConforms's subtests build sits on
// two independent connections to one container's one bucket: the suite's
// cross-instance assertions -- a value set through one instance must be
// visible through the other -- are the contract-suite form of verifying the
// MultiReplicaSafe bit this implementation declares when it registers, and
// they need genuinely independent connections to mean anything. kvStore
// holds no per-instance mutable state of its own, which is what makes the
// two connections over one shared bucket safe to reuse across subtests.
func TestKVStore_ConformsToKVStoreContract(t *testing.T) {
	ctx := context.Background()
	connA, connB := startNATSConnPair(t, ctx)

	storeA, err := kvnats.NewKVStore(ctx, connA, "kv-conform-it")
	if err != nil {
		t.Fatalf("NewKVStore() on the first connection error = %v, want nil", err)
	}
	storeB, err := kvnats.NewKVStore(ctx, connB, "kv-conform-it")
	if err != nil {
		t.Fatalf("NewKVStore() on the second connection error = %v, want nil", err)
	}

	// The caps argument is this implementation's declaration — register.go's
	// init declares MultiReplicaSafe | SurvivesRestart (pinned by
	// TestInit_RegistersKVNatsOnTheSharedRegistry_WithCapabilities above) —
	// so the capability-gated suite runs the cross-instance assertions for
	// the MultiReplicaSafe half of that declaration against the independent
	// connection pair below.
	kvstoretest.AssertConforms(t, pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart, func() (pkgcore.KVStore, pkgcore.KVStore) {
		return storeA, storeB
	})
}

// TestKVStore_NewKVStore_RefusesAdoptedMemoryStorageBucket pins the first
// half of the adopt-anything refusal: NewKVStore's probe must not adopt an
// existing bucket unchecked -- a bucket an operator provisioned on memory
// storage (which loses every key it holds the moment the NATS server itself
// restarts) would be silently adopted while this package's registration
// goes on declaring pkgcore.SurvivesRestart, and the distributed-mode
// bootstrap capability check against that declaration would pass on a claim
// the adopted bucket already contradicts (the check exists to fail startup,
// naming the seam, when a composition cannot run in the declared mode;
// adoption would bypass it entirely). Adoption must instead refuse a bucket
// whose configuration cannot satisfy what this
// implementation declares: NewKVStore must fail with the named
// kvnats.ErrUnadoptableBucket rather than hand back a store whose
// persistence the operator never provisioned for, and the refusal must
// leave the operator's bucket exactly as provisioned -- never rewritten
// toward the constructor defaults, never deleted.
func TestKVStore_NewKVStore_RefusesAdoptedMemoryStorageBucket(t *testing.T) {
	ctx := context.Background()
	conn := startNATSConn(t, ctx)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("jetstream.New() error = %v, want nil", err)
	}

	const bucket = "adopt-memory-refused"
	if _, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  bucket,
		Storage: jetstream.MemoryStorage,
	}); err != nil {
		t.Fatalf("CreateKeyValue() error = %v, want nil", err)
	}

	if _, err := kvnats.NewKVStore(ctx, conn, bucket); !errors.Is(err, kvnats.ErrUnadoptableBucket) {
		t.Fatalf("NewKVStore() error = %v, want it to wrap %v: a memory-storage bucket cannot satisfy the declared pkgcore.SurvivesRestart",
			err, kvnats.ErrUnadoptableBucket)
	}

	stream, err := js.Stream(ctx, "KV_"+bucket)
	if err != nil {
		t.Fatalf("read back bucket %q: %v", bucket, err)
	}
	if cfg := stream.CachedInfo().Config; cfg.Storage != jetstream.MemoryStorage {
		t.Errorf("refused bucket storage = %v, want memory untouched: the refusal must not rewrite or delete the operator's bucket", cfg.Storage)
	}
}

// TestKVStore_NewKVStore_RefusesAdoptedBucketLevelTTL is the regression for
// the second half of the adopt-anything hazard: a bucket whose operator gave
// it a whole-bucket TTL was silently adopted too, yet such a bucket
// overthrows the stores-forever contract every KVStore implementation pins --
// Set(k, v, 0) promises "no expiry", and the server would purge the key when
// the bucket's own MaxAge ran out, with nothing in the value's own envelope
// able to outlive a bucket setting (see the package doc comment for why this
// package leaves MaxAge at zero on every bucket it creates). NewKVStore must
// refuse the bucket with the named kvnats.ErrUnadoptableBucket, leaving its
// configuration untouched, rather than adopt a store on which a ttl<=0 write
// silently expires.
func TestKVStore_NewKVStore_RefusesAdoptedBucketLevelTTL(t *testing.T) {
	ctx := context.Background()
	conn := startNATSConn(t, ctx)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("jetstream.New() error = %v, want nil", err)
	}

	const bucket = "adopt-ttl-refused"
	if _, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: bucket,
		TTL:    45 * time.Second,
	}); err != nil {
		t.Fatalf("CreateKeyValue() error = %v, want nil", err)
	}

	if _, err := kvnats.NewKVStore(ctx, conn, bucket); !errors.Is(err, kvnats.ErrUnadoptableBucket) {
		t.Fatalf("NewKVStore() error = %v, want it to wrap %v: a bucket-level TTL silently expires ttl<=0 (stores-forever) writes",
			err, kvnats.ErrUnadoptableBucket)
	}

	stream, err := js.Stream(ctx, "KV_"+bucket)
	if err != nil {
		t.Fatalf("read back bucket %q: %v", bucket, err)
	}
	if cfg := stream.CachedInfo().Config; cfg.MaxAge != 45*time.Second {
		t.Errorf("refused bucket TTL = %v, want 45s untouched: the refusal must not rewrite or delete the operator's bucket", cfg.MaxAge)
	}
}

// TestKVStore_NewKVStore_AdoptsPreProvisionedCompliantBucket is the
// probe-then-adopt regression for the configuration the constructor may
// still adopt: a pre-provisioned bucket is adopted untouched -- never
// re-created or re-configured (the rewrite hazard the probe-first fix
// closed) -- exactly when its configuration satisfies what this
// implementation declares and relies on: file storage (the persistence the
// registered pkgcore.SurvivesRestart claim rests on) and no whole-bucket TTL
// (the envelope design's MaxAge-zero assumption). Every other field an
// operator set deliberately -- this test uses a ten-entry history, an
// explicit single-replica replication factor and a description naming the
// operator -- is preserved byte for byte, since none of them contradicts a
// declared claim (history depth and replication only ever make the bucket
// more capable, never less durable).
//
// The adopted store must also be a working store with the forever contract
// intact: a ttl<=0 write must still live after an envelope-dated write on
// the same store has come and gone, which is exactly the contract a
// bucket-level TTL would have overthrown.
func TestKVStore_NewKVStore_AdoptsPreProvisionedCompliantBucket(t *testing.T) {
	ctx := context.Background()
	conn := startNATSConn(t, ctx)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("jetstream.New() error = %v, want nil", err)
	}

	const bucket = "adopt-compliant"
	if _, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      bucket,
		History:     10,
		Storage:     jetstream.FileStorage,
		Replicas:    1,
		Description: "provisioned by an operator, must survive adoption",
	}); err != nil {
		t.Fatalf("CreateKeyValue() error = %v, want nil", err)
	}

	store, err := kvnats.NewKVStore(ctx, conn, bucket)
	if err != nil {
		t.Fatalf("NewKVStore() error = %v, want nil for a file-storage, no-TTL bucket", err)
	}

	// The tolerated fields must be byte-for-byte what the operator
	// provisioned. Before the probe-first fix, NewKVStore's
	// CreateOrUpdateKeyValue collapsed them all back to its constructor
	// defaults (history 1, no description); the adoption checks must keep the
	// same hands-off promise for everything they do not refuse.
	stream, err := js.Stream(ctx, "KV_"+bucket)
	if err != nil {
		t.Fatalf("read back bucket %q: %v", bucket, err)
	}
	cfg := stream.CachedInfo().Config
	if cfg.MaxMsgsPerSubject != 10 {
		t.Errorf("adopted bucket history = %d, want 10: NewKVStore must not rewrite a pre-provisioned bucket's config", cfg.MaxMsgsPerSubject)
	}
	if cfg.Storage != jetstream.FileStorage {
		t.Errorf("adopted bucket storage = %v, want file: NewKVStore must not rewrite a pre-provisioned bucket's config", cfg.Storage)
	}
	if cfg.Replicas != 1 {
		t.Errorf("adopted bucket replication = %d, want 1: NewKVStore must not rewrite a pre-provisioned bucket's config", cfg.Replicas)
	}
	if cfg.MaxAge != 0 {
		t.Errorf("adopted bucket TTL = %v, want 0: a compliant bucket must carry no whole-bucket TTL", cfg.MaxAge)
	}
	if cfg.Description != "provisioned by an operator, must survive adoption" {
		t.Errorf("adopted bucket description = %q, want the provisioned one: NewKVStore must not rewrite a pre-provisioned bucket's config", cfg.Description)
	}

	// And the adopted store must work with the stores-forever contract
	// intact: the ttl<=0 write must outlive the envelope-dated one.
	if err := store.Set(ctx, "forever", []byte("v"), 0); err != nil {
		t.Fatalf("Set() on the adopted store error = %v, want nil", err)
	}
	if err := store.Set(ctx, "ephemeral", []byte("gone"), 200*time.Millisecond); err != nil {
		t.Fatalf("Set() on the adopted store error = %v, want nil", err)
	}
	time.Sleep(500 * time.Millisecond)

	if value, found, err := store.Get(ctx, "forever"); err != nil || !found || string(value) != "v" {
		t.Errorf("Get(forever) = (%q, %t, %v), want (\"v\", true, nil): a ttl<=0 write on the adopted bucket must not expire", value, found, err)
	}
	if _, found, err := store.Get(ctx, "ephemeral"); err != nil || found {
		t.Errorf("Get(ephemeral) = (%t, %v), want (false, nil) once its envelope-dated expiry passed", found, err)
	}
}

// TestKVStore_NewKVStore_CreatesACompliantBucketFromScratch pins the create
// half of the constructor is untouched by the adoption checks: a genuinely
// absent bucket is still provisioned, and the configuration NewKVStore
// itself creates -- file storage, no whole-bucket TTL -- is exactly the
// configuration the adoption checks accept, so a store built by creating its
// bucket and one built by adopting a pre-provisioned bucket can never diverge
// in what the package's registration declares of them.
func TestKVStore_NewKVStore_CreatesACompliantBucketFromScratch(t *testing.T) {
	ctx := context.Background()
	conn := startNATSConn(t, ctx)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("jetstream.New() error = %v, want nil", err)
	}

	const bucket = "create-compliant"
	store, err := kvnats.NewKVStore(ctx, conn, bucket)
	if err != nil {
		t.Fatalf("NewKVStore() error = %v, want nil", err)
	}
	if err := store.Set(ctx, "k", []byte("v"), 0); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}

	stream, err := js.Stream(ctx, "KV_"+bucket)
	if err != nil {
		t.Fatalf("read back bucket %q: %v", bucket, err)
	}
	cfg := stream.CachedInfo().Config
	if cfg.Storage != jetstream.FileStorage {
		t.Errorf("created bucket storage = %v, want file: the create path must keep provisioning the storage the registered %s claim rests on", cfg.Storage, pkgcore.SurvivesRestart)
	}
	if cfg.MaxAge != 0 {
		t.Errorf("created bucket TTL = %v, want 0: the create path must keep provisioning the MaxAge-zero bucket the envelope design assumes", cfg.MaxAge)
	}
}

// TestKVStore_NewKVStore_RacingProvisionsConvergeOnOneBucket pins the
// provisioning-race path of NewKVStore's probe-then-create: two builders
// racing to provision the same never-before-seen bucket both succeed -- the
// loser's create answers "already in use", and the loser adopts the winner's
// bucket through the same probe an ordinary pre-provisioned bucket takes
// (see bucketNameInUse's own doc comment for the classification). Both
// returned stores must be usable over the one bucket that exists.
func TestKVStore_NewKVStore_RacingProvisionsConvergeOnOneBucket(t *testing.T) {
	ctx := context.Background()
	conn := startNATSConn(t, ctx)

	bucket := "race-provision-" + strconv.FormatInt(time.Now().UnixNano(), 36)

	start := make(chan struct{})
	stores := make([]pkgcore.KVStore, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			stores[i], errs[i] = kvnats.NewKVStore(ctx, conn, bucket)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("builder %d NewKVStore() error = %v, want nil: a provisioning race must converge, not fail", i, err)
		}
	}
	for i, store := range stores {
		if err := store.Set(ctx, fmt.Sprintf("from-%d", i), []byte("v"), 0); err != nil {
			t.Fatalf("builder %d's store Set() error = %v, want nil", i, err)
		}
	}
	// Exactly one bucket must exist, and both stores must read each other's
	// writes through it.
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("jetstream.New() error = %v, want nil", err)
	}
	if _, err := js.Stream(ctx, "KV_"+bucket); err != nil {
		t.Fatalf("read back provisioned bucket: %v", err)
	}
	if value, found, err := stores[1].Get(ctx, "from-0"); err != nil || !found || string(value) != "v" {
		t.Errorf("store 1 Get(from-0) = (%q, %t, %v), want (\"v\", true, nil): both racers must share one bucket", value, found, err)
	}
}

// newStore starts a fresh NATS container and returns a KVStore over a
// provisioned bucket, for the tests in this file that need no direct access
// to the underlying container or connection.
func newStore(t *testing.T, ctx context.Context) pkgcore.KVStore {
	t.Helper()

	conn := startNATSConn(t, ctx)
	kv, err := kvnats.NewKVStore(ctx, conn, "kv-it")
	if err != nil {
		t.Fatalf("NewKVStore() error = %v, want nil", err)
	}
	return kv
}
