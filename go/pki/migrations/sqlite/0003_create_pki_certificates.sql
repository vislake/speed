-- pki_certificates holds end-entity certificates (go/pki/model.go): the ONE
-- table of pki's four that is tenant data. tenant_id is real and enforced;
-- Certificate implements dbkit.TenantScoped and is reached only through
-- CertificateRepository (embedding dbkit.Repository[Certificate]),
-- isolation proven by tenancytest.AssertIsolated.
--
-- This is the SQLite copy; see the postgres/ sibling for the full rationale
-- of every column.
CREATE TABLE pki_certificates (
    id                 VARCHAR(36)  NOT NULL,
    tenant_id          VARCHAR(64)  NOT NULL,
    authority_id       VARCHAR(36)  NOT NULL,
    purpose            VARCHAR(128) NOT NULL,
    subject            VARCHAR(255) NOT NULL,
    sans               TEXT,
    serial             VARCHAR(64)  NOT NULL,
    certificate_pem    TEXT         NOT NULL,
    signer_name        VARCHAR(64)  NOT NULL,
    key_ref            VARCHAR(255) NOT NULL,
    status             VARCHAR(16)  NOT NULL DEFAULT 'active',
    key_delivered      BOOLEAN      NOT NULL DEFAULT FALSE,
    not_before         TIMESTAMP    NOT NULL,
    not_after          TIMESTAMP    NOT NULL,
    revoked_at         TIMESTAMP,
    revocation_reason  VARCHAR(255) NOT NULL DEFAULT '',
    created_at         TIMESTAMP    NOT NULL,
    updated_at         TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);

-- Every composite index below leads with tenant_id, per this codebase's own
-- convention for tenant-scoped tables.
CREATE INDEX idx_pki_certificates_tenant_authority ON pki_certificates (tenant_id, authority_id);
CREATE INDEX idx_pki_certificates_tenant_purpose ON pki_certificates (tenant_id, purpose);
CREATE INDEX idx_pki_certificates_tenant_serial ON pki_certificates (tenant_id, serial);

-- The expiry-scan index round 2/3's jobs-driven scan will read.
CREATE INDEX idx_pki_certificates_tenant_not_after ON pki_certificates (tenant_id, not_after);
