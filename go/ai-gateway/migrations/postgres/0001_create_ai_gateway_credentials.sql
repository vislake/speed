-- ai_gateway_credentials is the ai-gateway module's single table
-- (go/ai-gateway/model.go): one row per provider per scope, keyed by
-- (provider, scope, tenant_id). It is platform data, deliberately never
-- dbkit.TenantScoped -- see model.go's own doc comment for the full
-- reasoning, the identical shape go/config's own configs table follows.
--
-- provider is the ChatProviderRegistry name this row is a credential for
-- (for example "chat.openai-compatible").
--
-- scope holds "system" (the platform-wide default credential, resolved
-- when a tenant has no BYOK row of its own) or "tenant" (a tenant's own
-- bring-your-own-key override).
--
-- tenant_id holds the owning tenant on a tenant-tier row, and the
-- empty-string sentinel -- never NULL -- on a system-tier row. NULL is
-- deliberately avoided: NULLs are distinct in a PostgreSQL unique index, so
-- two system rows for one provider could otherwise coexist under NULL,
-- where the empty-string sentinel collapses them into the single row the
-- primary key promises. This is the identical convention go/config's own
-- configs table and go/org's org_nodes.parent_id already use.
--
-- api_key holds the AES-256-GCM ciphertext of the vendor API key (nonce
-- prepended, per dbkit's Cipher.Encrypt), sealed by the host's
-- dbkit.Cipher through the CredentialAPIKeySerializerName GORM serializer
-- -- never plaintext, and never looked up by its own value (a credential
-- is always addressed by (provider, scope, tenant_id), so no blind index
-- is needed here, unlike a phone number or email address used as a login
-- identifier).
--
-- base_url holds the provider's configured endpoint (for example
-- "https://api.openai.com/v1"), stored unencrypted: it identifies a
-- network destination, not a secret.
--
-- updated_at records the moment of the last write.
CREATE TABLE ai_gateway_credentials (
    provider    VARCHAR(100) NOT NULL,
    scope       VARCHAR(16)  NOT NULL,
    tenant_id   VARCHAR(64)  NOT NULL DEFAULT '',
    api_key     BYTEA        NOT NULL,
    base_url    VARCHAR(500) NOT NULL DEFAULT '',
    updated_at  TIMESTAMP    NOT NULL,
    PRIMARY KEY (provider, scope, tenant_id)
);
