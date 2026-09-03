-- audit_events is the append-only audit trail (go/dbkit/audit/model.go):
-- one row per "who did what to what, and what happened" fact, covering
-- both tenant-scoped and platform-level actions.
--
-- This is the SQLite copy of the migration; see the postgres/ sibling for
-- the full rationale of every column. The dialect differences stop at the
-- allowed SQL surface: no dialect-specific types, no gen_random_uuid(), no
-- NOW().
--
-- id is an application-generated UUID; actor_type/id/display_name are
-- always populated; on_behalf_of_type/id/display_name are genuinely
-- nullable (NULL means "no impersonation", never an empty-string
-- sentinel); resource_type/id/display_name and success/failure_reason are
-- the flattened Resource and Result elements; changes carries an optional
-- before/after diff as plain JSON text; tenant_id holds the owning tenant
-- or the empty-string sentinel for a platform-level event (never NULL);
-- ip/user_agent/trace_id are request-context metadata, empty when absent.
-- No column here is ever updated or deleted by application code.
CREATE TABLE audit_events (
    id                        VARCHAR(36)   NOT NULL PRIMARY KEY,
    actor_type                VARCHAR(32)   NOT NULL,
    actor_id                  VARCHAR(255)  NOT NULL,
    actor_display_name        VARCHAR(255)  NOT NULL,
    on_behalf_of_type         VARCHAR(32),
    on_behalf_of_id           VARCHAR(255),
    on_behalf_of_display_name VARCHAR(255),
    action                    VARCHAR(255)  NOT NULL,
    resource_type             VARCHAR(255)  NOT NULL,
    resource_id               VARCHAR(255)  NOT NULL,
    resource_display_name     VARCHAR(255)  NOT NULL,
    success                   BOOLEAN       NOT NULL,
    failure_reason            VARCHAR(1000) NOT NULL DEFAULT '',
    changes                   TEXT,
    tenant_id                 VARCHAR(64)   NOT NULL DEFAULT '',
    ip                        VARCHAR(64)   NOT NULL DEFAULT '',
    user_agent                VARCHAR(500)  NOT NULL DEFAULT '',
    trace_id                  VARCHAR(64)   NOT NULL DEFAULT '',
    occurred_at               TIMESTAMP     NOT NULL
);

CREATE INDEX idx_audit_events_tenant_occurred ON audit_events (tenant_id, occurred_at);
