package memcached

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/bradfitz/gomemcache/memcache"

	"github.com/vislake/speed/go/pkgcore"
)

// Hermetic unit tests for the Memcached-backed KVStore: everything here runs
// without a Memcached server. Behaviour that needs a real server -- the
// envelope's actual expiry semantics against a real eviction clock, the
// compare-and-swap loops' races across real network round trips -- lives in
// the integration tier (integration_test/kv_test.go). What belongs in this
// file is what needs no server at all: a nil client is a wiring error
// reported at construction, a cancelled context fails every operation before
// any command reaches the wire, and the pure encode/decode/exptime-derivation
// helpers behave correctly across their boundary cases.
//
// The store's own logic is a further tier, proven below against an
// in-process fake speaking the memcached text protocol on a loopback socket:
// the real gomemcache client drives genuine wire round trips against it, so
// Get/Set/Delete, both increment variants and CompareAndSwap execute their
// real read-modify-write and compare-and-swap paths -- including genuine
// lost races under concurrent writers -- with no server process. The fake is
// deliberately naive (no eviction, no expiry clock: the store's own envelope
// is what decides what a Get reports, and the fake's rows never disappear on
// their own), which is exactly the slice of Memcached semantics this store
// relies on.

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

// fakeMemcachedServer is an in-process server speaking just enough of the
// memcached text protocol for gomemcache's client to transact against it:
// the storage commands set/add/cas, gets, and delete. It is deliberately
// naive about the two dimensions the store itself is authoritative over --
// it never evicts and never expires anything, so a row only disappears when
// a delete lands, and every read answers the raw stored bytes; the store's
// own envelope clock is what decides what a Get reports. CAS semantics are
// real: every write assigns a fresh token and a cas at a stale token answers
// EXISTS, which is what lets concurrent store operations lose races against
// each other for genuine reasons.
type fakeMemcachedServer struct {
	ln net.Listener

	mu     sync.Mutex
	rows   map[string]*fakeMemcachedRow
	nextID uint64

	// Scripted one-shot behaviours, consumed under the lock.
	casConflicts  int  // next cas requests to answer EXISTS with (a lost race)
	addConflicts  int  // next add requests to answer NOT_STORED with (a lost race)
	addServerErrs int  // next add requests to answer SERVER_ERROR with
	dropNextReads bool // close the connection on the next gets without answering
}

func (s *fakeMemcachedServer) scriptCASConflict()    { s.casConflicts++ }
func (s *fakeMemcachedServer) scriptAddConflict()    { s.addConflicts++ }
func (s *fakeMemcachedServer) scriptAddServerError() { s.addServerErrs++ }
func (s *fakeMemcachedServer) scriptDroppedRead()    { s.dropNextReads = true }

type fakeMemcachedRow struct {
	value []byte
	casID uint64
}

// startFakeMemcachedServer starts the fake on a loopback port and returns
// it; the listener is closed when the test finishes.
func startFakeMemcachedServer(t *testing.T) *fakeMemcachedServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v, want nil", err)
	}
	s := &fakeMemcachedServer{ln: ln, rows: make(map[string]*fakeMemcachedRow)}
	go s.acceptLoop()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *fakeMemcachedServer) addr() string { return s.ln.Addr().String() }

