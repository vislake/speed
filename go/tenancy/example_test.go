package tenancy_test

// Runnable documentation for tenancy's public API, mirroring
// go/pkgcore/example_test.go's and go/dbkit/example_test.go's convention:
// every example here is compiled and executed by `go test`, so a change to
// tenancy's public API that breaks the documented usage fails the build
// instead of only rotting in prose.
//
// Example demonstrates exactly the pattern AGENTS.md's "Typical
// integration" section walks through: a DomainResolver wired into
// Middleware to protect a plain http.Handler -- tenancy's most central,
// most likely-to-be-copied capability. ExampleWithAllowlist and
// ExampleWithSystemContext cover the module's other two central entry
// points; tenancytest is exercised by its own tests instead of an Example
// here, the same way dbkit's dbtest helper package has no example_test.go
// of its own.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/tenancy"
)

// exampleTenantsByHost stands in for the persistent lookup (a platform
// table of custom domains and subdomains) a real DomainResolver would
// query. Deciding whether a Host is a tenant's own custom domain or a
// subdomain of the platform's domain -- and stripping any subdomain label
// -- is entirely this function's business, never DomainResolver's; see
// resolver.go's own doc comment.
func exampleTenantsByHost(host string) (pkgcore.TenantID, bool) {
	if host == "acme.example.com" {
		return pkgcore.TenantID("acme"), true
	}
	return "", false
}

// Example demonstrates a DomainResolver wired into Middleware to protect a
// plain http.Handler, for the unauthenticated routes -- a login page,
// public branding -- tenancy itself resolves. An authenticated route's
// Resolver, reading the tenant out of an already-verified access token, is
// authn's responsibility once that module exists; see resolver.go's own
// doc comment for why tenancy does not, and cannot, implement one itself.
func Example() {
	resolver := tenancy.NewDomainResolver(exampleTenantsByHost, "public")

	// A protected handler reads the tenant Middleware already resolved and
	// injected into the request context -- it never reads a header, query
	// parameter or request body for it, because Middleware's Resolver is
	// the only tenant source downstream code may trust.
	protected := tenancy.Middleware(resolver)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant, ok := pkgcore.TenantFromContext(r.Context())
		fmt.Printf("handler saw tenant=%q ok=%t\n", tenant, ok)
	}))

	// A recognized custom domain resolves to its own tenant.
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.Host = "acme.example.com"
	protected.ServeHTTP(httptest.NewRecorder(), req)

	// An unrecognized Host falls back to the default tenant configured
	// with NewDomainResolver -- enough to render a login page -- rather
	// than failing the request outright.
	req = httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.Host = "unknown.example.com"
	protected.ServeHTTP(httptest.NewRecorder(), req)

	// A client-supplied tenant hint is silently ignored: this request
	// still resolves through the Host, exactly as the first request did,
	// despite the forged header claiming a different tenant.
	req = httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.Host = "acme.example.com"
	req.Header.Set("X-Tenant-ID", "someone-elses-tenant")
	protected.ServeHTTP(httptest.NewRecorder(), req)

	// Output:
	// handler saw tenant="acme" ok=true
	// handler saw tenant="public" ok=true
	// handler saw tenant="acme" ok=true
}

// exampleTokenResolver stands in for the Resolver authn will eventually
// supply for authenticated requests (see resolver.go's own doc comment for
// why tenancy does not implement one itself): it "verifies" a bearer token
// by table lookup and fails whenever none is present or recognized --
// exactly the kind of Resolver failure WithAllowlist exists to carve a
// narrow, explicit exception around.
type exampleTokenResolver map[string]pkgcore.TenantID

// Resolve implements tenancy.Resolver.
func (r exampleTokenResolver) Resolve(req *http.Request) (pkgcore.TenantID, error) {
	token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	if tenant, ok := r[token]; ok {
		return tenant, nil
	}
	return "", errors.New("example: invalid or missing bearer token")
}

