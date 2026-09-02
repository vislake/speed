package pkgcore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"
)

const (
	// kvTestKey is the key the single-key cases operate on.
	kvTestKey = "kv-test-key"

	// kvCounterKey is the key the concurrency cases increment.
	kvCounterKey = "kv-test-counter"

	// kvShortTTL is the expiry the timing cases use. It is long enough to
	// survive ordinary scheduling jitter and short enough to keep the suite fast.
	kvShortTTL = 25 * time.Millisecond

	// kvExpiryWait is how long a timing case sleeps before asserting that a key
	// written with kvShortTTL is gone. The 5x margin keeps a loaded machine from
	// turning the assertion into a coin flip.
	kvExpiryWait = 5 * kvShortTTL

	// kvLongTTL outlives the test run, so a key carrying it must still be there.
	kvLongTTL = time.Hour

	// kvConcurrentIncrements is the number of goroutines racing on one counter.
	kvConcurrentIncrements = 50

	// kvIncrementStep is the delta each racing goroutine adds.
	kvIncrementStep = 1

	// kvMixedWorkers and kvMixedOpsPerWorker size the mixed-operation race case.
	kvMixedWorkers      = 16
	kvMixedOpsPerWorker = 25

	// kvStressWriters is the number of goroutines that hammer one key with both
	// Set and Get in TestMemoryKVStore_StressSameKeyGetSet. It is deliberately
	// well above kvMixedWorkers, so the mutex is contended hard enough for the
	// race detector to have something to find.
	kvStressWriters = 128

	// kvStressReaders are goroutines that only ever read, so the stress
	// workload is genuinely mixed rather than every goroutine following the
	// same path.
	kvStressReaders = 64

	// kvStressOps is how many Set/Get pairs each stress writer performs.
	kvStressOps = 40

	// kvStressValueBytes is the size of each distinctive stress value. A long
	// value is what makes a torn write detectable: a partially copied slice
	// would not match any value the test ever wrote.
	kvStressValueBytes = 192

	// kvStressKey is the single key every stress goroutine contends on.
	kvStressKey = "independent.stress.key"

	// kvStressMaxReportedFailures caps how many distinct failures the stress
	// test reports, so a systemic break does not bury the output.
	kvStressMaxReportedFailures = 10
)

// kvInternalStore unwraps the interface to the concrete standalone store, so
// a case can assert on the expiry bookkeeping that the KVStore surface
// deliberately hides. Timing-free assertions about expiry need it: whether an
// expiry was preserved or silently refreshed is invisible from the outside
// until it is too late to test without sleeping.
func kvInternalStore(t *testing.T, store KVStore) *memoryKVStore {
	t.Helper()
	mem, ok := store.(*memoryKVStore)
	if !ok {
		t.Fatalf("NewMemoryKVStore returned %T, want *memoryKVStore", store)
	}
	return mem
}

// kvStoredEntry returns the raw entry behind key, expiry included.
func kvStoredEntry(t *testing.T, store KVStore, key string) (kvEntry, bool) {
	t.Helper()
	mem := kvInternalStore(t, store)
	mem.mu.Lock()
	defer mem.mu.Unlock()
	entry, found := mem.entries[key]
	return entry, found
}

// kvEntryCount reports how many entries the store still holds.
func kvEntryCount(t *testing.T, store KVStore) int {
	t.Helper()
	mem := kvInternalStore(t, store)
	mem.mu.Lock()
	defer mem.mu.Unlock()
	return len(mem.entries)
}

// kvSet stores value under key with no expiry, failing the test on error.
func kvSet(t *testing.T, store KVStore, key, value string) {
	t.Helper()
	if err := store.Set(context.Background(), key, []byte(value), kvNoExpiry); err != nil {
		t.Fatalf("Set(%q): unexpected error: %v", key, err)
	}
}

// kvGet returns the value stored under key as a string, plus whether it exists.
func kvGet(t *testing.T, store KVStore, key string) (string, bool) {
	t.Helper()
	value, found, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get(%q): unexpected error: %v", key, err)
	}
	return string(value), found
}

