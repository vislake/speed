package chain

import (
	"context"
	"crypto"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vislake/speed/go/app"
	"github.com/vislake/speed/go/authn"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"
)

// noKeysKeySource is an authn.KeySource with no verification keys: every
// token fails verification, which is exactly what the chain's fail-closed
// cases need (a genuinely invalid bearer) without any signing machinery.
type noKeysKeySource struct{}

func (noKeysKeySource) EnsurePurpose(context.Context, string, string, time.Duration) error {
	return nil
}

func (noKeysKeySource) ActiveSigner(context.Context, string) (string, string, func(context.Context, []byte) ([]byte, error), error) {
	return "", "", nil, errors.New("noKeysKeySource: no signer")
}

func (noKeysKeySource) VerificationKeys(context.Context, string) ([]struct {
	KID       string
	Algorithm string
	Public    crypto.PublicKey
}, error,
) {
	return nil, nil
}

func newTestVerifier(t *testing.T) *authn.Verifier {
	t.Helper()
	v, err := authn.NewVerifier(noKeysKeySource{})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

// activeStatusResolver is a wired-but-inert tenancy.TenantStatusResolver:
// the option takes effect, and with no resolvable tenant the status is
// never consulted.
type activeStatusResolver struct{ calls int }

func (r *activeStatusResolver) Status(context.Context, pkgcore.TenantID) (tenancy.TenantStatus, error) {
	r.calls++
	return tenancy.TenantStatusActive, nil
}

// newTestChain builds a chain over marker handlers and returns the
// composed handler plus the hit counters the tests assert against.
func newTestChain(t *testing.T, mutate func(*Config)) (http.Handler, map[string]*int) {
	t.Helper()
	hits := map[string]*int{
		"protected": new(int),
		"authn":     new(int),
		"admin":     new(int),
	}
	protected := http.NewServeMux()
	protected.HandleFunc("/", func(http.ResponseWriter, *http.Request) { *hits["protected"]++ })
	protected.HandleFunc(obs.HealthzPath, func(http.ResponseWriter, *http.Request) { *hits["protected"]++ })

	cfg := Config{
		Verifier:  newTestVerifier(t),
		Protected: protected,
		AuthnRoutes: []pkgcore.MountedRoute{{
			Path:    app.AuthnAPIPath,
			Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { *hits["authn"]++ }),
		}},
		AdminRoutes: []pkgcore.MountedRoute{{
			Path:    "/api/v1/admin",
			Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { *hits["admin"]++ }),
		}},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	handler, err := Chain(cfg)
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	return handler, hits
}

func do(handler http.Handler, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestChain_RejectsMissingRequiredPieces(t *testing.T) {
	base := func() Config {
		return Config{
			Verifier:    newTestVerifier(t),
			Protected:   http.NewServeMux(),
			AuthnRoutes: []pkgcore.MountedRoute{{Path: app.AuthnAPIPath, Handler: http.NewServeMux()}},
		}
	}

	t.Run("nil verifier", func(t *testing.T) {
		cfg := base()
		cfg.Verifier = nil
		if _, err := Chain(cfg); err == nil {
			t.Fatal("Chain accepted a nil Verifier")
		}
	})
	t.Run("nil protected", func(t *testing.T) {
		cfg := base()
		cfg.Protected = nil
		if _, err := Chain(cfg); err == nil {
			t.Fatal("Chain accepted a nil Protected")
		}
	})
	t.Run("empty authn routes", func(t *testing.T) {
		cfg := base()
		cfg.AuthnRoutes = nil
		if _, err := Chain(cfg); err == nil {
			t.Fatal("Chain accepted an empty AuthnRoutes; the authn subtree must not fall through the tenancy chain")
		}
	})
}

// TestChain_AuthnAndAdminBranchesBypassTheTenancyChain pins the branch
// structure: with no Principal and therefore an unresolvable tenant, the
// authn subtree and the admin route still reach their handlers, while an
// ordinary protected route is refused.
func TestChain_AuthnAndAdminBranchesBypassTheTenancyChain(t *testing.T) {
	handler, hits := newTestChain(t, nil)

	if rec := do(handler, http.MethodGet, app.AuthnAPIPath+"/register", nil); *hits["authn"] != 1 {
		t.Fatalf("GET %s/register: status %d, authn handler hits %d; the authn subtree must be dispatched ahead of the tenancy chain", app.AuthnAPIPath, rec.Code, *hits["authn"])
	}
	if rec := do(handler, http.MethodGet, "/api/v1/admin/tenants", nil); *hits["admin"] != 1 {
		t.Fatalf("GET /api/v1/admin/tenants: status %d, admin handler hits %d; admin's own route must not sit behind tenancy resolution", rec.Code, *hits["admin"])
	}
	if rec := do(handler, http.MethodGet, "/api/v1/notes", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("GET /api/v1/notes: status %d, want %d (unresolved tenant, fail closed)", rec.Code, http.StatusForbidden)
	}
	if *hits["protected"] != 0 {
		t.Fatalf("the protected handler ran for a non-allowlisted request with no Principal")
	}
}

// TestChain_AuthnRunsFirst pins the outermost layer: an invalid bearer is
// 401ed even on a pre-auth allowlisted path, which is only possible when
// authn.Middleware wraps the whole composition (a tenancy-first chain
// would have let the allowlisted request through).
func TestChain_AuthnRunsFirst(t *testing.T) {
	handler, hits := newTestChain(t, nil)

	rec := do(handler, http.MethodGet, obs.HealthzPath, map[string]string{"Authorization": "Bearer not-a-token"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET %s with an invalid bearer: status %d, want %d (authn.Middleware is outermost)", obs.HealthzPath, rec.Code, http.StatusUnauthorized)
	}
	if *hits["protected"] != 0 {
		t.Fatalf("the protected handler ran for a request with an invalid bearer")
	}
}

// TestChain_PreAuthAllowlistIsGETAndHEAD closes the (method, path) pair
// from both sides: the allowlisted paths pass the tenancy chain under GET
// and HEAD, and the same path under any other method is refused.
func TestChain_PreAuthAllowlistIsGETAndHEAD(t *testing.T) {
	handler, _ := newTestChain(t, nil)

	for _, path := range []string{obs.HealthzPath, obs.MetricsPath, "/api/v1/config/public", "/api/v1/config/features"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			rec := do(handler, method, path, nil)
			if rec.Code == http.StatusForbidden {
				t.Errorf("%s %s: 403; the path is on the pre-auth allowlist under both GET and HEAD", method, path)
			}
		}
		if rec := do(handler, http.MethodPost, path, nil); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s: status %d, want %d; the allowlist must not widen to methods the route does not serve", path, rec.Code, http.StatusForbidden)
		}
	}
}

