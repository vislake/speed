package flowtests

// pki_revoke_gate_flow_test.go drives go/pki's signing-key revoke HTTP
// operation -- POST /api/v1/pki/signing-keys/{kid}/revoke under
// PkiRoutePath, gated by the pki entry of DemoRouteRules (internal/app/demo/demo_subject.go,
// pkiPermissionFor with pkiSubjectResolverFor's domain pin) -- through the
// composed HTTP stack, and pins the platform-domain half of the
// permission contract go/pki/module.go now records: revoking a row of
// pki_signing_keys (platform data) is gated on pki.PermissionRevokeSigningKey
// evaluated under rbac.SystemDomain, NEVER in the request tenant's domain.
//
// The negative leg is the exploit shape, made concrete: the demo
// owner role holds EVERY declared permission -- pki:revoke_signing_key
// included -- in every demo tenant (SeedDemoGrants), so a request acting
// as demo-owner in tenant-acme passes any tenant-domain evaluation of the
// permission and would revoke the platform signing key every tenant's
// tokens are verified under. The gate must refuse it anyway, because the
// subject resolver pins the signing-key revoke's evaluation tenant to
// rbac.SystemDomain, where demo-owner holds no grant at all. A route-level
// gate evaluating the permission in the request tenant's domain would
// answer that request 200 and revoke the key;
// this test's 403 expectation is the fail-before leg.
//
// The positive leg is the honest counterpart: a real platform-staff
// account -- DemoPlatformStaffEmail, holding BuiltinRoleOwner under
// rbac.SystemDomain (SeedDemoPlatformStaff, internal/app/demo/demo_admin.go) -- signs in
// with its own credential source and revokes the same key successfully.
// Without this leg the gate could be broken-closed instead of domain-
// shifted, and the demo would prove no way to reach the operation at all.
import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/apptest"
	"github.com/vislake/speed/examples/reference-app/internal/testutil"

	"github.com/vislake/speed/examples/reference-app/internal/app"
	"github.com/vislake/speed/examples/reference-app/internal/app/demo"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pki"
	"github.com/vislake/speed/go/rbac"
)

// pkiGateErrorBody is the {code, params} envelope the rbac gate (and every
// handler error) answers with.
type pkiGateErrorBody struct {
	Code string `json:"code"`
}

