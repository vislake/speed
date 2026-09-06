// Package postgres is a distributed deployment mode's pkgcore.KVStore,
// backed by a single PostgreSQL table with a TTL column. It exists for a
// deployment that already runs PostgreSQL (go/dbkit's own supported
// dialect) and does not want to stand up Redis purely to satisfy this one
// seam -- the genuinely zero-extra-infrastructure distributed KVStore
// option, alongside kv/redis's Redis implementation, mirroring
// go/pkgcore/eventbus/postgres's identical "already running PostgreSQL"
// rationale for the EventBus seam.
//
// It is split out of go/pkgcore's own package for the identical reason
// kv/redis is: a consumer which never wires a PostgreSQL-backed store must
// not inherit jackc/pgx/v5 in its dependency graph (docs/internal/03-
// deployment-modes.md's implementation-registry section measures the cost
// and names the boundary; this package's own AGENTS.md entry records the
// measured number for pgx specifically).
//
// Importing this package registers "kv.postgres" on pkgcore's shared
// KVStoreRegistry as a side effect (see register.go) -- the same
// database/sql-style driver-registration pattern kv/redis follows, applied
// a second time for a second implementation of the same seam. It is not
// named by pkgcore.PresetDistributed, which still points "kv" at
// "kv.redis"; a host that wants this implementation instead builds its own
// Preset (a plain map literal, see Preset's own doc comment) or bypasses the
// preset layer entirely by constructing NewKVStore and wiring it with
// pkgcore.WithKVStore, exactly as a host choosing kv/redis explicitly does
// today.
//
// # Sharing an existing connection pool
//
// A host that already runs dbkit.Open(ctx, dbkit.Options{Dialect:
// dbkit.DialectPostgres, ...}) for its business tables does NOT need a
// second PostgreSQL connection pool for this seam: NewKVStore takes a
// *pgxpool.Pool directly rather than a *gorm.DB, so the same pool a
// dedicated PostgreSQL client already opened for another purpose (an
// eventbus/postgres.EventBus, a direct pgxpool.New call) can be handed to
// NewKVStore too, at zero extra cost -- this is exactly the
// zero-extra-infrastructure property the package doc comment above
// promises. dbkit itself wraps gorm.io/driver/postgres rather than a bare
// *pgxpool.Pool, so a host on dbkit's own connection wants a second, small
// pool dedicated to this seam (pgxpool.New with the same DSN) rather than
// reaching into gorm's internals for one.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vislake/speed/go/pkgcore"
)

