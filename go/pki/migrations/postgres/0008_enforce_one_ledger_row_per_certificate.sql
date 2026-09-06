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
-- The index can only be created if the ledger actually holds one row per
-- certificate_id, and the ledger as 0007 left it carries no such
-- guarantee: round 3's pre-arbitration RevokeCertificate wrote its ledger
-- row through an unconditional Create behind a check-then-act
-- certificate-status read (revocation.go as it shipped before the
-- InsertIfAbsent round the header above describes), so two callers
-- revoking one active certificate concurrently could both pass the check
-- and land TWO rows for it -- a state 0007's non-unique per-certificate
-- index could not refuse. Such a database cannot build the unique index
-- below, and dbkit's MigrationRegistry applies one module's migration
-- files in a single transaction (go/dbkit/migrations.go's applyModule), so
-- that failure rolls 0008 and every later file back together, stranding
-- the deployment at startup with 0008 unrecorded and no remediation path.
-- Any duplicates that predate the constraint are therefore collapsed
-- first, keeping the EARLIEST row per certificate_id (by created_at, with
-- id as the deterministic tiebreak) -- the row the database-arbitrated
-- single-winner arbitration this index provides would itself have let
-- win, since of any number of racing revocations of one certificate the
-- first one is the revocation.
--
-- The collapse is safe for a duplicate-free ledger (the DELETE matches
-- nothing) and can never re-run against a database that already applied
-- this file: the registry records applied files by (module, filename) and
-- skips a recorded file forever, never comparing its content
-- (isApplied/recordApplied in go/dbkit/migrations.go). Editing this
-- shipped file in place therefore reaches exactly the deployment class it
-- rescues -- one stranded at 0008, whose rolled-back transaction left no
-- record behind -- and leaves a database that applied the earlier content
-- untouched, so no follow-up migration is needed for either class.
--
-- ROW_NUMBER OVER is standard SQL, supported by both dialects (SQLite
-- since 3.25, PostgreSQL from the start), so this statement is
-- byte-identical across the two copies like the rest of the file.
--
-- This is the PostgreSQL copy; see the sqlite/ sibling for the identical
-- DDL on that dialect.
DELETE FROM pki_certificate_revocations
WHERE id NOT IN (
    SELECT id
    FROM (
        SELECT id, ROW_NUMBER() OVER (
            PARTITION BY certificate_id
            ORDER BY created_at ASC, id ASC
        ) AS rn
        FROM pki_certificate_revocations
    ) AS deduped
    WHERE rn = 1
);

CREATE UNIQUE INDEX uq_pki_certificate_revocations_certificate
    ON pki_certificate_revocations (certificate_id);

DROP INDEX idx_pki_certificate_revocations_certificate_id;
