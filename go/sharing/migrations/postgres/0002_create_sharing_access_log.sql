-- sharing_access_log holds one row per attempted access against a Share
-- (go/sharing/model.go), granted or denied alike: tenant data, isolation
-- proven by tenancytest.AssertIsolated. Append-only -- this module never
-- updates or deletes a row here, mirroring go/dbkit/audit.AuditEvent's
-- identical append-only shape (no Update, no Delete method on
-- AccessLogRepository at all).
--
-- share_id names a sharing_shares row, deliberately without a SQL FOREIGN
-- KEY constraint -- this codebase avoids FK constraints even for
-- same-module references (see go/storage's object_derivatives.object_id
-- for the cross-module precedent this follows even though share_id is not
-- cross-module).
--
-- ip, user_agent and referrer are recorded as given by the caller of
-- Service.Access; this module neither parses nor validates them.
--
-- This is the PostgreSQL copy; see the sqlite/ sibling for the identical
-- schema on that dialect.
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
