-- Refresh tokens, stored hashed and rotated on every use.
--
-- token_hash is a SHA-256 digest of the opaque 256-bit random token handed to
-- the client, and it is what a presented token is looked up by (hence the
-- UNIQUE index). SHA-256 rather than argon2id is deliberate and is not the
-- password rule being relaxed: the value hashed here is a full-entropy random
-- secret the server generated, so there is no guessing attack for a slow KDF
-- to defend against, and a per-request KDF on the refresh path would be pure
-- latency.
--
-- family_id and rotated_from are what make replay detection possible. Every
-- rotation issues a new row in the same family whose rotated_from points at
-- the row it replaced, and consuming a row moves it out of the "active"
-- status atomically. Presenting a row that is already consumed therefore
-- means the token leaked: the whole family and its session are revoked at
-- once (see session.go). Without the family column the only available
-- response would be to reject that one token and leave the thief's freshly
-- rotated token working.
CREATE TABLE refresh_tokens (
    id           VARCHAR(36)  NOT NULL,
    session_id   VARCHAR(36)  NOT NULL,
    user_id      VARCHAR(36)  NOT NULL,
    family_id    VARCHAR(36)  NOT NULL,
    rotated_from VARCHAR(36)  NOT NULL,
    token_hash   VARCHAR(64)  NOT NULL,
    status       VARCHAR(32)  NOT NULL,
    created_at   TIMESTAMP    NOT NULL,
    expires_at   TIMESTAMP    NOT NULL,
    consumed_at  TIMESTAMP,
    PRIMARY KEY (id)
);

CREATE UNIQUE INDEX idx_refresh_tokens_token_hash ON refresh_tokens (token_hash);

CREATE INDEX idx_refresh_tokens_family_id ON refresh_tokens (family_id);

CREATE INDEX idx_refresh_tokens_session_id ON refresh_tokens (session_id);
