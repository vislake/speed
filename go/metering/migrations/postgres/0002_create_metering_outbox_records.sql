-- metering_outbox_records is metering's billing-grade outbox table
-- (go/metering/model.go): one row per usage event enqueued through
-- Enqueue, written in the SAME transaction as the business write it
-- measures, and delivered asynchronously into the aggregation pipeline by
-- Dispatcher.
--
-- Deliberately PLATFORM data, not tenant-scoped: OutboxRecord does NOT
-- implement dbkit.TenantScoped, is reached through plain *gorm.DB
-- functions (go/metering/repository.go), and isolation is proven by
-- tenancytest.AssertNotTenantScoped, never AssertIsolated. tenant_id is a
-- real, populated column -- every row genuinely belongs to one tenant --
-- it is simply not database-enforced, because Dispatcher must be able to
-- scan pending rows across every tenant at once to retry them, the exact
-- shape go/jobs' own jobRecord is platform data for. See model.go's
-- OutboxRecord doc comment and AGENTS.md's "Outbox table: platform data,
-- not tenant-scoped" section for the full argument.
--
-- This is the PostgreSQL copy; see the sqlite/ sibling for the identical
-- schema on that dialect. Kept portable on purpose: no dialect-specific
-- types, no native arrays, no JSONB, no gen_random_uuid(), no NOW().
--
-- idempotency_key is caller-derived (never randomly generated); the
-- unique index below on (tenant_id, idempotency_key) is what makes
-- Enqueue idempotent under retry -- a second Enqueue call for the same
-- key returns the existing row instead of creating a duplicate.
--
-- status is outboxStatusPending or outboxStatusDelivered -- there is no
-- "processing" transitional state this round, since exactly one
-- in-process Dispatcher runs against this table (see Dispatcher's own doc
-- comment). attempts and last_error record delivery-failure history
-- without ever causing a row to stop being retried: billing-grade
-- delivery retries indefinitely, it does not dead-letter.
CREATE TABLE metering_outbox_records (
    id              VARCHAR(36)  NOT NULL,
    tenant_id       VARCHAR(64)  NOT NULL,
    feature         VARCHAR(128) NOT NULL,
    quantity        DOUBLE PRECISION NOT NULL,
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

-- Enqueue's idempotent-retry guarantee: at most one row per
-- (tenant_id, idempotency_key).
CREATE UNIQUE INDEX uq_metering_outbox_records_tenant_idempotency
    ON metering_outbox_records (tenant_id, idempotency_key);

-- Dispatcher.RunOnce's claim query: pending rows, oldest first.
CREATE INDEX idx_metering_outbox_records_status_created
    ON metering_outbox_records (status, created_at);
