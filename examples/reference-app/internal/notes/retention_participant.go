package notes

import (
	"context"
	"time"

	"github.com/vislake/speed/go/pkgcore"
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
				if err := repo.HardDelete(ctx, row.ID); err != nil {
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
				if err := repo.HardDelete(ctx, row.ID); err != nil {
					return erased, err
				}
				erased++
			}
			return erased, nil
		},
		Export: func(ctx context.Context, _ pkgcore.TenantID) (any, error) {
			// Export runs with no system context (see export.go) and lists
			// live notes only -- data-portability gathering exports what the
			// tenant can see, never rows a soft-delete already hid.
			return repo.List(ctx)
		},
	}
}