func (s *fakeMemcachedServer) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *fakeMemcachedServer) handleConn(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)

	reply := func(line string) {
		w.WriteString(line)
		w.WriteString("\r\n")
		w.Flush()
	}

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		fields := strings.Split(line, " ")
		if len(fields) == 0 {
			reply("ERROR")
			continue
		}
		switch fields[0] {
		case "get", "gets":
			s.mu.Lock()
			if s.dropNextReads {
				s.dropNextReads = false
				s.mu.Unlock()
				return // close the connection without answering: an io.EOF read
			}
			for _, key := range fields[1:] {
				row, ok := s.rows[key]
				if !ok {
					continue
				}
				fmt.Fprintf(w, "VALUE %s 0 %d %d\r\n", key, len(row.value), row.casID)
				w.Write(row.value)
				w.WriteString("\r\n")
			}
			s.mu.Unlock()
			reply("END")
		case "set", "add", "cas":
			if len(fields) < 5 {
				reply("ERROR")
				continue
			}
			key := fields[1]
			size, err := strconv.Atoi(fields[4])
			if err != nil || size < 0 {
				reply("CLIENT_ERROR bad data chunk")
				continue
			}
			value := make([]byte, size+2)
			if _, err := io.ReadFull(r, value); err != nil {
				return
			}
			value = value[:size]

			s.mu.Lock()
			switch fields[0] {
			case "set":
				s.storeLocked(key, value)
				reply("STORED")
			case "add":
				if s.addServerErrs > 0 {
					s.addServerErrs--
					reply("SERVER_ERROR out of memory")
				} else if s.addConflicts > 0 {
					s.addConflicts--
					reply("NOT_STORED")
				} else if _, exists := s.rows[key]; exists {
					reply("NOT_STORED")
				} else {
					s.storeLocked(key, value)
					reply("STORED")
				}
			case "cas":
				want, err := strconv.ParseUint(fields[5], 10, 64)
				if err != nil {
					reply("ERROR")
				} else if s.casConflicts > 0 {
					s.casConflicts--
					reply("EXISTS")
				} else if row, exists := s.rows[key]; !exists {
					reply("NOT_FOUND")
				} else if row.casID != want {
					reply("EXISTS")
				} else {
					s.storeLocked(key, value)
					reply("STORED")
				}
			}
			s.mu.Unlock()
		case "delete":
			if len(fields) < 2 {
				reply("ERROR")
				continue
			}
			s.mu.Lock()
			if _, exists := s.rows[fields[1]]; !exists {
				reply("NOT_FOUND")
			} else {
				delete(s.rows, fields[1])
				reply("DELETED")
			}
			s.mu.Unlock()
		default:
			reply("ERROR")
		}
	}
}

// storeLocked writes row under the caller-held lock, assigning a fresh CAS
// token.
func (s *fakeMemcachedServer) storeLocked(key string, value []byte) {
	s.nextID++
	s.rows[key] = &fakeMemcachedRow{value: value, casID: s.nextID}
}

// rawValue returns key's current raw bytes under the lock, for asserting
// exactly what a store operation wrote to the server.
func (s *fakeMemcachedServer) rawValue(key string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[key]
	if !ok {
		return nil
	}
	return bytes.Clone(row.value)
}

// decodeRow decodes the envelope bytes the fake server holds for key,
// failing the test when nothing is stored or the envelope is corrupt.
func decodeRow(t *testing.T, srv *fakeMemcachedServer, key string) ([]byte, time.Time) {
	t.Helper()
	raw := srv.rawValue(key)
	if raw == nil {
		t.Fatalf("server holds no row for %q", key)
	}
	value, expiresAt, ok := decodeEnvelope(raw)
	if !ok {
		t.Fatalf("decodeEnvelope(server row for %q) ok = false, want true", key)
	}
	return value, expiresAt
}

