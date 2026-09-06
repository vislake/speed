-- Narrows idx_user_mfa_factors_user_type (0008) from a full unique index on
-- (user_id, type) to its "active rows only" equivalent, so a pending
-- replacement factor can coexist with the still-active factor it will
-- eventually replace -- see the identical rationale and technique in
-- go/pki/migrations/{sqlite,postgres}/0001_create_pki_signing_keys.sql's
-- uq_pki_signing_keys_active_purpose (also "at most one ACTIVE row of this
-- kind", scoped the same way for the same reason) and
-- go/rbac/migrations/{sqlite,postgres}/0002_add_soft_delete.sql (the
-- drop-and-recreate-under-the-same-name technique). SQLite has supported
-- partial indexes since 3.8.0, and the rewrite is standard SQL, not a
-- PostgreSQL-only feature.
--
-- This is the schema half of Service.EnrollTOTP/ConfirmTOTP's two-phase
-- replacement fix: EnrollTOTP used to delete an existing ACTIVE factor
-- before creating the new PENDING one, which meant an abandoned or
-- cancelled enrollment wizard left the account with no working second
-- factor and no working recovery codes at all -- a silent security-posture
-- downgrade a step-up-gated "replace" action must never cause. The fix
-- keeps the old active factor live until ConfirmTOTP genuinely succeeds,
-- which needs one pending row to be able to sit beside the still-active row
-- it will eventually replace; this index is what makes that legal instead
-- of a unique-constraint violation on INSERT.
--
-- The multiple-pending-rows-per-user-and-type case this leaves unconstrained
-- (nothing stops two pending rows for the same user+type existing at once)
-- is deliberately still ruled out at the application layer instead:
-- Service.EnrollTOTP deletes any existing PENDING row of the type before
-- creating a fresh one, exactly as it always deleted before -- only the
-- ACTIVE row is now spared.
DROP INDEX idx_user_mfa_factors_user_type;

CREATE UNIQUE INDEX idx_user_mfa_factors_user_type
    ON user_mfa_factors (user_id, type)
    WHERE status = 'active';
