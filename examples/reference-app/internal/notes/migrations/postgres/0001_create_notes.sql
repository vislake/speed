-- notes is examples/reference-app's placeholder tenant-scoped resource: a
-- minimal text record standing in for the real reference-app content that
-- lands in later milestones (see examples/reference-app's doc.go and
-- internal/notes's own package doc). Kept portable between PostgreSQL and
-- SQLite on purpose (see the sqlite/ copy of this file), following the
-- same dual-dialect constraints every real module's migrations must
-- follow: no dialect-specific types, no gen_random_uuid(), no NOW().
--
-- id is an application-generated UUID (see internal/notes/handler.go),
-- already globally unique on its own, so it alone is the primary key here.
-- tenant_id gets its own secondary index instead of participating in a
-- composite primary key -- see internal/notes/model.go's doc comment on
-- Note for why this particular resource does not need the composite
-- (tenant_id, id) primary key the backend coding standard otherwise
-- recommends for tenant-scoped tables.
CREATE TABLE notes (
    id         VARCHAR(36) NOT NULL,
    tenant_id  VARCHAR(64) NOT NULL,
    text       VARCHAR(4000) NOT NULL,
    created_at TIMESTAMP NOT NULL,
    PRIMARY KEY (id)
);

CREATE INDEX idx_notes_tenant_id ON notes (tenant_id);
