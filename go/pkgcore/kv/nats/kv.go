// Package nats is a distributed deployment mode's KVStore, backed by a NATS
// JetStream Key/Value bucket every replica of a deployment reads and writes.
// It is split out of go/pkgcore's own package the same way kv/redis is --
// rather than living beside the KVStore interface -- so that a consumer
// which never wires a NATS-backed store does not inherit nats.go in its
// dependency graph: Go resolves dependencies per package, and an interface
// package that also carries one implementation hands every importer that
// implementation's whole dependency closure.
//
// Importing this package registers "kv.nats" on pkgcore's shared
// component as a side effect (see component.go), the same database/sql-
// style driver-registration pattern kv/redis uses, now with a second name
// under the "kv" module. The built-in composition still names "kv.redis" by
// default -- adding this implementation does not change what any existing
// Preset resolves -- so a host that wants NATS instead either builds its own
// Preset naming "kv.nats" or calls NewKVStore directly and wires it with
// the kv value's configuration block.
//
// # Why a JetStream KV bucket needs its own envelope
//
// The nats.go jetstream package's KeyValue store gives per-key Get/Put/
// Create/Update(revision)/Delete, with no native atomic increment and no
// native compare-by-value: Update only compares by revision. This package
// builds CompareAndSwap and IncrByFloat on top of that revision check (see
// their own doc comments for exactly how), and encodes each key's expiry
// inside the stored value itself rather than relying on the bucket's own TTL
// -- a JetStream KV bucket's TTL is a whole-bucket setting, and cannot
// express "IncrByFloat resets expiry only when it creates the key, preserves
// it when it increments an existing one," so bucket TTL is never touched by
// this package: buckets this package creates leave MaxAge at zero (never
// expire), and an existing bucket whose operator gave it a TTL is refused at
// adoption with ErrUnadoptableBucket rather than adopted, since the server
// would silently expire that bucket's ttl<=0 "stores forever" keys on its
// own clock (NewKVStore's doc comment lists the full adoptable surface).
//
// The pinned nats.go v1.53.1 does expose a server-side per-key TTL -- the
// KeyTTL create option, which requires the bucket-level LimitMarkerTTL to be
// enabled -- and it would move an expiry onto the server's own clock, the
// same fix kv/postgres's database-clock arithmetic makes. It is deliberately
// unused here, and the reason is probed, not assumed: KeyTTL applies only
// when a key is created, and Put and Update take no TTL option at all, so
// the only write a refresh or an increment can issue is a header-less
// Put/Update. Probed empirically against real nats-server 2.11.7 and 2.12.3,
// such a Put/Update clears a KeyTTL-created key's per-key TTL entirely (the
// key becomes immortal), so the server-side mechanism cannot express the two
// expiry semantics every KVStore backend must honour: the increment-
// preserves-expiry half is pinned by kvstoretest.AssertConforms, while the
// Set-clears-expiry half is pinned by each backend's own real-server
// integration layer (the same-named
// TestKVStore_SetWithNonPositiveTTL_StoresForever_AndClearsAnExistingExpiry).
// The envelope above therefore remains the only mechanism that can honour
// the shared contract, its deadline written on the writer's clock and judged
// on the reading replica's -- a clock-skew window kv/postgres's single-clock
// fix closes by construction and this backend cannot, evidenced rather than
// assumed.
//
// # Why keys are hex-encoded
//
// pkgcore.KVStore's keys are opaque strings with no restriction on their
// characters, but a JetStream KV key must match a narrow charset
// (alphanumeric, dash, underscore, slash, equals, dot -- notably not colon,
// which every other implementation's own tests use freely, e.g.
// "billing:invoice:1042"). This package hex-encodes the raw UTF-8 bytes of
// every caller-supplied key before it reaches nats.go, a lossless, injective
// mapping onto a charset JetStream always accepts, so no valid
// pkgcore.KVStore key is ever rejected here for containing a character the
// underlying store dislikes.
package nats

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/vislake/speed/go/pkgcore"
)

