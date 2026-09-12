-- The case domain's case_photos child table (internal/cases): one row per
-- photo attached to a case, with the client's attachment order in the
-- position column (see internal/cases/model.go for why a child table
-- rather than a JSON array on the case row).
--
-- One copy serves both dialects (the postgres/ file is byte-identical):
-- VARCHAR/TIMESTAMP columns, application-generated ids, no PostgreSQL- or
-- SQLite-specific syntax. The CREATE ... IF NOT EXISTS spelling is the
-- upgrade contract -- a database whose table an earlier boot created
-- without this ledger must migrate cleanly, so the statement is a no-op
-- there and the ledger simply starts recording the file.
--
-- The column set and types match the GORM model tags in
-- internal/cases/model.go exactly. position is an INTEGER because it is a
-- number, and "position" is a non-reserved word on both dialects (it is a
-- function name in PostgreSQL's grammar, never a keyword a column cannot
-- use).
CREATE TABLE IF NOT EXISTS case_photos (
    id         VARCHAR(36) NOT NULL PRIMARY KEY,
    tenant_id  VARCHAR(64) NOT NULL,
    case_id    VARCHAR(36) NOT NULL,
    object_id  VARCHAR(64) NOT NULL,
    position   INTEGER     NOT NULL,
    created_at TIMESTAMP   NOT NULL
);

-- The lookup index behind listing one case's photos, ordered by position:
-- it also backs the (tenant_id, case_id, position) scan order of the
-- detail read.
CREATE INDEX IF NOT EXISTS idx_case_photos_tenant_case ON case_photos (tenant_id, case_id);

-- The uniqueness index behind the "one tenant, one case per photo object"
-- rule: within a tenant, an object id may be attached to at most one case
-- (internal/cases/service.go refuses the second attachment with a coded
-- conflict). It is UNIQUE where the other index is plain because it
-- enforces a domain rule, not merely a lookup shape.
CREATE UNIQUE INDEX IF NOT EXISTS uq_case_photos_tenant_object ON case_photos (tenant_id, object_id);
