package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestSelfServiceSignup_ClinicSignInDependsOnNoHostLedger is the round's
// regression test for the enumeration gap's consumer half: a
// runtime-created self-service account's browser-shaped sign-in must
// resolve its clinic tenant from org's own memberships table ALONE --
// with every trace of the host's own clinic ledger wiped between the
// boots. Before org grew MemberService.TenantsOf, the sign-in store
// scanned its own tenant lists, and the self_service_clinics ledger row
// was what told a restarted boot's scan set where to look for a clinic
// its universe of configured tenants could not name; a boot whose ledger
// had nothing to re-discover answered the clinic owner's no-tenant
// sign-in with 403 authn.tenant_membership_required no matter how real
// the org membership row was. The wipe below reproduces exactly that
// state, so this test fails against the pre-query code (failing before:
// boot-two's sign-in answers 403, because the store's scan needs the
// ledger) and passes against org's own answer (passing after: the org
// row alone resolves the clinic, and the ledger is not even read).
//
// The sqlite3 driver import below is this test's own, mirroring the
// module's existing raw-DB probe pattern (dbkit's migration-ledger
// counts in org's own upgrade tests): the wipe must reach the shared
// database file directly because no in-process handle survives between
// the two boots.
func TestSelfServiceSignup_ClinicSignInDependsOnNoHostLedger(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reference-app-self-service-no-ledger.db")

	boot := func() (*httptest.Server, func() error) {
		cfg := testConfig(t)
		cfg.SQLitePath = dbPath
		handler, cleanup, _, err := buildServer(context.Background(), cfg)
		if err != nil {
			t.Fatalf("buildServer: %v", err)
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
	// pre-retirement boot created one (self_service.go's
	// self_service_clinics, retired by this round): on the pre-query
	// tree this delete is exactly the state that broke boot-two's
	// sign-in -- the ledger row was the boot-time re-discovery source --
	// and on the fixed tree the table no longer exists at all, so the
	// no-such-table answer is just as fine. Either way, what boot two
	// signs in with may not depend on a row of this host bookkeeping.
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
