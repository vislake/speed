// Package redis is the distributed deployment mode's KVStore, backed by a
// shared Redis server every replica of a deployment reads and writes. It is
// split out of go/pkgcore's own package -- rather than living beside the
// KVStore interface as pkgcore.redisKVStore once did -- so that a consumer
// which never wires a Redis-backed store does not inherit go-redis in its
// dependency graph: Go resolves dependencies per package, and an interface
// package that also carries one implementation hands every importer that
// implementation's whole dependency closure (docs/internal/03-deployment-
// modes.md's implementation-registry section measures the cost and names the boundary).
//
// Importing this package registers "kv.redis" on pkgcore's shared
// KVStoreRegistry as a side effect (see register.go), the name
// pkgcore.PresetDistributed already names for the "kv" seam -- the same
// database/sql-style driver-registration pattern pkgcore's other built-in
// implementations use, now applied across a package boundary. A
// distributed-mode host that wants it either blank-imports this package
// (`import _ ".../pkgcore/kv/redis"`) so WithPreset(PresetDistributed)
// resolves it, or calls NewKVStore directly and wires it with
// pkgcore.WithKVStore.
package redis

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/pkgcore"
)

// kvNoExpiry is the ttl value that stores a key with no expiry -- go-redis's
// own "no TTL option" sentinel, mirroring pkgcore's in-memory store's
// zero-or-negative-ttl convention.
const kvNoExpiry = 0

// casScript atomically implements KVStore.CompareAndSwap against Redis: it
// reads the current value and its remaining time-to-live, compares, and only
// on a match replaces the value, restoring the TTL so that a swap is never a
// refresh -- the exact semantics of pkgcore's in-memory store. A missing or
// expired key (GET returns false) matches only the empty expectation, which
// turns the call into set-if-absent.
//
// The comparison and both writes happen inside one script, so no other client
// can observe or interleave a change between them. This is the one place the
// store reaches for server-side scripting; the KVStore interface itself still
// exposes nothing beyond the plain operations, so callers are never tied to
// it.
//
// One boundary the script accepts: PTTL answers in whole milliseconds, so a
// live key with less than a millisecond of life left reports 0, and the swap
// stores the new value with no expiry -- where the in-memory store's
// expiresAt would let it die a fraction of a millisecond later. The race is
// inherent to the round trip and cannot be closed from the caller's side, so
// the divergence is documented instead of "fixed": it only ever lands on a
// key that was expiring within the same instant the swap arrived, and a
// caller that depends on a swap not outliving its key cannot do so reliably
// against any backend.
var casScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
local matched = false
if current == false then
	if ARGV[1] == '' then
		matched = true
	end
elseif current == ARGV[1] then
	matched = true
end
if not matched then
	return 0
end
local ttl = redis.call('PTTL', KEYS[1])
if ttl < 0 then
	ttl = 0
end
redis.call('SET', KEYS[1], ARGV[2])
if ttl > 0 then
	redis.call('PEXPIRE', KEYS[1], ttl)
