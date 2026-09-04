-- integration_webhook_subscriptions is go/integration's round-2 table
-- (go/integration/webhook_model.go): one row per outbound webhook a tenant
-- has configured -- which public event types it wants delivered, and to
-- which URL. Tenant data (docs/internal/04-data-and-tenancy.md), isolation
-- proven by tenancytest.AssertIsolated, never AssertNotTenantScoped.
--
-- The primary key is (id) alone, matching round 1's integration_api_keys
-- precedent (itself matching go/storage's Object): id is an
-- application-generated UUID, globally unique on its own, so tenant_id
-- rides along as a plain, non-key column promoted by the embedded
-- dbkit.TenantModel.
--
-- This is the SQLite copy; see the postgres/ sibling for the full rationale
-- of every column. Dialect differences stop at the allowed SQL surface: no
-- dialect-specific types, no native arrays, no JSONB, no gen_random_uuid(),
-- no NOW().
CREATE TABLE integration_webhook_subscriptions (
    id           VARCHAR(36)   NOT NULL,
    tenant_id    VARCHAR(64)   NOT NULL,
    url          VARCHAR(2048) NOT NULL,
    event_types  TEXT          NOT NULL,
    secret       VARCHAR(512)  NOT NULL,
    active       BOOLEAN       NOT NULL,
    created_by   VARCHAR(64)   NOT NULL,
    created_at   TIMESTAMP     NOT NULL,
    updated_at   TIMESTAMP     NOT NULL,
    PRIMARY KEY (id)
);

-- handleDomainEvent (webhook_delivery.go) lists every active subscription of
-- one tenant on every matching domain event, so tenant_id must be indexed;
-- active is folded into the same index since the query always filters on
-- both together (an inactive subscription is never fanned out to).
CREATE INDEX idx_integration_webhook_subscriptions_tenant_active
    ON integration_webhook_subscriptions (tenant_id, active);
