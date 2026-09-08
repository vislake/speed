package notes

import (
	"context"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// NewRetentionParticipant returns notes' pkgcore.RetentionParticipant --
// the owning module's deliberate opt-in to the compliance retention
// mechanism (pkgcore/registry.go's RetentionParticipant doc comment):
// RetentionService's per-tenant sweep hard-deletes this participant's
// soft-deleted notes past the retention cutoff, ErasureService's
// right-to-erasure erases every note one creator-subject owns, and
// ExportService's data-portability gathering includes the tenant's live
// notes.
//
// The shape mirrors go/compliance's own internal/testutil
// NewParticipant fixture exactly, standing in the same three callbacks
// over this module's real Repository -- which is what makes this file the
// mechanism's first real business-module consumer (see
// go/compliance/AGENTS.md's "Known limitations" for what that discharges):
// every write below is a promoted dbkit.Repository[Note].HardDelete call,
// one row at a time, never a hand-written DELETE and never a bulk
// statement, running under the system-context-carrying, tenant-carrying
// ctx compliance's orchestrators supply -- HardDelete reads both from ctx
// (see go/dbkit/hard_delete.go) and stays strictly tenant-bound even past
// its system-context gate, which is where the cross-tenant non-erasure
// property of an Erase comes from at this layer.
//
// The host (cmd/server's buildServer) constructs this participant with a
// Repository over the very dbkit.Open *gorm.DB the notes Module already
// uses -- share the connection, never a second pool -- and registers it on
// the kernel's pkgcore.Registry.Retention seat during Bootstrap.
//
// Both destructive callbacks below tolerate a HardDelete that answers
// record-not-found by counting the row as removed-elsewhere rather than
// failing: a sweep and an on-demand erasure can legitimately converge on
// the same rows (a soft-deleted note past the retention cutoff is exactly
// what both target), and whichever orchestrator deletes a row first makes
// the other's HardDelete -- issued after its own candidate list was
// gathered -- answer ErrRecordNotFound. Reporting that as a failure would
// surface compliance's ErrSweepPartialFailure / ErrErasurePartialFailure
// for data that is in fact gone, which is convergence, not a partial
// failure -- the identical tolerance pkgcore.RetentionParticipant's Erase
// contract documents ("a participant with no data for subject returns
// (0, nil) -- never an error -- so that re-running an erasure already
// partially applied elsewhere converges instead of failing forever"),
// applied to the mid-loop race the contract's own (0, nil) empty-list case
// cannot cover. A row this callback did NOT remove is never counted in its
// result, and any other HardDelete error stays a genuine failure.
func NewRetentionParticipant(repo *Repository) pkgcore.RetentionParticipant {
	return pkgcore.RetentionParticipant{
		Name: "notes.note",
		Sweep: func(ctx context.Context, _ pkgcore.TenantID, cutoff time.Time) (int, error) {
			rows, err := repo.listSoftDeletedBefore(ctx, cutoff)
			if err != nil {
				return 0, err
			}
			reaped := 0
			for _, row := range rows {
				err := repo.HardDelete(ctx, row.ID)
				if hardDeleteSaysGone(err) {
					// Already removed between the list above and this
					// delete -- see hardDeleteSaysGone's doc comment.
					continue
				}
				if err != nil {
					return reaped, err
				}
				reaped++
			}
			return reaped, nil
		},
		Erase: func(ctx context.Context, subject pkgcore.SubjectRef) (int, error) {
			// A note's subject attribution is its CreatorUserID (see
			// model.go): the value the SubjectResolver resolved at create
			// time. Erase targets every note of ctx's tenant -- ErasureService
			// has already re-scoped ctx to subject.TenantID -- that this
			// creator owns, live or soft-deleted: a right-to-erasure
			// request bypasses the retention window entirely. A creator
			// with no notes at all yields an empty list and (0, nil) --
			// re-running an erasure already applied elsewhere converges
			// instead of failing.
			rows, err := repo.listByCreator(ctx, subject.SubjectID)
			if err != nil {
				return 0, err
			}
			erased := 0
			for _, row := range rows {
				err := repo.HardDelete(ctx, row.ID)
				if hardDeleteSaysGone(err) {
					// Already removed between the list above and this
					// delete -- see hardDeleteSaysGone's doc comment.
					continue
				}
				if err != nil {
					return erased, err
				}
				erased++
			}
			return erased, nil
		},
		Export: func(ctx context.Context, _ pkgcore.TenantID) (any, error) {
			// Export runs with no system context (see export.go) and lists
			// live notes only -- data-portability gathering exports what the
			// tenant can see, never rows a soft-delete already hid. The
			// field-level judgment pkgcore's Export contract requires is
			// stated here too: whole Note rows, Text included, are exactly
			// what this manifest should carry. A note body is the tenant's
			// own user content, and the export's recipient -- the holder of
			// the unauthenticated, single-view share link the manifest
			// travels (pkgcore/registry.go's Export doc) -- is by that link
			// entitled to read the export's data. Text's audit:"redact" tag
			// confines the body within the audit trail's own capture
			// (model.go: the append-only platform table the body must never
			// reach verbatim); it is not a statement about the tenant's own
			// portability export and does not follow the data beyond that
			// mechanism.
			return repo.List(ctx)
		},
	}
}

// hardDeleteSaysGone reports whether err is the "the row is already gone"
// answer a Repository.HardDelete gives when its physical DELETE matched no
// row -- whether because the row never existed under ctx's tenant, or
// because a concurrent removal (the other compliance orchestrator, or a
// re-run of this one) got there between this callback's candidate list and
// its own delete. It is matched by Code rather than by identity
// (apperr.WithParam always derives a new *apperr.Error, so pointer
// identity is not stable across the decoration the underlying repository
// applies), the same way service.go's isRecordNotFound helper matches.
func hardDeleteSaysGone(err error) bool {
	if err == nil {
		return false
	}
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == dbkit.ErrRecordNotFound.Code
}
