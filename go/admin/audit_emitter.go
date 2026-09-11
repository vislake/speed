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
// the emitXxxAudit wrappers tolerate by skipping the publish entirely,
// the same pre-Register tolerance every emitting service's own docs
// state.
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

// The three emitXxxAudit wrappers below are the emitter's whole surface
// for a post-hoc record -- one per record family. Each publishes in
// through dbkit's audited Emit (the shared publish skeleton) and reports
// a failure the one way a post-hoc record may: a Warn carrying the
// family's own message constant, the caller's own attrs and the error,
// and nothing else -- never a returned error. Every caller records
// metadata of an operation that has already durably committed and been
// answered (a ledger write, an impersonation grant, a completed export),
// so a lost record changes nothing about the operation's legitimacy and
// there is nothing left to refuse; that is audit.Emit's own caller-half
// contract, the skip-with-alert pole, and these wrappers are where this
// module honors it in one place.
//
// attrs is the caller's per-record log context (the resource's own
// identifier, the action). The message is each family's own constant,
// written as a string literal at its Warn call: the structured logger's
// message must be statically constant (the call shape
// tools/semgrep_rules/non-constant-log-message.yml checks), so the
// wrappers take no message parameter.
//
// The publish is skipped entirely while no bus is attached -- the
// pre-Register window and the in-package unit fixtures that attach a
// subset of the seats. Nothing is skipped silently once a bus exists: a
// failed publish always Warns before returning.
func (e *auditEmitter) emitTenantStatusChangeAudit(ctx context.Context, attrs []any, in audit.Input) {
	if err := e.publish(ctx, in); err != nil {
		obs.FromContext(ctx).Warn("admin failed to record a tenant status-change audit event",
			append(attrs, "error", err)...)
	}
}

// emitExportAudit carries admin.audit_export records
// (ExportService.recordAudit) under emitTenantStatusChangeAudit's own
// contract, with its own message constant.
func (e *auditEmitter) emitExportAudit(ctx context.Context, attrs []any, in audit.Input) {
	if err := e.publish(ctx, in); err != nil {
		obs.FromContext(ctx).Warn("admin failed to record an audit-export audit event",
			append(attrs, "error", err)...)
	}
}

// emitImpersonationAudit carries admin.impersonation.* records
// (ImpersonationService.recordAudit) under emitTenantStatusChangeAudit's
// own contract, with its own message constant.
func (e *auditEmitter) emitImpersonationAudit(ctx context.Context, attrs []any, in audit.Input) {
	if err := e.publish(ctx, in); err != nil {
		obs.FromContext(ctx).Warn("admin failed to record an impersonation audit event",
			append(attrs, "error", err)...)
	}
}

// publish runs the audited publish all three wrappers share: skipped
// while no bus is attached -- the pre-Register window and the in-package
// unit fixtures that attach a subset of the seats -- and otherwise
// audit.Emit's own error, handed straight back for the wrapper's Warn.
func (e *auditEmitter) publish(ctx context.Context, in audit.Input) error {
	if e.bus == nil {
		return nil
	}
	return audit.Emit(ctx, e.bus, e.actions, in)
}
