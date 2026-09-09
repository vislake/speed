package admin

import (
	"context"
	"net/http"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/pkgcore"
)

// ImpersonationHeader is the request header a caller presents an active
// impersonation grant id through, in the form
// "X-Admin-Impersonation: grant_id".
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

// ImpersonationMiddleware is the request-pipeline mechanism that turns a
// valid impersonation grant into a substituted identity for one request.
// It is an ordinary net/http middleware inserted into the chain BETWEEN
// authn.Middleware and tenancy.Middleware -- never a tenancy.Resolver
// decorator: tenancy.Resolver.Resolve(r *http.Request) returns a bare
// (pkgcore.TenantID, error) and tenancy.Middleware's only side effect is
// pkgcore.WithTenant, so a decorated Resolver could change WHICH TENANT is
// injected but has no channel at all to also swap WHICH USER the rest of
// the request pipeline believes it is talking to. The per-request identity
// every downstream consumer reads -- rbac.RequirePermission's default
// subjectFrom, and any handler that wants the caller's user id -- is
// authn.PrincipalFromContext(ctx), set once by authn.Middleware and never
// touched by tenancy.Middleware or its Resolver.
//
// The middleware reads the real authn.Principal already on the request
// context, checks ImpersonationHeader against lookup, and on a valid,
// unexpired, non-ended grant calls authn.WithPrincipal to substitute a
// Principal naming the target user and target tenant for everything
// downstream, while independently recording the real administrator with
// pkgcore.WithOnBehalfOf so audit capture keeps both identities. The
// chain's fixed order stays unchanged, and neither authn nor tenancy needs
// to know admin exists.
//
// Wire it as:
//
//	authn.Middleware(verifier)(
//	    admin.ImpersonationMiddleware(impersonationSvc)(
//	        tenancy.Middleware(authn.NewPrincipalResolver(), ...)(mux),
//	    ),
//	)
//
// The mechanism's five load-bearing properties:
//
//   - The credential on every request is still the administrator's own
//     real access token: this middleware never mints, reads or verifies any
//     token of its own -- it only runs after authn.Middleware already did,
//     and only ever reads the Principal that verification produced.
//   - A permission check downstream uses the TARGET user's Subject,
//     never the administrator's: the substituted Principal's UserID is
//     grant.TargetUserID, so a subject-resolving seam built from
//     authn.PrincipalFromContext (the reference app's DemoSubjectResolver,
//     for one) reads the target's identity, and rbac.RequirePermission
//     evaluates against exactly that.
//   - It fails closed on an invalid/expired/ended grant: lookup.Lookup's
//     own contract (GrantLookup's doc comment) is what this method leans
//     on -- any lookup miss falls straight through to next.ServeHTTP with
//     the request UNMODIFIED, which is the administrator's own real
//     identity, never a forged target identity and never a request failure
//     either.
//   - Audit is dual-identity: pkgcore.WithActor(target) +
//     pkgcore.WithOnBehalfOf(admin) are both set on the substituted
//     context, so ANY audit record produced for the rest of this request
//     -- admin's own explicit Emit calls in impersonation_service.go, and
//     any other module's dbkit automatic write-capture or explicit Emit
//     alike -- carries the correct dual identity with no further wiring.
//   - The mandatory notification is Start's own responsibility
//     (impersonation_service.go's dispatchStartNotification), not this
//     middleware's -- it fires once, when the grant is created, never on
//     every subsequent request the grant authorizes.
//
// Two more boundaries this middleware enforces, governing what the
// substituted Principal does NOT carry across the impersonation boundary:
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
// A grant is honored ONLY when its AdminUserID matches the CURRENT
// request's own verified Principal: a grant id that leaked or was guessed
// is useless to any administrator other than the one who started it --
// their own access token must still verify on every request, and a
// different administrator's token does not let them ride someone else's
// grant.
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
			// belongs to the administrator's own session. SessionID names
			// the ADMIN's real session, and authn's session lifecycle keys
			// on it alone -- Service.Logout revokes whatever session id it
			// is handed with no user-pairing check -- so an inherited
			// session id would let a request acting as the target sign the
			// administrator out of their own console session, and pair
			// (target user, admin session) in every session-scoped
			// operation. The impersonation's own session identity is the
			// GRANT -- the short-lived, revocable credential this principal
			// acts through, whose id every admin session-scoped call already
			// names -- so SessionID carries grant.ID, never the
			// administrator's. AMR lists the authentication methods the
			// ADMINISTRATOR proved, and inheriting it would let the
			// administrator's own second factor (mfa:totp, say) satisfy an
			// MFA gate as the target; the target proved no method in this
			// session, so AMR stays empty and no downstream "requires a
			// second factor" policy can be satisfied by this principal.
			targetPrincipal := authn.Principal{
				UserID:    grant.TargetUserID,
				TenantID:  pkgcore.TenantID(grant.TargetTenantID),
				SessionID: grant.ID,
			}

			ctx := authn.WithPrincipal(r.Context(), targetPrincipal)

			// DisplayName is deliberately left empty on both identities
			// below. This middleware's whole context is the grant row
			// (Lookup's contract) and the verified Principal -- which
			// carries no display name -- and it deliberately performs no
			// user-record lookup of its own, so no honest name exists at
			// this layer: filling the field would mean adding a users-store
			// seam to the narrow GrantLookup contract and paying a lookup
			// per impersonated request for a label only consumed if some
			// downstream module happens to write an audit row. The
			// impersonation audit events THEMSELVES --
			// admin.impersonation.started/ended, the rows an investigation
			// reads -- are not produced here but by
			// ImpersonationService.recordAudit, which does resolve both
			// names against the users table at record time (see its own doc
			// comment). Neither a user-lookup seam on this middleware nor
			// display names snapshotted onto the grant row at Start time
			// exists, so downstream capture rows produced during an
			// impersonated request stay id-only -- the honest record of
			// what this layer knows.
			ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: grant.TargetUserID})
			ctx = pkgcore.WithOnBehalfOf(ctx, pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: adminPrincipal.UserID})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
