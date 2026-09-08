package notification

// contact_type_unsubscribe_test.go drives the type-scoped opt-out the
// consent ledger ships this round: a verified contact narrows its consent
// to "everything but this one notification type" (ContactService
// UnsubscribeType / EnsureDeliverableForType, contact_type_unsubscribe.go),
// the finer shape the whole-contact unsubscribe of contact.go deliberately
// never covered (contact_type_unsubscribe.go's own file comment). The tests
// are white-box (package notification) for the same reasons contact_test.go
// documents: the write path's taxonomy reference is a private field, the
// opt-out rows are probed through the service's own repository, and the
// audit record of a transition is read off the env's recording bus.
//
// Every contact in this file is created business-attested (verified, no
// code send), so nothing here touches the verification-code transports; the
// one exception is the pending-contact refusal-matrix row, which creates a
// double_opt_in contact whose code send the env's recording mailer absorbs.

import (
	"fmt"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// contactWithTypeTaxonomy returns a contact env whose service carries the
// fixture type taxonomy -- the state Module.Register's attachTypes leaves a
// real host's service in -- needed by every UnsubscribeType write (the
// write validates the named type against the live registrar before any row
// is created).
func contactWithTypeTaxonomy(t *testing.T) *contactEnv {
	t.Helper()
	env := newContactEnv(t)
	env.svc.types = fixtureRegistrar{types: fixtureTypes}
	return env
}

// verifiedEmailContact creates one business-attested email contact in the
// env's tenant and returns it, the deliverable row every opt-out write in
// this file starts from.
func verifiedEmailContact(t *testing.T, env *contactEnv) *VerifiedContact {
	t.Helper()
	contact, err := env.svc.CreateContact(tenantCtx("tenant-acme"), ContactCreateInput{
		Channel:    ChannelEmail,
		Address:    "wangfang@external.example.com",
		ConsentRef: "consent-ref-1",
	})
	if err != nil {
		t.Fatalf("create the verified contact: %v", err)
	}
	if contact.Status != ContactStatusVerified {
		t.Fatalf("business-attested contact status = %s, want verified", contact.Status)
	}
	return contact
}

// assertParamErr fails t unless err is an *apperr.Error whose params carry
// key with the string value want.
func assertParamErr(t *testing.T, err error, key, want string) {
	t.Helper()
	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("error %v is not an *apperr.Error, want a %q parameter", err, want)
	}
	got, ok := appErr.Params[key].(string)
	if !ok || got != want {
		t.Errorf("param %q = %v, want %q", key, appErr.Params[key], want)
	}
}

// TestContactTypeUnsubscribe_AssertIsolated runs the tenant-data isolation
// suite every tenant-scoped repository must pass: one tenant's type-scoped
// opt-out rows must be invisible to every other, and a row can never be
// created without a tenant context. The closure returns a row with a fresh
// id on every call, as the suite requires -- the unique index is
// (tenant_id, contact_id, type_key), so no two rows the suite creates may
// collide.
func TestContactTypeUnsubscribe_AssertIsolated(t *testing.T) {
	env := newContactEnv(t)
	seq := 0
	tenancytest.AssertIsolated[ContactTypeUnsubscribe](t, env.svc.typeUnsubs.Repository,
		func(_ pkgcore.TenantID) *ContactTypeUnsubscribe {
			seq++
			return &ContactTypeUnsubscribe{
				ID:        fmt.Sprintf("ctu-%06d", seq),
				ContactID: fmt.Sprintf("contact-%06d", seq),
				TypeKey:   fmt.Sprintf("clinic.type_%d", seq),
			}
		})
}

