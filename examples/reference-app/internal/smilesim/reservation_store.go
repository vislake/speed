package smilesim

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// creditReservationsTable is ReservationStore's persisted table name.
const creditReservationsTable = "smilesim_credit_reservations"

// createCreditReservationsTableSQL is executed imperatively, with a plain
// CREATE TABLE IF NOT EXISTS, the same bootstrapping pattern go/jobs' own
// jobRecord uses for its jobsTable (see go/jobs/store.go's
// createJobsTableSQL doc comment) rather than going through
// dbkit.MigrationRegistry's cross-module, Atlas-generated, versioned
// migration machinery: this table is an implementation detail specific to
// this one reference-app package's own bookkeeping, with no other
// consumer and nothing shipped to a consuming project, so routing it
// through the machinery built for schema that ships and evolves across
// modules would be disproportionate. The statement is written to be
// portable across both dbkit dialects anyway (VARCHAR/TIMESTAMP,
// application-generated ids, no PostgreSQL- or SQLite-specific syntax),
// matching the backend coding standard's dual-dialect rule, even though
// only SQLite is exercised by this app today.
const createCreditReservationsTableSQL = `CREATE TABLE IF NOT EXISTS ` + creditReservationsTable + ` (
	job_id     VARCHAR(64) NOT NULL PRIMARY KEY,
	tenant_id  VARCHAR(64) NOT NULL,
	credit_key VARCHAR(128) NOT NULL,
	created_at TIMESTAMP NOT NULL
)`

// creditReservation is the durable record of one credit reservation
// Simulate opened and that neither NotifyOnCompletion's poll-driven path
// nor the reconciliation sweep (reconcile.go) has yet settled -- see this
// package's doc comment's "Credit accounting" section.
//
// It is platform data, like go/jobs' own jobRecord and go/config's row
// (root CLAUDE.md's data-domain census entries for both): it carries a
// real, unenforced tenant_id column rather than implementing
// dbkit.TenantScoped, because Service.ReconcileOutstandingCredits must
// list every tenant's outstanding rows in one query -- an access pattern
// dbkit.Repository[T]'s tenant-injecting plugin cannot serve, since it
// fails a query closed the instant its context carries no tenant, and
// Repository[T]'s generic constraint requires TenantScoped in the first
// place (the identical reasoning jobRecord's own doc comment gives for
// its cross-tenant dispatch query).
//
// A row's mere existence IS "not yet settled": save inserts it the moment
// Simulate's PreDeduct and the resulting enqueue both succeed, and delete
// removes it the moment settleCredit's Confirm/Refund succeeds --
// whichever of NotifyOnCompletion's poll-driven call or the reconciliation
// sweep gets there first, since both funnel through the identical
// settleCredit -> store.delete sequence (see settleCredit's own doc
// comment in service.go).
type creditReservation struct {
	// JobID is the go/ai-gateway image-generation job's id -- the same
	// value NotifyOnCompletion is later called with, and
	// ReconcileOutstandingCredits' own lookup key.
	JobID string `gorm:"column:job_id;primaryKey;size:64"`

	// TenantID is the reservation's owning tenant, stored so
	// ReconcileOutstandingCredits' cross-tenant sweep can rebuild
	// pkgcore.WithTenant(ctx, TenantID) for each row without ever needing
	// a system context -- go/jobs.Queue.Get accepts a ctx carrying the
	// Job's own tenant, which this always is.
	TenantID string `gorm:"column:tenant_id;size:64;not null"`

	// CreditKey is the billing.CreditTransaction idempotency key
	// Simulate's PreDeduct used -- see Simulate's own doc comment (in
	// service.go) for why it cannot be JobID itself. settleCredit passes
	// this, never JobID, to CreditService.Confirm/Refund.
	CreditKey string `gorm:"column:credit_key;size:128;not null"`

	// CreatedAt records when the reservation was opened, for operational
	// visibility only -- no code reads it back.
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
}

