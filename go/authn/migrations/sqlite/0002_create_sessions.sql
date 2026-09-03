-- One row per login. A session belongs to a USER, not to a tenant
-- (docs/internal/05-identity-and-access.md's section on how sessions relate to tenants): within one
-- session the user switches tenants freely, and each switch issues a new
-- access token carrying the new tenant while the session and its refresh
-- token stay the same. Revoking a session therefore signs the device out of
-- every tenant at once, which is exactly the intended meaning of "sign this
-- device out".
--
-- amr is the space-delimited list of authentication methods this session was
-- established with ("password", "mfa:totp", "social:google"). The delimiter
-- follows the JWT/OAuth convention for amr and scope rather than a native
-- array (banned: PostgreSQL-only) or a JSON document that would then have to
-- be filtered with JSONB operators (also banned).
--
-- ip_region ships empty on purpose. Resolving an IP to a region needs a local
-- GeoIP database whose licence has to clear the licence scanner before
-- commercial delivery (docs/internal/05-identity-and-access.md says so
-- explicitly), so the column exists now -- no later table migration -- and
-- the resolver that fills it lands with that decision.
--
-- current_tenant_id is NOT a tenant-scoping column and this table stays
-- identity-domain data. It records which tenant this session's access tokens
-- are CURRENTLY being issued for, so a refresh knows what to mint and a
-- tenant switch has somewhere to record itself; the session still belongs to
-- the user and still spans every tenant they are a member of. Membership is
-- re-checked against it on every refresh, which is also what makes removing
-- someone from a tenant take effect on their existing session rather than
-- waiting for it to expire.
CREATE TABLE sessions (
    id            VARCHAR(36)  NOT NULL,
    user_id       VARCHAR(36)  NOT NULL,
    status        VARCHAR(32)  NOT NULL,
    current_tenant_id VARCHAR(64) NOT NULL,
    amr           VARCHAR(255) NOT NULL,
    device        VARCHAR(255) NOT NULL,
    user_agent    VARCHAR(512) NOT NULL,
    ip            VARCHAR(45)  NOT NULL,
    ip_region     VARCHAR(128) NOT NULL,
    created_at    TIMESTAMP    NOT NULL,
    last_seen_at  TIMESTAMP    NOT NULL,
    expires_at    TIMESTAMP    NOT NULL,
    revoked_at    TIMESTAMP,
    revoke_reason VARCHAR(64)  NOT NULL,
    PRIMARY KEY (id)
);

CREATE INDEX idx_sessions_user_id ON sessions (user_id, created_at);
