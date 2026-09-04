-- pki_signing_keys is the pki module's key-lifecycle layer core table
-- (go/pki/model.go): one row per signing key, platform data -- it does NOT
-- carry a tenant_id column, and its isolation is proven by
-- tenancytest.AssertNotTenantScoped, never AssertIsolated.
--
-- This is the PostgreSQL copy; see the sqlite/ sibling for the identical
-- schema on that dialect. Kept portable on purpose: no dialect-specific
-- types, no native arrays, no JSONB, no gen_random_uuid(), no NOW().
--
-- id is the application-generated kid a JWT's header names. purpose groups
-- keys by what they sign (e.g. "authn.access_token"); the partial unique
-- index below is what makes "at most one active key per purpose"
-- (docs/internal/22-pki.md's lifecycle state machine) a database-enforced
-- fact rather than a hope.
--
-- public_key is the DER SubjectPublicKeyInfo encoding, not sensitive.
-- signer_name/key_ref are the only pointers to the actual private key --
-- there is no private-key column on this table, ever; see
-- pki_local_keys for where a "local" signer's key material actually lives.
--
-- status is the full five-value lifecycle vocabulary
-- (pending/active/retiring/retired/revoked) from day one, even though this
-- round's code only ever writes pending->active directly -- getting the
-- column's value set right now is what lets round 2's state machine avoid
-- a migration of its own (docs/internal/22-pki.md: "round 1 must get the
-- table shape right, or round 2's state machine has to be redone").
CREATE TABLE pki_signing_keys (
    id                 VARCHAR(64)  NOT NULL,
    purpose            VARCHAR(128) NOT NULL,
    algorithm          VARCHAR(32)  NOT NULL,
    signer_name        VARCHAR(64)  NOT NULL,
    key_ref            VARCHAR(255) NOT NULL,
    status             VARCHAR(16)  NOT NULL,
    public_key         BYTEA        NOT NULL,
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

-- The expiry-scan index round 2's jobs-driven scan will read; this round
-- adds it ahead of that scan existing, per this module's own "get the
-- table structure right now" instruction.
CREATE INDEX idx_pki_signing_keys_not_after ON pki_signing_keys (not_after);

-- At most one active key per purpose -- a real database constraint behind
-- SigningKeyRepository.FindActiveByPurpose's "the row" assumption, not just
-- an application-level convention. A partial unique index, the same
-- technique go/dbkit's soft-delete round documents for
-- "a value can repeat across states but not within one".
CREATE UNIQUE INDEX uq_pki_signing_keys_active_purpose
    ON pki_signing_keys (purpose)
    WHERE status = 'active';
