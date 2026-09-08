package billing

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

func newTestPaymentEvent(channel, providerEventID string, status ChannelStatus, occurredAt time.Time) *PaymentEvent {
	evt := &PaymentEvent{
		Channel:          channel,
		ProviderEventID:  providerEventID,
		ChannelReference: "ref-1",
		SubscriptionID:   "sub-1",
		InvoiceID:        "inv-1",
		EventType:        string(NormalizedEventChargeSucceeded),
		Status:           string(status),
		OccurredAt:       occurredAt,
		RawPayload:       []byte(`{}`),
	}
	return evt
}

func TestPaymentEventRepository_InsertIfNew_DedupsOnChannelAndProviderEventID(t *testing.T) {
	repo := NewPaymentEventRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	evt := newTestPaymentEvent("stripe", "evt_1", ChannelStatusPending, time.Now())
	inserted, err := repo.InsertIfNew(ctx, evt)
	if err != nil {
		t.Fatalf("InsertIfNew (first): %v", err)
	}
	if !inserted {
		t.Fatal("InsertIfNew (first) = false, want true")
	}
	firstID := evt.ID

	// A redelivery of the SAME event -- same Channel/ProviderEventID -- must
	// be recognized as already recorded, per the insert-first-to-dedup rule.
	redelivered := newTestPaymentEvent("stripe", "evt_1", ChannelStatusPending, time.Now())
	inserted, err = repo.InsertIfNew(ctx, redelivered)
	if err != nil {
		t.Fatalf("InsertIfNew (redelivery): %v", err)
	}
	if inserted {
		t.Error("InsertIfNew (redelivery) = true, want false -- the same (channel, provider_event_id) was already recorded")
	}

	// The first row must be exactly what a caller reads back -- the
	// redelivery must not have overwritten it.
	got, err := repo.Get(ctx, firstID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ProviderEventID != "evt_1" {
		t.Errorf("ProviderEventID = %q, want evt_1", got.ProviderEventID)
	}
}

func TestPaymentEventRepository_InsertIfNew_DifferentChannelsDoNotCollide(t *testing.T) {
	repo := NewPaymentEventRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	// The SAME provider_event_id string under two different channels must
	// be treated as two distinct events -- the dedup key is (channel,
	// provider_event_id), not provider_event_id alone.
	stripe := newTestPaymentEvent("stripe", "evt_shared", ChannelStatusPending, time.Now())
	if _, err := repo.InsertIfNew(ctx, stripe); err != nil {
		t.Fatalf("InsertIfNew (stripe): %v", err)
	}
	alipay := newTestPaymentEvent("alipay", "evt_shared", ChannelStatusPending, time.Now())
	inserted, err := repo.InsertIfNew(ctx, alipay)
	if err != nil {
		t.Fatalf("InsertIfNew (alipay): %v", err)
	}
	if !inserted {
		t.Error("InsertIfNew (alipay) = false, want true -- a different channel with the same provider_event_id is a distinct event")
	}
}

func TestPaymentEventRepository_Get_NotFound(t *testing.T) {
	repo := NewPaymentEventRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	_, err := repo.Get(ctx, "does-not-exist")
	if !hasCode(err, ErrPaymentEventNotFound.Code) {
		t.Errorf("err = %v, want ErrPaymentEventNotFound", err)
	}
}

func TestPaymentEventRepository_ListPending_FiltersByStatusAndAge(t *testing.T) {
	repo := NewPaymentEventRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	now := time.Now()
	old := newTestPaymentEvent("stripe", "evt_old", ChannelStatusPending, now.Add(-time.Hour))
	if _, err := repo.InsertIfNew(ctx, old); err != nil {
		t.Fatalf("insert old: %v", err)
	}
	recent := newTestPaymentEvent("stripe", "evt_recent", ChannelStatusPending, now)
	if _, err := repo.InsertIfNew(ctx, recent); err != nil {
		t.Fatalf("insert recent: %v", err)
	}
	settled := newTestPaymentEvent("stripe", "evt_settled", ChannelStatusSucceeded, now.Add(-time.Hour))
	if _, err := repo.InsertIfNew(ctx, settled); err != nil {
		t.Fatalf("insert settled: %v", err)
	}

	// before = now-30m: only "old" (occurred an hour ago, still pending) is
	// stuck; "recent" is too fresh, "settled" is not pending.
	got, err := repo.listPending(ctx, now.Add(-30*time.Minute), 100)
	if err != nil {
		t.Fatalf("listPending: %v", err)
	}
	if len(got) != 1 || got[0].ID != old.ID {
		t.Errorf("listPending = %v, want exactly [%s]", got, old.ID)
	}
}

func TestPaymentEventRepository_ListPending_RespectsLimit(t *testing.T) {
	repo := NewPaymentEventRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	now := time.Now()
	for i := 0; i < 3; i++ {
		evt := newTestPaymentEvent("stripe", "evt_"+string(rune('a'+i)), ChannelStatusPending, now.Add(-time.Hour))
		if _, err := repo.InsertIfNew(ctx, evt); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	got, err := repo.listPending(ctx, now, 2)
	if err != nil {
		t.Fatalf("listPending: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("listPending returned %d rows, want 2 (the limit)", len(got))
	}
}

func TestPaymentEventRepository_MarkStatus(t *testing.T) {
	repo := NewPaymentEventRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	evt := newTestPaymentEvent("stripe", "evt_1", ChannelStatusPending, time.Now())
	if _, err := repo.InsertIfNew(ctx, evt); err != nil {
		t.Fatalf("InsertIfNew: %v", err)
	}

	if err := repo.markStatus(ctx, evt.ID, ChannelStatusSucceeded, Money{Cents: 2900, Currency: "usd"}); err != nil {
		t.Fatalf("markStatus: %v", err)
	}

	got, err := repo.Get(ctx, evt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != string(ChannelStatusSucceeded) {
		t.Errorf("Status = %q, want %q", got.Status, ChannelStatusSucceeded)
	}
	if got.Amount() != (Money{Cents: 2900, Currency: "usd"}) {
		t.Errorf("Amount = %+v, want {2900 usd}", got.Amount())
	}
}

// TestPaymentEventRepository_MarkStatus_OverwritesZeroAmount is the
// regression test for the zero-amount-stuck defect: a row inserted with a
// zero-valued Amount -- exactly the shape event.go's
// normalizeCheckoutSession's ChannelStatusPending branch produces for an
// unsettled checkout.session.completed webhook -- would keep Amount=0
// forever even after resolving to Succeeded unless markStatus's third
// parameter is genuinely persisted. markStatus without the Money
// parameter would only ever write Status, leaving AmountCents/Currency
// at their zero-valued insert-time values forever.
func TestPaymentEventRepository_MarkStatus_OverwritesZeroAmount(t *testing.T) {
	repo := NewPaymentEventRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	evt := newTestPaymentEvent("stripe", "evt_zero_amount", ChannelStatusPending, time.Now())
	evt.SetAmount(Money{}) // the zeroed shape normalizeCheckoutSession's pending branch inserts
	if _, err := repo.InsertIfNew(ctx, evt); err != nil {
		t.Fatalf("InsertIfNew: %v", err)
	}

	// The active-polling fallback re-queries and finds the payment actually
	// succeeded, with a real amount -- QueryStatus's own authoritative
	// answer, never the webhook's zeroed number.
	resolved := Money{Cents: 2900, Currency: "usd"}
	if err := repo.markStatus(ctx, evt.ID, ChannelStatusSucceeded, resolved); err != nil {
		t.Fatalf("markStatus: %v", err)
	}

	got, err := repo.Get(ctx, evt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != string(ChannelStatusSucceeded) {
		t.Errorf("Status = %q, want %q", got.Status, ChannelStatusSucceeded)
	}
	if got.Amount() != resolved {
		t.Errorf("Amount = %+v, want %+v -- a real, money-moved payment must not be permanently ledgered as a zero-amount success", got.Amount(), resolved)
	}
}

// TestPaymentEventRepository_MarkStatus_CannotRegressResolvedRow pins the
// guarded-update rule: markStatus must never be an unguarded update keyed on the
// row id alone, which would let any caller overwrite a row's resolved Status with
// an older one -- e.g. two overlapping poll passes for one stuck row (or a
// poll racing the webhook that resolved the row) could have the later,
// stale mark clobber the earlier resolution the record had already
// committed. The payment_events row is the ledger of record: once a row
// has been marked out of Pending, no later mark may change it -- the
// UPDATE's WHERE is keyed on the row's own current Status (Pending), so
// an attempt against an already-resolved row affects nothing and returns
// nil: the record stands, nothing regresses. An unguarded second mark
// would overwrite the row back to Failed.
func TestPaymentEventRepository_MarkStatus_CannotRegressResolvedRow(t *testing.T) {
	repo := NewPaymentEventRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	evt := newTestPaymentEvent("stripe", "evt_no_regress", ChannelStatusPending, time.Now())
	if _, err := repo.InsertIfNew(ctx, evt); err != nil {
		t.Fatalf("InsertIfNew: %v", err)
	}

	// The authoritative poll re-query resolves the stuck row to Succeeded.
	resolved := Money{Cents: 2900, Currency: "usd"}
	if err := repo.markStatus(ctx, evt.ID, ChannelStatusSucceeded, resolved); err != nil {
		t.Fatalf("markStatus (first): %v", err)
	}

	// A second, older mark (a stale overlapping pass answering Failed, or a
	// racing duplicate) must not regress the row.
	stale := Money{Cents: 2900, Currency: "usd"}
	if err := repo.markStatus(ctx, evt.ID, ChannelStatusFailed, stale); err != nil {
		t.Fatalf("markStatus (second): %v", err)
	}

	got, err := repo.Get(ctx, evt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != string(ChannelStatusSucceeded) {
		t.Errorf("Status after a second, older mark = %q, want %q -- the row's first resolution is the record and must not regress", got.Status, ChannelStatusSucceeded)
	}
	if got.Amount() != resolved {
		t.Errorf("Amount = %+v, want %+v", got.Amount(), resolved)
	}

	// A mark naming a row that does not exist is refused, classified by the
	// disambiguating re-read rather than silently swallowed as success.
	if err := repo.markStatus(ctx, "does-not-exist", ChannelStatusSucceeded, resolved); !hasCode(err, ErrPaymentEventNotFound.Code) {
		t.Errorf("markStatus(missing id): err = %v, want %s", err, ErrPaymentEventNotFound.Code)
	}
}

// TestPaymentEventRepository_AssertIsolated proves PaymentEvent is
// genuinely tenant-scoped -- unlike Plan's dual-domain shape (plan_test.go's
// AssertNotTenantScoped), every payment_events row belongs to exactly one
// tenant.
func TestPaymentEventRepository_AssertIsolated(t *testing.T) {
	repo := NewPaymentEventRepository(newTestDB(t))
	n := 0
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *PaymentEvent {
		n++
		evt := newTestPaymentEvent("stripe", "probe-event-"+string(tenant)+"-"+strconv.Itoa(n), ChannelStatusPending, time.Now())
		evt.ID = "probe-payment-event-" + string(tenant) + "-" + strconv.Itoa(n)
		return evt
	})
}
