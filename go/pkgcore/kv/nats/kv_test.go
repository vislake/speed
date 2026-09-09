package nats

// Hermetic unit tests for the NATS-backed KVStore: everything here runs
// without a NATS server. That is a narrower set than kv/redis's own unit
// tier can cover, for a structural reason: kv/redis's NewKVStore never dials
// (go-redis connects lazily), so its unit tests can build a real store
// against an address nothing listens on and assert its cancelled-context
// behaviour directly. This package's NewKVStore provisions a JetStream KV
// bucket at construction, which is itself a round trip to the server, so the
// constructor's own provisioning flow -- the probe/adopt/create-race paths
// of NewKVStore itself -- has no hermetic form; it is proven by
// kvstoretest.AssertConforms and the adoption tests in integration_test, run
// against a real container.
//
// The *kvStore layer beneath the constructor is a different matter: the
// store is a thin wrapper over a jetstream.KeyValue handle, so the tests
// below build one around a faithful in-memory fake of that interface and
// drive every operation -- Get/Set/Delete, both increment variants' retry
// loops, CompareAndSwap's write shapes, the expired-row purge, the error
// paths -- against it. What belongs in this file is everything that needs no
// server: a nil connection is a wiring error reported at construction, the
// envelope encoding round-trips and rejects malformed input, every key this
// package's own key-encoding scheme produces is valid under JetStream's own
// key charset, and the store's own logic behaves correctly over the
// KeyValue contract it relies on.

import (
	"bytes"
	"context"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/vislake/speed/go/pkgcore"
)

// TestNewKVStore_PanicsOnNilConn pins that a nil connection is a wiring error
// reported at construction, not a failure deferred to the first operation --
// mirroring kv/redis's identical guarantee for a nil *redis.Client.
func TestNewKVStore_PanicsOnNilConn(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewKVStore(nil) did not panic, want it to")
		}
	}()
	_, _ = NewKVStore(t.Context(), nil, "irrelevant")
}

// TestEncodeDecodeEnvelope_RoundTrips pins that decodeEnvelope recovers
// exactly the value and expiry encodeEnvelope was given, for every shape
// IncrByFloat and CompareAndSwap rely on: no expiry, a real future expiry, an
// empty value, and a value long enough that its own bytes could plausibly be
// mistaken for header bytes if the header were not length-prefixed correctly.
func TestEncodeDecodeEnvelope_RoundTrips(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		value     []byte
		expiresAt time.Time
	}{
		{"no expiry, non-empty value", []byte("hello"), time.Time{}},
		{"no expiry, empty value", []byte{}, time.Time{}},
		{"no expiry, nil value", nil, time.Time{}},
		{"future expiry", []byte("counter:2.5"), time.Now().Add(time.Hour).Truncate(time.Nanosecond)},
		{"binary value", []byte{0x00, 0x01, 0xff, 0xfe, 0x00}, time.Time{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			encoded, err := encodeEnvelope(tt.value, tt.expiresAt)
			if err != nil {
				t.Fatalf("encodeEnvelope() error = %v, want nil", err)
			}
			gotValue, gotExpiresAt, err := decodeEnvelope(encoded)
			if err != nil {
				t.Fatalf("decodeEnvelope() error = %v, want nil", err)
			}
			if !bytes.Equal(gotValue, tt.value) {
				t.Errorf("decodeEnvelope() value = %v, want %v", gotValue, tt.value)
			}
			if !gotExpiresAt.Equal(tt.expiresAt) {
				t.Errorf("decodeEnvelope() expiresAt = %v, want %v", gotExpiresAt, tt.expiresAt)
			}
		})
	}
}

// TestDecodeEnvelope_TooShortIsAnError pins that a value shorter than the
// expiry header -- something this package itself never writes -- fails
// loudly rather than panicking or silently truncating.
func TestDecodeEnvelope_TooShortIsAnError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"one byte short of the header", make([]byte, envelopeExpiryLen-1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, _, err := decodeEnvelope(tt.data); err == nil {
				t.Errorf("decodeEnvelope(%v) error = nil, want a malformed-envelope error", tt.data)
			}
		})
	}
}

// TestExpired pins the same "zero time never expires, otherwise compare to
// now" rule pkgcore's own in-memory kvEntry.expired documents.
func TestExpired(t *testing.T) {
	t.Parallel()

	if expired(time.Time{}) {
		t.Error("expired(zero time) = true, want false: the zero time means no expiry")
	}
	if !expired(time.Now().Add(-time.Second)) {
		t.Error("expired(one second ago) = false, want true")
	}
	if expired(time.Now().Add(time.Hour)) {
		t.Error("expired(one hour from now) = true, want false")
	}
}

// validJetStreamKey mirrors the private validKeyRe nats.go's own jetstream
// package checks every KeyValue key against (see its kv.go): alphanumeric,
// dash, underscore, slash, equals and dot, never empty, never starting or
// ending with a dot, never containing "..". This package's own encodeKey
// must satisfy it for every possible opaque pkgcore.KVStore key, since that
// interface places no character restriction on keys at all.
var validJetStreamKey = regexp.MustCompile(`^[-/_=.a-zA-Z0-9]+$`)

// TestEncodeKey_IsAlwaysAValidJetStreamKey pins that encodeKey's hex encoding
// produces a JetStream-valid key for every input this package's own callers
// might pass through pkgcore.KVStore's opaque, unrestricted key type --
// including exactly the colon-separated shapes kv/redis's own tests and
// kvstoretest.conformKey use, which JetStream's own key charset rejects
// outright.
func TestEncodeKey_IsAlwaysAValidJetStreamKey(t *testing.T) {
	t.Parallel()

	tests := []string{
		"",
		"a",
		"billing:invoice:1042",
		"kvstoretest:TestFoo/bar_baz:cas",
		"lock:acme:import",
		"has a space",
		"has\nnewline\tand\ttabs",
		"has.dots..and..more",
		"unicode: 你好, мир, 🎉",
		"embedded\x00nul\x00bytes",
	}

	for _, key := range tests {
		t.Run(key, func(t *testing.T) {
			t.Parallel()

			encoded := encodeKey(key)
			if len(encoded) == 0 && key != "" {
				t.Fatalf("encodeKey(%q) = %q, want a non-empty encoded key", key, encoded)
			}
			if key != "" {
				if !validJetStreamKey.MatchString(encoded) {
					t.Errorf("encodeKey(%q) = %q, want it to match %s", key, encoded, validJetStreamKey)
				}
				if encoded[0] == '.' || encoded[len(encoded)-1] == '.' {
					t.Errorf("encodeKey(%q) = %q, want it not to start or end with '.'", key, encoded)
				}
			}
		})
	}
}

