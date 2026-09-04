package integration

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// alwaysAllowURL is a withWebhookURLValidator override that accepts any URL
// -- this module's own tests must be able to create subscriptions pointing
// at an httptest.Server on loopback, which ValidateWebhookURL's real,
// production behavior always refuses (this file's own point: SSRF
// protection is tested directly against ValidateWebhookURL and isBlockedIP
// in ssrf_test.go, not re-exercised here).
func alwaysAllowURL(context.Context, string) error { return nil }

// testMapping is the fixed EventMapping every test in this file and
// webhook_delivery_test.go that needs one wires, unless it needs to prove
// something about a SPECIFIC mapping's own fields.
var testMapping = EventMapping{
	InternalType:  "test.thing.happened",
	PublicType:    "test.thing.happened",
	PublicVersion: "v1",
	Transform: func(_ context.Context, evt pkgcore.Event) (json.RawMessage, error) {
		return json.RawMessage(`{"seen":true}`), nil
	},
}

// newWebhookTestService builds a fully Attach-ed Module/Service pair over a
// fresh webhook-capable test database, with testMapping declared and SSRF
// validation bypassed (see alwaysAllowURL) so tests can freely target
// httptest servers. Extra options apply after those two defaults, so a test
// needing a real jobs.Queue or a specific http.Client passes them here.
func newWebhookTestService(t *testing.T, opts ...Option) (*Module, *Service) {
	t.Helper()
	base := []Option{
		WithEventMapping(testMapping),
		withWebhookURLValidator(alwaysAllowURL),
	}
	m := NewModule(newWebhookTestDB(t), append(base, opts...)...)
	reg := newTestRegistry(t)
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	svc, err := m.Attach(reg)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	return m, svc
}

func TestService_CreateWebhookSubscription_ReturnsRawSecretOnce(t *testing.T) {
	_, svc := newWebhookTestService(t)

	created, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		URL:        "https://example.com/hook",
		EventTypes: []string{"test.thing.happened"},
		CreatedBy:  "user-1",
	})
	if err != nil {
		t.Fatalf("CreateWebhookSubscription: %v", err)
	}
	if created.Secret == "" {
		t.Error("Secret is empty, want the raw signing secret")
	}
	if created.ID == "" {
		t.Error("ID is empty")
	}

	list, err := svc.ListWebhookSubscriptions(ctxFor(testTenant))
	if err != nil {
		t.Fatalf("ListWebhookSubscriptions: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len(list) = %d, want 1", len(list))
	}
	// The list surface never re-exposes the secret -- WebhookSubscriptionSummary
	// has no Secret field at all, which is a compile-time guarantee this test
	// documents rather than needs to assert at runtime.
	if list[0].ID != created.ID || list[0].URL != created.URL {
		t.Errorf("list[0] = %+v, want it to match the created subscription", list[0])
	}
}

func TestService_CreateWebhookSubscription_EmptyCreatedBy_Refused(t *testing.T) {
	_, svc := newWebhookTestService(t)
	_, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		URL: "https://example.com/hook", EventTypes: []string{"test.thing.happened"},
	})
	if !apperrIs(err, ErrCreatedByRequired) {
		t.Errorf("error = %v, want ErrCreatedByRequired", err)
	}
}

func TestService_CreateWebhookSubscription_EmptyURL_Refused(t *testing.T) {
	_, svc := newWebhookTestService(t)
	_, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		CreatedBy: "user-1", EventTypes: []string{"test.thing.happened"},
	})
	if !apperrIs(err, ErrWebhookURLRequired) {
		t.Errorf("error = %v, want ErrWebhookURLRequired", err)
	}
}

func TestService_CreateWebhookSubscription_EmptyEventTypes_Refused(t *testing.T) {
	_, svc := newWebhookTestService(t)
	_, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		CreatedBy: "user-1", URL: "https://example.com/hook",
	})
	if !apperrIs(err, ErrEventTypesRequired) {
		t.Errorf("error = %v, want ErrEventTypesRequired", err)
	}
}

func TestService_CreateWebhookSubscription_UnknownEventType_Refused(t *testing.T) {
	_, svc := newWebhookTestService(t)
	_, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		CreatedBy: "user-1", URL: "https://example.com/hook", EventTypes: []string{"no.such.type"},
	})
	if !apperrIs(err, ErrWebhookEventTypeUnknown) {
		t.Errorf("error = %v, want ErrWebhookEventTypeUnknown", err)
	}
}

// TestService_CreateWebhookSubscription_BlockedURL_Refused proves the
// creation-time SSRF gate is actually wired into the service path (not
// only unit-tested in isolation against ValidateWebhookURL) -- this test
// uses the REAL validator, not alwaysAllowURL.
func TestService_CreateWebhookSubscription_BlockedURL_Refused(t *testing.T) {
	_, svc := newWebhookTestService(t, withWebhookURLValidator(nil)) // nil restores the production ValidateWebhookURL
	_, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		CreatedBy: "user-1", URL: "http://127.0.0.1/hook", EventTypes: []string{"test.thing.happened"},
	})
	if !apperrIs(err, ErrWebhookURLBlocked) {
		t.Errorf("error = %v, want ErrWebhookURLBlocked", err)
	}
}