// TestContact_UnsubscribeType_VerifiedContactOptsOutOfOneType drives the
// shape's payoff: a verified contact narrows itself out of one notification
// type, the row lands on the ledger, the transition's audit record names
// the type, the delivery gate refuses exactly that type -- and still passes
// for every other type the contact never opted out of, whole-contact
// deliverability (EnsureDeliverable) included.
func TestContact_UnsubscribeType_VerifiedContactOptsOutOfOneType(t *testing.T) {
	env := contactWithTypeTaxonomy(t)
	ctx := tenantCtx("tenant-acme")
	contact := verifiedEmailContact(t, env)

	if err := env.svc.UnsubscribeType(ctx, UnsubscribeTypeInput{
		ContactID: contact.ID,
		TypeKey:   fixtureTypeResult,
	}); err != nil {
		t.Fatalf("UnsubscribeType: %v", err)
	}

	// The row landed on the ledger, keyed on the contact and the type.
	row, err := env.svc.typeUnsubs.ByContactAndType(ctx, contact.ID, fixtureTypeResult)
	if err != nil {
		t.Fatalf("ByContactAndType: %v", err)
	}
	if row == nil {
		t.Fatal("no type-scoped opt-out row for (contact, result_ready), want one")
	}
	if row.TypeKey != fixtureTypeResult {
		t.Errorf("opt-out row type_key = %q, want %q", row.TypeKey, fixtureTypeResult)
	}

	// The transition is audited once, under its own action, with the type
	// named in the change -- the trail can reconstruct which type a contact
	// narrowed, the reason the consent actions exist at all (a tenant may
	// prune the contact rows themselves).
	events := recordedAudit(t, env, AuditActionContactTypeUnsubscribed)
	if len(events) != 1 {
		t.Fatalf("recorded %d type-unsubscribe audit events, want exactly one", len(events))
	}
	if changes := events[0].Changes; changes == nil || changes.After == nil {
		t.Fatalf("audit event carries Changes %+v, want an After map naming the type", changes)
	} else if got, _ := changes.After["type_key"].(string); got != fixtureTypeResult {
		t.Errorf("audit After type_key = %v, want %q", changes.After["type_key"], fixtureTypeResult)
	}

	// The delivery gate refuses exactly the opted-out type...
	got, err := env.svc.EnsureDeliverableForType(ctx, contact.ID, fixtureTypeResult)
	if err == nil {
		t.Fatalf("EnsureDeliverableForType(%s) returned a deliverable contact %+v, want the type-scoped refusal", fixtureTypeResult, got)
	}
	assertCode(t, err, ErrContactTypeUnsubscribed.Code)
	assertParamErr(t, err, "channel", ChannelEmail)

	// ...and still passes for the other unsubscribable type and for the
	// type-agnostic whole-contact gate: the contact stays reachable.
	if _, err := env.svc.EnsureDeliverableForType(ctx, contact.ID, fixtureTypeAppointment); err != nil {
		t.Errorf("EnsureDeliverableForType(%s) refused a contact that only opted out of %s: %v", fixtureTypeAppointment, fixtureTypeResult, err)
	}
	if _, err := env.svc.EnsureDeliverable(ctx, contact.ID); err != nil {
		t.Errorf("EnsureDeliverable refused a contact that only opted out of one type: %v", err)
	}
}

// TestContact_UnsubscribeType_DoesNotTouchOtherTypeRows pins the opt-out
// granularity at the ledger: opting out of one type writes one row and
// never any other -- a second type's probe stays empty.
func TestContact_UnsubscribeType_DoesNotTouchOtherTypeRows(t *testing.T) {
	env := contactWithTypeTaxonomy(t)
	ctx := tenantCtx("tenant-acme")
	contact := verifiedEmailContact(t, env)

	if err := env.svc.UnsubscribeType(ctx, UnsubscribeTypeInput{ContactID: contact.ID, TypeKey: fixtureTypeAppointment}); err != nil {
		t.Fatalf("UnsubscribeType: %v", err)
	}
	other, err := env.svc.typeUnsubs.ByContactAndType(ctx, contact.ID, fixtureTypeResult)
	if err != nil {
		t.Fatalf("ByContactAndType(other type): %v", err)
	}
	if other != nil {
		t.Errorf("opt-out of %s also wrote a row for %s, want one row only", fixtureTypeAppointment, fixtureTypeResult)
	}
}

// TestContact_UnsubscribeType_IdempotentRepeatEmitsNothing pins the repeat
// semantics the whole-contact Unsubscribe already established: opting out
// of the type a contact already opted out of succeeds without a second
// transition, so no second row and no second audit event -- the idempotent
// repeat is not a state change.
func TestContact_UnsubscribeType_IdempotentRepeatEmitsNothing(t *testing.T) {
	env := contactWithTypeTaxonomy(t)
	ctx := tenantCtx("tenant-acme")
	contact := verifiedEmailContact(t, env)

	in := UnsubscribeTypeInput{ContactID: contact.ID, TypeKey: fixtureTypeResult}
	if err := env.svc.UnsubscribeType(ctx, in); err != nil {
		t.Fatalf("first UnsubscribeType: %v", err)
	}
	if err := env.svc.UnsubscribeType(ctx, in); err != nil {
		t.Fatalf("idempotent repeat UnsubscribeType: %v", err)
	}

	if events := recordedAudit(t, env, AuditActionContactTypeUnsubscribed); len(events) != 1 {
		t.Errorf("recorded %d type-unsubscribe audit events after a repeat, want the one actual transition", len(events))
	}
	got, err := env.svc.repo.FindByID(ctx, contact.ID)
	if err != nil || got == nil || got.Status != ContactStatusVerified {
		t.Errorf("contact after the repeat = %+v (err %v), want the unchanged verified row", got, err)
	}
}