const (
	// kvNoExpiry is the boundary for the ttl argument of Set: any ttl at or
	// below it stores the key without an expiry, mirroring pkgcore's own
	// in-memory store and kv/redis.
	kvNoExpiry time.Duration = 0

	// envelopeExpiryLen is the width, in bytes, of the big-endian int64 unix-
	// nanosecond expiry header every stored value carries ahead of its
	// payload. Zero means "no expiry."
	envelopeExpiryLen = 8

	// maxEnvelopeValueLen is the largest payload encodeEnvelope accepts: the
	// envelope buffer's size is envelopeExpiryLen plus the payload, computed
	// in the platform's int, and an addition that wraps would hand make a
	// negative size to panic on. Values this long can only exist on a 32-bit
	// platform -- a 64-bit one cannot allocate a slice within a few bytes of
	// MaxInt -- and no real backend stores them either, JetStream's own max-
	// message ceiling being far lower, but the size arithmetic must be unable
	// to wrap on any platform: an overflowing payload is refused as an error
	// rather than encoded (CodeQL go/allocation-size-overflow).
	maxEnvelopeValueLen = math.MaxInt - envelopeExpiryLen

	// kvFloatFormat, kvFloatPrecisionShortest and kvFloatBitSize mirror
	// pkgcore's own unexported constants of the same name and purpose:
	// IncrByFloat's arithmetic happens client-side here (there is no native
	// atomic float increment in JetStream KV, unlike Redis's INCRBYFLOAT), so
	// this package needs its own copy of the exact encoding pkgcore.KVStore
	// promises callers -- the shortest decimal form that parses back to the
	// same float64, so repeated increments never lose precision to
	// formatting.
	kvFloatFormat            = 'g'
	kvFloatPrecisionShortest = -1
	kvFloatBitSize           = 64

	// maxIncrAttempts bounds IncrByFloat's compare-and-swap retry loop: no
	// native atomic increment exists, so every attempt is a fresh read
	// followed by a revision-guarded write, and a concurrent writer can make
	// any single attempt lose the race. 200 attempts, backing off up to
	// incrBackoffCap between them, comfortably covers the realistic
	// contention this package's own adversarial concurrency test drives (tens
	// of goroutines incrementing the same key) -- see
	// TestKVStore_ConcurrentIncrementsLoseNoUpdates in integration_test --
	// while still failing loudly, rather than hanging, if a caller is
	// genuinely starved.
	maxIncrAttempts = 200

	// incrBackoffBase and incrBackoffCap size the exponential backoff between
	// IncrByFloat's retry attempts: short enough to keep the common,
	// uncontended case fast (most calls need no retry at all), capped low
	// enough that even a fully-exhausted retry budget returns in well under a
	// second.
	incrBackoffBase = time.Millisecond
	incrBackoffCap  = 20 * time.Millisecond
)

// ErrUnadoptableBucket is returned by NewKVStore when the probe that found
// an existing JetStream KV bucket reports a configuration the store this
// package hands back cannot honour: adopting such a bucket would make a
// declaration the package registers silently false, or overthrow a contract
// pkgcore.KVStore pins for every implementation, while startup -- including
// the deployment-mode capability checks the assembly runs over the
// resolved "kv.nats" implementation -- would go on believing the declaration.
// The wrapped error names the bucket, the offending configuration and the
// declaration or contract it contradicts, so an operator who pre-provisioned
// the bucket knows exactly what to change; the refusal never rewrites or
// deletes the operator's bucket.
var ErrUnadoptableBucket = errors.New("pkgcore/kv/nats: existing bucket configuration is not adoptable by this implementation")

// kvStore is the distributed deployment mode's NATS-backed KVStore: a thin,
// stateless wrapper over a jetstream.KeyValue handle bound to one bucket.
// Nothing here holds a mutex or any other in-process state of its own --
// every operation's atomicity comes from the JetStream server's own
// per-subject revision check, never from anything this struct coordinates
// locally, which is why one instance is safe to share and reuse exactly like
// kv/redis's kvStore.
type kvStore struct {
	kv jetstream.KeyValue
}