// TestEncodeKey_IsInjective pins that distinct opaque keys never collide
// once hex-encoded -- the property every other correctness guarantee in this
// package assumes about the key it operates on.
func TestEncodeKey_IsInjective(t *testing.T) {
	t.Parallel()

	keys := []string{"a", "b", "ab", "a:b", "a\x00b", "", "billing:invoice:1042", "billing:invoice:1043"}
	seen := make(map[string]string, len(keys))
	for _, key := range keys {
		encoded := encodeKey(key)
		if prior, ok := seen[encoded]; ok && prior != key {
			t.Errorf("encodeKey(%q) and encodeKey(%q) both produced %q, want distinct keys never to collide", prior, key, encoded)
		}
		seen[encoded] = key
	}
}

// TestKVStore_Set_EnvelopeSizeOverflowValueRefused pins the fix for the
// allocation-size overflow CodeQL flagged in encodeEnvelope (rule id
// go/allocation-size-overflow): the envelope buffer's size is header plus
// payload, computed in the platform's int, and a payload length sitting
// within envelopeExpiryLen bytes of the int maximum wraps that addition
// negative, so make would panic on an allocation no such envelope can ever
// need. The refusal is driven through the store's own Set -- the overflow
// must surface as Set's error, never as a panic from the allocation itself.
// The store is a bare &kvStore{} whose kv handle is nil, because this
// package's constructor provisions a real JetStream bucket and needs a
// server: the refusal must fire before the handle is ever touched.
func TestKVStore_Set_EnvelopeSizeOverflowValueRefused(t *testing.T) {
	t.Parallel()

	store := &kvStore{}

	// The value is synthesized, not allocated: only a 32-bit platform can
	// hold a real slice anywhere near the int maximum, so no allocation can
	// reproduce the wrap -- only a forged slice header can. unsafe.Slice is
	// unusable for the forgery: the race detector's checkptr instrumentation
	// dies on the declared length alone (fatal error: checkptr: unsafe.Slice
	// result straddles multiple allocations), so the header is stamped by
	// hand over the runtime slice-header layout instead, a construction
	// nothing instruments. No byte of the forged slice is ever read -- which
	// is the point: the store must refuse on the length alone.
	var b byte
	header := struct {
		data uintptr
		len  int
		cap  int
	}{data: uintptr(unsafe.Pointer(&b)), len: math.MaxInt - 1, cap: math.MaxInt - 1} // envelopeExpiryLen + len(value) wraps past MaxInt
	value := *(*[]byte)(unsafe.Pointer(&header))
	if err := store.Set(context.Background(), "k", value, 0); err == nil {
		t.Fatal("Set() error = nil, want a refusal error for a value whose envelope size arithmetic would overflow")
	}
}

// TestAdoptionRefusal pins the adoptable-bucket decision surface hermetically,
// without a NATS server: adoptionRefusal is a pure decision over the effective
// configuration the server reports, so every row of the surface -- the two
// refusal dimensions and the tolerated fields -- is exercised here fast and
// deterministically, while the integration tier's own refusal and compliant-
// adoption tests prove the same surface end to end against a real server.
//
// The refusal must be the named ErrUnadoptableBucket (errors.Is-matchable,
// never a bare string) and must name the dimension that disqualifies the
// bucket, so an operator who pre-provisioned it knows what to change.
func TestAdoptionRefusal(t *testing.T) {
	t.Parallel()

	const bucket = "decision-surface"
	tests := []struct {
		name        string
		cfg         jetstream.KeyValueConfig
		wantRefusal bool
		wantInError string
	}{
		{
			name: "file storage, no TTL, unset replication",
			cfg:  jetstream.KeyValueConfig{Bucket: bucket, Storage: jetstream.FileStorage},
		},
		{
			name: "file storage, no TTL, replicated",
			cfg:  jetstream.KeyValueConfig{Bucket: bucket, Storage: jetstream.FileStorage, Replicas: 3},
		},
		{
			name: "file storage, no TTL, operator extras",
			cfg: jetstream.KeyValueConfig{
				Bucket:      bucket,
				Storage:     jetstream.FileStorage,
				History:     10,
				Replicas:    1,
				Description: "provisioned by an operator",
			},
		},
		{
			name:        "memory storage, no TTL",
			cfg:         jetstream.KeyValueConfig{Bucket: bucket, Storage: jetstream.MemoryStorage},
			wantRefusal: true,
			wantInError: "memory storage",
		},
		{
			name:        "an unrecognized storage type is refused as unvouchable",
			cfg:         jetstream.KeyValueConfig{Bucket: bucket, Storage: jetstream.StorageType(42)},
			wantRefusal: true,
			wantInError: "unrecognized storage type",
		},
		{
			name:        "file storage with a bucket TTL",
			cfg:         jetstream.KeyValueConfig{Bucket: bucket, Storage: jetstream.FileStorage, TTL: 45 * time.Second},
			wantRefusal: true,
			wantInError: "TTL",
		},
		{
			name:        "memory storage with a bucket TTL reports storage first",
			cfg:         jetstream.KeyValueConfig{Bucket: bucket, Storage: jetstream.MemoryStorage, TTL: 45 * time.Second},
			wantRefusal: true,
			wantInError: "memory storage",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := adoptionRefusal(tt.cfg)
			if !tt.wantRefusal {
				if err != nil {
					t.Fatalf("adoptionRefusal() error = %v, want nil for an adoptable configuration", err)
				}
				return
			}
			if !errors.Is(err, ErrUnadoptableBucket) {
				t.Fatalf("adoptionRefusal() error = %v, want it to wrap %v", err, ErrUnadoptableBucket)
			}
			if !strings.Contains(err.Error(), tt.wantInError) {
				t.Errorf("adoptionRefusal() error = %q, want it to name the disqualifying dimension %q", err, tt.wantInError)
			}
			if !strings.Contains(err.Error(), bucket) {
				t.Errorf("adoptionRefusal() error = %q, want it to name the refused bucket %q", err, bucket)
			}
		})
	}
}

// TestExpiryFromTTL pins the ttl-to-expiry boundary both increment variants
// rely on: a zero or negative ttl means no expiry, a positive one lands
// roughly ttl in the future.
func TestExpiryFromTTL(t *testing.T) {
	t.Parallel()

	for _, ttl := range []time.Duration{0, -time.Second} {
		if got := expiryFromTTL(ttl); !got.IsZero() {
			t.Errorf("expiryFromTTL(%v) = %v, want the zero time.Time", ttl, got)
		}
	}
	ttl := 30 * time.Minute
	expiresAt := expiryFromTTL(ttl)
	if expiresAt.Before(time.Now().Add(ttl-5*time.Second)) || expiresAt.After(time.Now().Add(ttl+5*time.Second)) {
		t.Errorf("expiryFromTTL(%v) = %v, want roughly now+%v", ttl, expiresAt, ttl)
	}
}

