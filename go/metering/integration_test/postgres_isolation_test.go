//go:build integration

// Package metering_test holds go/metering's PostgreSQL integration tier,
// the tier that exists because three defect classes SQLite cannot surface
// live only on a real server. It is physically separate from go/metering's
// unit tests (which live in package metering, one file per source file)
// and carries the "integration" build tag: a plain "go test ./..." never
// compiles or runs anything in this directory; it is invoked explicitly
// with "go test -tags=integration ./..." from the module directory.
//
// Every test here opens its own disposable PostgreSQL 16 container via
// testcontainers (through metering/internal/testutil.NewPostgres, which
// applies the module's real postgres/*.sql migrations from zero through
// dbkit.MigrationRegistry -- the same zero-to-head proof the unit tier's
// NewSQLite runs on the SQLite set) and skips itself when no Docker
// daemon is reachable, which is testutil.NewPostgres's own contract
// (go/dbkit/dbtest.NewPostgres's t.Skip on an absent daemon). In CI the
// full-check integration-tiers job runs this directory on ubuntu-latest
// runners, where Docker is always present.
//
// What this tier exists to prove -- three defect classes SQLite cannot
// surface, each with its own regression test below, plus the
// isolation-suite re-runs in this file:
//
//   - Recovery code must never depend on a failed statement leaving its
//     transaction usable. Enqueue recovers a unique-key conflict by
//     reading the existing row back on the SAME transaction; SQLite
//     tolerates a failed statement inside an open transaction, PostgreSQL
//     does not: after a statement error the transaction is aborted and
//     every later statement fails with SQLSTATE 25P02 until ROLLBACK --
//     so the read-back would fail, Enqueue would return the insert error,
//     and the idempotent-retry contract would collapse: a caller's retry
//     of an Enqueue whose first attempt already committed would get an
//     error and roll back its own unrelated business write along with it.
//     The insert therefore runs as ON CONFLICT DO NOTHING, which never
//     aborts the transaction, and only then is the pre-existing row read
//     back on the still-healthy transaction (postgres_outbox_test.go's
//     TestPostgres_Enqueue_IdempotentRetry_InsideOneCallerTransaction).
//
//   - A swallowed conflict must never let an enclosing transaction
//     commit. On PostgreSQL a COMMIT issued against an aborted
//     transaction is turned into a ROLLBACK by the server, which the pgx
//     driver surfaces as an error ("commit unexpectedly resulted in
//     rollback") -- so a redelivered event whose receipt insert conflicted
//     would make IngestBillingGrade fail, Dispatcher would leave the
//     outbox row pending forever, and the receipt's "exactly once" would
//     hold only on SQLite. foldIntoSummaryOnce's receipt insert runs as
//     ON CONFLICT DO NOTHING: the conflict is recognized through
//     RowsAffected == 0 with the transaction healthy, and the commit is a
//     real commit (postgres_ingest_receipts_test.go's two regressions).
//
//   - Every fixed-width field must be bounded before any write. TenantID,
//     Feature and IdempotencyKey have fixed VARCHAR columns (64/128/200
//     bytes on every table of this module, both dialects) that SQLite
//     never enforces but PostgreSQL rejects with SQLSTATE 22001 -- inside
//     the caller's own transaction when the write is Enqueue's, with the
//     same aborted-transaction knock-on as the first class. validate
//     refuses over-long fields before any write (postgres_validation_
//     test.go's TestPostgres_Enqueue_OverlongField_RefusedBeforeAnyWrite).
//
// This file itself re-runs the two tenant-scoped repositories'
// tenancytest.AssertIsolated suites (the mandatory isolation suite for
// tenant data) against the real server, the same re-run go/org, go/rbac,
// go/storage and go/notification integration tiers make for their own
// tenant-scoped repositories.
package metering_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"

	"github.com/vislake/speed/go/metering"
	"github.com/vislake/speed/go/metering/internal/testutil"
	"github.com/vislake/speed/go/metering/migrations"
)

// newPostgres returns a migrated PostgreSQL *gorm.DB with metering's
// migrations applied from zero, skipping the test when no Docker daemon
// is reachable (testutil.NewPostgres's own contract).
func newPostgres(t *testing.T) *gorm.DB {
	t.Helper()
	return testutil.NewPostgres(t, "metering", migrations.FS)
}

// tenantCtx is the context a host hands a tenant-scoped repository call
// after tenancy.Middleware (or, in this package's case, the equivalent
// hand-built pkgcore.WithTenant) has resolved the tenant.
func tenantCtx(tenant pkgcore.TenantID) context.Context {
	return pkgcore.WithTenant(context.Background(), tenant)
}

// TestSummaryRepository_AssertIsolated_Postgres re-runs go/metering's own
// TestSummaryRepository_AssertIsolated against a real PostgreSQL server.
// metering_usage_summaries is tenant data; AssertIsolated is the mandatory
// suite for it.
func TestSummaryRepository_AssertIsolated_Postgres(t *testing.T) {
	repo := metering.NewSummaryRepository(newPostgres(t))

	n := 0
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *metering.UsageSummary {
		n++
		start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		feature := fmt.Sprintf("feature-%d", n)
		return &metering.UsageSummary{
			ID:          fmt.Sprintf("summary-%d", n),
			Feature:     feature,
			PeriodStart: start,
			PeriodEnd:   start.AddDate(0, 1, 0),
			Quantity:    float64(n),
		}
	})
}

// TestIngestReceiptRepository_AssertIsolated_Postgres re-runs go/metering's
// own TestIngestReceiptRepository_AssertIsolated against a real PostgreSQL
// server: metering_ingest_receipts is tenant data, so AssertIsolated --
// not AssertNotTenantScoped -- is the correct half of the pair.
func TestIngestReceiptRepository_AssertIsolated_Postgres(t *testing.T) {
	repo := metering.NewIngestReceiptRepository(newPostgres(t))

	n := 0
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *metering.IngestReceipt {
		n++
		return &metering.IngestReceipt{ID: fmt.Sprintf("idem-%d", n)}
	})
}
