package rbac

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/vislake/speed/go/pkgcore"
)

// ErrRouteUndecided reports a mounted route or a route-table entry that
// declares no usable authorization decision: a mounted route the table does
// not name, an entry declaring neither form, an entry declaring both forms
// at once, or a public entry carrying an option that only takes effect
// inside a permission check. GuardRoutes returns it before serving, so a
// route whose decision was never made fails startup rather than being
// admitted undecided.
//
// It is a plain sentinel, not an apperr code: nothing here runs on a
// request path -- the error can only surface while a host assembles its
// routes -- so there is no HTTP response for a code to shape.
var ErrRouteUndecided = errors.New("rbac: mounted route has no authorization decision")

// ErrRouteTableMismatch reports a route table that does not line up with the
// mounted routes: one path declared twice, or an entry for a path no module
// mounted. Like ErrRouteUndecided it is a wiring-time sentinel, returned by
// GuardRoutes before serving.
var ErrRouteTableMismatch = errors.New("rbac: route table does not match the mounted routes")

// RouteRule declares how one mounted route is authorized: the decision
// itself, plus the two adjustments the admission check needs. A host builds
// one per route its modules mount and hands the set to GuardRoutes, which
// refuses to serve a mounted route the table does not name -- so the table
// is the complete record of every route's authorization at startup.
type RouteRule struct {
	// Path names the mounted route this rule decides, exactly as the module
	// mounted it (MountedRoute.Path).
	Path string

	// Access is the decision: public, or the permission each request must
	// hold. Exactly one form must be declared; see pkgcore.RouteAccess for
	// both forms' contracts.
	Access pkgcore.RouteAccess

	// SubjectResolver overrides how the admission check obtains the Subject
	// for requests to this route, for the reason WithSubjectResolver
	// documents. Nil keeps the default: the Subject the authenticating side
	// installed with WithSubject. A route whose decisions must be evaluated
	// in another tenant domain -- a platform-scoped permission pinned to
	// rbac.SystemDomain, say -- wires its own resolver here.
	SubjectResolver func(*http.Request) (Subject, bool)

	// Exempt names requests the route's own handler authorizes, which
	// therefore bypass the permission check entirely. It exists for the
	// shape a module's handler gates itself per operation -- accepting an
	// invitation addressed to the caller is the standing example: it needs
	// no rbac grant, and org.Handler refuses an unidentifiable caller on
	// its own -- while the rest of the route stays gated.
	//
	// The handler is the only gate for an exempt request, so the predicate
	// must select exactly the operations the handler itself refuses without
	// a standing grant; nil gates every request, which is the ordinary
	// shape. An exempt request also never reaches the rule's Layer: the
	// bypass skips everything the gate would have run. Meaningless (and
	// refused) on a public route.
	Exempt func(*http.Request) bool

	// Layer, when set, wraps the route's handler INSIDE the permission
	// gate: the gate decides first, then this layer runs for an admitted
	// request, then the handler itself. It exists for a check that narrows
	// what the gate allowed -- rbac's DataScope machinery's consumer is the
	// standing example, refusing a request whose target lies outside the
	// caller's scoped grant -- and it must sit between gate and handler
	// because that is where the Subject the gate decided against is on the
	// request context: WithSubject's own contract, which the gate fulfils
	// before calling next. Nil when the route needs no such layer, which
	// is the ordinary shape.
	Layer func(http.Handler) http.Handler
}