// fakeKeyValue is a faithful in-process stand-in for a JetStream KV bucket,
// implementing the exact subset of jetstream.KeyValue's contract the kvStore
// above relies on: Put/Create/Update with per-key revision counting and the
// server's own conflict answers (ErrKeyExists for a Create on an existing
// key, ErrKeyRevisionMismatch for an Update at a stale revision), Get
// reporting a deleted key as jetstream.ErrKeyNotFound, Delete placing a
// delete marker, and Status answering a scripted configuration. Scriptable
// misbehaviour fields let a test force the store's retry loops down their
// conflict paths deterministically; the remaining jetstream.KeyValue methods
// come from the embedded nil interface and are never called.
type fakeKeyValue struct {
	jetstream.KeyValue

	mu    sync.Mutex
	seq   uint64
	rows  map[string]*fakeKVRow
	order []string // insertion order, for deterministic iteration when needed

	// Scripted behaviour.
	statusCfg              jetstream.KeyValueConfig
	statusErr              error
	createExistsFailures   int // Create calls to answer ErrKeyExists before behaving
	updateMismatchFailures int // Update calls to answer ErrKeyRevisionMismatch before behaving
	getErr                 error
	deleteErr              error
	createErr              error    // one-shot: the next Create fails with this
	updateErr              error    // one-shot: the next Update fails with this
	deletedKeys            []string // keys Delete was asked to delete, in order
}

// fakeKVRow is one key's live state in fakeKeyValue: the latest raw value,
// its revision, and whether the key carries a delete marker.
type fakeKVRow struct {
	value    []byte
	revision uint64
	deleted  bool
}

func newFakeKeyValue() *fakeKeyValue {
	return &fakeKeyValue{
		rows:      make(map[string]*fakeKVRow),
		statusCfg: jetstream.KeyValueConfig{Bucket: "bucket", Storage: jetstream.FileStorage},
	}
}

func (f *fakeKeyValue) nextRevisionLocked() uint64 {
	f.seq++
	return f.seq
}

// seedRow plants key with raw envelope bytes at a fresh revision, bypassing
// the public write methods -- the way a test gets a specific expiry onto the
// fake's clock-independent "server" state.
func (f *fakeKeyValue) seedRow(key string, raw []byte) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	rev := f.nextRevisionLocked()
	f.rows[key] = &fakeKVRow{value: bytes.Clone(raw), revision: rev}
	return rev
}

func (f *fakeKeyValue) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rev := f.nextRevisionLocked()
	f.rows[key] = &fakeKVRow{value: bytes.Clone(value), revision: rev}
	return rev, nil
}

func (f *fakeKeyValue) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		err := f.createErr
		f.createErr = nil
		return 0, err
	}
	if f.createExistsFailures > 0 {
		f.createExistsFailures--
		return 0, jetstream.ErrKeyExists
	}
	if row, ok := f.rows[key]; ok && !row.deleted {
		return 0, jetstream.ErrKeyExists
	}
	rev := f.nextRevisionLocked()
	f.rows[key] = &fakeKVRow{value: bytes.Clone(value), revision: rev}
	return rev, nil
}

func (f *fakeKeyValue) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateErr != nil {
		err := f.updateErr
		f.updateErr = nil
		return 0, err
	}
	if f.updateMismatchFailures > 0 {
		f.updateMismatchFailures--
		return 0, jetstream.ErrKeyRevisionMismatch
	}
	row, ok := f.rows[key]
	if !ok || row.deleted {
		return 0, jetstream.ErrKeyNotFound
	}
	if row.revision != revision {
		return 0, jetstream.ErrKeyRevisionMismatch
	}
	rev := f.nextRevisionLocked()
	row.value = bytes.Clone(value)
	row.revision = rev
	return rev, nil
}

func (f *fakeKeyValue) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		err := f.getErr
		f.getErr = nil
		return nil, err
	}
	row, ok := f.rows[key]
	if !ok || row.deleted {
		return nil, jetstream.ErrKeyNotFound
	}
	return &fakeKVEntry{key: key, value: bytes.Clone(row.value), revision: row.revision}, nil
}

func (f *fakeKeyValue) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		err := f.deleteErr
		f.deleteErr = nil
		return err
	}
	f.deletedKeys = append(f.deletedKeys, key)
	if row, ok := f.rows[key]; ok {
		row.deleted = true
	} else {
		f.rows[key] = &fakeKVRow{deleted: true}
	}
	return nil
}

func (f *fakeKeyValue) Status(ctx context.Context) (jetstream.KeyValueStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return &fakeKVStatus{cfg: f.statusCfg}, nil
}

// valueOf returns key's current raw bytes, or nil when the key is absent or
// deleted -- for asserting what a store operation actually wrote.
func (f *fakeKeyValue) valueOf(key string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[key]
	if !ok || row.deleted {
		return nil
	}
	return bytes.Clone(row.value)
}

// fakeKVEntry is a jetstream.KeyValueEntry answering Key/Value/Revision from
// fields; the remaining methods come from the embedded nil interface and are
// never called by the store.
type fakeKVEntry struct {
	jetstream.KeyValueEntry
	key      string
	value    []byte
	revision uint64
}

func (e *fakeKVEntry) Key() string      { return e.key }
func (e *fakeKVEntry) Value() []byte    { return e.value }
func (e *fakeKVEntry) Revision() uint64 { return e.revision }

// fakeKVStatus is a jetstream.KeyValueStatus answering Config from a field.
type fakeKVStatus struct {
	jetstream.KeyValueStatus
	cfg jetstream.KeyValueConfig
}

func (s *fakeKVStatus) Config() jetstream.KeyValueConfig { return s.cfg }

// TestKVStore_GetSetDelete_OverFakeBucket drives the plain read/write/delete
// lifecycle against the fake bucket: an absent key is a miss, Set stores and
// replaces, Delete removes, deleting an absent key is not an error, and the
// caller's own key bytes -- not the hex-encoded wire key -- are what Get
// hands back.
func TestKVStore_GetSetDelete_OverFakeBucket(t *testing.T) {
	fake := newFakeKeyValue()
	store := &kvStore{kv: fake}
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
	if wireKey := encodeKey("k"); fake.valueOf(wireKey) == nil {
		t.Fatalf("fake bucket holds nothing at encoded key %q, want the value the store set", wireKey)
	}

	// Set replaces the prior value.
	if err := store.Set(ctx, "k", []byte{0x00, 0xff}, 0); err != nil {
		t.Fatalf("second Set() error = %v, want nil", err)
	}
	got, ok, err = store.Get(ctx, "k")
	if err != nil || !ok || !bytes.Equal(got, []byte{0x00, 0xff}) {
		t.Fatalf("Get after overwrite = (%v, %v, %v), want the binary replacement", got, ok, err)
	}

	// A key whose wire name differs from its caller key stays distinct: the
	// hex encoding must keep "k" and "K" apart.
	if err := store.Set(ctx, "K", []byte("other"), 0); err != nil {
		t.Fatalf("Set(K) error = %v, want nil", err)
	}
	if got, ok, _ := store.Get(ctx, "k"); !ok || !bytes.Equal(got, []byte{0x00, 0xff}) {
		t.Errorf("Get(k) after a Set(K) = (%q, %v), want the first key's own value untouched", got, ok)
	}

	if err := store.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete() error = %v, want nil", err)
	}
	if _, ok, err := store.Get(ctx, "k"); err != nil || ok {
		t.Errorf("Get after Delete = (_, %v, %v), want (_, false, nil)", ok, err)
	}
	if err := store.Delete(ctx, "k"); err != nil {
		t.Errorf("Delete of an absent key error = %v, want nil: deleting a key the store does not hold is not an error", err)
	}
}