func TestMemoryKVStore_SetThenGet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		value     []byte
		lookupKey string
		wantValue []byte
		wantFound bool
	}{
		{
			name:      "reads back the stored value",
			value:     []byte("hello"),
			lookupKey: kvTestKey,
			wantValue: []byte("hello"),
			wantFound: true,
		},
		{
			name:      "values are opaque bytes, not text",
			value:     []byte{0x00, 0xff, 0x10, 0x00},
			lookupKey: kvTestKey,
			wantValue: []byte{0x00, 0xff, 0x10, 0x00},
			wantFound: true,
		},
		{
			name:      "an empty value is stored, not reported as absent",
			value:     []byte{},
			lookupKey: kvTestKey,
			wantValue: []byte{},
			wantFound: true,
		},
		{
			name:      "a nil value is stored, not reported as absent",
			value:     nil,
			lookupKey: kvTestKey,
			wantValue: nil,
			wantFound: true,
		},
		{
			name:      "an unknown key is absent without an error",
			value:     []byte("hello"),
			lookupKey: "some-other-key",
			wantFound: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			store := NewMemoryKVStore()
			if err := store.Set(ctx, kvTestKey, tc.value, kvNoExpiry); err != nil {
				t.Fatalf("Set: unexpected error: %v", err)
			}

			got, found, err := store.Get(ctx, tc.lookupKey)
			if err != nil {
				t.Fatalf("Get: unexpected error: %v", err)
			}
			if found != tc.wantFound {
				t.Fatalf("Get(%q) found = %v, want %v", tc.lookupKey, found, tc.wantFound)
			}
			if !found {
				return
			}
			if !bytes.Equal(got, tc.wantValue) {
				t.Errorf("Get(%q) = %v, want %v", tc.lookupKey, got, tc.wantValue)
			}
		})
	}
}

func TestMemoryKVStore_SetOverwritesPreviousValue(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryKVStore()
	kvSet(t, store, kvTestKey, "first")

	if err := store.Set(ctx, kvTestKey, []byte("second"), kvNoExpiry); err != nil {
		t.Fatalf("Set: unexpected error: %v", err)
	}

	got, found := kvGet(t, store, kvTestKey)
	if !found {
		t.Fatal("Get after overwrite: key is absent, want present")
	}
	if got != "second" {
		t.Errorf("Get after overwrite = %q, want %q", got, "second")
	}
	if count := kvEntryCount(t, store); count != 1 {
		t.Errorf("overwriting created a second entry: count = %d, want 1", count)
	}
}

func TestMemoryKVStore_DoesNotAliasCallerSlices(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryKVStore()

	value := []byte("original")
	if err := store.Set(ctx, kvTestKey, value, kvNoExpiry); err != nil {
		t.Fatalf("Set: unexpected error: %v", err)
	}

	// A Redis-backed store copies the bytes onto the wire, so a caller reusing
	// its buffer after Set cannot corrupt what was stored. The demo store has to
	// behave identically or the two deployment modes diverge.
	value[0] = 'X'
	if got, _ := kvGet(t, store, kvTestKey); got != "original" {
		t.Errorf("mutating the caller's buffer changed the stored value: got %q, want %q", got, "original")
	}

	returned, found, err := store.Get(ctx, kvTestKey)
	if err != nil || !found {
		t.Fatalf("Get: found = %v, err = %v, want true, nil", found, err)
	}
	returned[0] = 'Y'
	if got, _ := kvGet(t, store, kvTestKey); got != "original" {
		t.Errorf("mutating the value returned by Get changed the stored value: got %q, want %q", got, "original")
	}
}

func TestMemoryKVStore_SetTTL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		ttl       time.Duration
		wait      time.Duration
		wantFound bool
	}{
		{
			name:      "a positive ttl keeps the key until it elapses",
			ttl:       kvLongTTL,
			wantFound: true,
		},
		{
			name:      "the key is gone once the ttl elapses",
			ttl:       kvShortTTL,
			wait:      kvExpiryWait,
			wantFound: false,
		},
		{
			name:      "a zero ttl means no expiry",
			ttl:       0,
			wait:      kvExpiryWait,
			wantFound: true,
		},
		{
			name:      "a negative ttl means no expiry",
			ttl:       -time.Second,
			wait:      kvExpiryWait,
			wantFound: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			store := NewMemoryKVStore()
			if err := store.Set(ctx, kvTestKey, []byte("value"), tc.ttl); err != nil {
				t.Fatalf("Set: unexpected error: %v", err)
			}
			if tc.wait > 0 {
				time.Sleep(tc.wait)
			}

			_, found, err := store.Get(ctx, kvTestKey)
			if err != nil {
				t.Fatalf("Get: unexpected error: %v", err)
			}
			if found != tc.wantFound {
				t.Errorf("Get after %v with ttl %v: found = %v, want %v", tc.wait, tc.ttl, found, tc.wantFound)
			}
		})
	}
}

