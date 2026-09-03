-- See sqlite/0008_create_user_mfa_factors.sql for the full design
-- discussion; the only differences are the timestamp type and the
-- ciphertext column type, per this module's dual-dialect discipline (the
-- same difference as users.email/users.phone in 0001_create_users.sql).
CREATE TABLE user_mfa_factors (
    id              VARCHAR(36) NOT NULL,
    user_id         VARCHAR(36) NOT NULL,
    type            VARCHAR(32) NOT NULL,
    secret          BYTEA,
    status          VARCHAR(16) NOT NULL,
    last_used_step  BIGINT      NOT NULL,
    created_at      TIMESTAMP   NOT NULL,
    confirmed_at    TIMESTAMP,
    PRIMARY KEY (id)
);

CREATE UNIQUE INDEX idx_user_mfa_factors_user_type ON user_mfa_factors (user_id, type);
