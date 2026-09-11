package app

// host_settings_wiring_test.go holds the host-level proof that the dynamic
// configuration the platform modules declare is effective through THIS
// assembled application: a row written through the config module's real Set
// path -- the surface an operator's write lands on -- changes the behavior
// the assembled authn module serves.

import (
	"context"
	"path/filepath"
	"testing"

	speedapp "github.com/vislake/speed/go/app"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"

	"github.com/vislake/speed/examples/reference-app/internal/app/demo"
)

// TestConfigRowsTakeEffectThroughTheAssembledHost drives the full host
// assembly (BuildServer's drive: every module and host component
// constructed, migrations applied, config attached) and then writes the
// authn.password_min_length row through the live config Service, the way an
// operator's write does. The assembled authn service must honor the row: a
// password under the row's minimum is refused with the module's coded
// refusal while the row stands, and the same password is accepted once the
// row is lowered. The two legs change exactly one variable -- the row's
// value -- so the refusal cannot be an artifact of the password itself or
// of the construction-time policy, which is what makes this the host-level
// proof that the module's declared dynamic config item is a live switch.
func TestConfigRowsTakeEffectThroughTheAssembledHost(t *testing.T) {
	ctx := context.Background()
	// Above the schema's default minimum (12), below the raised row (64).
	const pinPassword = "host-settings-pin"

	var configService *config.Service
	b := newServerBuild(ServerConfig{
		DeploymentMode: pkgcore.DeploymentModeStandalone,
		Port:           "0",
		SQLitePath:     filepath.Join(t.TempDir(), "host-settings.db"),
		HostTenants:    demo.DemoHostTenants,
		Memberships:    NewSignInMemberships(),
		OnConfigReady:  func(svc *config.Service) { configService = svc },
	})
	reg, err := b.assemble(ctx, false)
	if err != nil {
		t.Fatalf("assemble(): %v", err)
	}
	t.Cleanup(func() {
		if shutdownErr := speedapp.Shutdown(context.Background(), reg); shutdownErr != nil {
			t.Errorf("Shutdown(): %v", shutdownErr)
		}
	})
	if configService == nil {
		t.Fatal("the host's post-bootstrap step never published the config service")
	}
	authnModule, err := pkgcore.Get[*authn.Module](reg)
	if err != nil {
		t.Fatalf("read the assembled authn module: %v", err)
	}
	authnService := authnModule.Service()

	sysCtx, err := pkgcore.WithSystemContext(ctx, pkgcore.SystemReason{
		Actor:   "host-settings-wiring-test",
		Purpose: config.SystemPurposeSystemWrite,
	})
	if err != nil {
		t.Fatalf("build the system context for the config write: %v", err)
	}

	// The raised minimum refuses the password: the assembled authn resolves
	// the row through its settings seam, so the module's declared dynamic
	// config item governs the policy the Register call validates against.
	if setErr := configService.Set(sysCtx, config.ScopeSystem, authn.ConfigKeyPasswordMinLength,
		config.Value{Data: int64(64)}, "host-settings-wiring-test"); setErr != nil {
		t.Fatalf("set %s: %v", authn.ConfigKeyPasswordMinLength, setErr)
	}
	_, err = authnService.Register(ctx, authn.RegisterInput{
		Email: "row-raised@example.test", Password: pinPassword, DisplayName: "Row Raised",
	})
	if !apperr.HasCode(err, authn.ErrPasswordTooShort.Code) {
		t.Fatalf("Register with %s = 64 returned %v; want the %s refusal the configured row demands",
			authn.ConfigKeyPasswordMinLength, err, authn.ErrPasswordTooShort.Code)
	}

	// Lowering the row accepts the same password: the refusal above was the
	// row's doing, not the password's.
	if setErr := configService.Set(sysCtx, config.ScopeSystem, authn.ConfigKeyPasswordMinLength,
		config.Value{Data: int64(8)}, "host-settings-wiring-test"); setErr != nil {
		t.Fatalf("set %s: %v", authn.ConfigKeyPasswordMinLength, setErr)
	}
	if _, err := authnService.Register(ctx, authn.RegisterInput{
		Email: "row-lowered@example.test", Password: pinPassword, DisplayName: "Row Lowered",
	}); err != nil {
		t.Fatalf("Register after lowering %s returned %v; want success", authn.ConfigKeyPasswordMinLength, err)
	}
}