// TableName pins creditReservation to creditReservationsTable, so it does
// not depend on GORM's pluralization of the (unexported) type name.
func (creditReservation) TableName() string { return creditReservationsTable }

// ReservationStore is smilesim's own tiny, plain-gorm data-access type for
// creditReservation -- deliberately NOT a dbkit.Repository[T], whose
// generic constraint requires TenantScoped, exactly what creditReservation
// must NOT implement (see its own doc comment). This follows root
// CLAUDE.md's documented exception for platform data: "[platform data]
// can't use [Repository[T]] ... so they use dbkit.Open()'s plain *gorm.DB
// directly."
//
// The zero value is not ready to use; construct one with
// NewReservationStore.
type ReservationStore struct {
	db *gorm.DB
}

// NewReservationStore returns a ReservationStore backed by db, expected to
// come from dbkit.Open (directly, or through dbkit/dbtest in tests) --
// mirroring go/jobs.NewStandaloneQueue's own doc comment on why. It
// performs no I/O; call EnsureSchema once before first use.
func NewReservationStore(db *gorm.DB) *ReservationStore {
	return &ReservationStore{db: db}
}

// EnsureSchema creates ReservationStore's table if it does not already
// exist -- see createCreditReservationsTableSQL's own doc comment for why
// this is a plain, idempotent CREATE TABLE rather than a versioned
// dbkit.MigrationRegistry migration. Call it once, before Simulate or
// NotifyOnCompletion ever runs (cmd/server's own wiring does this
// immediately after opening the shared database, alongside the other
// modules' migrationRegistry.Apply call).
func (s *ReservationStore) EnsureSchema(ctx context.Context) error {
	return s.db.WithContext(ctx).Exec(createCreditReservationsTableSQL).Error
}

// save durably records that jobID's reservation, opened under tenant and
// keyed by creditKey, has not yet been settled. Called by Simulate exactly
// once per successful GenerateImage call, immediately after the job id
// becomes known.
func (s *ReservationStore) save(ctx context.Context, jobID jobs.JobID, tenant pkgcore.TenantID, creditKey string) error {
	return s.db.WithContext(ctx).Create(&creditReservation{
		JobID:     string(jobID),
		TenantID:  string(tenant),
		CreditKey: creditKey,
	}).Error
}

// get returns jobID's outstanding reservation, and whether one exists --
// false with a nil error means jobID has no reservation on file (never
// reserved in the first place, or already settled and deleted).
func (s *ReservationStore) get(ctx context.Context, jobID jobs.JobID) (creditReservation, bool, error) {
	var row creditReservation
	err := s.db.WithContext(ctx).First(&row, "job_id = ?", string(jobID)).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return creditReservation{}, false, nil
	}
	if err != nil {
		return creditReservation{}, false, err
	}
	return row, true, nil
}

// delete removes jobID's reservation row -- called once settleCredit's
// Confirm/Refund has already succeeded, so the row's presence keeps
// meaning "not yet settled" for exactly as long as it exists. jobID
// carrying no row is not an error: NotifyOnCompletion's poll-driven path
// and ReconcileOutstandingCredits' sweep may race to settle the same job,
// and whichever loses finds nothing left to delete.
func (s *ReservationStore) delete(ctx context.Context, jobID jobs.JobID) error {
	return s.db.WithContext(ctx).Delete(&creditReservation{}, "job_id = ?", string(jobID)).Error
}

// listAll returns every outstanding reservation, across every tenant at
// once -- ReconcileOutstandingCredits' own sweep entry point. Reading
// across tenants in one query is exactly why this store is a plain
// *gorm.DB wrapper rather than a dbkit.Repository[T] (see
// creditReservation's own doc comment).
func (s *ReservationStore) listAll(ctx context.Context) ([]creditReservation, error) {
	var rows []creditReservation
	if err := s.db.WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}
