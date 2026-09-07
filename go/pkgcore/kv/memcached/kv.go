// Package memcached is a second distributed deployment mode implementation of
// pkgcore.KVStore, on Memcached, split out of go/pkgcore's own package for
// the identical dependency-isolation reason kv/redis was: a consumer that
// never wires a Memcached-backed store does not inherit
// github.com/bradfitz/gomemcache in its dependency graph (docs/internal/03-
// deployment-modes.md's implementation-registry section, and see this
// package's own AGENTS.md entry for the measured cost).
//
// # Read this before choosing Memcached over Redis
//
// This implementation is honestly declared MultiReplicaSafe (many processes
// sharing one Memcached deployment see the same state) but NOT
// SurvivesRestart: Memcached is a pure in-memory cache with no persistence
// mechanism of any kind. A restart of the Memcached process, a rolling
// upgrade of the fleet, or the server evicting entries under ordinary memory
// pressure (Memcached's LRU eviction is not a bug -- it is the cache doing
// exactly what it is designed to do) all silently drop everything this store
// holds, with no error surfaced to any caller. This is a materially
// different risk profile from kv/redis (AOF/RDB persistence) or a
// PostgreSQL- or NATS-JetStream-backed KVStore (both back onto a real
// filesystem): those backends can lose data across a restart only if
// misconfigured; this one loses it by design, on every restart, always.
// Do not point session state, feature-flag overrides, or anything else that
// must outlive a cache flush at this implementation -- it exists for the
// same reason Redis' own maxmemory-policy eviction exists: fast, disposable,
// rebuildable-from-source-of-truth state. If your workload needs the
// KVStore's data to survive a restart, wire kv/redis or a NATS-KV-backed
// implementation instead.
//
// # Protocol constraints this store cannot hide
//
// Memcached imposes constraints pkgcore.KVStore's own doc comment does not:
// a key longer than 250 bytes or containing whitespace/control characters is
// rejected by the server (gomemcache.ErrMalformedKey, wrapped and returned
// rather than silently truncated or hashed), and a stored value is capped by
// the server's configured item-size limit (1 MiB by default). Both are
// genuine Memcached protocol limits with no client-side workaround; a caller
// that needs neither constraint should choose a different backend.
//
// # Why TTL needs an envelope: Memcached's expiry clock only ticks in whole seconds
//
// Memcached's native per-key expiry (its "exptime" field in every storage
// command) is granular to the second: relative expirations under 30 days are
// "N seconds from now", not fractions of one, and go/pkgcore/kvstoretest's
// shared AssertConforms suite deliberately exercises TTLs measured in tens of
// milliseconds (the same suite kv/redis's millisecond-granular PEXPIRE passes
// unmodified) to keep the whole conformance run fast. Rounding a sub-second
// ttl up to Memcached's minimum expressible unit -- one second -- would make
// a key set with a 25ms ttl still physically present when AssertConforms
// checks back 125ms later, silently breaking the "a short ttl expires" half
// of the contract for no reason a caller chose.
//
// So this store keeps its own logical expiry, independent of Memcached's
// coarse physical one: every stored value is wrapped in a small envelope (an
// 8-byte absolute expiry timestamp, or all-zero for "never", followed by the
// caller's own bytes verbatim) and every read compares that timestamp
// against the local wall clock before trusting what Memcached handed back.
// Memcached's own physical exptime is still set on every write -- rounded up
// to at least one whole second, or to an absolute Unix timestamp once the
// remaining time exceeds Memcached's 30-day relative-time threshold -- purely
// as a backstop so a key nobody ever reads again still eventually leaves the
// server's memory, exactly as an ordinary cache entry should; it is never
// what decides whether a Get call reports the key present. A row Memcached
// has not yet physically evicted but whose logical envelope has expired is
// deleted eagerly on the read that discovers it and reported absent, so two
// callers never observe it flip from present to absent at different moments.
//
// One inherent boundary the envelope cannot lift: Memcached's own exptime
// field is a signed 32-bit Unix timestamp once it is used in absolute-time
// form, so a ttl reaching far enough into the future (roughly a decade-plus
// from any date past 2026) pushes Memcached's own backstop past the year
// 2038 wraparound point. This store clamps the physical exptime it sends to
// the largest expressible value in that case rather than sending a wrapped,
// nonsensical one, which only ever makes Memcached's own eventual physical
// cleanup fire sooner than the logical envelope would have required --
// harmless, since the envelope is authoritative for whether Get reports the
// key present, never Memcached's own clock.
package memcached

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/bradfitz/gomemcache/memcache"

	"github.com/vislake/speed/go/pkgcore"
)