func TestMemoryKVStore_GetReclaimsExpiredEntry(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryKVStore()
	if err := store.Set(ctx, kvTestKey, []byte("value"), kvShortTTL); err != nil {
		t.Fatalf("Set: unexpected error: %v", err)
	}
	if count := kvEntryCount(t, store); count != 1 {
		t.Fatalf("before expiry: entry count = %d, want 1", count)
	}

	time.Sleep(kvExpiryWait)

	if _, found := kvGet(t, store, kvTestKey); found {
		t.Fatal("Get after expiry: found = true, want false")
	}
	// Expiry is lazy, so the read itself has to reclaim the entry; otherwise a
	// store used as a cache grows without bound.
	if count := kvEntryCount(t, store); count != 0 {
		t.Errorf("Get left the expired entry behind: entry count = %d, want 0", count)
	}
}

func TestMemoryKVStore_Delete(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		seedKeys    []string
		deleteKey   string
		deletes     int
		wantEntries int
	}{
		{
			name:        "removes an existing key",
			seedKeys:    []string{kvTestKey},
			deleteKey:   kvTestKey,
			deletes:     1,
			wantEntries: 0,
		},
		{
			name:        "deleting a missing key is not an error",
			deleteKey:   kvTestKey,
			deletes:     1,
			wantEntries: 0,
		},
		{
			name:        "deleting twice is not an error",
			seedKeys:    []string{kvTestKey},
			deleteKey:   kvTestKey,
			deletes:     2,
			wantEntries: 0,
		},
		{
			name:        "leaves other keys alone",
			seedKeys:    []string{kvTestKey, "another-key"},
			deleteKey:   kvTestKey,
			deletes:     1,
			wantEntries: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			store := NewMemoryKVStore()
			for _, key := range tc.seedKeys {
				kvSet(t, store, key, "value")
			}

			for i := 0; i < tc.deletes; i++ {
				if err := store.Delete(ctx, tc.deleteKey); err != nil {
					t.Fatalf("Delete call %d: unexpected error: %v", i+1, err)
				}
			}

			if _, found := kvGet(t, store, tc.deleteKey); found {
				t.Errorf("Get(%q) after Delete: found = true, want false", tc.deleteKey)
			}
			if count := kvEntryCount(t, store); count != tc.wantEntries {
				t.Errorf("entry count = %d, want %d", count, tc.wantEntries)
			}
		})
	}
}

func TestMemoryKVStore_IncrByFloat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		seed        string
		seeded      bool
		deltas      []float64
		want        float64
		wantEncoded string
	}{
		{
			name:        "a missing key starts from zero",
			deltas:      []float64{1},
			want:        1,
			wantEncoded: "1",
		},
		{
			name:        "increments accumulate across calls",
			deltas:      []float64{1, 2, 3},
			want:        6,
			wantEncoded: "6",
		},
		{
			name:        "a negative delta decrements",
			deltas:      []float64{10, -2.5},
			want:        7.5,
			wantEncoded: "7.5",
		},
		{
			name:        "a zero delta leaves the value untouched",
			seed:        "3.25",
			seeded:      true,
			deltas:      []float64{0},
			want:        3.25,
			wantEncoded: "3.25",
		},
		{
			name:        "picks up a value written with Set",
			seed:        "41.5",
			seeded:      true,
			deltas:      []float64{0.5},
			want:        42,
			wantEncoded: "42",
		},
		{
			// The want value is spelled out as a literal on purpose. Writing it
			// as 0.1 + 0.2 would be wrong: the compiler folds that constant
			// expression in exact arithmetic and yields 0.3, which is not what
			// adding the two float64 values at run time produces. The literal
			// below is the real sum, and it only survives a store-and-reload
			// because the value is encoded at full round-trip precision - an
			// encoding such as %.6f would come back as 0.3 and fail here.
			name:        "no precision is lost between increments",
			deltas:      []float64{0.1, 0.2},
			want:        0.30000000000000004,
			wantEncoded: "0.30000000000000004",
		},
		{
			// The second, zero delta forces the encoded value to be parsed back,
			// which proves the exponent form the encoding falls back to for large
			// magnitudes is readable by the store itself.
			name:        "large magnitudes round-trip through the encoding",
			deltas:      []float64{1234567890.123456789, 0},
			want:        1234567890.123456789,
			wantEncoded: "1.2345678901234567e+09",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			store := NewMemoryKVStore()
			if tc.seeded {
				kvSet(t, store, kvCounterKey, tc.seed)
			}

			var got float64
			for i, delta := range tc.deltas {
				var err error
				got, err = store.IncrByFloat(ctx, kvCounterKey, delta)
				if err != nil {
					t.Fatalf("IncrByFloat call %d (delta %v): unexpected error: %v", i+1, delta, err)
				}
			}

			// Exact comparison is the point: the result must be the float64 the
			// arithmetic produced, not something rounded by the storage format.
			if got != tc.want {
				t.Errorf("IncrByFloat = %v, want %v", got, tc.want)
			}

			encoded, found := kvGet(t, store, kvCounterKey)
			if !found {
				t.Fatal("Get after IncrByFloat: key is absent, want present")
			}
			if encoded != tc.wantEncoded {
				t.Errorf("stored encoding = %q, want %q", encoded, tc.wantEncoded)
			}
			reparsed, err := strconv.ParseFloat(encoded, kvFloatBitSize)
			if err != nil {
				t.Fatalf("stored encoding %q does not parse back: %v", encoded, err)
			}
			if reparsed != tc.want {
				t.Errorf("stored encoding %q parses back to %v, want %v", encoded, reparsed, tc.want)
			}
		})
	}
}

