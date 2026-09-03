-- A user's enrolled second factor. IDENTITY-domain data, like every table
-- before it: MFA is a property of the person, not of a tenant they happen
-- to be acting inside.
--
-- Only one factor type ships in this round: 'totp' (mfa.go's MFATypeTOTP).
-- The unique index on (user_id, type) is what makes enrolling again REPLACE
-- rather than accumulate a second pending row for the same type -- see
-- Service.EnrollTOTP's own doc comment -- and it is also the reserved slot
-- a future WebAuthn/passkey factor (docs/internal/05: not in v1.0 scope,
-- but the second-factor interface shape is) would take a second row under,
-- with a different 'type' value, rather than a schema change.
--
-- secret is TOTP's shared secret, base32-encoded before encryption at rest
-- through this module's PII serializer -- the same treatment as
-- users.email/users.phone and tenant_sso_configs.client_secret. It is
-- never returned by any API after enrollment; ProvisioningURI is the one
-- and only moment it leaves this table in a form other than ciphertext,
-- and that moment happens exactly once, during EnrollTOTP's own response.
--
-- last_used_step is the RFC 6238 time-step counter of the most recently
-- ACCEPTED code, the replay guard totp.Validate's own doc comment
-- describes: a code cannot be accepted twice, because doing so would
-- require last_used_step to move backwards.
CREATE TABLE user_mfa_factors (
    id              VARCHAR(36) NOT NULL,
    user_id         VARCHAR(36) NOT NULL,
    type            VARCHAR(32) NOT NULL,
    secret          BLOB,
    status          VARCHAR(16) NOT NULL,
    last_used_step  BIGINT      NOT NULL,
    created_at      TIMESTAMP   NOT NULL,
    confirmed_at    TIMESTAMP,
    PRIMARY KEY (id)
);

CREATE UNIQUE INDEX idx_user_mfa_factors_user_type ON user_mfa_factors (user_id, type);
