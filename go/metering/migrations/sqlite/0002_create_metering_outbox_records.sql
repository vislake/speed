-- metering_outbox_records is metering's billing-grade outbox table
-- (go/metering/model.go). Deliberately PLATFORM data, not tenant-scoped --
-- see the postgres/ sibling and model.go's OutboxRecord doc comment for
-- the full argument. tenant_id is real and populated but not
-- database-enforced; isolation is proven by
-- tenancytest.AssertNotTenantScoped.
--
-- This is the SQLite copy; see the postgres/ sibling for the full
-- rationale of every column.
CREATE TABLE metering_outbox_records (
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
    created_at      TIMESTAMP    NOT NULL,
    delivered_at    TIMESTAMP,
    PRIMARY KEY (id)
);

CREATE UNIQUE INDEX uq_metering_outbox_records_tenant_idempotency
    ON metering_outbox_records (tenant_id, idempotency_key);

CREATE INDEX idx_metering_outbox_records_status_created
    ON metering_outbox_records (status, created_at);
