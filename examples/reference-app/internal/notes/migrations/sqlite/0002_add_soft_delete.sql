-- Adds notes' dbkit.SoftDeletable pair (deleted_at/deleted_by): a
-- mark-delete is a plain UPDATE setting these two columns, never a
-- physical DELETE. Both columns stay dialect-portable (kept identical to
-- the postgres/ copy of this file) -- no PostgreSQL-only type, no
-- gen_random_uuid(), no NOW().
--
-- No unique-index interaction to fold deleted_at into here: notes has no
-- uniqueness constraint of its own (see 0001_create_notes.sql).
--
-- HardDelete (the irreversible, system-context-gated compliance-erasure
-- path) adds no DDL of its own: it is a physical DELETE the schema
-- already permits.
ALTER TABLE notes ADD COLUMN deleted_at TIMESTAMP NULL;
ALTER TABLE notes ADD COLUMN deleted_by VARCHAR(64) NOT NULL DEFAULT '';
