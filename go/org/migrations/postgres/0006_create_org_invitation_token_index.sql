-- org_invitation_token_index holds the narrow, deliberately
-- non-tenant-scoped token_hash -> tenant_id mapping go/org/invitation.go's
-- invitationTokenIndex documents in full -- the mechanism that lets a
-- genuinely tenantless caller (a freshly invited person, who by definition
-- has no membership in -- and typically no bearer token for -- the inviting
-- tenant yet) accept an invitation: InviteService.Accept resolves the
-- tenant from the presented token here, attaches it to ctx with
-- pkgcore.WithTenant, and re-enters the ordinary tenant-scoped accept flow
-- unchanged (invite.go's Accept doc comment). It is go/sharing's
-- sharing_token_index pattern, adopted for org's invitation token.
--
-- This is platform data, deliberately never dbkit.TenantScoped -- the
-- identical treatment go/authn's users table, go/jobs's jobRecord,
-- go/config's row, go/dbkit/audit's AuditEvent and go/sharing's own token
-- index already get, for the same reason: something that must be resolvable
-- before a tenant is known cannot itself be tenant-scoped, and dbkit's
-- tenant-scope GORM plugin fails every tenant-scoped query closed when the
-- context carries no tenant. Reached only through dbkit.Open()'s plain
-- *gorm.DB (invitation.go's (*InvitationRepository).tenantForTokenHash and
-- createPending), never through dbkit.Repository[T] -- whose generic
-- constraint requires TenantScoped, which this table's model must NOT
-- implement. Isolation proven by tenancytest.AssertNotTenantScoped, not
-- AssertIsolated.
--
-- Deliberately narrow: two columns, nothing else -- no invitation_id, no
-- node_id, no status, no expiry. This table answers exactly one question
-- ("which tenant does this token hash belong to") and nothing further; every
-- other question about the invitation it names is still answered
-- exclusively by the ordinary tenant-scoped org_invitations row, reached
-- only after this lookup hands back a tenant.
--
-- Written in the same transaction as its org_invitations row
-- (createPending), and never updated afterward -- Accept and Revoke leave
-- it in place, since a tenantless Accept needs it to resolve a tenant and
-- reach the ordinary tenant-scoped path even for a token whose invitation
-- has already been accepted or revoked, exactly how that path is meant to
-- answer the case (org.invitation_already_accepted / org.invitation_revoked,
-- not a dead end before it is ever reached).
--
-- This is the PostgreSQL copy; see the sqlite/ sibling for the identical
-- schema on that dialect.
CREATE TABLE org_invitation_token_index (
    token_hash VARCHAR(64) NOT NULL,
    tenant_id  VARCHAR(64) NOT NULL,
    PRIMARY KEY (token_hash)
);
