-- pki_authorities is the internal CA chain (go/pki/model.go): one row per
-- root or intermediate authority. Platform data -- no tenant_id column;
-- isolation proven by tenancytest.AssertNotTenantScoped.
--
-- This is the PostgreSQL copy; see the sqlite/ sibling for the identical
-- schema on that dialect.
--
-- parent_id is a genuine NULL for a root authority (which signs its own
-- certificate) and names the issuing authority's id for an intermediate.
-- Unlike org_nodes.parent_id or configs.tenant_id, there is no unique index
-- keyed on this column -- an authority may issue any number of
-- intermediates -- so there is no NULL-vs-empty-string collision to avoid,
-- and a real nullable column is the honest representation.
--
-- serial is 16 bytes of crypto/rand, lower-case hex encoded -- never a
-- timestamp; docs/internal/22-pki.md's diagnosis names
-- System.currentTimeMillis()-derived serials as a real collision risk under
-- concurrent issuance. certificate_pem is this authority's own certificate;
-- safe to expose, since a certificate is not a secret. signer_name/key_ref
-- point at the private key the same no-private-key-column way
-- pki_signing_keys does.
--
-- status carries both eventual values (active/revoked) from day one, even
-- though this round's code only ever writes 'active' -- revocation is round
-- 3's work, but the column shape is this round's.
CREATE TABLE pki_authorities (
    id                 VARCHAR(36)  NOT NULL,
    type               VARCHAR(16)  NOT NULL,
    parent_id          VARCHAR(36),
    subject            VARCHAR(255) NOT NULL,
    serial             VARCHAR(64)  NOT NULL,
    certificate_pem    TEXT         NOT NULL,
    signer_name        VARCHAR(64)  NOT NULL,
    key_ref            VARCHAR(255) NOT NULL,
    status             VARCHAR(16)  NOT NULL DEFAULT 'active',
    not_before         TIMESTAMP    NOT NULL,
    not_after          TIMESTAMP    NOT NULL,
    revoked_at         TIMESTAMP,
    revocation_reason  VARCHAR(255) NOT NULL DEFAULT '',
    created_at         TIMESTAMP    NOT NULL,
    updated_at         TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);

-- The chain-walk index: finding every intermediate a given authority issued.
CREATE INDEX idx_pki_authorities_parent_id ON pki_authorities (parent_id);

-- Lookup by serial (CRL and chain-validation paths a later round adds).
CREATE INDEX idx_pki_authorities_serial ON pki_authorities (serial);

-- The expiry-scan index round 2/3's jobs-driven scan will read.
CREATE INDEX idx_pki_authorities_not_after ON pki_authorities (not_after);
