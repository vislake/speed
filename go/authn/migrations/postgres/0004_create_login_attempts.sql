-- Every login ATTEMPT, successful or not, lands here
-- (docs/internal/05-identity-and-access.md's login-history section). It feeds three
-- things: the user's own security page, the progressive-delay and lockout
-- logic layered on go/ratelimit, and the anomalous-login detection that lands
-- with the notification module.
--
-- identifier_index, not the identifier itself. An attempt against an address
-- that matches no account still has to be countable per account -- that is
-- how credential stuffing is spotted -- but writing the attempted email or
-- phone in plaintext would turn this table into a PII dump of every address
-- anyone ever typed at the login form, including addresses that never
-- belonged to a user here. Storing the same HMAC blind index the users table
-- is looked up by keeps the counting exact and the plaintext absent.
--
-- user_id is empty when the identifier matched no account. That is not a
-- foreign key to users: cross-module foreign keys are banned repository-wide,
-- and even within this module the login history must survive the deletion of
-- the account it refers to for as long as the audit retention policy says.
--
-- ip_region ships empty for the reason given in the sessions migration.
CREATE TABLE login_attempts (
    id               VARCHAR(36)  NOT NULL,
    user_id          VARCHAR(36)  NOT NULL,
    identifier_index VARCHAR(64)  NOT NULL,
    method           VARCHAR(32)  NOT NULL,
    result           VARCHAR(16)  NOT NULL,
    failure_reason   VARCHAR(64)  NOT NULL,
    session_id       VARCHAR(36)  NOT NULL,
    ip               VARCHAR(45)  NOT NULL,
    ip_region        VARCHAR(128) NOT NULL,
    user_agent       VARCHAR(512) NOT NULL,
    created_at       TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);

CREATE INDEX idx_login_attempts_user_id ON login_attempts (user_id, created_at);

CREATE INDEX idx_login_attempts_identifier ON login_attempts (identifier_index, created_at);