const (
	// kvNoExpiry is the ttl argument boundary that stores a key without an
	// expiry, mirroring pkgcore's in-memory store's own convention.
	kvNoExpiry time.Duration = 0

	// kvEnvelopeHeaderSize is the width, in bytes, of the big-endian absolute
	// expiry timestamp (Unix nanoseconds, zero meaning "never") that this
	// store prepends to every value it writes -- see the package doc
	// comment's "why TTL needs an envelope" section.
	kvEnvelopeHeaderSize = 8

	// maxEnvelopeValueLen is the largest payload encodeEnvelope accepts: the
	// envelope buffer's size is kvEnvelopeHeaderSize plus the payload,
	// computed in the platform's int, and an addition that wraps would hand
	// make a negative size to panic on. Values this long can only exist on a
	// 32-bit platform -- a 64-bit one cannot allocate a slice within a few
	// bytes of MaxInt -- and no real backend stores them either, Memcached's
	// own item-size cap being far lower, but the size arithmetic must be
	// unable to wrap on any platform: an overflowing payload is refused as an
	// error rather than encoded (CodeQL go/allocation-size-overflow).
	maxEnvelopeValueLen = math.MaxInt - kvEnvelopeHeaderSize

	// kvFloatFormat, kvFloatPrecisionShortest and kvFloatBitSize mirror
	// pkgcore's own in-memory store's numeric encoding exactly: 'g' with
	// shortest-round-trip precision at 64 bits, so a value IncrByFloat
	// writes here parses back to the identical float64 with
	// strconv.ParseFloat, the same promise every KVStore implementation
	// makes.
	kvFloatFormat            = 'g'
	kvFloatPrecisionShortest = -1
	kvFloatBitSize           = 64

	// memcachedMaxRelativeExpiry is the real Memcached wire-protocol
	// threshold (30 days, in seconds) at and below which the exptime field
	// of a storage command is a relative "seconds from now" offset; above
	// it, the server instead interprets the same field as an absolute Unix
	// timestamp. Getting this wrong in either direction silently changes
	// what a caller's ttl means to the server, so it is verified against the
	// protocol rather than assumed.
	memcachedMaxRelativeExpiry = 60 * 60 * 24 * 30

	// kvMaxCASAttempts bounds the compare-and-swap retry loops IncrByFloat
	// and CompareAndSwap run against Memcached's native gets/cas primitives
	// (Memcached has no atomic float increment or byte-value compare at the
	// protocol level -- see the package doc comment). The bound exists so
	// pathological contention fails loudly with errCASAttemptsExhausted
	// rather than spinning forever; the adversarial concurrency tests in
	// this package's integration tier size real contention well under it.
	kvMaxCASAttempts = 200

	// kvCASBackoffBase and kvCASBackoffCeiling shape the exponential backoff
	// between retry attempts under contention: short enough that the common,
	// uncontended case pays nothing (attempt 0 never sleeps), capped low
	// enough that even kvMaxCASAttempts consecutive collisions resolve in
	// well under a second.
	kvCASBackoffBase    = 200 * time.Microsecond
	kvCASBackoffCeiling = 5 * time.Millisecond
)

// errCASAttemptsExhausted is returned by IncrByFloat and CompareAndSwap when
// kvMaxCASAttempts consecutive compare-and-swap races were all lost -- the
// store's honest answer to genuinely pathological contention, rather than
// spinning forever or silently returning a stale result.
var errCASAttemptsExhausted = errors.New("pkgcore/kv/memcached: exceeded retry attempts under compare-and-swap contention")

// errCorruptEnvelope reports a stored value too short to carry this store's
// own envelope header -- a row this store never wrote (a foreign write into
// the same key, or a value written by an incompatible earlier version of
// this package). It is wrapped rather than panicking, since a caller sharing
// a Memcached deployment's keyspace with another writer is a wiring choice
// this store cannot rule out on its own.
var errCorruptEnvelope = errors.New("pkgcore/kv/memcached: stored value is too short to be one of this store's envelopes")

