-- integration_api_keys is go/integration's round-1 table (go/integration/
-- model.go): one row per API key a tenant has issued for programmatic
-- access to its own data. Tenant data
-- (docs/internal/04-data-and-tenancy.md) -- a key belongs to exactly one
-- tenant and must never be visible from another -- so its isolation is
-- proven by tenancytest.AssertIsolated, never AssertNotTenantScoped, and
-- (per that same doc's distributed-mode rule) will carry a PostgreSQL RLS
-- policy in the distributed deployment mode once one is wired for this
-- module, the same way every other tenant-scoped table's does.
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
-- Service.Create, uuid.NewString -- never a database default, per the
-- backend coding standard's ban on gen_random_uuid()). prefix is the
-- plaintext, non-secret display portion (go/integration/keygen.go);
-- hash is the hex-encoded SHA-256 of the raw key, which is never itself
-- stored anywhere -- see APIKey's own doc comment in model.go for why a
-- one-way digest of full-entropy randomness needs no further encryption.
--
-- scopes is a JSON array of permission strings, stored as TEXT rather than
-- a native PostgreSQL array or JSONB column with operator filtering, per
-- the backend coding standard's dual-dialect rule -- this module only ever
-- reads the column back whole (go/integration/model.go's parseScopes),
-- never filters on an individual element inside SQL.
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
-- of one tenant) needs tenant_id indexed; this also supports an
-- authentication-time lookup by hash -- a later round's "authenticate a
-- request with a key" path (AGENTS.md's Deferred list) -- scoped by tenant
-- first for the identical reason the sqlite/ sibling's own comment gives.
CREATE UNIQUE INDEX uq_integration_api_keys_tenant_hash
    ON integration_api_keys (tenant_id, hash);

-- The expiry-sweep index a later round's cleanup job will read; added now,
-- ahead of that job existing, per the sqlite/ sibling's identical comment.
CREATE INDEX idx_integration_api_keys_tenant_expires_at
    ON integration_api_keys (tenant_id, expires_at);
