package tenancy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// errAuditSinkUnavailable stands in for a subscriber failure such as a down
// audit-log sink. Subscribing a handler that always returns it is how these
// tests force the real pkgcore.EventBus.Publish to fail, instead of writing a
// bespoke EventBus implementation -- per pkgcore/AGENTS.md: "Do not write a
// mock for KVStore or EventBus. NewMemoryEventBus and NewMemoryKVStore are
// the test doubles."
var errAuditSinkUnavailable = errors.New("audit sink unavailable")

// TestWithSystemContext_Success covers the happy path: the bus receives
// exactly one audit event carrying the reason's Actor/Purpose/Ticket, and the
// returned context is genuinely elevated. It also covers how the event's
// TenantID is populated: correlated from the parent context's tenant when
// there is one, left at its zero value otherwise.
func TestWithSystemContext_Success(t *testing.T) {
	const (
		purposeNoTenant   pkgcore.SystemPurpose = "tenancy_test.system_context_success.no_tenant"
		purposeWithTenant pkgcore.SystemPurpose = "tenancy_test.system_context_success.with_tenant"
	)
	pkgcore.RegisterSystemPurpose(purposeNoTenant)
	pkgcore.RegisterSystemPurpose(purposeWithTenant)

	tests := []struct {
		name       string
		parent     context.Context
		reason     pkgcore.SystemReason
		wantTenant pkgcore.TenantID // zero value means the event carries no tenant
	}{
		{
			name:   "no tenant on the parent context",
			parent: context.Background(),
			reason: pkgcore.SystemReason{Actor: "admin@example.com", Purpose: purposeNoTenant, Ticket: "SUP-1234"},
		},
		{
			name:   "ticket is optional",
			parent: context.Background(),
			reason: pkgcore.SystemReason{Actor: "jobs-worker", Purpose: purposeNoTenant},
		},
		{
			name:       "tenant already on the parent context is correlated onto the event",
			parent:     pkgcore.WithTenant(context.Background(), pkgcore.TenantID("acme")),
			reason:     pkgcore.SystemReason{Actor: "admin@example.com", Purpose: purposeWithTenant, Ticket: "SUP-5678"},
			wantTenant: pkgcore.TenantID("acme"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bus := pkgcore.NewMemoryEventBus()
			var received []pkgcore.Event
			bus.Subscribe(EventSystemContextEntered, func(_ context.Context, evt pkgcore.Event) error {
				received = append(received, evt)
				return nil
			})

			before := time.Now()
			ctx, err := WithSystemContext(tt.parent, bus, tt.reason)
			after := time.Now()
			if err != nil {
				t.Fatalf("WithSystemContext returned unexpected error: %v", err)
			}

			// The returned context is genuinely elevated, and the parent is
			// untouched.
			gotReason, ok := pkgcore.SystemReasonFromContext(ctx)
			if !ok {
				t.Fatal("SystemReasonFromContext reported no reason after a successful WithSystemContext")
			}
			if gotReason != tt.reason {
				t.Errorf("SystemReasonFromContext = %+v, want %+v", gotReason, tt.reason)
			}
			if _, parentElevated := pkgcore.SystemReasonFromContext(tt.parent); parentElevated {
				t.Error("WithSystemContext mutated the parent context")
			}

			// Exactly one audit event was published, with the right shape.
			if len(received) != 1 {
				t.Fatalf("bus received %d events, want exactly 1: %+v", len(received), received)
			}
			evt := received[0]
			if evt.Type != EventSystemContextEntered {
				t.Errorf("event.Type = %q, want %q", evt.Type, EventSystemContextEntered)
			}
			if evt.TenantID != tt.wantTenant {
				t.Errorf("event.TenantID = %q, want %q", evt.TenantID, tt.wantTenant)
			}

			payload, ok := evt.Payload.(SystemContextEnteredEvent)
			if !ok {
				t.Fatalf("event.Payload = %#v (%T), want a SystemContextEnteredEvent", evt.Payload, evt.Payload)
			}
			if payload.Actor != tt.reason.Actor {
				t.Errorf("payload.Actor = %q, want %q", payload.Actor, tt.reason.Actor)
			}
			if payload.Purpose != tt.reason.Purpose {
				t.Errorf("payload.Purpose = %q, want %q", payload.Purpose, tt.reason.Purpose)
			}
			if payload.Ticket != tt.reason.Ticket {
				t.Errorf("payload.Ticket = %q, want %q", payload.Ticket, tt.reason.Ticket)
			}
			if payload.EnteredAt.Before(before) || payload.EnteredAt.After(after) {
				t.Errorf("payload.EnteredAt = %v, want between %v and %v", payload.EnteredAt, before, after)
			}
		})
	}
}

// TestWithSystemContext_RejectedByPkgcore_NoEventPublished covers reasons
// pkgcore.WithSystemContext itself rejects (an unregistered Purpose, or an
// empty Actor): WithSystemContext must fail the same way, with the returned
// context left unelevated, and it must not publish anything -- nothing was
// ever elevated, so there is nothing to audit.
func TestWithSystemContext_RejectedByPkgcore_NoEventPublished(t *testing.T) {
	const registeredPurpose pkgcore.SystemPurpose = "tenancy_test.system_context_rejected.registered"
	pkgcore.RegisterSystemPurpose(registeredPurpose)

	tests := []struct {
		name    string
		reason  pkgcore.SystemReason
		wantErr error
	}{
		{
			name: "unregistered purpose",
			reason: pkgcore.SystemReason{
				Actor:   "admin@example.com",
				Purpose: pkgcore.SystemPurpose("tenancy_test.system_context_rejected.never_registered"),
			},
			wantErr: pkgcore.ErrSystemPurposeNotRegistered,
		},
		{
			name:    "empty actor",
			reason:  pkgcore.SystemReason{Purpose: registeredPurpose},
			wantErr: pkgcore.ErrSystemActorRequired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bus := pkgcore.NewMemoryEventBus()
			var publishCalls int
			bus.Subscribe(EventSystemContextEntered, func(_ context.Context, _ pkgcore.Event) error {
				publishCalls++
				return nil
			})

			parent := context.Background()
			ctx, err := WithSystemContext(parent, bus, tt.reason)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("WithSystemContext error = %v, want one matching %v", err, tt.wantErr)
			}
			if ctx != parent {
				t.Error("WithSystemContext did not return the original context unchanged on failure")
			}
			if _, ok := pkgcore.SystemReasonFromContext(ctx); ok {
				t.Error("a rejected WithSystemContext still elevated the returned context")
			}
			if publishCalls != 0 {
				t.Errorf("bus received %d events, want 0: nothing was elevated, so there was nothing to audit", publishCalls)
			}
		})
	}
}

