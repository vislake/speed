package audit

import (
	"context"
	"embed"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit/migrations"
	"github.com/vislake/speed/go/pkgcore"
)

// fakeAuditModule is a minimal pkgcore.Module used only by this package's
// own tests, to feed the package's embedded migrations to
// dbkit.MigrationRegistry without depending on the persister
// pkgcore.Module that lands alongside Emit and the write-capture plugin in
// this same round (see doc.go). Only Name and Migrations are ever read by
// MigrationRegistry.Apply here; DependsOn, Locales, OpenAPISpec and
// Register exist solely to satisfy the interface, mirroring dbkit's own
// migrations_test.go fakeModule.
type fakeAuditModule struct{}

func (fakeAuditModule) Name() string                     { return "audit" }
func (fakeAuditModule) DependsOn() []string              { return nil }
func (fakeAuditModule) Migrations() embed.FS             { return migrations.FS }
func (fakeAuditModule) Locales() embed.FS                { return embed.FS{} }
func (fakeAuditModule) OpenAPISpec() []byte              { return nil }
func (fakeAuditModule) Register(*pkgcore.Registry) error { return nil }

var _ pkgcore.Module = fakeAuditModule{}

// auditTestDBSeq numbers the in-memory SQLite databases this package's
// tests open, so parallel or repeated runs never share one -- mirroring
// go/config's modelTestDBSeq and go/dbkit's migrationsTestDBSeq.
var auditTestDBSeq atomic.Int64

// openAuditTestDB opens a private in-memory SQLite database with the
// audit_events table already migrated through the real
// dbkit.MigrationRegistry (fakeAuditModule above) -- never AutoMigrate --
// and registers its cleanup. Used by this file and model_test.go.
func openAuditTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:audit_test_%d?mode=memory&cache=shared", auditTestDBSeq.Add(1))
	db, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open: %v", err)
	}
	reg := dbkit.NewMigrationRegistry()
	if err := reg.Register(fakeAuditModule{}); err != nil {
		t.Fatalf("registering the audit migrations: %v", err)
	}
	if err := reg.Apply(context.Background(), db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("applying the audit migrations: %v", err)
	}
	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func sampleEvent() *AuditEvent {
	evt := &AuditEvent{Action: "notes.note.create", TenantID: "tenant-a"}
	evt.SetActor(pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1", DisplayName: "Ada"})
	evt.SetResource(Resource{Type: "note", ID: "note-1", DisplayName: "Meeting notes"})
	evt.SetResult(Result{Success: true})
	return evt
}

func TestRepository_Insert_GeneratesIDWhenEmpty(t *testing.T) {
	db := openAuditTestDB(t)
	repo := NewRepository(db)

	evt := sampleEvent()
	if evt.ID != "" {
		t.Fatalf("sampleEvent().ID = %q, want empty for this test's premise", evt.ID)
	}
	if err := repo.Insert(context.Background(), evt); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}
	if evt.ID == "" {
		t.Errorf("Insert() left evt.ID empty, want a generated id")
	}
}

func TestRepository_Insert_PreservesCallerSuppliedID(t *testing.T) {
	db := openAuditTestDB(t)
	repo := NewRepository(db)

	evt := sampleEvent()
	evt.ID = "caller-chosen-id"
	if err := repo.Insert(context.Background(), evt); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}
	if evt.ID != "caller-chosen-id" {
		t.Errorf("Insert() changed evt.ID to %q, want it left as the caller-supplied \"caller-chosen-id\"", evt.ID)
	}
}

func TestRepository_Insert_PopulatesOccurredAtWhenZero(t *testing.T) {
	db := openAuditTestDB(t)
	repo := NewRepository(db)

	before := time.Now().Add(-time.Second)
	evt := sampleEvent()
	if err := repo.Insert(context.Background(), evt); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}
	if evt.OccurredAt.Before(before) {
		t.Errorf("Insert() left OccurredAt = %v, want it populated by autoCreateTime to roughly now", evt.OccurredAt)
	}
}