// TestKVStore_GetSetDelete_OverFakeServer drives the plain lifecycle over
// real wire round trips: an absent key is a miss, Set stores and replaces,
// Delete removes, and deleting an absent key is not an error.
func TestKVStore_GetSetDelete_OverFakeServer(t *testing.T) {
	srv := startFakeMemcachedServer(t)
	store := NewKVStore(memcache.New(srv.addr()))
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
	if raw := srv.rawValue("k"); raw == nil {
		t.Fatal("server holds nothing for the key, want the envelope the store set")
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

// TestKVStore_EnvelopeExpiryRules_OverFakeServer pins the envelope-clock
// semantics over the wire: a ttl-less key stays readable, a key set with a
// short ttl becomes a miss once the envelope's own deadline passes, and an
// overwrite clears a previous expiry.
func TestKVStore_EnvelopeExpiryRules_OverFakeServer(t *testing.T) {
	srv := startFakeMemcachedServer(t)
	store := NewKVStore(memcache.New(srv.addr()))
	ctx := context.Background()

	if err := store.Set(ctx, "forever", []byte("v"), 0); err != nil {
		t.Fatalf("Set(forever) error = %v, want nil", err)
	}
	if err := store.Set(ctx, "brief", []byte("v"), 25*time.Millisecond); err != nil {
		t.Fatalf("Set(brief) error = %v, want nil", err)
	}
	if _, ok, err := store.Get(ctx, "brief"); err != nil || !ok {
		t.Fatalf("Get(brief) immediately after Set = (_, %v, %v), want (_, true, nil)", ok, err)
	}

	// Wait well past the envelope deadline: the fake server never evicts, so
	// this can only pass if the store's own logical expiry is what decides.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok, err := store.Get(ctx, "brief"); err == nil && !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Get(brief) still reports the key present long after its envelope expired")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok, err := store.Get(ctx, "forever"); err != nil || !ok {
		t.Errorf("Get(forever) = (_, %v, %v), want (_, true, nil): a ttl-less key must never expire", ok, err)
	}

	// Set clears an existing expiry: revive the key without one and check it
	// outlives the original brief window by a wide margin.
	if err := store.Set(ctx, "brief", []byte("v"), 0); err != nil {
		t.Fatalf("Set(brief, no ttl) error = %v, want nil", err)
	}
	time.Sleep(150 * time.Millisecond)
	if _, ok, err := store.Get(ctx, "brief"); err != nil || !ok {
		t.Errorf("Get(brief) after the expiry-clearing Set = (_, %v, %v), want (_, true, nil)", ok, err)
	}
}

// TestKVStore_IncrByFloat_OverFakeServer drives the increment contract over
// real wire round trips: a fresh key is created at zero plus delta with no
// expiry, a live key accumulates and keeps its own expiry, a logically
// expired row restarts from zero with no expiry, and a non-numeric live
// value fails with pkgcore.ErrNotNumeric untouched.
func TestKVStore_IncrByFloat_OverFakeServer(t *testing.T) {
	srv := startFakeMemcachedServer(t)
	store := NewKVStore(memcache.New(srv.addr()))
	ctx := context.Background()

	result, err := store.IncrByFloat(ctx, "n", 2.5)
	if err != nil {
		t.Fatalf("IncrByFloat on a fresh key error = %v, want nil", err)
	}
	if result != 2.5 {
		t.Fatalf("IncrByFloat on a fresh key = %v, want 2.5", result)
	}
	payload, expiresAt := decodeRow(t, srv, "n")
	if string(payload) != "2.5" || !expiresAt.IsZero() {
		t.Errorf("fresh increment wrote payload %q with expiry %v, want \"2.5\" with no expiry", payload, expiresAt)
	}

	// A live key keeps its own expiry through an increment.
	if err := store.Set(ctx, "n", []byte("10"), 45*time.Minute); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}
	before := time.Now()
	if result, err := store.IncrByFloat(ctx, "n", 5); err != nil || result != 15 {
		t.Fatalf("IncrByFloat on a live key = (%v, %v), want (15, nil)", result, err)
	}
	payload, expiresAt = decodeRow(t, srv, "n")
	if string(payload) != "15" {
		t.Errorf("live increment wrote payload %q, want \"15\"", payload)
	}
	if expiresAt.Before(before.Add(44*time.Minute)) || expiresAt.IsZero() {
		t.Errorf("live increment expiry = %v, want the key's own expiry preserved", expiresAt)
	}

	// A logically expired row restarts from zero, shedding the stale expiry.
	if err := store.Set(ctx, "e", []byte("99"), 25*time.Millisecond); err != nil {
		t.Fatalf("Set(e) error = %v, want nil", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		raw := srv.rawValue("e")
		if raw == nil {
			t.Fatal("row e disappeared from the server before its logical expiry")
		}
		_, expiresAt, ok := decodeEnvelope(raw)
		if ok && !expiresAt.IsZero() && time.Now().After(expiresAt) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("row e never reached its logical expiry")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if result, err := store.IncrByFloat(ctx, "e", 1); err != nil || result != 1 {
		t.Fatalf("IncrByFloat over an expired row = (%v, %v), want (1, nil)", result, err)
	}
	payload, expiresAt = decodeRow(t, srv, "e")
	if string(payload) != "1" || !expiresAt.IsZero() {
		t.Errorf("increment over an expired row wrote payload %q with expiry %v, want \"1\" with no expiry", payload, expiresAt)
	}

	// A non-numeric live value fails with ErrNotNumeric.
	if err := store.Set(ctx, "bad", []byte("not-a-number"), 0); err != nil {
		t.Fatalf("Set(bad) error = %v, want nil", err)
	}
	if _, err := store.IncrByFloat(ctx, "bad", 1); !errors.Is(err, pkgcore.ErrNotNumeric) {
		t.Errorf("IncrByFloat on a non-numeric value error = %v, want pkgcore.ErrNotNumeric", err)
	}
}

// TestKVStore_IncrByFloatWithTTL_OverFakeServer drives the ttl-attaching
// sibling over the wire: only a fresh key gets ttl attached, and a live
// key's own expiry is carried through untouched.
func TestKVStore_IncrByFloatWithTTL_OverFakeServer(t *testing.T) {
	srv := startFakeMemcachedServer(t)
	store := NewKVStore(memcache.New(srv.addr()))
	ctx := context.Background()

	ttl := 45 * time.Minute
	if _, err := store.IncrByFloatWithTTL(ctx, "n", 3, ttl); err != nil {
		t.Fatalf("IncrByFloatWithTTL on a fresh key error = %v, want nil", err)
	}
	_, expiresAt := decodeRow(t, srv, "n")
	if expiresAt.Before(time.Now().Add(ttl-5*time.Second)) || expiresAt.IsZero() {
		t.Errorf("fresh-key increment expiry = %v, want roughly now+%v", expiresAt, ttl)
	}

	// Live key with an expiry: the ttl argument must not extend it.
	base := time.Now().Add(45 * time.Minute)
	if err := store.Set(ctx, "n", []byte("7"), ttl); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}
	if result, err := store.IncrByFloatWithTTL(ctx, "n", 1, 10*time.Second); err != nil || result != 8 {
		t.Fatalf("IncrByFloatWithTTL on a live key = (%v, %v), want (8, nil)", result, err)
	}
	_, expiresAt = decodeRow(t, srv, "n")
	if expiresAt.Before(base) || expiresAt.After(base.Add(10*time.Second)) {
		t.Errorf("live-key increment expiry = %v, want the key's own expiry (~now+45m) preserved, not shortened to the ttl argument", expiresAt)
	}
}

// TestKVStore_CompareAndSwap_OverFakeServer drives CompareAndSwap's shapes
// over real cas round trips: set-if-absent for a missing key, immediate
// false for a missing key with a non-empty old, a matched live swap that
// preserves expiry, a mismatched live swap that changes nothing, and an
// expired row revivable only against the empty old.
func TestKVStore_CompareAndSwap_OverFakeServer(t *testing.T) {
	srv := startFakeMemcachedServer(t)
	store := NewKVStore(memcache.New(srv.addr()))
	ctx := context.Background()

	t.Run("set-if-absent", func(t *testing.T) {
		swapped, err := store.CompareAndSwap(ctx, "k", nil, []byte("new"))
		if err != nil || !swapped {
			t.Fatalf("CompareAndSwap(set-if-absent) = (%v, %v), want (true, nil)", swapped, err)
		}
		payload, expiresAt := decodeRow(t, srv, "k")
		if string(payload) != "new" || !expiresAt.IsZero() {
			t.Errorf("set-if-absent wrote payload %q with expiry %v, want \"new\" with no expiry", payload, expiresAt)
		}
	})

	t.Run("absent key with non-empty old is a mismatch", func(t *testing.T) {
		swapped, err := store.CompareAndSwap(ctx, "absent", []byte("old"), []byte("new"))
		if err != nil || swapped {
			t.Fatalf("CompareAndSwap on an absent key with a non-empty old = (%v, %v), want (false, nil)", swapped, err)
		}
		if srv.rawValue("absent") != nil {
			t.Error("a mismatched set-if-absent created the key")
		}
	})

	t.Run("matched live swap preserves the expiry", func(t *testing.T) {
		if err := store.Set(ctx, "k", []byte("old"), 45*time.Minute); err != nil {
			t.Fatalf("Set() error = %v, want nil", err)
		}
		before := time.Now()
		swapped, err := store.CompareAndSwap(ctx, "k", []byte("old"), []byte("new"))
		if err != nil || !swapped {
			t.Fatalf("matched live swap = (%v, %v), want (true, nil)", swapped, err)
		}
		payload, expiresAt := decodeRow(t, srv, "k")
		if string(payload) != "new" {
			t.Errorf("matched swap wrote payload %q, want \"new\"", payload)
		}
		if expiresAt.Before(before.Add(44*time.Minute)) || expiresAt.IsZero() {
			t.Errorf("matched swap expiry = %v, want the key's own expiry preserved", expiresAt)
		}
	})

	t.Run("mismatched live old changes nothing", func(t *testing.T) {
		if err := store.Set(ctx, "k", []byte("current"), 0); err != nil {
			t.Fatalf("Set() error = %v, want nil", err)
		}
		swapped, err := store.CompareAndSwap(ctx, "k", []byte("stale"), []byte("new"))
		if err != nil || swapped {
			t.Fatalf("mismatched live swap = (%v, %v), want (false, nil)", swapped, err)
		}
		payload, _ := decodeRow(t, srv, "k")
		if string(payload) != "current" {
			t.Errorf("mismatched swap changed the stored value to %q, want it untouched", payload)
		}
	})

	t.Run("expired row matches empty old only", func(t *testing.T) {
		if err := store.Set(ctx, "k", []byte("stale"), 25*time.Millisecond); err != nil {
			t.Fatalf("Set() error = %v, want nil", err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			raw := srv.rawValue("k")
			if raw == nil {
				t.Fatal("row disappeared from the server before its logical expiry")
			}
			_, expiresAt, ok := decodeEnvelope(raw)
			if ok && !expiresAt.IsZero() && time.Now().After(expiresAt) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("row never reached its logical expiry")
			}
			time.Sleep(5 * time.Millisecond)
		}

		if swapped, err := store.CompareAndSwap(ctx, "k", []byte("stale"), []byte("new")); err != nil || swapped {
			t.Errorf("swap of an expired row against its stale value = (%v, %v), want (false, nil)", swapped, err)
		}
		swapped, err := store.CompareAndSwap(ctx, "k", nil, []byte("fresh"))
		if err != nil || !swapped {
			t.Fatalf("swap of an expired row against the empty old = (%v, %v), want (true, nil)", swapped, err)
		}
		payload, expiresAt := decodeRow(t, srv, "k")
		if string(payload) != "fresh" || !expiresAt.IsZero() {
			t.Errorf("expired-row swap wrote payload %q with expiry %v, want \"fresh\" with no expiry", payload, expiresAt)
		}
	})
}

// TestKVStore_ErrorPaths_OverADeadClient pins that an unreachable server
// surfaces every operation as a wrapped pkgcore/kv/memcached error rather
// than panicking or silently succeeding. The increments' and compare-and-
// swap's read half fails first, so those two surface the read's own wrap
// ("get:") -- the code path that would retry a transient failure instead
// reports the non-transient connection error.
func TestKVStore_ErrorPaths_OverADeadClient(t *testing.T) {
	store := NewKVStore(memcache.New("127.0.0.1:1"))
	ctx := context.Background()

	if _, _, err := store.Get(ctx, "k"); err == nil || !strings.Contains(err.Error(), "pkgcore/kv/memcached: get:") {
		t.Errorf("Get() error = %v, want a wrapped error naming the get", err)
	}
	if err := store.Set(ctx, "k", []byte("v"), 0); err == nil || !strings.Contains(err.Error(), "pkgcore/kv/memcached: set:") {
		t.Errorf("Set() error = %v, want a wrapped error naming the set", err)
	}
	if err := store.Delete(ctx, "k"); err == nil || !strings.Contains(err.Error(), "pkgcore/kv/memcached: delete:") {
		t.Errorf("Delete() error = %v, want a wrapped error naming the delete", err)
	}
	for name, op := range map[string]func() error{
		"IncrByFloat": func() error {
			_, err := store.IncrByFloat(ctx, "k", 1)
			return err
		},
		"IncrByFloatWithTTL": func() error {
			_, err := store.IncrByFloatWithTTL(ctx, "k", 1, time.Hour)
			return err
		},
		"CompareAndSwap": func() error {
			_, err := store.CompareAndSwap(ctx, "k", nil, []byte("v"))
			return err
		},
	} {
		if err := op(); err == nil || !strings.Contains(err.Error(), "pkgcore/kv/memcached:") {
			t.Errorf("%s() error = %v, want a wrapped pkgcore/kv/memcached error", name, err)
		}
	}
}

// TestKVStore_ConcurrentIncrements_OverFakeServer is the concurrency proof
// this store's compare-and-swap loops exist for: many goroutines incrementing
// one key through the fake server lose real cas races against each other,
// and the loops' retries must converge on exactly the sum of every increment
// -- no lost update, no double count.
func TestKVStore_ConcurrentIncrements_OverFakeServer(t *testing.T) {
	srv := startFakeMemcachedServer(t)
	store := NewKVStore(memcache.New(srv.addr()))
	ctx := context.Background()

	const goroutines = 8
	const incrementsPerGoroutine = 20
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

	payload, _ := decodeRow(t, srv, "counter")
	const want = goroutines * incrementsPerGoroutine
	if string(payload) != strconv.Itoa(want) {
		t.Errorf("final counter = %q, want %d: the cas loop must lose no update", payload, want)
	}
}

// TestKVStore_CompareAndSwap_SetIfAbsent_SingleWinner pins the set-if-absent
// exclusivity under real concurrency: concurrent creators of one key agree
// on exactly one winner.
func TestKVStore_CompareAndSwap_SetIfAbsent_SingleWinner(t *testing.T) {
	srv := startFakeMemcachedServer(t)
	store := NewKVStore(memcache.New(srv.addr()))
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
	payload, _ := decodeRow(t, srv, "lock")
	if string(payload) != "holder" {
		t.Errorf("final value = %q, want \"holder\"", payload)
	}
}

// TestKVStore_IncrByFloat_LostCASRaceIsRetriedOverFakeServer pins the
// retry loop's response to a genuinely lost cas race, deterministically: the
// server answers the first conditional write with EXISTS (another writer
// landed between this call's read and its write), and the loop must retry
// from a fresh read and land the increment -- never report an error, never
// double-apply the delta.
func TestKVStore_IncrByFloat_LostCASRaceIsRetriedOverFakeServer(t *testing.T) {
	srv := startFakeMemcachedServer(t)
	store := NewKVStore(memcache.New(srv.addr()))
	ctx := context.Background()

	if err := store.Set(ctx, "n", []byte("10"), 0); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}
	srv.scriptCASConflict()

	result, err := store.IncrByFloat(ctx, "n", 5)
	if err != nil {
		t.Fatalf("IncrByFloat with one lost cas race error = %v, want nil (the loop retries)", err)
	}
	if result != 15 {
		t.Fatalf("IncrByFloat with one lost cas race = %v, want 15", result)
	}
	payload, _ := decodeRow(t, srv, "n")
	if string(payload) != "15" {
		t.Errorf("final payload = %q, want \"15\": a lost race must never double-apply the delta", payload)
	}
}

