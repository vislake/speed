// The reference app's demo glue for go/integration's round-6
// inbound-authentication surface: a minimal "whoami" route gated by
// go/integration's own AuthMiddleware, demonstrating the mechanism
// go/integration/AGENTS.md's earlier rounds documented as unbuilt --
// "authenticating an inbound request with an API key" -- now real. This is
// deliberately NOT a production feature: it exists only to prove, end to
// end through a real composed HTTP stack, that a tenant's own issued API key
// authenticates a request, that a rotated-away key is refused, and that
// LayeredLimiter/HTTPGuard now have a real Authenticate-gated surface to sit
// in front of (the "not yet a real inbound surface to demonstrate against"
// reasoning go/integration/AGENTS.md's round-5 section recorded for why
// those two stayed unwired in front of anything here).
//
// apikey_authenticate_flow_test.go drives this route through the composed
// HTTP stack: create, authenticate, rotate, authenticate with the old key
// (refused) and the new one (succeeds), revoke, authenticate again
// (refused).
package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/vislake/speed/go/integration"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/ratelimit"
)

// integrationWhoamiRateLimitWindow is the sliding window every one of
// integrationWhoamiLimits' three layers shares -- a single window kept this
// route's own local constant rather than reused from elsewhere in this app,
// since no other route in this codebase wires go/ratelimit at all yet.
const integrationWhoamiRateLimitWindow = time.Minute

// integrationWhoamiPath is this app's one demo route gated by
// integration.AuthMiddleware. GET it with an X-API-Key header naming a raw
// key this tenant issued through the ordinary integration_createAPIKey
// operation (apikey_flow_test.go's own surface), and the response names the
// tenant and key Authenticate resolved -- never anything the request itself
// claimed.
const integrationWhoamiPath = "/api/v1/demo/integration/whoami"

// integrationWhoamiLimits is this demo route's own LayeredLimits: generous
// enough that apikey_authenticate_flow_test.go's handful of requests never
// trips them, but present and real (not LayeredLimits{}'s all-disabled zero
// value) so the composed chain genuinely exercises LayeredLimiter/HTTPGuard
// against this Authenticate-gated surface -- the concrete realization
// go/integration/AGENTS.md's round-5 section deferred ("these rate-limit
// requests authenticated BY an API key against a host's own inbound API
// surface, a surface this module still does not build"). This round builds
// exactly that surface and wires the two together over it.
var integrationWhoamiLimits = integration.LayeredLimits{
	Global: ratelimit.Limit{Rate: 1000, Per: integrationWhoamiRateLimitWindow},
	Tenant: ratelimit.Limit{Rate: 200, Per: integrationWhoamiRateLimitWindow},
	Key:    ratelimit.Limit{Rate: 60, Per: integrationWhoamiRateLimitWindow},
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
// the composed integration.AuthMiddleware -> integration.HTTPGuard.Middleware
// chain over m -- AuthMiddleware first, so HTTPGuard's own Extractor can
// read the tenant and key Authenticate just resolved out of request
// context, rather than re-deriving either itself (see AuthMiddleware's own
// doc comment, "Deliberately separate from HTTPGuard"). kv backs the
// LayeredLimiter's three counters -- this app's own resolved KVStore seam
// (reg.KVStore()), the identical store every other rate-limited surface in
// this codebase would use.
//
// This route is deliberately NOT gated by rbac: unlike wireIntegrationWebhooks'
// or the spec-generated API-key CRUD surface's own session-authenticated,
// permission-checked management actions, this route IS the authentication
// layer for a DIFFERENT kind of caller entirely -- a script or third-party
// system holding nothing but the API key itself, never an authn session --
// so it carries no rbac.Authorizer dependency and needs none: Authenticate's
// own refusal is the whole of this route's access control.
func wireIntegrationAuthenticated(mux *http.ServeMux, m *integration.Module, kv pkgcore.KVStore) {
	limiter := integration.NewLayeredLimiter(ratelimit.New(kv), integrationWhoamiLimits)
	guard := integration.NewHTTPGuard(limiter, "integration-demo-whoami", func(r *http.Request) (tenantKey, apiKeyID string) {
		// Reads what AuthMiddleware (mounted below, ahead of this Extractor
		// in the chain) already attached -- this module "takes no position
		// on how a host authenticates a request" (HTTPGuard's own doc
		// comment), and this is the host's own answer: the tenant and key
		// Authenticate resolved, never re-derived.
		authenticated, ok := integration.AuthenticatedAPIKeyFromContext(r.Context())
		if !ok {
			return "", ""
		}
		return authenticated.TenantID, authenticated.KeyID
	})

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
		integration.NewAuthMiddleware(m).Middleware(guard.Middleware(whoami)))
}
