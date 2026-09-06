package admin

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/pkgcore"
)

// registerTestUser registers a real authn account through env's own
// *authn.Module, returning its id. locale is passed through verbatim
// (authn.RegisterInput.Locale, empty meaning "never chosen").
func registerTestUser(t *testing.T, env testAdminEnv, email, locale string) string {
	t.Helper()
	user, err := env.Authn.Service().Register(context.Background(), authn.RegisterInput{
		Email:       email,
		Password:    "a perfectly fine passphrase",
		DisplayName: "Locale Test Target",
		Locale:      locale,
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	return user.ID
}

// waitForSendRecord polls notification's own SendRecordRepository until a
// record for typeKey lands under tenant, or the deadline passes -- the
// observable proof that a REAL notification.Dispatch call actually got
// enqueued and the worker actually ran it: a Dispatch whose validate()
// refused an empty Locale never reaches the queue at all, so no send
// record -- succeeded, failed or skipped -- would ever exist for it.
func waitForSendRecord(t *testing.T, env testAdminEnv, tenant pkgcore.TenantID, typeKey string) *notification.SendRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		records, err := env.Notification.Deliveries().SendRecords().ListByFilter(context.Background(), notification.SendRecordFilter{
			TenantID: string(tenant),
			Limit:    10,
		})
		if err != nil {
			t.Fatalf("ListByFilter() error = %v", err)
		}
		for i := range records {
			if records[i].TypeKey == typeKey {
				return &records[i]
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no send record for type %q landed under tenant %q within the deadline", typeKey, tenant)
	return nil
}

// TestImpersonationService_Start_NoLocale_ResolvesThroughAuthn_RealDispatch
// is P1-1's THE scenario: Start is driven through the REAL
// notification.DeliveryService (buildTestAdminModule's genuine wiring --
// no fakeNotifier anywhere), with StartInput.Locale left empty, against a
// real authn user who has never chosen a locale (Locale: "" at
// registration).
//
// On unfixed main this StartInput.Locale="" was forwarded straight into
// notification.Dispatch, whose own validate() refuses an empty Locale for
// a RecipientClassUser recipient -- Dispatch returns before ever touching
// the queue -- and notifyStarted's Warn-and-swallow let Start still return
// a grant (201) with the mandatory notification silently never even
// enqueued. This test would fail against that behaviour: it demands an
// actual, successfully-settled send record, which cannot exist for a
// dispatch validate() ever refused.
func TestImpersonationService_Start_NoLocale_ResolvesThroughAuthn_RealDispatch(t *testing.T) {
	env := buildTestAdminModule(t)
	if err := env.Queue.RegisterHandler(env.Notification.Deliveries()); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const tenant = pkgcore.TenantID("tenant-locale-fallback")
	if _, err := env.Org.Tree().CreateRoot(pkgcore.WithTenant(context.Background(), tenant), "Locale Fallback Co", "workspace"); err != nil {
		t.Fatalf("CreateRoot() error = %v", err)
	}
	targetID := registerTestUser(t, env, "locale-fallback-target@example.com", "")

	grant, err := env.Admin.Impersonation().Start(context.Background(), StartInput{
		AdminUserID:    "admin-locale-1",
		TargetUserID:   targetID,
		TargetTenantID: tenant,
		Reason:         "prove the locale falls back to authn.DefaultLocale",
	})
	if err != nil {
		t.Fatalf("Start() error = %v, want success (the target's empty authn.User.Locale must resolve to authn.DefaultLocale)", err)
	}
	if grant.ID == "" {
		t.Fatal("Start() returned a grant with no id")
	}

	rec := waitForSendRecord(t, env, tenant, NotificationTypeImpersonationStarted)
	if rec.Status != notification.SendRecordStatusSucceeded && rec.Status != notification.SendRecordStatusSkipped {
		t.Fatalf("send record status = %q, want %q or %q (a channel skip is legal -- fakeUserAddressResolver reports no email -- but the in-app channel must succeed)",
			rec.Status, notification.SendRecordStatusSucceeded, notification.SendRecordStatusSkipped)
	}
}

// TestImpersonationService_Start_ExplicitLocale_UsedVerbatim_RealDispatch
// is P1-1's second, unchanged leg -- "a request WITH a locale keeps
// working exactly as today": an explicit StartInput.Locale is trusted
// verbatim and never overridden by the target's own authn.User.Locale,
// against the same real, non-fake notification pipeline.
func TestImpersonationService_Start_ExplicitLocale_UsedVerbatim_RealDispatch(t *testing.T) {
	env := buildTestAdminModule(t)
	if err := env.Queue.RegisterHandler(env.Notification.Deliveries()); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const tenant = pkgcore.TenantID("tenant-locale-explicit")
	if _, err := env.Org.Tree().CreateRoot(pkgcore.WithTenant(context.Background(), tenant), "Locale Explicit Co", "workspace"); err != nil {
		t.Fatalf("CreateRoot() error = %v", err)
	}
	// The target's OWN stored locale is en-US; the explicit request below
	// asks for zh-CN, which must be what actually gets used.
	targetID := registerTestUser(t, env, "locale-explicit-target@example.com", "en-US")

	grant, err := env.Admin.Impersonation().Start(context.Background(), StartInput{
		AdminUserID:    "admin-locale-2",
		TargetUserID:   targetID,
		TargetTenantID: tenant,
		Reason:         "prove an explicit locale is trusted verbatim",
		Locale:         "zh-CN",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if grant.ID == "" {
		t.Fatal("Start() returned a grant with no id")
	}

	rec := waitForSendRecord(t, env, tenant, NotificationTypeImpersonationStarted)
	if rec.Status != notification.SendRecordStatusSucceeded && rec.Status != notification.SendRecordStatusSkipped {
		t.Fatalf("send record status = %q, want %q or %q", rec.Status, notification.SendRecordStatusSucceeded, notification.SendRecordStatusSkipped)
	}
}

// TestImpersonationService_Start_UnknownTarget_RefusedWithNoGrant proves
// the honest refusal contract for a target that cannot even be resolved:
// no real authn account exists for TargetUserID, so locale resolution
// itself fails with authn.ErrNotFound, Start refuses with
// ErrImpersonationTargetNotFound, and -- exactly like the SystemDomain
// and NotifierFailure refusal tests -- no grant row is ever written.
func TestImpersonationService_Start_UnknownTarget_RefusedWithNoGrant(t *testing.T) {
	env := buildTestAdminModule(t)
	if err := env.Queue.RegisterHandler(env.Notification.Deliveries()); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const tenant = pkgcore.TenantID("tenant-locale-unknown")
	if _, err := env.Org.Tree().CreateRoot(pkgcore.WithTenant(context.Background(), tenant), "Locale Unknown Co", "workspace"); err != nil {
		t.Fatalf("CreateRoot() error = %v", err)
	}

	_, err := env.Admin.Impersonation().Start(context.Background(), StartInput{
		AdminUserID:    "admin-locale-3",
		TargetUserID:   "no-such-user",
		TargetTenantID: tenant,
		Reason:         "prove an unresolvable target is refused, not silently 201'd",
	})
	if !isCode(err, ErrImpersonationTargetNotFound.Code) {
		t.Fatalf("Start() error = %v, want %s", err, ErrImpersonationTargetNotFound.Code)
	}

	active, listErr := env.Admin.Impersonation().ListActive(context.Background())
	if listErr != nil {
		t.Fatalf("ListActive() error = %v", listErr)
	}
	if len(active) != 0 {
		t.Fatalf("ListActive() = %+v, want no grant ever written for an unresolvable target", active)
	}
}
