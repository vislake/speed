-- integration_api_keys is go/integration's round-1 table (go/integration/
-- model.go): one row per API key a tenant has issued for programmatic
-- access to its own data. Tenant data
-- (docs/internal/04-data-and-tenancy.md) -- a key belongs to exactly one
-- tenant and must never be visible from another -- so its isolation is
-- proven by tenancytest.AssertIsolated, never AssertNotTenantScoped.
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
-- of one tenant) needs tenant_id indexed; this also supports an
-- authentication-time lookup by hash -- a later round's "authenticate a
-- request with a key" path (AGENTS.md's Deferred list) -- scoped by tenant
-- first since every lookup this module ever performs already knows its
-- tenant from request context. A hash is 32 bytes of crypto/rand entropy
-- (keygen.go), so collisions across tenants are not a practical concern
-- this index needs to prevent, but scoping it by tenant regardless keeps
-- every index on this table consistent with "tenant first" and avoids a
-- table-wide unique constraint that would leak, through a duplicate-key
-- error alone, whether a given hash exists under some OTHER tenant.
CREATE UNIQUE INDEX uq_integration_api_keys_tenant_hash
    ON integration_api_keys (tenant_id, hash);

-- The expiry-sweep index a later round's cleanup job will read (AGENTS.md's
-- Deferred list); added now, ahead of that job existing, following the same
-- "get the table shape right the first time" instruction go/pki/migrations'
-- own not_after index documents for the identical situation.
CREATE INDEX idx_integration_api_keys_tenant_expires_at
    ON integration_api_keys (tenant_id, expires_at);
