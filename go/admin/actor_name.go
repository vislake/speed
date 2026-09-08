package admin

import (
	"context"
	"errors"

	"github.com/vislake/speed/go/authn"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// resolveActorName returns a with its DisplayName filled from authn's
// users table: every audit record this module emits for a HUMAN actor (a
// platform operator, an impersonated target user) must carry the account's
// display name so the record stays readable after the account is later
// renamed or deleted, the purpose pkgcore.Actor.DisplayName documents for
// itself. The caller-side construction sites (handler.go's
// callerUserID-derived actors, the impersonation pipeline's id-only
// identities) know a human only by the Principal's user id -- authn's
// Principal type carries no display name -- so the users table is the
// single honest source for the label, and every recordAudit-shaped call
// site resolves through this one helper at record time rather than each
// site chasing its own read.
//
// a is returned unchanged, with no lookup attempted, when:
//
//   - it already carries a DisplayName (never overwrite a name an earlier
//     resolution or a system site set deliberately);
//   - its Type is neither ActorTypeUser nor ActorTypePlatformAdmin: a
//     system actor (the retention sweep's named actor, the automatic
//     impersonation end's "system:rbac_permission_revoked" reason id) is
//     not a users-table account, and no other type has a user row to
//     resolve;
//   - its ID is empty;
//   - authnSvc is nil -- the seam not yet attached (before Module.Register
//     runs, or a lightweight unit test that wired none of the recorders'
//     audit seams). WithAuthn is a mandatory production option, so every
//     real deployment resolves names here; a nil-seam unit fixture keeps
//     its id-only records.
//
// A user row that cannot be found (authn.ErrNotFound) also leaves a
// unchanged, silently: the id has no name anywhere (a fixture id with no
// account behind it, or an account deleted before the record was made --
// the one case an empty label cannot be helped anyway), and a Warn per
// record would only add noise to paths that are often exercised with
// deliberately fake operator ids (impersonation_service_test.go's
// "admin-1" fixtures). Any OTHER lookup failure is Warn-logged and a is
// still returned unchanged: the caller's own recordAudit contract is
// log-and-swallow -- a best-effort label must never turn an
// already-committed operation's audit record into a lost one -- and the
// authoritative id is never dropped from the record either way.
func resolveActorName(ctx context.Context, authnSvc *authn.Service, a pkgcore.Actor) pkgcore.Actor {
	if a.DisplayName != "" || a.ID == "" || authnSvc == nil ||
		(a.Type != pkgcore.ActorTypeUser && a.Type != pkgcore.ActorTypePlatformAdmin) {
		return a
	}
	user, err := authnSvc.Users().FindByID(ctx, a.ID)
	if err != nil {
		if !errors.Is(err, authn.ErrNotFound) {
			obs.FromContext(ctx).Warn("admin could not resolve an audit actor's display name; recording id-only",
				"actor_type", a.Type, "actor_id", a.ID, "error", err)
		}
		return a
	}
	a.DisplayName = user.DisplayName
	return a
}
