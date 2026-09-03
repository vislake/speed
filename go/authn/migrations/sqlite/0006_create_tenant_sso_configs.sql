-- authn's per-tenant enterprise single sign-on configuration: which OpenID
-- Connect identity provider a tenant federates with.
--
-- This is the ONE tenant-domain table this module owns (see
-- docs/internal/05-identity-and-access.md, which specifies exactly one
-- configuration row per tenant). Everything else authn stores is identity
-- data with no tenant column at all, so the difference is worth stating
-- plainly: rows here belong to a tenant, are reached exclusively through
-- dbkit.Repository[TenantSSOConfig], and are pinned by tenancytest.AssertIsolated
-- in oidc_test.go -- while users, sessions, refresh_tokens, login_attempts and
-- user_identities are pinned by AssertNotTenantScoped.
--
-- The primary key is the composite (tenant_id, id), with tenant_id leftmost,
-- which is the backend coding standard's rule for every tenant-scoped table.
--
-- There is deliberately NO separate UNIQUE index on tenant_id alone. "At
-- most one configuration per tenant" is a product rule enforced by
-- SaveConfig, which always reads Current before deciding whether to create
-- or update -- a database-level constraint on tenant_id would additionally
-- reject the second of two rows the MANDATORY tenancytest.AssertIsolated
-- suite deliberately creates per tenant (List cannot be proven to filter by
-- tenant from a single-row result). See TenantSSOConfig.TenantID and
-- SSOConfigRepository.Current in oidc.go for the full reasoning and for how
-- "at most one" is kept true in the normal path regardless.
--
-- client_secret is encrypted at rest by dbkit's serializer. A tenant
-- administrator's relying-party secret sitting in plaintext in a table shared
-- by every tenant is one careless database export away from being a breach,
-- and unlike a password it cannot be rotated by the person it belongs to
-- without noticing first.
--
-- allowed_domains is a whitespace-delimited list rather than a native array
-- (PostgreSQL only, banned) or a JSON document that would then need JSONB
-- operators to filter (also banned). Nothing filters on it: it is read whole
-- and compared in Go.
--
-- Dual-dialect constraints (root CLAUDE.md): application-generated UUIDs, no
-- gen_random_uuid(), no NOW(), no native arrays, no JSONB filtering. The only
-- difference from the sqlite/ copy of this file is the ciphertext column type.
CREATE TABLE tenant_sso_configs (
    tenant_id       VARCHAR(64)   NOT NULL,
    id              VARCHAR(36)   NOT NULL,
    enabled         BOOLEAN       NOT NULL,
    issuer          VARCHAR(512)  NOT NULL,
    client_id       VARCHAR(255)  NOT NULL,
    client_secret   BLOB,
    allowed_domains VARCHAR(1024) NOT NULL,
    created_at      TIMESTAMP     NOT NULL,
    updated_at      TIMESTAMP     NOT NULL,
    PRIMARY KEY (tenant_id, id)
);
