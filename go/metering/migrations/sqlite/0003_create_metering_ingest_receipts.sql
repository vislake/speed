-- metering_ingest_receipts is metering's billing-grade idempotency ledger
-- (go/metering/ingest_receipt.go): one row per UsageEvent{TenantID,
-- IdempotencyKey} that Aggregator.IngestBillingGrade has folded into the
-- aggregation pipeline, written in the SAME database transaction as the
-- UsageSummary row it updates -- see IngestReceipt's own doc comment for
-- the crash-recovery double-count bug this table exists to close. Tenant
-- data, exactly like metering_usage_summaries; IngestReceipt implements
-- dbkit.TenantScoped, isolation proven by tenancytest.AssertIsolated.
--
-- This is the SQLite copy; see the postgres/ sibling for the full
-- rationale of every column.
CREATE TABLE metering_ingest_receipts (
    id         VARCHAR(200) NOT NULL,
    tenant_id  VARCHAR(64)  NOT NULL,
    created_at TIMESTAMP    NOT NULL,
    PRIMARY KEY (id, tenant_id)
);