// TestKVStore_CancelledContext_NoOpReachesTheBucket pins that every operation
// on a cancelled context fails before the handle is touched, so a cancelled
// caller can never race a write in behind its own cancellation.
func TestKVStore_CancelledContext_NoOpReachesTheBucket(t *testing.T) {
	fake := newFakeKeyValue()
	store := &kvStore{kv: fake}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := store.Get(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Errorf("Get on a cancelled context error = %v, want context.Canceled", err)
	}
	if err := store.Set(ctx, "k", []byte("v"), 0); !errors.Is(err, context.Canceled) {
		t.Errorf("Set on a cancelled context error = %v, want context.Canceled", err)
	}
	if err := store.Delete(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Errorf("Delete on a cancelled context error = %v, want context.Canceled", err)
	}
	if _, err := store.IncrByFloat(ctx, "k", 1); !errors.Is(err, context.Canceled) {
		t.Errorf("IncrByFloat on a cancelled context error = %v, want context.Canceled", err)
	}
	if _, err := store.IncrByFloatWithTTL(ctx, "k", 1, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("IncrByFloatWithTTL on a cancelled context error = %v, want context.Canceled", err)
	}
	if _, err := store.CompareAndSwap(ctx, "k", nil, []byte("v")); !errors.Is(err, context.Canceled) {
		t.Errorf("CompareAndSwap on a cancelled context error = %v, want context.Canceled", err)
	}
	if len(fake.deletedKeys) != 0 {
		t.Error("a cancelled operation reached the bucket's Delete")
	}
	if fake.valueOf(encodeKey("k")) != nil {
		t.Error("a cancelled operation wrote to the bucket")
	}
}

// TestKVStore_Get_ExpiredRowPurgedRevisionGuarded pins Get's lazy reclaim:
// a row whose envelope expiry has passed is reported absent, and the store
// asks the bucket to delete it -- revision-guarded, the call carrying
// LastRevision so a concurrent revive is never clobbered.
func TestKVStore_Get_ExpiredRowPurgedRevisionGuarded(t *testing.T) {
	fake := newFakeKeyValue()
	store := &kvStore{kv: fake}
	ctx := context.Background()

	key := "expiring"
	encKey := encodeKey(key)
	past := time.Now().Add(-time.Second)
	raw, err := encodeEnvelope([]byte("stale"), past)
	if err != nil {
		t.Fatalf("encodeEnvelope() error = %v, want nil", err)
	}
	fake.seedRow(encKey, raw)

	got, ok, err := store.Get(ctx, key)
	if err != nil || ok {
		t.Fatalf("Get on an expired row = (%q, %v, %v), want (_, false, nil)", got, ok, err)
	}
	if len(fake.deletedKeys) != 1 || fake.deletedKeys[0] != encKey {
		t.Errorf("expired row purge deletions = %v, want exactly [%q]", fake.deletedKeys, encKey)
	}
	// A second Get finds the delete marker and asks for nothing more.
	if _, ok, err := store.Get(ctx, key); err != nil || ok {
		t.Errorf("Get on a purged row = (_, %v, %v), want (_, false, nil)", ok, err)
	}
	if len(fake.deletedKeys) != 1 {
		t.Errorf("purge deletions after a second Get = %v, want still exactly one", fake.deletedKeys)
	}
}

// TestKVStore_GetAndSet_ErrorPathsSurfacedWrapped pins that a failing bucket
// surfaces as a wrapped error naming the operation, and that a stored value
// too short to be one of this package's envelopes is refused loudly rather
// than misparsed.
func TestKVStore_GetAndSet_ErrorPathsSurfacedWrapped(t *testing.T) {
	ctx := context.Background()

	t.Run("get failure is wrapped", func(t *testing.T) {
		fake := newFakeKeyValue()
		fake.getErr = errors.New("server went away")
		store := &kvStore{kv: fake}
		_, _, err := store.Get(ctx, "k")
		if err == nil || !strings.Contains(err.Error(), "pkgcore/kv/nats: get:") {
			t.Fatalf("Get() error = %v, want a wrapped error naming the get", err)
		}
	})

	t.Run("set failure is wrapped", func(t *testing.T) {
		fake := newFakeKeyValue()
		store := &kvStore{kv: &failingPutKV{KeyValue: fake}}
		if err := store.Set(ctx, "k", []byte("v"), 0); err == nil || !strings.Contains(err.Error(), "pkgcore/kv/nats: set:") {
			t.Fatalf("Set() error = %v, want a wrapped error naming the set", err)
		}
	})

	t.Run("delete failure is wrapped", func(t *testing.T) {
		fake := newFakeKeyValue()
		fake.deleteErr = errors.New("server went away")
		store := &kvStore{kv: fake}
		if err := store.Delete(ctx, "k"); err == nil || !strings.Contains(err.Error(), "pkgcore/kv/nats: delete:") {
			t.Fatalf("Delete() error = %v, want a wrapped error naming the delete", err)
		}
	})

	t.Run("corrupt envelope is refused", func(t *testing.T) {
		fake := newFakeKeyValue()
		fake.seedRow(encodeKey("k"), []byte("short"))
		store := &kvStore{kv: fake}
		_, _, err := store.Get(ctx, "k")
		if err == nil || !strings.Contains(err.Error(), "malformed envelope") {
			t.Fatalf("Get on a corrupt envelope error = %v, want a malformed-envelope error", err)
		}
	})
}

// failingPutKV is a jetstream.KeyValue whose Put always fails, for the set
// error-path assertion above.
type failingPutKV struct {
	jetstream.KeyValue
}

func (f *failingPutKV) Put(context.Context, string, []byte) (uint64, error) {
	return 0, errors.New("server went away")
}