// kvStore is the distributed deployment mode's Memcached-backed KVStore. A
// distributed-mode host passes one client for the whole deployment; nothing
// in the store constructs, closes or otherwise owns the client.
type kvStore struct {
	client *memcache.Client
}

// NewKVStore returns a pkgcore.KVStore backed by the given Memcached client,
// a second distributed deployment mode implementation of the seam
// pkgcore.NewMemoryKVStore covers in standalone mode and kv/redis already
// covers with a persistent backend. Read the package doc comment before
// choosing this over kv/redis: Memcached has no persistence of any kind, so
// this implementation is honestly NOT SurvivesRestart.
//
// The store preserves the same observable KVStore semantics kv/redis and the
// in-memory store do -- Set replaces both the value and any expiry,
// IncrByFloat keeps a live key's expiry and starts a missing key at zero with
// no expiry, IncrByFloatWithTTL does the same but atomically attaches an
// expiry on the call that creates the key, and CompareAndSwap compares the
// whole value and never changes the key's expiry -- by keeping its own
// logical expiry envelope on top of
// Memcached's coarser, whole-second native one (see the package doc
// comment's "why TTL needs an envelope" section).
//
// A nil client panics: it is an unrecoverable wiring error at startup, and
// every operation would fail identically at first use.
func NewKVStore(client *memcache.Client) pkgcore.KVStore {
	if client == nil {
		panic("pkgcore/kv/memcached: NewKVStore requires a non-nil *memcache.Client")
	}
	return &kvStore{client: client}
}

// Get implements pkgcore.KVStore.Get.
func (s *kvStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	payload, _, _, found, err := s.readLive(key)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	// Copy, so that a caller mutating the result cannot reach into whatever
	// buffer decoding the envelope handed back.
	return bytes.Clone(payload), true, nil
}

// Set implements pkgcore.KVStore.Set.
func (s *kvStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	expiresAt := expiryFromTTL(ttl)

	encoded, err := encodeEnvelope(value, expiresAt)
	if err != nil {
		return fmt.Errorf("pkgcore/kv/memcached: set: %w", err)
	}

	item := &memcache.Item{
		Key:        key,
		Value:      encoded,
		Expiration: physicalExptime(expiresAt),
	}
	if err := s.client.Set(item); err != nil {
		return fmt.Errorf("pkgcore/kv/memcached: set: %w", err)
	}
	return nil
}

// Delete implements pkgcore.KVStore.Delete. Deleting a key the server does
// not hold is not an error, mirroring every other KVStore implementation:
// gomemcache reports that case as memcache.ErrCacheMiss, which this method
// swallows.
func (s *kvStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := s.client.Delete(key); err != nil && !errors.Is(err, memcache.ErrCacheMiss) {
		return fmt.Errorf("pkgcore/kv/memcached: delete: %w", err)
	}
	return nil
}

