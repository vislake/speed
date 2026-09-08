-- integration_webhook_subscriptions is go/integration's webhook table
-- (go/integration/webhook_model.go): one row per outbound webhook a tenant
-- has configured -- which public event types it wants delivered, and to
-- which URL. Tenant data, isolation proven by tenancytest.AssertIsolated,
-- never AssertNotTenantScoped.
--
-- The primary key is (id) alone, matching integration_api_keys' precedent:
-- id is an application-generated UUID, globally unique on its own, so
-- tenant_id rides along as a plain, non-key column.
--
-- This is the PostgreSQL copy; see the sqlite/ sibling for the identical
-- schema on that dialect. Kept portable on purpose: no dialect-specific
-- types, no native arrays, no JSONB, no gen_random_uuid(), no NOW().
--
-- url is validated at creation time by ValidateWebhookURL (ssrf.go) --
-- refused if it resolves to a private/loopback/link-local address -- and
-- re-validated at delivery time by the same transport that sends the
-- request, so a URL whose DNS answer changed after creation is never
-- silently trusted either.
--
-- url's VARCHAR(2048) also flows, verbatim, into the audit trail:
-- webhook_service.go's emitWebhookAudit puts row.URL into the audit
-- event's resource_display_name, whose own column is VARCHAR(255) -- a
-- source wider than its target. That is deliberate and handled at the
-- audit write path (go/dbkit/audit's Repository.Insert cuts
-- caller-supplied content to its columns, recording each cut in a
-- structured warning), so a legal long URL is stored cut, never refused
-- after this subscription's mutation has committed.
--
-- event_types is a JSON array of PUBLIC event type strings
-- (webhook_model.go's own doc comment spells out why this is never the
-- internal pkgcore.Event.Type vocabulary), stored as TEXT rather than a
-- native array or JSONB column with operator filtering, per the
-- dual-dialect rule -- this module only ever reads the column back whole
-- and filters in application code (see webhook_delivery.go's
-- matchingSubscriptions).
--
-- secret is encrypted at rest under WebhookSecretSerializerName -- see that
-- constant's own doc comment in webhook_model.go for why this column is
-- reversibly encrypted rather than hashed the way an API key's Hash column
-- is (a webhook secret must be read back in plaintext to sign every
-- delivery attempt; an API key never is).
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
