package aigateway

// image_job_store.go is the persistence half of the image-generation job
// handler's idempotency invariant, stated in full in image_gateway.go's
// own doc comment: per enqueued image-generation Job -- no matter how many
// times go/jobs re-runs its Handle -- there is at most one successful
// vendor call and at most one usage record. Without this table, a retry
// after a failure that landed after a successful ImageProvider call would
// call the vendor again -- billing the tenant again -- and record usage
// under a fresh random idempotency key no UsageRecorder could dedup.
//
// # Why a marker table, and not the output storage object itself
//
// The object-reference boundary (image_gateway.go's own doc comment) means
// the eventual output lives in go/storage as an Object -- but
// storage.ObjectService.Create always mints a fresh id (there is no
// caller-supplied-id create), so nothing about a storage Object can be
// addressed by this job's stable jobs.JobID ahead of time, and the
// go/storage row itself cannot serve as the "did the vendor already
// answer" marker. A dedicated, job-id-keyed table inside this module is
// therefore the mechanism the invariant is built on.
//
// # Claim BEFORE the vendor call, not just a marker written after it
//
// The marker is written in two steps -- a content-less "pending" claim
// INSERT before the vendor call, then a "generated" transition after it.
// A single "generated"-only write placed after the vendor answered would
// leave a window in which an ORDINARY transient failure of that one INSERT
// -- SQLITE_BUSY-class contention, not a crash -- would leave no durable
// row behind, so the queue's next retry would see no marker and call the
// vendor a SECOND time. Splitting the marker fixes this structurally
// rather than papering over the contention with an in-process retry loop,
// which is this repository's established answer to SQLITE_BUSY-class
// contention (go/storage/derive.go's own doc comment: "a retry converges
// once the racing writer clears", achieved by making the retried statement
// safe to redo, not by looping around a single attempt):
//
//  1. claimPending, an INSERT of a content-less "pending" row, runs FIRST,
//     before ImageProvider is ever called. Its own primary key (id is the
//     jobs.JobID) makes it a compare-and-swap: at most one caller's INSERT
//     can ever succeed for one job id, so two overlapping Handle calls for
//     the SAME job (a distributed-mode lease-expiry redelivery, or two
//     workers racing a poll -- the standalone in-process queue cannot
//     produce this, since it never starts a second Handle for a job
//     already running one, but nothing in this table's own shape depends
//     on that) can never both proceed to call the vendor. The loser sees
//     an ordinary duplicate-key answer and MUST refuse to call the vendor
//     at all (see ErrImageJobClaimInFlight).
//  2. If claimPending itself fails for any OTHER reason -- including the
//     transient contention discussed above -- no vendor call has happened
//     yet, so surfacing the error to go/jobs as an ordinary attempt
//     failure is always safe: a retry redoes the claim from a clean slate,
//     exactly like a provider call failing outright.
//  3. Only once claimPending has actually committed does
//     imageGenerateHandler.Handle call ImageProvider. A failure there
//     (network error, non-2xx, etc.) means no successful answer exists to
//     remember, so the handler releases the claim (releaseClaim, best
//     effort) before returning the error -- the next attempt starts fresh
//     rather than waiting out a stuck "pending" row.
//  4. Once the vendor answers successfully, markGenerated transitions the
//     SAME row from "pending" to "generated" in one guarded UPDATE ("WHERE
//     id = ? AND status = 'pending'"), storing the vendor's raw answer.
//     This is still, unavoidably, one write that must durably record "the
//     vendor already answered" -- and it is the one residual gap this
//     design does not eliminate; see "Accepted residual risk" below for
//     exactly how it is bounded and why bounding it (rather than an
//     in-process retry loop) is this file's deliberate choice.
//
// # Why a "pending" row is never resurrected once orphaned
//
// If markGenerated's own UPDATE fails, the row is left at "pending" with
// no way to tell, from the row alone, whether the vendor was ever actually
// called for it (the crash could have landed either before or after that
// call). Rather than guess -- which would mean sometimes calling the
// vendor a second time, exactly the bug this table exists to prevent --
// imageGenerateHandler.Handle's own "pending" branch always refuses to
// call the vendor again for a job whose marker is still "pending": it
// returns ErrImageJobClaimInFlight and lets go/jobs' own retry policy
// govern what happens next. A job stuck this way eventually dead-letters
// without ever billing the tenant twice, which is the safe failure mode --
// see "Accepted residual risk" below for the trade-off this makes.
//
// status is "pending" (claimed, no vendor answer recorded yet -- Content/
// MIME/etc. are all still zero), "generated" (the vendor answered;
// Content/MIME/ImageCount/Steps/ResolutionTier are its raw answer) or
// "completed" (go/storage durably holds the output and usage has been
// reported exactly once). There is deliberately no fourth "no attempt yet"
// state: the absence of a row for a job id IS that state.
//
// # Accepted residual risk
//
// Two narrow windows remain, both bounded to "the job never completes and
// eventually dead-letters", never "the vendor is billed twice":
//
//   - A crash between claimPending's own INSERT committing and
//     ImageProvider actually being called leaves a "pending" row with no
//     vendor call behind it at all. Nothing else will ever advance or
//     clean up that row (see the section above), so the job stalls at
//     ErrImageJobClaimInFlight until go/jobs exhausts its retries and
//     dead-letters it. Recovering it (the vendor genuinely was never
//     called) needs an operator to delete the stuck row by hand -- an
//     accepted, documented limitation, not a silent gap.
//   - A crash between the vendor answering and markGenerated's own UPDATE
//     committing leaves the identical "pending" row behind, this time with
//     a real vendor answer that is now unrecoverable in memory. The job
//     stalls the same way; the tenant's real-world vendor bill (charged by
//     the vendor's own out-of-band metering, not this package) is not
//     matched by an internal usage record, an accepted reconciliation gap
//     rather than the double-billing/double-recording failure this table
//     exists to prevent.
//
// Similarly, a crash between writeImageObject succeeding and markCompleted
// committing leaves the marker at "generated": the next attempt correctly
// skips the vendor call and usage recording (the table's actual
// invariant), but redoes writeImageObject, producing a second,
// orphaned-but-harmless go/storage object nothing ever references. This
// is a resource-cleanup concern, not a billing or usage-duplication one,
// and is accepted for the same reason as go/storage's own accepted orphan
// windows: fixing it would need an idempotent create at the storage
// layer, outside this module's scope.

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// The three states an ai_gateway_image_jobs row can be in. There is
// deliberately no fourth "no attempt yet" constant: the absence of a row
// for a job id IS that state (see this file's own doc comment).
const (
	// imageJobStatusPending means claimPending's INSERT has committed but
	// no vendor answer has been recorded yet -- Content/MIME/ImageCount/
	// Steps/ResolutionTier are all still zero. imageGenerateHandler.Handle
	// must never call ImageProvider for a job whose marker is already in
	// this state (see this file's own doc comment on why the row is never
	// resurrected).
	imageJobStatusPending = "pending"

	// imageJobStatusGenerated means ImageProvider already answered
	// successfully for this job -- Content/MIME/ImageCount/Steps/
	// ResolutionTier are its raw answer -- but go/storage does not yet
	// durably hold the output. A subsequent Handle attempt must reuse this
	// answer, never call the vendor again.
	imageJobStatusGenerated = "generated"

	// imageJobStatusCompleted means go/storage durably holds the output as
	// OutputObjectID and usage has been reported exactly once. Content is
	// cleared once a row reaches this status (see markCompleted). A
	// subsequent Handle attempt answers from this row alone -- no vendor
	// call, no storage write, no usage record.
	imageJobStatusCompleted = "completed"
)

