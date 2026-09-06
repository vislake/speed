package smilesim

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// simulationsTable is SimulationStore's persisted table name.
const simulationsTable = "smilesim_simulations"

// createSimulationsTableSQL is executed imperatively, with a plain CREATE
// TABLE IF NOT EXISTS -- the exact bootstrapping pattern ReservationStore's
// own createCreditReservationsTableSQL uses and documents (see that
// constant's doc comment for why this app's own bookkeeping tables do not
// go through dbkit.MigrationRegistry's cross-module machinery). The
// statement is portable across both dbkit dialects, like its sibling:
// VARCHAR/TIMESTAMP columns, application-generated ids, no PostgreSQL- or
// SQLite-specific syntax.
const createSimulationsTableSQL = `CREATE TABLE IF NOT EXISTS ` + simulationsTable + ` (
	job_id          VARCHAR(64)  NOT NULL PRIMARY KEY,
	tenant_id       VARCHAR(64)  NOT NULL,
	photo_object_id VARCHAR(64)  NOT NULL,
	options_json    VARCHAR(256) NOT NULL,
	created_at      TIMESTAMP    NOT NULL
)`

// createSimulationsTenantPhotoIndexSQL is the lookup index behind
// ListSimulationsByPhoto's per-photo enumeration. Kept as its own statement
// (CREATE INDEX IF NOT EXISTS is accepted by both SQLite and PostgreSQL)
// because the composite index cannot be declared portably inside the CREATE
// TABLE above.
const createSimulationsTenantPhotoIndexSQL = `CREATE INDEX IF NOT EXISTS idx_smilesim_simulations_tenant_photo ON ` + simulationsTable + ` (tenant_id, photo_object_id)`

// simulationRecord is the durable record of one simulation generation
// request: which photo it was generated from, which effective option set
// produced it, and which image-generation job carries its outcome. The row
// is written the moment Simulate's enqueue succeeds and is never deleted or
// updated -- it is this package's append-only per-photo index, the data
// source the P3 gallery's "simulations of this photo" list reads through
// Service.ListSimulationsByPhoto, with each record's live status and output
// object read from the job at query time (the job's own persisted row is
// the outcome's source of truth; see ListSimulationsByPhoto's doc comment).
//
// Unlike creditReservation -- platform data by design, because the credit
// reconciliation sweep must list across every tenant in one query -- this
// row is tenant data: every access path (save, get, listByPhoto) is
// parameterized by the owning tenant, read from pkgcore context by the
// Service methods that call them, never from the request. It is still a
// plain-gorm store rather than a dbkit.Repository[T] because it is this
// reference app's own demo bookkeeping, not a shipped module repository:
// the same precedent ReservationStore's doc comment records, with the
// isolation guarantee that Repository[T]'s plugin would otherwise provide
// carried by the tenant parameter in every query plus the cross-tenant
// invisibility pin in service_test.go.
type simulationRecord struct {
	// JobID is the go/ai-gateway image-generation job's id -- Simulate's
	// own return value, and the key the outcome is retrieved under.
	JobID string `gorm:"column:job_id;primaryKey;size:64"`

	// TenantID is the record's owning tenant, stored so every read can be
	// tenant-scoped and so a future cross-tenant sweep (if this index ever
	// grows one) can rebuild context per row the way
	// ReconcileOutstandingCredits already does for reservations.
	TenantID string `gorm:"column:tenant_id;size:64;not null"`

	// PhotoObjectID is the go/storage object id of the patient photo the
	// simulation was generated from -- the P3 gallery's grouping key.
	PhotoObjectID string `gorm:"column:photo_object_id;size:64;not null"`

	// OptionsJSON is the canonical JSON of the EFFECTIVE option set that
	// produced this simulation -- after defaults were applied, exactly the
	// set whose rendering became the job's vendor prompt. Stored as JSON
	// text in a VARCHAR column (the dual-dialect-portable spelling of a
	// JSON document; SQLite has no native JSON column type) rather than as
	// discrete columns, because the option set is this package's own
	// shape: it may grow new dimensions without a schema change, and
	// nothing ever queries an individual option in SQL.
	OptionsJSON string `gorm:"column:options_json;size:256;not null"`

	// CreatedAt records when the generation was requested. The enumeration
	// read orders by it, newest first.
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
}