// NewKVStore returns a pkgcore.KVStore backed by a JetStream KV bucket named
// bucket, provisioning that bucket if it does not already exist. nc must
// already be connected -- this package never dials, closes, or otherwise
// owns the connection, exactly like kv/redis's NewKVStore with its
// *redis.Client -- but unlike that constructor, provisioning a JetStream KV
// bucket is itself a round trip to the server, so this one takes a context
// and can fail: a JetStream KV bucket is schema the server must agree to
// hold, not a bare namespace a client can assume into existence the way a
// Redis key can.
//
// The provisioning is probe-then-create, never create-or-update: an
// existing bucket is probed with a plain KeyValue lookup and adopted when
// its configuration can satisfy what this implementation declares and
// relies on, and refused with ErrUnadoptableBucket otherwise.
// Update-overwriting an existing bucket with this constructor's defaults
// would silently rewrite a configuration a provisioning operator set
// deliberately
// -- replica count, storage engine, history depth and TTL all collapsed
// back to this constructor's single-replica, one-entry-history, no-TTL,
// file-storage defaults the moment a store was built against it (a
// pre-provisioned three-replica production bucket quietly demoted to
// single-replica, with all the availability that replication existed to
// buy). A bucket created here -- the genuinely absent case -- is
// provisioned with every KeyValueConfig field left at its default except
// Bucket itself: History defaults to 1 (this package keeps no history of
// its own -- every write replaces the prior value outright, the same
// behaviour Set/Update/Create give every other KVStore implementation), TTL
// defaults to zero (the bucket never expires a key on its own; see the
// package doc comment for why this package's own envelope is what carries
// expiry instead), and Storage defaults to jetstream.FileStorage, which is
// what lets this implementation declare pkgcore.SurvivesRestart.
//
// The adoptable surface is deliberately narrow, because it is exactly the
// surface the package's declarations and contracts cover: adopting a bucket
// must never make a registered declaration silently false, or overthrow a
// contract pkgcore.KVStore pins for every implementation, while the
// deployment-mode capability checks the assembly runs over the resolved
// "kv.nats" implementation go on believing the declaration. Two
// configuration facts are therefore verified before a pre-provisioned
// bucket is adopted (adoptionRefusal):
//
//   - Storage must be jetstream.FileStorage. A MemoryStorage bucket loses
//     every key it holds the moment the NATS server itself restarts,
//     silently forfeiting the pkgcore.SurvivesRestart capability "kv.nats"'s
//     registration declares -- exactly the claim the bootstrap capability
//     check exists to enforce, and the one an adoption would otherwise
//     bypass.
//   - TTL must be zero. A whole-bucket TTL silently expires keys this store
//     wrote with ttl <= 0 -- pkgcore.KVStore's stores-forever contract -- on
//     the server's own clock, overthrowing the envelope expiry design this
//     package builds on (see the package doc comment). Expiring must stay
//     entirely with each value's own envelope, as it is on the buckets this
//     constructor creates.
//
// Every other configuration fact an operator set is adopted untouched and
// is never a refusal reason: History depth, Description, MaxValueSize,
// Replicas, Placement and the rest are preserved byte for byte, since none
// of them can contradict a declared claim. Replicas in particular is not a
// checked dimension: pkgcore.MultiReplicaSafe speaks of concurrent
// application replicas sharing one bucket -- which JetStream's own
// server-side revision arbitration provides at any replication factor --
// never of the bucket's server-side replication, which can only make
// durability better than the single-replica bucket this constructor
// creates, never worse.
//
// Two builders racing to provision the same never-seen bucket are resolved
// by re-probing: the loser's create answers "already in use", and the loser
// then adopts the winner's bucket exactly as it would have adopted a
// pre-provisioned one, instead of failing. The "already in use" answer is
// classified by name -- nats.go maps the single-replica form to
// jetstream.ErrBucketExists (joined with ErrStreamNameAlreadyInUse) and a
// replicated-stream form can surface the stream-level error alone; both,
// plus any future rephrasing of the same server answer, are caught by
// bucketNameInUse, so a create race never surfaces as a hard error while a
// perfectly good bucket sits there to adopt.
//
// A nil nc panics: it is an unrecoverable wiring error at startup, exactly
// like kv/redis's nil-client panic, and every operation -- starting with the
// bucket provisioning this constructor itself performs -- would fail
// identically at first use.
func NewKVStore(ctx context.Context, nc *nats.Conn, bucket string) (pkgcore.KVStore, error) {
	if nc == nil {
		panic("pkgcore/kv/nats: NewKVStore requires a non-nil *nats.Conn")
	}

	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("pkgcore/kv/nats: build jetstream context: %w", err)
	}

	// Probe first: an existing bucket is adopted as-is, never re-created or
	// re-configured -- see the constructor doc comment for the rewrite
	// hazard this probe exists to prevent -- but only when its configuration
	// can satisfy what this implementation declares and relies on. An
	// unadoptable bucket is refused with ErrUnadoptableBucket, never adopted
	// with the declaration silently falsified (the constructor doc comment's
	// adoptable-surface list names the two checked dimensions).
	kv, err := js.KeyValue(ctx, bucket)
	if err == nil {
		if refusalErr := refuseUnadoptable(ctx, bucket, kv); refusalErr != nil {
			return nil, refusalErr
		}
		return &kvStore{kv: kv}, nil
	}
	if !errors.Is(err, jetstream.ErrBucketNotFound) {
		return nil, fmt.Errorf("pkgcore/kv/nats: probe bucket %q: %w", bucket, err)
	}

	created, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket})
	if err == nil {
		return &kvStore{kv: created}, nil
	}
	if bucketNameInUse(err) {
		// Lost a provisioning race against another builder of this
		// never-before-seen bucket: the bucket exists now, so adopt it the
		// same way a pre-provisioned one would have been adopted -- through
		// the same configuration verification, so a winner this constructor
		// itself created (always compliant) or an operator bucket that won
		// the race instead is refused, not adopted, when it cannot satisfy
		// the declarations. If the re-probe finds nothing, the create failed
		// for a reason other than the race and the original error is the
		// honest answer.
		if winner, probeErr := js.KeyValue(ctx, bucket); probeErr == nil {
			if refusalErr := refuseUnadoptable(ctx, bucket, winner); refusalErr != nil {
				return nil, refusalErr
			}
			return &kvStore{kv: winner}, nil
		}
	}
	return nil, fmt.Errorf("pkgcore/kv/nats: provision bucket %q: %w", bucket, err)
}