// imageJobRow is one ai_gateway_image_jobs row: at most one per
// TaskTypeImageGenerate Job, keyed by its jobs.JobID.
//
// # Data domain and primary key
//
// Tenant data, implementing dbkit.TenantScoped through the embedded
// dbkit.TenantModel, reached only through imageJobRepository (which embeds
// dbkit.Repository[imageJobRow]): a job's marker is meaningless outside
// the tenant it was enqueued under. The primary key is (id) alone: id is
// jobs.JobID itself (already globally unique on its own, minted once by
// whichever Queue.Enqueue created the Job and never regenerated across a
// retry -- see jobs.Job.ID's own doc comment), so tenant_id needs no part
// in the key.
//
// Its own isolation is proven by tenancytest.AssertIsolated in
// image_job_store_test.go.
type imageJobRow struct {
	// ID is the owning Job's jobs.JobID, converted to string. Never
	// generated here -- imageGenerateHandler.Handle supplies it from the
	// *jobs.Job it was called with.
	ID string `gorm:"column:id;primaryKey;size:64"`

	// TenantModel promotes the tenant_id column and the GetTenantID method
	// that satisfies dbkit.TenantScoped.
	dbkit.TenantModel

	// Status is imageJobStatusPending, imageJobStatusGenerated or
	// imageJobStatusCompleted.
	Status string `gorm:"column:status;size:16;not null"`

	// Provider is the ImageProviderRegistry name that actually answered,
	// recorded so an attempt that skips the vendor call (Status is already
	// imageJobStatusGenerated) can still log which provider generated the
	// image without re-resolving a credential it no longer needs. Empty
	// while Status is imageJobStatusPending.
	Provider string `gorm:"column:provider;size:100;not null;default:''"`

	// Content and MIME are the vendor's raw answer (ImageBytes), kept only
	// until markCompleted clears them -- see this file's own doc comment
	// for why a "generated" row must carry the actual bytes, not just a
	// flag. Both are zero while Status is imageJobStatusPending.
	Content []byte `gorm:"column:content"`
	MIME    string `gorm:"column:mime;size:255;not null;default:''"`

	// ImageCount, Steps and ResolutionTier are ImageUsage's own fields,
	// stored as plain columns (ImageUsage is small and fixed-shape) rather
	// than a JSON blob.
	ImageCount     int    `gorm:"column:image_count;not null;default:0"`
	Steps          int    `gorm:"column:steps;not null;default:0"`
	ResolutionTier string `gorm:"column:resolution_tier;size:64;not null;default:''"`

	// OutputObjectID is the go/storage object id the generated image was
	// finally written as. Empty until Status is imageJobStatusCompleted.
	OutputObjectID string `gorm:"column:output_object_id;size:64;not null;default:''"`

	// CreatedAt and UpdatedAt are written by gorm's autoCreateTime/
	// autoUpdateTime conventions, never a database default -- the same
	// dual-dialect discipline every other table in this codebase follows.
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// TableName names the shared ai_gateway_image_jobs table.
func (imageJobRow) TableName() string { return "ai_gateway_image_jobs" }

// image reconstructs the ImageBytes a "generated" row was claimed with.
func (r imageJobRow) image() ImageBytes {
	return ImageBytes{Content: r.Content, MIME: r.MIME}
}

// usage reconstructs the ImageUsage a row (either status) was claimed or
// completed with.
func (r imageJobRow) usage() ImageUsage {
	return ImageUsage{ImageCount: r.ImageCount, Steps: r.Steps, ResolutionTier: r.ResolutionTier}
}

// imageJobRepository is the tenant-scoped data-access type for
// imageJobRow, mirroring go/storage's ObjectRepository exactly: it embeds
// *dbkit.Repository[imageJobRow] for the promoted Create/FindByID (get
// below wraps FindByID's not-found case), and keeps db alongside it only to
// compose the guarded, conditional updates below -- a query shape
// Repository[T]'s deliberately minimal surface cannot express (Repository's
// own Update is a full-record Save, not a "transition only if still in
// state X" compare-and-swap). Every query here runs against imageJobRow
// (a TenantScoped destination) inside dbkit.WithTenantSession, so the GORM
// isolation plugin still injects "WHERE tenant_id = ?" and the PostgreSQL
// RLS session variable is still set, exactly as for every promoted method
// -- and every conditional update below resolves its target table from the
// destination struct passed to Updates/Delete, exactly like
// dbkit.Repository[T].Restore's own guarded transition does, never through
// db.Table, db.Model-as-bypass or db.Raw.
type imageJobRepository struct {
	*dbkit.Repository[imageJobRow]
	db *gorm.DB
}

// newImageJobRepository returns an imageJobRepository backed by db. db is
// expected to already carry this module's migrations (the
// ai_gateway_image_jobs table) -- exactly the same expectation
// NewCredentialService's own db parameter carries, and in production the
// very same db: see gateway.go's NewGateway, which builds this repository
// from credentials' own connection rather than requiring a second db
// parameter of its own.
func newImageJobRepository(db *gorm.DB) *imageJobRepository {
	return &imageJobRepository{Repository: dbkit.NewRepository[imageJobRow](db), db: db}
}

// get returns jobID's marker row under ctx's tenant, or (nil, nil) when no
// row exists yet -- the "no attempt has reached the vendor yet" state.
// imageGenerateHandler.Handle calls this FIRST, before anything else, on
// every attempt.
func (r *imageJobRepository) get(ctx context.Context, jobID string) (*imageJobRow, error) {
	row, err := r.FindByID(ctx, jobID)
	if err != nil {
		// dbkit.ErrRecordNotFound is derived fresh by WithParam on every
		// return (its own doc comment), so it can never be recognized with
		// errors.Is or == -- compare its Code through hasCode instead, the
		// same convention this module's own errors.go documents for this
		// exact sentinel.
		if hasCode(err, dbkit.ErrRecordNotFound.Code) {
			return nil, nil
		}
		return nil, err
	}
	return row, nil
}

// claimPending atomically claims jobID for THIS attempt by inserting a
// content-less "pending" row, transitioning from "no row" to
// imageJobStatusPending. imageGenerateHandler.Handle calls this before
// ImageProvider is ever invoked -- see this file's own doc comment for why
// this ordering, not a marker written after the vendor call, is what
// closes both the transient-write-failure window and the concurrent-claim
// race a marker-after-the-fact design cannot.
//
// claimed is true only when THIS call's own INSERT committed -- the caller
// then owns the job and must proceed to call the vendor. claimed is false
// with err nil when the primary-key INSERT found a row already there (a
// concurrent claim, whatever its current status): the caller must not call
// the vendor and should treat this exactly like the imageJobStatusPending
// branch of an already-fetched marker (see image_gateway.go's Handle).
// A non-nil err is any OTHER database failure -- including the exact
// transient contention this file's own doc comment discusses -- and is
// always safe to surface to the caller as an ordinary attempt failure: no
// vendor call has happened yet.
func (r *imageJobRepository) claimPending(ctx context.Context, jobID string) (claimed bool, err error) {
	row := imageJobRow{ID: jobID, Status: imageJobStatusPending}
	if createErr := r.Create(ctx, &row); createErr != nil {
		if errors.Is(createErr, gorm.ErrDuplicatedKey) {
			return false, nil
		}
		return false, createErr
	}
	return true, nil
}

// releaseClaim best-effort deletes jobID's marker row, but only while it is
// still imageJobStatusPending -- imageGenerateHandler.Handle calls this
// after claimPending succeeded but something before a successful vendor
// answer failed (route/credential resolution, reading an input/mask
// object, or the vendor call itself), so the next attempt starts from a
// clean slate instead of being permanently refused by
// ErrImageJobClaimInFlight. The status guard means releaseClaim can never
// remove a row a concurrent or later write has already advanced past
// pending, even though only the attempt that owns the claim is expected to
// call this in practice.
//
// A releaseClaim failure is deliberately not escalated to the caller: it is
// called only while an error is already about to be returned, and the
// row's own accepted residual-risk section (this file's own doc comment)
// already covers a "pending" row that never gets cleaned up.
func (r *imageJobRepository) releaseClaim(ctx context.Context, jobID string) error {
	return dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.
			Where("id = ? AND status = ?", jobID, imageJobStatusPending).
			Delete(&imageJobRow{}).Error
	})
}