// TableName pins simulationRecord to simulationsTable, so it does not depend
// on GORM's pluralization of the (unexported) type name.
func (simulationRecord) TableName() string { return simulationsTable }

// options decodes the record's OptionsJSON back into the typed option set.
// A row whose JSON fails to decode is a corrupted record; the error is
// surfaced rather than guessed at.
func (r simulationRecord) options() (SimulationOptions, error) {
	var o SimulationOptions
	if err := json.Unmarshal([]byte(r.OptionsJSON), &o); err != nil {
		return SimulationOptions{}, err
	}
	return o, nil
}

// SimulationStore is smilesim's plain-gorm data-access type for
// simulationRecord -- the same shape ReservationStore gives creditReservation
// (same unexported methods, same EnsureSchema/save/get/list vocabulary), and
// the same deliberate non-Repository[T] choice that type's doc comment
// records.
//
// The zero value is not ready to use; construct one with
// NewSimulationStore.
type SimulationStore struct {
	db *gorm.DB
}

// NewSimulationStore returns a SimulationStore backed by db, expected to
// come from dbkit.Open (directly, or through dbkit/dbtest in tests) --
// mirroring NewReservationStore's identical contract. It performs no I/O;
// call EnsureSchema once before first use.
func NewSimulationStore(db *gorm.DB) *SimulationStore {
	return &SimulationStore{db: db}
}

// EnsureSchema creates SimulationStore's table and its per-photo lookup
// index if they do not already exist -- see
// createSimulationsTableSQL's own doc comment for why this is a plain,
// idempotent CREATE rather than a versioned dbkit.MigrationRegistry
// migration. Call it once, before Simulate or ListSimulationsByPhoto ever
// run (cmd/server's own wiring does this alongside
// ReservationStore.EnsureSchema).
func (s *SimulationStore) EnsureSchema(ctx context.Context) error {
	if err := s.db.WithContext(ctx).Exec(createSimulationsTableSQL).Error; err != nil {
		return err
	}
	return s.db.WithContext(ctx).Exec(createSimulationsTenantPhotoIndexSQL).Error
}

// save durably records that one generation request -- jobID over
// photoObjectID under tenant, produced by options -- exists. Called by
// Simulate exactly once per successful GenerateImage call, immediately after
// the job id becomes known. JobID is the primary key, so a repeated save of
// the same job would conflict rather than duplicate; Simulate never calls it
// twice for one job.
func (s *SimulationStore) save(ctx context.Context, jobID jobs.JobID, tenant pkgcore.TenantID, photoObjectID string, options SimulationOptions) error {
	data, err := json.Marshal(options)
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).Create(&simulationRecord{
		JobID:         string(jobID),
		TenantID:      string(tenant),
		PhotoObjectID: photoObjectID,
		OptionsJSON:   string(data),
	}).Error
}

// get returns tenant's record for jobID, and whether one exists -- false
// with a nil error means no record is on file under that tenant (never
// simulated through this package, or simulated under another tenant).
func (s *SimulationStore) get(ctx context.Context, tenant pkgcore.TenantID, jobID jobs.JobID) (simulationRecord, bool, error) {
	var row simulationRecord
	err := s.db.WithContext(ctx).First(&row, "job_id = ? AND tenant_id = ?", string(jobID), string(tenant)).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return simulationRecord{}, false, nil
	}
	if err != nil {
		return simulationRecord{}, false, err
	}
	return row, true, nil
}

// listByPhoto returns every record tenant has on file for photoObjectID,
// newest first (created_at descending, with job id as a stable tiebreak for
// two records created within the same timestamp tick). The tenant filter is
// part of the query itself -- never a post-query filter -- so a caller can
// only ever enumerate its own tenant's rows.
func (s *SimulationStore) listByPhoto(ctx context.Context, tenant pkgcore.TenantID, photoObjectID string) ([]simulationRecord, error) {
	var rows []simulationRecord
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND photo_object_id = ?", string(tenant), photoObjectID).
		Order("created_at DESC, job_id DESC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}
