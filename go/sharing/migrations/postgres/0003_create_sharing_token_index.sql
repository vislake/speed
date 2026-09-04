-- sharing_token_index holds the narrow, deliberately non-tenant-scoped
-- token_hash -> tenant_id mapping go/sharing/model.go's shareTokenIndex
-- documents in full -- the round-2 mechanism that resolves a bearer
-- token's owning tenant before any tenant is known at all, so a genuinely
-- unauthenticated visitor (holding no tenant claim) can still reach the
-- ordinary tenant-scoped Service.Access path (AGENTS.md's "Tenant
-- resolution for an unauthenticated viewer" section).
--
-- This is platform data, deliberately never dbkit.TenantScoped -- the
-- identical treatment go/authn's users table, go/jobs's jobRecord and
-- go/config's row already get, for the same reason: something that must be
-- resolvable before a tenant is known cannot itself be tenant-scoped, and
-- dbkit's tenant-scope GORM plugin fails every tenant-scoped query closed
-- when the context carries no tenant. Reached only through dbkit.Open()'s
-- plain *gorm.DB (repository.go's (*ShareRepository).tenantForTokenHash and
-- createWithTokenIndex), never through dbkit.Repository[T] -- whose generic
-- constraint requires TenantScoped, which this table's model must NOT
-- implement. Isolation proven by tenancytest.AssertNotTenantScoped, not
-- AssertIsolated.
--
-- Deliberately narrow: two columns, nothing else -- no resource_ref, no
-- share_id, no password state. This table answers exactly one question
-- ("which tenant does this token hash belong to") and nothing further;
-- every other question about the share it names is still answered
-- exclusively by the ordinary tenant-scoped sharing_shares row, reached
-- only after this lookup hands back a tenant.
--
-- Written in the same transaction as its sharing_shares row
-- (createWithTokenIndex), and never updated afterward -- Service.Revoke
-- sets only sharing_shares.revoked_at, leaving this row in place, since
-- Service.AccessPublic needs it to resolve a tenant and reach the ordinary
-- Access path even for a token whose share has since been revoked, expired
-- or exhausted.
--
-- This is the PostgreSQL copy; see the sqlite/ sibling for the identical
-- schema on that dialect.
CREATE TABLE sharing_token_index (
    token_hash VARCHAR(64) NOT NULL,
    tenant_id  VARCHAR(64) NOT NULL,
    PRIMARY KEY (token_hash)
);