// refuseUnadoptable reads bucket's effective configuration back from the
// server and returns adoptionRefusal's verdict on it, or a read-back error.
// It is the one place an adopted bucket's configuration enters the package,
// shared by NewKVStore's two adoption paths -- the plain probe and the
// lost-create-race re-probe -- so neither can diverge on what is adoptable.
func refuseUnadoptable(ctx context.Context, bucket string, kv jetstream.KeyValue) error {
	status, err := kv.Status(ctx)
	if err != nil {
		return fmt.Errorf("pkgcore/kv/nats: read back adopted bucket %q configuration: %w", bucket, err)
	}
	return adoptionRefusal(status.Config())
}

// adoptionRefusal reports why the effective configuration cfg makes the
// bucket unadoptable, or nil when it may be adopted. The two refusal
// dimensions are exactly the two facts that can make a declaration this
// package registers silently false or overthrow a contract pkgcore.KVStore
// pins: a Storage other than jetstream.FileStorage (a MemoryStorage bucket
// loses every key on a NATS server restart, forfeiting the registered
// pkgcore.SurvivesRestart) and a nonzero TTL (the server silently expires
// the bucket's ttl<=0 "stores forever" keys on its own clock). They are
// evaluated in this fixed order, so a bucket violating both is refused for
// the first one, deterministically. Everything else an operator configured
// -- History, Description, MaxValueSize, Replicas, Placement -- is never a
// refusal reason and is adopted untouched; see the constructor doc comment's
// adoptable-surface list for the full decision surface.
func adoptionRefusal(cfg jetstream.KeyValueConfig) error {
	switch cfg.Storage {
	case jetstream.FileStorage:
		// The storage every declaration and contract in this package rests on.
	case jetstream.MemoryStorage:
		return fmt.Errorf("%w: bucket %q uses memory storage, which loses every key it holds when the NATS server itself restarts: only file storage (the configuration NewKVStore itself creates) can satisfy the pkgcore.SurvivesRestart capability this implementation declares -- delete the bucket and let NewKVStore create it, or re-provision it with file storage",
			ErrUnadoptableBucket, cfg.Bucket)
	default:
		return fmt.Errorf("%w: bucket %q uses an unrecognized storage type (%d), which this implementation cannot vouch for: only file storage (the configuration NewKVStore itself creates) can satisfy the pkgcore.SurvivesRestart capability this implementation declares -- delete the bucket and let NewKVStore create it, or re-provision it with file storage",
			ErrUnadoptableBucket, cfg.Bucket, cfg.Storage)
	}
	if cfg.TTL > 0 {
		return fmt.Errorf("%w: bucket %q carries a whole-bucket TTL of %v, which silently expires keys this store wrote with a ttl <= 0 on the server's own clock: pkgcore.KVStore's stores-forever contract requires the bucket itself to never expire a key, since expiry lives in each value's own envelope (the configuration NewKVStore itself creates) -- delete the bucket and let NewKVStore create it, or re-provision it with no TTL",
			ErrUnadoptableBucket, cfg.Bucket, cfg.TTL)
	}
	return nil
}

// bucketNameInUse reports whether err is one of the answers a JetStream
// server gives when a CreateKeyValue races another builder of the same
// bucket: nats.go's own CreateKeyValue maps the single-replica
// "stream name already in use" API answer onto ErrBucketExists (joined with
// ErrStreamNameAlreadyInUse for backward compatibility), while a
// replicated-stream create race can surface as the bare stream-level error
// or, across server versions, a rephrased answer this package cannot see in
// advance -- so the classification is errors.Is over both nats.go sentinels
// plus a text match on the server's own fixed wording, the same coupling
// kv/redis's ErrNotNumeric detection documents for Redis's fixed error
// text. Every path through here ends in a re-probe that adopts the winner,
// so over- and under-matching both degrade safely: a missed classification
// reports the original create error, and an over-match on a genuinely
// absent bucket is caught by the re-probe's own ErrBucketNotFound.
func bucketNameInUse(err error) bool {
	return errors.Is(err, jetstream.ErrBucketExists) ||
		errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) ||
		strings.Contains(err.Error(), "stream name already in use")
}

