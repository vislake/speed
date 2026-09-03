-- Adds notes' dbkit.SoftDeletable pair (deleted_at/deleted_by), following
-- dbkit's own soft-delete design (docs/internal/04-data-and-tenancy.md,
-- delete-semantics section): a mark-delete is a plain UPDATE setting these two columns,
-- never a physical DELETE, and both columns stay dialect-portable (kept
-- identical to the postgres/ copy of this file) -- no PostgreSQL-only
-- type, no gen_random_uuid(), no NOW().
--
-- No unique-index interaction to fold deleted_at into here: notes has no
-- uniqueness constraint of its own (see 0001_create_notes.sql), unlike
-- go/org's UNIQUE(tenant_id, parent_id, name), the design doc's own named
-- example for that interaction. See go/dbkit/AGENTS.md's "Soft deletion"
-- section for the general partial-unique-index guidance a future model
-- that does need one should follow.
--
-- This migration deliberately does not add anything for HardDelete (the
-- irreversible, system-context-gated compliance-erasure path the design
-- doc's own §3 describes) -- that remains out of scope for this round,
-- left for a future one.
ALTER TABLE notes ADD COLUMN deleted_at TIMESTAMP NULL;
ALTER TABLE notes ADD COLUMN deleted_by VARCHAR(64) NOT NULL DEFAULT '';
