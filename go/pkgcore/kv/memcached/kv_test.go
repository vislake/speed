package memcached

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"testing"
	"time"
	"unsafe"

	"github.com/bradfitz/gomemcache/memcache"
)

// Hermetic unit tests for the Memcached-backed KVStore: everything here runs
// without a Memcached server. Behaviour that needs a real server -- the
// envelope's actual expiry semantics, the compare-and-swap loops' real
// races -- lives in the integration tier (integration_test/kv_test.go); what
// belongs here is what is local to the store itself: a nil client is a
// wiring error reported at construction, a cancelled context fails every
// operation before any command reaches the wire, and the pure encode/decode/
// exptime-derivation helpers behave correctly across their boundary cases.

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

// TestKVStore_CancelledContext pins that no operation runs on a cancelled
// context: every method returns the context's error instead of performing
// the operation, mirroring kv/redis's identical test for the identical
// contract. The client points at a closed port, so an operation that ignored
// the context and reached for the server would have to fail with a
// connection error, never the context's error this test asserts.
func TestKVStore_CancelledContext(t *testing.T) {
	t.Parallel()

	client := memcache.New("127.0.0.1:1")
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

// TestEncodeDecodeEnvelope_RoundTrips pins that the envelope carries an
// arbitrary byte payload -- embedded NUL bytes included -- and its expiry
// timestamp through unchanged, for both the "never expires" and "expires at"
// shapes.
func TestEncodeDecodeEnvelope_RoundTrips(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		value     []byte
		expiresAt time.Time
	}{
		{"no expiry", []byte("hello"), time.Time{}},
		{"with expiry", []byte{0x00, 0x01, 0xff, 0x00}, time.Now().Add(time.Hour).Truncate(time.Nanosecond)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := encodeEnvelope(tt.value, tt.expiresAt)
			if err != nil {
				t.Fatalf("encodeEnvelope() error = %v, want nil", err)
			}
			value, expiresAt, ok := decodeEnvelope(encoded)
			if !ok {
				t.Fatal("decodeEnvelope() ok = false, want true")
			}
			if string(value) != string(tt.value) {
				t.Errorf("decodeEnvelope() value = %v, want %v", value, tt.value)
			}
			if !expiresAt.Equal(tt.expiresAt) {
				t.Errorf("decodeEnvelope() expiresAt = %v, want %v", expiresAt, tt.expiresAt)
			}
		})
	}
}

// TestDecodeEnvelope_TooShortReportsNotOK pins that a stored value shorter
// than the envelope header -- one this store could never have written
// itself -- is reported rather than misparsed.
func TestDecodeEnvelope_TooShortReportsNotOK(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, 1, 7} {
		if _, _, ok := decodeEnvelope(make([]byte, n)); ok {
			t.Errorf("decodeEnvelope(%d bytes) ok = true, want false", n)
		}
	}
}

// TestPhysicalExptime_NeverExpiresIsZero pins that a zero time.Time -- this
// store's "no expiry" shape -- maps to Memcached's own "never expire" value.
func TestPhysicalExptime_NeverExpiresIsZero(t *testing.T) {
	t.Parallel()

	if got := physicalExptime(time.Time{}); got != 0 {
		t.Errorf("physicalExptime(zero time) = %d, want 0", got)
	}
}

// TestPhysicalExptime_SubSecondRoundsUpToOne pins that a ttl under one
// second never becomes Memcached's "never expire" 0 -- the exact bug this
// package's envelope design exists to route around at the logical level, and
// which the physical exptime itself must not reintroduce as a backstop that
// never fires.
func TestPhysicalExptime_SubSecondRoundsUpToOne(t *testing.T) {
	t.Parallel()

	got := physicalExptime(time.Now().Add(25 * time.Millisecond))
	if got != 1 {
		t.Errorf("physicalExptime(25ms from now) = %d, want 1", got)
	}
}

// TestPhysicalExptime_BeyondThirtyDaysUsesAbsoluteUnixTime pins the real
// Memcached wire-protocol switch: past memcachedMaxRelativeExpiry seconds,
// the exptime field must be an absolute Unix timestamp rather than a
// relative offset, or the server would silently treat it as a spuriously
// tiny relative expiry instead of the long one the caller asked for.
func TestPhysicalExptime_BeyondThirtyDaysUsesAbsoluteUnixTime(t *testing.T) {
	t.Parallel()

	expiresAt := time.Now().Add(60 * 24 * time.Hour) // 60 days out
	got := physicalExptime(expiresAt)
	want := int32(expiresAt.Unix())
	// Allow a small slack for the wall-clock read inside physicalExptime
	// itself happening a moment after expiresAt was computed above.
	if diff := got - want; diff < -2 || diff > 2 {
		t.Errorf("physicalExptime(60 days from now) = %d, want close to %d (absolute Unix time)", got, want)
	}
	if got <= memcachedMaxRelativeExpiry {
		t.Errorf("physicalExptime(60 days from now) = %d, want a value recognizable as an absolute Unix timestamp, not a relative offset", got)
	}
}

