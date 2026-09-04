-- billing_payment_events is the durable, deduplicated record of every
-- inbound payment-channel webhook delivery (go/billing/payment_event.go):
-- docs/internal/06-billing-and-metering.md's mandatory insert-first-to-dedup
-- rule requires one row per genuinely distinct channel event, inserted
-- BEFORE any processing runs, so a channel's own event id (provider_event_id)
-- together with channel is the natural key a redelivered event dedups on.
--
-- Unlike billing_plans (whose tenant_id column deliberately spans both a
-- platform-wide and a tenant-custom face of the same table), this table is
-- genuinely per-tenant payment history, tenant_id decoded from the
-- channel-side object's own metadata at webhook-verification time -- see
-- PaymentEvent's own doc comment.
--
-- id is an application-generated UUID, already globally unique on its
-- own, so it alone is the primary key here -- tenant_id gets its own
-- secondary index instead of participating in a composite primary key, the
-- identical shape billing_subscriptions and billing_invoices already use.
--
-- uq_billing_payment_events_channel_event is NOT further scoped by
-- tenant_id: a channel's own event id is already globally unique across
-- that channel's entire account, so widening the key would only let a
-- tenant-decoding bug silently create a second row for the one event it
-- actually was, defeating the dedup the rule exists to enforce.
--
-- This is the SQLite copy; see the postgres/ sibling for the identical
-- schema on that dialect.
CREATE TABLE billing_payment_events (
    id                  VARCHAR(36)  NOT NULL,
    tenant_id           VARCHAR(64)  NOT NULL,
    channel             VARCHAR(32)  NOT NULL,
    provider_event_id   VARCHAR(255) NOT NULL,
    channel_reference   VARCHAR(255) NOT NULL,
    subscription_id     VARCHAR(36)  NOT NULL,
    invoice_id          VARCHAR(36)  NOT NULL,
    event_type          VARCHAR(32)  NOT NULL,
    status              VARCHAR(16)  NOT NULL,
    amount_cents        BIGINT       NOT NULL,
    currency            VARCHAR(3)   NOT NULL,
    occurred_at         TIMESTAMP    NOT NULL,
    raw_payload         BLOB         NOT NULL,
    processed_at        TIMESTAMP,
    created_at          TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);

CREATE INDEX idx_billing_payment_events_tenant_id ON billing_payment_events (tenant_id);

CREATE UNIQUE INDEX uq_billing_payment_events_channel_event ON billing_payment_events (channel, provider_event_id);
