package pkgcore

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"sync"
	"time"
)

// ErrNotNumeric is returned by IncrByFloat when the key already holds a value
// that is not a valid float64 encoding. The stored value is left untouched.
// The offending value is deliberately kept out of the error text, because a
// KVStore may hold sensitive data.
var ErrNotNumeric = errors.New("pkgcore: value is not a valid float")

const (
	// kvNoExpiry is the boundary for the ttl argument of Set: any ttl at or below
	// it stores the key without an expiry.
	kvNoExpiry time.Duration = 0

	// kvFloatFormat is the strconv verb used to encode numeric values. 'g' keeps
	// small numbers readable and falls back to exponent notation only when the
	// decimal form would be unwieldy.
	kvFloatFormat = 'g'

	// kvFloatPrecisionShortest asks strconv for the shortest representation that
	// parses back to the exact same float64, so repeated increments never lose
	// precision to formatting.
	kvFloatPrecisionShortest = -1

	// kvFloatBitSize is the width of the numeric values IncrByFloat handles.
	kvFloatBitSize = 64
)

// KVStore is the key-value contract shared by every runtime profile: an
// in-memory map in the demo profile, Redis or an equivalent server in
// production.
//
// The interface is deliberately designed against the weakest backend it must
// support, so it exposes no server-side scripting, pipelines, pub/sub or data
// types beyond opaque byte values. Atomicity is expressed through IncrByFloat
// and CompareAndSwap, which every backend can honour; callers that need a
// read-modify-write cycle must build it from those two rather than from a Get
// followed by a Set.
//
// Keys are opaque strings and values are opaque byte slices; the store never
// interprets either, except for the numeric encoding IncrByFloat reads and
// writes. Implementations must be safe for concurrent use by multiple
// goroutines, must not retain or hand out the caller's slices, and must honour
// a cancelled context by returning its error instead of performing the
// operation.
type KVStore interface {
	// Get returns the value stored under key. The boolean reports whether the
	// key was present; a key whose expiry has passed counts as absent. A
	// missing key is not an error, so the returned error is non-nil only when
	// the store itself failed.
	Get(ctx context.Context, key string) ([]byte, bool, error)

	// Set stores value under key, replacing any existing value and expiry. A
	// ttl greater than zero expires the key after that duration; a ttl of zero
	// or less stores the key without an expiry.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error

	// Delete removes key. Deleting a key that does not exist is not an error.
	Delete(ctx context.Context, key string) error

	// IncrByFloat adds delta to the number stored under key and returns the
	// result. A missing or expired key starts from zero and is stored without
	// an expiry; a key that already exists keeps the expiry it has, so counters
	// under a rolling window are not extended by being incremented. The value
	// is stored as its shortest exact decimal encoding, which callers should
	// parse with strconv.ParseFloat rather than compare as text. A key holding
	// a non-numeric value fails with ErrNotNumeric and is left unchanged.
	IncrByFloat(ctx context.Context, key string, delta float64) (float64, error)

	// CompareAndSwap replaces the value under key with newVal only if the
	// current value equals old, and reports whether the swap happened. A
	// missing or expired key matches an old of nil or zero length, which makes
	// CompareAndSwap usable as set-if-absent. A mismatch is an expected outcome
	// and returns false with a nil error. The swap never changes the key's
	// expiry.
	CompareAndSwap(ctx context.Context, key string, old, newVal []byte) (bool, error)
}

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

// memoryKVStore is the demo-profile KVStore: a mutex-guarded map that lives and
// dies with the process. Expired entries are dropped lazily, when an operation
// next touches the key.
type memoryKVStore struct {
	mu      sync.Mutex
	entries map[string]kvEntry
}

// NewMemoryKVStore returns a KVStore backed by an in-memory map, with no
// external dependencies. It is the demo-profile implementation, and doubles as
// a test double for unit tests of code written against KVStore. Nothing it
// holds survives the process, and nothing is shared between two stores.
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
		return true, nil
	}

	if !bytes.Equal(entry.value, old) {
		return false, nil
	}

	// entry.expiresAt is carried over untouched: a swap is not a refresh.
	entry.value = bytes.Clone(newVal)
	s.entries[key] = entry
	return true, nil
}
