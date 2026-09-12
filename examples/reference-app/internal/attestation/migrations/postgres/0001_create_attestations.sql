-- The attestation layer's attestations table (internal/attestation): the
-- durable record of one output's attestation -- that the platform vouches
-- for the bytes of object_id under the tenant, by certificate_id, at the
-- moment created_at records. One row per object, holding the CURRENT
-- attestation (a re-attestation replaces the row's certificate, message
-- and signature).
--
-- One copy serves both dialects (the postgres/ file is byte-identical):
-- VARCHAR/TIMESTAMP columns, application-generated values, no PostgreSQL-
-- or SQLite-specific syntax. The CREATE ... IF NOT EXISTS spelling is the
-- upgrade contract -- a database whose table an earlier boot created
-- without this ledger must migrate cleanly, so the statement is a no-op
-- there and the ledger simply starts recording the file.
--
-- The column set and types match the GORM model tags in record.go exactly:
-- message holds the canonical JSON bytes of the attested message (the exact
-- bytes signed) and signature the raw Ed25519 signature over them,
-- hex-encoded -- both kept as written so the gate verifies the signature
-- over the stored bytes and then parses the message.
CREATE TABLE IF NOT EXISTS smilesim_attestations (
    object_id      VARCHAR(64)  NOT NULL PRIMARY KEY,
    tenant_id      VARCHAR(64)  NOT NULL,
    certificate_id VARCHAR(36)  NOT NULL,
    message        VARCHAR(512) NOT NULL,
    signature      VARCHAR(256) NOT NULL,
    created_at     TIMESTAMP    NOT NULL
);

-- The lookup index behind the store's per-tenant newest-row read (the
-- tenant's current attestation certificate), ordered by created_at
-- descending. Indexes cannot be declared portably inside CREATE TABLE.
CREATE INDEX IF NOT EXISTS idx_smilesim_attestations_tenant_created ON smilesim_attestations (tenant_id, created_at);
