-- integration_api_key_hash_index holds the narrow, deliberately
-- non-tenant-scoped hash -> tenant_id mapping go/integration/model.go's
-- apiKeyHashIndex documents in full: platform data, never
-- dbkit.TenantScoped, reached only through dbkit.Open()'s plain *gorm.DB
-- (repository.go's (*APIKeyRepository).tenantForHash and
-- createWithHashIndex), never through dbkit.Repository[T]. Isolation proven
-- by tenancytest.AssertNotTenantScoped, not AssertIsolated.
--
-- This is the mechanism that closes the gap keygen.go's own hashAPIKeyToken
-- doc comment named ("go/integration ships no Authenticate/Verify method
-- yet ... there is no lookup path today that could ever feed this function
-- attacker-influenced input") -- Service.Authenticate is that lookup path,
-- and this table is how it resolves a tenant from a raw presented key
-- alone, mirroring go/sharing's sharing_token_index table exactly. See
-- model.go's apiKeyHashIndex doc comment for the full "why a new table"
-- argument, including why 0001's own uq_integration_api_keys_tenant_hash
-- comment's original assumption ("every lookup already knows its tenant")
-- did not survive this round's real design need.
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
