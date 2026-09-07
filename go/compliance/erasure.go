package compliance

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"
)

// SystemPurposeRightToErasure is the audited system purpose ErasureService.
// Erase enters (via tenancy.WithSystemContext) before calling any
// registered participant's Erase callback. Module.Register calls
// pkgcore.RegisterSystemPurpose with it, mirroring
// SystemPurposeRetentionSweep's identical convention.
const SystemPurposeRightToErasure pkgcore.SystemPurpose = "compliance.right_to_erasure"

// erasureFallbackActorID is the actor id an Erase call falls back to when
// its requestedBy Actor carries an empty ID (including the zero Actor) --
// see Erase's own doc comment for why the fallback must land on the Actor
// placed on ctx itself, not only in the system reason's own Actor field.
const erasureFallbackActorID = "compliance.right_to_erasure"

// AuditActionErasureRequest is the audit action Module.Register declares
// and ErasureService.Erase emits under, once per erasure request (never
// once per participant): one AuditEvent records the whole request, with
// Changes carrying the per-participant breakdown.
const AuditActionErasureRequest = "compliance.erasure.request"

// ErasureResult is Erase's outcome: how many rows each registered
// participant erased for the requested subject, and any per-participant
// error that did not stop the request -- see ErasureResult's Errors field
// and Erase's own doc comment for the partial-failure handling this
// records.
type ErasureResult struct {
	// Subject is the subject the erasure request named.
	Subject pkgcore.SubjectRef
	// Erased maps participant Name to how many rows it reported erasing.
	Erased map[string]int
	// Errors maps participant Name to the error its Erase callback
	// returned, for participants whose callback failed.
	Errors map[string]error
}

// TotalErased sums Erased across every participant.
func (r ErasureResult) TotalErased() int {
	total := 0
	for _, n := range r.Erased {
		total += n
	}
	return total
}

// HasErrors reports whether any participant's Erase callback failed.
func (r ErasureResult) HasErrors() bool { return len(r.Errors) > 0 }

// ErasureService runs the right-to-erasure request: for one subject, it
// calls every registered pkgcore.RetentionParticipant's Erase callback
// immediately, bypassing the retention window entirely, under an audited
// system context. Like RetentionService, it never touches a participant's
// table directly.
//
// The zero value is not ready to use; construct one with
// newErasureService and wire it through Module.Register.
type ErasureService struct {
	retention pkgcore.RetentionRegistrar
	bus       pkgcore.EventBus
	actions   pkgcore.AuditActionRegistrar
}

// newErasureService returns an ErasureService with no seams wired yet;
// Module.Register attaches the registry's EventBus, AuditActions and
// Retention registrar.
func newErasureService() *ErasureService {
	return &ErasureService{}
}

