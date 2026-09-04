-- billing_plans is billing's Plan/Feature/Entitlement domain table
-- (go/billing/plan.go). Deliberately dual-domain: a platform-wide plan
-- (tenant_id = '') is platform data, visible to any tenant's Resolve
-- lookup, while a tenant-custom plan (tenant_id set) is that one tenant's
-- private data -- see Plan's own doc comment for why this table is
-- therefore NOT dbkit.TenantScoped and is reached through a plain
-- *gorm.DB (PlanStore), the identical (key, scope, tenant_id) duality
-- go/config's own "configs" table already establishes for the same
-- reason.
--
-- tenant_id is NOT NULL, holding the empty-string sentinel on a
-- platform-wide row -- empty rather than NULL for the same reason
-- go/config's row and go/org's org_nodes.parent_id both document: NULLs
-- are distinct in a PostgreSQL unique index, so two platform rows for one
-- key could coexist under NULL where the empty string collapses them into
-- the one row the (tenant_id, plan_key) unique index promises.
--
-- id is an application-generated UUID (see plan.go's PlanStore.Create),
-- never a database-generated one. grants holds the plan's []Grant slice
-- as portable JSON text (Plan.Grants/SetGrants) -- no native PostgreSQL
-- JSONB with operator filtering, per the dual-dialect rule.
--
-- This is the SQLite copy; see the postgres/ sibling for the identical
-- schema on that dialect.
CREATE TABLE billing_plans (
    id         VARCHAR(36)  NOT NULL,
    tenant_id  VARCHAR(64)  NOT NULL DEFAULT '',
    plan_key    VARCHAR(100) NOT NULL,
    name       VARCHAR(200) NOT NULL,
    price_cents BIGINT      NOT NULL,
    currency   VARCHAR(3)   NOT NULL,
    billing_interval VARCHAR(16) NOT NULL,
    grants     TEXT         NOT NULL DEFAULT '',
    created_at TIMESTAMP    NOT NULL,
    updated_at TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);

-- The lookup-precedence unique index PlanStore.Resolve relies on: at most
-- one row per (tenant_id, plan_key) pair, so "the platform-wide plan for key"
-- and "tenant X's custom plan for key" are each well-defined.
CREATE UNIQUE INDEX uq_billing_plans_tenant_key ON billing_plans (tenant_id, plan_key);