// Get implements pkgcore.KVStore.Get. A key JetStream itself has never held,
// and one this package earlier marked deleted through Delete, both surface
// from nats.go as jetstream.ErrKeyNotFound and are reported as a miss here,
// not an error. A key whose own envelope-encoded expiry has passed is
// likewise reported as a miss -- JetStream still holds the message, since
// this package never relies on the bucket's own TTL, so Get purges it with a
// revision-guarded Delete before returning: a best-effort reclaim, exactly
// mirroring pkgcore's own in-memory store's "drop the entry lazily, when an
// operation next touches the key" precedent. The revision guard means a
// concurrent write that revived the key between this Get's read and its
// cleanup attempt is never clobbered by that cleanup; the cleanup's own
// success or failure is never reported, since Get's correctness -- reporting
// the key as absent -- does not depend on it landing.
func (s *kvStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	encKey := encodeKey(key)
	entry, err := s.kv.Get(ctx, encKey)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("pkgcore/kv/nats: get: %w", err)
	}

	value, expiresAt, err := decodeEnvelope(entry.Value())
	if err != nil {
		return nil, false, fmt.Errorf("pkgcore/kv/nats: get: %w", err)
	}
	if expired(expiresAt) {
		_ = s.kv.Delete(ctx, encKey, jetstream.LastRevision(entry.Revision()))
		return nil, false, nil
	}

	// Copy, so that a caller mutating the result cannot reach into whatever
	// nats.go's own entry still holds.
	return bytes.Clone(value), true, nil
}

// Set implements pkgcore.KVStore.Set. Put is an unconditional write -- it
// replaces whatever revision, value or delete marker the key held, which is
// exactly what "replacing any existing value and expiry" requires -- and the
// new envelope always carries the expiry ttl computes fresh, so a previous
// expiry is cleared exactly as pkgcore.KVStore documents.
func (s *kvStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	var expiresAt time.Time
	if ttl > kvNoExpiry {
		expiresAt = time.Now().Add(ttl)
	}

	encoded, err := encodeEnvelope(value, expiresAt)
	if err != nil {
		return fmt.Errorf("pkgcore/kv/nats: set: %w", err)
	}
	if _, err := s.kv.Put(ctx, encodeKey(key), encoded); err != nil {
		return fmt.Errorf("pkgcore/kv/nats: set: %w", err)
	}
	return nil
}

// Delete implements pkgcore.KVStore.Delete. nats.go's own KeyValue.Delete
// takes no revision expectation unless a LastRevision option is given, so it
// succeeds unconditionally whether or not the key currently exists -- which
// is exactly "deleting a key that does not exist is not an error."
func (s *kvStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := s.kv.Delete(ctx, encodeKey(key)); err != nil {
		return fmt.Errorf("pkgcore/kv/nats: delete: %w", err)
	}
	return nil
}

