-- One-time verification codes, currently used only by the phone-login flow
-- (VerificationPurposePhoneLogin in verification.go). IDENTITY-domain data,
-- like every table before it in this module: a code is issued to a phone
-- number, not to a tenant.
--
-- target_index is the same HMAC blind index the users table's phone lookup
-- uses (UserRepository.PhoneIndexOf), never the plaintext phone number --
-- this table has no reason to hold a second copy of a PII column the users
-- table already carries encrypted.
--
-- code_hash is the SHA-256 digest of the code, not an argon2id hash: a
-- six-digit code is drawn by the SERVER with full entropy over its short
-- space, so unlike a user-chosen password there is no offline dictionary
-- attack a slow hash would need to defend against, and its brute-force
-- resistance instead comes from attempts/max_attempts below plus the
-- go/ratelimit-backed send/verify guards in ratelimit.go. This mirrors
-- exactly why refresh_tokens.token_hash (0003_create_refresh_tokens.sql) is
-- a plain SHA-256 rather than a password KDF.
--
-- attempts and max_attempts implement per-code lockout: a wrong code
-- increments attempts, and once it reaches max_attempts the row moves to
-- 'locked' and a fresh code must be requested -- see
-- Service.verifyPhoneLoginCode's own doc comment.
--
-- There is deliberately no separate "purpose" index or table per purpose:
-- purpose is a column precisely so a second use (a future phone-based
-- password-reset flow, say) reuses this table and this row shape rather
-- than duplicating it.
CREATE TABLE verification_codes (
    id           VARCHAR(36) NOT NULL,
    purpose      VARCHAR(32) NOT NULL,
    target_index VARCHAR(64) NOT NULL,
    code_hash    VARCHAR(64) NOT NULL,
    attempts     INTEGER     NOT NULL,
    max_attempts INTEGER     NOT NULL,
    status       VARCHAR(16) NOT NULL,
    created_at   TIMESTAMP   NOT NULL,
    expires_at   TIMESTAMP   NOT NULL,
    consumed_at  TIMESTAMP,
    PRIMARY KEY (id)
);

CREATE INDEX idx_verification_codes_target ON verification_codes (target_index, purpose, created_at);
