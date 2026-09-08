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
-- This index exists for Service.EnrollTOTP/ConfirmTOTP's two-phase
-- replacement: an existing ACTIVE factor stays live until ConfirmTOTP
-- genuinely succeeds -- an enrollment abandoned before confirming must not
-- leave the account with no working second factor and no working recovery
-- codes -- which needs one pending row to be able to sit beside the
-- still-active row it will eventually replace. This index is what makes
-- that legal instead of a unique-constraint violation on INSERT.
--
-- Two pending rows for the same user+type are not constrained by this
-- index; ruling them out stays the application layer's job:
-- Service.EnrollTOTP deletes any existing PENDING row of the type before
-- creating a fresh one, while the ACTIVE row is spared until a confirm
-- genuinely succeeds (migration 0011 later constrains the pending case in
-- the schema as well).
DROP INDEX idx_user_mfa_factors_user_type;

CREATE UNIQUE INDEX idx_user_mfa_factors_user_type
    ON user_mfa_factors (user_id, type)
    WHERE status = 'active';