// TestWithSystemContext_PublishFails_FailsClosedWithoutElevatingContext is
// the core fail-closed guarantee this wrapper exists for: pkgcore's own
// WithSystemContext succeeds, but the audit publish itself fails (a
// subscriber -- e.g. the audit-log sink -- returns an error). WithSystemContext
// must not return the already-elevated context in that case: a granted escape
// hatch with no audit record is exactly the gap this wrapper closes.
func TestWithSystemContext_PublishFails_FailsClosedWithoutElevatingContext(t *testing.T) {
	const purpose pkgcore.SystemPurpose = "tenancy_test.system_context_publish_fails"
	pkgcore.RegisterSystemPurpose(purpose)
	reason := pkgcore.SystemReason{Actor: "admin@example.com", Purpose: purpose, Ticket: "SUP-9999"}

	// A real in-memory bus (per pkgcore/AGENTS.md, EventBus must not be
	// mocked) whose one subscriber always fails, so Publish itself returns a
	// non-nil error.
	bus := pkgcore.NewMemoryEventBus()
	var publishCalls int
	bus.Subscribe(EventSystemContextEntered, func(_ context.Context, _ pkgcore.Event) error {
		publishCalls++
		return errAuditSinkUnavailable
	})

	parent := context.Background()
	ctx, err := WithSystemContext(parent, bus, reason)

	if err == nil {
		t.Fatal("WithSystemContext returned a nil error despite the audit publish failing")
	}
	if publishCalls != 1 {
		t.Fatalf("bus subscriber invoked %d times, want exactly 1", publishCalls)
	}

	// The failure is reported through the structured tenancy error. Because
	// WithParam/WithCause always derive a new *apperr.Error rather than
	// mutating the receiver, the package-level sentinel must be matched by
	// .Code via apperr.As, not errors.Is/== -- see ErrAuditPublishFailed's own
	// doc comment.
	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("WithSystemContext error = %v (%T), want an *apperr.Error", err, err)
	}
	if appErr.Code != ErrAuditPublishFailed.Code {
		t.Errorf("error code = %q, want %q", appErr.Code, ErrAuditPublishFailed.Code)
	}
	// The underlying subscriber failure must still be reachable, for logs and
	// errors.Is-based diagnosis.
	if !errors.Is(err, errAuditSinkUnavailable) {
		t.Errorf("WithSystemContext error does not wrap the publish failure: %v", err)
	}

	// The crux of the fail-closed requirement: the escape hatch must NOT have
	// been granted on the returned context, which must be the original,
	// unelevated one.
	if gotReason, ok := pkgcore.SystemReasonFromContext(ctx); ok {
		t.Errorf("WithSystemContext elevated the returned context despite the audit publish failing: %+v", gotReason)
	}
	if ctx != parent {
		t.Error("WithSystemContext did not return the original context unchanged on publish failure")
	}
}

// TestWithSystemContext_RepeatedCalls_OneEventPerCall proves repeated use
// produces exactly one audit event per call -- never zero (a dropped audit
// record) and never more than one (a duplicated record inflating the trail).
func TestWithSystemContext_RepeatedCalls_OneEventPerCall(t *testing.T) {
	const purpose pkgcore.SystemPurpose = "tenancy_test.system_context_repeated"
	pkgcore.RegisterSystemPurpose(purpose)

	bus := pkgcore.NewMemoryEventBus()
	var received []pkgcore.Event
	bus.Subscribe(EventSystemContextEntered, func(_ context.Context, evt pkgcore.Event) error {
		received = append(received, evt)
		return nil
	})

	reasons := []pkgcore.SystemReason{
		{Actor: "admin-1@example.com", Purpose: purpose, Ticket: "SUP-1"},
		{Actor: "admin-2@example.com", Purpose: purpose, Ticket: "SUP-2"},
		{Actor: "jobs-worker", Purpose: purpose},
	}

	for i, reason := range reasons {
		if _, err := WithSystemContext(context.Background(), bus, reason); err != nil {
			t.Fatalf("call %d: WithSystemContext returned unexpected error: %v", i, err)
		}

		if len(received) != i+1 {
			t.Fatalf("after call %d: bus received %d events total, want %d (one per call, no drops or duplicates)",
				i, len(received), i+1)
		}

		payload, ok := received[i].Payload.(SystemContextEnteredEvent)
		if !ok {
			t.Fatalf("call %d: event.Payload = %#v, want a SystemContextEnteredEvent", i, received[i].Payload)
		}
		if payload.Actor != reason.Actor || payload.Ticket != reason.Ticket {
			t.Errorf("call %d: payload = %+v, want it to match reason %+v", i, payload, reason)
		}
	}

	if len(received) != len(reasons) {
		t.Fatalf("total events received = %d, want %d", len(received), len(reasons))
	}
}
