-- Adds OrgNode's and Membership's dbkit.SoftDeletable pair (deleted_at/
-- deleted_by) to org_nodes and memberships, following dbkit's own
-- soft-delete design (docs/internal/04-data-and-tenancy.md, delete-semantics
-- section): a mark-delete is a plain UPDATE setting these two columns, never
-- a physical DELETE, and both columns stay dialect-portable (kept identical
-- to the postgres/ copy of this file) -- no PostgreSQL-only type, no
-- gen_random_uuid(), no NOW().
--
-- Accidental deletion of a sub-org (cascade included) or of a membership is
-- exactly the "oops, get it back" scenario mark-delete exists for -- see
-- go/org/AGENTS.md's "Soft deletion" section for the full round.
--
-- Unlike examples/reference-app/internal/notes' identically-purposed
-- migration, org_nodes and memberships both carry a real unique-index
-- interaction the design doc names by name
-- (docs/internal/04-data-and-tenancy.md, delete-semantics section, §4; see
-- also go/dbkit/AGENTS.md's "Soft deletion" section and
-- go/pki/migrations/{sqlite,postgres}/0001_create_pki_signing_keys.sql's
-- uq_pki_signing_keys_active_purpose for the identical technique used there
-- for a different reason): a soft-deleted row still occupies whatever
-- unique constraint it held while live, so without a change here a
-- soft-deleted sibling name or a soft-deleted membership's seat would stay
-- permanently unavailable -- a real functional regression, not merely a
-- cosmetic one. Both existing full unique indexes are therefore replaced by
-- their WHERE-scoped, "live rows only" equivalents: dropped and re-created
-- under the SAME index name, so every other reference to that name (error
-- mapping via gorm.ErrDuplicatedKey, this module's own tests) needs no
-- change. SQLite has supported partial indexes since 3.8.0, and the
-- rewrite is standard SQL, not a PostgreSQL-only feature.
--
-- This migration deliberately does not touch org_invitations -- see this
-- round's own scope note in go/org/AGENTS.md's "Soft deletion" section for
-- why invitations were left out.
ALTER TABLE org_nodes ADD COLUMN deleted_at TIMESTAMP NULL;
ALTER TABLE org_nodes ADD COLUMN deleted_by VARCHAR(64) NOT NULL DEFAULT '';

ALTER TABLE memberships ADD COLUMN deleted_at TIMESTAMP NULL;
ALTER TABLE memberships ADD COLUMN deleted_by VARCHAR(64) NOT NULL DEFAULT '';

-- Sibling names are unique among LIVE rows only: a soft-deleted node's name
-- becomes reusable immediately, rather than staying reserved until some
-- future hard-delete.
DROP INDEX uq_org_nodes_sibling_name;
CREATE UNIQUE INDEX uq_org_nodes_sibling_name
    ON org_nodes (tenant_id, parent_id, name)
    WHERE deleted_at IS NULL;

-- One LIVE seat per person per tenant: a removed member's seat frees up
-- immediately for a fresh MemberService.Add, instead of staying reserved by
-- a row nobody can see.
DROP INDEX uq_memberships_tenant_user;
CREATE UNIQUE INDEX uq_memberships_tenant_user
    ON memberships (tenant_id, user_id)
    WHERE deleted_at IS NULL;
