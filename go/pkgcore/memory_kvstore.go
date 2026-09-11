package pkgcore

import (
	"bytes"
	"context"
	"strconv"
	"sync"
	"time"
)

// kvEntry is one value held by memoryKVStore together with its optional
// expiry.
type kvEntry struct {
	value []byte

	// expiresAt is the instant the entry stops being visible. The zero time
	// means the entry never expires.
	expiresAt time.Time
}

// expired reports whether the entry has an expiry that has already passed at now.
func (e kvEntry) expired(now time.Time) bool {
	return !e.expiresAt.IsZero() && !now.Before(e.expiresAt)
}

// sweepExpired reclaims every entry whose expiry has already passed at now.
// The caller holds s.mu. Deleting during a map range is safe: Go allows
// removing the entry currently being visited, and every other removal this
// pass makes is of an entry the pass would have visited anyway.
func (s *memoryKVStore) sweepExpired(now time.Time) {
	for key, entry := range s.entries {
		if entry.expired(now) {
			delete(s.entries, key)
		}
	}
}

// noteWrite counts one write operation toward the amortized sweep and runs
// the sweep once the interval elapses, so reclamation follows the store's own
// write traffic rather than waiting for a specific key to be touched again.
// The caller holds s.mu.
func (s *memoryKVStore) noteWrite() {
	s.writesSinceSweep++
	if s.writesSinceSweep >= kvSweepInterval {
		s.writesSinceSweep = 0
		s.sweepExpired(time.Now())
	}
}

// memoryKVStore is the standalone deployment mode's KVStore: a mutex-guarded
// map that lives and dies with the process. Expiry reclamation has two
// halves, mirroring the expiry story of the PostgreSQL-backed store
// (kv/postgres's "invisible but not yet removed" reads plus its host-run
// Sweep): an expired entry is invisible to every operation, and a touched
// key's expired entry is dropped by the operation that touches it, while
// every kvSweepInterval-th write runs an amortized sweep that reclaims every
// expired entry in one pass -- so a key nothing ever touches again (a spent
// rate-limit window, an expired lock) does not occupy the map for the rest
// of the process. Reclamation is a hygiene matter, never a correctness one:
// correctness never depends on a sweep having run.
type memoryKVStore struct {
	mu      sync.Mutex
	entries map[string]kvEntry

	// writesSinceSweep counts the write operations since the last amortized
	// sweep; when it reaches kvSweepInterval the next write sweeps and
	// resets it.
	writesSinceSweep int
}

// NewMemoryKVStore returns a KVStore backed by an in-memory map, with no
// external dependencies. It is the standalone deployment mode's
// implementation, and doubles as a test double for unit tests of code written
// against KVStore. Nothing it holds survives the process, and nothing is
// shared between two stores.
func NewMemoryKVStore() KVStore {
	return &memoryKVStore{entries: make(map[string]kvEntry)}
}

// Get implements KVStore.Get and removes the entry if it has expired.
func (s *memoryKVStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entry, found := s.entries[key]
	if !found {
		return nil, false, nil
	}
	if entry.expired(time.Now()) {
		delete(s.entries, key)
		return nil, false, nil
	}
	// Copy, so that a caller mutating the result cannot reach into the store.
	return bytes.Clone(entry.value), true, nil
}

// Set implements KVStore.Set.
func (s *memoryKVStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	entry := kvEntry{value: bytes.Clone(value)}
	if ttl > kvNoExpiry {
		entry.expiresAt = time.Now().Add(ttl)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.entries[key] = entry
	s.noteWrite()
	return nil
}

// Delete implements KVStore.Delete.
func (s *memoryKVStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.entries, key)
	return nil
}

// IncrByFloat implements KVStore.IncrByFloat.
func (s *memoryKVStore) IncrByFloat(ctx context.Context, key string, delta float64) (float64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// A missing or expired key starts from zero with no expiry; a live one
	// contributes both its number and its expiry.
	var (
		current   float64
		expiresAt time.Time
	)
	if entry, found := s.entries[key]; found && !entry.expired(time.Now()) {
		parsed, err := strconv.ParseFloat(string(entry.value), kvFloatBitSize)
		if err != nil {
			return 0, ErrNotNumeric
		}
		current = parsed
		expiresAt = entry.expiresAt
	}

	result := current + delta
	s.entries[key] = kvEntry{
		value:     strconv.AppendFloat(nil, result, kvFloatFormat, kvFloatPrecisionShortest, kvFloatBitSize),
		expiresAt: expiresAt,
	}
	s.noteWrite()
	return result, nil
}

// IncrByFloatWithTTL implements KVStore.IncrByFloatWithTTL. It runs inside
// the same mutex critical section as IncrByFloat, so the whole
// read-current-value-then-decide-the-expiry-then-write sequence is one
// atomic step from any concurrent caller's point of view -- there is no
// Get-then-Set gap for another goroutine's increment to land in and be
// overwritten, which is the exact gap a caller-side IncrByFloat-then-Set
// sequence cannot close (see go/ratelimit's own doc comment on the race this
// primitive exists to close).
func (s *memoryKVStore) IncrByFloatWithTTL(ctx context.Context, key string, delta float64, ttl time.Duration) (float64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// A missing or expired key starts from zero; a live one contributes both
	// its number and its expiry, exactly like IncrByFloat.
	var (
		current   float64
		expiresAt time.Time
	)
	entry, found := s.entries[key]
	live := found && !entry.expired(time.Now())
	if live {
		parsed, err := strconv.ParseFloat(string(entry.value), kvFloatBitSize)
		if err != nil {
			return 0, ErrNotNumeric
		}
		current = parsed
		expiresAt = entry.expiresAt
	} else if ttl > kvNoExpiry {
		// Only a fresh (missing or expired) key ever gets ttl attached; a
		// live key's own expiry above is carried over untouched.
		expiresAt = time.Now().Add(ttl)
	}

	result := current + delta
	s.entries[key] = kvEntry{
		value:     strconv.AppendFloat(nil, result, kvFloatFormat, kvFloatPrecisionShortest, kvFloatBitSize),
		expiresAt: expiresAt,
	}
	s.noteWrite()
	return result, nil
}

// CompareAndSwap implements KVStore.CompareAndSwap.
func (s *memoryKVStore) CompareAndSwap(ctx context.Context, key string, old, newVal []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entry, found := s.entries[key]
	if found && entry.expired(time.Now()) {
		delete(s.entries, key)
		entry, found = kvEntry{}, false
	}

	if !found {
		// An absent key matches only the empty expectation, which turns
		// CompareAndSwap into set-if-absent.
		if len(old) != 0 {
			return false, nil
		}
		s.entries[key] = kvEntry{value: bytes.Clone(newVal)}
		s.noteWrite()
		return true, nil
	}

	if !bytes.Equal(entry.value, old) {
		return false, nil
	}

	// entry.expiresAt is carried over untouched: a swap is not a refresh.
	entry.value = bytes.Clone(newVal)
	s.entries[key] = entry
	s.noteWrite()
	return true, nil
}