func TestService_UpdateWebhookSubscription_ChangesURLEventTypesAndActive(t *testing.T) {
	_, svc := newWebhookTestService(t)
	created, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		URL: "https://example.com/hook", EventTypes: []string{"test.thing.happened"}, CreatedBy: "user-1",
	})
	if err != nil {
		t.Fatalf("CreateWebhookSubscription: %v", err)
	}

	newURL := "https://example.com/hook-v2"
	inactive := false
	summary, err := svc.UpdateWebhookSubscription(ctxFor(testTenant), UpdateWebhookSubscriptionInput{
		ID:     created.ID,
		URL:    &newURL,
		Active: &inactive,
	})
	if err != nil {
		t.Fatalf("UpdateWebhookSubscription: %v", err)
	}
	if summary.URL != newURL {
		t.Errorf("URL = %q, want %q", summary.URL, newURL)
	}
	if summary.Active {
		t.Error("Active = true, want false")
	}
	if len(summary.EventTypes) != 1 || summary.EventTypes[0] != "test.thing.happened" {
		t.Errorf("EventTypes = %v, want unchanged [test.thing.happened]", summary.EventTypes)
	}
}

func TestService_UpdateWebhookSubscription_EmptyEventTypesSlice_Refused(t *testing.T) {
	_, svc := newWebhookTestService(t)
	created, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		URL: "https://example.com/hook", EventTypes: []string{"test.thing.happened"}, CreatedBy: "user-1",
	})
	if err != nil {
		t.Fatalf("CreateWebhookSubscription: %v", err)
	}
	_, err = svc.UpdateWebhookSubscription(ctxFor(testTenant), UpdateWebhookSubscriptionInput{
		ID: created.ID, EventTypes: []string{},
	})
	if !apperrIs(err, ErrEventTypesRequired) {
		t.Errorf("error = %v, want ErrEventTypesRequired", err)
	}
}

func TestService_UpdateWebhookSubscription_NotFound_Refused(t *testing.T) {
	_, svc := newWebhookTestService(t)
	_, err := svc.UpdateWebhookSubscription(ctxFor(testTenant), UpdateWebhookSubscriptionInput{ID: "no-such-id"})
	if !apperrIs(err, ErrWebhookSubscriptionNotFound) {
		t.Errorf("error = %v, want ErrWebhookSubscriptionNotFound", err)
	}
}

func TestService_UpdateWebhookSubscription_CrossTenant_NotFound(t *testing.T) {
	_, svc := newWebhookTestService(t)
	created, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		URL: "https://example.com/hook", EventTypes: []string{"test.thing.happened"}, CreatedBy: "user-1",
	})
	if err != nil {
		t.Fatalf("CreateWebhookSubscription: %v", err)
	}
	_, err = svc.UpdateWebhookSubscription(ctxFor("tenant-other"), UpdateWebhookSubscriptionInput{ID: created.ID})
	if !apperrIs(err, ErrWebhookSubscriptionNotFound) {
		t.Errorf("error = %v, want ErrWebhookSubscriptionNotFound (cross-tenant indistinguishable from absent)", err)
	}
}

func TestService_DeleteWebhookSubscription_RemovesIt(t *testing.T) {
	_, svc := newWebhookTestService(t)
	created, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		URL: "https://example.com/hook", EventTypes: []string{"test.thing.happened"}, CreatedBy: "user-1",
	})
	if err != nil {
		t.Fatalf("CreateWebhookSubscription: %v", err)
	}
	if deleteErr := svc.DeleteWebhookSubscription(ctxFor(testTenant), created.ID); deleteErr != nil {
		t.Fatalf("DeleteWebhookSubscription: %v", deleteErr)
	}
	list, err := svc.ListWebhookSubscriptions(ctxFor(testTenant))
	if err != nil {
		t.Fatalf("ListWebhookSubscriptions: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("len(list) = %d after delete, want 0", len(list))
	}
	if err := svc.DeleteWebhookSubscription(ctxFor(testTenant), created.ID); !apperrIs(err, ErrWebhookSubscriptionNotFound) {
		t.Errorf("second delete error = %v, want ErrWebhookSubscriptionNotFound", err)
	}
}

func TestService_ListRecentWebhookDeliveries_EmptyForUnknownSubscription(t *testing.T) {
	_, svc := newWebhookTestService(t)
	got, err := svc.ListRecentWebhookDeliveries(ctxFor(testTenant), "no-such-subscription", 10)
	if err != nil {
		t.Fatalf("ListRecentWebhookDeliveries: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
}
