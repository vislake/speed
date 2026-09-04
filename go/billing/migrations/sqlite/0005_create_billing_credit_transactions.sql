-- billing_credit_transactions is billing's append-only credit ledger
-- (go/billing/credit_transaction.go): grant/deduct/refund/expire entries,
-- reconstructable/auditable from this table alone -- see
-- CreditTransaction's own doc comment for why it deliberately is NOT
-- reached through dbkit.Repository[T] despite genuinely being tenant data
-- (Repository[T] would promote an Update/Delete this ledger must never
-- offer).
--
-- id is the caller's own IdempotencyKey for a Deduct row (never a second,
-- unrelated generated id -- see the Go model's own doc comment), so it is
-- NOT globally unique across tenants: two different tenants may
-- reasonably reuse the same idempotency-key string for their own,
-- unrelated operations. The primary key is therefore the composite
-- (id, tenant_id), the identical shape go/metering's UsageSummary and
-- go/dbkit/audit's IngestReceipt tables both already use for the same
-- reason.
--
-- This is the SQLite copy; see the postgres/ sibling for the identical
-- schema on that dialect.
CREATE TABLE billing_credit_transactions (
    id         VARCHAR(100) NOT NULL,
    tenant_id  VARCHAR(64)  NOT NULL,
    type       VARCHAR(16)  NOT NULL,
    status     VARCHAR(16)  NOT NULL,
    amount     BIGINT       NOT NULL,
    reason     VARCHAR(255) NOT NULL DEFAULT '',
    created_at TIMESTAMP    NOT NULL,
    updated_at TIMESTAMP    NOT NULL,
    PRIMARY KEY (id, tenant_id)
);

-- Backs CreditTransactionRepository.ListByTenant's own query shape
-- (tenant_id filter, created_at DESC ordering) -- the same
-- (tenant_id, occurred_at) index shape go/dbkit/audit's audit_events table
-- uses for its identical ListByTenant.
CREATE INDEX idx_billing_credit_transactions_tenant_created ON billing_credit_transactions (tenant_id, created_at);
