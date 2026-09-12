package httpserve_test

// This file pins the component's declaration and configuration contract:
// the two provided faces, the required host link policy (whose absence
// fails the Prepare stage rather than degrading to an unauthenticated
// router), the policy's chainless/guarded validation, the configuration
// schema's empty-address refusal, and the background composition -- a
// pure-background assembly with a route-mounting module and no http
// component assembles and shuts down with nothing mounted.

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/vislake/speed/go/app"
	"github.com/vislake/speed/go/app/httpserve"
	"github.com/vislake/speed/go/pkgcore"
)

// policyProvider returns a component delivering the host link policy a test
// hands it.
func policyProvider(policy *httpserve.LinkPolicy) pkgcore.Component {
	return pkgcore.Component{
		Name:     "test.link-policy",
		Provides: []any{(*httpserve.LinkPolicy)(nil)},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return policy, nil
		},
	}
}

// routesModuleComponent returns a module-shaped component that mounts
// exactly the routes a test hands it through the optional route face -- the
// shape every route-mounting module declares.
func routesModuleComponent(name string, routes ...pkgcore.MountedRoute) pkgcore.Component {
	return pkgcore.Component{
		Name: name,
		Requires: []pkgcore.Requirement{
			{Token: (*pkgcore.RouteRegistrar)(nil), Optional: true},
		},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &struct{}{}, nil
		},
		Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
			for _, route := range routes {
				if err := pkgcore.MountRoute(reg, route.Path, route.Handler); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// chainlessPolicy is the policy a composition with no authn module
// declares: no fixed chain, the protected mux served directly.
func chainlessPolicy() *httpserve.LinkPolicy {
	return &httpserve.LinkPolicy{Chainless: true}
}

// assemble drives a minimal assembly to the end of the Serve stage over the
// given component-selection block, registering the extra components first;
// the registry is returned even on failure (a failed assembly has already
// rolled itself back), while on success the caller owns the cleanup.
func assemble(t *testing.T, block pkgcore.ComponentConfig, extra ...pkgcore.Component) (*pkgcore.ComponentRegistry, error) {
	t.Helper()
	reg := pkgcore.NewComponentRegistry()
	for _, c := range extra {
		if err := reg.Register(c); err != nil {
			t.Fatalf("register %q: %v", c.Name, err)
		}
	}
	spec := app.LoadSpec{
		Host: &struct{}{},
		Overrides: &app.CompositionOverrides{
			Config: pkgcore.ComponentConfig{}.
				With("deployment", string(pkgcore.DeploymentModeStandalone)).
				With("strict", true).
				// The builtin defaults select the observability component;
				// a test's minimal composition deselects it so nothing
				// telemetry-shaped participates.
				With("components", block.With("observability", false)),
		},
	}
	return reg, app.Assemble(context.Background(), reg, spec)
}

// TestComponent_DeclaresTheTwoFaces pins the descriptor: one product
// satisfies both declaration faces, the host link policy is the one
// required dependency (not optional), and the entry-point lifecycle is
// declared on the component rather than on Init.
func TestComponent_DeclaresTheTwoFaces(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	c, ok := pkgcore.LookupComponent(reg, httpserve.ComponentName)
	if !ok {
		t.Fatal("the http component is not registered")
	}
	want := map[string]bool{
		reflect.TypeOf((*pkgcore.RouteRegistrar)(nil)).String():      false,
		reflect.TypeOf((*pkgcore.MiddlewareRegistrar)(nil)).String(): false,
	}
	for _, provided := range c.Provides {
		name := reflect.TypeOf(provided).String()
		if _, declared := want[name]; !declared {
			t.Errorf("the component provides %s, want only the two declaration faces", name)
			continue
		}
		want[name] = true
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("the component does not provide %s", name)
		}
	}
	if len(c.Requires) != 1 || reflect.TypeOf(c.Requires[0].Token).String() != reflect.TypeOf((*httpserve.LinkPolicy)(nil)).String() || c.Requires[0].Optional {
		t.Fatalf("the component Requires = %v, want one mandatory (*httpserve.LinkPolicy) requirement", c.Requires)
	}
	if c.Serve == nil || c.Stop == nil || c.Close == nil {
		t.Fatalf("the component declares Serve=%v Stop=%v Close=%v, want the entry-point lifecycle", c.Serve != nil, c.Stop != nil, c.Close != nil)
	}
	if c.Init != nil {
		t.Error("the component declares an Init callback; the faces open at construction and the assembly happens in Serve")
	}
}

// TestAssemble_RequiresTheHostLinkPolicy pins the required dependency: a
// composition selecting the http component without a policy provider fails
// the Prepare stage naming the missing token, rather than degrading to an
// unauthenticated naked router.
func TestAssemble_RequiresTheHostLinkPolicy(t *testing.T) {
	_, err := assemble(t, pkgcore.ComponentConfig{}.
		With(httpserve.ComponentName, nil).
		With("test.module", nil),
		routesModuleComponent("test.module"),
	)
	if err == nil {
		t.Fatal("Assemble() without a link-policy provider succeeded, want the Prepare-stage refusal")
	}
	if !errors.Is(err, pkgcore.ErrMissingRequirement) || !strings.Contains(err.Error(), "LinkPolicy") {
		t.Fatalf("Assemble() error = %v, want ErrMissingRequirement naming the LinkPolicy token", err)
	}
}

// TestAssemble_RefusesContradictoryPolicies pins the policy validation: a
// chainless policy carrying the guarded half, and a guarded policy with no
// verifier, both fail the Serve stage loudly instead of silently ignoring
// what the host declared.
func TestAssemble_RefusesContradictoryPolicies(t *testing.T) {
	t.Run("chainless with guarded fields", func(t *testing.T) {
		policy := &httpserve.LinkPolicy{Chainless: true, AdminPrefix: "/api/v1/admin"}
		reg, err := assemble(t, policyAssemblyBlock(), policyProvider(policy))
		if err == nil {
			_ = app.Shutdown(context.Background(), reg)
			t.Fatal("Assemble() with a chainless policy carrying guarded fields succeeded, want the contradiction refusal")
		}
		if !strings.Contains(err.Error(), "Chainless") {
			t.Fatalf("Assemble() error = %v, want it to name the Chainless contradiction", err)
		}
	})

	t.Run("guarded without a verifier", func(t *testing.T) {
		reg, err := assemble(t, policyAssemblyBlock(), policyProvider(&httpserve.LinkPolicy{}))
		if err == nil {
			_ = app.Shutdown(context.Background(), reg)
			t.Fatal("Assemble() with a guarded policy carrying no verifier succeeded, want the missing-verifier refusal")
		}
		if !strings.Contains(err.Error(), "Verifier") {
			t.Fatalf("Assemble() error = %v, want it to name the missing Verifier", err)
		}
	})
}

// policyAssemblyBlock is the composition the policy tests select: the http
// component in compose-only mode and the policy provider.
func policyAssemblyBlock() pkgcore.ComponentConfig {
	return pkgcore.ComponentConfig{}.
		With(httpserve.ComponentName, pkgcore.ComponentConfig{}.With("listen", false)).
		With("test.link-policy", nil)
}

// TestNew_RefusesAnEmptyAddress pins the component's own configuration
// validation: an explicitly emptied addr would bind every interface on a
// default port, so the construction refuses it.
func TestNew_RefusesAnEmptyAddress(t *testing.T) {
	reg, err := assemble(t, pkgcore.ComponentConfig{}.
		With(httpserve.ComponentName, pkgcore.ComponentConfig{}.With("listen", false).With("addr", "")).
		With("test.link-policy", nil),
		policyProvider(chainlessPolicy()),
	)
	if err == nil {
		_ = app.Shutdown(context.Background(), reg)
		t.Fatal("Assemble() with an empty configured addr succeeded, want the construction refusal")
	}
	if !strings.Contains(err.Error(), "addr") {
		t.Fatalf("Assemble() error = %v, want it to name the configured addr", err)
	}
}

// TestBackground_AssemblesWithoutTheHTTPComponent pins the pure-background
// composition: a module declaring the optional route face constructs and
// runs with nothing mounted and nothing listening when no http component is
// selected -- the composition stays a legitimate configuration, and the
// optional requirement neither fails the plan nor auto-pulls the component.
func TestBackground_AssemblesWithoutTheHTTPComponent(t *testing.T) {
	reg, err := assemble(t, pkgcore.ComponentConfig{}.With("test.module", nil),
		routesModuleComponent("test.module", pkgcore.MountedRoute{Path: "/api/v1/thing", Handler: http.NotFoundHandler()}),
	)
	if err != nil {
		t.Fatalf("Assemble() over a pure-background composition: %v", err)
	}
	if _, ok, getErr := pkgcore.GetOptional[pkgcore.RouteRegistrar](reg); getErr != nil || ok {
		t.Fatalf("the background composition carries a route registrar (ok=%v, err=%v); nothing serves HTTP here", ok, getErr)
	}
	if err := app.Shutdown(context.Background(), reg); err != nil {
		t.Fatalf("Shutdown() over a pure-background composition: %v", err)
	}
}