// ExampleWithAllowlist demonstrates Middleware's fail-closed default and
// the one, narrowly-scoped exception WithAllowlist carves out of it: a
// route that must work before any tenant can be known, such as a health
// check, allowlisted for exactly the method it is actually served on.
func ExampleWithAllowlist() {
	resolver := exampleTokenResolver{"good-token": "acme"}

	protected := tenancy.Middleware(resolver, tenancy.WithAllowlist(http.MethodGet, "/healthz"))(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenant, ok := pkgcore.TenantFromContext(r.Context())
			fmt.Printf("handler saw tenant=%q ok=%t\n", tenant, ok)
		}),
	)

	// A valid token resolves normally.
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.Header.Set("Authorization", "Bearer good-token")
	protected.ServeHTTP(httptest.NewRecorder(), req)

	// An invalid token on a non-allowlisted route never reaches the
	// handler: Middleware fails closed with 403, not a zero-value tenant.
	rec := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	protected.ServeHTTP(rec, req)
	fmt.Println("status:", rec.Code, "body:", strings.TrimSpace(rec.Body.String()))

	// The exact same failing resolution on the allowlisted (GET, /healthz)
	// pair proceeds anyway -- with no tenant in context, never a
	// substituted zero-value tenant.
	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	protected.ServeHTTP(httptest.NewRecorder(), req)

	// POST was never allowlisted for /healthz -- only GET was -- so the
	// identical path still fails closed under a different method.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/healthz", nil)
	protected.ServeHTTP(rec, req)
	fmt.Println("status:", rec.Code)

	// Output:
	// handler saw tenant="acme" ok=true
	// status: 403 body: {"code":"tenancy.tenant_unresolved"}
	// handler saw tenant="" ok=false
	// status: 403
}

// ExampleWithSystemContext demonstrates the audited wrapper business code
// should call instead of pkgcore.WithSystemContext directly -- see
// system_context.go's own doc comment for which code sits below tenancy in
// the module graph and therefore has no choice but to use the raw
// primitive instead. Every grant publishes a SystemContextEnteredEvent
// before the call returns, which is what lets a future audit-log consumer
// build a complete record purely by subscribing to
// tenancy.EventSystemContextEntered.
func ExampleWithSystemContext() {
	const purpose = pkgcore.SystemPurpose("example.support_lookup")
	pkgcore.RegisterSystemPurpose(purpose)

	bus := pkgcore.NewMemoryEventBus()
	bus.Subscribe(tenancy.EventSystemContextEntered, func(ctx context.Context, evt pkgcore.Event) error {
		entered, _ := evt.Payload.(tenancy.SystemContextEnteredEvent)
		fmt.Printf("audit: actor=%s purpose=%s ticket=%s\n", entered.Actor, entered.Purpose, entered.Ticket)
		return nil
	})

	ctx, err := tenancy.WithSystemContext(context.Background(), bus, pkgcore.SystemReason{
		Actor:   "support@example.com",
		Purpose: purpose,
		Ticket:  "SUP-1234",
	})
	if err != nil {
		fmt.Println("system context:", err)
		return
	}

	reason, ok := pkgcore.SystemReasonFromContext(ctx)
	fmt.Println(ok, reason.Actor, reason.Purpose)

	// A publish failure fails the whole call closed: the returned context
	// is never the elevated one, exactly like any other WithSystemContext
	// rejection -- an escape hatch granted with no audit record is the gap
	// this wrapper exists to close, not a corner case it tolerates.
	failingBus := pkgcore.NewMemoryEventBus()
	failingBus.Subscribe(tenancy.EventSystemContextEntered, func(ctx context.Context, evt pkgcore.Event) error {
		return errors.New("audit sink unavailable")
	})
	elevated, err := tenancy.WithSystemContext(context.Background(), failingBus, pkgcore.SystemReason{
		Actor:   "support@example.com",
		Purpose: purpose,
	})
	appErr, _ := apperr.As(err)
	_, stillElevated := pkgcore.SystemReasonFromContext(elevated)
	fmt.Println(appErr.Code, stillElevated)

	// Output:
	// audit: actor=support@example.com purpose=example.support_lookup ticket=SUP-1234
	// true support@example.com example.support_lookup
	// tenancy.system_context_audit_publish_failed false
}