// IncrByFloat implements pkgcore.KVStore.IncrByFloat. JetStream KV has no
// native atomic increment, so this is a bounded compare-and-swap retry loop:
// read the current envelope (or treat a genuinely absent key, and a key
// whose own encoded expiry already passed, as zero with no prior expiry),
// compute the new value, and attempt a revision-guarded write -- Create for a
// key JetStream has never held, Update at the revision just read otherwise,
// including for a logically-expired-but-still-present entry, since only
// Update's per-subject revision check can safely replace a message JetStream
// itself still considers live. A revision conflict on either call means
// another writer landed between this attempt's read and its write; that is
// not reported as an error, it is retried from a fresh read, up to
// maxIncrAttempts times.
//
// A key already holding a non-numeric value fails immediately with
// pkgcore.ErrNotNumeric and is left completely unchanged: this happens on the
// read side, before any write is attempted, so there is nothing to retry.
func (s *kvStore) IncrByFloat(ctx context.Context, key string, delta float64) (float64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	encKey := encodeKey(key)
	backoff := incrBackoffBase
	var lastConflict error

	for attempt := 0; attempt < maxIncrAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepOrDone(ctx, backoff); err != nil {
				return 0, err
			}
			backoff *= 2
			if backoff > incrBackoffCap {
				backoff = incrBackoffCap
			}
		}

		current, expiresAt, _, revision, err := s.readNumeric(ctx, encKey)
		if err != nil {
			return 0, err
		}

		result := current + delta
		encoded, err := encodeEnvelope(
			strconv.AppendFloat(nil, result, kvFloatFormat, kvFloatPrecisionShortest, kvFloatBitSize),
			expiresAt,
		)
		if err != nil {
			return 0, fmt.Errorf("pkgcore/kv/nats: incr: %w", err)
		}

		if revision == 0 {
			// nats.go's own Create sequence number 0 means "no message has
			// ever been published on this subject," the same convention this
			// package's readNumeric uses for a key JetStream has never held.
			if _, err := s.kv.Create(ctx, encKey, encoded); err != nil {
				if errors.Is(err, jetstream.ErrKeyExists) {
					lastConflict = err
					continue
				}
				return 0, fmt.Errorf("pkgcore/kv/nats: incr: %w", err)
			}
			return result, nil
		}

		if _, err := s.kv.Update(ctx, encKey, encoded, revision); err != nil {
			if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
				lastConflict = err
				continue
			}
			return 0, fmt.Errorf("pkgcore/kv/nats: incr: %w", err)
		}
		return result, nil
	}

	return 0, fmt.Errorf("pkgcore/kv/nats: incr: exceeded %d compare-and-swap attempts without winning the race on %q: %w",
		maxIncrAttempts, key, lastConflict)
}

// IncrByFloatWithTTL implements pkgcore.KVStore.IncrByFloatWithTTL. It is the
// identical revision-guarded compare-and-swap retry loop IncrByFloat runs,
// with one difference: when readNumeric reports the key not live (wasLive
// false -- covering both a key JetStream has never held and one whose
// envelope-encoded expiry has already passed), the new envelope's expiry is
// computed from ttl instead of always being time.Time{} ("never"). A live
// key's own expiresAt -- read straight from readNumeric, whether or not it
// happens to be the zero time itself -- is carried through unchanged on
// every retry; ttl is never consulted for it, matching IncrByFloat's own
// non-extension rule.
func (s *kvStore) IncrByFloatWithTTL(ctx context.Context, key string, delta float64, ttl time.Duration) (float64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	encKey := encodeKey(key)
	backoff := incrBackoffBase
	var lastConflict error

	for attempt := 0; attempt < maxIncrAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepOrDone(ctx, backoff); err != nil {
				return 0, err
			}
			backoff *= 2
			if backoff > incrBackoffCap {
				backoff = incrBackoffCap
			}
		}

		current, expiresAt, wasLive, revision, err := s.readNumeric(ctx, encKey)
		if err != nil {
			return 0, err
		}
		if !wasLive {
			// Only a fresh (missing or logically expired) key ever gets ttl
			// attached; a live key's own expiresAt above is used untouched.
			expiresAt = expiryFromTTL(ttl)
		}

		result := current + delta
		encoded, err := encodeEnvelope(
			strconv.AppendFloat(nil, result, kvFloatFormat, kvFloatPrecisionShortest, kvFloatBitSize),
			expiresAt,
		)
		if err != nil {
			return 0, fmt.Errorf("pkgcore/kv/nats: incr with ttl: %w", err)
		}

		if revision == 0 {
			if _, err := s.kv.Create(ctx, encKey, encoded); err != nil {
				if errors.Is(err, jetstream.ErrKeyExists) {
					lastConflict = err
					continue
				}
				return 0, fmt.Errorf("pkgcore/kv/nats: incr with ttl: %w", err)
			}
			return result, nil
		}

		if _, err := s.kv.Update(ctx, encKey, encoded, revision); err != nil {
			if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
				lastConflict = err
				continue
			}
			return 0, fmt.Errorf("pkgcore/kv/nats: incr with ttl: %w", err)
		}
		return result, nil
	}

	return 0, fmt.Errorf("pkgcore/kv/nats: incr with ttl: exceeded %d compare-and-swap attempts without winning the race on %q: %w",
		maxIncrAttempts, key, lastConflict)
}

