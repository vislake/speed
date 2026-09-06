package nats

// Hermetic unit tests for the NATS-backed KVStore: everything here runs
// without a NATS server. That is a narrower set than kv/redis's own unit
// tier can cover, for a structural reason: kv/redis's NewKVStore never dials
// (go-redis connects lazily), so its unit tests can build a real store
// against an address nothing listens on and assert its cancelled-context
// behaviour directly. This package's NewKVStore provisions a JetStream KV
// bucket at construction, which is itself a round trip to the server, so no
// *kvStore can exist here without a live NATS -- the equivalent
// cancelled-context behaviour is proven instead by
// kvstoretest.AssertConforms's own subtest of the same name, run against a
// real container in integration_test/kv_test.go.
//
// What belongs here is what needs no server at all: a nil connection is a
// wiring error reported at construction, the envelope encoding round-trips
// and rejects malformed input, and every key this package's own key-encoding
// scheme produces is valid under JetStream's own key charset regardless of
// what the original opaque key contained.

import (
	"bytes"
	"regexp"
	"testing"
	"time"
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

			encoded := encodeEnvelope(tt.value, tt.expiresAt)
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
