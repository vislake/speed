-- billing_credit_balances is billing's per-tenant credit position
-- (go/billing/credit_balance.go): available (freely spendable) and
-- reserved (set aside by a not-yet-resolved CreditService.PreDeduct)
-- credit counts. Exactly one row per tenant -- id equals the owning
-- tenant's id (see CreditBalance's own doc comment for why a second,
-- independent id would only invite the wrong row to be looked up by
-- mistake).
--
-- Every write goes through CreditService's compare-and-swap UPDATE
-- (credit_service.go's applyBalanceDelta), never a plain read-modify-write
-- -- see that function's own doc comment for the full concurrency
-- argument. This table's own row is otherwise a completely ordinary
-- tenant-scoped table: dbkit.TenantScoped (CreditBalance embeds
-- dbkit.TenantModel), reached through dbkit.Repository[CreditBalance] for
-- its read path (CreditBalanceRepository.FindByID).
--
-- This is the PostgreSQL copy; see the sqlite/ sibling for the identical
-- schema on that dialect.
CREATE TABLE billing_credit_balances (
    id         VARCHAR(64) NOT NULL,
    tenant_id  VARCHAR(64) NOT NULL,
    available  BIGINT      NOT NULL DEFAULT 0,
    reserved   BIGINT      NOT NULL DEFAULT 0,
    created_at TIMESTAMP   NOT NULL,
    updated_at TIMESTAMP   NOT NULL,
    PRIMARY KEY (id)
);

CREATE INDEX idx_billing_credit_balances_tenant_id ON billing_credit_balances (tenant_id);
