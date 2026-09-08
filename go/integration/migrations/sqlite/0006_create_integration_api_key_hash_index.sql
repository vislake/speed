-- integration_api_key_hash_index holds the narrow, deliberately
-- non-tenant-scoped hash -> tenant_id mapping go/integration/model.go's
-- apiKeyHashIndex documents in full: platform data, never
-- dbkit.TenantScoped, reached only through dbkit.Open()'s plain *gorm.DB
-- (repository.go's (*APIKeyRepository).tenantForHash and
-- createWithHashIndex), never through dbkit.Repository[T]. Isolation proven
-- by tenancytest.AssertNotTenantScoped, not AssertIsolated.
--
-- Service.Authenticate is the lookup path that feeds hashAPIKeyToken a
-- presented key, and this table is how it resolves a tenant from a raw
-- presented key alone, before any tenant is known at all, mirroring
-- go/sharing's sharing_token_index table exactly; model.go's apiKeyHashIndex
-- doc comment carries the full "why a new table" argument.
--
-- This is the SQLite copy; see the postgres/ sibling for the full
-- rationale. The dialect differences stop at the allowed SQL surface: no
-- dialect-specific types, no native arrays, no JSONB, no
-- gen_random_uuid(), no NOW().
CREATE TABLE integration_api_key_hash_index (
    hash      VARCHAR(64) NOT NULL,
    tenant_id VARCHAR(64) NOT NULL,
    PRIMARY KEY (hash)
);
