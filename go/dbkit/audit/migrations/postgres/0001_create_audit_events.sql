-- audit_events is the append-only audit trail (go/dbkit/audit/model.go):
-- one row per "who did what to what, and what happened" fact, covering
-- both tenant-scoped and platform-level actions.
--
-- Kept portable between PostgreSQL and SQLite on purpose (see the sqlite/
-- copy of this file, which is structurally identical): no dialect-specific
-- types, no gen_random_uuid(), no NOW(), following the same dual-dialect
-- constraints every module's migrations must follow.
--
-- id is an application-generated UUID (go/dbkit/audit/repository.go's
-- Insert), never a database-generated one.
--
-- actor_type/id/display_name are the flattened acting pkgcore.Actor and
-- are always populated (NOT NULL); on_behalf_of_type/id/display_name are
-- genuinely nullable (NULL, never an empty-string sentinel), because NULL
-- means "no impersonation" -- distinguishing it from an impersonating
-- actor whose fields happen to be empty. All three on_behalf_of_* columns
-- are only ever written or read together (see model.go's SetOnBehalfOf /
-- OnBehalfOf).
--
-- resource_type/id/display_name and success/failure_reason are the
-- flattened Resource and Result elements of the six-element AuditEvent
-- shape; changes carries an optional before/after diff as plain JSON
-- text, never a native PostgreSQL JSONB column with operator filtering --
-- this table is only ever read back whole, never queried inside its
-- Changes column, so a portable TEXT column is both sufficient and
-- dialect-neutral (go/dbkit/audit/model.go's own doc comment on the
-- Changes field).
--
-- tenant_id holds the owning tenant on a tenant-scoped action and the
-- empty-string sentinel on a platform-level one (never NULL, matching
-- go/config's configs.tenant_id and go/jobs's jobs.tenant_id -- see
-- model.go's doc comment for why this column is real but deliberately not
-- enforced by dbkit's tenant-scoping plugin). The composite index below is
-- the ListByTenant read path's own query shape
-- (docs/internal/10-compliance-and-audit.md's by-time-range retrieval
-- need).
--
-- ip/user_agent/trace_id are request-context metadata, empty when the
-- action producing a row had no such context (a background job, an event
-- subscriber).
--
-- No column here is ever updated or deleted by application code -- see
-- go/dbkit/audit/repository.go's own doc comment on why Repository
-- exposes no such method. The database-role/trigger backstop against a
-- determined operator with raw database access, and the optional hash
-- chain, are both explicitly M4 (docs/internal/15-roadmap.md).
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
