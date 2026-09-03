-- The ten single-use recovery codes generated when a user confirms TOTP
-- enrollment (Service.ConfirmTOTP) or asks for a fresh batch
-- (Service.RegenerateRecoveryCodes). IDENTITY-domain data, like
-- user_mfa_factors: a recovery code belongs to the person, not to a tenant.
--
-- code_hash is a plain SHA-256 digest of the code's NORMALIZED form (upper
-- case, dashes stripped -- see verification.go's hashRecoveryCode), for the
-- same reason verification_codes.code_hash and refresh_tokens.token_hash
-- are: the plaintext is drawn by the server with full entropy, so there is
-- no offline dictionary attack a slow hash would need to defend against.
-- The plaintext is shown to the account owner exactly once, at generation
-- time, and never stored anywhere.
--
-- used_at is NULL for an unused code and is what makes a code single-use:
-- RecoveryCodeRepository.MarkUsed is a compare-and-swap on "used_at IS
-- NULL", the same pattern RefreshTokenRepository.Consume uses for refresh
-- tokens, so two concurrent uses of the same code cannot both succeed.
CREATE TABLE user_recovery_codes (
    id         VARCHAR(36) NOT NULL,
    user_id    VARCHAR(36) NOT NULL,
    code_hash  VARCHAR(64) NOT NULL,
    used_at    TIMESTAMP,
    created_at TIMESTAMP   NOT NULL,
    PRIMARY KEY (id)
);

CREATE INDEX idx_user_recovery_codes_user_id ON user_recovery_codes (user_id);