// markGenerated transitions jobID's marker from imageJobStatusPending to
// imageJobStatusGenerated, storing the vendor's answer -- provider is the
// ImageProviderRegistry name that actually answered, img/usage its raw
// result. imageGenerateHandler.Handle calls this immediately after a
// provider call returns success, before attempting anything else that
// could still fail (writeImageObject) -- see this file's own doc comment
// for why this write, not the eventual storage write, is what closes the
// double-billing window, and for the one residual gap (a failure of this
// very UPDATE) this design accepts rather than eliminates.
//
// The update is expressed as tx.Where(...).Select(...).Updates(&imageJobRow{...})
// -- gorm resolves the target table from the struct passed to Updates, the
// identical pattern dbkit.Repository[T].Restore's own guarded transition
// uses, so this file never reaches for db.Table, db.Model or db.Raw. Select
// names every column this transition must write, including zero-valued
// ones (Steps can genuinely be 0 for an operation the vendor does not
// report diffusion steps for): gorm's struct-based Updates otherwise
// silently omits a zero-valued field, which would leave a previous value
// in place -- never a concern here in practice (the row was pending, so
// every one of these columns already held its zero value), but named
// explicitly rather than relied upon.
//
// An error here (including the exact transient contention this file's own
// doc comment discusses) is returned to the caller unmodified -- this
// package deliberately does not wrap it in an in-process retry loop, the
// same choice go/storage/derive.go's own doc comment documents for the
// identical class of SQLite contention. rowsAffected == 0 with no error
// reported (translated here into ErrInternal) would mean the row was not
// still "pending" at update time, which should never happen given Handle's
// own call discipline -- surfaced rather than silently ignored regardless.
func (r *imageJobRepository) markGenerated(ctx context.Context, jobID, provider string, img ImageBytes, usage ImageUsage) error {
	m := imageJobRow{
		Status:         imageJobStatusGenerated,
		Provider:       provider,
		Content:        img.Content,
		MIME:           img.MIME,
		ImageCount:     usage.ImageCount,
		Steps:          usage.Steps,
		ResolutionTier: usage.ResolutionTier,
		UpdatedAt:      time.Now().UTC(),
	}
	var rowsAffected int64
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		res := tx.
			Where("id = ? AND status = ?", jobID, imageJobStatusPending).
			Select("Status", "Provider", "Content", "MIME", "ImageCount", "Steps", "ResolutionTier", "UpdatedAt").
			Updates(&m)
		rowsAffected = res.RowsAffected
		return res.Error
	})
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return ErrInternal.WithParam("job_id", jobID).WithParam("reason", "image job marker was not pending at markGenerated time")
	}
	return nil
}

