-- admin_impersonation_grants is admin's impersonation authorization
-- credential ledger (go/admin/model.go, D5): platform data, no tenant_id
-- column scoping the TABLE itself (target_tenant_id is a data column
-- naming the tenant a grant is scoped to, not a filter column). Its
-- isolation is proven by tenancytest.AssertNotTenantScoped.
--
-- This is the SQLite copy; see the postgres/ sibling for the identical
-- schema on that dialect.
--
-- id is the grant's own credential value: a random, unguessable identifier
-- (impersonation_repository.go's newGrantID), never derived from
-- admin_user_id or target_user_id.
CREATE TABLE admin_impersonation_grants (
    id                 VARCHAR(36)  NOT NULL,
    admin_user_id      VARCHAR(64)  NOT NULL,
    target_user_id     VARCHAR(64)  NOT NULL,
    target_tenant_id   VARCHAR(64)  NOT NULL,
    reason             VARCHAR(2000) NOT NULL DEFAULT '',
    created_at         TIMESTAMP    NOT NULL,
    expires_at         TIMESTAMP    NOT NULL,
    ended_at           TIMESTAMP,
    ended_by           VARCHAR(64)  NOT NULL DEFAULT '',
    PRIMARY KEY (id)
);

-- ImpersonationMiddleware's Lookup path reads by id (the primary key
-- already covers that); these three secondary indexes back
-- ImpersonationRepository.ListActive's "ended_at IS NULL AND expires_at >
-- ?" scan and any future per-actor listing.
CREATE INDEX idx_admin_impersonation_grants_admin_user_id ON admin_impersonation_grants (admin_user_id);
CREATE INDEX idx_admin_impersonation_grants_target_user_id ON admin_impersonation_grants (target_user_id);
CREATE INDEX idx_admin_impersonation_grants_target_tenant_id ON admin_impersonation_grants (target_tenant_id);
CREATE INDEX idx_admin_impersonation_grants_active ON admin_impersonation_grants (ended_at, expires_at);