const (
	// kvNoExpiry is the ttl value that stores a key with no expiry, mirroring
	// pkgcore's in-memory store's zero-or-negative-ttl convention.
	kvNoExpiry time.Duration = 0

	// kvFloatFormat/kvFloatPrecisionShortest/kvFloatBitSize mirror
	// pkgcore.kv.go's own unexported constants of the identical name and
	// value byte for byte (this package cannot import them: they are
	// unexported in a different package) -- 'g' with a precision of -1
	// asks strconv for the shortest decimal text that parses back to the
	// exact same float64, which is what keeps a freshly created counter's
	// stored text small and readable rather than a raw binary expansion.
	kvFloatFormat            = 'g'
	kvFloatPrecisionShortest = -1
	kvFloatBitSize           = 64

	// getSQL implements KVStore.Get: a live row (no expiry, or an expiry not
	// yet passed) is visible; an expired or absent row is not. This is the
	// "drop lazily" half of the table's expiry story -- see the migration
	// file's own doc comment on expires_at, and Sweep for the other half.
	getSQL = `SELECT value FROM pkgcore_kv_entries WHERE key = $1 AND (expires_at IS NULL OR expires_at > now())`

	// setSQL implements KVStore.Set: an upsert that always replaces both the
	// value and the expiry column wholesale, matching the interface's own
	// "Set... replacing any existing value and expiry" contract -- a
	// pre-existing TTL is cleared exactly when the new ttl argument asks for
	// no expiry, because EXCLUDED.expires_at is NULL in that case.
	setSQL = `INSERT INTO pkgcore_kv_entries (key, value, expires_at) VALUES ($1, $2, $3)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, expires_at = EXCLUDED.expires_at`

	// deleteSQL implements KVStore.Delete. Deleting an absent key affects
	// zero rows, which is not an error -- pgx.Exec succeeding with a zero
	// CommandTag is a completely ordinary outcome, not a failure this store
	// needs to detect.
	deleteSQL = `DELETE FROM pkgcore_kv_entries WHERE key = $1`

	// incrByFloatSQL implements KVStore.IncrByFloat as one database-
	// arbitrated upsert, never a Go-level read-modify-write -- see the
	// package's own Example and this file's doc comment on why that matters
	// under concurrency.
	//
	// $1 is the key; $2 is delta pre-formatted in Go as its shortest exact
	// decimal text (strconv.FormatFloat(delta, 'g', -1, 64)), never a raw
	// float8 parameter -- casting a float8 straight to numeric preserves
	// its exact binary value, which for a value like 0.1 is a long,
	// ugly-but-correct decimal expansion rather than the short form a
	// caller would expect to see on a later plain Get; casting the
	// pre-formatted TEXT to numeric instead parses exactly the digits Go
	// chose, with the actual addition still happening entirely in SQL.
	//
	// The ON CONFLICT branch's CASE distinguishes exactly the two cases the
	// interface's doc comment on IncrByFloat describes: an expired row is
	// treated as absent (reset to delta, with no expiry -- EXCLUDED.value,
	// not the row's own expired value, and NULL, not the row's own stale
	// expires_at), while a live row keeps its own expiry untouched and has
	// delta added to its own numeric value. A live row whose value is not
	// valid decimal text fails the whole statement (see the SQL error the
	// convert_from/numeric cast raises), which IncrByFloat below maps to
	// pkgcore.ErrNotNumeric with the row left exactly as it was, since a
	// failed statement never partially applies.
	//
	// This is genuinely atomic under concurrency: PostgreSQL serializes
	// concurrent INSERT ... ON CONFLICT DO UPDATE attempts against the same
	// key at the row level, so two callers racing an increment on the same
	// key can never both read the same "current" value -- the second to
	// proceed always sees the first's already-committed result, exactly the
	// guarantee go/billing's applyBalanceDelta relies on for its own
	// single-UPDATE arithmetic guard.
	incrByFloatSQL = `INSERT INTO pkgcore_kv_entries AS kv (key, value, expires_at)
VALUES ($1, ($2::numeric)::text::bytea, NULL)
ON CONFLICT (key) DO UPDATE SET
    value = CASE
        WHEN kv.expires_at IS NOT NULL AND kv.expires_at <= now()
            THEN ($2::numeric)::text::bytea
        ELSE (convert_from(kv.value, 'UTF8')::numeric + $2::numeric)::text::bytea
    END,
    expires_at = CASE
        WHEN kv.expires_at IS NOT NULL AND kv.expires_at <= now()
            THEN NULL
        ELSE kv.expires_at
    END
RETURNING value`

	// incrByFloatWithTTLSQL implements KVStore.IncrByFloatWithTTL: the exact
	// upsert incrByFloatSQL performs, with one difference -- everywhere
	// incrByFloatSQL resets expires_at to NULL (a genuinely missing row, or
	// an existing row whose own expiry has already passed), this statement
	// instead sets it to $3, the caller-computed absolute expiry (NULL when
	// the caller passed a ttl of zero or less, matching Set's own
	// zero-or-less-means-no-expiry convention). A live, non-expired row's
	// branch is untouched byte for byte: its own expires_at is carried over
	// exactly as incrByFloatSQL already does, so $3 is never consulted for
	// it -- ttl is ignored for a live key, never extending it, the identical
	// non-extension rule IncrByFloat's own doc comment states.
	//
	// This is one statement, not incrByFloatSQL followed by a second write:
	// the same row-level serialization argument in incrByFloatSQL's own doc
	// comment applies unchanged, so no concurrent caller creating this key
	// can ever have its increment overwritten by another caller's own
	// expiry-attaching write -- there is no second write to race against.
	incrByFloatWithTTLSQL = `INSERT INTO pkgcore_kv_entries AS kv (key, value, expires_at)
VALUES ($1, ($2::numeric)::text::bytea, $3::timestamptz)
ON CONFLICT (key) DO UPDATE SET
    value = CASE
        WHEN kv.expires_at IS NOT NULL AND kv.expires_at <= now()
            THEN ($2::numeric)::text::bytea
        ELSE (convert_from(kv.value, 'UTF8')::numeric + $2::numeric)::text::bytea
    END,
    expires_at = CASE
        WHEN kv.expires_at IS NOT NULL AND kv.expires_at <= now()
            THEN $3::timestamptz
        ELSE kv.expires_at
    END
RETURNING value`

	// compareAndSwapSQL implements KVStore.CompareAndSwap as one database-
	// arbitrated statement combining an UPDATE CTE (the "key exists" path,
	// live-match or expired-treated-as-absent) with an INSERT CTE guarded by
	// NOT EXISTS (the "key genuinely absent" path), so exactly one of the two
	// ever changes a row, and the result is the sum of however many rows
	// each one touched (0 or 1, never both).
	//
	// $1 is the key, $2 is old (normalized to a non-nil, possibly
	// zero-length slice -- see normalizeValue's doc comment for why nil
	// must never reach this parameter), $3 is newVal (also normalized).
	// Both carry an explicit ::bytea cast at every use: without one,
	// PostgreSQL's own untyped-parameter inference resolves $2's type from
	// its first appearance (inside octet_length, which accepts both text
	// and bytea) as text, and then refuses "value = $2" against the bytea
	// column with "operator does not exist: bytea = text" -- a real failure
	// this package's own integration tier caught before the cast was added,
	// pinned by every test in this file that exercises CompareAndSwap.
	//
	// The "matched" CTE's WHERE clause is "current value equals old,
	// treating missing/expired as empty" written out exactly: a live row
	// (no expiry, or an expiry not yet passed) matches when its value
	// equals old byte-for-byte; an expired row matches only when old is
	// empty (octet_length($2) = 0), the row's stale value never compared
	// against, since an expired key is absent for matching purposes -- and
	// on a match, expires_at collapses to NULL exactly when the row being
	// matched was expired (a fresh start, mirroring IncrByFloat's identical
	// rule), otherwise stays exactly as it was, because a swap is never a
	// refresh.
	//
	// The "inserted" CTE only ever proposes a row when old is empty
	// (set-if-absent) AND "matched" touched nothing -- guaranteeing it never
	// fires for a live key matched instead, and never fires for a
	// non-empty old expecting a specific value on an absent key (which must
	// report a mismatch, not create the key). Its own
	// "ON CONFLICT (key) DO NOTHING" is what makes many concurrent
	// set-if-absent callers on the same never-before-seen key race safely:
	// PostgreSQL resolves the concurrent-insert race at the row level, so
	// exactly one caller's INSERT commits and every other caller's INSERT
	// silently inserts zero rows (never a raised unique-violation error),
	// which is exactly "loses the race, swapped=false, err=nil" -- see the
	// package's own adversarial concurrency test.
	compareAndSwapSQL = `WITH matched AS (
    UPDATE pkgcore_kv_entries
    SET value = $3::bytea,
        expires_at = CASE
            WHEN expires_at IS NOT NULL AND expires_at <= now() THEN NULL
            ELSE expires_at
        END
    WHERE key = $1
      AND (
          (expires_at IS NOT NULL AND expires_at <= now() AND octet_length($2::bytea) = 0)
          OR ((expires_at IS NULL OR expires_at > now()) AND value = $2::bytea)
      )
    RETURNING 1
),
inserted AS (
    INSERT INTO pkgcore_kv_entries (key, value, expires_at)
    SELECT $1, $3::bytea, NULL
    WHERE octet_length($2::bytea) = 0 AND NOT EXISTS (SELECT 1 FROM matched)
    ON CONFLICT (key) DO NOTHING
    RETURNING 1
)
SELECT (SELECT count(*) FROM matched) + (SELECT count(*) FROM inserted)`

	// sweepSQL implements Store.Sweep: a plain physical delete of every row
	// whose expiry has already passed, using the partial index the
	// migration file creates for exactly this query shape.
	sweepSQL = `DELETE FROM pkgcore_kv_entries WHERE expires_at IS NOT NULL AND expires_at <= now()`
)

