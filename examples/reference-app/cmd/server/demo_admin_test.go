package main

// demo_admin_test.go is the consumer proof of the demo platform-staff
// account's credential source (demo_admin.go's seedDemoPlatformStaff): the
// platform administrator -- the one account holding BuiltinRoleOwner under
// rbac.SystemDomain, which carries every permission any module declared,
// admin's admin:* permissions included -- must never be seeded from the
// ordinary demo-users password variable (APP_DEMO_USERS_PASSWORD,
// demo_users.go). One passphrase unlocking the platform administrator and
// every ordinary demo account at once is the exact sharing the reviewer
// finding this file pins (P1-1) names as the bug.
import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vislake/speed/go/rbac"
)

// TestDemoPlatformStaff_NotSeededWithTheDemoUsersPassword is the mandatory
// regression for the finding: a boot with ONLY the demo-users password set
// -- cfg.DemoUsersPassword filled, exactly what an operator setting
// APP_DEMO_USERS_PASSWORD alone produces -- must not leave the platform
// administrator reachable with that password. The bug it pins: the staff
// account WAS registered with the very same password, so a single
// APP_DEMO_USERS_PASSWORD value unlocked every demo user AND the platform
// administrator at once.
func TestDemoPlatformStaff_NotSeededWithTheDemoUsersPassword(t *testing.T) {
	srv, _ := buildSeededUsersTestServer(t, demoSeedPassword)

	status, code, _ := demoLogin(t, srv, demoPlatformStaffEmail, demoSeedPassword, rbac.SystemDomain)
	if status == http.StatusOK {
		t.Fatalf("the demo platform-staff account signed in with the ordinary demo users password (status 200, code %q) "+
			"-- the platform administrator must never be seeded from APP_DEMO_USERS_PASSWORD; it needs its own credential source",
			code)
	}
}

// TestDemoPlatformStaff_SeededFromItsOwnVariableOnly pins the positive half
// of the split: a boot with BOTH seed variables set -- each to a DIFFERENT
// passphrase, the honest image of an operator enabling the whole demo --
// seeds both account sets, and each signs in with its own password only.
// The demo users' password never opens the platform administrator, and the
// staff account's own passphrase is what does.
func TestDemoPlatformStaff_SeededFromItsOwnVariableOnly(t *testing.T) {
	cfg := testConfig(t)
	cfg.DemoUsersPassword = demoSeedPassword
	cfg.DemoPlatformStaffPassword = demoPlatformStaffSeedPassword
	handler, cleanup, _, err := buildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// The staff account signs in with its OWN variable's passphrase...
	status, code, _ := demoLogin(t, srv, demoPlatformStaffEmail, demoPlatformStaffSeedPassword, rbac.SystemDomain)
	if status != http.StatusOK {
		t.Fatalf("platform-staff login with its own password variable's value: status = %d, code = %q, want %d",
			status, code, http.StatusOK)
	}

	// ...and NEVER with the demo users' passphrase, even though that
	// variable is set on the same boot.
	status, code, _ = demoLogin(t, srv, demoPlatformStaffEmail, demoSeedPassword, rbac.SystemDomain)
	if status == http.StatusOK {
		t.Fatalf("the demo platform-staff account signed in with the demo users password (status 200, code %q) "+
			"-- the two credential sources must stay distinct even when both are set", code)
	}

	// The ordinary demo accounts are untouched by the split: the owner still
	// signs in with the demo users' passphrase.
	status, code, _ = demoLogin(t, srv, demoOwnerEmail, demoSeedPassword, "tenant-acme")
	if status != http.StatusOK {
		t.Fatalf("demo-owner login with the demo users password: status = %d, code = %q, want %d",
			status, code, http.StatusOK)
	}
}
