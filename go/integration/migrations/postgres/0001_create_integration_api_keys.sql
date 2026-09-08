-- integration_api_keys is go/integration's core API-key table
-- (go/integration/model.go): one row per API key a tenant has issued for
-- programmatic access to its own data. Tenant data -- a key belongs to
-- exactly one tenant and must never be visible from another -- so its
-- isolation is proven by tenancytest.AssertIsolated, never
-- AssertNotTenantScoped.
--
-- The primary key is (id) alone, matching go/storage's Object precedent
-- (see go/storage/model.go's own "Primary key" doc comment section): id is
-- an application-generated UUID, globally unique on its own, so tenant_id
-- rides along as a plain, non-key column rather than joining a composite
-- key.
--
-- This is the PostgreSQL copy; see the sqlite/ sibling for the identical
-- schema on that dialect. Kept portable on purpose: no dialect-specific
-- types, no native arrays, no JSONB, no gen_random_uuid(), no NOW().
--
-- id is an application-generated UUID (go/integration/service.go's
-- Service.Create, uuid.NewString -- never a database default).
-- prefix is the
-- plaintext, non-secret display portion (go/integration/keygen.go);
-- hash is the hex-encoded SHA-256 of the raw key, which is never itself
-- stored anywhere -- see APIKey's own doc comment in model.go for why a
-- one-way digest of full-entropy randomness needs no further encryption.
--
-- scopes is a JSON array of permission strings, stored as TEXT rather than
-- a native PostgreSQL array or JSONB column with operator filtering, per
-- the dual-dialect rule -- this module only ever reads the column back
-- whole (go/integration/model.go's parseScopes), never filters on an
-- individual element inside SQL.
--
-- expires_at is mandatory: every key has a forced expiry
-- (Service.MaxAPIKeyLifetime caps it at creation). last_used_at and
-- revoked_at are both genuinely nullable: nil until first use / until
-- revoked respectively, never an empty-string or epoch sentinel.
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
-- pair keeps every index on this table consistent with "tenant first",
-- for the identical reason the sqlite/ sibling's own comment gives.
-- Request-time authentication does NOT read through this index: it
-- resolves the tenant from the hash alone through
-- integration_api_key_hash_index (migration 0006, model.go's
-- apiKeyHashIndex doc comment), then reads this table's row under that
-- resolved tenant.
CREATE UNIQUE INDEX uq_integration_api_keys_tenant_hash
    ON integration_api_keys (tenant_id, hash);

-- The expiry-sweep index no periodic job reads yet (no expiry sweep
-- exists -- see the module's Known limitations); it exists so such a job
-- needs no migration of its own, per the sqlite/ sibling's identical
-- comment.
CREATE INDEX idx_integration_api_keys_tenant_expires_at
    ON integration_api_keys (tenant_id, expires_at);