// Erase immediately erases every registered participant's data for
// subject, bypassing the retention window entirely -- the right-to-
// erasure ("right to be forgotten") path docs/internal/10-compliance-and-audit.md
// describes. requestedBy identifies who is asking: it becomes both the
// audited system context's SystemReason.Actor (a short stable string) and
// the pkgcore.Actor set on ctx before the system context is entered, so
// every participant's own HardDelete write -- and this call's own final
// audit event -- attributes to a real identity rather than the zero
// Actor, exactly the attribution obligation go/dbkit/hard_delete.go's doc
// comment warns a bare system context leaves unmet. A caller acting on
// behalf of the subject's own request (an intake ticket, a support case)
// should still pass a real operator or system-task Actor here -- Erase
// has no notion of "the subject erasing themselves": every erasure is
// performed by someone accountable for having triggered it. An empty
// requestedBy -- the zero Actor, or one whose ID is empty -- is replaced
// by a stable system actor (Type pkgcore.ActorTypeSystem, ID
// erasureFallbackActorID) before EITHER placement, so the Actor
// participants and the audit trail read off ctx never carries an empty
// ID even for a caller that passed none.
//
// Erase erases within the tenant ctx already carries, never a tenant a
// caller names from nowhere: the ctx tenant is the single data boundary
// an erasure may ever cross -- every participant's Erase callback below
// hard-deletes rows through it -- so subject.TenantID may only echo that
// same tenant back, mirroring Export's identical gate and its documented
// principle. A ctx carrying no tenant is refused with pkgcore.ErrNoTenant
// and a SubjectRef naming any other tenant is refused with
// ErrErasureTenantMismatch, both before any participant is called and
// before any system context is entered -- a bare or mis-scoped ctx must
// never become a license to pick another tenant for an operation that is,
// by design, irreversible.
//
// subject.TenantID and subject.SubjectID must both be non-empty; an empty
// either fails closed with ErrEmptySubjectRef before any participant is
// called.
//
// # Partial failure
//
// Every registered participant's own dbkit.Repository[T].HardDelete call
// happens in that participant's own database transaction -- there is no
// way to compose them into one cross-module transaction, since
// participants' tables routinely live in different modules and,
// deployment-mode permitting, different physical databases (this
// repository's own "no cross-module foreign keys" rule is the same
// reasoning: independently released modules cannot share a commit). Erase
// therefore treats one participant's failure exactly like SweepTenant
// does: every other participant still runs, the failure is recorded in
// the returned ErasureResult.Errors, and Erase returns ErrErasurePartialFailure
// (never a bare nil error, and never a nil ErasureResult) whenever
// ErasureResult.HasErrors() is true -- callers inspect ErasureResult.Errors
// for exactly which participants need a retry.
//
// Retrying is the documented recovery path, not a rollback: every
// participant's Erase callback is required (pkgcore.RetentionParticipant's
// doc comment) to return (0, nil) for a subject it has already fully
// erased, so calling Erase again with the same subject after a partial
// failure converges the remaining participants to completion without
// re-erasing, and re-emitting, what already succeeded as a duplicate.
//
// The whole request, once every participant has run, is recorded as
// exactly one AuditActionErasureRequest audit event via dbkit/audit.Emit,
// with Resource naming the subject and Changes carrying the full per-
// participant erased/error breakdown. A failure to publish that audit
// event is reported by wrapping ErrAuditRecordFailed -- see that error's
// own doc comment for why it is surfaced rather than swallowed.
func (s *ErasureService) Erase(ctx context.Context, subject pkgcore.SubjectRef, requestedBy pkgcore.Actor) (ErasureResult, error) {
	if subject.TenantID == "" || subject.SubjectID == "" {
		return ErasureResult{}, ErrEmptySubjectRef
	}

	// The ctx tenant is the tenant an erasure may ever touch: every
	// participant's Erase callback below hard-deletes rows through it, so
	// subject.TenantID may only echo it back. A bare ctx must never become
	// a license to pick any tenant for an irreversible operation -- refuse
	// it fail-closed, mirroring Export's identical no-tenant handling (a
	// raw pkgcore.MustTenantFromContext error, unwrapped, is this module's
	// established no-tenant idiom), and refuse a mismatch outright before
	// any participant is called and before any system context is entered.
	ctxTenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return ErasureResult{}, err
	}
	if ctxTenant != subject.TenantID {
		return ErasureResult{}, ErrErasureTenantMismatch.
			WithParam("ctx_tenant", string(ctxTenant)).
			WithParam("subject_tenant", string(subject.TenantID))
	}

	actor := requestedBy
	if actor.Type == "" {
		actor.Type = pkgcore.ActorTypeSystem
	}
	if actor.ID == "" {
		actor.ID = erasureFallbackActorID
	}
	actorID := actor.ID

	// ctx already carries subject.TenantID -- the gate above guarantees
	// the two agree -- so no WithTenant re-scope happens here: the
	// context's tenant is the single data boundary, never overwritten
	// from the SubjectRef.
	ctx = pkgcore.WithActor(ctx, actor)
	sysCtx, err := tenancy.WithSystemContext(ctx, s.bus, pkgcore.SystemReason{
		Actor:   actorID,
		Purpose: SystemPurposeRightToErasure,
	})
	if err != nil {
		return ErasureResult{}, err
	}

	result := ErasureResult{
		Subject: subject,
		Erased:  make(map[string]int),
		Errors:  make(map[string]error),
	}
	for _, p := range s.retention.Participants() {
		if p.Erase == nil {
			continue
		}
		erased, err := p.Erase(sysCtx, subject)
		result.Erased[p.Name] = erased
		if err != nil {
			// The participant's reported count is still recorded: a
			// callback that failed part-way through has already
			// hard-deleted erased rows (pkgcore.RetentionParticipant's
			// own contract, and the count this module's testutil
			// participants report on a mid-loop failure), and
			// TotalErased -- and the audit event's Changes["erased"]
			// breakdown -- must count rows that are genuinely gone, not
			// silently drop them from the record of an irreversible
			// operation because the callback also errored.
			result.Errors[p.Name] = err
		}
	}

	if err := s.emitErasureAudit(ctx, result); err != nil {
		return result, ErrAuditRecordFailed.WithCause(err)
	}
	if result.HasErrors() {
		return result, ErrErasurePartialFailure.WithParam("participants", erasureFailureReason(result))
	}
	return result, nil
}

// emitErasureAudit records one AuditActionErasureRequest event for a
// completed request, using dbkit/audit.Emit against the pre-elevation
// ctx -- see RetentionService.emitSweepAudit's identical convention.
func (s *ErasureService) emitErasureAudit(ctx context.Context, result ErasureResult) error {
	changes := map[string]any{"erased": result.Erased}
	if result.HasErrors() {
		errs := make(map[string]string, len(result.Errors))
		for name, err := range result.Errors {
			errs[name] = err.Error()
		}
		changes["errors"] = errs
	}
	return audit.Emit(ctx, s.bus, s.actions, audit.Input{
		Action: AuditActionErasureRequest,
		Resource: audit.Resource{
			Type: "compliance.subject",
			ID:   result.Subject.SubjectID,
		},
		Result: audit.Result{
			Success:       !result.HasErrors(),
			FailureReason: erasureFailureReason(result),
		},
		Changes: &audit.Diff{After: changes},
	})
}

// erasureFailureReason renders a short summary of which participants
// failed an erasure request, for audit.Result.FailureReason. Empty when
// result has no errors.
func erasureFailureReason(result ErasureResult) string {
	if !result.HasErrors() {
		return ""
	}
	names := make([]string, 0, len(result.Errors))
	for name := range result.Errors {
		names = append(names, name)
	}
	sort.Strings(names)
	return fmt.Sprintf("participants failed: %s", strings.Join(names, ", "))
}
