-- integration_webhook_deliveries is go/integration's round-2 delivery log
-- (go/integration/webhook_model.go): one row per (subscription, event)
-- delivery attempted or about to be attempted -- the delivery log
-- docs/internal/07-platform-services.md requires. Tenant data,
-- isolation proven by tenancytest.AssertIsolated, and will carry a
-- PostgreSQL RLS policy in the distributed deployment mode once one is
-- wired for this module, the same way every other tenant-scoped table's
-- does.
--
-- The primary key is (id) alone, matching this module's other two tables.
--
-- This is the PostgreSQL copy; see the sqlite/ sibling for the identical
-- schema on that dialect. Kept portable on purpose: no dialect-specific
-- types, no native arrays, no JSONB, no gen_random_uuid(), no NOW().
--
-- payload is the exact JSON body sent (or about to be sent): the versioned
-- public envelope {event: {type, version}, data: ...}, captured once when
-- the row is created and never recomputed on retry -- see
-- WebhookDelivery.Payload's own doc comment in webhook_model.go for why.
--
-- status is one of the DeliveryStatus* values (webhook_model.go); a
-- PostgreSQL enum type is deliberately not used, per the backend coding
-- standard's dual-dialect rule (SQLite has no enum type).
CREATE TABLE integration_webhook_deliveries (
    id                VARCHAR(36)  NOT NULL,
    tenant_id         VARCHAR(64)  NOT NULL,
    subscription_id   VARCHAR(36)  NOT NULL,
    event_type        VARCHAR(128) NOT NULL,
    event_version     VARCHAR(16)  NOT NULL,
    idempotency_key   VARCHAR(64)  NOT NULL,
    payload           TEXT         NOT NULL,
    status            VARCHAR(16)  NOT NULL,
    attempts          INTEGER      NOT NULL,
    last_status_code  INTEGER,
    last_error        VARCHAR(4000) NOT NULL,
    last_attempt_at   TIMESTAMP,
    delivered_at      TIMESTAMP,
    created_at        TIMESTAMP    NOT NULL,
    updated_at        TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);

-- handleDomainEvent (webhook_delivery.go) probes for an existing delivery
-- under (tenant, subscription, idempotency key) before creating a new row,
-- so a redelivered domain event -- which an at-least-once EventBus
-- subscriber must tolerate -- fans out to the same subscription exactly
-- once. Scoped by tenant first, then subscription, matching this module's
-- other unique index's own "tenant first" convention.
CREATE UNIQUE INDEX uq_integration_webhook_deliveries_tenant_subscription_key
    ON integration_webhook_deliveries (tenant_id, subscription_id, idempotency_key);

-- The "list recent deliveries for this subscription" query
-- (WebhookDeliveryRepository.ListRecentBySubscription) reads this table
-- ordered by created_at descending, scoped to one subscription within one
-- tenant -- exactly the shape this index serves.
CREATE INDEX idx_integration_webhook_deliveries_subscription_created_at
    ON integration_webhook_deliveries (tenant_id, subscription_id, created_at);