func TestMemoryKVStore_IncrByFloatRejectsNonNumericValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		seed string
	}{
		{name: "plain text", seed: "not-a-number"},
		{name: "an empty value", seed: ""},
		{name: "a number with trailing text", seed: "1.5kg"},
		{name: "binary data", seed: "\x00\x01\x02"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			store := NewMemoryKVStore()
			kvSet(t, store, kvCounterKey, tc.seed)

			got, err := store.IncrByFloat(ctx, kvCounterKey, kvIncrementStep)
			if !errors.Is(err, ErrNotNumeric) {
				t.Fatalf("IncrByFloat on %q: error = %v, want ErrNotNumeric", tc.seed, err)
			}
			if got != 0 {
				t.Errorf("IncrByFloat returned %v alongside an error, want 0", got)
			}

			// A failed increment must not damage what is already there.
			value, found := kvGet(t, store, kvCounterKey)
			if !found {
				t.Fatal("the failed increment deleted the key")
			}
			if value != tc.seed {
				t.Errorf("the failed increment rewrote the value: got %q, want %q", value, tc.seed)
			}
		})
	}
}

func TestMemoryKVStore_IncrByFloatExpiry(t *testing.T) {
	t.Parallel()

	t.Run("a counter created by the increment has no expiry", func(t *testing.T) {
		t.Parallel()

		store := NewMemoryKVStore()
		if _, err := store.IncrByFloat(context.Background(), kvCounterKey, kvIncrementStep); err != nil {
			t.Fatalf("IncrByFloat: unexpected error: %v", err)
		}

		entry, found := kvStoredEntry(t, store, kvCounterKey)
		if !found {
			t.Fatal("the counter was not stored")
		}
		if !entry.expiresAt.IsZero() {
			t.Errorf("the new counter expires at %v, want no expiry", entry.expiresAt)
		}
	})

	t.Run("an existing expiry is preserved, not extended", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		store := NewMemoryKVStore()
		if err := store.Set(ctx, kvCounterKey, []byte("1"), kvLongTTL); err != nil {
			t.Fatalf("Set: unexpected error: %v", err)
		}
		before, found := kvStoredEntry(t, store, kvCounterKey)
		if !found {
			t.Fatal("the counter was not stored")
		}

		if _, err := store.IncrByFloat(ctx, kvCounterKey, kvIncrementStep); err != nil {
			t.Fatalf("IncrByFloat: unexpected error: %v", err)
		}

		after, found := kvStoredEntry(t, store, kvCounterKey)
		if !found {
			t.Fatal("the counter disappeared after the increment")
		}
		// Comparing the instants rather than sleeping catches the subtle bug: an
		// increment that refreshes the window would keep a rate-limit counter
		// alive forever.
		if !after.expiresAt.Equal(before.expiresAt) {
			t.Errorf("IncrByFloat moved the expiry from %v to %v, want it unchanged", before.expiresAt, after.expiresAt)
		}
	})

	t.Run("an expired counter restarts from zero without an expiry", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		store := NewMemoryKVStore()
		if err := store.Set(ctx, kvCounterKey, []byte("100"), kvShortTTL); err != nil {
			t.Fatalf("Set: unexpected error: %v", err)
		}

		time.Sleep(kvExpiryWait)

		got, err := store.IncrByFloat(ctx, kvCounterKey, kvIncrementStep)
		if err != nil {
			t.Fatalf("IncrByFloat: unexpected error: %v", err)
		}
		if got != kvIncrementStep {
			t.Errorf("IncrByFloat after expiry = %v, want %v: the expired value must not survive", got, float64(kvIncrementStep))
		}

		entry, found := kvStoredEntry(t, store, kvCounterKey)
		if !found {
			t.Fatal("the counter was not stored")
		}
		if !entry.expiresAt.IsZero() {
			t.Errorf("the restarted counter expires at %v, want no expiry", entry.expiresAt)
		}
	})
}

