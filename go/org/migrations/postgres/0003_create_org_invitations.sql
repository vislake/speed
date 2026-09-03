-- org_invitations holds the pending offers to join a tenant at a particular
-- node of its organization tree (go/org/invitation.go).
--
-- Data domain: tenant data. An invitation belongs to exactly one tenant and
-- must never be visible from another, so the model implements
-- dbkit.TenantScoped and tenancytest.AssertIsolated is mandatory for it.
--
-- email holds CIPHERTEXT, not an address. The column is written through the
-- GORM serializer registered under org.EmailSerializerName, so the bytes on
-- disk are AES-256-GCM output and a database backup leaks no addresses. The
-- column type is the dialect's binary type (BYTEA here, BLOB in the sqlite/ sibling).
--
-- email_index is the HMAC-SHA256 blind index of the normalized address
-- (dbkit.NewBlindIndexer over dbkit.NormalizeEmail), and it is the ONLY way
-- this table can be searched by address: an encrypted column cannot be
-- queried, which is the trap the root CLAUDE.md warns about by name. It is a
-- keyed digest, safe to index and safe to log; the address is neither.
--
-- token_hash is the hex SHA-256 of the invitation token. THE TOKEN ITSELF IS
-- NEVER STORED: it is handed to the caller once and lives only in the message
-- addressed to the invitee, so a leaked backup yields no usable link.
-- uq_org_invitations_token makes a token unique within its tenant, and
-- acceptance is a lookup on this column inside the tenant of the request --
-- the tenant is never read out of the token.
--
-- inviter_user_id references authn's users.id with no foreign key, for the
-- reason memberships' own header gives.
--
-- locale records the language the invitation was rendered in, captured when
-- the invitation was created. Backend-generated content renders in the
-- RECIPIENT's locale, and the recipient may not be a user at all, so there is
-- no profile to read it from later.
--
-- status holds the closed set go/org/invitation.go declares
-- ("pending" / "accepted" / "revoked"), as a plain VARCHAR rather than an
-- enum type, which SQLite does not have.
--
-- expires_at is evaluated at acceptance time against the service clock, so no
-- sweeper is needed for correctness. accepted_at is the one nullable column
-- here: it is genuinely absent until acceptance, and nothing indexes it, so
-- the empty-string sentinel org_nodes.parent_id needs has no reason to exist.
--
-- created_at / updated_at are written by gorm's autoCreateTime and
-- autoUpdateTime, never by a database default.

CREATE TABLE org_invitations (
    id              VARCHAR(36) NOT NULL,
    tenant_id       VARCHAR(64) NOT NULL,
    node_id         VARCHAR(36) NOT NULL,
    email           BYTEA       NOT NULL,
    email_index     VARCHAR(64) NOT NULL,
    inviter_user_id VARCHAR(64) NOT NULL,
    locale          VARCHAR(16) NOT NULL,
    token_hash      VARCHAR(64) NOT NULL,
    status          VARCHAR(16) NOT NULL,
    expires_at      TIMESTAMP   NOT NULL,
    accepted_at     TIMESTAMP,
    created_at      TIMESTAMP   NOT NULL,
    updated_at      TIMESTAMP   NOT NULL,
    PRIMARY KEY (id)
);

-- Acceptance looks a token hash up inside the tenant of the request.
CREATE UNIQUE INDEX uq_org_invitations_token ON org_invitations (tenant_id, token_hash);

-- The blind-index lookup: "does this address already have a live invitation
-- here?", asked before a new one is issued so one address never holds two
-- valid tokens at once.
CREATE INDEX idx_org_invitations_tenant_email ON org_invitations (tenant_id, email_index);

-- The pending-invitation listing.
CREATE INDEX idx_org_invitations_tenant_status ON org_invitations (tenant_id, status);
