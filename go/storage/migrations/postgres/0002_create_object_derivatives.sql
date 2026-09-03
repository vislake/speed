-- object_derivatives is the storage module's rendition table
-- (go/storage/model.go): one row per derived rendition of a completed
-- Object -- in this round, exactly the thumbnail the derive pipeline
-- produces. The rendition's bytes live in the ObjectStore under the
-- internal key this row names, like the original's.
--
-- This is the PostgreSQL copy of the migration; the SQLite sibling carries
-- the identical shape with INTEGER standing in for BIGINT.
--
-- object_id is a plain id reference, deliberately not a foreign key:
-- cross-table foreign keys make independently released migrations and
-- cascading deletes unmanageable (root CLAUDE.md's own rule), so the
-- referential integrity between an Object and its derivatives is maintained
-- by LifecycleService, which deletes an object's derivatives when it
-- deletes the object.
--
-- The unique index is the idempotent-skip backstop of the derive pipeline:
-- a tenant holds at most one derivative of a given kind per object, so a
-- re-derive (a completion retried after a crash, say) is a no-op instead of
-- a duplicate row. Its leftmost columns also serve the delete cascade's
-- per-object scan, WHERE tenant_id = ? AND object_id = ?. There is no
-- (tenant_id, created_at) index here: nothing lists derivatives this round,
-- and the objects cursor index lives on the objects table, where the
-- objects listing actually scans -- see 0001's header note.
CREATE TABLE object_derivatives (
    id         VARCHAR(36)  NOT NULL,
    tenant_id  VARCHAR(64)  NOT NULL,
    object_id  VARCHAR(36)  NOT NULL,
    kind       VARCHAR(32)  NOT NULL,
    key        VARCHAR(512) NOT NULL,
    mime       VARCHAR(255) NOT NULL DEFAULT '',
    size       BIGINT       NOT NULL,
    width      INTEGER      NULL,
    height     INTEGER      NULL,
    created_at TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);

CREATE UNIQUE INDEX uq_object_derivatives_object_kind
    ON object_derivatives (tenant_id, object_id, kind);