func TestMemoryKVStore_CompareAndSwap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		seed        string
		seeded      bool
		old         []byte
		newVal      []byte
		wantSwapped bool
		wantValue   string
		wantFound   bool
	}{
		{
			name:        "an absent key accepts a nil expectation, so it works as set-if-absent",
			old:         nil,
			newVal:      []byte("first"),
			wantSwapped: true,
			wantValue:   "first",
			wantFound:   true,
		},
		{
			name:        "an absent key accepts an empty expectation",
			old:         []byte{},
			newVal:      []byte("first"),
			wantSwapped: true,
			wantValue:   "first",
			wantFound:   true,
		},
		{
			name:        "an absent key refuses a non-empty expectation",
			old:         []byte("ghost"),
			newVal:      []byte("first"),
			wantSwapped: false,
			wantFound:   false,
		},
		{
			name:        "a matching value is replaced",
			seed:        "current",
			seeded:      true,
			old:         []byte("current"),
			newVal:      []byte("next"),
			wantSwapped: true,
			wantValue:   "next",
			wantFound:   true,
		},
		{
			name:        "a mismatching value is left alone without an error",
			seed:        "current",
			seeded:      true,
			old:         []byte("stale"),
			newVal:      []byte("next"),
			wantSwapped: false,
			wantValue:   "current",
			wantFound:   true,
		},
		{
			name:        "the comparison is byte-for-byte, not by prefix",
			seed:        "current",
			seeded:      true,
			old:         []byte("curr"),
			newVal:      []byte("next"),
			wantSwapped: false,
			wantValue:   "current",
			wantFound:   true,
		},
		{
			name:        "an existing key does not match an empty expectation",
			seed:        "current",
			seeded:      true,
			old:         nil,
			newVal:      []byte("next"),
			wantSwapped: false,
			wantValue:   "current",
			wantFound:   true,
		},
		{
			name:        "a stored empty value matches an empty expectation",
			seed:        "",
			seeded:      true,
			old:         nil,
			newVal:      []byte("next"),
			wantSwapped: true,
			wantValue:   "next",
			wantFound:   true,
		},
		{
			name:        "a value can be swapped to empty",
			seed:        "current",
			seeded:      true,
			old:         []byte("current"),
			newVal:      []byte{},
			wantSwapped: true,
			wantValue:   "",
			wantFound:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			store := NewMemoryKVStore()
			if tc.seeded {
				kvSet(t, store, kvTestKey, tc.seed)
			}

			swapped, err := store.CompareAndSwap(ctx, kvTestKey, tc.old, tc.newVal)
			if err != nil {
				t.Fatalf("CompareAndSwap: unexpected error: %v", err)
			}
			if swapped != tc.wantSwapped {
				t.Fatalf("CompareAndSwap = %v, want %v", swapped, tc.wantSwapped)
			}

			value, found := kvGet(t, store, kvTestKey)
			if found != tc.wantFound {
				t.Fatalf("Get after CompareAndSwap: found = %v, want %v", found, tc.wantFound)
			}
			if found && value != tc.wantValue {
				t.Errorf("Get after CompareAndSwap = %q, want %q", value, tc.wantValue)
			}
		})
	}
}

