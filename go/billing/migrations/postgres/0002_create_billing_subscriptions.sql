-- billing_subscriptions is billing's channel-agnostic Subscription model
-- (go/billing/subscription.go): a tenant's relationship to a Plan, with no
-- payment-channel reference of any kind (no gateway subscription id, no
-- external transaction id) -- docs/internal/06-billing-and-metering.md's
-- core principle that Subscription is an internal domain concept and a
-- payment channel is merely the collector.
--
-- id is an application-generated UUID, already globally unique on its
-- own, so it alone is the primary key here -- tenant_id gets its own
-- secondary index instead of participating in a composite primary key,
-- the identical shape examples/reference-app's notes table already uses
-- for the same reason (see Subscription's own doc comment).
--
-- This is the PostgreSQL copy; see the sqlite/ sibling for the identical
-- schema on that dialect.
CREATE TABLE billing_subscriptions (
    id          VARCHAR(36) NOT NULL,
    tenant_id   VARCHAR(64) NOT NULL,
    plan_id     VARCHAR(36) NOT NULL,
    status      VARCHAR(16) NOT NULL,
    created_at  TIMESTAMP   NOT NULL,
    updated_at  TIMESTAMP   NOT NULL,
    PRIMARY KEY (id)
);

CREATE INDEX idx_billing_subscriptions_tenant_id ON billing_subscriptions (tenant_id);
