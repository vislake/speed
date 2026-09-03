-- authn's user record: the identity-domain root of this module (see
-- docs/internal/04-data-and-tenancy.md's data-domain table). It deliberately
-- carries NO tenant_id column: one person can belong to several tenants, so
-- scoping the person to one of them would make the "same human, two tenants"
-- case unrepresentable. Membership is a link-domain table owned by the org
-- module, and this module never reads or writes it directly.
--
-- email and phone are encrypted at rest by dbkit's encrypted serializer
-- (authn.SerializerName), so the column holds ciphertext and no query can
-- filter on it. email_index and phone_index are the HMAC blind-index columns
-- that make an exact-match lookup possible anyway -- computed by
-- dbkit.NewBlindIndexer over the canonical form (lowercased email, E.164
-- phone), which is what makes their UNIQUE constraints mean "one account per
-- real-world address" rather than "one account per spelling".
--
-- Both index columns are nullable and both are UNIQUE. That combination is
-- deliberate: a user may register with an email only, a phone only, or both,
-- and SQL unique indexes allow repeated NULLs on both dialects, so
-- "no phone at all" never collides with another account that also has none.
--
-- password_hash holds a PHC-encoded argon2id digest (see password.go). It is
-- NOT NULL with an empty string meaning "this account has no password" -- a
-- social-only or SSO-only account -- so a caller checks one column rather
-- than distinguishing NULL from empty.
--
-- Dual-dialect constraints (root CLAUDE.md): application-generated UUIDs, no
-- gen_random_uuid(), no NOW(), no native arrays, no JSONB filtering. The only
-- difference from the sqlite/ copy of this file is the ciphertext column
-- type, which has no single spelling both dialects accept.
CREATE TABLE users (
    id             VARCHAR(36)  NOT NULL,
    email          BYTEA,
    email_index    VARCHAR(64),
    phone          BYTEA,
    phone_index    VARCHAR(64),
    password_hash  VARCHAR(255) NOT NULL,
    display_name   VARCHAR(128) NOT NULL,
    locale         VARCHAR(16)  NOT NULL,
    status         VARCHAR(32)  NOT NULL,
    email_verified BOOLEAN NOT NULL,
    phone_verified BOOLEAN NOT NULL,
    created_at     TIMESTAMP    NOT NULL,
    updated_at     TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);

CREATE UNIQUE INDEX idx_users_email_index ON users (email_index);

CREATE UNIQUE INDEX idx_users_phone_index ON users (phone_index);
