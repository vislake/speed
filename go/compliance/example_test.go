package compliance_test

// Runnable documentation for compliance's public API, mirroring
// go/metering/example_test.go's and go/pki/example_test.go's convention:
// this example is compiled AND executed by `go test`, so a change to
// compliance's public API that breaks the documented usage fails the
// build rather than only rotting in prose. It doubles as the
// compensating-obligations proof root CLAUDE.md's Documentation rules
// asks for wherever a round ships with no real business-module consumer
// yet -- exactly the go/pki X.509-layer precedent this round's own
// AGENTS.md Known limitations section names.
//
// exampleNotesModule plays the part of a real business module: it
// implements pkgcore.Module and, in its own Register call, registers one
// pkgcore.RetentionParticipant over its own tiny SoftDeletable model --
// the whole mechanism this package exists to orchestrate, shown from the
// registering module's side rather than compliance's own internal
// testutil fixture.

import (
	"context"
	"embed"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/compliance"
)

// exampleNote is a tiny SoftDeletable, tenant-scoped model, standing in
// for a real business module's own model.
type exampleNote struct {
	ID        string     `gorm:"primaryKey;size:36"`
	TenantID  string     `gorm:"primaryKey;size:64"`
	DeletedAt *time.Time `gorm:"column:deleted_at"`
	DeletedBy string     `gorm:"column:deleted_by;not null;default:''"`
}

func (exampleNote) TableName() string               { return "compliance_example_notes" }
func (n exampleNote) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(n.TenantID) }
func (n exampleNote) GetDeletedAt() *time.Time      { return n.DeletedAt }

// exampleNotesModule is the minimal pkgcore.Module a real business module
// implements to opt into compliance's retention-sweep orchestration: it
// registers one pkgcore.RetentionParticipant during Register, and its
// Sweep callback calls its own dbkit.Repository[exampleNote].HardDelete --
// never compliance code touching exampleNote's table directly.
type exampleNotesModule struct {
	repo *dbkit.Repository[exampleNote]
	db   *gorm.DB
}

func (m *exampleNotesModule) Name() string         { return "example_notes" }
func (m *exampleNotesModule) DependsOn() []string  { return nil }
func (m *exampleNotesModule) Migrations() embed.FS { return embed.FS{} }
func (m *exampleNotesModule) Locales() embed.FS    { return embed.FS{} }
func (m *exampleNotesModule) OpenAPISpec() []byte  { return nil }
func (m *exampleNotesModule) Register(reg *pkgcore.Registry) error {
	return reg.Retention.Add(pkgcore.RetentionParticipant{
		Name: "example_notes.note",
		Sweep: func(ctx context.Context, _ pkgcore.TenantID, cutoff time.Time) (int, error) {
			var rows []exampleNote
			err := dbkit.WithTenantSession(ctx, m.db, func(tx *gorm.DB) error {
				return tx.Unscoped().
					Where("deleted_at IS NOT NULL AND deleted_at <= ?", cutoff).
					Find(&rows).Error
			})
			if err != nil {
				return 0, err
			}
			reaped := 0
			for _, row := range rows {
				if err := m.repo.HardDelete(ctx, row.ID); err != nil {
					return reaped, err
				}
				reaped++
			}
			return reaped, nil
		},
	})
}

var _ pkgcore.Module = (*exampleNotesModule)(nil)

// Example wires compliance.Module alongside a fake business module, seeds
// one soft-deleted row well past the retention window, sweeps it, and
// reports how many rows were reaped.
func Example() {
	ctx := context.Background()

	// A real host opens PostgreSQL in the distributed deployment mode.
	// SQLite keeps this example self-contained under `go test`, with no
	// external service required.
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:compliance_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}
	if execErr := db.Exec(`CREATE TABLE compliance_example_notes (
		id TEXT NOT NULL, tenant_id TEXT NOT NULL,
		deleted_at TIMESTAMP NULL, deleted_by TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (tenant_id, id)
	)`).Error; execErr != nil {
		fmt.Println("create table:", execErr)
		return
	}

	notes := &exampleNotesModule{repo: dbkit.NewRepository[exampleNote](db), db: db}

	// compliance.NewModule needs an *audit.Repository -- audit.Emit only
	// ever publishes an event on the bus (dbkit/audit's own write-back
	// persister is a separate, host-wired subscriber this example does not
	// need), so wrapping the same connection is enough; no audit_events
	// migration is required for this example to run.
	m := compliance.NewModule(audit.NewRepository(db), compliance.WithQueue(exampleNoopQueue{}))

	reg, err := pkgcore.NewKernel().Bootstrap(ctx, m, notes)
	if err != nil {
		fmt.Println("bootstrap:", err)
		return
	}
	_ = reg

	tenant := pkgcore.TenantID("tenant-acme")
	note := exampleNote{ID: "note-1", TenantID: string(tenant)}
	if createErr := notes.repo.Create(pkgcore.WithTenant(ctx, tenant), &note); createErr != nil {
		fmt.Println("create:", createErr)
		return
	}
	if deleteErr := notes.repo.Delete(pkgcore.WithTenant(ctx, tenant), note.ID); deleteErr != nil {
		fmt.Println("delete:", deleteErr)
		return
	}
	// Backdate deleted_at well past the default retention window (30
	// days), so this example's sweep has something real to reap without
	// waiting.
	if execErr := db.Exec(
		"UPDATE compliance_example_notes SET deleted_at = ? WHERE id = ?",
		time.Now().Add(-40*24*time.Hour), note.ID,
	).Error; execErr != nil {
		fmt.Println("backdate:", execErr)
		return
	}

	result, err := m.Retention().SweepTenant(pkgcore.WithTenant(ctx, tenant), tenant)
	if err != nil {
		fmt.Println("sweep:", err)
		return
	}
	fmt.Println("reaped:", result.TotalReaped())

	// Output:
	// reaped: 1
}

// exampleNoopQueue is a minimal jobs.Queue satisfying Module.Register's
// WithQueue requirement; this example never enqueues anything onto it.
type exampleNoopQueue struct{}

func (exampleNoopQueue) Enqueue(context.Context, jobs.Task, ...jobs.EnqueueOption) (jobs.JobID, error) {
	return "", nil
}
func (exampleNoopQueue) Get(context.Context, jobs.JobID) (*jobs.Job, error) { return nil, nil }
func (exampleNoopQueue) Cancel(context.Context, jobs.JobID) error           { return nil }

var _ jobs.Queue = exampleNoopQueue{}
