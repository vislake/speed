-- metering_usage_summaries is metering's aggregated usage table
-- (go/metering/model.go): one row per (tenant, feature, calendar period).
-- tenant_id is real and enforced; UsageSummary implements
-- dbkit.TenantScoped, isolation proven by tenancytest.AssertIsolated.
--
-- This is the SQLite copy; see the postgres/ sibling for the full
-- rationale of every column. The dialect differences stop at the allowed
-- SQL surface: no dialect-specific types, no native arrays, no JSONB, no
-- gen_random_uuid(), no NOW().
CREATE TABLE metering_usage_summaries (
    id           VARCHAR(200) NOT NULL,
    tenant_id    VARCHAR(64)  NOT NULL,
    feature      VARCHAR(128) NOT NULL,
    period_start TIMESTAMP    NOT NULL,
    period_end   TIMESTAMP    NOT NULL,
    quantity     REAL         NOT NULL,
    created_at   TIMESTAMP    NOT NULL,
    updated_at   TIMESTAMP    NOT NULL,
    PRIMARY KEY (id, tenant_id)
);

CREATE INDEX idx_metering_usage_summaries_period ON metering_usage_summaries (tenant_id, period_start);
CREATE INDEX idx_metering_usage_summaries_feature ON metering_usage_summaries (tenant_id, feature);
