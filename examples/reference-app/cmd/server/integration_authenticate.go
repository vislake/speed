// The reference app's demo glue for go/integration's
// inbound-authentication surface: a minimal "whoami" route gated by
// go/integration's own AuthMiddleware, demonstrating that a tenant's own
// issued API key authenticates an inbound request. This is
// deliberately NOT a production feature: it exists only to prove, end to
// end through a real composed HTTP stack, that a tenant's own issued API key
// authenticates a request, that a rotated-away key is refused, and that
// LayeredLimiter/HTTPGuard have a real Authenticate-gated surface to sit
// in front of.
//
// The guard is wired through the module's WithAuthenticationGuard option,
// so AuthMiddleware runs it BEFORE authenticating anything -- a
// forged-X-API-Key request pays the route's global attempt budget instead
// of reaching Service.Authenticate's database lookups with no bound at all
// (the "rate limit BEFORE authentication" layering argument lives in
// go/integration/middleware.go's own doc comment).
//
// apikey_authenticate_flow_test.go drives this route through the composed
// HTTP stack: create, authenticate, rotate, authenticate with the old key
// (refused) and the new one (succeeds), revoke, authenticate again
// (refused), and the forged-flood proof above.
package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/vislake/speed/go/integration"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/ratelimit"
)

// integrationWhoamiRateLimitWindow is the sliding window the demo route's
// authentication-attempt budget uses -- a single window kept this route's
// own local constant rather than reused from elsewhere in this app, since
// no other route in this codebase wires go/ratelimit.
const integrationWhoamiRateLimitWindow = time.Minute

// integrationWhoamiPath is this app's one demo route gated by
// integration.AuthMiddleware. GET it with an X-API-Key header naming a raw
// key this tenant issued through the ordinary integration_createAPIKey
// operation (apikey_flow_test.go's own surface), and the response names the
// tenant and key Authenticate resolved -- never anything the request itself
// claimed.
const integrationWhoamiPath = "/api/v1/demo/integration/whoami"

// integrationWhoamiLimits is this demo route's own pre-auth attempt budget:
// a real, non-zero global bound over every request the guard sees -- forged
// X-API-Key floods included -- generous enough that
// apikey_authenticate_flow_test.go's handful of requests never trips it.
// Only the global layer is set, and deliberately: the guard evaluates
// BEFORE any Authenticate call (it is wired through
// integration.WithAuthenticationGuard in wireIntegrationAuthenticated
// below), when no tenant or key has been resolved for the request yet, so
// per-tenant and per-key tiers would have nothing genuine to key on --
// this is the Global-only shape go/integration's own
// ExampleWithAuthenticationGuard demonstrates, and the module's
// WithAuthenticationGuard doc comment explains why empty pre-auth
// identifiers charging the global layer is "the usual answer for anonymous
// floods". A var rather than a const so a test can bound the budget:
// apikey_authenticate_flow_test.go's forged-flood regression overrides it
// to two hits per window for the duration of that test.
var integrationWhoamiLimits = integration.LayeredLimits{
	Global: ratelimit.Limit{Rate: 1000, Per: integrationWhoamiRateLimitWindow},
}

// integrationWhoamiResponse is the demo route's own response shape: the
// tenant and key Authenticate resolved from the presented credential alone,
// plus the frozen-at-issuance scopes and the responsible creator of record
// -- never anything echoing the raw key itself.
type integrationWhoamiResponse struct {
	TenantID  string   `json:"tenant_id"`
	KeyID     string   `json:"key_id"`
	CreatedBy string   `json:"created_by"`
	Scopes    []string `json:"scopes"`
}

// wireIntegrationAuthenticated mounts integrationWhoamiPath on mux, gated by
// integration.AuthMiddleware over a Module carrying the module's own
// pre-auth rate-limit guard -- integration.WithAuthenticationGuard, the same
// wiring go/integration's ExampleWithAuthenticationGuard demonstrates.
// AuthMiddleware then runs the guard BEFORE it authenticates anything (see
// AuthMiddleware's own "Deliberately separate from HTTPGuard" doc section
// for why the guard belongs outside authentication): a request presenting a
// forged or unrecognized X-API-Key header -- which would otherwise reach
// Service.Authenticate's lookups with no bound at all, refused after paying
// for two database lookups and never charged against any budget -- pays
// integrationWhoamiLimits' global attempt budget instead, and once that
// budget is spent answers 429 with authentication never running.
//
// The option is applied here, not at integration.NewModule: the guard's
// LayeredLimiter must be built over the kernel-resolved KVStore seam (kv
// below -- reg.KVStore(), the identical store every other rate-limited
// surface in this codebase would use), which exists only after Bootstrap has
// run. That is sound because the option is a plain Module-field setter whose
// field AuthMiddleware reads at call time, and no request can reach this
// route before buildServer returns -- applying it here is equivalent to
// NewModule-time wiring.
//
// This route is deliberately NOT gated by rbac: unlike the module's
// spec-generated CRUD surfaces' own session-authenticated,
// permission-checked management actions -- the API-key fragment and the
// webhook-subscription fragment, both mounted through the same generic
// route gate (webhooks.go holds only the EventMapping machinery) -- this
// route IS the authentication layer for a
// DIFFERENT kind of caller entirely -- a script or third-party system
// holding nothing but the API key itself, never an authn session -- so it
// carries no rbac.Authorizer dependency and needs none: Authenticate's own
// refusal is the whole of this route's access control.
func wireIntegrationAuthenticated(mux *http.ServeMux, m *integration.Module, kv pkgcore.KVStore) {
	limiter := integration.NewLayeredLimiter(ratelimit.New(kv), integrationWhoamiLimits)
	guard := integration.NewHTTPGuard(limiter, "integration-demo-whoami", func(*http.Request) (tenantKey, apiKeyID string) {
		// The guard evaluates BEFORE any Authenticate call (it is wired
		// through WithAuthenticationGuard below), so no tenant or key has
		// been resolved for this request yet and nothing exists in request
		// context to derive identifiers from. Empty identifiers are ordinary
		// keys to LayeredLimiter.Allow, so every attempt charges the global
		// layer -- this route's budget is a global authentication-attempt
		// bound, and the empty-answer shape is exactly what
		// ExampleWithAuthenticationGuard and WithAuthenticationGuard's own
		// doc comment describe for pre-auth guards.
		return "", ""
	})

	// Wire the guard onto m so AuthMiddleware wraps the authenticate gate
	// with it -- the request pays the budget before any Authenticate call,
	// and a denial answers 429 with authentication never running.
	integration.WithAuthenticationGuard(guard)(m)

	whoami := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authenticated, ok := integration.AuthenticatedAPIKeyFromContext(r.Context())
		if !ok {
			// Unreachable in practice -- AuthMiddleware never calls next
			// without one -- but answered rather than panicking on a nil
			// dereference if that invariant is ever broken.
			writeIntegrationError(w, integrationErrInternal)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(integrationWhoamiResponse{
			TenantID:  authenticated.TenantID,
			KeyID:     authenticated.KeyID,
			CreatedBy: authenticated.CreatedBy,
			Scopes:    authenticated.Scopes,
		})
	})

	mux.Handle(http.MethodGet+" "+integrationWhoamiPath,
		integration.NewAuthMiddleware(m).Middleware(whoami))
}
