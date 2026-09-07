package template

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/dbkit"
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite" // registers dbkit.DialectSQLite for dbkit.Open
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pki"
	"github.com/vislake/speed/go/tenancy"
)

// TestPreauthExemption_DrivesTheGeneratedProjectShape is the behavioral
// half of the P1 regression: an anonymous enterprise-OIDC authorize
// request -- provider "oidc:acme", the dynamic per-tenant name authn
// derives from authn.ProviderOIDCPrefix + a tenant id, which no fixed
// allowlist can enumerate -- must reach authn's own handler instead of
// being refused 403 tenancy.tenant_unresolved before it. The test
// composes the real authn, pki and tenancy packages in exactly the shape
// the generated server.go templates produce (the structural twin,
// TestAuthnSelectionsExemptAuthnSubtreeByStructure, pins those templates
// to this shape byte-level): authn's own subtree mounted on topMux
// directly behind authn.Middleware, tenancy.Middleware wrapping only the
// other routes. Authn answers a provider it has never seen with its own
// coded refusal -- 400 authn.provider_unknown -- which is the proof the
// request crossed the tenancy layer: the pre-fix allowlist shape answered
// this exact request 403 tenancy.tenant_unresolved, asserted by the
// legacyShape leg below.
func TestPreauthExemption_DrivesTheGeneratedProjectShape(t *testing.T) {
	handler := buildComposedHandler(t, composedShapeNew)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/authn/social/oidc:acme/authorize?redirect_uri=https%3A%2F%2Fapp.example%2Fcb", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("GET oidc:acme authorize = %d %s; authn's provider-unknown refusal is 400 (any non-403 answer that reaches authn proves the exemption; the pre-fix answer was the tenancy 403)", rec.Code, rec.Body.String())
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Code != "authn.provider_unknown" {
		t.Fatalf("authorize body = %q, want the authn.provider_unknown envelope (code %q, err %v)", rec.Body.String(), body.Code, err)
	}
}

// TestPreauthExemption_CallbackAlsoReachesAuthn drives the callback half
// of the same pair. An anonymous POST with no usable state answers authn's
// own refusal once it reaches the handler; the assertion that matters is
// the negative one -- never the tenancy 403 that the pre-fix allowlist
// shape produced for this path.
func TestPreauthExemption_CallbackAlsoReachesAuthn(t *testing.T) {
	handler := buildComposedHandler(t, composedShapeNew)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/authn/social/oidc:acme/callback", strings.NewReader(`{"code":"c","state":"s"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "tenancy.tenant_unresolved") {
		t.Fatalf("oidc callback refused by tenancy: %d %s; the callback must reach authn's own handler", rec.Code, rec.Body.String())
	}
}

// TestPreauthExemption_LegacyAllowlistShapeRefusesTheSameRequest is the
// regression anchor for what the fix removed: compose the OLD generated
// shape -- the whole mux, authn subtree included, wrapped in
// tenancy.Middleware with the fixed allowlist that enumerated the five
// built-in social channels -- and the identical anonymous oidc:acme
// authorize request is refused 403 tenancy.tenant_unresolved, because no
// fixed enumeration can contain a per-tenant provider name. If this leg
// ever starts passing, the tenancy layer no longer refuses unlisted
// anonymous pairs and the structural exemption would be moot; if the new
// shape ever regresses to this one, the first test fails.
func TestPreauthExemption_LegacyAllowlistShapeRefusesTheSameRequest(t *testing.T) {
	handler := buildComposedHandler(t, composedShapeLegacy)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/authn/social/oidc:acme/authorize", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "tenancy.tenant_unresolved") {
		t.Fatalf("legacy allowlist shape answered %d %s; want the 403 tenancy.tenant_unresolved the fixed enumeration produced for an unenumerable provider", rec.Code, rec.Body.String())
	}
}

// composedShape selects which generated-server shape the composed handler
// mirrors: the structural exemption (new) or the fixed allowlist (legacy).
type composedShape int

const (
	composedShapeNew composedShape = iota
	composedShapeLegacy
)