// TestKVStore_IncrByFloat_OverFakeBucket drives the increment contract
// against the fake bucket: a fresh key starts at zero plus delta with no
// expiry, a live key accumulates and keeps its own expiry, a logically
// expired row restarts from zero and sheds its stale expiry, a non-numeric
// live value fails with pkgcore.ErrNotNumeric and is left untouched.
func TestKVStore_IncrByFloat_OverFakeBucket(t *testing.T) {
	fake := newFakeKeyValue()
	store := &kvStore{kv: fake}
	ctx := context.Background()

	// Fresh key: created at zero + delta, never expires.
	result, err := store.IncrByFloat(ctx, "n", 2.5)
	if err != nil {
		t.Fatalf("IncrByFloat on a fresh key error = %v, want nil", err)
	}
	if result != 2.5 {
		t.Fatalf("IncrByFloat on a fresh key = %v, want 2.5", result)
	}
	raw := fake.valueOf(encodeKey("n"))
	payload, expiresAt, err := decodeEnvelope(raw)
	if err != nil {
		t.Fatalf("decodeEnvelope(written value) error = %v, want nil", err)
	}
	if string(payload) != "2.5" || !expiresAt.IsZero() {
		t.Errorf("fresh increment wrote payload %q with expiry %v, want \"2.5\" with no expiry", payload, expiresAt)
	}

	// Live key: accumulates, and its own expiry survives the write.
	base := time.Now().Add(30 * time.Minute).Truncate(time.Millisecond)
	liveRaw, err := encodeEnvelope([]byte("10"), base)
	if err != nil {
		t.Fatalf("encodeEnvelope() error = %v, want nil", err)
	}
	fake.seedRow(encodeKey("n"), liveRaw)
	if result, err := store.IncrByFloat(ctx, "n", 5); err != nil || result != 15 {
		t.Fatalf("IncrByFloat on a live key = (%v, %v), want (15, nil)", result, err)
	}
	payload, expiresAt, err = decodeEnvelope(fake.valueOf(encodeKey("n")))
	if err != nil {
		t.Fatalf("decodeEnvelope() error = %v, want nil", err)
	}
	if string(payload) != "15" {
		t.Errorf("live increment wrote payload %q, want \"15\"", payload)
	}
	if !expiresAt.Equal(base) {
		t.Errorf("live increment changed the key's expiry from %v to %v, want it preserved", base, expiresAt)
	}

	// Logically expired row: restarts at zero plus delta and sheds the stale
	// expiry entirely.
	stale := time.Now().Add(-time.Minute)
	staleRaw, err := encodeEnvelope([]byte("99"), stale)
	if err != nil {
		t.Fatalf("encodeEnvelope() error = %v, want nil", err)
	}
	fake.seedRow(encodeKey("n"), staleRaw)
	if result, err := store.IncrByFloat(ctx, "n", 1); err != nil || result != 1 {
		t.Fatalf("IncrByFloat on an expired row = (%v, %v), want (1, nil): the value restarts from zero", result, err)
	}
	payload, expiresAt, err = decodeEnvelope(fake.valueOf(encodeKey("n")))
	if err != nil {
		t.Fatalf("decodeEnvelope() error = %v, want nil", err)
	}
	if string(payload) != "1" || !expiresAt.IsZero() {
		t.Errorf("increment over an expired row wrote payload %q with expiry %v, want \"1\" with no expiry", payload, expiresAt)
	}

	// Non-numeric live value: ErrNotNumeric, value untouched.
	nonNumeric, err := encodeEnvelope([]byte("not-a-number"), time.Time{})
	if err != nil {
		t.Fatalf("encodeEnvelope() error = %v, want nil", err)
	}
	fake.seedRow(encodeKey("n"), nonNumeric)
	if _, err := store.IncrByFloat(ctx, "n", 1); !errors.Is(err, pkgcore.ErrNotNumeric) {
		t.Errorf("IncrByFloat on a non-numeric value error = %v, want pkgcore.ErrNotNumeric", err)
	}
	if got := fake.valueOf(encodeKey("n")); !bytes.Equal(got, nonNumeric) {
		t.Error("IncrByFloat on a non-numeric value modified the stored bytes")
	}
}

// TestKVStore_IncrByFloatWithTTL_OverFakeBucket drives the ttl-attaching
// sibling: only a fresh (missing or logically expired) key gets ttl applied,
// while a live key's own expiry is carried through untouched.
func TestKVStore_IncrByFloatWithTTL_OverFakeBucket(t *testing.T) {
	fake := newFakeKeyValue()
	store := &kvStore{kv: fake}
	ctx := context.Background()

	// Fresh key: the ttl is attached on the creating call.
	ttl := 45 * time.Minute
	if _, err := store.IncrByFloatWithTTL(ctx, "n", 3, ttl); err != nil {
		t.Fatalf("IncrByFloatWithTTL on a fresh key error = %v, want nil", err)
	}
	_, expiresAt, err := decodeEnvelope(fake.valueOf(encodeKey("n")))
	if err != nil {
		t.Fatalf("decodeEnvelope() error = %v, want nil", err)
	}
	if expiresAt.Before(time.Now().Add(ttl-5*time.Second)) || expiresAt.After(time.Now().Add(ttl+5*time.Second)) {
		t.Errorf("fresh-key increment expiry = %v, want roughly now+%v", expiresAt, ttl)
	}

	// Live key with its own expiry: ttl is never consulted.
	base := time.Now().Add(2 * time.Hour).Truncate(time.Millisecond)
	liveRaw, err := encodeEnvelope([]byte("7"), base)
	if err != nil {
		t.Fatalf("encodeEnvelope() error = %v, want nil", err)
	}
	fake.seedRow(encodeKey("n"), liveRaw)
	if result, err := store.IncrByFloatWithTTL(ctx, "n", 1, 10*time.Second); err != nil || result != 8 {
		t.Fatalf("IncrByFloatWithTTL on a live key = (%v, %v), want (8, nil)", result, err)
	}
	_, expiresAt, err = decodeEnvelope(fake.valueOf(encodeKey("n")))
	if err != nil {
		t.Fatalf("decodeEnvelope() error = %v, want nil", err)
	}
	if !expiresAt.Equal(base) {
		t.Errorf("live-key increment with a ttl argument changed the expiry from %v to %v, want it preserved", base, expiresAt)
	}

	// Logically expired row: restarts from zero and re-attaches ttl.
	staleRaw, err := encodeEnvelope([]byte("42"), time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("encodeEnvelope() error = %v, want nil", err)
	}
	fake.seedRow(encodeKey("n"), staleRaw)
	if result, err := store.IncrByFloatWithTTL(ctx, "n", 2, ttl); err != nil || result != 2 {
		t.Fatalf("IncrByFloatWithTTL over an expired row = (%v, %v), want (2, nil)", result, err)
	}
	_, expiresAt, err = decodeEnvelope(fake.valueOf(encodeKey("n")))
	if err != nil {
		t.Fatalf("decodeEnvelope() error = %v, want nil", err)
	}
	if expiresAt.IsZero() || expiresAt.Before(time.Now().Add(ttl-10*time.Second)) {
		t.Errorf("expired-row increment expiry = %v, want the ttl re-attached", expiresAt)
	}

	// Non-numeric live value fails read-side with ErrNotNumeric.
	bad, err := encodeEnvelope([]byte("not-a-number"), time.Time{})
	if err != nil {
		t.Fatalf("encodeEnvelope() error = %v, want nil", err)
	}
	fake.seedRow(encodeKey("n"), bad)
	if _, err := store.IncrByFloatWithTTL(ctx, "n", 1, ttl); !errors.Is(err, pkgcore.ErrNotNumeric) {
		t.Errorf("IncrByFloatWithTTL on a non-numeric value error = %v, want pkgcore.ErrNotNumeric", err)
	}
}