// readNumeric reads the current numeric value, expiry, liveness and
// NATS-level revision for a key already hex-encoded by the caller, in the
// shape IncrByFloat's and IncrByFloatWithTTL's retry loops need to decide
// between Create and Update, and (IncrByFloatWithTTL only) whether a ttl
// argument should apply:
//
//   - A key JetStream has never held (jetstream.ErrKeyNotFound) reports
//     (0, zero time, false, 0, nil): the zero revision is the loop's own
//     signal to attempt Create, and wasLive is false -- this is a fresh key.
//   - A key whose envelope-encoded expiry has passed reports (0, zero time,
//     false, entry.Revision(), nil): the value restarts at zero exactly like
//     a genuinely absent key (wasLive is false here too -- IncrByFloatWithTTL
//     treats this identically to a genuinely missing key), but the non-zero
//     revision tells the loop the NATS-level message is still live, so the
//     write that follows must go through Update at that revision, never
//     Create (which would fail ErrKeyExists against a message that, to
//     JetStream, was never deleted).
//   - A live key holding a non-numeric value reports pkgcore.ErrNotNumeric,
//     read-side, before anything is written.
//   - A live key holding a numeric value -- whether or not it happens to
//     carry an expiry of its own -- reports wasLive true, with expiresAt
//     exactly the entry's own (the zero time.Time if it has none, which
//     IncrByFloatWithTTL must then leave alone, never mistaking for "fresh").
func (s *kvStore) readNumeric(ctx context.Context, encKey string) (current float64, expiresAt time.Time, wasLive bool, revision uint64, err error) {
	entry, err := s.kv.Get(ctx, encKey)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return 0, time.Time{}, false, 0, nil
	}
	if err != nil {
		return 0, time.Time{}, false, 0, fmt.Errorf("pkgcore/kv/nats: incr: %w", err)
	}

	value, entryExpiresAt, decodeErr := decodeEnvelope(entry.Value())
	if decodeErr != nil {
		return 0, time.Time{}, false, 0, fmt.Errorf("pkgcore/kv/nats: incr: %w", decodeErr)
	}
	if expired(entryExpiresAt) {
		return 0, time.Time{}, false, entry.Revision(), nil
	}

	parsed, parseErr := strconv.ParseFloat(string(value), kvFloatBitSize)
	if parseErr != nil {
		return 0, time.Time{}, false, 0, pkgcore.ErrNotNumeric
	}
	return parsed, entryExpiresAt, true, entry.Revision(), nil
}

// CompareAndSwap implements pkgcore.KVStore.CompareAndSwap. The read
// determines only which comparison and which write to attempt; the write
// itself is what is actually atomic, since nats.go's Create and Update both
// carry the server-side per-subject revision check as their real guard.
// Concretely: Create fails with jetstream.ErrKeyExists, and Update fails with
// jetstream.ErrKeyRevisionMismatch, if anything changed the key between this
// call's read and its write -- either failure is reported here as an
// ordinary false/nil "the swap did not happen," never as an error, so a
// caller cannot tell a lost race apart from a genuine value mismatch, which
// is exactly what pkgcore.KVStore promises. The swap never changes the key's
// expiry: a matched swap re-encodes newVal against the same expiresAt the
// read found, and a set-if-absent swap always creates with no expiry, mirroring
// pkgcore's own in-memory store and kv/redis.
func (s *kvStore) CompareAndSwap(ctx context.Context, key string, old, newVal []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	encKey := encodeKey(key)

	entry, err := s.kv.Get(ctx, encKey)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return s.casCreate(ctx, encKey, old, newVal)
	}
	if err != nil {
		return false, fmt.Errorf("pkgcore/kv/nats: cas: %w", err)
	}

	value, expiresAt, decodeErr := decodeEnvelope(entry.Value())
	if decodeErr != nil {
		return false, fmt.Errorf("pkgcore/kv/nats: cas: %w", decodeErr)
	}

	if expired(expiresAt) {
		// A logically expired key matches an empty old exactly like a
		// genuinely absent one, but JetStream still holds a live message at
		// entry.Revision(), so the write must go through Update at that
		// revision rather than Create.
		if len(old) != 0 {
			return false, nil
		}
		return s.casWrite(ctx, encKey, entry.Revision(), newVal, time.Time{})
	}

	if !bytes.Equal(value, old) {
		return false, nil
	}
	return s.casWrite(ctx, encKey, entry.Revision(), newVal, expiresAt)
}

// casCreate attempts the set-if-absent half of CompareAndSwap against a key
// jetstream.ErrKeyNotFound already told the caller JetStream has never held
// (or has since deleted; nats.go's own Create already resolves that
// distinction internally).
func (s *kvStore) casCreate(ctx context.Context, encKey string, old, newVal []byte) (bool, error) {
	if len(old) != 0 {
		return false, nil
	}
	encoded, err := encodeEnvelope(newVal, time.Time{})
	if err != nil {
		return false, fmt.Errorf("pkgcore/kv/nats: cas: %w", err)
	}
	if _, err := s.kv.Create(ctx, encKey, encoded); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return false, nil
		}
		return false, fmt.Errorf("pkgcore/kv/nats: cas: %w", err)
	}
	return true, nil
}

