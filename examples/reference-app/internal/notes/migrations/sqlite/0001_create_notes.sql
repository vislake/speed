-- notes is examples/reference-app's tenant-scoped business module: a
-- minimal text record demonstrating real, end-to-end usage of the speed
-- module stack (see examples/reference-app's doc.go and internal/notes's
-- own package doc). Kept portable between PostgreSQL and SQLite on
-- purpose (see the postgres/ copy of this file), under the same
-- dual-dialect constraints every migration follows: no dialect-specific
-- types, no gen_random_uuid(), no NOW().
--
-- id is an application-generated UUID (see internal/notes/handler.go),
-- already globally unique on its own, so it alone is the primary key
-- here. tenant_id gets its own secondary index instead of participating
-- in a composite primary key; see internal/notes/model.go's doc comment
-- on Note for the rationale.
CREATE TABLE notes (
    id         VARCHAR(36) NOT NULL,
    tenant_id  VARCHAR(64) NOT NULL,
    text       VARCHAR(4000) NOT NULL,
    created_at TIMESTAMP NOT NULL,
    PRIMARY KEY (id)
);

CREATE INDEX idx_notes_tenant_id ON notes (tenant_id);
