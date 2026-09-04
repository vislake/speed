-- sharing_shares holds public share links (go/sharing/model.go): tenant
-- data -- a share link belongs to the tenant whose resource it exposes.
-- Share implements dbkit.TenantScoped and is reached only through
-- ShareRepository (embedding dbkit.Repository[Share]); isolation proven by
-- tenancytest.AssertIsolated.
--
-- resource_ref is an opaque reference this module never interprets --
-- typically another module's own key scheme (e.g. go/storage's object id),
-- stored as plain data with no foreign key. Cross-module foreign keys are
-- forbidden in this codebase (docs/internal/04-data-and-tenancy.md rule 4).
--
-- token_hash is the hex-encoded SHA-256 of the bearer token a viewer
-- presents. The token itself is NEVER stored -- see token.go's own doc
-- comment; a leaked database backup yields no usable share link.
--
-- expires_at is NOT NULL: rule 2 (docs/internal/07-platform-services.md's
-- "default expiry" rule) requires every share to carry an expiry, and
-- Service.Create resolves a nil caller request into a concrete time before
-- the row is ever written, refusing outright a caller that explicitly asks
-- for one that never expires. This column's NOT NULL constraint is the
-- database-level line of defense behind that application-level rule.
--
-- password_hash, when non-null, is an argon2id PHC digest -- never
-- plaintext (password.go).
--
-- sensitive records whether the caller declared the shared resource as
-- carrying sensitive personal information -- rule 4
-- (docs/internal/07-platform-services.md's "sensitive resource sharing
-- needs confirmation" rule), what makes Service.Create fire the
-- sensitive-share audit action.
--
-- revoked_at is null for a live share; once set (by Service.Revoke or by
-- the expiry sweep, cleanup.go), every access is refused on the very next
-- check -- there is no cache to invalidate.
--
-- This is the PostgreSQL copy; see the sqlite/ sibling for the identical
-- schema on that dialect.
CREATE TABLE sharing_shares (
    id             VARCHAR(36)  NOT NULL,
    tenant_id      VARCHAR(64)  NOT NULL,
    resource_ref   VARCHAR(512) NOT NULL,
    token_hash     VARCHAR(64)  NOT NULL,
    expires_at     TIMESTAMP    NOT NULL,
    max_views      INTEGER,
    view_count     INTEGER      NOT NULL DEFAULT 0,
    password_hash  VARCHAR(255),
    sensitive      BOOLEAN      NOT NULL DEFAULT FALSE,
    revoked_at     TIMESTAMP,
    created_at     TIMESTAMP    NOT NULL,
    updated_at     TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);

-- The bearer-token lookup Service.Access runs on every call. Unique because
-- a token is drawn from 256 bits of crypto/rand -- a collision is
-- cryptographically negligible, and the constraint is cheap insurance, not
-- a meaningful global-uniqueness requirement this module actively defends.
CREATE UNIQUE INDEX uq_sharing_shares_token_hash ON sharing_shares (token_hash);

-- The expiry-sweep listing (cleanup.go's ShareRepository.listExpiredOrExhausted)
-- leads with tenant_id, per this codebase's own convention for tenant-scoped
-- tables.
CREATE INDEX idx_sharing_shares_tenant_expires_at ON sharing_shares (tenant_id, expires_at);