// buildComposedHandler assembles a REAL authn module (with pki as its
// KeySource, over a real SQLite file) and composes the HTTP chain exactly
// as the generated server.go does under the requested shape. The authn
// handler is reached the way the generated code reaches it: through
// reg.Routes after Kernel.Bootstrap registered the module.
func buildComposedHandler(t *testing.T, shape composedShape) http.Handler {
	t.Helper()
	ctx := context.Background()
	gdb, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: t.TempDir() + "/preauth.db"})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, closeErr := gdb.DB(); closeErr == nil {
			_ = sqlDB.Close()
		}
	})

	pkiModule := pki.NewModule(gdb)
	blindIndexKey := make([]byte, 32)
	authnModule, err := authn.NewModule(gdb, authn.WithKeySource(pkiModule.Service()), authn.WithBlindIndexKey(blindIndexKey))
	if err != nil {
		t.Fatalf("authn.NewModule: %v", err)
	}
	reg, err := pkgcore.NewKernel(pkgcore.WithDeploymentMode(pkgcore.DeploymentModeStandalone)).
		Bootstrap(ctx, pkiModule, authnModule)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// The generated mountModuleRoutes split and the topMux dispatch,
	// spelled as the templates spell it.
	const authnAPIPath = "/api/v1/authn"
	authnMux := http.NewServeMux()
	protectedMux := http.NewServeMux()
	for _, route := range reg.Routes.Routes() {
		target := protectedMux
		if strings.HasPrefix(route.Path, authnAPIPath) {
			target = authnMux
		}
		target.Handle(route.Path, route.Handler)
		if !strings.HasSuffix(route.Path, "/") {
			target.Handle(route.Path+"/", route.Handler)
		}
	}

	if shape == composedShapeLegacy {
		// The pre-fix shape: every route -- authn's subtree included --
		// behind tenancy.Middleware, with authn's pre-auth operations
		// allowlisted one (method, path) pair at a time, the fixed
		// built-in social channels enumerated by hand.
		allMux := http.NewServeMux()
		for _, route := range reg.Routes.Routes() {
			allMux.Handle(route.Path, route.Handler)
			if !strings.HasSuffix(route.Path, "/") {
				allMux.Handle(route.Path+"/", route.Handler)
			}
		}
		opts := []tenancy.MiddlewareOption{
			tenancy.WithAllowlist(http.MethodPost, authnAPIPath+"/register"),
			tenancy.WithAllowlist(http.MethodPost, authnAPIPath+"/login/password"),
			tenancy.WithAllowlist(http.MethodPost, authnAPIPath+"/login/sms/request"),
			tenancy.WithAllowlist(http.MethodPost, authnAPIPath+"/login/sms"),
			tenancy.WithAllowlist(http.MethodPost, authnAPIPath+"/token/refresh"),
		}
		for _, provider := range []string{
			authn.ProviderGoogle, authn.ProviderGitHub, authn.ProviderWeChat,
			authn.ProviderDingTalk, authn.ProviderFeishu,
		} {
			opts = append(opts,
				tenancy.WithAllowlist(http.MethodGet, authnAPIPath+"/social/"+provider+"/authorize"),
				tenancy.WithAllowlist(http.MethodPost, authnAPIPath+"/social/"+provider+"/callback"),
			)
		}
		return authn.Middleware(authnModule.Service().Verifier())(
			tenancy.Middleware(authn.NewPrincipalResolver(), opts...)(allMux))
	}

	topMux := http.NewServeMux()
	topMux.Handle(authnAPIPath, authnMux)
	topMux.Handle(authnAPIPath+"/", authnMux)
	topMux.Handle("/", tenancy.Middleware(authn.NewPrincipalResolver(),
		tenancy.WithAllowlist(http.MethodGet, "/healthz"),
		tenancy.WithAllowlist(http.MethodHead, "/healthz"),
	)(protectedMux))
	return authn.Middleware(authnModule.Service().Verifier())(topMux)
}

// TestPreauthExemption_ComposedShapeIsTheTemplatesOwn pins the test's
// composition constants to the template files it claims to mirror: if the
// generated server.go templates ever compose differently (a renamed
// constant, a different dispatch), the behavior test would silently be
// testing a shape the templates no longer produce -- this twin assertion
// closes that gap by requiring the templates to carry the very markers
// this test's composition is built from.
func TestPreauthExemption_ComposedShapeIsTheTemplatesOwn(t *testing.T) {
	for _, key := range []string{"authn+org+rbac", "authn+rbac", "authn+org", "authn"} {
		content, err := fs.ReadFile(Project, ProjectRoot+"/selection/"+key+"/server.go")
		if err != nil {
			t.Errorf("%s: %v", key, err)
			continue
		}
		server := string(content)
		for _, marker := range []string{
			"const authnAPIPath = \"/api/v1/authn\"",
			"topMux.Handle(authnAPIPath, authnMux)",
			"handler := authn.Middleware(authnModule.Service().Verifier())(topMux)",
		} {
			if !strings.Contains(server, marker) {
				t.Errorf("%s: the composed-shape twin marker %q is missing from the template", key, marker)
			}
		}
	}
}
