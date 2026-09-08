-- Adds the PENDING twin of idx_user_mfa_factors_user_type (0010, scoped to
-- status='active'): a partial unique index on (user_id, type) over the
-- status='pending' rows only, so at most one in-progress enrollment can
-- exist per user and factor type. Two rapid enroll requests can otherwise
-- interleave their deletes and creates (del, del, insert, insert) and leave
-- two pending rows, after which ConfirmTOTP could activate whichever the
-- database returned first, which need not be the enrollment the user
-- scanned. The partial-index technique is the same as the active-scoped
-- index in 0010 and the identical patterns in go/pki
-- (uq_pki_signing_keys_active_purpose) and go/rbac (0002): the active index
-- lets one pending row coexist with an active one, and this index makes
-- that same pending row unique.
--
-- Service.EnrollTOTP (through MFAFactorRepository.ReplacePending) deletes
-- any existing pending row before creating its own, so the ordinary
-- sequential path never sees this index; it exists so a LOST race against
-- a concurrent enrollment is refused by the database instead of silently
-- producing a second pending row, and ReplacePending retries its
-- delete-then-create once the refusing row has committed.
--
-- A database that holds two pending rows for one (user_id, type) --
-- possible when enrollments raced before this index -- will see this index
-- creation refuse loudly at upgrade; the stale pending rows are safe to
-- remove (a pending factor verifies nothing), and the migration can then
-- be re-run.
CREATE UNIQUE INDEX idx_user_mfa_factors_user_type_pending
    ON user_mfa_factors (user_id, type)
    WHERE status = 'pending';
