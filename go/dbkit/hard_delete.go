package dbkit

import (
	"context"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// ErrHardDeleteRequiresSystemContext is returned by Repository[T].HardDelete
// when ctx carries no system context (pkgcore.SystemReasonFromContext
// reports none). A physical DELETE is irreversible, so the method refuses to
// run on an ordinary tenant-scoped context: whoever requests one must have
// gone through pkgcore.WithSystemContext (or, in code that may import it,
// the tenancy module's audited WithSystemContext wrapper) first. The
// apperr.Invalid family matches ErrNotSoftDeletable and the other
// mechanism-level caller errors in this package — dbkit does no
// authorization, so this is not a Forbidden: it reports a misuse of the
// data-access API, and the whitelist that decides who may hold a system
// context at all is enforced elsewhere (see HardDelete's doc comment).
var ErrHardDeleteRequiresSystemContext = apperr.Invalid("dbkit.hard_delete_requires_system_context")

// HardDelete physically removes the row with the given id, scoped to the
// ctx tenant — an irreversible DELETE, never a mark-delete. Even a T
// implementing SoftDeletable is removed outright, soft-deleted or not:
// soft_delete.go's auto-scope is query-only (it registers Before
// "gorm:query", nothing else), so a soft-deleted row is just as deletable
// as a live one, and this method never consults SoftDeletable at all — a
// soft-deleted row is not a security boundary, and HardDelete is the
// repository path that actually erases it (docs/internal/04-data-and-tenancy.md's
// delete-semantics section, §3). It returns ErrRecordNotFound, not a
// generic gorm error and not a silent no-op success, when nothing matches —
// including when id exists under a different tenant. No migration is
// involved: HardDelete issues the same physical DELETE Delete always issued
// for a non-SoftDeletable T, so it changes no DDL and no table structure.
//
// HardDelete is the restricted, irreversible half of the delete semantics,
// the sequential-next phase after the mark-delete round. Its two intended
// callers are retention-expiry cleanup and compliance right-to-erasure
// (the M4 compliance module), both acting through a whitelisted module;
// it exists for rows that must actually be gone — rows whose SoftDeletable
// mark alone is not enough.
//
// The system-context gate is a necessary condition, never a sufficient
// one, and dbkit checks only the presence of a system context, never who
// holds it. The caller-side obligations live outside this package: the
// whitelist of modules allowed to hold a system context (admin, compliance,
// jobs, authn — enforced by code review and by the doc comments on the
// WithSystemContext functions, not by dbkit), and the audited
// tenancy.WithSystemContext entry every grant should go through so the
// grant itself is recorded. HardDelete adds no authorization of its own
// beyond the gate.
//
// The tenant in ctx stays mandatory and stays binding. A system context
// never substitutes for a tenant — a context carrying one but no tenant
// fails closed with pkgcore.ErrNoTenant before the database is touched —
// and never widens the delete beyond the ctx tenant's own rows: the
// tenant-scoped DELETE below runs with the ctx tenant both in the WHERE
// clause and in WithTenantSession's session, so HardDelete is not the
// cross-tenant escape hatch and never deletes across tenants, system
// context or not.
//
// Under dbkit's audit capture (audit_capture.go's Auditable marker plus
// Options.AuditBus), the physical DELETE is captured automatically as
// Operation "delete" with After nil, exactly like Delete's physical branch
// — the row is gone, so there is no after-image to record. Never hand-emit
// a duplicate Delete event after a successful HardDelete: the capture
// plugin already published one, and a second, hand-written event would
// double-count the deletion in the audit trail.
//
// One obligation the gate does not discharge is attribution. The capture
// reads the event's Actor from pkgcore.ActorFromContext on the write's
// context, and a system context never supplies one: pkgcore.WithSystemContext
// stores only the SystemReason — whose Actor is a bare string naming who
// the grant was for, never promoted into the structured Actor carrier —
// and tenancy's audited wrapper returns exactly that context. A caller
// erasing under system context must therefore layer pkgcore.WithActor
// (plus pkgcore.WithOnBehalfOf when the erase is performed under
// impersonation, per the dual-identity rule) before entering it, or the
// erasure record — the one audit record whose subject row is about to
// cease to exist — lands attributed to the zero Actor.
// TestAuditCapturePlugin_HardDelete_SystemContextAlone_DoesNotAttribute
// (audit_capture_test.go) pins the mechanism behind this warning.
//
// When ctx carries no system context, HardDelete returns
// ErrHardDeleteRequiresSystemContext before the database is touched at
// all; when ctx carries one but no tenant, it returns pkgcore's error
// unmodified, also before the database is touched.
func (r *Repository[T]) HardDelete(ctx context.Context, id string) error {
	var zero T
	if _, ok := pkgcore.SystemReasonFromContext(ctx); !ok {
		return ErrHardDeleteRequiresSystemContext
	}

	// From here on this body must stay byte-identical to Delete's physical
	// branch (repository.go, the branch every non-SoftDeletable T takes):
	// the same tenant resolution, the same WithTenantSession transaction,
	// the same physical DELETE, the same ErrRecordNotFound on zero rows
	// affected. HardDelete is that branch behind the gate above — it
	// differs from Delete only in the gate and in never consulting
	// SoftDeletable. Do not extract a shared helper: the two branches
	// deliberately mirror each other, on the same per-file duplication
	// convention the column constants in this package already follow.
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return err
	}

	var rowsAffected int64
	err = WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		res := tx.
			Where(idColumn+" = ?", id).
			Where(tenantIDColumn+" = ?", tenant).
			Delete(&zero)
		rowsAffected = res.RowsAffected
		return res.Error
	})
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return ErrRecordNotFound.WithParam("id", id)
	}
	return nil
}
