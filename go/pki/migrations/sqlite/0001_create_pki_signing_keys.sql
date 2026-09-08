-- pki_signing_keys is the pki module's key-lifecycle layer core table
-- (go/pki/model.go): one row per signing key, platform data -- it does NOT
-- carry a tenant_id column, and its isolation is proven by
-- tenancytest.AssertNotTenantScoped, never AssertIsolated.
--
-- This is the SQLite copy; see the postgres/ sibling for the full rationale
-- of every column. The dialect differences stop at the allowed SQL surface:
-- no dialect-specific types, no native arrays, no JSONB, no
-- gen_random_uuid(), no NOW().
CREATE TABLE pki_signing_keys (
    id                 VARCHAR(64)  NOT NULL,
    purpose            VARCHAR(128) NOT NULL,
    algorithm          VARCHAR(32)  NOT NULL,
    signer_name        VARCHAR(64)  NOT NULL,
    key_ref            VARCHAR(255) NOT NULL,
    status             VARCHAR(16)  NOT NULL,
    public_key         BLOB         NOT NULL,
    not_before         TIMESTAMP    NOT NULL,
    not_after          TIMESTAMP    NOT NULL,
    activated_at       TIMESTAMP,
    retired_at         TIMESTAMP,
    revoked_at         TIMESTAMP,
    revocation_reason  VARCHAR(255) NOT NULL DEFAULT '',
    created_at         TIMESTAMP    NOT NULL,
    updated_at         TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);

-- The lookup index behind SigningKeyRepository.FindActiveByPurpose and
-- ListVerifiableByPurpose: both filter by purpose first.
CREATE INDEX idx_pki_signing_keys_purpose ON pki_signing_keys (purpose);

-- The expiry-scan index: the periodic scan job reads not_after to find
-- keys whose validity window has closed, so the lookup is indexed from
-- the start.
CREATE INDEX idx_pki_signing_keys_not_after ON pki_signing_keys (not_after);

-- At most one active key per purpose -- see the postgres/ sibling's doc
-- comment. SQLite supports partial indexes (since 3.8.0), and
-- go/dbkit's own soft-delete indexes already rely on the identical
-- WHERE-qualified-unique-index technique across both dialects.
CREATE UNIQUE INDEX uq_pki_signing_keys_active_purpose
    ON pki_signing_keys (purpose)
    WHERE status = 'active';