end
return 1
`)

// kvStore is the distributed deployment mode's KVStore: the shared in-memory
// server every replica of a deployment reads and writes. A distributed-mode
// host passes one client for the whole deployment; nothing in the store
// constructs, closes or otherwise owns the client.
type kvStore struct {
	client *redis.Client
}

// NewKVStore returns a pkgcore.KVStore backed by the given Redis client, the
// distributed deployment mode's implementation of the seam
// pkgcore.NewMemoryKVStore covers in standalone mode. The store runs its
// operations directly on the client, which the caller keeps owning: it stays
// open when the store is garbage-collected, and the store never dials, pings
// or closes it.
//
// The store preserves the in-memory store's semantics, including the ones a
// shared server has to implement explicitly: Set replaces both the value and
// any expiry, IncrByFloat keeps a live key's expiry and starts a missing key
// at zero with no expiry, and CompareAndSwap compares the whole value and
// never changes the key's expiry. The server is authoritative for a few
// boundary details, which callers cannot depend on either way:
//
//   - Expiry is stored with millisecond granularity (Redis expires keys on a
//     millisecond clock), where the in-memory store keeps nanoseconds.
//   - IncrByFloat stores the server's own decimal rendering of the result,
//     which is exact but not always the shortest form; as with the in-memory
//     store, callers must parse the value with strconv.ParseFloat rather than
//     compare it as text.
//   - IncrByFloat reports a key whose value is not a number by returning
//     pkgcore.ErrNotNumeric with the stored value untouched, exactly like the
//     in-memory store; the offending value never appears in the error.
//
// A nil client panics: it is an unrecoverable wiring error at startup, and
// every operation would fail identically at first use.
func NewKVStore(client *redis.Client) pkgcore.KVStore {
	if client == nil {
		panic("pkgcore/kv/redis: NewKVStore requires a non-nil *redis.Client")
	}
	return &kvStore{client: client}
}

// Get implements pkgcore.KVStore.Get. Redis treats a key whose expiry has
// passed as absent, and Get on an expired key reports redis.Nil, which the
// store maps to the interface's absent result; expiry is enforced by the
// server rather than by the store dropping the entry lazily.
func (s *kvStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	value, err := s.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("pkgcore/kv/redis: get: %w", err)
	}
	return []byte(value), true, nil
}

// Set implements pkgcore.KVStore.Set. A ttl above zero becomes the key's
// expiry; a ttl of zero or less stores the key without one, clearing
// whatever expiry the key had, because SET without a TTL option replaces the
// whole entry.
func (s *kvStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ttl > kvNoExpiry {
		if err := s.client.Set(ctx, key, value, ttl).Err(); err != nil {
			return fmt.Errorf("pkgcore/kv/redis: set: %w", err)
		}
		return nil
	}
	if err := s.client.Set(ctx, key, value, kvNoExpiry).Err(); err != nil {
		return fmt.Errorf("pkgcore/kv/redis: set: %w", err)
	}
	return nil
}

// Delete implements pkgcore.KVStore.Delete. Deleting a key the server does
// not hold is not an error, mirroring the in-memory store.
func (s *kvStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.client.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("pkgcore/kv/redis: delete: %w", err)
	}
	return nil
}

// IncrByFloat implements pkgcore.KVStore.IncrByFloat. INCRBYFLOAT is the
// server's own read-modify-write: it parses the stored value, adds the delta
// and stores the result in one step, so concurrent increments never lose an
// update. The server keeps a live key's expiry and creates a missing key
// without one, which matches the in-memory store exactly.
func (s *kvStore) IncrByFloat(ctx context.Context, key string, delta float64) (float64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	result, err := s.client.IncrByFloat(ctx, key, delta).Result()
	if err == nil {
		return result, nil
	}
	// The server rejects a non-numeric value ("value is not a valid float") or
	// a value of the wrong type ("WRONGTYPE ..."). Both mean the key is not a
	// number, so both map to the interface's pkgcore.ErrNotNumeric, returned
	// bare -- like the in-memory store, the offending value stays out of the
	// error text, and a wrapped error would add nothing the interface
	// promises.
	//
	// The detection is a substring match on the server's error wording,
	// because go-redis exposes no structured error code to key on. That makes
	// this a coupling to Redis's own copy: a server that rewords either
	// message makes the ErrNotNumeric errors.Is checks in every consumer
	// fail loudly -- the integration suite exercises this path against a real
	// server -- rather than silently misclassify, which is what keeps the
	// coupling acceptable.
	if strings.Contains(err.Error(), "value is not a valid float") || strings.Contains(err.Error(), "WRONGTYPE") {
		return 0, pkgcore.ErrNotNumeric
	}
	return 0, fmt.Errorf("pkgcore/kv/redis: incr: %w", err)
}

// CompareAndSwap implements pkgcore.KVStore.CompareAndSwap. The comparison
// and the conditional write run inside one Lua script (see casScript), so
// the whole compare-and-swap is atomic against every other client of the
// server. A mismatch or an absent key that does not match the empty
// expectation is an expected outcome and returns false with a nil error; the
// swap never changes the key's expiry.
func (s *kvStore) CompareAndSwap(ctx context.Context, key string, old, newVal []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	swapped, err := casScript.Run(ctx, s.client, []string{key}, old, newVal).Int64()
	if err != nil {
		return false, fmt.Errorf("pkgcore/kv/redis: cas: %w", err)
	}
	return swapped == 1, nil
}
