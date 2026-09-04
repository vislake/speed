-- sharing_access_log holds one row per attempted access against a Share
-- (go/sharing/model.go), granted or denied alike: tenant data, isolation
-- proven by tenancytest.AssertIsolated. Append-only -- this module never
-- updates or deletes a row here.
--
-- This is the SQLite copy; see the postgres/ sibling for the full
-- rationale of every column.
CREATE TABLE sharing_access_log (
    id           VARCHAR(36)  NOT NULL,
    tenant_id    VARCHAR(64)  NOT NULL,
    share_id     VARCHAR(36)  NOT NULL,
    occurred_at  TIMESTAMP    NOT NULL,
    ip           VARCHAR(64)  NOT NULL DEFAULT '',
    user_agent   VARCHAR(512) NOT NULL DEFAULT '',
    referrer     VARCHAR(512) NOT NULL DEFAULT '',
    outcome      VARCHAR(16)  NOT NULL,
    PRIMARY KEY (id)
);

-- Service.ListAccessLog's own listing: every row of one share, newest
-- first. Leads with tenant_id per this codebase's own convention.
CREATE INDEX idx_sharing_access_log_tenant_share ON sharing_access_log (tenant_id, share_id, occurred_at DESC);
