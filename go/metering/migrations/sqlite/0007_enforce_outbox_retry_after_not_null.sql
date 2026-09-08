-- Makes metering_outbox_records.retry_after NOT NULL on SQLite -- the
-- schema-force half of the decision to make NULL structurally
-- impossible; the postgres/ sibling carries the full rationale (why NOT
-- NULL was chosen over dropping the claim query's COALESCE on a
-- still-nullable column, and over declaring the index/order-key mismatch
-- a known trade-off). End state identical to the PostgreSQL copy: NULL
-- is structurally impossible from this migration on, and the claim
-- query's COALESCE and IS NULL escape are dead and removed
-- (go/metering/repository.go's claimPendingOutboxRecords).
--
-- SQLite cannot alter an existing column's nullability (no ALTER COLUMN
-- SET NOT NULL), so the enforcement is a table rebuild: the table is
-- recreated with 0002's exact shape plus retry_after NOT NULL, the rows
-- are copied over, the old table is dropped and the copy renamed, and
-- the two indexes -- dropped with the old table -- are recreated. The
-- rebuild is the standard SQLite price for a nullability change, safe
-- here because the registry runs this file inside the module's own
-- migration transaction (dbkit.MigrationRegistry applies each module's
-- new files atomically): a failure anywhere in the file leaves the
-- pre-0007 schema untouched.
--
-- The same idempotent backfill 0005 ran runs FIRST, exactly as in the
-- postgres/ copy -- a database that applied 0005 has no NULL rows, so
-- the UPDATE matches zero rows there (no-op, the in-file precedent for
-- re-running it), while a row that somehow still carries NULL is
-- converted before the copy lands it in a NOT NULL column.
--
-- NOT NULL with no DEFAULT, deliberately (the postgres/ sibling carries
-- the rationale): a constant retry-schedule default would be a lie, and
-- any write path that forgets retry_after fails loudly at the
-- constraint rather than writing a silently-wrong schedule.
--
-- This is the SQLite copy; the postgres/ sibling carries the full
-- rationale. The two end schema-identical.
UPDATE metering_outbox_records
   SET retry_after = created_at
 WHERE retry_after IS NULL;

CREATE TABLE metering_outbox_records_new (
    id              VARCHAR(36)  NOT NULL,
    tenant_id       VARCHAR(64)  NOT NULL,
    feature         VARCHAR(128) NOT NULL,
    quantity        REAL         NOT NULL,
    idempotency_key VARCHAR(200) NOT NULL,
    occurred_at     TIMESTAMP    NOT NULL,
    metadata        TEXT         NOT NULL DEFAULT '',
    status          VARCHAR(16)  NOT NULL,
    attempts        INTEGER      NOT NULL DEFAULT 0,
    last_error      VARCHAR(500) NOT NULL DEFAULT '',
    retry_after     TIMESTAMP    NOT NULL,
    created_at      TIMESTAMP    NOT NULL,
    delivered_at    TIMESTAMP,
    PRIMARY KEY (id)
);

INSERT INTO metering_outbox_records_new
    (id, tenant_id, feature, quantity, idempotency_key, occurred_at,
     metadata, status, attempts, last_error, retry_after, created_at,
     delivered_at)
SELECT id, tenant_id, feature, quantity, idempotency_key, occurred_at,
       metadata, status, attempts, last_error, retry_after, created_at,
       delivered_at
  FROM metering_outbox_records;

DROP TABLE metering_outbox_records;

ALTER TABLE metering_outbox_records_new RENAME TO metering_outbox_records;

CREATE UNIQUE INDEX uq_metering_outbox_records_tenant_idempotency
    ON metering_outbox_records (tenant_id, idempotency_key);

CREATE INDEX idx_metering_outbox_records_status_retry_after
    ON metering_outbox_records (status, retry_after);
