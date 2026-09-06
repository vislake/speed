-- Backs InviteService's own claim (invite.go: "one address has at most one
-- live token at a time") with a real database constraint, closing the P3
-- concurrency finding revokePendingFor's read-then-write-loop shape used to
-- leave open: two concurrent InviteService.Invite calls for the SAME
-- address could both observe no pending invitation to revoke and both
-- INSERT, landing two simultaneously live tokens for one address --
-- something the module's own doc comment claimed could never happen.
--
-- A partial unique index, scoped WHERE status = 'pending' exactly like
-- 0004_add_soft_delete.sql's own uq_org_nodes_sibling_name and
-- uq_memberships_tenant_user: an address may hold any number of ACCEPTED or
-- REVOKED rows over its history (each is a permanent, append-only fact this
-- index must never block), but at most one PENDING row at a time. Since
-- Invite always revokes whatever was pending for the address before it
-- inserts the new row (in the same transaction as of this round -- see
-- invite.go's revokePendingFor), the ordinary, uncontended path never
-- touches this index at all; it exists purely as the database-arbitrated
-- backstop for the race two concurrent Invite calls create, translated by
-- InviteService.Invite into the coded ErrInvitationAlreadyPending rather
-- than a raw gorm.ErrDuplicatedKey.
--
-- This is the sqlite/ copy; the postgres/ sibling carries the full
-- rationale. The two are byte-identical -- partial unique indexes are
-- standard SQL both dialects support with the same syntax.
CREATE UNIQUE INDEX uq_org_invitations_pending_email
    ON org_invitations (tenant_id, email_index)
    WHERE status = 'pending';