// TestKVStore_IncrByFloat_TransientReadFailureIsRetriedOverFakeServer pins
// the transient-failure arm of the retry loop: a read whose connection the
// server closed under it (io.EOF) is retried from a fresh read rather than
// reported, and the increment still lands.
func TestKVStore_IncrByFloat_TransientReadFailureIsRetriedOverFakeServer(t *testing.T) {
	srv := startFakeMemcachedServer(t)
	store := NewKVStore(memcache.New(srv.addr()))
	ctx := context.Background()

	srv.scriptDroppedRead()

	result, err := store.IncrByFloat(ctx, "n", 3)
	if err != nil {
		t.Fatalf("IncrByFloat with one dropped read error = %v, want nil (the loop retries transient read failures)", err)
	}
	if result != 3 {
		t.Fatalf("IncrByFloat with one dropped read = %v, want 3", result)
	}
}

// TestKVStore_IncrByFloat_ServerFailureSurfacedWrappedOverFakeServer pins
// the non-transient failure arm: an add the server refuses outright (not a
// lost race) is reported as a wrapped error, never silently retried into an
// endless loop.
func TestKVStore_IncrByFloat_ServerFailureSurfacedWrappedOverFakeServer(t *testing.T) {
	srv := startFakeMemcachedServer(t)
	store := NewKVStore(memcache.New(srv.addr()))
	ctx := context.Background()

	srv.scriptAddServerError()

	if _, err := store.IncrByFloat(ctx, "n", 3); err == nil || !strings.Contains(err.Error(), "pkgcore/kv/memcached: incr:") {
		t.Fatalf("IncrByFloat() error = %v, want a wrapped error naming the incr", err)
	}
}