// TestExpiryFromTTL_NonPositiveMeansNever pins that a zero or negative ttl
// -- the KVStore contract's "store the key without an expiry" boundary --
// produces the zero time.Time this store treats as "never".
func TestExpiryFromTTL_NonPositiveMeansNever(t *testing.T) {
	t.Parallel()

	for _, ttl := range []time.Duration{0, -time.Second} {
		if got := expiryFromTTL(ttl); !got.IsZero() {
			t.Errorf("expiryFromTTL(%v) = %v, want the zero time.Time", ttl, got)
		}
	}
}

// TestIsLostCASRace_ClassifiesTheThreeRaceAnswers pins the exact set of
// gomemcache errors this store treats as "someone else won the race, retry"
// rather than a hard failure.
func TestIsLostCASRace_ClassifiesTheThreeRaceAnswers(t *testing.T) {
	t.Parallel()

	race := []error{memcache.ErrCASConflict, memcache.ErrNotStored, memcache.ErrCacheMiss}
	for _, err := range race {
		if !isLostCASRace(err) {
			t.Errorf("isLostCASRace(%v) = false, want true", err)
		}
	}
	if isLostCASRace(errors.New("some other failure")) {
		t.Error("isLostCASRace(unrelated error) = true, want false")
	}
	if isLostCASRace(nil) {
		t.Error("isLostCASRace(nil) = true, want false")
	}
}

// TestIsTransientServerErr_ClassifiesTransportFailures pins the boundary
// isTransientServerErr draws for the retry loops' read side: a connection
// the server closed under the call (io.EOF) and a round trip that timed out
// (a net.Error) are transient and retried, while an ordinary wrapped error
// is not.
func TestIsTransientServerErr_ClassifiesTransportFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "io.EOF is transient", err: io.EOF, want: true},
		{name: "a wrapped io.EOF is transient", err: fmt.Errorf("pkgcore/kv/memcached: get: %w", io.EOF), want: true},
		{name: "a net timeout is transient", err: &net.OpError{Err: &timeoutErr{}}, want: true},
		{name: "a wrapped net timeout is transient", err: fmt.Errorf("pkgcore/kv/memcached: get: %w", &net.OpError{Err: &timeoutErr{}}), want: true},
		{name: "a plain error is not transient", err: errors.New("connection refused"), want: false},
		{name: "a corrupt envelope is not transient", err: fmt.Errorf("pkgcore/kv/memcached: get: %w", errCorruptEnvelope), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransientServerErr(tt.err); got != tt.want {
				t.Errorf("isTransientServerErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// timeoutErr is a net.Error that is a timeout and not otherwise interesting,
// for the table above.
type timeoutErr struct{}

func (*timeoutErr) Error() string   { return "i/o timeout" }
func (*timeoutErr) Timeout() bool   { return true }
func (*timeoutErr) Temporary() bool { return true }

// TestKVStore_Set_EnvelopeSizeOverflowValueRefused pins the fix for the
// allocation-size overflow CodeQL flagged in encodeEnvelope (rule id
// go/allocation-size-overflow): the envelope buffer's size is header plus
// payload, computed in the platform's int, and a payload length sitting
// within kvEnvelopeHeaderSize bytes of the int maximum wraps that addition
// negative, so make would panic on an allocation no such envelope can ever
// need. The refusal is driven through the store's own Set -- the overflow
// must surface as Set's error, never as a panic from the allocation itself.
func TestKVStore_Set_EnvelopeSizeOverflowValueRefused(t *testing.T) {
	t.Parallel()

	// The refusal fires before anything reaches the client, so the client
	// may point at a dead address.
	store := NewKVStore(memcache.New("127.0.0.1:1"))

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
	}{data: uintptr(unsafe.Pointer(&b)), len: math.MaxInt - 1, cap: math.MaxInt - 1} // kvEnvelopeHeaderSize + len(value) wraps past MaxInt
	value := *(*[]byte)(unsafe.Pointer(&header))
	if err := store.Set(context.Background(), "k", value, 0); err == nil {
		t.Fatal("Set() error = nil, want a refusal error for a value whose envelope size arithmetic would overflow")
	}
}
