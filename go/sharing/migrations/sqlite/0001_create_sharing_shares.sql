-- sharing_shares holds public share links (go/sharing/model.go): tenant
-- data -- a share link belongs to the tenant whose resource it exposes.
-- Share implements dbkit.TenantScoped and is reached only through
-- ShareRepository (embedding dbkit.Repository[Share]); isolation proven by
-- tenancytest.AssertIsolated.
--
-- This is the SQLite copy; see the postgres/ sibling for the full rationale
-- of every column. The dialect differences stop at the allowed SQL surface:
-- no dialect-specific types, no native arrays, no JSONB, no
-- gen_random_uuid(), no NOW().
CREATE TABLE sharing_shares (
    id             VARCHAR(36)  NOT NULL,
    tenant_id      VARCHAR(64)  NOT NULL,
    resource_ref   VARCHAR(512) NOT NULL,
    token_hash     VARCHAR(64)  NOT NULL,
    expires_at     TIMESTAMP    NOT NULL,
    max_views      INTEGER,
    view_count     INTEGER      NOT NULL DEFAULT 0,
    password_hash  VARCHAR(255),
    sensitive      BOOLEAN      NOT NULL DEFAULT FALSE,
    revoked_at     TIMESTAMP,
    created_at     TIMESTAMP    NOT NULL,
    updated_at     TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);

-- The bearer-token lookup Service.Access runs on every call. Unique because
-- a token is drawn from 256 bits of crypto/rand -- a collision is
-- cryptographically negligible, and the constraint is cheap insurance, not
-- a meaningful global-uniqueness requirement this module actively defends.
CREATE UNIQUE INDEX uq_sharing_shares_token_hash ON sharing_shares (token_hash);

-- The expiry-sweep listing (cleanup.go's ShareRepository.listExpiredOrExhausted)
-- leads with tenant_id, per this codebase's own convention for tenant-scoped
-- tables.
CREATE INDEX idx_sharing_shares_tenant_expires_at ON sharing_shares (tenant_id, expires_at);
