package rbac

import (
	"context"

	"github.com/vislake/speed/go/dbkit/audit"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// This file is rbac's declarative audit-emission half: the four write
// paths that change who may do what -- defineRole (DefineRole and the
// built-in seeding that runs through it), AssignRole, RevokeRole and
// RestoreRole -- call Service.
// emitAudit once their write has committed, and the row lands through the
// same audit.event.recorded channel every other module's explicit Emit
// calls use (go/dbkit/audit's EventRecorded, persisted by the host's
// audit.Module subscriber). Every role define, assign, revoke and restore
// -- an impersonation-era grant included -- therefore produces an audit
// row.
//
// The emitted rows carry the dual-identity shape audit.Emit itself reads
// from ctx (pkgcore.ActorFromContext / pkgcore.OnBehalfOfFromContext), so
// an impersonation flow whose middleware installed both carriers -- go/
// admin's ImpersonationMiddleware does -- is recorded with the impersonated
// user as Actor and the real administrator as OnBehalfOf with no further
// wiring here. What this module guarantees on top is that an ordinary role-
// management write never lands a blank attribution: when ctx carries no
// pkgcore.Actor at all, emitAudit derives one from the rbac.Subject the
// host's authenticating layer installed (rbac.WithSubject, the module's
// own audit-context carrier; service.go's actorFrom reads it the same
// way). A Subject in rbac.SystemDomain -- the pseudo-
// tenant every admin:* permission is evaluated in, and the one go/admin's
// operations handlers install -- derives a
// platform-admin Actor; any other tenant derives a user Actor (pkgcore's
// own ActorType vocabulary). DisplayName stays empty: rbac holds no names
// and performs no user lookup.
// Only a ctx carrying neither carrier -- a write no authenticating layer
// vouched for -- falls through to audit.Emit's own zero-value Actor, the
// row shape go/dbkit/audit sanctions for exactly this corner.
//
// A failure to publish the record is logged at Error and never returned:
// by the time emitAudit runs, the role write has already committed, and
// surfacing the audit failure as the write's own error would report a
// failure that did not happen -- the identical choice go/pki's Handler,
// go/admin's services and the reference app's notes handler make for their
// own post-commit audits.
func (s *Service) emitAudit(ctx context.Context, in audit.Input) {
	if s.actions == nil || s.bus == nil {
		// A Service whose Attach ran on a registry always carries both (the
		// AuditActions seat and the Events bus exist on every Registry); a
		// Service constructed by hand, without Attach, can carry neither
		// and is refused here.
		return
	}
	ctx = s.withAuditActor(ctx)
	if err := audit.Emit(ctx, s.bus, s.actions, in); err != nil {
		obs.FromContext(ctx).Error("rbac role-management audit event emit failed",
			"action", in.Action, "resource_type", in.Resource.Type,
			"resource_id", in.Resource.ID, "error", err)
	}
}

// auditResourceRole is the audit.Resource.Type every role-management
// emission carries, in the dotted "<module>.<entity>" form the other
// modules' emissions use ("sharing.share", "admin.tenant",
// "integration.apikey"). A role write names the ROLE it is about as its
// resource -- the resource's DisplayName is the role's key, which is
// exactly what a role-management trail is read by -- and the binding's
// own (user, node) tuple travels in the Changes diff, where
// audit_test.go's assertions read it back.
const auditResourceRole = "rbac.role"

// bindingAuditDiff is the Changes element every binding write's emission
// carries: the (user, node) tuple the grant attached to, so a row naming
// only the role as its resource still answers "who was granted where".
// NodeID is empty for a tenant-wide grant.
func bindingAuditDiff(userID, nodeID string) *audit.Diff {
	return &audit.Diff{After: map[string]any{
		"user_id": userID,
		"node_id": nodeID,
	}}
}

// withAuditActor layers the Actor audit.Emit will record when ctx carries
// none yet, so a role-management write is never recorded with a blank
// attribution on the flows this module controls. A pkgcore.Actor the host
// already installed (an impersonation middleware's target, say) is left
// untouched -- including OnBehalfOf, which is layered independently and
// always flows through as ctx carries it. The derivation is from the
// module's own context carrier (see the file comment above):
//
//   - ctx carries an Actor: returned unchanged.
//   - ctx carries a Subject in rbac.SystemDomain: a platform-admin Actor
//     named by the subject's user id.
//   - ctx carries any other Subject: a user Actor named by its user id.
//   - ctx carries neither: returned unchanged, so audit.Emit records the
//     zero Actor it sanctions for unwitnessed writes.
func (s *Service) withAuditActor(ctx context.Context) context.Context {
	if _, ok := pkgcore.ActorFromContext(ctx); ok {
		return ctx
	}
	sub, ok := SubjectFromContext(ctx)
	if !ok {
		return ctx
	}
	actorType := pkgcore.ActorTypeUser
	if sub.TenantID == SystemDomain {
		actorType = pkgcore.ActorTypePlatformAdmin
	}
	return pkgcore.WithActor(ctx, pkgcore.Actor{Type: actorType, ID: sub.UserID})
}