// Store is the distributed deployment mode's KVStore backed by a
// PostgreSQL connection pool every replica of a deployment shares. It
// implements pkgcore.KVStore (see the var _ assertion below) and additionally
// exposes Sweep, a capability the interface itself has no room for -- see
// Sweep's own doc comment for why a plain pkgcore.KVStore-typed variable
// cannot reach it, and NewKVStore's doc comment for why that is deliberate
// rather than an oversight.
type Store struct {
	pool *pgxpool.Pool
}

// NewKVStore returns a *Store backed by the given PostgreSQL connection
// pool, the distributed deployment mode's PostgreSQL-backed implementation
// of the seam pkgcore.NewMemoryKVStore covers in standalone mode. *Store
// implements pkgcore.KVStore, so it is assignable anywhere that interface is
// expected (pkgcore.WithKVStore, a struct field, a test double slot) exactly
// like kv/redis.NewKVStore's return value; NewKVStore returns the concrete
// type instead of the bare interface only so that a caller who does want
// Sweep can reach it without a type assertion, the same shape
// go/billing.NewCreditService and similar constructors in this codebase
// already use for "interface plus one extra capability".
//
// The store runs its operations directly on pool, which the caller keeps
// owning: it stays open when the store is garbage-collected, and the store
// never opens, closes or configures it.
//
// pkgcore_kv_entries must already exist -- call EnsureSchema against pool
// once, before the store's first operation, or wire a host's own
// dbkit.MigrationRegistry against migrations.FS instead (see EnsureSchema's
// own doc comment for both paths).
//
// The store preserves pkgcore.NewMemoryKVStore's and kv/redis.NewKVStore's
// semantics exactly: Set replaces both the value and any expiry, IncrByFloat
// keeps a live key's expiry and starts a missing or expired key at zero with
// no expiry, IncrByFloatWithTTL does the same but atomically attaches an
// expiry on the call that creates the key, and CompareAndSwap compares the
// whole value and never changes the key's expiry. One boundary detail is this
// backend's own: unlike
// Redis, PostgreSQL has no native active expiry, so an expired row is
// invisible to every read (the WHERE guard every statement in this file
// carries) but is not physically removed until something touches it again
// or a host calls Sweep -- see the migration file's own doc comment on
// expires_at for the full story, and Sweep's doc comment for the hygiene
// half a host is expected to run on its own schedule.
//
// A nil pool panics: it is an unrecoverable wiring error at startup, and
// every operation would fail identically at first use.
func NewKVStore(pool *pgxpool.Pool) *Store {
	if pool == nil {
		panic("pkgcore/kv/postgres: NewKVStore requires a non-nil *pgxpool.Pool")
	}
	return &Store{pool: pool}
}

