package flowtests

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/app"
)

// TestSelfServiceSignup_ClinicSignInDependsOnNoHostLedger is the
// regression test for the sign-in tenant-resolution contract: a
// runtime-created self-service account's browser-shaped sign-in must
// resolve its clinic tenant from org's own memberships table ALONE --
// with every trace of the host's own clinic ledger wiped between the
// boots. The sign-in store scans org's memberships
// (MemberService.TenantsOf) for the tenant to land the clinic owner in
// and never consults a row of the host's self_service_clinics
// bookkeeping; a resolution that needed the ledger would answer the
// clinic owner's no-tenant sign-in with the uniform 401
// authn.invalid_credentials every failed sign-in answers, no matter how
// real the org membership row was. The wipe below is that dependency's
// probe: it reproduces a ledger-less state, so a sign-in that resolves
// the clinic from the org row alone succeeds while one that needs the
// ledger answers 401 and fails the test.
//
// The sqlite3 driver import below is this test's own, mirroring the
// raw-DB probe pattern org's own upgrade tests use: the wipe must reach
// the shared database file directly because no in-process handle
// survives between the two boots.
func TestSelfServiceSignup_ClinicSignInDependsOnNoHostLedger(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reference-app-self-service-no-ledger.db")

	boot := func() (*httptest.Server, func() error) {
		cfg := testConfig(t)
		cfg.SQLitePath = dbPath
		handler, cleanup, _, err := app.BuildServer(context.Background(), cfg)
		if err != nil {
			t.Fatalf("BuildServer: %v", err)
		}
		return httptest.NewServer(handler), cleanup
	}

	// Boot one: register a clinic owner and prove the clinic sign-in
	// works, exactly like the restart test in self_service_test.go.
	srv1, cleanup1 := boot()
	registerFreshAccount(t, srv1, selfServiceFreshEmail, selfServicePassword)
	status, code, _, tenant := browserSignIn(t, srv1, selfServiceFreshEmail, selfServicePassword)
	if status != http.StatusOK {
		t.Fatalf("boot-one sign-in of the freshly registered account: status = %d, code = %q, want %d",
			status, code, http.StatusOK)
	}
	clinic := tenant
	srv1.Close()
	if err := cleanup1(); err != nil {
		t.Fatalf("boot-one cleanup: %v", err)
	}

	// Wipe the host's own clinic-ledger table between the boots, if a
	// boot created one (internal/app/self_service.go's self_service_clinics ledger).
	// Whether the delete succeeds or the table does not exist at all,
	// what boot two signs in with may not depend on a row of this host
	// bookkeeping.
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open the shared database to wipe the ledger: %v", err)
	}
	if _, wipeErr := rawDB.Exec("DELETE FROM self_service_clinics"); wipeErr != nil &&
		!strings.Contains(strings.ToLower(wipeErr.Error()), "no such table") {
		rawDB.Close()
		t.Fatalf("wipe the self-service ledger: %v", wipeErr)
	}
	if closeErr := rawDB.Close(); closeErr != nil {
		t.Fatalf("close the shared database after wiping: %v", closeErr)
	}

	// Boot two: the same browser-shaped sign-in must still land in the
	// same clinic -- org's own memberships row answers, and no host list
	// stands between the account and its tenant.
	srv2, cleanup2 := boot()
	defer func() {
		srv2.Close()
		if err := cleanup2(); err != nil {
			t.Errorf("boot-two cleanup: %v", err)
		}
	}()
	status, code, _, tenant = browserSignIn(t, srv2, selfServiceFreshEmail, selfServicePassword)
	if status != http.StatusOK {
		t.Fatalf("boot-two sign-in of the clinic owner with the host ledger wiped: status = %d, code = %q, want %d "+
			"(the clinic tenant must resolve from org's own memberships table, not from host bookkeeping)",
			status, code, http.StatusOK)
	}
	if tenant != clinic {
		t.Fatalf("boot-two sign-in landed the principal in tenant %q, want the boot-one clinic %q", tenant, clinic)
	}
}
