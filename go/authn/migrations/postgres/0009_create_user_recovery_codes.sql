-- See sqlite/0009_create_user_recovery_codes.sql for the full design
-- discussion; the only difference is the timestamp type, per this module's
-- dual-dialect discipline.
CREATE TABLE user_recovery_codes (
    id         VARCHAR(36) NOT NULL,
    user_id    VARCHAR(36) NOT NULL,
    code_hash  VARCHAR(64) NOT NULL,
    used_at    TIMESTAMP,
    created_at TIMESTAMP   NOT NULL,
    PRIMARY KEY (id)
);

CREATE INDEX idx_user_recovery_codes_user_id ON user_recovery_codes (user_id);
