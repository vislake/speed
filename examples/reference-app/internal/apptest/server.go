// Package apptest carries the reference app's boot-level test fixtures:
// the per-test server configuration, the composed-server constructor
// (BuildServer's real output behind an httptest.Server) and the
// register-and-sign-in helper the HTTP-driven suites build their journeys
// on. They live in their own package rather than in internal/testutil
// because they depend on internal/app, whose own white-box tests import
// internal/testutil -- keeping the app-dependent fixtures separate is what
// keeps that direction acyclic. It is test-only: nothing in this app's
// executable code may import it.
package apptest

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/app"
	"github.com/vislake/speed/examples/reference-app/internal/app/demo"
	"github.com/vislake/speed/go/compliance"
	"github.com/vislake/speed/go/pkgcore"
)

// ServerConfig returns a ServerConfig backed by a fresh, per-test temp-file
// SQLite database, so tests never share state and never touch a real file
// outside t.TempDir(). Memberships is always a fresh, empty
// signInMemberships -- tests that need an account to actually reach a
// tenant grant it explicitly via RegisterAndAuthenticate below, keeping
// the same reference BuildServer itself wires (and attaches to org) so a
// test's grant is visible to the running server.
//
// The six platform key materials need no field here: every boot resolves them
// from the declaring components' declarations on the loader chain, with
// app.BootstrapDevDefaults as the documented development defaults -- the
// engine's platform cipher, the org, notification and authn blind indexers,
// and the authn PII and pki local-key ciphers are all built from that
// material, and a missing one fails the boot before the first request. The
// environment resolution (each key's own variable, or APP_ROOT_KEY's
// derivation) runs on the same chain, on ConfigFromEnv's path and the
// assembly's alike.
func ServerConfig(t *testing.T) app.ServerConfig {
	t.Helper()
	return app.ServerConfig{
		DeploymentMode: pkgcore.DeploymentModeStandalone,
		Port:           "0",
		SQLitePath:     filepath.Join(t.TempDir(), "reference-app-test.db"),
		HostTenants:    demo.DemoHostTenants,
		Memberships:    app.NewSignInMemberships(),
	}
}

// BuildServer wires up BuildServer's real output behind an
// httptest.Server, so tests exercise the exact composed handler main.go
// itself serves -- the authn+tenancy middleware chain, the notes Module's
// real handler, and a real (if temp-file) SQLite database -- not a mock of
// any of them. It returns the ServerConfig alongside the server so a
// caller can reach cfg.Memberships to grant a demo account tenant
// membership after registering it (RegisterAndAuthenticate does this), and
// BuildServer's wired *compliance.Module -- the one reach a test has into
// the retention/erasure/export services, which flowtests' compliance_flow_test.go
// drives (every other flow test in the flowtests package is HTTP-driven and
// discards it, exactly as BuildServer's own doc comment describes main.go doing).
func BuildServer(t *testing.T) (*httptest.Server, app.ServerConfig, *compliance.Module) {
	t.Helper()

	cfg := ServerConfig(t)
	handler, cleanup, complianceModule, err := app.BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, cfg, complianceModule
}
