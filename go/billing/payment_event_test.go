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
	// be recognized as already recorded, per
	// docs/internal/06-billing-and-metering.md's insert-first-to-dedup rule.
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

	if err := repo.markStatus(ctx, evt.ID, ChannelStatusSucceeded); err != nil {
		t.Fatalf("markStatus: %v", err)
	}

	got, err := repo.Get(ctx, evt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != string(ChannelStatusSucceeded) {
		t.Errorf("Status = %q, want %q", got.Status, ChannelStatusSucceeded)
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
