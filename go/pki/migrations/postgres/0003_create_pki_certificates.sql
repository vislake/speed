-- pki_certificates holds end-entity certificates (go/pki/model.go): the ONE
-- table of pki's four that is tenant data. tenant_id is real and enforced;
-- Certificate implements dbkit.TenantScoped and is reached only through
-- CertificateRepository (embedding dbkit.Repository[Certificate]),
-- isolation proven by tenancytest.AssertIsolated.
--
-- Keeping this split -- platform CAs in pki_authorities, tenant
-- certificates here, never merged into one table -- is round 1's central
-- schema decision: docs/internal/22-pki.md's diagnosed system put both a
-- platform CA and per-tenant certificates in one table under one weak key,
-- and that is the exact anti-pattern this split exists to rule out
-- ("one table must never mix two data domains").
--
-- This is the PostgreSQL copy; see the sqlite/ sibling for the identical
-- schema on that dialect.
--
-- authority_id names the pki_authorities row that signed this certificate.
-- Deliberately not a foreign key: cross-module foreign keys are forbidden
-- in this codebase (docs/internal/04-data-and-tenancy.md rule 4), and here
-- the two tables additionally sit in different data domains, so a foreign
-- key would also cross the tenant/platform boundary a single constraint
-- cannot express correctly.
--
-- sans is a JSON array of subject alternative names, plain TEXT on both
-- dialects (datatypes.JSON on the Go side) -- never a native array column,
-- and nothing ever filters into its structure.
--
-- key_delivered records whether the private key itself has left this
-- platform's custody (some consumers, per docs/internal/22-pki.md's
-- diagnosis, must hand the raw key to a downstream system). Once true, the
-- Signer-side protection has nothing left to protect -- see the Go type's
-- doc comment for the full argument.
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