// TestKVStore_IncrByFloatWithTTL_LostRaceIsRetriedOverFakeServer pins the
// ttl-attaching sibling's retry: one lost cas race is retried, and the live
// key's own expiry survives the retried write.
func TestKVStore_IncrByFloatWithTTL_LostRaceIsRetriedOverFakeServer(t *testing.T) {
	srv := startFakeMemcachedServer(t)
	store := NewKVStore(memcache.New(srv.addr()))
	ctx := context.Background()

	if err := store.Set(ctx, "n", []byte("10"), 30*time.Minute); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}
	srv.scriptCASConflict()

	result, err := store.IncrByFloatWithTTL(ctx, "n", 5, 30*time.Minute)
	if err != nil {
		t.Fatalf("IncrByFloatWithTTL with one lost cas race error = %v, want nil", err)
	}
	if result != 15 {
		t.Fatalf("IncrByFloatWithTTL with one lost cas race = %v, want 15", result)
	}
	_, expiresAt := decodeRow(t, srv, "n")
	if expiresAt.IsZero() {
		t.Error("ttl-attaching increment lost the expiry: a live key's expiry must survive the retry")
	}
}

// TestKVStore_CompareAndSwap_LostRacesRetriedOverFakeServer pins the swap's
// retry arms deterministically: a set-if-absent whose add loses a race
// (NOT_STORED) retries and succeeds, and a matched swap whose cas loses a
// race (EXISTS) retries and succeeds -- each reported as a successful swap,
// never as an error.
func TestKVStore_CompareAndSwap_LostRacesRetriedOverFakeServer(t *testing.T) {
	ctx := context.Background()

	t.Run("set-if-absent add race", func(t *testing.T) {
		srv := startFakeMemcachedServer(t)
		store := NewKVStore(memcache.New(srv.addr()))
		srv.scriptAddConflict()
		swapped, err := store.CompareAndSwap(ctx, "k", nil, []byte("new"))
		if err != nil || !swapped {
			t.Fatalf("set-if-absent with one lost add race = (%v, %v), want (true, nil)", swapped, err)
		}
		payload, _ := decodeRow(t, srv, "k")
		if string(payload) != "new" {
			t.Errorf("final payload = %q, want \"new\"", payload)
		}
	})

	t.Run("matched swap cas race", func(t *testing.T) {
		srv := startFakeMemcachedServer(t)
		store := NewKVStore(memcache.New(srv.addr()))
		if err := store.Set(ctx, "k", []byte("old"), 0); err != nil {
			t.Fatalf("Set() error = %v, want nil", err)
		}
		srv.scriptCASConflict()
		swapped, err := store.CompareAndSwap(ctx, "k", []byte("old"), []byte("new"))
		if err != nil || !swapped {
			t.Fatalf("matched swap with one lost cas race = (%v, %v), want (true, nil)", swapped, err)
		}
	})

	t.Run("absent-key add server failure is wrapped", func(t *testing.T) {
		srv := startFakeMemcachedServer(t)
		store := NewKVStore(memcache.New(srv.addr()))
		srv.scriptAddServerError()
		if _, err := store.CompareAndSwap(ctx, "k", nil, []byte("new")); err == nil || !strings.Contains(err.Error(), "pkgcore/kv/memcached: cas:") {
			t.Fatalf("CompareAndSwap() error = %v, want a wrapped error naming the cas", err)
		}
	})
}
