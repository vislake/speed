package compliance_test

// Runnable documentation for compliance's public API, mirroring
// go/metering/example_test.go's and go/pki/example_test.go's convention:
// these examples are compiled AND executed by `go test`, so a change to
// compliance's public API that breaks the documented usage fails the
// build rather than only rotting in prose. ExampleRenderAuditReport
// below carries the compensating obligations of RenderAuditReport, the
// one public API here with no real business-module consumer yet.
//
// exampleNotesModule plays the part of a real business module: it
// implements the module contract and, in its own Register call, registers one
// pkgcore.RetentionParticipant over its own tiny SoftDeletable model --
// the whole mechanism this package exists to orchestrate, shown from the
// registering module's side rather than compliance's own internal
// testutil fixture.

import (
	"context"
	"embed"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	auditmigrations "github.com/vislake/speed/go/dbkit/audit/migrations"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"

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

// exampleNotesModule is the minimal module a real business module
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
func (m *exampleNotesModule) Register(reg *pkgcore.ComponentRegistry) error {
	return reg.RetentionSeat().Add(pkgcore.RetentionParticipant{
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
		// Erase is mandatory at registration too
		// (pkgcore.ErrNilRetentionErase). This example model has no
		// subject attribution of its own, so its answer to every
		// right-to-erasure request is an explicit nothing-to-erase --
		// (0, nil) -- declared here rather than left nil.
		Erase: func(context.Context, pkgcore.SubjectRef) (int, error) { return 0, nil },
	})
}

// exampleAuditModule feeds dbkit/audit's own embedded migrations to
// dbkit.MigrationRegistry -- the same shape module_test.go's fakeAuditModule
// uses; only Name and Migrations are ever read by MigrationRegistry.Apply
// here.
type exampleAuditModule struct{}

func (exampleAuditModule) Name() string                              { return "audit" }
func (exampleAuditModule) DependsOn() []string                       { return nil }
func (exampleAuditModule) Migrations() embed.FS                      { return auditmigrations.FS }
func (exampleAuditModule) Locales() embed.FS                         { return embed.FS{} }
func (exampleAuditModule) OpenAPISpec() []byte                       { return nil }
func (exampleAuditModule) Register(*pkgcore.ComponentRegistry) error { return nil }

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

	// A real host migrates dbkit/audit's audit_events table before wiring
	// anything that audits -- and compliance's own Register now registers
	// the module's export-manifests cleanup participant (export_cleanup.go),
	// which reads the tenant's audit events on every retention sweep, so
	// this example must too: without the table the module's own sweep
	// callback would fail its read.
	registry := dbkit.NewMigrationRegistry()
	if registryErr := registry.Register(exampleAuditModule{}); registryErr != nil {
		fmt.Println("register audit migrations:", registryErr)
		return
	}
	if applyErr := registry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply audit migrations:", applyErr)
		return
	}

	// compliance.NewModule takes an *audit.Repository -- audit.Emit only
	// ever publishes an event on the bus (dbkit/audit's own write-back
	// persister is a separate, host-wired subscriber this example does not
	// need), so wrapping the migrated connection is enough.
	m := compliance.NewModule(audit.NewRepository(db), compliance.WithQueue(exampleNoopQueue{}))

	reg, err := componenttest.DeclareModules(m, notes)
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

