-- admin_tenants is admin's operator-facing tenant ledger (go/admin/model.go,
-- D3): platform data -- it does NOT carry a tenant_id column scoping it to
-- one tenant, since it describes every tenant the platform knows about. Its
-- isolation is proven by tenancytest.AssertNotTenantScoped, never
-- AssertIsolated.
--
-- This is the PostgreSQL copy; see the sqlite/ sibling for the identical
-- schema on that dialect. Kept portable on purpose: no dialect-specific
-- types, no native arrays, no JSONB, no gen_random_uuid(), no NOW().
CREATE TABLE admin_tenants (
    tenant_id          VARCHAR(64)   NOT NULL,
    display_name       VARCHAR(255)  NOT NULL DEFAULT '',
    status             VARCHAR(32)   NOT NULL,
    suspended_reason   VARCHAR(2000) NOT NULL DEFAULT '',
    suspended_at       TIMESTAMP,
    created_at         TIMESTAMP     NOT NULL,
    created_by         VARCHAR(64)   NOT NULL DEFAULT '',
    notes              VARCHAR(4000) NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id)
);

-- The listing index behind TenantRepository.List's optional status filter.
CREATE INDEX idx_admin_tenants_status ON admin_tenants (status);
