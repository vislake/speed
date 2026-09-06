package aigateway

// image_job_store.go is the persistence half of this round's fix for a
// real, audited bug: imageGenerateHandler.Handle (image_gateway.go) used to
// have no memory of a previous attempt at all, so a go/jobs retry after a
// failure that landed AFTER a successful ImageProvider call re-called the
// vendor -- billing the tenant again -- and re-recorded usage under a fresh
// random idempotency key, which no UsageRecorder could dedup. See this
// module's AGENTS.md for the concrete scenario that hits this in practice
// (the SQLITE_BUSY contention entry) and image_gateway.go's own doc comment
// for the invariant this table now enforces: at most one successful vendor
// call, and at most one usage record, per enqueued image-generation Job --
// no matter how many times go/jobs re-runs its Handle.
//
// # Why a marker table, and not the output storage object itself
//
// The design doc's object-reference boundary (image_gateway.go's own doc
// comment) means the eventual output lives in go/storage as an Object --
// but storage.ObjectService.Create always mints a fresh id (there is no
// caller-supplied-id create), so nothing about a storage Object can be
// addressed by this job's stable jobs.JobID ahead of time, and the
// go/storage row itself cannot serve as the "did the vendor already
// answer" marker without a storage-side change this round's task
// instruction rules out ("no changes outside go/ai-gateway"). A dedicated,
// job-id-keyed table inside this module is therefore the only mechanism
// this round's real code can build the invariant on.
//
// # Why the marker is written BEFORE the storage write, not after
//
// A marker written only once the full pipeline (including the go/storage
// write) has succeeded would not close the bug at all -- it would only
// shrink the window, since the exact failure this bug is about is a
// storage write that fails AFTER the vendor already answered. Instead,
// imageGenerateHandler.Handle persists a "generated" marker -- the
// vendor's raw answer, verbatim -- in the same step where it just learned
// the vendor call succeeded, before attempting anything that could still
// fail (writeImageObject). A subsequent attempt for the same job consults
// this table FIRST: a "generated" row means the vendor answer already
// exists and must be reused, never re-requested; a "completed" row means
// the whole job already finished and Handle can answer from the row alone,
// without touching ImageProvider or go/storage again.
//
// # Accepted residual risk
//
// The one write this cannot make atomic with the vendor call itself is the
// marker insert (claimGenerated) that follows it -- a crash in the narrow
// gap between "the vendor answered" and "claimGenerated's own INSERT
// committed" is still a real, if far smaller, window than the one this
// table closes: unlike go/storage's multi-step Create/Upload/Complete
// transfer protocol (the actual, observed source of the SQLITE_BUSY
// contention this fix targets), claimGenerated is one INSERT into this
// module's own table, sharing none of storage's read-then-write-upgrade
// lock pattern. A failure of that single INSERT is reported to Handle's
// caller (the queue) as an ordinary attempt failure, exactly like any
// other error before a marker exists -- so a retry safely calls the vendor
// again, which is correct: nothing durable recorded that the vendor had
// already answered. This residual window is documented, not eliminated,
// per this round's own task instruction ("closes the window, not just
// shrinks it" is asked of the storage-write failure this bug was filed
// for, not of every conceivable failure in the universe).
//
// Similarly, a crash between writeImageObject succeeding and markCompleted
// committing leaves the marker at "generated": the next attempt correctly
// skips the vendor call and usage recording (this fix's actual invariant),
// but redoes writeImageObject, producing a second, orphaned-but-harmless
// go/storage object nothing ever references. This is a resource-cleanup
// concern, not a billing or usage-duplication one, and is accepted for the
// same reason: fixing it would need an idempotent create at the storage
// layer, out of this round's scope.