// TestChain_ExtraAllowlistScopesByMethod pins the host-supplied half: an
// entry added for one method exempts exactly that pair, nothing more.
func TestChain_ExtraAllowlistScopesByMethod(t *testing.T) {
	handler, _ := newTestChain(t, func(cfg *Config) {
		cfg.ExtraAllowlist = []tenancy.MiddlewareOption{
			tenancy.WithAllowlist(http.MethodGet, "/api/v1/public/share"),
		}
	})

	if rec := do(handler, http.MethodGet, "/api/v1/public/share", nil); rec.Code == http.StatusForbidden {
		t.Errorf("GET /api/v1/public/share: 403; the host added it to the allowlist")
	}
	if rec := do(handler, http.MethodPost, "/api/v1/public/share", nil); rec.Code != http.StatusForbidden {
		t.Errorf("POST /api/v1/public/share: status %d, want %d; the entry names GET only", rec.Code, http.StatusForbidden)
	}
}

// TestChain_ImpersonationDecoratorSitsBetweenAuthnAndTenancy pins the
// optional decorator's position: it wraps the tenancy chain (every
// request on the protected half passes through it) and is dispatched
// AFTER the two structurally exempt branches (no authn or admin request
// ever reaches it).
func TestChain_ImpersonationDecoratorSitsBetweenAuthnAndTenancy(t *testing.T) {
	calls := 0
	marker := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			next.ServeHTTP(w, r)
		})
	}
	handler, _ := newTestChain(t, func(cfg *Config) { cfg.Impersonation = marker })

	if rec := do(handler, http.MethodGet, obs.HealthzPath, nil); rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d, want %d", obs.HealthzPath, rec.Code, http.StatusOK)
	}
	if calls != 1 {
		t.Fatalf("the impersonation decorator ran %d times for one protected-half request, want 1", calls)
	}

	for _, path := range []string{app.AuthnAPIPath + "/register", "/api/v1/admin/tenants"} {
		do(handler, http.MethodGet, path, nil)
	}
	if calls != 1 {
		t.Fatalf("the impersonation decorator ran %d times after requests to the exempt branches; it must sit behind them", calls)
	}
}

// TestChain_TenantStatusResolverIsOptionalAndWired pins the option's
// wiring: a host can pass a status resolver without disturbing the
// fail-closed default.
func TestChain_TenantStatusResolverIsOptionalAndWired(t *testing.T) {
	status := &activeStatusResolver{}
	handler, _ := newTestChain(t, func(cfg *Config) {
		cfg.TenantStatusResolver = status
	})

	if rec := do(handler, http.MethodGet, obs.HealthzPath, nil); rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d, want %d", obs.HealthzPath, rec.Code, http.StatusOK)
	}
	if rec := do(handler, http.MethodGet, "/api/v1/notes", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("GET /api/v1/notes: status %d, want %d", rec.Code, http.StatusForbidden)
	}
	if status.calls != 0 {
		t.Fatalf("the status resolver was consulted %d times with no resolvable tenant", status.calls)
	}
}
