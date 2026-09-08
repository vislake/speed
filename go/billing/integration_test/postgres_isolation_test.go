//go:build integration

// Package billing_test holds go/billing's PostgreSQL integration tier,
// the tier that exists because PreDeduct's poisoned-transaction recovery
// is a PostgreSQL-only defect class. It is physically separate from
// go/billing's unit tests (which live in package billing, one file per
// source file, per the backend
// coding standard's testing layout rule) and carries the "integration"
// build tag: a plain "go test ./..." never compiles or runs anything in
// this directory; it is invoked explicitly with
// "go test -tags=integration ./..." (and, in CI, under -race) from the
// module directory.
//
// Every test here opens its own disposable PostgreSQL 16 container via
// testcontainers (through billing/internal/testutil.NewPostgres, which
// applies the module's real postgres/*.sql migrations from zero through
// dbkit.MigrationRegistry -- the same zero-to-head proof the unit tier's
// newTestDB runs on the SQLite set) and skips itself when no Docker
// daemon is reachable, which is testutil.NewPostgres's own contract
// (go/dbkit/dbtest.NewPostgres's t.Skip on an absent daemon). In CI the
// full-check integration-tiers job runs this directory on ubuntu-latest
// runners, where Docker is always present.
//
// What this tier exists to prove:
//
//   - credit_service.go's PreDeduct recovered a duplicate (id, tenant_id)
//     insert -- a retried PreDeduct meeting its own earlier attempt's row
//     -- by catching the unique-constraint violation and reading the
//     existing row back on the SAME open dbkit.WithTenantSession
//     transaction. SQLite tolerates that; PostgreSQL does not: after a
//     statement error the transaction is aborted and every later
//     statement fails with SQLSTATE 25P02 until ROLLBACK, so the read-back
//     failed, PreDeduct returned an error, and the idempotent-retry
//     contract collapsed -- a retried PreDeduct whose first attempt had
//     already committed got an error instead of its own earlier
//     reservation, on the money path itself. The recovery now inserts with
//     ON CONFLICT DO NOTHING, which never aborts the transaction, and only
//     then reads the pre-existing row back on the still-healthy
//     transaction (postgres_credit_transactions_test.go's
//     TestPostgres_PreDeduct_IdempotentRetry_ReturnsTheSameReservation,
//     which only real PostgreSQL can make fail).
//
//   - the whole credit reserve -> confirm -> refund family runs on the
//     real server: the full lifecycle with both idempotent-retry legs,
//     the concurrent over-balance single-winner property (whose
//     database-arbitrated CAS guard is the family's concurrency core, and
//     which an SQLite-only proof could pass with a broken statement-level
//     behavior PostgreSQL would not tolerate), and the two tenant-scoped
//     models' mandatory tenancytest.AssertIsolated suites re-run against
//     real PostgreSQL, the same re-run go/org, go/rbac, go/storage,
//     go/notification and go/metering integration tiers make for their own
//     tenant-scoped repositories.
package billing_test

import (
	"context"
	"fmt"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/billing/internal/testutil"
	"github.com/vislake/speed/go/billing/migrations"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// newPostgres returns a migrated PostgreSQL *gorm.DB with billing's
// migrations applied from zero, skipping the test when no Docker daemon is
// reachable (testutil.NewPostgres's own contract).
func newPostgres(t *testing.T) *gorm.DB {
	t.Helper()
	return testutil.NewPostgres(t, "billing", migrations.FS)
}

// tenantCtx is the context a host hands a tenant-scoped repository call
// after tenancy.Middleware (or, in this package's case, the equivalent
// hand-built pkgcore.WithTenant) has resolved the tenant.
func tenantCtx(tenant pkgcore.TenantID) context.Context {
	return pkgcore.WithTenant(context.Background(), tenant)
}

// TestCreditBalanceRepository_AssertIsolated_Postgres re-runs go/billing's
// own TestCreditBalanceRepository suite's isolation proof against a real
// PostgreSQL server. billing_credit_balances is tenant data
// AssertIsolated is the mandatory
// suite for it.
func TestCreditBalanceRepository_AssertIsolated_Postgres(t *testing.T) {
	repo := billing.NewCreditBalanceRepository(newPostgres(t))

	n := 0
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *billing.CreditBalance {
		n++
		return &billing.CreditBalance{
			ID: fmt.Sprintf("balance-%d", n),
		}
	})
}

// TestCreditTransaction_AssertIsolated_Postgres re-runs the isolation proof
// against a real PostgreSQL server for billing_credit_transactions, the
// append-only ledger model. Like the SQLite original
// (credit_transaction_test.go's TestCreditTransaction_AssertIsolated), it
// exercises the generic isolation mechanics directly against
// dbkit.Repository[CreditTransaction] -- proving the model itself is
// genuinely tenant-scoped -- even though CreditTransactionRepository
// deliberately never embeds that generic Repository (see
// CreditTransaction's own doc comment for why).
func TestCreditTransaction_AssertIsolated_Postgres(t *testing.T) {
	repo := dbkit.NewRepository[billing.CreditTransaction](newPostgres(t))

	n := 0
	tenancytest.AssertIsolated(t, repo, func(tenant pkgcore.TenantID) *billing.CreditTransaction {
		n++
		return &billing.CreditTransaction{
			ID:     fmt.Sprintf("isolation-probe-%d", n),
			Type:   string(billing.CreditTransactionGrant),
			Status: string(billing.CreditTransactionStatusConfirmed),
			Amount: 1,
		}
	})
}
