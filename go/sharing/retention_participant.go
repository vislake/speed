package sharing

// This file owns the sharing module's contribution to the compliance
// retention mechanism (pkgcore/registry.go's RetentionParticipant): the
// participant Module.Register registers so a host's retention sweep --
// RetentionService's per-tenant, per-participant sweep, go/compliance --
// reaps access-log entries past the tenant's retention window.
//
// sharing_access_log is tenant data that nothing ever reaped: rule 4's
// trail ("every access is logged") grew without bound, and its entries
// record viewer IPs and user agents -- exactly the unbounded PII retention
// an operator's retention window exists to bound. The participant makes
// the sweep cover the log under the SAME operator-tunable window
// (ConfigDefaultRetentionWindow, go/compliance) every other participant's
// expired data follows, mirroring go/compliance's own export-manifests
// participant, which reaps stored objects past the window the same way
// ("expired data survives until the window the operator configured" is
// that participant's documented reading of the sweep's cutoff). The shape
// follows examples/reference-app/internal/notes/retention_participant.go
// -- the mechanism's first real business-module consumer -- with one
// deliberate difference: an access-log entry is LIVE data (it was never
// mark-deleted; the module has no soft-delete), so "expired" means its
// recorded access time has fallen at or before the sweep's cutoff, not
// that its row was soft-deleted before it.
//
// The Sweep callback hard-deletes one candidate row at a time through the
// repository's promoted dbkit.HardDelete -- never a hand-written DELETE,
// never a bulk statement -- running under the system-context-carrying,
// tenant-carrying ctx RetentionService supplies: HardDelete reads both
// from ctx and stays strictly tenant-bound even past its system-context
// gate, which is where the sweep's cross-tenant non-reaping property comes
// from (the tenant-scoped candidate listing is the first half of it).
//
// Erase states the explicit nothing-to-erase answer -- (0, nil) -- because
// no sharing row carries a subject attribution a right-to-erasure request
// could target: a Share stores no creator user id and an AccessLogEntry no
// viewer user id (the viewer is often anonymous by the module's own
// design, rule 5's outward-identical answers included), so "erase the
// rows of this subject" has no rows to name. Destroying a tenant's whole
// access log on one subject's request would destroy other subjects' and
// anonymous viewers' records -- the identical reasoning go/compliance's
// export-manifests participant gives for its own (0, nil) -- so the
// explicit declaration is the honest answer, and the erasure audit never
// reports full success over a silently skipped participant.
//
// Export stays nil (the optional callback): sharing contributes no data to
// compliance's portability manifests, because a share is a tenant's
// pointer to a resource, never the resource's bytes -- the data behind a
// share lives in the resource's own store, and a tenant's portability
// export already gathers that store's content through the store's own
// participant. The absence costs a missing share-manifest section, never
// a false success.

import (
	"context"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// AccessLogRetentionParticipantName is the pkgcore.RetentionParticipant
// Name under which sharing's access-log reaping appears in sweep results
// and audit records -- the owner module's Name plus the model, per the
// mechanism's "notes.note" convention. The module's Register reserves it,
// so a host that wanted to register its own sweep logic for the access log
// under this name would collide with the module's own (ErrDuplicate
// at Add time), exactly like compliance's reserved export-manifests name.
const AccessLogRetentionParticipantName = "sharing.access_log"

// accessLogSweepBatchSize caps how many expired entries one sweep's
// candidate listing may return (sweepAccessLog). The access log is the one
// table in this module nothing else ever reaps, so an unbounded listing
// would hand the sweep a tenant's entire expired history at once; the cap
// keeps both the sweep's memory and the length of any one pass bounded
// however long that history has grown, the batch loop deleting each
// listing before asking for the next.
const accessLogSweepBatchSize = 100

// NewAccessLogRetentionParticipant returns sharing's
// pkgcore.RetentionParticipant over repo -- see this file's doc comment
// for the mechanism in full. Module.Register registers it on the host
// registry's Retention seat during Register, so any host that boots
// sharing alongside go/compliance gets access-log coverage on its
// retention sweeps with no further wiring; a host that never runs
// retention sweeps is unaffected (the registration is inert until an
// orchestrator calls the callbacks).
func NewAccessLogRetentionParticipant(repo *AccessLogRepository) pkgcore.RetentionParticipant {
	return pkgcore.RetentionParticipant{
		Name: AccessLogRetentionParticipantName,
		Sweep: func(ctx context.Context, tenant pkgcore.TenantID, cutoff time.Time) (int, error) {
			return sweepAccessLog(ctx, repo, tenant, cutoff)
		},
		// Nothing subject-shaped lives in an access-log row (see the file
		// doc comment); the explicit (0, nil) is the pkgcore contract's own
		// nothing-to-erase answer, never a nil Erase -- nil is refused by
		// the registrar and would read as a silent skip in the erasure
		// audit.
		Erase: func(context.Context, pkgcore.SubjectRef) (int, error) {
			return 0, nil
		},
		// Export deliberately nil -- see the file doc comment.
	}
}

// sweepAccessLog reaps tenant's access-log entries whose recorded access
// time is at or before cutoff -- older than the tenant's retention window,
// the same "expired past the window" reading go/compliance's
// export-manifests participant gives its own sweep -- and reports how many
// rows it actually removed. See the file doc comment for why the entries
// are reaped by age rather than by a soft-delete marker.
//
// The reap runs in bounded batches (accessLogSweepBatchSize): each
// candidate listing returns at most that many oldest-first rows and the
// batch is hard-deleted before the next listing, so a tenant with an
// arbitrarily long expired history costs one bounded listing-and-delete
// pass per batch, never one listing of the whole set. The sweep ends when
// a listing comes back empty or shorter than a full batch.
//
// Retry convergence follows every participant's documented contract: each
// candidate is hard-deleted one row at a time, an entry a concurrent pass
// already removed between the listing and its own delete (dbkit's
// ErrRecordNotFound, whose `id` the row's own delete carries) is counted
// as removed-elsewhere rather than as a failure, and a re-run over a
// tenant whose past-cutoff entries are gone lists nothing and reports 0 --
// an interruption between batches leaves the remaining batches for the
// next run exactly as an interruption mid-batch always did.
func sweepAccessLog(ctx context.Context, repo *AccessLogRepository, tenant pkgcore.TenantID, cutoff time.Time) (int, error) {
	reaped := 0
	for {
		rows, err := repo.listOlderThan(ctx, cutoff, accessLogSweepBatchSize)
		if err != nil {
			return reaped, err
		}
		if len(rows) == 0 {
			return reaped, nil
		}
		for _, row := range rows {
			err := repo.HardDelete(ctx, row.ID)
			if dbkit.IsRecordNotFound(err) {
				// Already removed between the listing above and this delete --
				// convergence, never a partial failure.
				continue
			}
			if err != nil {
				return reaped, err
			}
			reaped++
		}
		if len(rows) < accessLogSweepBatchSize {
			// A short batch is the last one: everything older than cutoff
			// has been reaped, so the loop is over.
			return reaped, nil
		}
	}
}