// TestKVStore_IncrByFloat_CreateRaceIsRetried pins the increment loop's
// response to a lost provisioning race: the bucket answers ErrKeyExists for
// the first Create (another writer created the key between this loop's read
// and its write), the loop retries from a fresh read instead of failing, and
// the second attempt lands the value.
func TestKVStore_IncrByFloat_CreateRaceIsRetried(t *testing.T) {
	fake := newFakeKeyValue()
	fake.createExistsFailures = 1
	store := &kvStore{kv: fake}
	ctx := context.Background()

	result, err := store.IncrByFloat(ctx, "n", 4)
	if err != nil {
		t.Fatalf("IncrByFloat with one lost create race error = %v, want nil (the loop retries)", err)
	}
	if result != 4 {
		t.Fatalf("IncrByFloat with one lost create race = %v, want 4", result)
	}
	payload, _, err := decodeEnvelope(fake.valueOf(encodeKey("n")))
	if err != nil {
		t.Fatalf("decodeEnvelope() error = %v, want nil", err)
	}
	if string(payload) != "4" {
		t.Errorf("final payload = %q, want \"4\"", payload)
	}
}

// TestKVStore_IncrByFloat_UpdateRaceIsRetriedWithoutDoubleCounting pins the
// same retry for the update half: one lost revision race between read and
// write is retried from a fresh read, and the retry must not double-apply the
// delta -- the value read after the failed write is unchanged, so the second
// attempt computes the same result.
func TestKVStore_IncrByFloat_UpdateRaceIsRetriedWithoutDoubleCounting(t *testing.T) {
	fake := newFakeKeyValue()
	seed, err := encodeEnvelope([]byte("10"), time.Time{})
	if err != nil {
		t.Fatalf("encodeEnvelope() error = %v, want nil", err)
	}
	fake.seedRow(encodeKey("n"), seed)
	fake.updateMismatchFailures = 1
	store := &kvStore{kv: fake}
	ctx := context.Background()

	result, err := store.IncrByFloat(ctx, "n", 5)
	if err != nil {
		t.Fatalf("IncrByFloat with one lost update race error = %v, want nil (the loop retries)", err)
	}
	if result != 15 {
		t.Fatalf("IncrByFloat with one lost update race = %v, want 15", result)
	}
	payload, _, err := decodeEnvelope(fake.valueOf(encodeKey("n")))
	if err != nil {
		t.Fatalf("decodeEnvelope() error = %v, want nil", err)
	}
	if string(payload) != "15" {
		t.Errorf("final payload = %q, want \"15\": a lost race must never double-apply the delta", payload)
	}
}

// TestKVStore_CompareAndSwap_OverFakeBucket drives CompareAndSwap's write
// shapes: set-if-absent for a missing key with an empty old, immediate false
// for a missing key with a non-empty old, a matched live swap preserving the
// key's expiry, a mismatched live swap changing nothing, an expired row
// matching only the empty old and being replaced without an expiry, and a
// corrupt envelope surfacing as an error.
func TestKVStore_CompareAndSwap_OverFakeBucket(t *testing.T) {
	ctx := context.Background()

	t.Run("absent key with empty old creates without expiry", func(t *testing.T) {
		fake := newFakeKeyValue()
		store := &kvStore{kv: fake}
		swapped, err := store.CompareAndSwap(ctx, "k", nil, []byte("new"))
		if err != nil || !swapped {
			t.Fatalf("CompareAndSwap(set-if-absent) = (%v, %v), want (true, nil)", swapped, err)
		}
		payload, expiresAt, err := decodeEnvelope(fake.valueOf(encodeKey("k")))
		if err != nil {
			t.Fatalf("decodeEnvelope() error = %v, want nil", err)
		}
		if string(payload) != "new" || !expiresAt.IsZero() {
			t.Errorf("set-if-absent wrote payload %q with expiry %v, want \"new\" with no expiry", payload, expiresAt)
		}
	})

	t.Run("absent key with non-empty old is a mismatch", func(t *testing.T) {
		fake := newFakeKeyValue()
		store := &kvStore{kv: fake}
		swapped, err := store.CompareAndSwap(ctx, "k", []byte("old"), []byte("new"))
		if err != nil || swapped {
			t.Fatalf("CompareAndSwap on an absent key with a non-empty old = (%v, %v), want (false, nil)", swapped, err)
		}
		if fake.valueOf(encodeKey("k")) != nil {
			t.Error("a mismatched set-if-absent created the key")
		}
	})

	t.Run("matched live swap preserves the expiry", func(t *testing.T) {
		fake := newFakeKeyValue()
		base := time.Now().Add(time.Hour).Truncate(time.Millisecond)
		raw, err := encodeEnvelope([]byte("old"), base)
		if err != nil {
			t.Fatalf("encodeEnvelope() error = %v, want nil", err)
		}
		fake.seedRow(encodeKey("k"), raw)
		store := &kvStore{kv: fake}
		swapped, err := store.CompareAndSwap(ctx, "k", []byte("old"), []byte("new"))
		if err != nil || !swapped {
			t.Fatalf("matched live swap = (%v, %v), want (true, nil)", swapped, err)
		}
		payload, expiresAt, err := decodeEnvelope(fake.valueOf(encodeKey("k")))
		if err != nil {
			t.Fatalf("decodeEnvelope() error = %v, want nil", err)
		}
		if string(payload) != "new" {
			t.Errorf("matched swap wrote payload %q, want \"new\"", payload)
		}
		if !expiresAt.Equal(base) {
			t.Errorf("matched swap changed the expiry from %v to %v, want it preserved", base, expiresAt)
		}
	})

	t.Run("mismatched live old changes nothing", func(t *testing.T) {
		fake := newFakeKeyValue()
		raw, err := encodeEnvelope([]byte("current"), time.Time{})
		if err != nil {
			t.Fatalf("encodeEnvelope() error = %v, want nil", err)
		}
		fake.seedRow(encodeKey("k"), raw)
		store := &kvStore{kv: fake}
		swapped, err := store.CompareAndSwap(ctx, "k", []byte("stale"), []byte("new"))
		if err != nil || swapped {
			t.Fatalf("mismatched live swap = (%v, %v), want (false, nil)", swapped, err)
		}
		if got := fake.valueOf(encodeKey("k")); !bytes.Equal(got, raw) {
			t.Error("a mismatched swap modified the stored value")
		}
	})

	t.Run("expired row matches empty old only", func(t *testing.T) {
		fake := newFakeKeyValue()
		staleRaw, err := encodeEnvelope([]byte("stale"), time.Now().Add(-time.Minute))
		if err != nil {
			t.Fatalf("encodeEnvelope() error = %v, want nil", err)
		}
		fake.seedRow(encodeKey("k"), staleRaw)
		store := &kvStore{kv: fake}

		if swapped, err := store.CompareAndSwap(ctx, "k", []byte("stale"), []byte("new")); err != nil || swapped {
			t.Errorf("swap of an expired row against its stale value = (%v, %v), want (false, nil): the old value is gone", swapped, err)
		}
		swapped, err := store.CompareAndSwap(ctx, "k", nil, []byte("fresh"))
		if err != nil || !swapped {
			t.Fatalf("swap of an expired row against the empty old = (%v, %v), want (true, nil)", swapped, err)
		}
		payload, expiresAt, err := decodeEnvelope(fake.valueOf(encodeKey("k")))
		if err != nil {
			t.Fatalf("decodeEnvelope() error = %v, want nil", err)
		}
		if string(payload) != "fresh" || !expiresAt.IsZero() {
			t.Errorf("expired-row swap wrote payload %q with expiry %v, want \"fresh\" with no expiry", payload, expiresAt)
		}
	})

	t.Run("corrupt envelope surfaces as an error", func(t *testing.T) {
		fake := newFakeKeyValue()
		fake.seedRow(encodeKey("k"), []byte("x"))
		store := &kvStore{kv: fake}
		if _, err := store.CompareAndSwap(ctx, "k", nil, []byte("new")); err == nil || !strings.Contains(err.Error(), "malformed envelope") {
			t.Fatalf("CompareAndSwap on a corrupt envelope error = %v, want a malformed-envelope error", err)
		}
	})
}

