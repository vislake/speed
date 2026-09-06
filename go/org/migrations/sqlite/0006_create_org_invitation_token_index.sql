-- org_invitation_token_index holds the narrow, deliberately
-- non-tenant-scoped token_hash -> tenant_id mapping go/org/invitation.go's
-- invitationTokenIndex documents in full: platform data, never
-- dbkit.TenantScoped, reached only through dbkit.Open()'s plain *gorm.DB
-- (invitation.go's (*InvitationRepository).tenantForTokenHash and
-- createPending), never through dbkit.Repository[T]. Isolation proven by
-- tenancytest.AssertNotTenantScoped, not AssertIsolated.
--
-- This is the SQLite copy; see the postgres/ sibling for the full
-- rationale. The dialect differences stop at the allowed SQL surface: no
-- dialect-specific types, no native arrays, no JSONB, no gen_random_uuid(),
-- no NOW().
CREATE TABLE org_invitation_token_index (
    token_hash VARCHAR(64) NOT NULL,
    tenant_id  VARCHAR(64) NOT NULL,
    PRIMARY KEY (token_hash)
);