// var _ pins that *Store satisfies pkgcore.KVStore at compile time.
var _ pkgcore.KVStore = (*Store)(nil)

// Get implements pkgcore.KVStore.Get.
func (s *Store) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	var value []byte
	err := s.pool.QueryRow(ctx, getSQL, key).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("pkgcore/kv/postgres: get: %w", err)
	}
	return value, true, nil
}

// Set implements pkgcore.KVStore.Set.
func (s *Store) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	var expiresAt *time.Time
	if ttl > kvNoExpiry {
		at := time.Now().Add(ttl)
		expiresAt = &at
	}

	if _, err := s.pool.Exec(ctx, setSQL, key, normalizeValue(value), expiresAt); err != nil {
		return fmt.Errorf("pkgcore/kv/postgres: set: %w", err)
	}
	return nil
}

// Delete implements pkgcore.KVStore.Delete. Deleting a key the table does
// not hold is not an error.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if _, err := s.pool.Exec(ctx, deleteSQL, key); err != nil {
		return fmt.Errorf("pkgcore/kv/postgres: delete: %w", err)
	}
	return nil
}

// IncrByFloat implements pkgcore.KVStore.IncrByFloat. See incrByFloatSQL's
// own doc comment for the single-statement design and its atomicity
// argument.
func (s *Store) IncrByFloat(ctx context.Context, key string, delta float64) (float64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	deltaText := formatFloat(delta)

	var result []byte
	err := s.pool.QueryRow(ctx, incrByFloatSQL, key, deltaText).Scan(&result)
	if err != nil {
		if isNotNumericErr(err) {
			return 0, pkgcore.ErrNotNumeric
		}
		return 0, fmt.Errorf("pkgcore/kv/postgres: incr: %w", err)
	}
	parsed, err := parseFloat(result)
	if err != nil {
		// The value this store's own arithmetic just wrote and read back in
		// the same round trip failed to parse: something outside this
		// store's control corrupted the row between the write and this
		// Scan (or numeric::text ever renders a form strconv cannot parse,
		// which the package's own tests guard against). Either way this is
		// not the documented ErrNotNumeric outcome -- that path never
		// reaches a value this store itself just wrote -- so it is reported
		// as a plain error rather than misclassified.
		return 0, fmt.Errorf("pkgcore/kv/postgres: incr: parse stored result %q: %w", result, err)
	}
	return parsed, nil
}

