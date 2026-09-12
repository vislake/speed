package httpserve

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/vislake/speed/go/app"
	"github.com/vislake/speed/go/app/chain"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"
	"github.com/vislake/speed/go/tenancy"
)

// component.go carries the descriptor and its configuration schema, plus
// the LinkPolicy type: the host-supplied half of the face, a required
// dependency of this component. The product the descriptor constructs is
// Face (face.go); the Serve-stage assembly and the two-beat shutdown live in
// serve.go.

// ComponentName is the name this component selects under in a composition
// configuration.
const ComponentName = "http"

// The component self-registers at package initialization, exactly as any
// component package does. A binary that imports this package carries the
// component; a composition that does not select it never assembles it, and
// a composition that does select it must also carry a host component
// providing the link policy -- the requirement is not optional, so an
// assembly without one fails the Prepare stage naming the missing token
// instead of degrading to an unauthenticated naked router.
func init() {
	pkgcore.MustRegister(httpComponent())
}

// httpConfig is the component's configuration schema: the listen address and
// the two serve timeouts the kernel owns. All three are plain fields of the
// component's own block (components.http.*): the values a host passes travel
// through the composition configuration's block channel, and nothing here
// claims a bare flag or environment spelling.
type httpConfig struct {
	// Addr is the listen address (":8080", "127.0.0.1:0"). Default ":8080".
	Addr string `json:"addr"`
	// ReadHeaderTimeout bounds how long the server waits for a request's
	// headers. Default go/app's ReadHeaderTimeout (5s).
	ReadHeaderTimeout time.Duration `json:"read_header_timeout"`
	// ShutdownTimeout bounds the graceful drain the Stop beat begins.
	// Default go/app's ShutdownTimeout (10s).
	ShutdownTimeout time.Duration `json:"shutdown_timeout"`
	// Listen declares whether the component opens its listener in the Serve
	// stage. Default true; a drive that hands the composed handler to a
	// caller instead (an in-process server a test or another process
	// fronts) declares false and the Serve stage composes the handler
	// without binding a socket.
	Listen bool `json:"listen"`
}

// defaults returns the schema's declaration defaults: the values in force
// when the component's block (or the file configuration fragment behind it)
// omits a field. Decode only assigns present keys, so a pre-filled struct
// carries them.
func (c httpConfig) defaults() httpConfig {
	c.Addr = ":8080"
	c.ReadHeaderTimeout = app.ReadHeaderTimeout
	c.ShutdownTimeout = app.ShutdownTimeout
	c.Listen = true
	return c
}

// LinkPolicy is the host's link policy: how the process's HTTP face is
// linked to the request. It is the http component's required dependency --
// the host provides a component whose product is a *LinkPolicy -- and the
// component consumes it in the Serve stage when it assembles the handler.
//
// The policy is host policy by construction: the token verifier, the
// authorization table, the admin prefix, the impersonation decorator, the
// tenant-status gate, the extra pre-auth allowlist entries, the host's own
// routes and the host's own outer wrapper are decisions the platform must
// not make on the host's behalf. There is no default and no degraded form:
// a composition that selects this component without a policy provider fails
// its Prepare stage.
//
// # Constructing the policy
//
// A policy provider's New returns the *LinkPolicy (that product is what
// satisfies the required token), and the provider completes the value
// during its own Init turn when the policy reads services that are only
// published then (a runtime authorization service, a permission-scoped
// route table). The component reads the policy in the Serve stage, after
// every Init callback has run, so a filled-during-Init value is complete
// where it matters.
//
// # Chainless compositions
//
// A composition with no authn module -- the project skeleton's "none"
// selection -- declares Chainless: the protected mux is served directly,
// with only the platform middleware (and the outer wrapper) around it. The
// field is a positive declaration, never a default: a chainless policy that
// also carries guarded fields, and a guarded policy without a verifier, are
// both refused when the component assembles.
type LinkPolicy struct {
	// Chainless declares that this composition runs no fixed chain: no
	// verifier, no tenancy resolution, no authorization table. Legal only
	// for a composition that carries no authn module -- there would be
	// nothing to verify and no claim to resolve a tenant from -- and never
	// a shortcut for a host that simply has not wired its chain yet.
	Chainless bool

	// Verifier is authn's token verifier (authn.Module.Service().Verifier()):
	// the fixed chain's outermost layer, and what turns a bearer token into
	// the verified Principal every layer below reads. Required unless
	// Chainless.
	Verifier *authn.Verifier

	// Authorizer and RouteRules declare the route-authorization domain: the
	// authorizer every gated route's permission check runs against, and the
	// host's route table naming what each module route requires. The pair
	// travels to rbac.GuardRoutes (through chain.WithAuthorization), so the
	// table is checked for exhaustiveness in both directions. Both empty
	// leaves every mounted route unguarded.
	Authorizer rbac.Authorizer
	RouteRules []rbac.RouteRule

	// AdminPrefix is the admin module's mount prefix (admin.APIPath), so
	// the admin subtree is dispatched ahead of impersonation and the
	// tenancy chain. Empty for a composition with no admin module.
	AdminPrefix string

	// Impersonation is the impersonation decorator applied between
	// authn.Middleware and the tenancy chain -- its one correct position.
	// Nil for a composition with no module providing identity substitution.
	Impersonation func(http.Handler) http.Handler

	// TenantStatusResolver wires the tenancy chain's tenant-status gate:
	// admin's own tenant ledger satisfies it structurally. Nil leaves the
	// gate unwired.
	TenantStatusResolver tenancy.TenantStatusResolver

	// ExtraAllowlist adds the host's own entries to the pre-auth allowlist
	// the tenancy chain appends to app.PreAuthAllowlist(): a genuinely
	// public route the host mounts that resolves its own tenant
	// server-side. Scope each entry to the exact method the route serves.
	ExtraAllowlist []tenancy.MiddlewareOption

	// MountHostRoutes mounts the host's own hand-written routes on the
	// protected mux, before the chain's derivation mounts the module
	// routes on it. The routes mounted here sit behind the tenancy chain
	// exactly like module routes, but are NOT admitted through the
	// authorization table (which covers the modules' mounted routes only),
	// so a host route that needs a permission check applies it itself.
	// Nil means the host mounts none.
	MountHostRoutes func(mux *http.ServeMux)

	// OuterWrapper wraps the finished handler, outside everything else: the
	// platform middleware, the fixed chain and the host's own routes. It is
	// where a host expresses the one layer that legitimately answers
	// requests without reaching the chain at all -- the SPA file server
	// that serves the built frontend is the standing example. Nil means no
	// outer layer.
	OuterWrapper func(next http.Handler) http.Handler
}

