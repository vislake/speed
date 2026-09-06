-- A certificate can be revoked at most once, so the revocation ledger
-- (0007) must be able to hold at most one row per certificate_id --
-- CAService.RevokeCertificate (revocation.go) arbitrates the running ->
-- revoked transition on exactly this constraint: its ledger write is an
-- INSERT ... ON CONFLICT (certificate_id) DO NOTHING whose RowsAffected
-- verdict names the single winner of a concurrent double-revoke
-- (CertificateRevocationRepository.InsertIfAbsent, repository.go), the
-- shape the round-3 code's check-then-act Create could not provide.
--
-- The unique index replaces 0007's non-unique per-certificate lookup
-- index: a query for "the revocation row(s) of certificate X" is served
-- identically by this index (no query names idx_pki_certificate_revocations_certificate_id
-- in code -- the CRL-generation query keys on authority_id, through
-- idx_pki_certificate_revocations_authority_id), so the redundant index is
-- dropped rather than kept alongside the constraint that supersedes it.
--
-- This is the SQLite copy; see the postgres/ sibling for the identical
-- DDL on that dialect.
CREATE UNIQUE INDEX uq_pki_certificate_revocations_certificate
    ON pki_certificate_revocations (certificate_id);

DROP INDEX idx_pki_certificate_revocations_certificate_id;
