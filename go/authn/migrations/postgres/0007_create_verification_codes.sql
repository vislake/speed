-- See sqlite/0007_create_verification_codes.sql for the full design
-- discussion; the only difference between the two dialects is the timestamp
-- type, per this module's dual-dialect discipline (application-generated
-- UUIDs, no gen_random_uuid(), no NOW()).
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
