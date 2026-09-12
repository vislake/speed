-- The smile-simulation domain's credit-reservation ledger
-- (internal/smilesim/reservation_store.go): the durable record of one
-- credit reservation Simulate opened and that no settlement driver has yet
-- acted on -- a row's mere existence IS "not settled" (see the package's
-- "Credit accounting" doc section).
--
-- One copy serves both dialects (the postgres/ file is byte-identical):
-- VARCHAR/TIMESTAMP columns, application-generated ids, no PostgreSQL- or
-- SQLite-specific syntax. The CREATE ... IF NOT EXISTS spelling is the
-- upgrade contract -- a database whose table an earlier boot created
-- without this ledger must migrate cleanly, so the statement is a no-op
-- there and the ledger simply starts recording the file.
--
-- The column set and types match the GORM model tags in
-- reservation_store.go exactly.
CREATE TABLE IF NOT EXISTS smilesim_credit_reservations (
    job_id     VARCHAR(64)  NOT NULL PRIMARY KEY,
    tenant_id  VARCHAR(64)  NOT NULL,
    credit_key VARCHAR(128) NOT NULL,
    created_at TIMESTAMP    NOT NULL
);
