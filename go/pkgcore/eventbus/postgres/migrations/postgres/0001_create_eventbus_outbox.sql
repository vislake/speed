-- pkgcore_eventbus_outbox and pkgcore_eventbus_cursor back the
-- eventbus/postgres implementation of pkgcore.EventBus (see the parent
-- package's eventbus.go for the full delivery-semantics story). Unlike
-- every other migration in this codebase, this file has no SQLite
-- counterpart on purpose: LISTEN/NOTIFY, and the durable outbox built on
-- top of it, are inherently PostgreSQL-specific -- the standalone
-- deployment mode never runs this implementation at all, since
-- pkgcore.NewMemoryEventBus already covers it with no database of its
-- own. This follows go/pki's signer/vault and signer/kmsaws subpackages,
-- which ship no migrations whatsoever for the identical reason: a
-- mechanism with no cross-dialect meaning carries no cross-dialect
-- migration. See migrations/fs.go for why this directory has no sibling
-- "sqlite/" the way every dual-dialect module's migrations/ does.
--
-- pkgcore_eventbus_outbox is the durable record of every Publish call.
-- NOTIFY alone cannot replay a notification a session missed while
-- disconnected or not yet listening -- PostgreSQL's own documentation is
-- explicit that there is no such replay -- so this table is the source of
-- truth NOTIFY only ever nudges a reader toward; Publish writes the row
-- and issues NOTIFY inside the very same transaction (see
-- insertOutboxAndNotify in outbox.go), so the two either both happen or
-- neither does.
--
-- id is a database-generated, strictly increasing identity column --
-- deliberately NOT an application-generated ULID/UUID the way every
-- tenant-facing table in this codebase is required to be (root
-- CLAUDE.md's migrations section) -- because the catch-up mechanism's
-- whole job is answering "everything after row N for this event type",
-- which needs a single total ordering source shared by every concurrent
-- publisher; only a database sequence gives that without the publishers
-- coordinating amongst themselves. This table is infrastructure
-- bookkeeping for the bus itself, not a business record, so the
-- generate-IDs-in-the-application rule does not bind it the way it binds
-- a domain table.
--
-- payload is stored as TEXT, never a native JSONB column with operator
-- filtering, mirroring go/dbkit/audit's audit_events.changes column
-- exactly (see that migration's own doc comment): a row here is always
-- read back whole and JSON-decoded in application code, never queried
-- inside its payload column, so a portable, simple TEXT column already
-- does everything this table needs from it.
-- IF NOT EXISTS guards on both the table and the index are deliberate: this
-- file carries no schema_migrations-style ledger of its own (see
-- schema.go's EnsureSchema doc comment), so every statement here must
-- tolerate being re-run, by the same replica or a concurrent one, forever.
CREATE TABLE IF NOT EXISTS pkgcore_eventbus_outbox (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_type VARCHAR(255) NOT NULL,
    tenant_id  VARCHAR(64)  NOT NULL DEFAULT '',
    payload    TEXT         NOT NULL,
    created_at TIMESTAMP    NOT NULL
);

-- The catch-up read path's own query shape: "rows of this event_type with
-- id greater than my watermark", ordered by id.
CREATE INDEX IF NOT EXISTS idx_pkgcore_eventbus_outbox_type_id ON pkgcore_eventbus_outbox (event_type, id);

-- pkgcore_eventbus_cursor is one row per (replica, event type): the
-- durable watermark that lets a specific, caller-named replica -- the
-- replicaID NewEventBus is constructed with -- resume catch-up exactly
-- where it left off after a restart, rather than from the live end the
-- way a fresh Redis consumer group would (see eventbus.go's
-- delivery-semantics note on the difference this makes, and on why a
-- caller-supplied, stable-across-restarts replicaID is what makes the
-- difference possible at all). A replica's cursor row is private to it --
-- two replicas never share one, and this table enforces nothing about how
-- many replicas exist or what their ids are. A replica retired for good
-- leaves its row behind as harmless bookkeeping; removing it is an
-- operator task outside this table's own scope, exactly like the
-- abandoned-consumer-group cleanup eventbus/redis's own Close doc comment
-- describes for its sibling implementation.
CREATE TABLE IF NOT EXISTS pkgcore_eventbus_cursor (
    replica_id        VARCHAR(255) NOT NULL,
    event_type        VARCHAR(255) NOT NULL,
    last_delivered_id BIGINT       NOT NULL,
    updated_at        TIMESTAMP    NOT NULL,
    PRIMARY KEY (replica_id, event_type)
);