// guarded reports the guarded-half fields the policy sets, for the
// construction checks: a chainless policy must carry none of them, and a
// guarded policy must carry a verifier.
func (p *LinkPolicy) guarded() []string {
	var set []string
	if p.Verifier != nil {
		set = append(set, "Verifier")
	}
	if p.Authorizer != nil {
		set = append(set, "Authorizer")
	}
	if len(p.RouteRules) > 0 {
		set = append(set, "RouteRules")
	}
	if p.AdminPrefix != "" {
		set = append(set, "AdminPrefix")
	}
	if p.Impersonation != nil {
		set = append(set, "Impersonation")
	}
	if p.TenantStatusResolver != nil {
		set = append(set, "TenantStatusResolver")
	}
	if len(p.ExtraAllowlist) > 0 {
		set = append(set, "ExtraAllowlist")
	}
	return set
}

// validate refuses a policy whose two halves contradict one another. The
// check runs when the component assembles the face, so a misdeclared policy
// fails the Serve stage loudly rather than producing a face that ignores
// half of what the host declared.
func (p *LinkPolicy) validate() error {
	guarded := p.guarded()
	if p.Chainless {
		if len(guarded) > 0 {
			return fmt.Errorf("httpserve: the link policy declares Chainless and also carries the guarded field(s) %v; a chainless composition runs no fixed chain, so the guarded half would be silently unused", guarded)
		}
		return nil
	}
	if p.Verifier == nil {
		return fmt.Errorf("httpserve: the link policy declares a fixed chain but carries no Verifier; a composition with no authn module declares Chainless instead, and the chainless form is a positive declaration, never a missing-verifier fallback")
	}
	return nil
}

// chainOptions translates the policy into the chain.Standard options; the
// fixed order itself stays chain's, this component only supplies the
// host's half.
func (p *LinkPolicy) chainOptions() []chain.Option {
	var opts []chain.Option
	if p.Authorizer != nil || len(p.RouteRules) > 0 {
		opts = append(opts, chain.WithAuthorization(p.Authorizer, p.RouteRules))
	}
	if p.AdminPrefix != "" {
		opts = append(opts, chain.WithAdminPrefix(p.AdminPrefix))
	}
	if p.Impersonation != nil {
		opts = append(opts, chain.WithImpersonation(p.Impersonation))
	}
	if p.TenantStatusResolver != nil {
		opts = append(opts, chain.WithTenantStatusResolver(p.TenantStatusResolver))
	}
	if len(p.ExtraAllowlist) > 0 {
		opts = append(opts, chain.WithExtraAllowlist(p.ExtraAllowlist...))
	}
	return opts
}

// httpComponent returns the descriptor: the component's one product is the
// *Face, which satisfies both declaration-face tokens. It declares
// MultiReplicaSafe -- every replica runs its own listener over the same
// deployed composition, sharing no state with any sibling -- and its one
// required dependency is the host's link policy.
func httpComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:         ComponentName,
		ConfigSchema: (*httpConfig)(nil),
		Capabilities: pkgcore.MultiReplicaSafe,
		Provides: []any{
			(*pkgcore.RouteRegistrar)(nil),
			(*pkgcore.MiddlewareRegistrar)(nil),
		},
		Requires: []pkgcore.Requirement{{Token: (*LinkPolicy)(nil)}},
		New:      newFace,
		Serve:    serveFace,
		Stop:     stopFace,
		Close:    closeFace,
	}
}

// newFace constructs the component's product: the face reads the host link
// policy from the by-type context (the requirement machinery has resolved
// its provider by now) and decodes the component's own configuration block.
// The Init stage does NOT assemble anything -- other components are still
// mounting their routes then -- and the Serve stage is where serveFace reads
// the accumulated declarations.
func newFace(_ context.Context, reg *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
	policy, err := pkgcore.Get[*LinkPolicy](reg)
	if err != nil {
		return nil, fmt.Errorf("httpserve: read the host link policy: %w", err)
	}
	decoded := httpConfig{}.defaults()
	if err := cfg.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("httpserve: the component's configuration: %w", err)
	}
	if decoded.Addr == "" {
		return nil, fmt.Errorf("httpserve: the component's configured addr is empty; an empty listen address binds every interface on a default port and is refused -- set addr (for example \":8080\") in the component's block")
	}
	return &Face{reg: reg, cfg: decoded, policy: policy}, nil
}
