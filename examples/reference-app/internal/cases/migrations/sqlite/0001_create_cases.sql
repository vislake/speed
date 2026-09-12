-- The case domain's cases table (internal/cases): one row per case, with
-- the clinic-given patient record embedded as the two fields an intake
-- form provides (see internal/cases/model.go for the shape decision).
--
-- One copy serves both dialects (the postgres/ file is byte-identical):
-- VARCHAR/TIMESTAMP columns, application-generated ids, no PostgreSQL- or
-- SQLite-specific syntax. The CREATE ... IF NOT EXISTS spelling is the
-- upgrade contract -- a database whose table an earlier boot created
-- without this ledger must migrate cleanly, so the statement is a no-op
-- there and the ledger simply starts recording the file.
--
-- The column set and types match the GORM model tags in
-- internal/cases/model.go exactly.
CREATE TABLE IF NOT EXISTS cases (
    id              VARCHAR(36)  NOT NULL PRIMARY KEY,
    tenant_id       VARCHAR(64)  NOT NULL,
    patient_name    VARCHAR(200) NOT NULL,
    patient_ref     VARCHAR(64)  NOT NULL DEFAULT '',
    creator_user_id VARCHAR(64)  NOT NULL DEFAULT '',
    created_at      TIMESTAMP    NOT NULL
);

-- The lookup index behind Service.List's clinic-wide enumeration: every
-- case of one tenant, newest first. Indexes cannot be declared portably
-- inside CREATE TABLE, and the model tags' index declarations only matter
-- to AutoMigrate, which this app never runs.
CREATE INDEX IF NOT EXISTS idx_cases_tenant_created ON cases (tenant_id, created_at DESC);