// TestContact_UnsubscribeType_RefusalMatrix pins every non-transition
// answer of the write path, one per row: an id no row of the tenant holds,
// a pending contact whose consent was never proved, a whole-unsubscribed
// and a bounced contact (both terminal, both already covered by a broader
// refusal than the type-scoped one), a type nobody declared, and a
// declared type whose declaration does not permit opting out
// (Unsubscribable false). Each refusal leaves no row behind.
func TestContact_UnsubscribeType_RefusalMatrix(t *testing.T) {
	env := contactWithTypeTaxonomy(t)
	ctx := tenantCtx("tenant-acme")
	contact := verifiedEmailContact(t, env)

	// A pending contact: create without a consent ref (email channel, so
	// the code send lands in the recording mailer).
	pending, err := env.svc.CreateContact(ctx, ContactCreateInput{
		Channel: ChannelEmail,
		Address: "pending@external.example.com",
	})
	if err != nil {
		t.Fatalf("create the pending contact: %v", err)
	}
	if pending.Status != ContactStatusPending {
		t.Fatalf("double_opt_in contact status = %s, want pending", pending.Status)
	}

	whole, err := env.svc.CreateContact(ctx, ContactCreateInput{
		Channel:    ChannelEmail,
		Address:    "whole@external.example.com",
		ConsentRef: "consent-ref-2",
	})
	if err != nil {
		t.Fatalf("create the whole-unsubscribe fixture contact: %v", err)
	}
	if _, err = env.svc.Unsubscribe(ctx, UnsubscribeInput{ContactID: whole.ID}); err != nil {
		t.Fatalf("unsubscribe the whole-unsubscribe fixture: %v", err)
	}

	bounced, err := env.svc.CreateContact(ctx, ContactCreateInput{
		Channel:    ChannelEmail,
		Address:    "bounced@external.example.com",
		ConsentRef: "consent-ref-3",
	})
	if err != nil {
		t.Fatalf("create the bounced fixture contact: %v", err)
	}
	if err = env.svc.MarkBounced(ctx, bounced.ID); err != nil {
		t.Fatalf("mark the bounced fixture contact bounced: %v", err)
	}

	cases := []struct {
		name    string
		contact string
		typeKey string
		want    string
	}{
		{
			name:    "unknown contact",
			contact: "no-such-contact",
			typeKey: fixtureTypeResult,
			want:    ErrContactNotFound.Code,
		},
		{
			name:    "pending contact",
			contact: pending.ID,
			typeKey: fixtureTypeResult,
			want:    ErrContactNotVerified.Code,
		},
		{
			name:    "whole-unsubscribed contact",
			contact: whole.ID,
			typeKey: fixtureTypeResult,
			want:    ErrContactUnsubscribed.Code,
		},
		{
			name:    "bounced contact",
			contact: bounced.ID,
			typeKey: fixtureTypeResult,
			want:    ErrContactBounced.Code,
		},
		{
			name:    "undeclared type",
			contact: contact.ID,
			typeKey: "clinic.never_declared",
			want:    ErrTypeNotFound.Code,
		},
		{
			name:    "type whose declaration forbids opting out",
			contact: contact.ID,
			typeKey: fixtureTypeSecurity,
			want:    ErrContactTypeOptoutNotAllowed.Code,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := env.svc.UnsubscribeType(ctx, UnsubscribeTypeInput{
				ContactID: tc.contact,
				TypeKey:   tc.typeKey,
			})
			assertCode(t, err, tc.want)
			row, err := env.svc.typeUnsubs.ByContactAndType(ctx, tc.contact, tc.typeKey)
			if err != nil {
				t.Fatalf("ByContactAndType after the refusal: %v", err)
			}
			if row != nil {
				t.Errorf("a refused UnsubscribeType wrote a row (%s, %s), want none", tc.contact, tc.typeKey)
			}
		})
	}

	if events := recordedAudit(t, env, AuditActionContactTypeUnsubscribed); len(events) != 0 {
		t.Errorf("refused UnsubscribeType calls recorded %d audit events, want none", len(events))
	}
}

// TestContact_UnsubscribeType_UnknownTypeRefusalNamesTheKey pins the type
// refusal's parameter: the offending key rides in "type_key" so a caller
// can render it back, exactly as the preference matrix's own type refusal
// does.
func TestContact_UnsubscribeType_UnknownTypeRefusalNamesTheKey(t *testing.T) {
	env := contactWithTypeTaxonomy(t)
	ctx := tenantCtx("tenant-acme")
	contact := verifiedEmailContact(t, env)

	err := env.svc.UnsubscribeType(ctx, UnsubscribeTypeInput{
		ContactID: contact.ID,
		TypeKey:   "clinic.never_declared",
	})
	assertCode(t, err, ErrTypeNotFound.Code)
	assertParamErr(t, err, "type_key", "clinic.never_declared")
}

