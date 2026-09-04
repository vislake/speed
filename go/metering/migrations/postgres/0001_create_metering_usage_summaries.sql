-- metering_usage_summaries is metering's aggregated usage table
-- (go/metering/model.go): one row per (tenant, feature, calendar period),
-- the SQLite/PostgreSQL "usage_*_summary" table
-- docs/internal/06-billing-and-metering.md's in-process-implementation
-- column names. tenant_id is real and enforced; UsageSummary implements dbkit.TenantScoped
-- and is reached only through SummaryRepository (embedding
-- dbkit.Repository[UsageSummary]), isolation proven by
-- tenancytest.AssertIsolated.
--
-- This is the PostgreSQL copy; see the sqlite/ sibling for the identical
-- schema on that dialect. Kept portable on purpose: no dialect-specific
-- types, no native arrays, no JSONB, no gen_random_uuid(), no NOW().
--
-- id is summaryID(feature, period_start) -- deterministic, not a random
-- UUID -- so Aggregator.upsertSummary can reach the one row for a given
-- (tenant, feature, period) through dbkit.Repository[T].FindByID rather
-- than a hand-written query. Because it is derived from feature and
-- period_start alone, NOT tenant_id, it is not globally unique across
-- tenants, which is why tenant_id is a genuine second primary-key column
-- here rather than a plain indexed one -- see model.go's UsageSummary doc
-- comment for the full reasoning, the same rule go/config's own
-- (key, scope, tenant_id) composite key follows.
--
-- quantity is DOUBLE PRECISION: the running sum of every UsageEvent.Quantity
-- folded into this row so far, updated by Aggregator.upsertSummary under
-- its own single-process serializing mutex (see Aggregator's doc comment
-- for why that is this round's concurrency story, and what a distributed
-- aggregation backend would replace it with).
CREATE TABLE metering_usage_summaries (
    id           VARCHAR(200)     NOT NULL,
    tenant_id    VARCHAR(64)      NOT NULL,
    feature      VARCHAR(128)     NOT NULL,
    period_start TIMESTAMP        NOT NULL,
    period_end   TIMESTAMP        NOT NULL,
    quantity     DOUBLE PRECISION NOT NULL,
    created_at   TIMESTAMP        NOT NULL,
    updated_at   TIMESTAMP        NOT NULL,
    PRIMARY KEY (id, tenant_id)
);

-- Every index below leads with tenant_id, per this codebase's own
-- convention for tenant-scoped tables. idx_metering_usage_summaries_period
-- backs a "this tenant's usage across every feature within one period"
-- dashboard-style query; idx_metering_usage_summaries_feature backs "this
-- tenant's usage for one feature across every period".
CREATE INDEX idx_metering_usage_summaries_period ON metering_usage_summaries (tenant_id, period_start);
CREATE INDEX idx_metering_usage_summaries_feature ON metering_usage_summaries (tenant_id, feature);