// GuardRoutes applies a host's route table to the routes its modules
// mounted, returning one guarded route per mounted route, in mount order,
// each carrying the decision it was admitted under (MountedRoute.Access is
// filled from the matching rule).
//
// It is the platform's enforcement of the route-authorization discipline:
// every mounted route must have an explicit decision recorded before the
// process serves a request. A mounted route the table does not name is an
// error (ErrRouteUndecided), not a route served ungated; an entry for a
// path no module mounted, or the same path declared twice, is likewise an
// error (ErrRouteTableMismatch) rather than silently ignored -- the table
// and the mounted set must name exactly the same routes, so neither side
// can drift while the other still looks exhaustive.
//
// A route whose rule declares it public passes through untouched: the
// declaration is what admits it, and no check runs. A route whose rule
// selects a permission is wrapped in the same fail-closed gate
// RequirePermission and RequirePermissionFunc document -- one refusal shape
// for every gated route (403 rbac.permission_denied, 500 rbac.storage_error)
// -- with the rule's SubjectResolver when it has one, the rule's Layer (when
// declared) between the gate and the handler, and the rule's Exempt
// predicate short-circuiting the whole pipeline to the handler for exactly
// the requests it names.
//
// The returned routes are what the host mounts (pkgcore.MountRoutes); the
// input slice is not modified. az is the Authorizer every gated route's
// check runs against; a nil az leaves public routes served and every gated
// request answered 500 rbac.service_not_attached, exactly as the gate's own
// contract states.
func GuardRoutes(az Authorizer, mounted []pkgcore.MountedRoute, rules []RouteRule) ([]pkgcore.MountedRoute, error) {
	byPath := make(map[string]RouteRule, len(rules))
	for _, rule := range rules {
		if err := validateRouteRule(rule); err != nil {
			return nil, err
		}
		if _, duplicate := byPath[rule.Path]; duplicate {
			return nil, fmt.Errorf("%w: route %q is declared twice", ErrRouteTableMismatch, rule.Path)
		}
		byPath[rule.Path] = rule
	}

	guarded := make([]pkgcore.MountedRoute, 0, len(mounted))
	matched := make(map[string]struct{}, len(rules))
	for _, route := range mounted {
		rule, declared := byPath[route.Path]
		if !declared {
			return nil, fmt.Errorf("%w: a module mounted %q, which the route table does not name; declare it public or with the permission it requires",
				ErrRouteUndecided, route.Path)
		}
		matched[route.Path] = struct{}{}

		route.Access = rule.Access
		if !rule.Access.Public {
			route.Handler = gateRoute(az, rule, route.Handler)
		}
		guarded = append(guarded, route)
	}
	for _, rule := range rules {
		if _, mounted := matched[rule.Path]; !mounted {
			return nil, fmt.Errorf("%w: the route table declares %q, which no module mounted", ErrRouteTableMismatch, rule.Path)
		}
	}
	return guarded, nil
}

// validateRouteRule refuses a rule that declares no usable decision, or one
// whose options contradict its decision. Every case is a wiring error the
// host must resolve before serving, so the message names the path and what
// is wrong with it.
func validateRouteRule(rule RouteRule) error {
	switch {
	case rule.Access.Public && rule.Access.Permission != nil:
		return fmt.Errorf("%w: route %q is declared both public and permission-gated, which no check could honor",
			ErrRouteUndecided, rule.Path)
	case !rule.Access.Public && rule.Access.Permission == nil:
		return fmt.Errorf("%w: route %q declares neither public nor a permission it requires",
			ErrRouteUndecided, rule.Path)
	case rule.Access.Public && (rule.SubjectResolver != nil || rule.Exempt != nil || rule.Layer != nil):
		return fmt.Errorf("%w: route %q is public yet carries a subject resolver, an exemption or a layer, which only a permission check would use",
			ErrRouteUndecided, rule.Path)
	}
	return nil
}

// gateRoute wraps next in the rule's admission pipeline:
// RequirePermissionFunc with the rule's permission selector and, when
// declared, its subject resolver, around the rule's Layer (when declared)
// around next; the rule's Exempt predicate, when present, short-circuits
// the whole pipeline to next for the requests it names.
func gateRoute(az Authorizer, rule RouteRule, next http.Handler) http.Handler {
	inner := next
	if rule.Layer != nil {
		inner = rule.Layer(next)
	}
	var opts []MiddlewareOption
	if rule.SubjectResolver != nil {
		opts = append(opts, WithSubjectResolver(rule.SubjectResolver))
	}
	gated := RequirePermissionFunc(az, rule.Access.Permission, opts...)(inner)
	if rule.Exempt == nil {
		return gated
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rule.Exempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		gated.ServeHTTP(w, r)
	})
}
