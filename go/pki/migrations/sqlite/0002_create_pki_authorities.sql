-- pki_authorities is the internal CA chain (go/pki/model.go): one row per
-- root or intermediate authority. Platform data -- no tenant_id column;
-- isolation proven by tenancytest.AssertNotTenantScoped.
--
-- This is the SQLite copy; see the postgres/ sibling for the full rationale
-- of every column.
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
