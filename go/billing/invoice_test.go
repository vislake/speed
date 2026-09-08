package billing

import (
	"context"
	"sync"
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

// TestInvoiceRepository_VoidOnPaidInvoice_Refused pins the transition guard:
// setStatus must validate every move, so a caller could never overwrite
// a settled invoice's Status -- an unguarded Void on a Paid invoice would
// rewrite the record of a collected payment to "void", as if the money
// had never arrived, and report success. Every transition is validated
// against the same legal-table shape Subscription transitions use: Open
// may move to Paid or Void, and both Paid and Void are terminal -- any
// other move (including Void on a Paid invoice) is
// ErrInvalidInvoiceTransition.
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

// TestInvoiceRepository_MarkPaidAndVoid_RacingTransitions_ExactlyOneCommits
// pins the guard on setStatus's write shape: a transition validates the
// move against the invoice status it reads and applies through a write
// tied to that read -- a write with no such tie would let two racing
// transitions that both validated from Open BOTH commit: a Void landing
// after a MarkPaid would rewrite a settled payment's record into a voided
// one, silently breaking the transition table's own terminal-state
// invariant ("an invoice that recorded a settled payment ... must not be
// rewritten into a voided one"), and neither caller would see an error.
//
// The two transitions are raced against fresh invoices in a loop;
// whichever way each race resolves, exactly one transition may commit --
// the winner's terminal state stands and the loser reports
// billing.invalid_invoice_transition, mirroring what a caller who had
// observed the winner's state directly would have gotten. A race where
// both calls report success would violate the transition table and fails
// the assertions below.
func TestInvoiceRepository_MarkPaidAndVoid_RacingTransitions_ExactlyOneCommits(t *testing.T) {
	repo := NewInvoiceRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	for trial := 0; trial < 60; trial++ {
		inv, err := repo.CreateInvoice(ctx, CreateInvoiceInput{
			SubscriptionID: "sub-1", Amount: Money{Cents: 100, Currency: "USD"},
		})
		if err != nil {
			t.Fatalf("trial %d CreateInvoice: %v", trial, err)
		}

		var (
			wg      sync.WaitGroup
			start   = make(chan struct{})
			paidErr error
			voidErr error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, paidErr = repo.MarkPaid(ctx, inv.ID)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, voidErr = repo.Void(ctx, inv.ID)
		}()
		close(start)
		wg.Wait()

		if paidErr == nil && voidErr == nil {
			t.Fatalf("trial %d: MarkPaid and Void BOTH reported success racing one open invoice -- a terminal state was rewritten by the loser's unguarded write", trial)
		}
		if paidErr != nil && !hasCode(paidErr, ErrInvalidInvoiceTransition.Code) {
			t.Fatalf("trial %d: losing MarkPaid error = %v, want %s", trial, paidErr, ErrInvalidInvoiceTransition.Code)
		}
		if voidErr != nil && !hasCode(voidErr, ErrInvalidInvoiceTransition.Code) {
			t.Fatalf("trial %d: losing Void error = %v, want %s", trial, voidErr, ErrInvalidInvoiceTransition.Code)
		}

		// The row must end in the WINNER's terminal state -- a successful
		// MarkPaid is never followed by a committed void, and vice versa.
		got, err := repo.FindByID(ctx, inv.ID)
		if err != nil {
			t.Fatalf("trial %d FindByID: %v", trial, err)
		}
		want := InvoiceStatusVoid
		if paidErr == nil {
			want = InvoiceStatusPaid
		}
		if got.Status != string(want) {
			t.Fatalf("trial %d: final status = %q, want %q (the winner's terminal state)", trial, got.Status, want)
		}
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
