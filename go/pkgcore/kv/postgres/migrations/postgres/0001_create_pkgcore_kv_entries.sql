-- pkgcore_kv_entries backs the kv/postgres implementation of pkgcore.KVStore
-- (see the parent package's kv.go for the full semantics story). Unlike
-- every other migration in this codebase, this file has no SQLite
-- counterpart on purpose: the standalone deployment mode never runs this
-- implementation at all, since pkgcore.NewMemoryKVStore already covers it
-- with no database of its own. This follows
-- go/pkgcore/eventbus/postgres/migrations/postgres's identical precedent and
-- go/pki's signer/vault and signer/kmsaws subpackages, which ship no
-- migrations whatsoever for the same reason: a mechanism with no
-- cross-dialect meaning carries no cross-dialect migration. See
-- migrations/fs.go for why this directory has no sibling "sqlite/" the way
-- every dual-dialect module's migrations/ does.
--
-- key is the opaque string pkgcore.KVStore.Get/Set/Delete/IncrByFloat/
-- CompareAndSwap key argument, never interpreted by this table. 1024 bytes
-- comfortably covers every key shape this codebase's own callers build
-- (go/ratelimit's dimension-prefixed counters, go/authn's lockout counters,
-- idempotency keys), and a key this table refuses because it is longer is
-- refused by the database rather than silently truncated.
--
-- value is stored as raw bytes, never as a native PostgreSQL TEXT or JSONB
-- column: the interface promises an opaque byte value except for the
-- numeric encoding IncrByFloat reads and writes, and a portable BYTEA column
-- carries both without caring which one a given row holds. NOT NULL: the
-- store's own Go code always normalizes a caller's nil value to an empty,
-- non-nil slice before it ever reaches a parameter placeholder (kv.go's own
-- doc comment on that normalization explains why), so a NULL in this column
-- would only ever mean a row this table's own writers never produced.
--
-- expires_at is the entry's absolute expiry: NULL means "never expires",
-- matching pkgcore's in-memory store's own zero-time convention. Every read
-- and write in kv.go filters on "expires_at IS NULL OR expires_at > now()"
-- (a live row) so an expired row is invisible without needing a delete to
-- have run first -- the same "drop lazily, when an operation next touches
-- the key" precedent pkgcore.memoryKVStore's own doc comment sets, chosen
-- over an eager per-write delete because it keeps every read and write to
-- one round trip. Unlike the in-memory store, this table's own dead rows do
-- not disappear merely because nothing ever reads them again (an
-- idempotency key set once and never re-read, for instance), so
-- kv/postgres.Store.Sweep is the periodic, one-shot counterpart a host runs
-- on its own schedule (a cron job, a ticker) to physically remove rows whose
-- expiry has already passed; the partial index below is that sweep's own
-- query shape, and skips indexing the (large, common) no-expiry rows
-- entirely since Sweep never needs to find them.
--
-- IF NOT EXISTS guards on both the table and the index are deliberate: this
-- file carries no schema_migrations-style ledger of its own (see
-- schema.go's EnsureSchema doc comment), so every statement here must
-- tolerate being re-run, by the same replica or a concurrent one, forever.
CREATE TABLE IF NOT EXISTS pkgcore_kv_entries (
    key        VARCHAR(1024) NOT NULL PRIMARY KEY,
    value      BYTEA         NOT NULL,
    expires_at TIMESTAMPTZ
);

-- Sweep's own query shape: "every row whose expiry has already passed".
-- Partial on "expires_at IS NOT NULL" so a no-TTL row -- the common case for
-- a value nobody ever intends to expire -- never occupies this index at all.
CREATE INDEX IF NOT EXISTS idx_pkgcore_kv_entries_expires_at ON pkgcore_kv_entries (expires_at) WHERE expires_at IS NOT NULL;
