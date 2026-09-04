-- pki_certificate_revocations is round 3's denormalized, append-only
-- revocation ledger (go/pki/model.go's CertificateRevocation): one row per
-- CAService.RevokeCertificate call, existing so CAService.GenerateCRL can
-- enumerate every certificate an authority ever revoked WITHOUT a
-- cross-tenant read against the tenant-scoped pki_certificates table.
--
-- Platform data -- no dbkit.TenantScoped, isolation proven by
-- tenancytest.AssertNotTenantScoped. tenant_id is a real, deliberately
-- UNENFORCED column, the same treatment go/notification's send_records and
-- platform_blacklist and go/dbkit/audit's AuditEvent already get, kept here
-- purely as informational metadata for an eventual audit view -- see the
-- Go type's own doc comment for the full "why this table exists at all"
-- argument.
--
-- This is the SQLite copy; see the postgres/ sibling for the identical
-- schema on that dialect.
CREATE TABLE pki_certificate_revocations (
    id                 VARCHAR(36)  NOT NULL,
    certificate_id     VARCHAR(36)  NOT NULL,
    authority_id       VARCHAR(36)  NOT NULL,
    serial             VARCHAR(64)  NOT NULL,
    tenant_id          VARCHAR(64)  NOT NULL,
    revoked_at         TIMESTAMP    NOT NULL,
    revocation_reason  VARCHAR(255) NOT NULL,
    created_at         TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);

-- The CRL-generation query: every revocation for a given authority, in no
-- particular order (CAService.GenerateCRL sorts nothing -- x509.CreateRevocationList
-- does not require sorted entries).
CREATE INDEX idx_pki_certificate_revocations_authority_id ON pki_certificate_revocations (authority_id);

-- Looking up whether (and when) one specific certificate was revoked,
-- without a table scan.
CREATE INDEX idx_pki_certificate_revocations_certificate_id ON pki_certificate_revocations (certificate_id);