import (
	"context"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// The two states an ai_gateway_image_jobs row can be in. There is
// deliberately no third "no attempt yet" constant: the absence of a row
// for a job id IS that state (see this file's own doc comment).
const (
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
// Tenant data (docs/internal/04-data-and-tenancy.md), implementing
// dbkit.TenantScoped through the embedded dbkit.TenantModel, reached only
// through imageJobRepository (which embeds dbkit.Repository[imageJobRow])
// -- the identical shape go/storage's own Object follows, and for the
// identical reason: a job's marker is meaningless outside the tenant it
// was enqueued under. The primary key is (id) alone: id is jobs.JobID
// itself (already globally unique on its own, minted once by whichever
// Queue.Enqueue created the Job and never regenerated across a retry --
// see jobs.Job.ID's own doc comment), so tenant_id needs no part in the
// key, mirroring Object's own id-alone-key rationale (go/storage/model.go).
type imageJobRow struct {
	// ID is the owning Job's jobs.JobID, converted to string. Never
	// generated here -- imageGenerateHandler.Handle supplies it from the
	// *jobs.Job it was called with.
	ID string `gorm:"column:id;primaryKey;size:64"`

	// TenantModel promotes the tenant_id column and the GetTenantID method
	// that satisfies dbkit.TenantScoped.
	dbkit.TenantModel

	// Status is imageJobStatusGenerated or imageJobStatusCompleted.
	Status string `gorm:"column:status;size:16;not null"`

	// Provider is the ImageProviderRegistry name that actually answered,
	// recorded so an attempt that skips the vendor call (Status is already
	// imageJobStatusGenerated) can still log which provider generated the
	// image without re-resolving a credential it no longer needs.
	Provider string `gorm:"column:provider;size:100;not null;default:''"`

	// Content and MIME are the vendor's raw answer (ImageBytes), kept only
	// until markCompleted clears them -- see this file's own doc comment
	// for why a "generated" row must carry the actual bytes, not just a
	// flag.
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
// compose markCompleted's guarded, conditional update -- a query shape
// Repository[T]'s deliberately minimal surface cannot express (Repository's
// own Update is a full-record Save, not a "transition only if still in
// state X" compare-and-swap). Every query here runs against imageJobRow
// (a TenantScoped destination) inside dbkit.WithTenantSession, so the GORM
// isolation plugin still injects "WHERE tenant_id = ?" and the PostgreSQL
// RLS session variable is still set, exactly as for every promoted method
// -- this file never hand-writes a tenant_id filter and never reaches for
// db.Table/db.Model-as-bypass/db.Raw.
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
		// same convention this module's own errors.go documents and
		// go/storage's/go/rbac's identical helpers already use for this
		// exact sentinel.
		if hasCode(err, dbkit.ErrRecordNotFound.Code) {
			return nil, nil
		}
		return nil, err
	}
	return row, nil
}

// claimGenerated persists that provider already answered img/usage
// successfully for jobID, transitioning from "no row" to
// imageJobStatusGenerated. imageGenerateHandler.Handle calls this
// immediately after a provider call returns success and before attempting
// anything else -- see this file's own doc comment for why this ordering,
// not the eventual storage write, is what closes the double-billing
// window, and for the one residual gap (a failure of this very INSERT)
// this design accepts rather than eliminates.
func (r *imageJobRepository) claimGenerated(ctx context.Context, jobID, provider string, img ImageBytes, usage ImageUsage) error {
	row := imageJobRow{
		ID:             jobID,
		Status:         imageJobStatusGenerated,
		Provider:       provider,
		Content:        img.Content,
		MIME:           img.MIME,
		ImageCount:     usage.ImageCount,
		Steps:          usage.Steps,
		ResolutionTier: usage.ResolutionTier,
	}
	return r.Create(ctx, &row)
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
// or does not dedup on IdempotencyKey (this is this fix's option (c): usage
// idempotency that does not rely on the marker's existence alone).
//
// The update is expressed as tx.Model(&imageJobRow{}).Where(...).Updates(...)
// against imageJobRow -- a TenantScoped destination -- inside
// dbkit.WithTenantSession, so the isolation plugin still injects the tenant
// filter and forbids moving the row to a different tenant; this file never
// hand-writes "tenant_id = ?" itself.
func (r *imageJobRepository) markCompleted(ctx context.Context, jobID, outputObjectID string) (bool, error) {
	var rowsAffected int64
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		res := tx.Model(&imageJobRow{}).
			Where("id = ? AND status = ?", jobID, imageJobStatusGenerated).
			Updates(map[string]any{
				"status":           imageJobStatusCompleted,
				"output_object_id": outputObjectID,
				"content":          nil,
				"updated_at":       time.Now().UTC(),
			})
		rowsAffected = res.RowsAffected
		return res.Error
	})
	if err != nil {
		return false, err
	}
	return rowsAffected > 0, nil
}
