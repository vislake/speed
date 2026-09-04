package metering

import (
	"context"
	"fmt"
	"testing"

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

// TestIngestReceiptRepository_Create_DuplicateIsAUniqueViolation proves the
// exact mechanism Aggregator.foldIntoSummaryOnce relies on: a second
// Create for the same (tenant, id) fails with the portable
// gorm.ErrDuplicatedKey isUniqueViolation checks for, not a generic error
// -- see IngestReceipt's own doc comment for why that distinction is what
// makes a redelivered event a safe no-op rather than a hard failure.
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
	if !isUniqueViolation(err) {
		t.Errorf("Create (duplicate) error = %v, want isUniqueViolation(err) = true", err)
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
