package admin

import (
	"context"
	"net/http"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/pkgcore"
)

// ImpersonationHeader is the request header a caller presents an active
// impersonation grant id through, exactly the name
// docs/internal/23-admin.md section 4.1 proposes ("X-Admin-Impersonation:
// grant_id").
const ImpersonationHeader = "X-Admin-Impersonation"

// GrantLookup is the read-only view of *ImpersonationService the
// middleware below needs. It is declared here as a narrow,
// structurally-typed interface -- not *ImpersonationService itself --
// purely so a test can substitute a fake grant store without building a
// whole service; *ImpersonationService satisfies it through its own
// Lookup method with no adapter required.
type GrantLookup interface {
	// Lookup returns the grant named by id and true only when it exists
	// and is currently Active; otherwise (nil, false), and NEVER an
	// error -- see ImpersonationService.Lookup's own doc comment for why
	// that fail-closed contract is load-bearing here.
	Lookup(ctx context.Context, id string) (*ImpersonationGrant, bool)
}

// ImpersonationMiddleware is D5's request-pipeline mechanism -- the piece
// docs/internal/23-admin.md section 4.2 describes as a "PrincipalResolver
// decorator" wrapping authn.NewPrincipalResolver().
//
// # A corrected mechanism, not the document's literal wording
//
// This round verified the real go/tenancy and go/authn source before
// implementing anything, per this document's own closing instruction (a
// stale citation must be corrected against the real code). The verification
// found the document's mechanism description imprecise in a way that
// matters:
//
//   - tenancy.Resolver.Resolve(r *http.Request) returns
//     (pkgcore.TenantID, error) -- a BARE tenant id, nothing richer. A
//     "decorator" wrapping authn.NewPrincipalResolver() and returning a
//     substituted Resolver could therefore change WHICH TENANT
//     tenancy.Middleware injects, but has no channel at all to also swap
//     WHICH USER the rest of the request pipeline believes it is talking
//     to, because tenancy.Middleware's only side effect is
//     pkgcore.WithTenant -- it never touches the Principal authn.Middleware
//     already installed.
//   - The actual per-request identity every downstream consumer reads --
//     rbac.RequirePermission's default subjectFrom, and any handler that
//     wants the caller's user id -- is authn.PrincipalFromContext(ctx), set
//     once by authn.Middleware and never touched by tenancy.Middleware or
//     its Resolver.
//
// The correct real implementation is therefore an ordinary net/http
// middleware inserted into the chain BETWEEN authn.Middleware and
// tenancy.Middleware -- never a tenancy.Resolver decorator -- that reads
// the real authn.Principal already on the request context, checks
// ImpersonationHeader against lookup, and on a valid, unexpired,
// non-ended grant, calls authn.WithPrincipal to substitute a Principal
// naming the target user and target tenant for everything downstream,
// while independently recording the real administrator with
// pkgcore.WithOnBehalfOf so audit capture keeps both identities. This
// satisfies every one of section 4.2's actual requirements -- the
// administrator's own token is still what authn.Middleware verified
// (property (a)), the chain's fixed order is unchanged, and neither authn
// nor tenancy needs to know admin exists -- while matching what the real
// tenancy.Resolver interface can and cannot express.
//
// Wire it as:
//
//	authn.Middleware(verifier)(
//	    admin.ImpersonationMiddleware(impersonationSvc)(
//	        tenancy.Middleware(authn.NewPrincipalResolver(), ...)(mux),
//	    ),
//	)
//
// # The five mandatory properties
//
//   - (a) The credential on every request is still the administrator's own
//     real access token: this middleware never mints, reads or verifies any
//     token of its own -- it only runs after authn.Middleware already did,
//     and only ever reads the Principal that verification produced.
//   - (b) A permission check downstream uses the TARGET user's Subject,
//     never the administrator's: the substituted Principal's UserID is
//     grant.TargetUserID, so a subject-resolving seam built from
//     authn.PrincipalFromContext (the reference app's demoSubjectResolver,
//     for one) reads the target's identity, and rbac.RequirePermission
//     evaluates against exactly that.
//   - (c) fail-closed on an invalid/expired/ended grant: lookup.Lookup's own
//     contract (GrantLookup's doc comment) is what this method leans on --
//     any lookup miss falls straight through to next.ServeHTTP with the
//     request UNMODIFIED, which is the administrator's own real identity,
//     never a forged target identity and never a request failure either.
//   - (d) dual-identity audit: pkgcore.WithActor(target) +
//     pkgcore.WithOnBehalfOf(admin) are both set on the substituted
//     context, so ANY audit record produced for the rest of this request
//     -- admin's own explicit Emit calls in impersonation_service.go, and
//     any other module's dbkit automatic write-capture or explicit Emit
//     alike -- carries the correct dual identity with no further wiring.
//   - (e) the mandatory notification is Start's own responsibility
//     (impersonation_service.go's notifyStarted), not this middleware's --
//     it fires once, when the grant is created, never on every
//     subsequent request the grant authorizes.
//
// Two more boundaries this middleware enforces beyond the five properties
// (P1-2's and P1-3's fixes), governing what the substituted Principal does
// NOT carry across the impersonation boundary:
//
//   - SessionID is NOT copied from the administrator's principal. authn's
//     session lifecycle keys on a session id alone (Service.Logout revokes
//     whatever id it is handed), so an impersonated request presenting the
//     administrator's session id could sign the administrator out of their
//     own console session. The substituted principal's SessionID is the
//     GRANT's id instead -- the grant is the short-lived, revocable
//     session this principal acts through -- so a session-scoped operation
//     during impersonation pairs (target user, grant), never (target user,
//     administrator session).
//   - AMR is never copied either. The administrator's authentication
//     methods (a second factor above all) describe how the ADMINISTRATOR
//     proved themselves, not the target; inheriting them would let an
//     administrator whose own session carries mfa:totp satisfy an MFA gate
//     as the target. The target proved no method in this session, so the
//     substituted principal's AMR is empty and no "requires a second
//     factor" policy can be satisfied by impersonation alone.
//
// An extra, conservative tightening beyond the document's own text: a
// grant is honored ONLY when its AdminUserID matches the CURRENT request's
// own verified Principal. A grant id that leaked or was guessed is
// therefore useless to any administrator other than the one who started
// it -- their own access token must still verify on every request, exactly
// as property (a) requires, and a different administrator's token does not
// let them ride someone else's grant.
func ImpersonationMiddleware(lookup GrantLookup) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			grantID := r.Header.Get(ImpersonationHeader)
			if grantID == "" {
				next.ServeHTTP(w, r)
				return
			}

			adminPrincipal, ok := authn.PrincipalFromContext(r.Context())
			if !ok {
				// No verified identity to substitute FOR at all. Proceed
				// unmodified; whatever downstream gate exists (tenancy's
				// own fail-closed default, most likely) refuses the
				// request exactly as it would with no header present.
				next.ServeHTTP(w, r)
				return
			}

			grant, ok := lookup.Lookup(r.Context(), grantID)
			if !ok || grant.AdminUserID != adminPrincipal.UserID {
				// Invalid, unknown, expired, ended, or belonging to a
				// DIFFERENT administrator: fall back to the
				// administrator's own real identity. NEVER fail open as
				// the target user.
				next.ServeHTTP(w, r)
				return
			}

			// The substituted principal deliberately carries NOTHING that
			// belongs to the administrator's own session (P1-2's and
			// P1-3's fixes). SessionID names the ADMIN's real session, and
			// authn's session lifecycle keys on it alone -- Service.Logout
			// revokes whatever session id it is handed with no user-pairing
			// check -- so an inherited session id would let a request
			// acting as the target sign the administrator out of their own
			// console session, and pair (target user, admin session) in
			// every session-scoped operation. The impersonation's own
			// session identity is the GRANT -- the short-lived, revocable
			// credential this principal acts through, whose id every admin
			// session-scoped call already names -- so SessionID carries
			// grant.ID, never the administrator's. AMR lists the
			// authentication methods the ADMINISTRATOR proved, and
			// inheriting it would let the administrator's own second
			// factor (mfa:totp, say) satisfy an MFA gate as the target; the
			// target proved no method in this session, so AMR stays empty
			// and no downstream "requires a second factor" policy can be
			// satisfied by this principal.
			targetPrincipal := authn.Principal{
				UserID:    grant.TargetUserID,
				TenantID:  pkgcore.TenantID(grant.TargetTenantID),
				SessionID: grant.ID,
			}

			ctx := authn.WithPrincipal(r.Context(), targetPrincipal)

			// P2-pkgcore-actor-1: DisplayName is deliberately left empty on
			// both identities below. This middleware's whole context is the
			// grant row (Lookup's contract) and the verified Principal --
			// which carries no display name (go/authn/token.go) -- and it
			// deliberately performs no user-record lookup of its own, so no
			// honest name exists at this layer: filling the field would mean
			// adding a users-store seam to the narrow GrantLookup contract
			// and paying a lookup per impersonated request for a label only
			// consumed if some downstream module happens to write an audit
			// row. The impersonation audit events THEMSELVES --
			// admin.impersonation.started/ended, the rows an investigation
			// reads -- are not produced here but by ImpersonationService.
			// recordAudit, which does resolve both names against the users
			// table at record time (see its own doc comment). A future round
			// wanting names on every DOWNSTREAM module's capture rows during
			// an impersonated request would need either a user-lookup seam
			// on this middleware or display names snapshot onto the grant
			// row at Start time; neither exists today, and id-only ctx
			// actors remain the honest record of what this layer knows.
			ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: grant.TargetUserID})
			ctx = pkgcore.WithOnBehalfOf(ctx, pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: adminPrincipal.UserID})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
