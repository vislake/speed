-- audit_events is meant to be genuinely append-only: model.go's and
-- repository.go's own doc comments already describe Repository as
-- exposing no Update or Delete method at all -- but that absence is an
-- APPLICATION-layer discipline only. Nothing before this migration stops
-- a raw UPDATE or DELETE issued through some other path (a bug, a future
-- careless migration, a direct database console session, a *gorm.DB
-- obtained some other way than dbkit.Open) from mutating or physically
-- removing a row.
--
-- This migration adds the database-level backstop
-- docs/internal/10-compliance-and-audit.md's "tamper-proof" (immutability)
-- section calls for: a BEFORE UPDATE/DELETE trigger that raises an
-- exception on any attempt, regardless of which database role or
-- connection issues the statement.
--
-- Trigger, not REVOKE-from-a-restricted-role: go/dbkit's Open
-- (go/dbkit/open.go) opens exactly one connection/role per *gorm.DB --
-- the very same one every other table's reads and writes already go
-- through -- and provisioning a second, more restricted role for this one
-- table alone is explicitly a deployment-side responsibility this
-- codebase assumes but does not create (go/dbkit/AGENTS.md's "Out of
-- scope": "provisioning the PostgreSQL roles and RLS policies isolation
-- layer 3 depends on ... is a deployment-side responsibility this
-- package assumes but does not create"). A REVOKE-based scheme would
-- therefore either have no effect (the application keeps connecting as
-- whatever single role its DSN names, which this migration cannot itself
-- provision or rename) or would need this codebase to start owning role
-- provisioning it deliberately does not own elsewhere. A trigger has
-- neither problem: it enforces the guarantee against ANY role that
-- connects, including a future superuser session at a console, with no
-- role-provisioning prerequisite of any kind -- the honestly enforceable
-- choice given how this connection is actually established.
--
-- INSERT is completely unaffected -- Repository.Insert/InsertIdempotent
-- keep working exactly as before, since neither trigger's event matches
-- an INSERT.
CREATE FUNCTION audit_events_reject_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_audit_events_reject_update
    BEFORE UPDATE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_reject_mutation();

CREATE TRIGGER trg_audit_events_reject_delete
    BEFORE DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_reject_mutation();