// TestKVStore_CompareAndSwap_LostRaceReportsFalseNotError pins that a lost
// race on either write half is reported as an ordinary "the swap did not
// happen" -- never as an error -- so a caller cannot tell a race apart from a
// genuine mismatch, exactly as pkgcore.KVStore promises.
func TestKVStore_CompareAndSwap_LostRaceReportsFalseNotError(t *testing.T) {
	ctx := context.Background()

	t.Run("create race", func(t *testing.T) {
		fake := newFakeKeyValue()
		fake.createExistsFailures = 1
		store := &kvStore{kv: fake}
		swapped, err := store.CompareAndSwap(ctx, "k", nil, []byte("new"))
		if err != nil || swapped {
			t.Fatalf("set-if-absent with a lost create race = (%v, %v), want (false, nil)", swapped, err)
		}
	})

	t.Run("update race", func(t *testing.T) {
		fake := newFakeKeyValue()
		raw, err := encodeEnvelope([]byte("old"), time.Time{})
		if err != nil {
			t.Fatalf("encodeEnvelope() error = %v, want nil", err)
		}
		fake.seedRow(encodeKey("k"), raw)
		fake.updateMismatchFailures = 1
		store := &kvStore{kv: fake}
		swapped, err := store.CompareAndSwap(ctx, "k", []byte("old"), []byte("new"))
		if err != nil || swapped {
			t.Fatalf("matched swap with a lost update race = (%v, %v), want (false, nil)", swapped, err)
		}
	})
}

// TestKVStore_ConcurrentIncrements_OverFakeBucket is the concurrency proof
// this store's revision-guarded loop exists for: many goroutines incrementing
// one key through the fake bucket lose races against each other for real (the
// fake serializes only its own state, never the read-modify-write window the
// store performs outside it), and the loop's retries must converge on exactly
// the sum of every increment -- no lost update, no double count.
func TestKVStore_ConcurrentIncrements_OverFakeBucket(t *testing.T) {
	fake := newFakeKeyValue()
	store := &kvStore{kv: fake}
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

	payload, _, err := decodeEnvelope(fake.valueOf(encodeKey("counter")))
	if err != nil {
		t.Fatalf("decodeEnvelope() error = %v, want nil", err)
	}
	const want = goroutines * incrementsPerGoroutine
	if string(payload) != strconv.Itoa(want) {
		t.Errorf("final counter = %q, want %d: a revision-guarded loop must lose no update", payload, want)
	}
}

// TestRefuseUnadoptable pins refuseUnadoptable's two outcomes: a bucket whose
// configuration read-back fails refuses with a wrapped error, and a compliant
// configuration adopts cleanly.
func TestRefuseUnadoptable(t *testing.T) {
	ctx := context.Background()

	t.Run("configuration read-back failure is wrapped", func(t *testing.T) {
		fake := newFakeKeyValue()
		fake.statusErr = errors.New("server went away")
		err := refuseUnadoptable(ctx, "bucket", fake)
		if err == nil || !strings.Contains(err.Error(), "read back adopted bucket") {
			t.Fatalf("refuseUnadoptable() error = %v, want a wrapped read-back error", err)
		}
	})

	t.Run("compliant configuration adopts", func(t *testing.T) {
		fake := newFakeKeyValue()
		if err := refuseUnadoptable(ctx, "bucket", fake); err != nil {
			t.Fatalf("refuseUnadoptable() error = %v, want nil for a compliant configuration", err)
		}
	})
}

// TestBucketNameInUse pins the create-race classification: both nats.go
// sentinels and the server's own fixed wording count as "the bucket exists
// now, adopt it", and an unrelated error does not.
func TestBucketNameInUse(t *testing.T) {
	t.Parallel()

	inUse := []error{
		jetstream.ErrBucketExists,
		jetstream.ErrStreamNameAlreadyInUse,
		errors.New("nats: stream name already in use"),
	}
	for _, err := range inUse {
		if !bucketNameInUse(err) {
			t.Errorf("bucketNameInUse(%v) = false, want true", err)
		}
	}
	if bucketNameInUse(errors.New("nats: insufficient resources")) {
		t.Error("bucketNameInUse(unrelated error) = true, want false")
	}
}

