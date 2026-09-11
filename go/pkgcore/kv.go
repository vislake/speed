package pkgcore

import (
	"context"
	"errors"
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

	// kvSweepInterval is how many write operations the in-memory store lets
	// pass between amortized sweeps: every interval-th write reclaims every
	// expired entry in one pass. A rate-limit window key embeds its window,
	// so once a window passes nothing ever touches its key again -- without a
	// reclamation that runs on its own, such keys would occupy the map for
	// the rest of the process. One O(entries) pass per interval keeps the
	// amortized cost at O(1) per write while bounding the map to live
	// entries plus whatever expired during the last interval.
	kvSweepInterval = 1024
)

// KVStore is the key-value contract shared by every deployment mode: an
// in-memory map in the standalone deployment mode, Redis or an equivalent
// server in the distributed deployment mode.
//
// The interface is deliberately designed against the weakest backend it must
// support, so it exposes no server-side scripting, pipelines, pub/sub or data
// types beyond opaque byte values. Atomicity is expressed through IncrByFloat,
// IncrByFloatWithTTL and CompareAndSwap, which every backend can honour;
// callers that need a read-modify-write cycle must build it from those rather
// than from a Get followed by a Set.
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
	//
	// The arithmetic itself is not guaranteed bit-identical across
	// implementations for a delta not exactly representable in binary
	// float64 (0.1, for instance): some backends round after every
	// accumulation, others accumulate at a wider precision and round once at
	// the end, and the two paths can land on different, equally valid,
	// adjacent float64 values (kvstoretest.AssertConforms's own
	// binary-inexact subtest pins both outcomes rather than asserting one).
	// Callers that need cross-backend-identical results should keep to
	// integer-valued deltas (exactly representable in float64), the shape
	// every real caller in this codebase already uses.
	IncrByFloat(ctx context.Context, key string, delta float64) (float64, error)

	// IncrByFloatWithTTL adds delta to the number stored under key and
	// returns the result, exactly like IncrByFloat, but atomically attaches
	// ttl as the key's expiry on the same call that creates it: a missing or
	// expired key starts from zero and is stored with ttl as its expiry (a
	// ttl of zero or less stores it without one, matching Set's own
	// zero-or-less convention); a key that already exists is incremented and
	// keeps the expiry it already has -- ttl is ignored for a live key, never
	// extending it, so a rolling-window counter under sustained traffic still
	// ages out on schedule (the identical non-extension rule IncrByFloat
	// itself documents). The value is stored and parsed exactly as
	// IncrByFloat's own doc comment describes, and a key holding a
	// non-numeric value fails with ErrNotNumeric and is left unchanged, same
	// as IncrByFloat.
	//
	// This exists to close a real concurrency gap IncrByFloat alone cannot:
	// attaching a TTL to a freshly created key otherwise needs a caller-side
	// Get-then-Set after IncrByFloat, and any concurrent increment landing in
	// that Get-to-Set gap is silently overwritten by the Set, undercounting
	// the key. IncrByFloatWithTTL collapses "increment" and "attach the
	// expiry, but only on creation" into one atomic step, so no caller-side
	// sequence, retried or not, can lose an increment that way. Every backend
	// implements this as a single atomic operation -- extending whatever
	// mechanism its own IncrByFloat already uses to be atomic -- never as two
	// separate calls a caller could still race between.
	//
	// The same cross-implementation caveats IncrByFloat's own doc comment
	// states about arithmetic on a binary-inexact delta apply here
	// identically, since the arithmetic itself is unchanged.
	IncrByFloatWithTTL(ctx context.Context, key string, delta float64, ttl time.Duration) (float64, error)

	// CompareAndSwap replaces the value under key with newVal only if the
	// current value equals old, and reports whether the swap happened. A
	// missing or expired key matches an old of nil or zero length, which makes
	// CompareAndSwap usable as set-if-absent. A mismatch is an expected outcome
	// and returns false with a nil error. The swap never changes the key's
	// expiry.
	CompareAndSwap(ctx context.Context, key string, old, newVal []byte) (bool, error)
}
