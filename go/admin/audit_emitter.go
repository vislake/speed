package admin

import (
	"context"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/dbkit/audit"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// auditEmitter is admin's audited seam carried as one value: the EventBus
// every tenancy.WithSystemContext grant audits onto and every explicit
// audit record publishes through, the frozen audit-action catalog dbkit's
// Emit validates each record's action against, and the *authn.Service
// actor display names resolve from at record time (resolveActor).
//
// Every service in this module that emits an audit record or takes out a
// system-context grant holds one, attached by Module.Register, so the
// seats cannot end up half-wired per service: there is no service that
// has the bus but not the catalog it publishes against.
//
// The zero value is the pre-Register state -- no seats attached -- which
// emitBestEffort tolerates by skipping the publish entirely, the same
// pre-Register tolerance every emitting service's own docs state.
type auditEmitter struct {
	bus      pkgcore.EventBus
	actions  pkgcore.AuditActionRegistrar
	authnSvc *authn.Service
}

// attach wires all three seats, read from the host's
// *pkgcore.ComponentRegistry (and, for authnSvc, from the authn module)
// during Module.Register. It is the wiring door for a service that emits
// audit records; a service whose only use of the seam is its
// tenancy.WithSystemContext grant wires through attachBus instead.
func (e *auditEmitter) attach(bus pkgcore.EventBus, actions pkgcore.AuditActionRegistrar, authnSvc *authn.Service) {
	e.bus = bus
	e.actions = actions
	e.authnSvc = authnSvc
}

// attachBus wires only the bus -- the seam door for a service that takes
// system-context grants but never builds an audit record of its own, so
// it has no use for the action catalog or the name resolver.
func (e *auditEmitter) attachBus(bus pkgcore.EventBus) { e.bus = bus }

// resolveActor returns a with its DisplayName filled from authn's users
// table, through resolveActorName -- the module's one record-time
// name-resolution path (that helper's own doc comment has the policy).
func (e *auditEmitter) resolveActor(ctx context.Context, a pkgcore.Actor) pkgcore.Actor {
	return resolveActorName(ctx, e.authnSvc, a)
}

// emitBestEffort publishes in through dbkit's audited Emit and reports a
// failure the one way a post-hoc record may: a Warn carrying logMsg, the
// caller's own attrs and the error, and nothing else -- never a returned
// error. Every caller records metadata of an operation that has already
// durably committed and been answered (a ledger write, an impersonation
// grant, a completed export), so a lost record changes nothing about the
// operation's legitimacy and there is nothing left to refuse; that is
// audit.Emit's own caller-half contract, the skip-with-alert pole, and
// this method is where this module honors it in one place.
//
// attrs is the caller's per-record log context (the resource's own
// identifier, the action); the message itself stays a constant per call
// site, never a concatenation.
//
// The publish is skipped entirely while no bus is attached -- the
// pre-Register window and the in-package unit fixtures that attach a
// subset of the seats. Nothing is skipped silently once a bus exists: a
// failed publish always Warns before returning.
func (e *auditEmitter) emitBestEffort(ctx context.Context, logMsg string, attrs []any, in audit.Input) {
	if e.bus == nil {
		return
	}
	if err := audit.Emit(ctx, e.bus, e.actions, in); err != nil {
		obs.FromContext(ctx).Warn(logMsg, append(attrs, "error", err)...)
	}
}