// ExampleRenderAuditReport renders one audit.AuditEvent -- the shape an
// AuditQuery.Query call returns -- as a CSV audit report: the header row
// first, then one RFC 4180 row per event. The report's bytes are what an
// HTTP handler would set as a text/csv response body (no such handler
// exists; RenderAuditReport has no real consumer yet). This example
// stands in for that consumer as runnable, compile-checked documentation
// of RenderAuditReport and ReportFormat, so a change to their signatures
// fails the build instead of rotting in prose.
func ExampleRenderAuditReport() {
	evt := audit.AuditEvent{
		ID:         "audit-1",
		Action:     "notes.note.create",
		TenantID:   "tenant-acme",
		IP:         "203.0.113.5",
		UserAgent:  "curl/8.0",
		TraceID:    "trace-1",
		OccurredAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	evt.SetActor(pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1", DisplayName: "Ada"})
	evt.SetResource(audit.Resource{Type: "note", ID: "note-1", DisplayName: "Meeting notes"})
	evt.SetResult(audit.Result{Success: true})

	report, err := compliance.RenderAuditReport([]audit.AuditEvent{evt}, compliance.ReportFormatCSV)
	if err != nil {
		fmt.Println("render:", err)
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(string(report)), "\n") {
		fmt.Println(line)
	}

	// Output:
	// id,actor_type,actor_id,actor_display_name,has_on_behalf_of,on_behalf_of_type,on_behalf_of_id,on_behalf_of_display_name,action,resource_type,resource_id,resource_display_name,success,failure_reason,changes,tenant_id,ip,user_agent,trace_id,occurred_at
	// audit-1,user,user-1,Ada,false,,,,notes.note.create,note,note-1,Meeting notes,true,,,tenant-acme,203.0.113.5,curl/8.0,trace-1,2026-01-02T03:04:05Z
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

// ExampleNewConfigReader wires the config module's lazy Handle into
// compliance's export-delivery-expiry seam: the reader is constructed while
// assembling -- before Attach has produced the *config.Service, which the
// config module's own Attach contract only allows after Bootstrap returns --
// and its first read, in that window, fails closed with the config module's
// coded not-attached error. Once Attach has run and an operator has written
// the tenant's own compliance.export_delivery_expiry row, the same reader
// resolves that row.
func ExampleNewConfigReader() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:compliance_example_config_reader?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	registry := dbkit.NewMigrationRegistry()
	if registryErr := registry.Register(exampleAuditModule{}); registryErr != nil {
		fmt.Println("register audit migrations:", registryErr)
		return
	}
	configModule := config.NewModule(db, config.WithPollInterval(0))
	if registryErr := registry.Register(configModule); registryErr != nil {
		fmt.Println("register config migrations:", registryErr)
		return
	}
	if applyErr := registry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations:", applyErr)
		return
	}

	// The reader is wired here, at assembly time, over the config module's
	// handle: compliance's own Register declares the
	// compliance.export_delivery_expiry item that the handle will serve
	// once the schema is frozen.
	reader := compliance.NewConfigReader(configModule.Handle())
	m := compliance.NewModule(audit.NewRepository(db), compliance.WithQueue(exampleNoopQueue{}),
		compliance.WithExportConfigReader(reader))

	tenant := pkgcore.TenantID("tenant-acme")

	// Before Attach there is no schema to resolve against, and the read
	// says so: a coded refusal, never a fabricated "unconfigured" answer.
	if _, _, readErr := reader.ExportDeliveryExpiry(ctx, tenant); readErr != nil {
		fmt.Println("before Attach:", readErr)
	}

	// The module's Register and config's Attach share the registry's one
	// Init window: the seats accept writes only during Init.
	reg := componenttest.NewRegistry()
	var svc *config.Service
	if declareErr := componenttest.DeclareAll(reg, m.Register, configModule.Register,
		func(r *pkgcore.ComponentRegistry) error {
			attached, attachErr := configModule.Attach(r)
			if attachErr != nil {
				return attachErr
			}
			svc = attached
			return nil
		},
	); declareErr != nil {
		fmt.Println("declare and attach config:", err)
		return
	}
	defer func() { _ = svc.Close() }()

	// No row yet: ok is false, the "tenant configured none" answer Export
	// falls back to defaultExportDeliveryExpiry on.
	_, configured, err := reader.ExportDeliveryExpiry(ctx, tenant)
	if err != nil {
		fmt.Println("read:", err)
		return
	}
	fmt.Println("unconfigured:", configured)

	// The operator's tenant-tier write is what the same reader now serves.
	writer := pkgcore.WithTenant(ctx, tenant)
	if err = svc.Set(writer, config.ScopeTenant, compliance.ConfigExportDeliveryExpiry,
		config.Value{Data: 2 * time.Hour}, "ops-1"); err != nil {
		fmt.Println("set:", err)
		return
	}
	expiry, configured, err := reader.ExportDeliveryExpiry(ctx, tenant)
	if err != nil {
		fmt.Println("read:", err)
		return
	}
	fmt.Println("configured:", configured, expiry)

	// Output:
	// before Attach: config.service_not_attached
	// unconfigured: false
	// configured: true 2h0m0s
}