// IncrByFloat implements pkgcore.KVStore.IncrByFloat. Memcached's native
// incr/decr are unsigned 64-bit integer only -- there is no float support at
// the protocol level at all -- so this is implemented as a compare-and-swap
// retry loop over the value's canonical decimal encoding: read the current
// value and its CAS token together (Memcached's "gets" command, which
// gomemcache's Get always issues), parse it, add delta, and write the result
// back conditioned on that same token via Memcached's native "cas" (an
// existing key) or "add" (a missing or logically expired one). A lost race
// -- someone else's write landed between this call's read and its write --
// is not an error and is retried from a fresh read, bounded by
// kvMaxCASAttempts.
func (s *kvStore) IncrByFloat(ctx context.Context, key string, delta float64) (float64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	for attempt := 0; attempt < kvMaxCASAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if attempt > 0 {
			time.Sleep(casBackoff(attempt))
		}

		payload, expiresAt, casID, found, err := s.readLive(key)
		if err != nil {
			return 0, err
		}

		// A missing or expired key starts from zero with no expiry; a live
		// one contributes both its number and its expiry -- the exact
		// contract pkgcore.KVStore.IncrByFloat documents, and the one
		// pkgcore's in-memory store and kv/redis both honour.
		var current float64
		if found {
			parsed, perr := strconv.ParseFloat(string(payload), kvFloatBitSize)
			if perr != nil {
				// The stored value is not a number: fail now, without
				// retrying and without touching the key, exactly like
				// every other KVStore implementation. Returned bare, like
				// pkgcore's own store, so the offending value never
				// reaches an error message.
				return 0, pkgcore.ErrNotNumeric
			}
			current = parsed
		}

		result := current + delta
		newValue := strconv.AppendFloat(nil, result, kvFloatFormat, kvFloatPrecisionShortest, kvFloatBitSize)

		if !found {
			encoded, encodeErr := encodeEnvelope(newValue, time.Time{})
			if encodeErr != nil {
				return 0, fmt.Errorf("pkgcore/kv/memcached: incr: %w", encodeErr)
			}
			item := &memcache.Item{
				Key:   key,
				Value: encoded,
			}
			err = s.client.Add(item)
			if isLostCASRace(err) {
				continue
			}
			if err != nil {
				return 0, fmt.Errorf("pkgcore/kv/memcached: incr: %w", err)
			}
			return result, nil
		}

		encoded, err := encodeEnvelope(newValue, expiresAt)
		if err != nil {
			return 0, fmt.Errorf("pkgcore/kv/memcached: incr: %w", err)
		}

		item := &memcache.Item{
			Key:        key,
			Value:      encoded,
			Expiration: physicalExptime(expiresAt),
			CasID:      casID,
		}
		err = s.client.CompareAndSwap(item)
		if isLostCASRace(err) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("pkgcore/kv/memcached: incr: %w", err)
		}
		return result, nil
	}
	return 0, fmt.Errorf("pkgcore/kv/memcached: incr: %w", errCASAttemptsExhausted)
}

// IncrByFloatWithTTL implements pkgcore.KVStore.IncrByFloatWithTTL. It is the
// identical compare-and-swap retry loop IncrByFloat runs, with one
// difference: when readLive reports the key absent (found is false --
// readLive already collapses "genuinely never set" and "logically expired"
// into that one signal, unlike this store's own envelope-decoding internals
// for other operations), the new envelope's expiry is computed from ttl
// instead of always being time.Time{} ("never"). A live key's own expiresAt,
// read straight from its envelope, is carried through unchanged on every
// retry -- ttl is never consulted for it, matching IncrByFloat's own
// non-extension rule.
func (s *kvStore) IncrByFloatWithTTL(ctx context.Context, key string, delta float64, ttl time.Duration) (float64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	for attempt := 0; attempt < kvMaxCASAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if attempt > 0 {
			time.Sleep(casBackoff(attempt))
		}

		payload, expiresAt, casID, found, err := s.readLive(key)
		if err != nil {
			return 0, err
		}

		var current float64
		if found {
			parsed, perr := strconv.ParseFloat(string(payload), kvFloatBitSize)
			if perr != nil {
				return 0, pkgcore.ErrNotNumeric
			}
			current = parsed
		} else {
			// Only a fresh (missing or logically expired) key ever gets ttl
			// attached; a live key's own expiresAt above is used untouched.
			expiresAt = expiryFromTTL(ttl)
		}

		result := current + delta
		newValue := strconv.AppendFloat(nil, result, kvFloatFormat, kvFloatPrecisionShortest, kvFloatBitSize)

		if !found {
			encoded, encodeErr := encodeEnvelope(newValue, expiresAt)
			if encodeErr != nil {
				return 0, fmt.Errorf("pkgcore/kv/memcached: incr with ttl: %w", encodeErr)
			}
			item := &memcache.Item{
				Key:        key,
				Value:      encoded,
				Expiration: physicalExptime(expiresAt),
			}
			err = s.client.Add(item)
			if isLostCASRace(err) {
				continue
			}
			if err != nil {
				return 0, fmt.Errorf("pkgcore/kv/memcached: incr with ttl: %w", err)
			}
			return result, nil
		}

		encoded, err := encodeEnvelope(newValue, expiresAt)
		if err != nil {
			return 0, fmt.Errorf("pkgcore/kv/memcached: incr with ttl: %w", err)
		}

		item := &memcache.Item{
			Key:        key,
			Value:      encoded,
			Expiration: physicalExptime(expiresAt),
			CasID:      casID,
		}
		err = s.client.CompareAndSwap(item)
		if isLostCASRace(err) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("pkgcore/kv/memcached: incr with ttl: %w", err)
		}
		return result, nil
	}
	return 0, fmt.Errorf("pkgcore/kv/memcached: incr with ttl: %w", errCASAttemptsExhausted)
}