// IncrByFloatWithTTL implements pkgcore.KVStore.IncrByFloatWithTTL. See
// incrByFloatWithTTLSQL's own doc comment for the single-statement design and
// its atomicity argument.
func (s *Store) IncrByFloatWithTTL(ctx context.Context, key string, delta float64, ttl time.Duration) (float64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	deltaText := formatFloat(delta)

	var expiresAt *time.Time
	if ttl > kvNoExpiry {
		at := time.Now().Add(ttl)
		expiresAt = &at
	}

	var result []byte
	err := s.pool.QueryRow(ctx, incrByFloatWithTTLSQL, key, deltaText, expiresAt).Scan(&result)
	if err != nil {
		if isNotNumericErr(err) {
			return 0, pkgcore.ErrNotNumeric
		}
		return 0, fmt.Errorf("pkgcore/kv/postgres: incr with ttl: %w", err)
	}
	parsed, err := parseFloat(result)
	if err != nil {
		return 0, fmt.Errorf("pkgcore/kv/postgres: incr with ttl: parse stored result %q: %w", result, err)
	}
	return parsed, nil
}

// CompareAndSwap implements pkgcore.KVStore.CompareAndSwap. See
// compareAndSwapSQL's own doc comment for the single-statement design and
// its atomicity argument.
func (s *Store) CompareAndSwap(ctx context.Context, key string, old, newVal []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	var swapped int64
	err := s.pool.QueryRow(ctx, compareAndSwapSQL, key, normalizeValue(old), normalizeValue(newVal)).Scan(&swapped)
	if err != nil {
		return false, fmt.Errorf("pkgcore/kv/postgres: cas: %w", err)
	}
	return swapped > 0, nil
}

