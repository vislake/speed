-- Adds RoleBinding's dbkit.SoftDeletable pair (deleted_at/deleted_by) to
-- rbac_role_bindings, following dbkit's own soft-delete design
-- (docs/internal/04-data-and-tenancy.md, delete-semantics section): a
-- mark-delete is a plain UPDATE setting these two columns, never a physical
-- DELETE, and both columns stay dialect-portable (kept identical to the
-- sqlite/ copy of this file) -- no PostgreSQL-only type, no
-- gen_random_uuid(), no NOW().
--
-- RoleBinding is the only one of rbac's three models this round touches.
-- Service.RevokeRole's bindings.Delete(ctx, binding.ID) call is the only
-- real delete-shaped operation this module has ever had -- rbac.Role has no
-- DefineRole-adjacent delete path to retrofit at all today (see
-- go/rbac/AGENTS.md's "Soft deletion" section for the full scope note), so
-- rbac_roles and rbac_role_permissions are untouched by this migration.
--
-- Unlike examples/reference-app/internal/notes' identically-purposed
-- migration, rbac_role_bindings carries a real unique-index interaction
-- (see go/org/migrations/{sqlite,postgres}/0004_add_soft_delete.sql and
-- go/pki/migrations/{sqlite,postgres}/0001_create_pki_signing_keys.sql's
-- uq_pki_signing_keys_active_purpose for the identical technique, used
-- there for a different reason): a soft-deleted row still occupies whatever
-- unique constraint it held while live, so without a change here a revoked
-- binding's (tenant, user, role, node) tuple would stay permanently
-- unavailable for a fresh AssignRole -- a real functional regression, not
-- merely a cosmetic one. The existing full unique index is therefore
-- replaced by its WHERE-scoped, "live rows only" equivalent: dropped and
-- re-created under the SAME name, so every other reference to it (the
-- unique-index collision handling in assign.go's AssignRole, and this
-- module's own tests) needs no change. This is standard SQL, not a
-- PostgreSQL-specific feature -- the identical DDL string runs on SQLite
-- too (see the sqlite/ sibling).
--
-- This migration deliberately does not touch rbac_roles or
-- rbac_role_permissions -- see this round's own scope note in
-- go/rbac/AGENTS.md's "Soft deletion" section for why.
ALTER TABLE rbac_role_bindings ADD COLUMN deleted_at TIMESTAMP NULL;
ALTER TABLE rbac_role_bindings ADD COLUMN deleted_by VARCHAR(64) NOT NULL DEFAULT '';

-- A binding is unique among LIVE rows only: revoking a grant frees its
-- (tenant, user, role, node) tuple immediately for a fresh AssignRole,
-- rather than reserving it forever for a row nobody can see.
DROP INDEX uq_rbac_role_bindings_tenant_user_role_node;
CREATE UNIQUE INDEX uq_rbac_role_bindings_tenant_user_role_node
    ON rbac_role_bindings (tenant_id, user_id, role_id, node_id)
    WHERE deleted_at IS NULL;