// TestBuildServer_PkiSigningKeyRevoke_RequiresThePlatformDomainPermission is
// the composed-stack proof described in this file's header.
func TestBuildServer_PkiSigningKeyRevoke_RequiresThePlatformDomainPermission(t *testing.T) {
	cfg := apptest.ServerConfig(t)
	cfg.DemoUsersPassword = testutil.DemoSeedPassword
	cfg.DemoPlatformStaffPassword = testutil.DemoPlatformStaffSeedPassword
	handler, cleanup, _, err := app.BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			t.Errorf("cleanup: %v", cleanupErr)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// The staff account's sign-in is what bootstraps the purpose's signing
	// key (authn's Signer runs EnsurePurpose lazily on its first token
	// issue -- the identical mechanism periodic_pki_scan_flow_test.go
	// relies on), so the key this test revokes is the app's real boot key,
	// never a hand-seeded row. The owner's sign-in provides the tenant
	// bearer token the negative leg rides on.
	status, code, staffToken := testutil.DemoLogin(t, srv, demo.DemoPlatformStaffEmail, testutil.DemoPlatformStaffSeedPassword, rbac.SystemDomain)
	if status != http.StatusOK {
		t.Fatalf("platform-staff login: status = %d, code = %q, want %d", status, code, http.StatusOK)
	}
	status, code, ownerToken := testutil.DemoLogin(t, srv, demo.DemoOwnerEmail, testutil.DemoSeedPassword, "tenant-acme")
	if status != http.StatusOK {
		t.Fatalf("demo-owner login: status = %d, code = %q, want %d", status, code, http.StatusOK)
	}

	// The observer: a second connection to the same SQLite file the
	// running server writes, read through pki's own repository -- the same
	// reach periodic_pki_scan_flow_test.go uses.
	observerDB, err := dbkit.Open(context.Background(), dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     cfg.SQLitePath,
	})
	if err != nil {
		t.Fatalf("open observer connection to %q: %v", cfg.SQLitePath, err)
	}
	t.Cleanup(func() {
		sqlDB, dbErr := observerDB.DB()
		if dbErr != nil {
			t.Errorf("observer connection handle: %v", dbErr)
			return
		}
		if closeErr := sqlDB.Close(); closeErr != nil {
			t.Errorf("close observer connection: %v", closeErr)
		}
	})
	keys := pki.NewSigningKeyRepository(observerDB)

	boot := activeSigningKey(t, keys, authn.AccessTokenKeyPurpose)
	if boot == nil {
		t.Fatalf("purpose %q has no active signing key after the two sign-ins -- authn never bootstrapped it", authn.AccessTokenKeyPurpose)
	}
	revokePath := demo.PkiRoutePath + "/signing-keys/" + boot.ID + "/revoke"

	// Negative leg: demo-owner in tenant-acme holds pki:revoke_signing_key
	// there (the owner role carries every declared permission), yet the
	// revoke must be refused: the gate evaluates that permission under
	// rbac.SystemDomain, where the header identity has no grant. This is
	// the leg that an ungated route answers with a 200 that revokes the key.
	ownerResp := storageRequest(t, srv, http.MethodPost, revokePath, ownerToken, demo.DemoOwnerUserID, "application/json",
		strings.NewReader(`{"reason":"tenant admin test revoke"}`))
	defer ownerResp.Body.Close()
	ownerBody, err := io.ReadAll(ownerResp.Body)
	if err != nil {
		t.Fatalf("owner revoke: read body: %v", err)
	}
	if ownerResp.StatusCode != http.StatusForbidden {
		t.Fatalf("tenant-domain holder revoking the platform signing key: status = %d, want %d (regression: a tenant's pki:revoke_signing_key grant must not reach platform data); body = %s",
			ownerResp.StatusCode, http.StatusForbidden, ownerBody)
	}
	var ownerErr pkiGateErrorBody
	if decodeErr := json.Unmarshal(ownerBody, &ownerErr); decodeErr != nil {
		t.Fatalf("owner revoke: decoding %s: %v", ownerBody, decodeErr)
	}
	if ownerErr.Code != rbacPermissionDeniedCode {
		t.Fatalf("owner revoke: code = %q, want %q", ownerErr.Code, rbacPermissionDeniedCode)
	}
	// The key survived the refusal.
	if still := activeSigningKey(t, keys, authn.AccessTokenKeyPurpose); still == nil || still.ID != boot.ID {
		t.Fatalf("after the refused owner revoke active key = %+v, want the boot key %s untouched", still, boot.ID)
	}

	// Positive leg: the platform-staff account -- the real principal, no
	// demo header, a SystemDomain session and the SystemDomain owner grant
	// -- revokes the same key successfully.
	staffResp := storageRequest(t, srv, http.MethodPost, revokePath, staffToken, "", "application/json",
		strings.NewReader(`{"reason":"platform staff revoke"}`))
	defer staffResp.Body.Close()
	staffBody, err := io.ReadAll(staffResp.Body)
	if err != nil {
		t.Fatalf("staff revoke: read body: %v", err)
	}
	if staffResp.StatusCode != http.StatusOK {
		t.Fatalf("platform-staff holder revoking the platform signing key: status = %d, want %d; body = %s",
			staffResp.StatusCode, http.StatusOK, staffBody)
	}
	revoked, err := keys.FindByID(context.Background(), boot.ID)
	if err != nil {
		t.Fatalf("read boot key after the staff revoke: %v", err)
	}
	if revoked.Status != pki.SigningKeyStatusRevoked {
		t.Fatalf("boot key status after the staff revoke = %q, want %q", revoked.Status, pki.SigningKeyStatusRevoked)
	}
}

// rbacPermissionDeniedCode is the error code the rbac gate answers a
// denied request with -- asserted by string here (the module's own
// constant lives in go/rbac's middleware internals) so the fail-before
// leg runs against the ungated shape too.
const rbacPermissionDeniedCode = "rbac.permission_denied"