// CompareAndSwap implements pkgcore.KVStore.CompareAndSwap. A missing or
// logically expired key is treated identically: it matches only an empty
// old, which is written through Memcached's native "add" (store only if
// absent); an existing key is compared against the value this call's own
// "gets" read, and a match is written back through Memcached's native "cas"
// conditioned on that read's token, which never touches the key's expiry. A
// mismatch is reported false immediately, without attempting a write; a lost
// race against another writer -- the row changed shape between this call's
// read and its conditional write -- is not an error and is retried from a
// fresh read, bounded by kvMaxCASAttempts.
func (s *kvStore) CompareAndSwap(ctx context.Context, key string, old, newVal []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	for attempt := 0; attempt < kvMaxCASAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if attempt > 0 {
			time.Sleep(casBackoff(attempt))
		}

		payload, expiresAt, casID, found, err := s.readLive(key)
		if err != nil {
			return false, err
		}

		if !found {
			if len(old) != 0 {
				// An absent key matches only the empty expectation.
				return false, nil
			}
			encoded, encodeErr := encodeEnvelope(newVal, time.Time{})
			if encodeErr != nil {
				return false, fmt.Errorf("pkgcore/kv/memcached: cas: %w", encodeErr)
			}
			item := &memcache.Item{
				Key:   key,
				Value: encoded,
			}
			err = s.client.Add(item)
			if isLostCASRace(err) {
				continue
			}
			if err != nil {
				return false, fmt.Errorf("pkgcore/kv/memcached: cas: %w", err)
			}
			return true, nil
		}

		if !bytes.Equal(payload, old) {
			// A mismatch on a present key is an expected outcome, reported
			// immediately without attempting a write.
			return false, nil
		}

		encoded, err := encodeEnvelope(newVal, expiresAt)
		if err != nil {
			return false, fmt.Errorf("pkgcore/kv/memcached: cas: %w", err)
		}

		item := &memcache.Item{
			Key:        key,
			Value:      encoded,
			Expiration: physicalExptime(expiresAt),
			CasID:      casID,
		}
		err = s.client.CompareAndSwap(item)
		if isLostCASRace(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("pkgcore/kv/memcached: cas: %w", err)
		}
		return true, nil
	}
	return false, fmt.Errorf("pkgcore/kv/memcached: cas: %w", errCASAttemptsExhausted)
}

// readLive fetches key through Memcached's native "gets" (gomemcache's Get
// always issues gets, populating the CAS token), decodes this store's own
// envelope, and applies the logical expiry check the package doc comment's
// "why TTL needs an envelope" section describes on top of Memcached's own
// coarser, whole-second native TTL. A physically present but logically
// expired row is deleted eagerly -- best-effort, since a concurrent reader
// or Memcached's own eviction may already have removed it -- and reported
// absent, exactly like every other KVStore implementation treats an expired
// key.
func (s *kvStore) readLive(key string) (payload []byte, expiresAt time.Time, casID uint64, found bool, err error) {
	item, err := s.client.Get(key)
	if errors.Is(err, memcache.ErrCacheMiss) {
		return nil, time.Time{}, 0, false, nil
	}
	if err != nil {
		return nil, time.Time{}, 0, false, fmt.Errorf("pkgcore/kv/memcached: get: %w", err)
	}

	payload, expiresAt, ok := decodeEnvelope(item.Value)
	if !ok {
		return nil, time.Time{}, 0, false, fmt.Errorf("pkgcore/kv/memcached: get: %w", errCorruptEnvelope)
	}
	if !expiresAt.IsZero() && !time.Now().Before(expiresAt) {
		_ = s.client.Delete(key)
		return nil, time.Time{}, 0, false, nil
	}
	return payload, expiresAt, item.CasID, true, nil
}

// isLostCASRace reports whether err is one of the answers Memcached's own
// "add"/"cas" commands give when another writer's operation landed between
// this call's read and its conditional write -- an expected outcome under
// contention, not a failure: ErrCASConflict (the value changed since this
// call's gets), ErrNotStored (the row was evicted or already exists,
// depending on which verb produced it) and ErrCacheMiss (the row disappeared
// entirely). Every one of them means the caller should retry from a fresh
// read rather than treat the operation as having failed.
func isLostCASRace(err error) bool {
	return errors.Is(err, memcache.ErrCASConflict) ||
		errors.Is(err, memcache.ErrNotStored) ||
		errors.Is(err, memcache.ErrCacheMiss)
}

