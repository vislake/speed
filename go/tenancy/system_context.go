package tenancy

import (
	"context"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// EventSystemContextEntered is the pkgcore.Event.Type published every time
// WithSystemContext successfully grants the tenant-isolation escape hatch.
// Its Payload is a SystemContextEnteredEvent.
//
// Per docs/internal/01-architecture.md, the event bus is the one seam
// reserved for observability and audit: a future audit-log consumer (the
// eventual compliance module) can build a complete record of every
// system-context grant purely by subscribing to this one event type on the
// same pkgcore.EventBus, without any further dependency on tenancy.
const EventSystemContextEntered = "tenancy.system_context.entered"

// SystemContextEnteredEvent is the Payload carried by an
// EventSystemContextEntered event. Its fields mirror pkgcore.SystemReason
// plus the time of the grant, which together answer who entered the
// tenant-isolation escape hatch, for what declared purpose, and when -- the
// whole of what an audit-log consumer needs, without requiring anything
// beyond this one event.
type SystemContextEnteredEvent struct {
	// Actor identifies who entered the system context: a platform
	// administrator's identifier, or a named system task. Copied verbatim
	// from the SystemReason passed to WithSystemContext.
	Actor string

	// Purpose is the registered reason the escape hatch was granted for.
	// Copied verbatim from the SystemReason passed to WithSystemContext.
	Purpose pkgcore.SystemPurpose

	// Ticket optionally links the grant to an external record such as a
	// support ticket. Copied verbatim from the SystemReason passed to
	// WithSystemContext; empty when the caller supplied none.
	Ticket string

	// EnteredAt is when the system context was granted.
	EnteredAt time.Time
}

// ErrAuditPublishFailed is returned by WithSystemContext when
// pkgcore.WithSystemContext itself succeeded but publishing the resulting
// audit event failed. See WithSystemContext's doc comment for why this fails
// the whole call closed instead of returning the context it already
// elevated.
//
// Because WithParam and WithCause always derive a new *apperr.Error rather
// than mutating the receiver, do not match this with errors.Is or == once a
// call may have decorated it -- use apperr.As(err) and compare .Code, the
// same convention dbkit's own error index documents.
var ErrAuditPublishFailed = apperr.Internal("tenancy.system_context_audit_publish_failed")

// WithSystemContext wraps pkgcore.WithSystemContext, additionally publishing
// an audit event (via the given pkgcore.EventBus) recording who entered the
// system context, for what declared purpose, and when. Per CLAUDE.md's
// security rules, entering system context without a resulting audit record
// is exactly the failure mode this wrapper exists to prevent -- if the audit
// publish fails, WithSystemContext must fail closed (return the original ctx
// and a non-nil error), NOT silently proceed as if nothing happened.
//
// Business code should call this instead of pkgcore.WithSystemContext
// directly. The raw primitive still exists, and remains the right choice, for
// code that sits at or below tenancy in the module dependency graph -- dbkit
// in particular cannot depend on tenancy at all, on pain of an import cycle
// (tenancy itself depends on dbkit for its GORM plugin machinery) -- so it
// has no audited wrapper available and uses pkgcore's version directly. Any
// code able to import tenancy should prefer this one.
//
// IMPORTANT -- what this does NOT do: granting a system context has no
// effect on dbkit.Repository[T] (the sanctioned data-access path, backend
// coding standard section 3.2) in either direction. Repository[T]'s
// methods never consult pkgcore.SystemReasonFromContext at all, so
// composing WithSystemContext with a Repository[T] call is currently
// inert: it does not widen visibility beyond whatever tenant the context
// already carries, and on a context with no tenant at all it does not
// substitute for one either -- Repository[T] still fails closed with
// pkgcore.ErrNoTenant exactly as if WithSystemContext had never been
// called. See system_context_repository_test.go and
// middleware_system_context_test.go for this proved directly against a
// real Repository[T] and a real net/http request. Until Repository[T]
// deliberately implements the cross-tenant escape hatch that dbkit's own
// tenant_scope.go doc comment anticipates it will, WithSystemContext only
// produces an audited record that the escape hatch was granted -- it does
// not, by itself, unlock any cross-tenant Repository[T] read or write. A
// genuine cross-tenant admin search or background job needs the raw-SQL
// escape hatch (backend coding standards section 3.2) instead, today.
//
// Two failure modes are handled differently:
//
//   - pkgcore.WithSystemContext rejects reason outright (an empty Actor, or a
//     Purpose never declared through pkgcore.RegisterSystemPurpose): nothing
//     was ever elevated, so there is nothing to audit. The error is
//     propagated unchanged and ctx comes back exactly as
//     pkgcore.WithSystemContext itself would return it -- unelevated, with no
//     event published.
//   - pkgcore.WithSystemContext succeeds but bus.Publish fails (for example,
//     one of the bus's subscribers returned an error): WithSystemContext
//     fails closed. It returns the original, non-elevated ctx -- never the
//     context it successfully elevated -- together with a non-nil error,
//     because a granted escape hatch with no audit record is exactly the gap
//     this wrapper exists to close. Callers must treat this exactly like any
//     other WithSystemContext failure: no escape hatch was granted.
//
// bus is taken as an explicit parameter, rather than resolved from ctx or a
// package-level global, so the audit trail's destination is always visible
// at the call site. The event is published against the original ctx, not the
// elevated one, so a subscriber does not silently inherit the escape hatch
// merely by listening for its audit trail -- a subscriber that itself needs
// to bypass tenant filtering (to write a platform-wide audit log row, say)
// declares its own, separately attributed SystemReason.
func WithSystemContext(ctx context.Context, bus pkgcore.EventBus, reason pkgcore.SystemReason) (context.Context, error) {
	elevated, err := pkgcore.WithSystemContext(ctx, reason)
	if err != nil {
		// Nothing was granted, so there is nothing to audit: propagate the
		// error unchanged. elevated is ctx itself here, per
		// pkgcore.WithSystemContext's own contract on failure.
		return elevated, err
	}

	evt := pkgcore.Event{
		Type: EventSystemContextEntered,
		Payload: SystemContextEnteredEvent{
			Actor:     reason.Actor,
			Purpose:   reason.Purpose,
			Ticket:    reason.Ticket,
			EnteredAt: time.Now(),
		},
	}
	// TenantID correlates the audit event with whichever tenant, if any, the
	// caller was already scoped to -- for example an admin using the escape
	// hatch while working one specific tenant's support ticket. It stays at
	// its zero value when ctx carries no tenant (registration, or a
	// cross-tenant search), matching pkgcore.Event's own documented
	// convention for events that are not tenant-scoped. Either way this is
	// descriptive metadata only: entering system context never grants tenant
	// scoping by itself.
	if tenant, ok := pkgcore.TenantFromContext(ctx); ok {
		evt.TenantID = tenant
	}

	if err := bus.Publish(ctx, evt); err != nil {
		// Fail closed: the escape hatch was granted but has no corresponding
		// audit record, which is exactly the gap this wrapper exists to
		// close. Return the original, non-elevated ctx.
		return ctx, ErrAuditPublishFailed.WithCause(err)
	}

	return elevated, nil
}
