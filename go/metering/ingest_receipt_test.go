package metering

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// TestIngestReceiptRepository_AssertIsolated runs the mandatory
// tenant-isolation suite against metering_ingest_receipts, matching
// SummaryRepository's own equivalent test: IngestReceipt is tenant data
// (see its own doc comment), so AssertIsolated -- not
// AssertNotTenantScoped -- is the correct half of the pair.
func TestIngestReceiptRepository_AssertIsolated(t *testing.T) {
	repo := NewIngestReceiptRepository(newTestDB(t))

	n := 0
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *IngestReceipt {
		n++
		return &IngestReceipt{ID: fmt.Sprintf("idem-%d", n)}
	})
}

// TestIngestReceiptRepository_Create_DuplicateIsAUniqueViolation proves
// the raw repository path (a caller deliberately not using
// IngestBillingGrade's idempotent fold) still surfaces a second Create
// for the same (tenant, id) as the portable gorm.ErrDuplicatedKey -- the
// classification dbkit's TranslateError produces from each driver's own
// raw unique-violation error, re-proven against real PostgreSQL by the
// integration tier's
// TestPostgres_IngestReceiptRepository_DuplicateCreate_IsErrDuplicatedKey.
// The idempotent fold path itself does not depend on catching this
// error: it avoids it entirely through an ON CONFLICT DO NOTHING insert
// (see foldIntoSummaryOnce's doc comment for why catching it would abort
// the transaction on PostgreSQL).
func TestIngestReceiptRepository_Create_DuplicateIsAUniqueViolation(t *testing.T) {
	repo := NewIngestReceiptRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if err := repo.Create(ctx, &IngestReceipt{ID: "idem-1"}); err != nil {
		t.Fatalf("Create (first): %v", err)
	}

	err := repo.Create(ctx, &IngestReceipt{ID: "idem-1"})
	if err == nil {
		t.Fatal("Create (duplicate) = nil error, want a unique-constraint violation")
	}
	if !errors.Is(err, gorm.ErrDuplicatedKey) {
		t.Errorf("Create (duplicate) error = %v, want errors.Is(err, gorm.ErrDuplicatedKey)", err)
	}
}

// TestIngestReceiptRepository_Create_ScopedByTenant proves the uniqueness
// is (tenant_id, id) together, not id alone -- two different tenants may
// reuse the same caller-chosen IdempotencyKey without colliding, the same
// property TestFindOutboxByIdempotencyKey_ScopedByTenant proves for
// OutboxRecord's own (tenant_id, idempotency_key) index.
func TestIngestReceiptRepository_Create_ScopedByTenant(t *testing.T) {
	repo := NewIngestReceiptRepository(newTestDB(t))
	ctxA := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctxB := pkgcore.WithTenant(context.Background(), "tenant-b")

	if err := repo.Create(ctxA, &IngestReceipt{ID: "idem-shared"}); err != nil {
		t.Fatalf("Create(tenant-a): %v", err)
	}
	if err := repo.Create(ctxB, &IngestReceipt{ID: "idem-shared"}); err != nil {
		t.Fatalf("Create(tenant-b): %v", err)
	}
}