// TestContact_UnsubscribeType_TenantScoped pins the write path's tenant
// boundary twice: tenant B cannot opt out tenant A's contact (the id is
// indistinguishable from an absent one), and an opt-out A records is never
// visible from B's probe of the same contact id.
func TestContact_UnsubscribeType_TenantScoped(t *testing.T) {
	env := contactWithTypeTaxonomy(t)
	ctxA := tenantCtx("tenant-acme")
	ctxB := tenantCtx("tenant-other")
	contact := verifiedEmailContact(t, env)

	err := env.svc.UnsubscribeType(ctxB, UnsubscribeTypeInput{
		ContactID: contact.ID,
		TypeKey:   fixtureTypeResult,
	})
	assertCode(t, err, ErrContactNotFound.Code)

	if err = env.svc.UnsubscribeType(ctxA, UnsubscribeTypeInput{
		ContactID: contact.ID,
		TypeKey:   fixtureTypeResult,
	}); err != nil {
		t.Fatalf("UnsubscribeType from the owning tenant: %v", err)
	}
	fromB, err := env.svc.typeUnsubs.ByContactAndType(ctxB, contact.ID, fixtureTypeResult)
	if err != nil {
		t.Fatalf("ByContactAndType from another tenant: %v", err)
	}
	if fromB != nil {
		t.Errorf("another tenant's probe found the opt-out row, want it indistinguishable from an absent one")
	}
}

// TestContact_EnsureDeliverableForType_WholeContactStatusesStillDominant
// pins the type-aware gate's status answers: the whole-contact terminal
// and pre-consent statuses refuse exactly as EnsureDeliverable refuses
// them, whatever type is named -- a type-scoped opt-out only ever narrows
// a verified contact's delivery, never answers for the contact's own
// status.
func TestContact_EnsureDeliverableForType_WholeContactStatusesStillDominant(t *testing.T) {
	env := contactWithTypeTaxonomy(t)
	ctx := tenantCtx("tenant-acme")

	verified := verifiedEmailContact(t, env)
	whole, err := env.svc.CreateContact(ctx, ContactCreateInput{
		Channel:    ChannelEmail,
		Address:    "whole2@external.example.com",
		ConsentRef: "consent-ref-4",
	})
	if err != nil {
		t.Fatalf("create the whole-unsubscribe fixture contact: %v", err)
	}
	if _, err = env.svc.Unsubscribe(ctx, UnsubscribeInput{ContactID: whole.ID}); err != nil {
		t.Fatalf("unsubscribe the fixture: %v", err)
	}
	bounced, err := env.svc.CreateContact(ctx, ContactCreateInput{
		Channel:    ChannelEmail,
		Address:    "bounced2@external.example.com",
		ConsentRef: "consent-ref-5",
	})
	if err != nil {
		t.Fatalf("create the bounced fixture contact: %v", err)
	}
	if err = env.svc.MarkBounced(ctx, bounced.ID); err != nil {
		t.Fatalf("mark the fixture bounced: %v", err)
	}
	pending, err := env.svc.CreateContact(ctx, ContactCreateInput{
		Channel: ChannelEmail,
		Address: "pending2@external.example.com",
	})
	if err != nil {
		t.Fatalf("create the pending fixture contact: %v", err)
	}

	if _, err := env.svc.EnsureDeliverableForType(ctx, verified.ID, fixtureTypeAppointment); err != nil {
		t.Errorf("the verified contact refused a type it never opted out of: %v", err)
	}
	for _, tc := range []struct {
		name        string
		contact     *VerifiedContact
		want        string
		withChannel bool
	}{
		{"whole-unsubscribed", whole, ErrContactUnsubscribed.Code, true},
		{"bounced", bounced, ErrContactBounced.Code, true},
		// A pending refusal deliberately carries no channel (EnsureDeliverable's
		// own contract: only the two terminal status refusals attach one, for
		// the skip record the job settles under the contact's channel).
		{"pending", pending, ErrContactNotVerified.Code, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.svc.EnsureDeliverableForType(ctx, tc.contact.ID, fixtureTypeAppointment)
			assertCode(t, err, tc.want)
			if tc.withChannel {
				assertParamErr(t, err, "channel", ChannelEmail)
			}
		})
	}
}
