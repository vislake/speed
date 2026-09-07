-- The index behind MemberService.TenantsOf (go/org/membership.go): the
-- cross-tenant "which tenants does this user belong to" read, answered
-- under a system context by one lookup on user_id instead of by a scan.
--
-- Every index on memberships so far leads with tenant_id
-- (uq_memberships_tenant_user, idx_memberships_tenant_node,
-- idx_memberships_tenant_status) because every other read of the table
-- is a tenant-scoped one the isolation plugin filters first. TenantsOf
-- is the deliberate exception: the question it answers spans tenants by
-- definition (a person's memberships across every organization), so its
-- statement carries no tenant filter at all and needs its own index
-- with user_id leftmost -- the one sanctioned composite index in this
-- codebase whose leading column is not tenant_id, and the reason is the
-- query itself, not an oversight.
--
-- The index is partial (WHERE deleted_at IS NULL), exactly like the
-- live-rows-only unique index 0004_add_soft_delete.sql created: the
-- lookup filters out soft-deleted memberships, so a row a Remove hid
-- must neither be scanned nor kept in the index. tenant_id is the
-- second column so the ordered scan the read's ORDER BY tenant_id needs
-- is the index's own order.
--
-- This is the postgres copy; the sqlite/ sibling carries the same
-- schema and the same rationale. Nothing here is dialect-specific.

CREATE INDEX idx_memberships_user_tenant
    ON memberships (user_id, tenant_id)
    WHERE deleted_at IS NULL;