// casWrite performs the revision-guarded write for a matched comparison,
// preserving expiresAt exactly (the zero value for a fresh, absent-turned-
// present key; the entry's own expiry for a genuine value match), and maps a
// lost race to (false, nil) rather than an error.
func (s *kvStore) casWrite(ctx context.Context, encKey string, revision uint64, newVal []byte, expiresAt time.Time) (bool, error) {
	encoded, err := encodeEnvelope(newVal, expiresAt)
	if err != nil {
		return false, fmt.Errorf("pkgcore/kv/nats: cas: %w", err)
	}
	if _, err := s.kv.Update(ctx, encKey, encoded, revision); err != nil {
		if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
			return false, nil
		}
		return false, fmt.Errorf("pkgcore/kv/nats: cas: %w", err)
	}
	return true, nil
}

// encodeKey maps an opaque pkgcore.KVStore key onto a JetStream KV key: the
// lowercase hex encoding of key's raw UTF-8 bytes, a lossless, injective
// mapping onto a charset (0-9, a-f) that always satisfies JetStream's own
// key-validity rule (alphanumeric, dash, underscore, slash, equals, dot),
// regardless of what characters key itself contains. See the package doc
// comment's "Why keys are hex-encoded" section.
func encodeKey(key string) string {
	return hex.EncodeToString([]byte(key))
}

// encodeEnvelope prefixes value with its expiry, encoded as a big-endian
// int64 of Unix nanoseconds -- zero for "no expiry," matching every other
// pkgcore.KVStore implementation's zero-value convention -- so that a
// JetStream message, which carries no expiry concept of its own once bucket-
// level TTL is left unused, still carries exactly the expiry pkgcore.KVStore
// requires. A payload too long to fit behind the header inside the platform
// int the allocation size is computed in (see maxEnvelopeValueLen) is
// refused with an error, never encoded through size arithmetic that would
// wrap.
func encodeEnvelope(value []byte, expiresAt time.Time) ([]byte, error) {
	if len(value) > maxEnvelopeValueLen {
		return nil, fmt.Errorf("value of %d bytes is too large to store: adding the %d-byte expiry header would overflow the size arithmetic",
			len(value), envelopeExpiryLen)
	}

	var expiryNano int64
	if !expiresAt.IsZero() {
		expiryNano = expiresAt.UnixNano()
	}

	buf := make([]byte, envelopeExpiryLen+len(value))
	binary.BigEndian.PutUint64(buf[:envelopeExpiryLen], uint64(expiryNano))
	copy(buf[envelopeExpiryLen:], value)
	return buf, nil
}

// decodeEnvelope is encodeEnvelope's inverse. data shorter than the expiry
// header is reported as a malformed-envelope error rather than panicking or
// silently truncating: every envelope this package itself ever wrote is at
// least envelopeExpiryLen bytes long, so a shorter value here means the
// bucket holds something this package did not write.
func decodeEnvelope(data []byte) ([]byte, time.Time, error) {
	if len(data) < envelopeExpiryLen {
		return nil, time.Time{}, fmt.Errorf("malformed envelope: %d bytes, want at least %d", len(data), envelopeExpiryLen)
	}

	nano := int64(binary.BigEndian.Uint64(data[:envelopeExpiryLen])) //nolint:gosec // deliberate reinterpretation of a stored bit pattern, not a truncating conversion.
	var expiresAt time.Time
	if nano != 0 {
		expiresAt = time.Unix(0, nano)
	}
	return data[envelopeExpiryLen:], expiresAt, nil
}

// expired reports whether expiresAt has already passed at the current
// instant. The zero time -- encodeEnvelope's "no expiry" sentinel -- never
// expires, mirroring pkgcore's own in-memory kvEntry.expired.
func expired(expiresAt time.Time) bool {
	return !expiresAt.IsZero() && !time.Now().Before(expiresAt)
}

// expiryFromTTL turns a KVStore ttl argument into this package's own absolute
// expiry, the zero time.Time for "no expiry" -- mirroring every other
// backend's identical zero-or-less-means-no-expiry convention (see, e.g.,
// kv/memcached's own expiryFromTTL).
func expiryFromTTL(ttl time.Duration) time.Time {
	if ttl <= kvNoExpiry {
		return time.Time{}
	}
	return time.Now().Add(ttl)
}

// sleepOrDone waits for d, or returns ctx's error immediately if ctx is
// cancelled first -- IncrByFloat's retry loop uses this for its backoff so a
// caller that cancels mid-retry is never kept waiting out a full backoff
// window.
func sleepOrDone(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
