-- metering_ingest_receipts is metering's billing-grade idempotency ledger
-- (go/metering/ingest_receipt.go): one row per UsageEvent{TenantID,
-- IdempotencyKey} that Aggregator.IngestBillingGrade has folded into the
-- aggregation pipeline. IngestReceipt implements dbkit.TenantScoped and is
-- reached only through IngestReceiptRepository (embedding
-- dbkit.Repository[IngestReceipt]), isolation proven by
-- tenancytest.AssertIsolated.
--
-- This is the PostgreSQL copy; see the sqlite/ sibling for the identical
-- schema on that dialect. Kept portable on purpose: no dialect-specific
-- types, no native arrays, no JSONB, no gen_random_uuid(), no NOW().
--
-- id is the ingested UsageEvent's own IdempotencyKey -- not globally unique
-- across tenants (two different tenants may reuse the same caller-chosen
-- key), which is why tenant_id is a genuine second primary-key column here
-- rather than a plain indexed one, exactly the composite-key shape
-- metering_usage_summaries already uses (see that table's own migration
-- and model.go's UsageSummary doc comment).
--
-- The row itself carries no other columns: its EXISTENCE, keyed by (id,
-- tenant_id), is the entire fact IngestReceiptRepository.Create needs to
-- record -- "this event has already been folded into
-- metering_usage_summaries" -- inserted in the SAME database transaction
-- as that UsageSummary write (via GORM's automatic SAVEPOINT nesting), so
-- either both commit or neither does. A second IngestBillingGrade call for
-- the same event hits this row's own primary key as a unique-constraint
-- violation and applies nothing -- see ingest_receipt.go's IngestReceipt
-- doc comment for the full crash-recovery argument.
CREATE TABLE metering_ingest_receipts (
    id         VARCHAR(200) NOT NULL,
    tenant_id  VARCHAR(64)  NOT NULL,
    created_at TIMESTAMP    NOT NULL,
    PRIMARY KEY (id, tenant_id)
);
