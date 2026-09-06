package billing

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

func TestInvoiceRepository_CreateInvoice_StartsAtOpen(t *testing.T) {
	repo := NewInvoiceRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	inv, err := repo.CreateInvoice(ctx, CreateInvoiceInput{
		SubscriptionID: "sub-1",
		Amount:         Money{Cents: 4900, Currency: "USD"},
		PeriodStart:    start,
		PeriodEnd:      start.AddDate(0, 1, 0),
	})
	if err != nil {
		t.Fatalf("CreateInvoice: %v", err)
	}
	if inv.Status != string(InvoiceStatusOpen) {
		t.Errorf("Status = %q, want %q", inv.Status, InvoiceStatusOpen)
	}
	if inv.Amount() != (Money{Cents: 4900, Currency: "USD"}) {
		t.Errorf("Amount() = %+v, want {4900 USD}", inv.Amount())
	}
}

func TestInvoiceRepository_MarkPaidAndVoid(t *testing.T) {
	repo := NewInvoiceRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	inv, err := repo.CreateInvoice(ctx, CreateInvoiceInput{SubscriptionID: "sub-1", Amount: Money{Cents: 100, Currency: "USD"}})
	if err != nil {
		t.Fatalf("CreateInvoice: %v", err)
	}

	paid, err := repo.MarkPaid(ctx, inv.ID)
	if err != nil {
		t.Fatalf("MarkPaid: %v", err)
	}
	if paid.Status != string(InvoiceStatusPaid) {
		t.Errorf("Status after MarkPaid = %q, want %q", paid.Status, InvoiceStatusPaid)
	}

	inv2, err := repo.CreateInvoice(ctx, CreateInvoiceInput{SubscriptionID: "sub-1", Amount: Money{Cents: 100, Currency: "USD"}})
	if err != nil {
		t.Fatalf("CreateInvoice: %v", err)
	}
	voided, err := repo.Void(ctx, inv2.ID)
	if err != nil {
		t.Fatalf("Void: %v", err)
	}
	if voided.Status != string(InvoiceStatusVoid) {
		t.Errorf("Status after Void = %q, want %q", voided.Status, InvoiceStatusVoid)
	}
}

func TestInvoiceRepository_MarkPaid_NotFound(t *testing.T) {
	repo := NewInvoiceRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	_, err := repo.MarkPaid(ctx, "does-not-exist")
	if !hasCode(err, ErrInvoiceNotFound.Code) {
		t.Errorf("MarkPaid(missing): err = %v, want %s", err, ErrInvoiceNotFound.Code)
	}
}

// TestInvoiceRepository_VoidOnPaidInvoice_Refused is P3-15's regression
// test: setStatus used to validate nothing, so any caller could overwrite
// a settled invoice's Status -- Void on a Paid invoice silently rewrote
// the record of a collected payment to "void", as if the money had never
// arrived. The fix validates every transition against the same legal-table
// shape Subscription transitions use: Open may move to Paid or Void, and
// both Paid and Void are terminal -- any other move (including Void on a
// Paid invoice) is ErrInvalidInvoiceTransition. This fails on the pre-fix
// setStatus (which rewrote the row and reported success).
func TestInvoiceRepository_VoidOnPaidInvoice_Refused(t *testing.T) {
	repo := NewInvoiceRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	inv, err := repo.CreateInvoice(ctx, CreateInvoiceInput{SubscriptionID: "sub-1", Amount: Money{Cents: 100, Currency: "USD"}})
	if err != nil {
		t.Fatalf("CreateInvoice: %v", err)
	}
	if _, markErr := repo.MarkPaid(ctx, inv.ID); markErr != nil {
		t.Fatalf("MarkPaid: %v", markErr)
	}

	_, err = repo.Void(ctx, inv.ID)
	if !hasCode(err, "billing.invalid_invoice_transition") {
		t.Errorf("Void on a Paid invoice: err = %v, want %s", err, "billing.invalid_invoice_transition")
	}

	// The row must be untouched by the refused transition.
	got, err := repo.FindByID(ctx, inv.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.Status != string(InvoiceStatusPaid) {
		t.Errorf("Status after refused Void = %q, want %q -- a settled invoice's record must stand", got.Status, InvoiceStatusPaid)
	}

	// The other terminal direction mirrors: MarkPaid on a Voided invoice is
	// equally refused.
	inv2, err := repo.CreateInvoice(ctx, CreateInvoiceInput{SubscriptionID: "sub-1", Amount: Money{Cents: 100, Currency: "USD"}})
	if err != nil {
		t.Fatalf("CreateInvoice: %v", err)
	}
	if _, voidErr := repo.Void(ctx, inv2.ID); voidErr != nil {
		t.Fatalf("Void: %v", voidErr)
	}
	_, err = repo.MarkPaid(ctx, inv2.ID)
	if !hasCode(err, "billing.invalid_invoice_transition") {
		t.Errorf("MarkPaid on a Voided invoice: err = %v, want %s", err, "billing.invalid_invoice_transition")
	}
}

func TestInvoiceRepository_AssertIsolated(t *testing.T) {
	repo := NewInvoiceRepository(newTestDB(t))
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *Invoice {
		inv := &Invoice{ID: uuid.NewString(), SubscriptionID: "sub-1", Status: string(InvoiceStatusOpen)}
		inv.SetAmount(Money{Cents: 100, Currency: "USD"})
		return inv
	})
}
