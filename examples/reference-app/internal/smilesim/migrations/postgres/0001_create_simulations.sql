-- The smile-simulation domain's per-photo result index
-- (internal/smilesim/simulation_store.go): one append-only row per
-- generation request, recording which photo it was generated from and
-- which job carries its outcome.
--
-- One copy serves both dialects (the postgres/ file is byte-identical):
-- VARCHAR/TIMESTAMP columns, application-generated ids, no PostgreSQL- or
-- SQLite-specific syntax. The CREATE ... IF NOT EXISTS spelling is the
-- upgrade contract -- a database whose table an earlier boot created
-- without this ledger must migrate cleanly, so the statement is a no-op
-- there and the ledger simply starts recording the file.
--
-- The column set and types match the GORM model tags in
-- simulation_store.go exactly.
CREATE TABLE IF NOT EXISTS smilesim_simulations (
    job_id          VARCHAR(64)  NOT NULL PRIMARY KEY,
    tenant_id       VARCHAR(64)  NOT NULL,
    photo_object_id VARCHAR(64)  NOT NULL,
    options_json    VARCHAR(256) NOT NULL,
    created_at      TIMESTAMP    NOT NULL
);

-- The lookup index behind ListSimulationsByPhoto's per-photo enumeration.
-- Indexes cannot be declared portably inside CREATE TABLE.
CREATE INDEX IF NOT EXISTS idx_smilesim_simulations_tenant_photo ON smilesim_simulations (tenant_id, photo_object_id);