// Sweep physically removes every row whose expiry has already passed and
// reports how many rows it removed.
//
// This is the periodic-sweep half of this store's expiry story (the
// migration file's own doc comment on expires_at has the full picture):
// every read and write already treats an expired row as absent (the
// "IS NULL OR ... > now()" guard on getSQL, incrByFloatSQL and
// compareAndSwapSQL), so correctness never depends on Sweep running at all
// or on any particular schedule -- it exists purely so a row nothing ever
// reads again (a spent idempotency key, an expired lock nobody re-acquires)
// does not occupy space and an index entry forever. NewKVStore does not
// call it and owns no goroutine of its own that would; a host runs it on
// whatever schedule fits its own operational tooling (a cron job, a
// go/jobs-scheduled periodic task, a plain time.Ticker loop it owns), the
// same "an explicit, separate step" discipline EnsureSchema's own doc
// comment describes for schema application.
//
// Sweep is safe to call concurrently with itself, from any number of
// replicas, and with every other operation this store performs: it is a
// plain DELETE with no transaction of its own beyond what the single
// statement already gets, and a row it is racing to delete against a
// concurrent write to the same key is resolved by the database exactly like
// any other concurrent write to that row.
func (s *Store) Sweep(ctx context.Context) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	tag, err := s.pool.Exec(ctx, sweepSQL)
	if err != nil {
		return 0, fmt.Errorf("pkgcore/kv/postgres: sweep: %w", err)
	}
	return tag.RowsAffected(), nil
}

// normalizeValue returns v unchanged if it is already non-nil, or a
// non-nil, zero-length slice if v is nil.
//
// This matters for exactly one reason: every SQL statement in this file
// tests "old is empty" (set-if-absent, and the expired-row-matches-empty
// rule) with octet_length($n) = 0, and the pgx driver binds a nil []byte
// argument as SQL NULL rather than an empty bytea value -- octet_length(NULL)
// is NULL, which is neither true nor false in a WHERE clause, so a genuinely
// nil old (or newVal, for the identical reason on the value column's own NOT
// NULL constraint) would silently fail to match anything at all rather than
// behaving as the interface's own "nil or zero length" wording promises.
// Normalizing here, once, before any parameter is bound, is what makes a Go
// nil and a Go []byte{} behave identically everywhere in this file, matching
// pkgcore.memoryKVStore's own bytes.Equal-based treatment of the two as
// interchangeable.
func normalizeValue(v []byte) []byte {
	if v == nil {
		return []byte{}
	}
	return v
}

// formatFloat renders delta as its shortest exact decimal text, the same
// encoding pkgcore.memoryKVStore's own IncrByFloat uses for a freshly
// created counter -- see incrByFloatSQL's own doc comment for why this text
// form, rather than a raw float8 parameter, is what keeps the numeric
// cast's rendering free of binary-to-decimal noise.
func formatFloat(delta float64) string {
	return strconv.FormatFloat(delta, kvFloatFormat, kvFloatPrecisionShortest, kvFloatBitSize)
}

// parseFloat parses raw -- the bytea column's own text-shaped content, per
// this store's IncrByFloat/CompareAndSwap contract -- into a float64, the
// value KVStore.IncrByFloat returns to its caller.
func parseFloat(raw []byte) (float64, error) {
	return strconv.ParseFloat(string(raw), kvFloatBitSize)
}

// isNotNumericErr reports whether err is the PostgreSQL server's own
// "the value in this column is not a valid number" answer to
// incrByFloatSQL's convert_from/::numeric cast -- either the value's bytes
// are not valid UTF-8 text at all, or they are valid text that is not a
// valid decimal number. Both are the server's own fixed English error
// wording, which travels unchanged through any client driver (not a
// driver-specific error code), which is why a substring match on it is a
// coupling to PostgreSQL's own message text rather than to jackc/pgx --
// the identical acceptance kv/redis.kv.go's own IncrByFloat documents for
// its own substring match against go-redis's error text.
func isNotNumericErr(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "invalid input syntax for type numeric") ||
		strings.Contains(msg, "invalid byte sequence for encoding")
}
