-- ai_gateway_credentials is the ai-gateway module's single table
-- (go/ai-gateway/model.go): one row per provider per scope, keyed by
-- (provider, scope, tenant_id).
--
-- This is the SQLite copy of the migration; see the postgres/ sibling for
-- the full rationale of every column. The dialect differences stop at the
-- allowed SQL surface: no dialect-specific types, no gen_random_uuid(), no
-- NOW().
--
-- scope holds "system" (the platform-wide default credential) or "tenant"
-- (a tenant's own BYOK override); tenant_id holds the owning tenant on
-- tenant-tier rows and the empty-string sentinel on system-tier rows
-- (never NULL, so the primary key stays a true unique constraint);
-- api_key holds the AES-256-GCM ciphertext of the vendor API key, sealed
-- by the host's dbkit.Cipher via CredentialAPIKeySerializerName;
-- base_url holds the provider's configured endpoint, stored unencrypted
-- since it identifies a network destination, not a secret.
CREATE TABLE ai_gateway_credentials (
    provider    VARCHAR(100) NOT NULL,
    scope       VARCHAR(16)  NOT NULL,
    tenant_id   VARCHAR(64)  NOT NULL DEFAULT '',
    api_key     BLOB         NOT NULL,
    base_url    VARCHAR(500) NOT NULL DEFAULT '',
    updated_at  TIMESTAMP    NOT NULL,
    PRIMARY KEY (provider, scope, tenant_id)
);
