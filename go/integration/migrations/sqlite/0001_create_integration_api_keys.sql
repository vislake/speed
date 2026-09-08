-- integration_api_keys is go/integration's core API-key table
-- (go/integration/model.go): one row per API key a tenant has issued for
-- programmatic access to its own data. Tenant data -- a key belongs to
-- exactly one tenant and must never be visible from another -- so its
-- isolation is proven by tenancytest.AssertIsolated, never
-- AssertNotTenantScoped.
--
-- The primary key is (id) alone, matching go/storage's Object precedent
-- (see go/storage/model.go's own "Primary key" doc comment section, which
-- go/integration/model.go's APIKey.ID doc comment points back to): id is an
-- application-generated UUID (go/integration/service.go's
-- Service.Create, uuid.NewString), globally unique on its own, so tenant_id
-- rides along as a plain, non-key column (promoted by the embedded
-- dbkit.TenantModel) rather than joining a composite key.
--
-- This is the SQLite copy; see the postgres/ sibling for the full rationale
-- of every column. The dialect differences stop at the allowed SQL surface:
-- no dialect-specific types, no native arrays, no JSONB, no
-- gen_random_uuid(), no NOW().
CREATE TABLE integration_api_keys (
    id           VARCHAR(36)  NOT NULL,
    tenant_id    VARCHAR(64)  NOT NULL,
    prefix       VARCHAR(32)  NOT NULL,
    hash         VARCHAR(64)  NOT NULL,
    scopes       TEXT         NOT NULL,
    created_by   VARCHAR(64)  NOT NULL,
    expires_at   TIMESTAMP    NOT NULL,
    last_used_at TIMESTAMP,
    revoked_at   TIMESTAMP,
    created_at   TIMESTAMP    NOT NULL,
    updated_at   TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);

-- The tenant-scoped listing query behind Repository[APIKey].List (every key
-- of one tenant) needs tenant_id indexed; the unique (tenant_id, hash)
-- pair keeps every index on this table consistent with "tenant first". A
-- hash is 32 bytes of crypto/rand entropy (keygen.go), so collisions
-- across tenants are not a practical concern, but scoping the uniqueness
-- by tenant avoids a table-wide unique constraint that would leak, through
-- a duplicate-key error alone, whether a given hash exists under some
-- OTHER tenant. Request-time authentication does NOT read through this
-- index: it resolves the tenant from the hash alone through
-- integration_api_key_hash_index (migration 0006, model.go's
-- apiKeyHashIndex doc comment), then reads this table's row under that
-- resolved tenant.
CREATE UNIQUE INDEX uq_integration_api_keys_tenant_hash
    ON integration_api_keys (tenant_id, hash);

-- The expiry-sweep index no periodic job reads yet (no expiry sweep
-- exists -- see the module's Known limitations); it exists so such a job
-- needs no migration of its own, following the same "get the table shape
-- right the first time" instruction go/pki/migrations' own not_after index
-- documents for the identical situation.
CREATE INDEX idx_integration_api_keys_tenant_expires_at
    ON integration_api_keys (tenant_id, expires_at);
