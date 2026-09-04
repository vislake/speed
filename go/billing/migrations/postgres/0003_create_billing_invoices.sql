-- billing_invoices is billing's channel-agnostic Invoice model
-- (go/billing/invoice.go): one billing document for a Subscription's
-- billing cycle, with no payment-channel reference of any kind, mirroring
-- Subscription's own channel-agnostic shape.
--
-- id is an application-generated UUID, already globally unique on its
-- own, so it alone is the primary key here -- tenant_id gets its own
-- secondary index instead of participating in a composite primary key,
-- the same shape billing_subscriptions and examples/reference-app's notes
-- table both use.
--
-- subscription_id is an ID reference to billing_subscriptions -- no
-- database foreign key (this codebase's own "no cross-module foreign
-- keys" discipline applies within a module's own tables too, for the
-- same independently-evolvable-migration reason).
--
-- This is the PostgreSQL copy; see the sqlite/ sibling for the identical
-- schema on that dialect.
CREATE TABLE billing_invoices (
    id              VARCHAR(36) NOT NULL,
    tenant_id       VARCHAR(64) NOT NULL,
    subscription_id VARCHAR(36) NOT NULL,
    amount_cents    BIGINT      NOT NULL,
    currency        VARCHAR(3)  NOT NULL,
    status          VARCHAR(16) NOT NULL,
    period_start    TIMESTAMP   NOT NULL,
    period_end      TIMESTAMP   NOT NULL,
    created_at      TIMESTAMP   NOT NULL,
    updated_at      TIMESTAMP   NOT NULL,
    PRIMARY KEY (id)
);

CREATE INDEX idx_billing_invoices_tenant_id ON billing_invoices (tenant_id);
CREATE INDEX idx_billing_invoices_subscription_id ON billing_invoices (subscription_id);
