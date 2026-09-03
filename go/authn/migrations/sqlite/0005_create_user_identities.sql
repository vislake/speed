-- authn's external identity bindings: one row per (social account or
-- enterprise single sign-on subject) attached to a user.
--
-- IDENTITY-domain data, like users and sessions (see
-- docs/internal/04-data-and-tenancy.md). No tenant_id column: the person is
-- the same person in every tenant they belong to, and a per-tenant copy of
-- their GitHub account would be meaningless. The AssertNotTenantScoped suite
-- in identity_test.go fails if the Go model ever starts claiming otherwise.
--
-- The UNIQUE index on (provider, external_id) is a security control, not an
-- optimisation. It is what makes "this external account is already bound to
-- somebody" a constraint the database enforces, rather than a check-then-act
-- race between two concurrent sign-ins that both find nothing and both bind.
--
-- provider holds either a platform channel name ("google", "github",
-- "wechat", "dingtalk", "feishu") or an enterprise channel "oidc:<tenant_id>".
-- The tenant appears in the provider name rather than in external_id so that
-- external_id stays exactly the "sub" claim the identity provider issued,
-- which is the value an operator will compare against their own directory.
-- external_id is sized at 191 rather than 255 because that is the largest
-- prefix a utf8mb4 unique index tolerates on the MySQL-family engines a
-- consuming project might later add; neither supported dialect needs it, and
-- no provider issues a subject anywhere near that long.
--
-- email is encrypted at rest by dbkit's serializer and has NO blind index,
-- deliberately: it is display data for the settings page and for support, and
-- is never used to find an account. Looking an account up by a third party's
-- email address is exactly the takeover this module refuses.
--
-- Dual-dialect constraints (root CLAUDE.md): application-generated UUIDs, no
-- gen_random_uuid(), no NOW(), no native arrays, no JSONB filtering. The only
-- difference from the sqlite/ copy of this file is the ciphertext column type.
CREATE TABLE user_identities (
    id            VARCHAR(36)  NOT NULL,
    user_id       VARCHAR(36)  NOT NULL,
    provider      VARCHAR(64)  NOT NULL,
    external_id   VARCHAR(191) NOT NULL,
    email         BLOB,
    display_name  VARCHAR(128) NOT NULL,
    avatar_url    VARCHAR(512) NOT NULL,
    created_at    TIMESTAMP    NOT NULL,
    updated_at    TIMESTAMP    NOT NULL,
    last_login_at TIMESTAMP,
    PRIMARY KEY (id)
);

CREATE UNIQUE INDEX idx_user_identities_provider_external ON user_identities (provider, external_id);

CREATE INDEX idx_user_identities_user_id ON user_identities (user_id);
