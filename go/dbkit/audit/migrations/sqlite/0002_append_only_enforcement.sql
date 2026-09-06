-- See the postgres/ sibling for the full rationale: audit_events must
-- refuse any UPDATE or DELETE at the database level, not only through
-- Repository's application-layer absence of a mutating method.
--
-- SQLite has no role/GRANT model to revoke a privilege from in the first
-- place, so a trigger is not merely the more portable choice here -- it
-- is the only mechanism available, and it is the standard, documented way
-- SQLite itself recommends for making a table immutable: a BEFORE
-- UPDATE/DELETE trigger whose body unconditionally RAISEs, matching the
-- identical mechanism SHAPE the postgres/ migration uses (a trigger per
-- write verb that always fails), even though the two dialects' trigger
-- syntax cannot be byte-identical (SQLite's trigger body is inline SQL,
-- with no separate function/language declaration the way PostgreSQL's
-- PL/pgSQL trigger function needs).
--
-- INSERT is completely unaffected -- neither trigger below is declared
-- BEFORE INSERT, so Repository.Insert/InsertIdempotent keep working
-- exactly as before.
CREATE TRIGGER trg_audit_events_reject_update
BEFORE UPDATE ON audit_events
BEGIN
    SELECT RAISE(ABORT, 'audit_events is append-only: UPDATE is not permitted');
END;

CREATE TRIGGER trg_audit_events_reject_delete
BEFORE DELETE ON audit_events
BEGIN
    SELECT RAISE(ABORT, 'audit_events is append-only: DELETE is not permitted');
END;