// markCompleted transitions jobID's marker from imageJobStatusGenerated to
// imageJobStatusCompleted now that outputObjectID is durably written to
// go/storage, clearing the now-unneeded Content bytes in the same write,
// and reports whether THIS call performed the transition (rowsAffected of
// the guarded "WHERE status = imageJobStatusGenerated" update). false means
// a concurrent or earlier attempt already completed this job (or, which
// should never happen given Handle's own call order, that no "generated"
// row exists at all) -- imageGenerateHandler.Handle uses that boolean as
// the gate for calling recordImageUsage, so usage is reported at most once
// per job ever, independent of whatever a wired UsageRecorder itself does
// or does not dedup on IdempotencyKey (usage idempotency that does not
// rely on the marker's existence alone).
//
// The update is expressed as tx.Where(...).Select(...).Updates(&imageJobRow{...})
// against imageJobRow -- a TenantScoped destination -- inside
// dbkit.WithTenantSession, so the isolation plugin still injects the tenant
// filter and forbids moving the row to a different tenant; gorm resolves
// the target table from the struct passed to Updates (the identical
// pattern dbkit.Repository[T].Restore's own guarded transition uses), so
// this file never hand-writes a tenant_id filter and never reaches for
// db.Table, db.Model-as-bypass or db.Raw.
func (r *imageJobRepository) markCompleted(ctx context.Context, jobID, outputObjectID string) (bool, error) {
	m := imageJobRow{
		Status:         imageJobStatusCompleted,
		OutputObjectID: outputObjectID,
		Content:        nil,
		UpdatedAt:      time.Now().UTC(),
	}
	var rowsAffected int64
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		res := tx.
			Where("id = ? AND status = ?", jobID, imageJobStatusGenerated).
			Select("Status", "OutputObjectID", "Content", "UpdatedAt").
			Updates(&m)
		rowsAffected = res.RowsAffected
		return res.Error
	})
	if err != nil {
		return false, err
	}
	return rowsAffected > 0, nil
}
