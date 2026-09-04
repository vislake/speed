-- sharing_token_index holds the narrow, deliberately non-tenant-scoped
-- token_hash -> tenant_id mapping go/sharing/model.go's shareTokenIndex
-- documents in full: platform data, never dbkit.TenantScoped, reached only
-- through dbkit.Open()'s plain *gorm.DB (repository.go's
-- (*ShareRepository).tenantForTokenHash and createWithTokenIndex), never
-- through dbkit.Repository[T]. Isolation proven by
-- tenancytest.AssertNotTenantScoped, not AssertIsolated.
--
-- This is the SQLite copy; see the postgres/ sibling for the full
-- rationale. The dialect differences stop at the allowed SQL surface: no
-- dialect-specific types, no native arrays, no JSONB, no gen_random_uuid(),
-- no NOW().
CREATE TABLE sharing_token_index (
    token_hash VARCHAR(64) NOT NULL,
    tenant_id  VARCHAR(64) NOT NULL,
    PRIMARY KEY (token_hash)
);