// TestRepository_Insert_DuplicateID_Fails pins Insert's own documented,
// stricter, non-idempotent contract -- a duplicate caller-supplied ID is a
// genuine error -- so a future change to InsertIdempotent's dedup logic
// cannot accidentally loosen Insert itself along with it.
func TestRepository_Insert_DuplicateID_Fails(t *testing.T) {
	db := openAuditTestDB(t)
	repo := NewRepository(db)

	first := sampleEvent()
	first.ID = "duplicate-id"
	if err := repo.Insert(context.Background(), first); err != nil {
		t.Fatalf("Insert() [first] error = %v", err)
	}

	second := sampleEvent()
	second.ID = "duplicate-id"
	if err := repo.Insert(context.Background(), second); err == nil {
		t.Fatal("Insert() [second, duplicate ID] error = nil, want a primary-key conflict")
	}
}

// TestRepository_InsertIdempotent_DuplicateID_IsANoOp is the regression
// test for the finding recorded in go/dbkit/audit/AGENTS.md's
// "Multi-replica delivery" section: in distributed deployment mode with
// more than one replica, pkgcore.RedisEventBus delivers every event to
// every replica once each, so Module's subscribers (module.go's
// onWriteCaptured, onRecorded, onSystemContextEntered) independently call
// Insert once per replica for the SAME logical event. Before
// InsertIdempotent existed, each of those calls generated its own random
// ID (Insert's default when evt.ID is left empty), so N replicas produced
// N rows for one real action. This test proves the fix at the Repository
// layer directly: inserting the same evt.ID twice through
// InsertIdempotent succeeds both times but persists exactly one row --
// module_test.go's
// TestModule_OnWriteCaptured_DeliveredToMultipleReplicas_PersistsExactlyOnce
// and its two siblings prove the same property end to end, through
// Module's real deterministic-ID derivation.
func TestRepository_InsertIdempotent_DuplicateID_IsANoOp(t *testing.T) {
	db := openAuditTestDB(t)
	repo := NewRepository(db)

	first := sampleEvent()
	first.ID = "replica-shared-id"
	if err := repo.InsertIdempotent(context.Background(), first); err != nil {
		t.Fatalf("InsertIdempotent() [replica A] error = %v", err)
	}

	second := sampleEvent()
	second.ID = "replica-shared-id"
	if err := repo.InsertIdempotent(context.Background(), second); err != nil {
		t.Fatalf("InsertIdempotent() [replica B, same ID] error = %v, want nil (a duplicate ID must be a silent no-op, not an error)", err)
	}

	got, err := repo.Get(context.Background(), "replica-shared-id")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got == nil {
		t.Fatal("Get() = nil, want the row from the first InsertIdempotent() call")
	}

	rows, err := repo.ListByTenant(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("ListByTenant() error = %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("ListByTenant() returned %d rows, want exactly 1 (the second InsertIdempotent() call must not have inserted a second row)", len(rows))
	}
}

// TestRepository_InsertIdempotent_NewID_Inserts proves InsertIdempotent
// behaves exactly like Insert for the ordinary, non-duplicate case.
func TestRepository_InsertIdempotent_NewID_Inserts(t *testing.T) {
	db := openAuditTestDB(t)
	repo := NewRepository(db)

	evt := sampleEvent()
	evt.ID = "fresh-id"
	if err := repo.InsertIdempotent(context.Background(), evt); err != nil {
		t.Fatalf("InsertIdempotent() error = %v", err)
	}

	got, err := repo.Get(context.Background(), "fresh-id")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got == nil {
		t.Fatal("Get() = nil, want the inserted event")
	}
}

func TestRepository_Get_ReturnsInsertedEvent(t *testing.T) {
	db := openAuditTestDB(t)
	repo := NewRepository(db)

	want := sampleEvent()
	admin := pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: "admin-1", DisplayName: "Grace"}
	want.SetOnBehalfOf(&admin)
	if err := repo.Insert(context.Background(), want); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}

	got, err := repo.Get(context.Background(), want.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got == nil {
		t.Fatalf("Get() = nil, want the inserted event")
	}
	if got.Action != want.Action || got.TenantID != want.TenantID {
		t.Errorf("Get() = %+v, want Action=%q TenantID=%q", got, want.Action, want.TenantID)
	}
	if got.Actor() != want.Actor() {
		t.Errorf("Get().Actor() = %+v, want %+v", got.Actor(), want.Actor())
	}
	gotOnBehalfOf, ok := got.OnBehalfOf()
	if !ok {
		t.Fatalf("Get().OnBehalfOf() ok = false, want true (Insert was given one)")
	}
	if gotOnBehalfOf != admin {
		t.Errorf("Get().OnBehalfOf() = %+v, want %+v", gotOnBehalfOf, admin)
	}
	if got.Resource() != want.Resource() {
		t.Errorf("Get().Resource() = %+v, want %+v", got.Resource(), want.Resource())
	}
	if got.Result() != want.Result() {
		t.Errorf("Get().Result() = %+v, want %+v", got.Result(), want.Result())
	}
}