// casBackoff returns the delay before retry attempt (1-indexed against the
// loops above, which never sleep before attempt 0) sleeps: an exponential
// ramp from kvCASBackoffBase, capped at kvCASBackoffCeiling so a long run of
// collisions still resolves quickly.
func casBackoff(attempt int) time.Duration {
	shift := attempt
	if shift > 8 {
		shift = 8
	}
	d := kvCASBackoffBase * time.Duration(1<<shift)
	if d > kvCASBackoffCeiling {
		return kvCASBackoffCeiling
	}
	return d
}

// expiryFromTTL turns a KVStore ttl argument into this store's own absolute
// logical expiry: the zero time.Time for "never", matching pkgcore's in-
// memory store's own kvNoExpiry convention.
func expiryFromTTL(ttl time.Duration) time.Time {
	if ttl <= kvNoExpiry {
		return time.Time{}
	}
	return time.Now().Add(ttl)
}

// encodeEnvelope prepends value with its kvEnvelopeHeaderSize-byte absolute
// expiry timestamp (Unix nanoseconds, big-endian, zero meaning "never"),
// verbatim -- value's own bytes, embedded NUL and all, are never
// reinterpreted. A payload too long to fit behind the header inside the
// platform int the allocation size is computed in (see maxEnvelopeValueLen)
// is refused with an error, never encoded through size arithmetic that
// would wrap.
func encodeEnvelope(value []byte, expiresAt time.Time) ([]byte, error) {
	if len(value) > maxEnvelopeValueLen {
		return nil, fmt.Errorf("value of %d bytes is too large to store: adding the %d-byte expiry header would overflow the size arithmetic",
			len(value), kvEnvelopeHeaderSize)
	}

	envelope := make([]byte, kvEnvelopeHeaderSize+len(value))
	var nanos uint64
	if !expiresAt.IsZero() {
		nanos = uint64(expiresAt.UnixNano())
	}
	binary.BigEndian.PutUint64(envelope[:kvEnvelopeHeaderSize], nanos)
	copy(envelope[kvEnvelopeHeaderSize:], value)
	return envelope, nil
}

// decodeEnvelope reverses encodeEnvelope. ok is false when stored is too
// short to carry a header this store could have written -- see
// errCorruptEnvelope.
func decodeEnvelope(stored []byte) (value []byte, expiresAt time.Time, ok bool) {
	if len(stored) < kvEnvelopeHeaderSize {
		return nil, time.Time{}, false
	}
	nanos := binary.BigEndian.Uint64(stored[:kvEnvelopeHeaderSize])
	if nanos != 0 {
		expiresAt = time.Unix(0, int64(nanos)) //nolint:gosec // G115: nanos only ever comes back from a uint64(int64) cast encodeEnvelope itself made on a non-negative UnixNano value (any real wall-clock time is positive), so the reverse conversion here never wraps
	}
	return stored[kvEnvelopeHeaderSize:], expiresAt, true
}

// physicalExptime derives the Memcached wire-protocol exptime field for
// expiresAt -- 0 ("never") for the zero time.Time, otherwise the number of
// whole seconds remaining, rounded up so the physical row never disappears
// before the logical envelope says it should. Once that remaining duration
// exceeds memcachedMaxRelativeExpiry, the server would reinterpret a plain
// seconds count as an absolute Unix timestamp instead, so this switches to
// sending expiresAt's own Unix time explicitly; see the package doc
// comment's note on the resulting year-2038 boundary for a ttl reaching far
// enough into the future.
func physicalExptime(expiresAt time.Time) int32 {
	if expiresAt.IsZero() {
		return 0
	}

	remainingSeconds := math.Ceil(time.Until(expiresAt).Seconds())
	if remainingSeconds < 1 {
		remainingSeconds = 1
	}
	if remainingSeconds <= memcachedMaxRelativeExpiry {
		return int32(remainingSeconds)
	}

	unix := expiresAt.Unix()
	if unix > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(unix) //nolint:gosec // G115: guarded by the unix > math.MaxInt32 check immediately above -- this line only ever runs on a value already proven to fit
}
