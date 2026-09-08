-- Narrows uq_org_nodes_single_root -- the one-root-per-tenant index 0007
-- adds -- to LIVE rows only: 0007's
-- predicate (parent_id = '') admits every root-shaped row, soft-deleted
-- ones included, so a mark-deleted root keeps occupying its tenant's single
-- root slot. CreateRoot's insert then collides with an invisible row on
-- every attempt -- translated into org.root_already_exists -- and a tenant
-- whose root has been removed (through the exported Repository surface;
-- TreeService.Delete itself refuses the root, so no org-level path can
-- restore the slot) has no way back to a tree: unrecoverable tenant state.
--
-- Every unique index over a soft-deletable row counts live rows only, the
-- convention this module's other migrations already follow: 0004's
-- uq_org_nodes_sibling_name and uq_memberships_tenant_user are both scoped
-- WHERE deleted_at IS NULL. The index is dropped and
-- re-created under the SAME name, exactly as 0004 did, so every other
-- reference to the name (error mapping via gorm.ErrDuplicatedKey, this
-- module's own tests and Restore's slot-reuse handling in tree.go) needs no
-- change.
--
-- With the narrowed index a soft-deleted root's slot frees immediately: a
-- fresh CreateRoot succeeds for the tenant, and TreeService.Restore of the
-- old root then collides at the database with the new live root and answers
-- the coded org.duplicate_sibling_name mapWriteError gives every other
-- slot-reuse collision -- the root-slot reuse tree.go's Restore doc comment
-- already promises, and which 0007 as shipped could not deliver.
--
-- A database that already carries two live root rows for one tenant fails
-- this CREATE INDEX loudly rather than being silently repaired; such a
-- database could not have applied 0007 either, which fails on the identical
-- condition.
--
-- This is the postgres/ copy; the sqlite/ sibling carries the full
-- rationale. The two are byte-identical -- partial unique indexes with an
-- extra boolean-typed predicate are standard SQL both dialects support with
-- the same syntax.
DROP INDEX uq_org_nodes_single_root;
CREATE UNIQUE INDEX uq_org_nodes_single_root
    ON org_nodes (tenant_id)
    WHERE parent_id = '' AND deleted_at IS NULL;