func TestMemoryKVStore_CompareAndSwapDoesNotAliasCallerSlice(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryKVStore()

	newVal := []byte("swapped")
	swapped, err := store.CompareAndSwap(ctx, kvTestKey, nil, newVal)
	if err != nil || !swapped {
		t.Fatalf("CompareAndSwap = %v, err = %v, want true, nil", swapped, err)
	}

	newVal[0] = 'X'
	if got, _ := kvGet(t, store, kvTestKey); got != "swapped" {
		t.Errorf("mutating the caller's buffer changed the stored value: got %q, want %q", got, "swapped")
	}
}

func TestMemoryKVStore_CompareAndSwapExpiry(t *testing.T) {
	t.Parallel()

	t.Run("a swap leaves the expiry untouched", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		store := NewMemoryKVStore()
		if err := store.Set(ctx, kvTestKey, []byte("current"), kvLongTTL); err != nil {
			t.Fatalf("Set: unexpected error: %v", err)
		}
		before, found := kvStoredEntry(t, store, kvTestKey)
		if !found {
			t.Fatal("the key was not stored")
		}

		swapped, err := store.CompareAndSwap(ctx, kvTestKey, []byte("current"), []byte("next"))
		if err != nil || !swapped {
			t.Fatalf("CompareAndSwap = %v, err = %v, want true, nil", swapped, err)
		}

		after, found := kvStoredEntry(t, store, kvTestKey)
		if !found {
			t.Fatal("the key disappeared after the swap")
		}
		if !after.expiresAt.Equal(before.expiresAt) {
			t.Errorf("CompareAndSwap moved the expiry from %v to %v, want it unchanged", before.expiresAt, after.expiresAt)
		}
	})

	t.Run("a key created by a swap has no expiry", func(t *testing.T) {
		t.Parallel()

		store := NewMemoryKVStore()
		swapped, err := store.CompareAndSwap(context.Background(), kvTestKey, nil, []byte("first"))
		if err != nil || !swapped {
			t.Fatalf("CompareAndSwap = %v, err = %v, want true, nil", swapped, err)
		}

		entry, found := kvStoredEntry(t, store, kvTestKey)
		if !found {
			t.Fatal("the key was not stored")
		}
		if !entry.expiresAt.IsZero() {
			t.Errorf("the new key expires at %v, want no expiry", entry.expiresAt)
		}
	})

	t.Run("an expired key counts as absent", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		store := NewMemoryKVStore()
		if err := store.Set(ctx, kvTestKey, []byte("current"), kvShortTTL); err != nil {
			t.Fatalf("Set: unexpected error: %v", err)
		}

		time.Sleep(kvExpiryWait)

		swapped, err := store.CompareAndSwap(ctx, kvTestKey, []byte("current"), []byte("next"))
		if err != nil {
			t.Fatalf("CompareAndSwap: unexpected error: %v", err)
		}
		if swapped {
			t.Error("CompareAndSwap matched the value of an expired key, want no match")
		}

		swapped, err = store.CompareAndSwap(ctx, kvTestKey, nil, []byte("fresh"))
		if err != nil {
			t.Fatalf("CompareAndSwap: unexpected error: %v", err)
		}
		if !swapped {
			t.Fatal("set-if-absent failed on an expired key, want it to succeed")
		}
		if got, _ := kvGet(t, store, kvTestKey); got != "fresh" {
			t.Errorf("Get after set-if-absent = %q, want %q", got, "fresh")
		}
	})
}

