-- pki_certificate_revocations is round 3's denormalized, append-only
-- revocation ledger (go/pki/model.go's CertificateRevocation). See the
-- sqlite/ sibling for the full rationale; this is the PostgreSQL copy.
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

CREATE INDEX idx_pki_certificate_revocations_authority_id ON pki_certificate_revocations (authority_id);
CREATE INDEX idx_pki_certificate_revocations_certificate_id ON pki_certificate_revocations (certificate_id);
