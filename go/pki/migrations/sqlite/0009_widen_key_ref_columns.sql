-- pki_signing_keys.key_ref, pki_authorities.key_ref and
-- pki_certificates.key_ref hold whichever pki.Signer implementation owns a
-- key's material, as that implementation's own opaque handle -- and for the
-- vault and kmsaws providers in envelope mode that handle is the base64 of
-- the whole provider-side ciphertext (go/pki/signer/kmsaws/signer.go:
-- keyRef = base64.StdEncoding.EncodeToString of the KMS Encrypt
-- CiphertextBlob; go/pki/AGENTS.md's P1-2 record carries the assessment).
-- The arithmetic is deterministic: base64 of any blob of 192 bytes or more
-- exceeds the 255 characters every key_ref column declared at creation
-- (0001/0002/0003), and a real KMS symmetric ciphertext blob for an
-- ~80-byte PKCS8 ed25519 key is empirically 0.5-2KB raw, i.e. up to ~2732
-- base64 characters. SQLite does not enforce VARCHAR length, which is
-- exactly why this migration must still ship: PostgreSQL, which does
-- enforce it, refuses the write a real envelope-mode deployment must make
-- (see the postgres/ sibling for the widening's own half there).
--
-- This migration widens the three columns to VARCHAR(4096) -- the next
-- power-of-two bound above the observed 0.5-2KB window's ~2732 base64
-- characters, admitting any raw blob up to 3072 bytes, kept finite on
-- purpose so a key_ref handle cannot silently grow into a document store
-- without a schema review. The GORM model size tags (go/pki/model.go, the
-- Atlas-style source of the schema) were widened in lockstep.
--
-- SQLite cannot ALTER a column type, so each table is rebuilt instead,
-- with the standard idiom: rename the old table aside, create the new one
-- under the same name with the widened column, copy every row across by
-- explicit column list, drop the renamed original. Every index on these
-- tables is a standalone CREATE INDEX object (nothing is an inline UNIQUE
-- constraint -- 0001/0002/0003 each used CREATE INDEX / CREATE UNIQUE
-- INDEX), so dropping the original table drops them with it and each is
-- re-created below on the new table, under the same name, so every
-- reference to that name (error mapping via gorm.ErrDuplicatedKey, this
-- module's own tests) needs no change. The rebuild is atomic: dbkit's
-- MigrationRegistry applies one module's migration files inside a single
-- transaction (go/dbkit/migrations.go's applyModule), and SQLite DDL is
-- transactional, so an interruption mid-file leaves the pre-0009 schema
-- intact with 0009 unrecorded.
--
-- This is the SQLite copy; see the postgres/ sibling for the identical
-- schema on that dialect.
ALTER TABLE pki_signing_keys RENAME TO pki_signing_keys_0009_old;

CREATE TABLE pki_signing_keys (
    id                 VARCHAR(64)  NOT NULL,
    purpose            VARCHAR(128) NOT NULL,
    algorithm          VARCHAR(32)  NOT NULL,
    signer_name        VARCHAR(64)  NOT NULL,
    key_ref            VARCHAR(4096) NOT NULL,
    status             VARCHAR(16)  NOT NULL,
    public_key         BLOB         NOT NULL,
    not_before         TIMESTAMP    NOT NULL,
    not_after          TIMESTAMP    NOT NULL,
    activated_at       TIMESTAMP,
    retiring_at        TIMESTAMP,
    retired_at         TIMESTAMP,
    revoked_at         TIMESTAMP,
    revocation_reason  VARCHAR(255) NOT NULL DEFAULT '',
    retiring_overlap   BIGINT       NOT NULL DEFAULT 0,
    created_at         TIMESTAMP    NOT NULL,
    updated_at         TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);

INSERT INTO pki_signing_keys
    (id, purpose, algorithm, signer_name, key_ref, status, public_key,
     not_before, not_after, activated_at, retiring_at, retired_at,
     revoked_at, revocation_reason, retiring_overlap, created_at, updated_at)
SELECT id, purpose, algorithm, signer_name, key_ref, status, public_key,
       not_before, not_after, activated_at, retiring_at, retired_at,
       revoked_at, revocation_reason, retiring_overlap, created_at, updated_at
FROM pki_signing_keys_0009_old;

DROP TABLE pki_signing_keys_0009_old;

-- The 0001 indexes, re-created under their original names (see the header).
CREATE INDEX idx_pki_signing_keys_purpose ON pki_signing_keys (purpose);
CREATE INDEX idx_pki_signing_keys_not_after ON pki_signing_keys (not_after);
CREATE UNIQUE INDEX uq_pki_signing_keys_active_purpose
    ON pki_signing_keys (purpose)
    WHERE status = 'active';

ALTER TABLE pki_authorities RENAME TO pki_authorities_0009_old;

CREATE TABLE pki_authorities (
    id                     VARCHAR(36)  NOT NULL,
    type                   VARCHAR(16)  NOT NULL,
    parent_id              VARCHAR(36),
    subject                VARCHAR(255) NOT NULL,
    serial                 VARCHAR(64)  NOT NULL,
    certificate_pem        TEXT         NOT NULL,
    signer_name            VARCHAR(64)  NOT NULL,
    key_ref                VARCHAR(4096) NOT NULL,
    status                 VARCHAR(16)  NOT NULL DEFAULT 'active',
    not_before             TIMESTAMP    NOT NULL,
    not_after              TIMESTAMP    NOT NULL,
    revoked_at             TIMESTAMP,
    revocation_reason      VARCHAR(255) NOT NULL DEFAULT '',
    crl_distribution_point VARCHAR(500) NOT NULL DEFAULT '',
    crl_number             BIGINT       NOT NULL DEFAULT 0,
    crl_pem                TEXT,
    crl_issued_at          TIMESTAMP,
    crl_next_update        TIMESTAMP,
    created_at             TIMESTAMP    NOT NULL,
    updated_at             TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);

INSERT INTO pki_authorities
    (id, type, parent_id, subject, serial, certificate_pem, signer_name,
     key_ref, status, not_before, not_after, revoked_at, revocation_reason,
     crl_distribution_point, crl_number, crl_pem, crl_issued_at,
     crl_next_update, created_at, updated_at)
SELECT id, type, parent_id, subject, serial, certificate_pem, signer_name,
       key_ref, status, not_before, not_after, revoked_at, revocation_reason,
       crl_distribution_point, crl_number, crl_pem, crl_issued_at,
       crl_next_update, created_at, updated_at
FROM pki_authorities_0009_old;

DROP TABLE pki_authorities_0009_old;

-- The 0002 and 0006 indexes, re-created under their original names.
CREATE INDEX idx_pki_authorities_parent_id ON pki_authorities (parent_id);
CREATE INDEX idx_pki_authorities_serial ON pki_authorities (serial);
CREATE INDEX idx_pki_authorities_not_after ON pki_authorities (not_after);

ALTER TABLE pki_certificates RENAME TO pki_certificates_0009_old;

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
    key_ref            VARCHAR(4096) NOT NULL,
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

INSERT INTO pki_certificates
    (id, tenant_id, authority_id, purpose, subject, sans, serial,
     certificate_pem, signer_name, key_ref, status, key_delivered,
     not_before, not_after, revoked_at, revocation_reason, created_at,
     updated_at)
SELECT id, tenant_id, authority_id, purpose, subject, sans, serial,
       certificate_pem, signer_name, key_ref, status, key_delivered,
       not_before, not_after, revoked_at, revocation_reason, created_at,
       updated_at
FROM pki_certificates_0009_old;

DROP TABLE pki_certificates_0009_old;

-- The 0003 indexes, re-created under their original names.
CREATE INDEX idx_pki_certificates_tenant_authority
    ON pki_certificates (tenant_id, authority_id);
CREATE INDEX idx_pki_certificates_tenant_purpose
    ON pki_certificates (tenant_id, purpose);
CREATE INDEX idx_pki_certificates_tenant_serial
    ON pki_certificates (tenant_id, serial);
CREATE INDEX idx_pki_certificates_tenant_not_after
    ON pki_certificates (tenant_id, not_after);