func TestRepository_Get_ReturnsNilNilWhenNotFound(t *testing.T) {
	db := openAuditTestDB(t)
	repo := NewRepository(db)

	got, err := repo.Get(context.Background(), "does-not-exist")
	if err != nil {
		t.Fatalf("Get() error = %v, want nil", err)
	}
	if got != nil {
		t.Errorf("Get() = %+v, want nil for a missing id", got)
	}
}

func TestRepository_ListByTenant_ReturnsOnlyMatchingTenantNewestFirst(t *testing.T) {
	db := openAuditTestDB(t)
	repo := NewRepository(db)
	ctx := context.Background()

	older := sampleEvent()
	older.TenantID = "tenant-a"
	older.OccurredAt = time.Now().Add(-time.Hour)
	if err := repo.Insert(ctx, older); err != nil {
		t.Fatalf("Insert(older) error = %v", err)
	}

	newer := sampleEvent()
	newer.TenantID = "tenant-a"
	newer.OccurredAt = time.Now()
	if err := repo.Insert(ctx, newer); err != nil {
		t.Fatalf("Insert(newer) error = %v", err)
	}

	otherTenant := sampleEvent()
	otherTenant.TenantID = "tenant-b"
	if err := repo.Insert(ctx, otherTenant); err != nil {
		t.Fatalf("Insert(otherTenant) error = %v", err)
	}

	got, err := repo.ListByTenant(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("ListByTenant() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListByTenant(tenant-a) returned %d events, want 2 (tenant-b's own event must not appear)", len(got))
	}
	if got[0].ID != newer.ID || got[1].ID != older.ID {
		t.Errorf("ListByTenant(tenant-a) order = [%s, %s], want [newer=%s, older=%s] (newest first)",
			got[0].ID, got[1].ID, newer.ID, older.ID)
	}
}

func TestRepository_ListByTenant_EmptyTenantID_ReturnsPlatformEvents(t *testing.T) {
	db := openAuditTestDB(t)
	repo := NewRepository(db)
	ctx := context.Background()

	platformEvt := sampleEvent()
	platformEvt.TenantID = ""
	platformEvt.Action = "tenancy.system_context.entered"
	if err := repo.Insert(ctx, platformEvt); err != nil {
		t.Fatalf("Insert(platformEvt) error = %v", err)
	}

	tenantEvt := sampleEvent()
	tenantEvt.TenantID = "tenant-a"
	if err := repo.Insert(ctx, tenantEvt); err != nil {
		t.Fatalf("Insert(tenantEvt) error = %v", err)
	}

	got, err := repo.ListByTenant(ctx, "")
	if err != nil {
		t.Fatalf("ListByTenant(\"\") error = %v", err)
	}
	if len(got) != 1 || got[0].ID != platformEvt.ID {
		t.Fatalf("ListByTenant(\"\") = %+v, want exactly the platform-level event %s", got, platformEvt.ID)
	}
}

// TestRepository_HasNoUpdateOrDeleteMethod is the compile-shape proof
// behind Repository's own "append-only by construction" doc comment: Go
// has no way to assert "this type lacks a method" at compile time, so this
// reflects over Repository's method set instead, and fails loudly the
// moment a future change adds one of these names back.
func TestRepository_HasNoUpdateOrDeleteMethod(t *testing.T) {
	repoType := reflect.TypeOf(&Repository{})
	for _, name := range []string{"Update", "Updates", "Delete", "Remove", "Save"} {
		if _, ok := repoType.MethodByName(name); ok {
			t.Errorf("Repository has a method named %q; audit_events must be append-only at the application layer (see repository.go's own doc comment)", name)
		}
	}
}
