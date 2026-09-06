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

	"github.com/nats-io/nats.go"

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

// TestKVStore_ReconnectAfterServerRestart_ResumesOperation proves this
// package's declared pkgcore.SurvivesRestart capability is real: a value
// written before the server restarts is still readable after it comes back
// (JetStream's default File storage persists across a stop/start of the same
// container, which never removes its filesystem the way Terminate would),
// and the client -- built with nats.Connect's own default reconnect
// behaviour, untouched -- resumes ordinary operation once reconnected,
// without this package's own code doing anything to notice or recover from
// the disconnect.
func TestKVStore_ReconnectAfterServerRestart_ResumesOperation(t *testing.T) {
	ctx := context.Background()
	container, conn := startNATSConnWithContainer(t, ctx)

	kv, err := kvnats.NewKVStore(ctx, conn, "reconnect-test")
	if err != nil {
		t.Fatalf("NewKVStore() error = %v, want nil", err)
	}

	if err := kv.Set(ctx, "before-restart", []byte("v1"), 0); err != nil {
		t.Fatalf("Set() before restart error = %v, want nil", err)
	}

	reconnected := make(chan struct{}, 1)
	conn.SetReconnectHandler(func(*nats.Conn) {
		select {
		case reconnected <- struct{}{}:
		default:
		}
	})

	if err := container.Stop(ctx, nil); err != nil {
		t.Fatalf("stop nats container: %v", err)
	}
	if err := container.Start(ctx); err != nil {
		t.Fatalf("restart nats container: %v", err)
	}

	select {
	case <-reconnected:
	case <-time.After(30 * time.Second):
		t.Fatal("client did not reconnect within 30s of the server restarting")
	}

	value, found, err := kv.Get(ctx, "before-restart")
	if err != nil || !found || string(value) != "v1" {
		t.Fatalf("Get() after reconnect = (%q, %t, %v), want (\"v1\", true, nil): data must survive the restart", value, found, err)
	}

	if err := kv.Set(ctx, "after-restart", []byte("v2"), 0); err != nil {
		t.Fatalf("Set() after reconnect error = %v, want nil", err)
	}
	value, found, err = kv.Get(ctx, "after-restart")
	if err != nil || !found || string(value) != "v2" {
		t.Errorf("Get() = (%q, %t, %v), want (\"v2\", true, nil)", value, found, err)
	}
}

// TestInit_RegistersKVNatsOnTheSharedRegistry_WithCapabilities is the
// integration-tier counterpart of the package's own unit-level registration
// test: with a real server reachable, Build actually succeeds, so this is
// the one place the exact declared Capability value can be pinned end to
// end, the way kv/redis's own register_test.go pins it without needing a
// real server at all (go-redis's lazy client lets that test succeed with
// nothing listening).
func TestInit_RegistersKVNatsOnTheSharedRegistry_WithCapabilities(t *testing.T) {
	ctx := context.Background()
	conn := startNATSConn(t, ctx)
	uri := conn.ConnectedUrl()
	conn.Close()

	impl, caps, err := pkgcore.KVStoreRegistry.Build("kv.nats", pkgcore.Config{"url": uri, "bucket": "registry-check"})
	if err != nil {
		t.Fatalf("Build(%q) error = %v, want nil", "kv.nats", err)
	}
	if impl == nil {
		t.Error("Build(\"kv.nats\") returned a nil KVStore")
	}
	if want := pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart; caps != want {
		t.Errorf("Build(%q) capabilities = %v, want %v", "kv.nats", caps, want)
	}
}

// TestKVStore_ConformsToKVStoreContract proves kvnats.NewKVStore satisfies
// the shared contract kvstoretest.AssertConforms checks -- the same suite
// go/pkgcore's own kv_conformance_test.go runs against
// pkgcore.NewMemoryKVStore and kv/redis's own integration tier runs against
// a real Redis -- against a real, JetStream-enabled NATS, so drift between
// the three registered KVStore implementations is caught here once instead
// of pairwise. Every store AssertConforms's subtests build shares one store
// instance (backed by one container's one bucket), which is safe because
// kvStore holds no per-instance mutable state of its own.
func TestKVStore_ConformsToKVStoreContract(t *testing.T) {
	kv := newStore(t, context.Background())
	kvstoretest.AssertConforms(t, func() pkgcore.KVStore { return kv })
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
