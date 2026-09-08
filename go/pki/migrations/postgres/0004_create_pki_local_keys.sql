-- pki_local_keys is the LocalSigner implementation's own private-key store
-- (go/pki/model.go). Only LocalSigner ever reads or writes this table; the
-- vault and kmsaws implementations never touch it, because their key
-- material never lives in this database at all. Platform data -- no tenant_id
-- column; isolation proven by tenancytest.AssertNotTenantScoped.
--
-- This is the PostgreSQL copy; see the sqlite/ sibling for the identical
-- schema on that dialect.
--
-- Separating this from pki_signing_keys / pki_authorities / pki_certificates
-- is what makes "no business table ever holds a private key" a schema fact
-- rather than a documentation promise.
--
-- encrypted_private_key is sealed with dbkit's field-level AES-256-GCM
-- encryption (go/pki's LocalKeySerializerName / RegisterLocalKeySerializer)
-- -- authenticated and randomized, unlike the ECB-mode, no-IV cipher the
-- diagnosed system used for the equivalent column.
--
-- not_after is nullable and not populated by any code path --
-- LocalSigner.GenerateKey takes no expiry parameter, so nothing writes it.
-- The column and its index exist so the expiry-scan job can scan
-- pki_local_keys directly, across whichever table actually owns a given
-- key_ref, without a three-way join.
CREATE TABLE pki_local_keys (
    key_ref                VARCHAR(64) NOT NULL,
    algorithm              VARCHAR(32) NOT NULL,
    encrypted_private_key  BYTEA       NOT NULL,
    not_after              TIMESTAMP,
    created_at             TIMESTAMP   NOT NULL,
    updated_at             TIMESTAMP   NOT NULL,
    PRIMARY KEY (key_ref)
);

-- The expiry-scan index -- see the doc comment above.
CREATE INDEX idx_pki_local_keys_not_after ON pki_local_keys (not_after);