// TestSleepOrDone pins the retry backoff's cancellation behaviour: a
// cancelled context returns its error immediately instead of waiting out the
// delay, and an uncancelled context waits it out.
func TestSleepOrDone(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepOrDone(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("sleepOrDone on a cancelled context error = %v, want context.Canceled", err)
	}

	start := time.Now()
	if err := sleepOrDone(context.Background(), 20*time.Millisecond); err != nil {
		t.Errorf("sleepOrDone on a live context error = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed < 15*time.Millisecond {
		t.Errorf("sleepOrDone returned after %v, want it to wait out the delay", elapsed)
	}
}

// TestKVStore_SetWithPositiveTTL_AttachesTheExpiry pins the Set expiry
// branch the lifecycle test above deliberately skips: a Set with a positive
// ttl writes an envelope whose expiry is the caller's ttl from now, and a
// later Set without one clears it.
func TestKVStore_SetWithPositiveTTL_AttachesTheExpiry(t *testing.T) {
	fake := newFakeKeyValue()
	store := &kvStore{kv: fake}
	ctx := context.Background()

	ttl := 30 * time.Minute
	if err := store.Set(ctx, "k", []byte("v"), ttl); err != nil {
		t.Fatalf("Set with a ttl error = %v, want nil", err)
	}
	_, expiresAt, err := decodeEnvelope(fake.valueOf(encodeKey("k")))
	if err != nil {
		t.Fatalf("decodeEnvelope() error = %v, want nil", err)
	}
	if expiresAt.Before(time.Now().Add(ttl-5*time.Second)) || expiresAt.After(time.Now().Add(ttl+5*time.Second)) {
		t.Errorf("Set-with-ttl expiry = %v, want roughly now+%v", expiresAt, ttl)
	}

	if err := store.Set(ctx, "k", []byte("v"), 0); err != nil {
		t.Fatalf("Set without a ttl error = %v, want nil", err)
	}
	_, expiresAt, err = decodeEnvelope(fake.valueOf(encodeKey("k")))
	if err != nil {
		t.Fatalf("decodeEnvelope() error = %v, want nil", err)
	}
	if !expiresAt.IsZero() {
		t.Errorf("Set-without-ttl expiry = %v, want the previous expiry cleared", expiresAt)
	}
}

// TestKVStore_BackoffReachesItsCapUnderSustainedContention pins the retry
// backoff's ceiling: a long run of lost create races (more than the
// doubling range can express) keeps retrying at incrBackoffCap instead of
// growing without bound.
func TestKVStore_BackoffReachesItsCapUnderSustainedContention(t *testing.T) {
	fake := newFakeKeyValue()
	fake.createExistsFailures = 10
	store := &kvStore{kv: fake}
	ctx := context.Background()

	start := time.Now()
	result, err := store.IncrByFloat(ctx, "n", 1)
	if err != nil {
		t.Fatalf("IncrByFloat under sustained contention error = %v, want nil (the loop retries)", err)
	}
	if result != 1 {
		t.Fatalf("IncrByFloat under sustained contention = %v, want 1", result)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("contended increment took %v, want the capped backoff to keep it well under a second of retries", elapsed)
	}
}

// TestKVStore_IncrementAndSwap_ServerFailuresSurfacedWrapped pins the
// generic-failure wraps: a Create, Update or read that fails with something
// other than the race sentinels surfaces as a wrapped error naming the
// operation, so a caller can tell a lost race (retried or reported as a
// non-swap) apart from a genuinely failing server.
func TestKVStore_IncrementAndSwap_ServerFailuresSurfacedWrapped(t *testing.T) {
	ctx := context.Background()

	t.Run("increment read failure is wrapped", func(t *testing.T) {
		fake := newFakeKeyValue()
		fake.getErr = errors.New("server went away")
		store := &kvStore{kv: fake}
		if _, err := store.IncrByFloat(ctx, "n", 1); err == nil || !strings.Contains(err.Error(), "pkgcore/kv/nats: incr:") {
			t.Fatalf("IncrByFloat() error = %v, want a wrapped error naming the incr", err)
		}
	})

	t.Run("increment create failure is wrapped", func(t *testing.T) {
		fake := newFakeKeyValue()
		fake.createErr = errors.New("server went away")
		store := &kvStore{kv: fake}
		if _, err := store.IncrByFloat(ctx, "n", 1); err == nil || !strings.Contains(err.Error(), "pkgcore/kv/nats: incr:") {
			t.Fatalf("IncrByFloat() error = %v, want a wrapped error naming the incr", err)
		}
	})

	t.Run("increment update failure is wrapped", func(t *testing.T) {
		fake := newFakeKeyValue()
		seed, err := encodeEnvelope([]byte("5"), time.Time{})
		if err != nil {
			t.Fatalf("encodeEnvelope() error = %v, want nil", err)
		}
		fake.seedRow(encodeKey("n"), seed)
		fake.updateErr = errors.New("server went away")
		store := &kvStore{kv: fake}
		if _, err := store.IncrByFloat(ctx, "n", 1); err == nil || !strings.Contains(err.Error(), "pkgcore/kv/nats: incr:") {
			t.Fatalf("IncrByFloat() error = %v, want a wrapped error naming the incr", err)
		}
	})

	t.Run("increment over a corrupt envelope is refused", func(t *testing.T) {
		fake := newFakeKeyValue()
		fake.seedRow(encodeKey("n"), []byte("x"))
		store := &kvStore{kv: fake}
		if _, err := store.IncrByFloat(ctx, "n", 1); err == nil || !strings.Contains(err.Error(), "malformed envelope") {
			t.Fatalf("IncrByFloat() over a corrupt envelope error = %v, want a malformed-envelope error", err)
		}
	})

	t.Run("swap read failure is wrapped", func(t *testing.T) {
		fake := newFakeKeyValue()
		fake.getErr = errors.New("server went away")
		store := &kvStore{kv: fake}
		if _, err := store.CompareAndSwap(ctx, "k", nil, []byte("v")); err == nil || !strings.Contains(err.Error(), "pkgcore/kv/nats: cas:") {
			t.Fatalf("CompareAndSwap() error = %v, want a wrapped error naming the cas", err)
		}
	})

	t.Run("swap create failure is wrapped", func(t *testing.T) {
		fake := newFakeKeyValue()
		fake.createErr = errors.New("server went away")
		store := &kvStore{kv: fake}
		if _, err := store.CompareAndSwap(ctx, "k", nil, []byte("v")); err == nil || !strings.Contains(err.Error(), "pkgcore/kv/nats: cas:") {
			t.Fatalf("CompareAndSwap() error = %v, want a wrapped error naming the cas", err)
		}
	})

	t.Run("swap update failure is wrapped", func(t *testing.T) {
		fake := newFakeKeyValue()
		seed, err := encodeEnvelope([]byte("old"), time.Time{})
		if err != nil {
			t.Fatalf("encodeEnvelope() error = %v, want nil", err)
		}
		fake.seedRow(encodeKey("k"), seed)
		fake.updateErr = errors.New("server went away")
		store := &kvStore{kv: fake}
		if _, err := store.CompareAndSwap(ctx, "k", []byte("old"), []byte("new")); err == nil || !strings.Contains(err.Error(), "pkgcore/kv/nats: cas:") {
			t.Fatalf("CompareAndSwap() error = %v, want a wrapped error naming the cas", err)
		}
	})
}