func TestMemoryKVStore_ConcurrentIncrByFloatLosesNoIncrement(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryKVStore()

	var wg sync.WaitGroup
	results := make(chan float64, kvConcurrentIncrements)
	errs := make(chan error, kvConcurrentIncrements)

	wg.Add(kvConcurrentIncrements)
	for i := 0; i < kvConcurrentIncrements; i++ {
		go func() {
			defer wg.Done()
			value, err := store.IncrByFloat(ctx, kvCounterKey, kvIncrementStep)
			if err != nil {
				errs <- err
				return
			}
			results <- value
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatalf("IncrByFloat: unexpected error: %v", err)
	}

	// Every caller must have observed a different intermediate total: a lost
	// update would show up as two callers reading the same number.
	seen := make(map[float64]int, kvConcurrentIncrements)
	for value := range results {
		seen[value]++
	}
	for i := 1; i <= kvConcurrentIncrements; i++ {
		want := float64(i * kvIncrementStep)
		if count := seen[want]; count != 1 {
			t.Errorf("intermediate total %v was returned %d times, want exactly 1", want, count)
		}
	}

	encoded, found := kvGet(t, store, kvCounterKey)
	if !found {
		t.Fatal("the counter is absent after the increments")
	}
	total, err := strconv.ParseFloat(encoded, kvFloatBitSize)
	if err != nil {
		t.Fatalf("the counter %q does not parse: %v", encoded, err)
	}
	if want := float64(kvConcurrentIncrements * kvIncrementStep); total != want {
		t.Errorf("counter = %v, want %v", total, want)
	}
}

func TestMemoryKVStore_ConcurrentMixedOperations(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryKVStore()
	keys := []string{"key-a", "key-b", "key-c"}

	var wg sync.WaitGroup
	errs := make(chan error, kvMixedWorkers)

	wg.Add(kvMixedWorkers)
	for worker := 0; worker < kvMixedWorkers; worker++ {
		go func() {
			defer wg.Done()
			// Workers deliberately share keys, so every method contends with
			// every other one; under -race this is what proves the mutex covers
			// all five, not just the increment path.
			key := keys[worker%len(keys)]
			for op := 0; op < kvMixedOpsPerWorker; op++ {
				value := []byte(strconv.Itoa(op))
				if err := store.Set(ctx, key, value, kvLongTTL); err != nil {
					errs <- err
					return
				}
				if _, _, err := store.Get(ctx, key); err != nil {
					errs <- err
					return
				}
				if _, err := store.CompareAndSwap(ctx, key, value, []byte("swapped")); err != nil {
					errs <- err
					return
				}
				if _, err := store.IncrByFloat(ctx, kvCounterKey, kvIncrementStep); err != nil {
					errs <- err
					return
				}
				if err := store.Delete(ctx, key); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("mixed worker: unexpected error: %v", err)
	}

	// The counter is on its own key, so churn on the shared keys must not cost
	// it a single increment.
	encoded, found := kvGet(t, store, kvCounterKey)
	if !found {
		t.Fatal("the counter is absent after the mixed run")
	}
	total, err := strconv.ParseFloat(encoded, kvFloatBitSize)
	if err != nil {
		t.Fatalf("the counter %q does not parse: %v", encoded, err)
	}
	if want := float64(kvMixedWorkers * kvMixedOpsPerWorker * kvIncrementStep); total != want {
		t.Errorf("counter = %v, want %v", total, want)
	}
}

// kvStressValues builds one distinctive value per writer, together with the
// set of values that are legal for a reader to observe. Each value is a long
// run of a two-byte token unique to its writer, so any interleaving of two
// writers' bytes produces something outside the set.
func kvStressValues(n int) ([][]byte, map[string]struct{}) {
	values := make([][]byte, n)
	legal := make(map[string]struct{}, n)
	for i := range n {
		token := []byte{byte('A' + i%26), byte('a' + i/26)}
		v := bytes.Repeat(token, kvStressValueBytes/len(token))
		values[i] = v
		legal[string(v)] = struct{}{}
	}
	return values, legal
}

// TestMemoryKVStore_StressSameKeyGetSet drives 128 writers and 64 readers at a
// single key at once. Under -race it proves the store serialises every path;
// independently of the race detector, the value check proves no reader can
// ever observe a half-written or aliased value.
func TestMemoryKVStore_StressSameKeyGetSet(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryKVStore()
	values, legal := kvStressValues(kvStressWriters)

	// Seed the key so that a reader starting first still finds a legal value
	// and the "found" assertion below is meaningful from the very first read.
	if err := store.Set(ctx, kvStressKey, values[0], kvNoExpiry); err != nil {
		t.Fatalf("seed Set: %v", err)
	}

	var (
		mu       sync.Mutex
		failures []string
		reads    int
		absent   int
		observed = make(map[string]struct{}, kvStressWriters)
	)
	// t.Fatalf may not be called from a non-test goroutine, so failures are
	// funnelled back and reported from the test goroutine after the join.
	record := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if len(failures) < kvStressMaxReportedFailures {
			failures = append(failures, fmt.Sprintf(format, args...))
		}
	}
	// check validates one observation and folds it into the shared tallies.
	check := func(got []byte, found bool, err error, who string) bool {
		if err != nil {
			record("%s: Get returned an error: %v", who, err)
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		reads++
		if !found {
			// The key is written without a TTL and never deleted, so it must
			// never vanish once seeded.
			absent++
			return true
		}
		if _, ok := legal[string(got)]; !ok {
			if len(failures) < kvStressMaxReportedFailures {
				failures = append(failures, fmt.Sprintf(
					"%s: Get returned a value no goroutine ever wrote (len %d, want %d)",
					who, len(got), kvStressValueBytes))
			}
			return false
		}
		observed[string(got)] = struct{}{}
		return true
	}

	var wg sync.WaitGroup
	wg.Add(kvStressWriters + kvStressReaders)

	for i := range kvStressWriters {
		go func() {
			defer wg.Done()
			mine := values[i]
			for op := range kvStressOps {
				if err := store.Set(ctx, kvStressKey, mine, kvNoExpiry); err != nil {
					record("writer %d op %d: Set returned an error: %v", i, op, err)
					return
				}
				got, found, err := store.Get(ctx, kvStressKey)
				if !check(got, found, err, fmt.Sprintf("writer %d op %d", i, op)) {
					return
				}
			}
		}()
	}

	for r := range kvStressReaders {
		go func() {
			defer wg.Done()
			for op := range kvStressOps {
				got, found, err := store.Get(ctx, kvStressKey)
				if !check(got, found, err, fmt.Sprintf("reader %d op %d", r, op)) {
					return
				}
			}
		}()
	}

	wg.Wait()

	for _, f := range failures {
		t.Error(f)
	}
	if absent != 0 {
		t.Errorf("Get reported the key absent %d times; a key set without a TTL must stay present", absent)
	}
	if want := (kvStressWriters + kvStressReaders) * kvStressOps; reads != want {
		t.Errorf("completed reads = %d, want %d; some goroutine bailed out early", reads, want)
	}
	// If the store were serialising everything behind one writer the readers
	// would only ever see a single value. Seeing many proves the goroutines
	// genuinely interleaved, so the run above was a real race exercise.
	if len(observed) < 2 {
		t.Errorf("readers observed %d distinct values, want at least 2: the goroutines did not actually interleave", len(observed))
	}

	// The surviving value must be one an actual writer wrote, not a blend.
	final, found, err := store.Get(ctx, kvStressKey)
	if err != nil {
		t.Fatalf("final Get: %v", err)
	}
	if !found {
		t.Fatal("final Get: the key is absent after the stress run")
	}
	if _, ok := legal[string(final)]; !ok {
		t.Fatalf("final value is not one of the values written (len %d, want %d)", len(final), kvStressValueBytes)
	}
}

func TestMemoryKVStore_CancelledContext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		seed        bool
		call        func(ctx context.Context, store KVStore) error
		wantEntries int
	}{
		{
			name: "Get",
			call: func(ctx context.Context, store KVStore) error {
				_, _, err := store.Get(ctx, kvTestKey)
				return err
			},
		},
		{
			name: "Set",
			call: func(ctx context.Context, store KVStore) error {
				return store.Set(ctx, kvTestKey, []byte("value"), kvNoExpiry)
			},
		},
		{
			name: "Delete",
			seed: true,
			call: func(ctx context.Context, store KVStore) error {
				return store.Delete(ctx, kvTestKey)
			},
			wantEntries: 1,
		},
		{
			name: "IncrByFloat",
			call: func(ctx context.Context, store KVStore) error {
				_, err := store.IncrByFloat(ctx, kvTestKey, kvIncrementStep)
				return err
			},
		},
		{
			name: "CompareAndSwap",
			call: func(ctx context.Context, store KVStore) error {
				_, err := store.CompareAndSwap(ctx, kvTestKey, nil, []byte("value"))
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := NewMemoryKVStore()
			if tc.seed {
				kvSet(t, store, kvTestKey, "value")
			}

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			if err := tc.call(ctx, store); !errors.Is(err, context.Canceled) {
				t.Fatalf("%s with a cancelled context: error = %v, want context.Canceled", tc.name, err)
			}
			// A cancelled call must not have touched the store, so a caller that
			// gives up mid-flight cannot leave a half-applied write behind.
			if count := kvEntryCount(t, store); count != tc.wantEntries {
				t.Errorf("%s ran despite the cancelled context: entry count = %d, want %d", tc.name, count, tc.wantEntries)
			}
		})
	}
}
