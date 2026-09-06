package dbkit_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	// Blank-imported for its init side effect: registers dbkit.DialectSQLite
	// so the dbkit.Open call in openAuditCaptureTestDB has a driver to build
	// from.
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
	"github.com/vislake/speed/go/dbkit/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// createWidgetsTable mirrors internal/testutil/migrations/sqlite/
// 0001_create_widgets.sql so this file's tests can apply it directly
// through dbkit.Open (which never auto-migrates) without reaching into
// testutil's own embed.FS, the same way tenant_scope_test.go's
// createPlatformFlagsTable creates its own fixture table by hand.
func createWidgetsTable(t *testing.T, db *gorm.DB) {
	t.Helper()
	err := db.Exec(`CREATE TABLE widgets (
		id        VARCHAR(26)  NOT NULL,
		tenant_id VARCHAR(26)  NOT NULL,
		name      VARCHAR(255) NOT NULL,
		value     INTEGER      NOT NULL DEFAULT 0,
		PRIMARY KEY (tenant_id, id)
	)`).Error
	if err != nil {
		t.Fatalf("create widgets table: %v", err)
	}
}

// isRecordNotFound reports whether err is dbkit's ErrRecordNotFound — by
// code, never by pointer identity, because apperr.WithParam always returns a
// new *apperr.Error, so the pointer a Repository method returns is never the
// package-level sentinel itself. It is this black-box file's twin of the
// same-named helper in repository_test.go (which package dbkit cannot share
// across the package boundary).
func isRecordNotFound(err error) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == dbkit.ErrRecordNotFound.Code
}

// rawSoftDeletableWidgetRow reads id's raw deleted_at/deleted_by columns
// directly through db.Raw, bypassing every GORM callback (including the
// soft-delete auto-scope plugin, which raw SQL never runs) — the "what
// actually landed in the database" ground truth this file's soft-delete
// capture tests cross-check their captured After against. found is false
// when no row with id exists at all. It is this black-box file's twin of
// the same-named helper in repository_test.go.
func rawSoftDeletableWidgetRow(t *testing.T, db *gorm.DB, id string) (found bool, deletedAt *time.Time, deletedBy string) {
	t.Helper()
	var (
		nullDeletedAt sql.NullTime
		nullDeletedBy sql.NullString
	)
	row := db.Raw(`SELECT deleted_at, deleted_by FROM soft_deletable_widgets WHERE id = ?`, id).Row()
	if err := row.Scan(&nullDeletedAt, &nullDeletedBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil, ""
		}
		t.Fatalf("raw soft_deletable_widgets lookup for id %q: %v", id, err)
	}
	if nullDeletedAt.Valid {
		deletedAt = &nullDeletedAt.Time
	}
	return true, deletedAt, nullDeletedBy.String
}

// auditCaptureTestDBSeq numbers this file's in-memory SQLite databases so
// concurrent or repeated test runs never share one.
var auditCaptureTestDBSeq atomic.Int64

// openAuditCaptureTestDB opens a dbkit.Open connection with AuditBus set to
// bus (nil is valid: no capture installed) and the widgets table migrated.
func openAuditCaptureTestDB(t *testing.T, bus pkgcore.EventBus) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:audit_capture_test_%d?mode=memory&cache=shared", auditCaptureTestDBSeq.Add(1))
	db, err := dbkit.Open(context.Background(), dbkit.Options{
		Dialect:  dbkit.DialectSQLite,
		DSN:      dsn,
		AuditBus: bus,
	})
	if err != nil {
		t.Fatalf("dbkit.Open: %v", err)
	}
	createWidgetsTable(t, db)
	return db
}

// capturedBus is a pkgcore.EventBus test double that records every
// WriteCapturedEvent published to it, and can be made to fail Publish on
// demand to exercise the plugin's loud-failure contract.
type capturedBus struct {
	mu     sync.Mutex
	events []dbkit.WriteCapturedEvent
	fail   error
}

func (b *capturedBus) Subscribe(string, pkgcore.EventHandler) {}

func (b *capturedBus) Publish(_ context.Context, evt pkgcore.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail != nil {
		return b.fail
	}
	payload, ok := evt.Payload.(dbkit.WriteCapturedEvent)
	if !ok {
		return fmt.Errorf("dbkit_test: unexpected payload type %T", evt.Payload)
	}
	b.events = append(b.events, payload)
	return nil
}

func (b *capturedBus) captured() []dbkit.WriteCapturedEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]dbkit.WriteCapturedEvent, len(b.events))
	copy(out, b.events)
	return out
}

var _ pkgcore.EventBus = (*capturedBus)(nil)

func TestOpen_AuditBusNil_InstallsNoCapture(t *testing.T) {
	db := openAuditCaptureTestDB(t, nil)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	w := &testutil.Widget{ID: "w1", Name: "gadget"}
	if err := db.WithContext(ctx).Create(w).Error; err != nil {
		t.Fatalf("Create() error = %v, want nil (AuditBus nil must behave exactly like before this field existed)", err)
	}
}

func TestAuditCapturePlugin_Create_PublishesWriteCapturedEvent(t *testing.T) {
	bus := &capturedBus{}
	db := openAuditCaptureTestDB(t, bus)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1", DisplayName: "Ada"})
	w := &testutil.Widget{ID: "w1", Name: "gadget", Value: 42}
	if err := db.WithContext(ctx).Create(w).Error; err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	events := bus.captured()
	if len(events) != 1 {
		t.Fatalf("captured %d events, want exactly 1", len(events))
	}
	got := events[0]
	if got.ResourceType != "widget" {
		t.Errorf("ResourceType = %q, want %q", got.ResourceType, "widget")
	}
	if got.Operation != "create" {
		t.Errorf("Operation = %q, want %q", got.Operation, "create")
	}
	if got.ResourceID != "w1" {
		t.Errorf("ResourceID = %q, want %q", got.ResourceID, "w1")
	}
	if got.TenantID != "tenant-a" {
		t.Errorf("TenantID = %q, want %q", got.TenantID, "tenant-a")
	}
	if got.Actor.ID != "user-1" {
		t.Errorf("Actor.ID = %q, want %q", got.Actor.ID, "user-1")
	}
	if got.OnBehalfOf != nil {
		t.Errorf("OnBehalfOf = %+v, want nil (no impersonation set)", got.OnBehalfOf)
	}
	if got.Table != "widgets" {
		t.Errorf("Table = %q, want %q", got.Table, "widgets")
	}
	if got.After == nil || got.After["name"] != "gadget" {
		t.Errorf("After = %+v, want a map with name=gadget", got.After)
	}
}

func TestAuditCapturePlugin_Create_CapturesOnBehalfOf(t *testing.T) {
	bus := &capturedBus{}
	db := openAuditCaptureTestDB(t, bus)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1"})
	ctx = pkgcore.WithOnBehalfOf(ctx, pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: "admin-1"})
	w := &testutil.Widget{ID: "w1", Name: "gadget"}
	if err := db.WithContext(ctx).Create(w).Error; err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	events := bus.captured()
	if len(events) != 1 {
		t.Fatalf("captured %d events, want exactly 1", len(events))
	}
	if events[0].OnBehalfOf == nil || events[0].OnBehalfOf.ID != "admin-1" {
		t.Errorf("OnBehalfOf = %+v, want an Actor with ID=admin-1", events[0].OnBehalfOf)
	}
}

func TestAuditCapturePlugin_Update_PublishesWriteCapturedEvent(t *testing.T) {
	bus := &capturedBus{}
	db := openAuditCaptureTestDB(t, bus)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	w := &testutil.Widget{ID: "w1", Name: "gadget", Value: 1}
	if err := db.WithContext(ctx).Create(w).Error; err != nil {
		t.Fatalf("seed Create() error = %v", err)
	}

	w.Value = 2
	if err := db.WithContext(ctx).Where("id = ?", "w1").Where("tenant_id = ?", "tenant-a").Save(w).Error; err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	events := bus.captured()
	if len(events) != 2 {
		t.Fatalf("captured %d events, want 2 (create then update)", len(events))
	}
	got := events[1]
	if got.Operation != "update" {
		t.Errorf("Operation = %q, want %q", got.Operation, "update")
	}
	if got.ResourceID != "w1" {
		t.Errorf("ResourceID = %q, want %q", got.ResourceID, "w1")
	}
	if got.After == nil || got.After["value"] != 2 {
		t.Errorf("After = %+v, want a map with value=2", got.After)
	}
}

// TestAuditCapturePlugin_SoftDelete_ClassifiesAsUpdateWithRealDiff is the
// scouting round's own named regression test: soft-delete
// (Repository[T].Delete against a SoftDeletable model) is, underneath, one
// UPDATE, and must be captured with Update semantics -- never a hand-rolled
// extra Delete-semantics event bolted onto Delete's own code, which would
// duplicate the automatic capture
// (docs/internal/04-data-and-tenancy.md's delete-semantics section, §4's own named pitfall).
//
// Checking Operation == "update" alone is not enough to catch the real
// hazard here: softDelete (repository.go) must build a real *T and write
// through Where(...).Select(...).Updates(&m) -- Model == Dest == &m --
// rather than tx.Model(&zero).Updates(map[string]any{...}), because gorm's
// SetupUpdateReflectValue sets Statement.ReflectValue to the (untouched,
// zero-valued) Model whenever Model != Dest, which the map-payload shape
// always is. Under that wrong shape, this test's Operation assertion would
// still pass, but After["deleted_at"] would silently come back nil even
// though the real SQL write set a real timestamp -- a lying audit trail on
// an otherwise "passing" test. Asserting the real, non-nil, matching
// deleted_at value is what actually exercises the fix.
func TestAuditCapturePlugin_SoftDelete_ClassifiesAsUpdateWithRealDiff(t *testing.T) {
	bus := &capturedBus{}
	db := openAuditCaptureTestDB(t, bus)
	if err := db.Exec(testutil.SoftDeletableWidgetTableSQL).Error; err != nil {
		t.Fatalf("create soft_deletable_widgets table: %v", err)
	}
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1"})

	repo := dbkit.NewRepository[testutil.SoftDeletableWidget](db)
	w := &testutil.SoftDeletableWidget{ID: "sdw1", Name: "gadget"}
	if err := repo.Create(ctx, w); err != nil {
		t.Fatalf("seed Create() error = %v", err)
	}

	before := time.Now()
	if err := repo.Delete(ctx, w.ID); err != nil {
		t.Fatalf("Delete() (soft-delete) error = %v", err)
	}
	after := time.Now()

	events := bus.captured()
	if len(events) != 2 {
		t.Fatalf("captured %d events, want 2 (create then soft-delete)", len(events))
	}
	got := events[1]
	if got.Operation != "update" {
		t.Errorf("Operation = %q, want %q (soft-delete is an UPDATE underneath, never captured with Delete semantics)", got.Operation, "update")
	}
	if got.ResourceID != "sdw1" {
		t.Errorf("ResourceID = %q, want %q", got.ResourceID, "sdw1")
	}
	if got.Before != nil {
		t.Errorf("Before = %+v, want nil -- the automatic capture mechanism never reads a pre-write snapshot for Update, soft-delete included (see WriteCapturedEvent.Before's own doc comment); this is not a gap to fix here", got.Before)
	}
	rawDeletedAt, ok := got.After["deleted_at"]
	if !ok || rawDeletedAt == nil {
		t.Fatalf("After[\"deleted_at\"] = %v (ok=%v), want a real, non-nil timestamp -- this is exactly the hazard TestAuditCapturePlugin_SoftDelete_ClassifiesAsUpdateWithRealDiff exists to catch: a map-payload Updates call would leave this nil even with the right Operation", rawDeletedAt, ok)
	}
	deletedAtPtr, ok := rawDeletedAt.(*time.Time)
	if !ok || deletedAtPtr == nil {
		t.Fatalf("After[\"deleted_at\"] = %v (%T), want a non-nil *time.Time", rawDeletedAt, rawDeletedAt)
	}
	if deletedAtPtr.Before(before.Add(-time.Second)) || deletedAtPtr.After(after.Add(time.Second)) {
		t.Errorf("After[\"deleted_at\"] = %v, want a timestamp between %v and %v", deletedAtPtr, before, after)
	}
	if got.After["deleted_by"] != "user-1" {
		t.Errorf("After[\"deleted_by\"] = %v, want %q", got.After["deleted_by"], "user-1")
	}

	// The second regression this test now also pins: softDelete's write is a
	// two-column UPDATE whose payload is a freshly built struct carrying
	// nothing but DeletedAt/DeletedBy — every other field is zero on that
	// payload. Capturing the whole payload as After would fabricate zero
	// values for the id/tenant_id/name columns the write never touched,
	// values that contradict the real row (which still holds name="gadget").
	// After must be scoped to exactly the columns the UPDATE assigned.
	//
	// softDelete's Select is the raw field names "DeletedAt"/"DeletedBy"
	// (repository.go), which GORM resolves to the DB column names
	// "deleted_at"/"deleted_by"; the assertion below uses the DB names
	// because After is keyed by DB column name (WriteCapturedEvent.After's
	// own doc comment).
	if len(got.After) != 2 {
		t.Errorf("After = %+v, want exactly the 2 columns this UPDATE wrote (deleted_at, deleted_by) -- capturing the untouched id/tenant_id/name columns would fabricate zero values for a row that still holds real ones", got.After)
	}
	for key := range got.After {
		if key != "deleted_at" && key != "deleted_by" {
			t.Errorf("After has key %q, want only deleted_at/deleted_by: a column this UPDATE never touched cannot truthfully appear in After", key)
		}
	}

	// Ground-truth cross-check: the row the soft-delete left behind must
	// agree with the captured After on every captured column — After is
	// documented as "the row's column values following the write", so a
	// captured value that differs from the real row is a lie by definition.
	rowFound, rowDeletedAt, rowDeletedBy := rawSoftDeletableWidgetRow(t, db, w.ID)
	if !rowFound {
		t.Fatal("row physically gone after Delete() on a SoftDeletable model, want it still present (mark-delete, not physical delete)")
	}
	if rowDeletedAt == nil {
		t.Fatal("row deleted_at = nil after a successful soft-delete, want a populated timestamp")
	}
	if rowDeletedAt.Before(before.Add(-time.Second)) || rowDeletedAt.After(after.Add(time.Second)) {
		t.Errorf("row deleted_at = %v, want a timestamp between %v and %v", rowDeletedAt, before, after)
	}
	if !deletedAtPtr.Equal(*rowDeletedAt) {
		t.Errorf("captured After[\"deleted_at\"] = %v does not equal the row's own deleted_at %v -- the captured After must describe the row state the write really produced", deletedAtPtr, rowDeletedAt)
	}
	if rowDeletedBy != "user-1" {
		t.Errorf("row deleted_by = %q, want %q", rowDeletedBy, "user-1")
	}
}

func TestAuditCapturePlugin_Delete_PublishesWriteCapturedEventWithResourceIDFromWhere(t *testing.T) {
	bus := &capturedBus{}
	db := openAuditCaptureTestDB(t, bus)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	w := &testutil.Widget{ID: "w1", Name: "gadget"}
	if err := db.WithContext(ctx).Create(w).Error; err != nil {
		t.Fatalf("seed Create() error = %v", err)
	}

	var zero testutil.Widget
	err := db.WithContext(ctx).Where("id = ?", "w1").Where("tenant_id = ?", "tenant-a").Delete(&zero).Error
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	events := bus.captured()
	if len(events) != 2 {
		t.Fatalf("captured %d events, want 2 (create then delete)", len(events))
	}
	got := events[1]
	if got.Operation != "delete" {
		t.Errorf("Operation = %q, want %q", got.Operation, "delete")
	}
	if got.ResourceID != "w1" {
		t.Errorf("ResourceID = %q, want %q (extracted from the WHERE clause, since a delete's Dest carries no field values)", got.ResourceID, "w1")
	}
	if got.After != nil {
		t.Errorf("After = %+v, want nil for a delete", got.After)
	}
}

// nonAuditableFlag is a TenantScoped-only fixture with no AuditResourceType
// method, proving the plugin leaves a non-Auditable model completely
// untouched -- the reverse-and-equally-important property tenantScopePlugin
// itself is already proven against in tenant_scope_test.go.
type nonAuditableFlag struct {
	ID       string `gorm:"primaryKey;size:26"`
	TenantID string `gorm:"primaryKey;size:26;not null"`
	Enabled  bool   `gorm:"not null"`
}

func (f nonAuditableFlag) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(f.TenantID) }
func (nonAuditableFlag) TableName() string               { return "non_auditable_flags" }

var _ dbkit.TenantScoped = nonAuditableFlag{}

// errAuditCaptureFailingHookAlwaysFails is the sentinel
// auditCaptureFailingHookWidget's AfterCreate hook always returns.
var errAuditCaptureFailingHookAlwaysFails = errors.New("audit_capture_test: AfterCreate always fails")

// auditCaptureFailingHookWidget is a throwaway Auditable model whose
// AfterCreate hook always fails, used only by
// TestAuditCapturePlugin_BareWrite_RollbackAfterCapture_PublishesNothing to
// reproduce, for a bare (non-WithTenantSession) write, a real rollback of
// the implicit per-statement transaction GORM opens for it — after this
// plugin's own capture callback has already run (see that test's own doc
// comment for exactly why AfterCreate is the right hook for this). It
// deliberately does not implement dbkit.TenantScoped: nothing about this
// fixture needs tenant scoping, and isTenantScopedValue (tenant_scope.go)
// simply skips a model that does not implement it, exactly like
// nonAuditableFlag above.
type auditCaptureFailingHookWidget struct {
	ID   string `gorm:"primaryKey;size:26"`
	Name string `gorm:"size:255;not null"`
}

func (auditCaptureFailingHookWidget) TableName() string { return "audit_capture_failing_hook_widgets" }

// AuditResourceType satisfies dbkit.Auditable.
func (auditCaptureFailingHookWidget) AuditResourceType() string { return "failing_hook_widget" }

// AfterCreate is a real GORM model hook (gorm.AfterCreateInterface),
// invoked by GORM's own "gorm:after_create" callback — strictly after this
// plugin's After("gorm:create") capture callback runs, in the same Create
// Process chain. It always fails, which GORM's callbacks.AfterCreate folds
// into db.Error via db.AddError, in turn making
// callbacks.CommitOrRollbackTransaction roll back the real per-statement
// transaction this bare Create opened.
func (auditCaptureFailingHookWidget) AfterCreate(tx *gorm.DB) error {
	return errAuditCaptureFailingHookAlwaysFails
}

var _ dbkit.Auditable = auditCaptureFailingHookWidget{}

func TestAuditCapturePlugin_NonAuditableModel_PublishesNothing(t *testing.T) {
	bus := &capturedBus{}
	db := openAuditCaptureTestDB(t, bus)
	if err := db.Exec(`CREATE TABLE non_auditable_flags (
		id        VARCHAR(26) NOT NULL,
		tenant_id VARCHAR(26) NOT NULL,
		enabled   BOOLEAN     NOT NULL,
		PRIMARY KEY (tenant_id, id)
	)`).Error; err != nil {
		t.Fatalf("create non_auditable_flags table: %v", err)
	}

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	f := &nonAuditableFlag{ID: "f1", Enabled: true}
	if err := db.WithContext(ctx).Create(f).Error; err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if events := bus.captured(); len(events) != 0 {
		t.Errorf("captured %d events for a non-Auditable model, want 0", len(events))
	}
}

// captureSlogDefault swaps slog.Default() for a text handler writing into a
// buffer this returns, restoring the previous default logger on test
// cleanup. It is this file's twin of go/pkgcore/registry_test.go's own
// TestBootstrap_WarnsOncePerNonSurvivingStatefulSeam swap — slog's default
// logger is process-global, so a test using this must not run in parallel
// with another that also touches it; none of this file's tests call
// t.Parallel.
func captureSlogDefault(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	// Level: slog.LevelWarn (rather than the LevelError this helper used
	// before dbkit-tenancy P2-2's fix added a Warn-level alert of its own,
	// auditTenantMismatch) still captures every existing Error-level alert
	// this file's other tests assert on -- LevelWarn is strictly lower, so
	// the filter "handle anything >= this level" still passes Error
	// records through unchanged -- while now also capturing the new
	// Warn-level one.
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// TestAuditCapturePlugin_BareWrite_RollbackAfterCapture_PublishesNothing
// pins the same property TestAuditCapturePlugin_WithTenantSession_Rollback...
// below reproduces as a real, fail-before/pass-after regression, but for
// the bare-write shape: a Create/Update/Delete issued directly against
// Open's plain *gorm.DB (no WithTenantSession), never going through
// Repository[T] or any other speed module today — confirmed by grep
// across the whole tree: the only types implementing dbkit.Auditable are
// this package's own test fixtures (testutil.Widget,
// testutil.SoftDeletableWidget, and auditCaptureFailingHookWidget below)
// and examples/reference-app/internal/notes.Note, and Note is written
// exclusively through dbkit.Repository[T] (every call in
// notes/repository.go goes through WithTenantSession) — so this bare
// shape has no real production caller today, but the mechanism must
// still handle it correctly, since dbkit cannot assume every future
// Auditable model will be Repository[T]-backed.
//
// This one is NOT a fail-before/pass-after regression, and its own doc
// comment says so rather than overclaiming: investigating exactly where
// GORM's callback sort (gorm.io/gorm@v1.31.2/callbacks.go's
// sortCallbacks) places an After("gorm:create")-only registration showed
// it appends to the very end of the already-fully-sorted default chain
// whenever "gorm:create" itself was registered first (true here — Open's
// db.Use(newAuditCapturePlugin(...)) always runs after gorm.Open's own
// RegisterDefaultCallbacks) — confirmed empirically too, with a temporary
// debug print, while designing this test: by the time the pre-fix
// single capture callback ran for this model, db.Error already carried
// AfterCreate's own failure. So capture's pre-existing
// "if db.Error != nil { return }" guard already prevented a phantom
// publish in this exact shape, by accident, before this round's fix —
// this test pins that the split into capture (After "gorm:create") and
// publishPending (After "gorm:commit_or_rollback_transaction") keeps that
// property, explicitly and by design rather than by a GORM sort-order
// coincidence a future GORM version could change. The real, reproducible
// bug this shape does NOT protect against — a transaction opened outside
// GORM's own per-statement chain entirely — is
// TestAuditCapturePlugin_WithTenantSession_RollbackAfterCapture_PublishesNothing
// below.
func TestAuditCapturePlugin_BareWrite_RollbackAfterCapture_PublishesNothing(t *testing.T) {
	bus := &capturedBus{}
	db := openAuditCaptureTestDB(t, bus)
	if err := db.Exec(`CREATE TABLE audit_capture_failing_hook_widgets (
		id   VARCHAR(26)  NOT NULL PRIMARY KEY,
		name VARCHAR(255) NOT NULL
	)`).Error; err != nil {
		t.Fatalf("create audit_capture_failing_hook_widgets table: %v", err)
	}

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	w := &auditCaptureFailingHookWidget{ID: "w1", Name: "gadget"}
	err := db.WithContext(ctx).Create(w).Error
	if !errors.Is(err, errAuditCaptureFailingHookAlwaysFails) {
		t.Fatalf("Create() error = %v, want it to wrap the AfterCreate hook's own error (errAuditCaptureFailingHookAlwaysFails)", err)
	}

	if events := bus.captured(); len(events) != 0 {
		t.Errorf("captured %d events for a write whose real per-statement transaction rolled back after capture ran, want 0 (pre-fix: capture published synchronously before AfterCreate/commit even ran, so a rollback here left a phantom audit row)", len(events))
	}

	var count int64
	if err := db.Raw(`SELECT count(*) FROM audit_capture_failing_hook_widgets WHERE id = ?`, "w1").Scan(&count).Error; err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 0 {
		t.Errorf("row count for id=w1 = %d, want 0 (the real transaction rolled back, so the row must not exist either)", count)
	}
}

// TestAuditCapturePlugin_WithTenantSession_RollbackAfterCapture_PublishesNothing
// is dbkit-tenancy P1-1's regression for the primary, real-production
// shape: a write inside dbkit.WithTenantSession's transaction — the shape
// examples/reference-app/internal/notes.Note (dbkit's one real Auditable
// production consumer) always uses, and the shape every
// dbkit.Repository[T] write uses underneath.
//
// The fn passed to WithTenantSession creates an Auditable widget (letting
// capture run and, pre-fix, publish synchronously) and then deliberately
// returns a non-nil error — reproducing, with a REAL gorm.DB.Transaction
// rollback against a REAL in-process SQLite database (never a mocked
// bus or a mocked transaction), a business transaction whose
// audit-relevant write already happened but whose surrounding transaction
// ultimately failed. Pre-fix, the event was already on the bus by the
// time fn returned its error, since capture published inside the
// "gorm:create" callback long before WithTenantSession's own
// db.Transaction call could roll back — a phantom event for a row that
// was never durably created.
func TestAuditCapturePlugin_WithTenantSession_RollbackAfterCapture_PublishesNothing(t *testing.T) {
	bus := &capturedBus{}
	db := openAuditCaptureTestDB(t, bus)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	w := &testutil.Widget{ID: "w1", Name: "gadget"}
	forceRollback := errors.New("audit_capture_test: forced rollback after the audited write")

	err := dbkit.WithTenantSession(ctx, db, func(tx *gorm.DB) error {
		if err := tx.Create(w).Error; err != nil {
			return err
		}
		return forceRollback
	})
	if !errors.Is(err, forceRollback) {
		t.Fatalf("WithTenantSession() error = %v, want it to wrap forceRollback", err)
	}

	if events := bus.captured(); len(events) != 0 {
		t.Errorf("captured %d events for a WithTenantSession transaction that rolled back after the audited write, want 0 (pre-fix: capture published synchronously inside the Create call, before fn's forced error ever reached db.Transaction)", len(events))
	}

	var count int64
	if err := db.Raw(`SELECT count(*) FROM widgets WHERE id = ? AND tenant_id = ?`, "w1", "tenant-a").Scan(&count).Error; err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 0 {
		t.Errorf("row count for id=w1 = %d, want 0 (the transaction rolled back, so the row must not exist either)", count)
	}
}

// TestAuditCapturePlugin_BareWrite_PublishFailure_CommitsAndAlerts is
// dbkit-tenancy P1-2's regression for the bare-write shape (see the P1-1
// test above for why this shape has no real production caller today, and
// why the mechanism must still handle it). It replaces the pre-fix
// TestAuditCapturePlugin_PublishFailure_FailsTheWriteLoudly, which
// asserted the very bug this round fixes as intended behavior: a
// transient audit-bus outage failing the business write itself.
//
// Post-fix, the write's own real per-statement transaction has already
// committed by the time publishPending calls Publish (see
// audit_capture.go's Initialize / publishPending doc comments), so a
// Publish failure at that point can no longer roll back a write that has
// already durably happened — it is reported as a structured alert
// instead (auditPublishFailed), never surfaced as the triggering
// Create/Update/Delete call's own error.
func TestAuditCapturePlugin_BareWrite_PublishFailure_CommitsAndAlerts(t *testing.T) {
	publishErr := errors.New("bus unavailable")
	bus := &capturedBus{fail: publishErr}
	db := openAuditCaptureTestDB(t, bus)
	logs := captureSlogDefault(t)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	w := &testutil.Widget{ID: "w1", Name: "gadget"}
	if err := db.WithContext(ctx).Create(w).Error; err != nil {
		t.Fatalf("Create() error = %v, want nil (docs/internal/10-compliance-and-audit.md: a publish failure after commit must alert, never fail a write that already durably happened)", err)
	}

	var count int64
	if err := db.Raw(`SELECT count(*) FROM widgets WHERE id = ? AND tenant_id = ?`, "w1", "tenant-a").Scan(&count).Error; err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Fatalf("row count for id=w1 = %d, want 1 (the write must durably commit even though its audit publish failed)", count)
	}

	out := logs.String()
	for _, want := range []string{
		"dbkit: audit event publish failed after commit",
		"resource_type=widget",
		"operation=create",
		"tenant_id=tenant-a",
		"bus unavailable",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("alert log = %q, want it to contain %q", out, want)
		}
	}
}

// TestAuditCapturePlugin_WithTenantSession_PublishFailure_CommitsAndAlerts
// is dbkit-tenancy P1-2's regression for the primary, real-production
// shape (WithTenantSession / Repository[T] — see the P1-1 WithTenantSession
// test above). It drives a real Auditable write through WithTenantSession
// with a bus whose Publish always fails, and asserts the business write
// still durably commits — read back through a second, independent
// connection to the same SQLite file, never the same *gorm.DB the write
// went through, so this cannot pass merely because of an in-process cache
// — a fresh session proves it landed for real — while the alert fires.
func TestAuditCapturePlugin_WithTenantSession_PublishFailure_CommitsAndAlerts(t *testing.T) {
	publishErr := errors.New("bus unavailable")
	bus := &capturedBus{fail: publishErr}
	dsn := fmt.Sprintf("file:audit_capture_wts_publish_failure_%d?mode=memory&cache=shared", auditCaptureTestDBSeq.Add(1))
	db, err := dbkit.Open(context.Background(), dbkit.Options{
		Dialect:  dbkit.DialectSQLite,
		DSN:      dsn,
		AuditBus: bus,
	})
	if err != nil {
		t.Fatalf("dbkit.Open: %v", err)
	}
	createWidgetsTable(t, db)
	logs := captureSlogDefault(t)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	w := &testutil.Widget{ID: "w1", Name: "gadget"}
	if wtsErr := dbkit.WithTenantSession(ctx, db, func(tx *gorm.DB) error {
		return tx.Create(w).Error
	}); wtsErr != nil {
		t.Fatalf("WithTenantSession() error = %v, want nil (a publish failure after commit must alert, never fail a write that already durably committed)", wtsErr)
	}

	freshDB, err := dbkit.Open(context.Background(), dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     dsn,
	})
	if err != nil {
		t.Fatalf("dbkit.Open (fresh session): %v", err)
	}
	var count int64
	if err := freshDB.Raw(`SELECT count(*) FROM widgets WHERE id = ? AND tenant_id = ?`, "w1", "tenant-a").Scan(&count).Error; err != nil {
		t.Fatalf("count query on fresh session: %v", err)
	}
	if count != 1 {
		t.Fatalf("row count for id=w1 on a fresh session = %d, want 1 (the write must durably commit even though its audit publish failed)", count)
	}

	out := logs.String()
	for _, want := range []string{
		"dbkit: audit event publish failed after commit",
		"resource_type=widget",
		"operation=create",
		"tenant_id=tenant-a",
		"bus unavailable",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("alert log = %q, want it to contain %q", out, want)
		}
	}
}

// TestAuditCapturePlugin_WithTenantSession_SameFileSynchronousPersister_NoLongerDeadlocks
// is not one of dbkit-tenancy's two mandated regressions — it exists to
// verify, empirically, a consequential side claim before touching any
// prose about it: go/dbkit/AGENTS.md's "Audit trail collection" Known
// limitation section, and docs/internal/10-compliance-and-audit.md's own
// stale implementation-status note on this exact mechanism, both describe
// (and the doc 10 note explicitly predicts the fix for) a same-goroutine
// SQLITE_BUSY self-deadlock: a synchronous persister subscriber writing to
// the SAME SQLite file the audited write itself used, from the SAME
// goroutine, while that audited write's own transaction was STILL OPEN.
// Both docs name the fix as "defer the plugin's publish until after the
// enclosing transaction actually commits" — precisely this round's
// change for the WithTenantSession/Repository[T] shape.
//
// This drives that exact real scenario against two real *gorm.DB
// connections to one real (temp-file, not in-memory) SQLite database: dbA
// carries the AuditBus, whose subscriber synchronously writes to dbB — a
// second connection to the same file — exactly mirroring the shape
// go/dbkit/AGENTS.md's Known limitation describes for the automatic
// mechanism paired with go/dbkit/audit's own persister. Pre-fix, the
// subscriber's write races the still-open audited transaction on the same
// file and either waits out busy_timeout or fails immediately (an upgrade
// shape); post-fix, publishBuffered runs only after dbA's own transaction
// has already committed and released its lock, so dbB's write meets no
// contention at all.
func TestAuditCapturePlugin_WithTenantSession_SameFileSynchronousPersister_NoLongerDeadlocks(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "audit_capture_samefile.db")

	bus := pkgcore.NewMemoryEventBus()

	dbA, err := dbkit.Open(context.Background(), dbkit.Options{
		Dialect:  dbkit.DialectSQLite,
		DSN:      dsn,
		AuditBus: bus,
	})
	if err != nil {
		t.Fatalf("dbkit.Open (dbA): %v", err)
	}
	createWidgetsTable(t, dbA)
	if createErr := dbA.Exec(`CREATE TABLE persisted_marks (id VARCHAR(26) PRIMARY KEY)`).Error; createErr != nil {
		t.Fatalf("create persisted_marks table: %v", createErr)
	}

	dbB, err := dbkit.Open(context.Background(), dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     dsn,
	})
	if err != nil {
		t.Fatalf("dbkit.Open (dbB, same file, no AuditBus): %v", err)
	}

	var persisterCalled bool
	var persistErr error
	bus.Subscribe(dbkit.EventWriteCaptured, func(ctx context.Context, _ pkgcore.Event) error {
		persisterCalled = true
		// A synchronous, same-goroutine write to the SAME file dbA just
		// wrote through, on a genuinely separate connection — exactly the
		// go/dbkit/AGENTS.md-described persister shape.
		persistErr = dbB.WithContext(ctx).Exec(`INSERT INTO persisted_marks (id) VALUES (?)`, "marked-w1").Error
		return persistErr
	})

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	w := &testutil.Widget{ID: "w1", Name: "gadget"}
	if err := dbkit.WithTenantSession(ctx, dbA, func(tx *gorm.DB) error {
		return tx.Create(w).Error
	}); err != nil {
		t.Fatalf("WithTenantSession() error = %v, want nil", err)
	}

	if !persisterCalled {
		t.Fatal("the persister subscriber was never called")
	}
	if persistErr != nil {
		t.Errorf("same-file persister write error = %v, want nil (post-fix, the audited transaction has already committed and released its lock by the time the persister's own write on a second connection runs)", persistErr)
	}

	var count int64
	if err := dbA.Raw(`SELECT count(*) FROM persisted_marks WHERE id = ?`, "marked-w1").Scan(&count).Error; err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Errorf("persisted_marks row count = %d, want 1", count)
	}
}

// auditCaptureSecretWidget is a throwaway Auditable model carrying one
// GORM-serializer field (dbkit's own encrypted-field mechanism), used only
// by TestAuditCapturePlugin_SerializerField_RedactsRatherThanCrashOrLeak to
// reproduce the write-capture plugin's handling of a serializer field --
// mirroring encryption_test.go's own encryptedFieldModel fixture, which
// this package cannot reuse directly since it lives in the internal
// dbkit package and this file is dbkit_test.
type auditCaptureSecretWidget struct {
	ID       string `gorm:"primaryKey;size:26"`
	TenantID string `gorm:"primaryKey;size:26;not null"`
	Phone    string `gorm:"column:phone;serializer:dbkit_audit_capture_test_phone_encrypted"`
}

// TableName pins the table name so it does not depend on GORM's
// pluralization of an unexported type name.
func (auditCaptureSecretWidget) TableName() string { return "audit_capture_secret_widgets" }

// GetTenantID satisfies dbkit's tenant-scoping contract, which
// dbkit.Open's plugin chain requires of every model it processes.
func (w auditCaptureSecretWidget) GetTenantID() pkgcore.TenantID {
	return pkgcore.TenantID(w.TenantID)
}

// AuditResourceType satisfies dbkit.Auditable.
func (auditCaptureSecretWidget) AuditResourceType() string { return "secret_widget" }

// TestAuditCapturePlugin_SerializerField_RedactsRatherThanCrashOrLeak
// reproduces the bug recorded in go/dbkit/AGENTS.md's "Audit trail
// collection" section: before the fix, fieldValuesMap called
// field.ValueOf on a GORM-serializer field (dbkit's own
// RegisterEncryptedSerializer mechanism, used for any encrypted PII
// column) and got back GORM's internal *schema.serializer wrapper --
// which embeds a self-referential *schema.Field and so cannot be
// json.Marshal'd (distributed mode's RedisEventBus.Publish and
// standalone mode's audit.changesJSON both marshal it, and both would
// fail: distributed mode fails the triggering write itself via
// db.AddError, standalone mode silently drops the diff).
//
// This test proves the fixed behavior: the write succeeds, the captured
// event's After map holds a redacted marker rather than GORM's unmarshalable
// wrapper *and* rather than the phone number's plaintext (writing the
// plaintext into the audit trail would itself violate the "no plaintext
// PII in logs/traces/API responses" security rule -- redacting is the only
// safe capture here), and the captured value round-trips through
// json.Marshal exactly as audit.changesJSON needs it to.
func TestAuditCapturePlugin_SerializerField_RedactsRatherThanCrashOrLeak(t *testing.T) {
	key := sha256.Sum256([]byte("audit-capture-serializer-field-test-key"))
	cipher, err := dbkit.NewCipher(key[:])
	if err != nil {
		t.Fatalf("dbkit.NewCipher() error = %v", err)
	}
	dbkit.RegisterEncryptedSerializer("dbkit_audit_capture_test_phone_encrypted", cipher)

	bus := &capturedBus{}
	db := openAuditCaptureTestDB(t, bus)
	if err := db.Exec(`CREATE TABLE audit_capture_secret_widgets (
		id        VARCHAR(26) NOT NULL,
		tenant_id VARCHAR(26) NOT NULL,
		phone     BLOB        NOT NULL,
		PRIMARY KEY (tenant_id, id)
	)`).Error; err != nil {
		t.Fatalf("create audit_capture_secret_widgets table: %v", err)
	}

	const plaintext = "+15550100777"
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	w := &auditCaptureSecretWidget{ID: "w1", TenantID: "tenant-a", Phone: plaintext}
	if err := db.WithContext(ctx).Create(w).Error; err != nil {
		t.Fatalf("Create() error = %v, want the write to succeed for a model with a serializer field", err)
	}

	events := bus.captured()
	if len(events) != 1 {
		t.Fatalf("captured %d events, want exactly 1", len(events))
	}
	got, ok := events[0].After["phone"]
	if !ok {
		t.Fatalf("After = %+v, want a \"phone\" key", events[0].After)
	}
	gotStr, ok := got.(string)
	if !ok {
		t.Fatalf("After[\"phone\"] = %#v (%T), want a plain redacted string, not GORM's internal serializer wrapper", got, got)
	}
	if gotStr != "[redacted]" {
		t.Errorf("After[\"phone\"] = %q, want the redacted marker \"[redacted]\" (must never be the plaintext phone number)", gotStr)
	}
	if gotStr == plaintext {
		t.Fatalf("After[\"phone\"] leaked the plaintext phone number into the audit trail")
	}

	// The concrete regression: audit.changesJSON (go/dbkit/audit/module.go)
	// and RedisEventBus.Publish both json.Marshal this map before the fix
	// existed, this failed with "json: unsupported value: encountered a
	// cycle via *schema.Field" because After["phone"] held GORM's
	// self-referential *schema.serializer wrapper instead of a plain value.
	if _, err := json.Marshal(events[0].After); err != nil {
		t.Fatalf("json.Marshal(After) error = %v, want captured field values to always be JSON-marshalable", err)
	}
}

// TestAuditCapturePlugin_Restore_CapturesOnlyTheColumnsItWrites pins the
// same After-scoping regression as the soft-delete test above, for the
// inverse write: Restore (repository.go) issues the identical two-column,
// fresh-struct UPDATE shape against a soft-deleted row, so its captured
// After must likewise carry exactly deleted_at/deleted_by — with nil and ""
// being the truthful post-restore state — and never fabricate values for
// the untouched id/tenant_id/name columns (which the real row still holds
// after the restore).
func TestAuditCapturePlugin_Restore_CapturesOnlyTheColumnsItWrites(t *testing.T) {
	bus := &capturedBus{}
	db := openAuditCaptureTestDB(t, bus)
	if err := db.Exec(testutil.SoftDeletableWidgetTableSQL).Error; err != nil {
		t.Fatalf("create soft_deletable_widgets table: %v", err)
	}
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1"})

	repo := dbkit.NewRepository[testutil.SoftDeletableWidget](db)
	w := &testutil.SoftDeletableWidget{ID: "sdw1", Name: "gadget"}
	if err := repo.Create(ctx, w); err != nil {
		t.Fatalf("seed Create() error = %v", err)
	}
	if err := repo.Delete(ctx, w.ID); err != nil {
		t.Fatalf("seed Delete() (soft-delete) error = %v", err)
	}
	if err := repo.Restore(ctx, w.ID); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}

	events := bus.captured()
	if len(events) != 3 {
		t.Fatalf("captured %d events, want 3 (create, soft-delete, restore)", len(events))
	}
	got := events[2]
	if got.Operation != "update" {
		t.Errorf("Operation = %q, want %q (Restore is an UPDATE underneath, exactly like soft-delete)", got.Operation, "update")
	}
	if got.ResourceID != "sdw1" {
		t.Errorf("ResourceID = %q, want %q", got.ResourceID, "sdw1")
	}
	if len(got.After) != 2 {
		t.Errorf("After = %+v, want exactly the 2 columns Restore's UPDATE wrote (deleted_at, deleted_by) -- the untouched id/tenant_id/name columns must not appear as fabricated zeroes", got.After)
	}
	for key := range got.After {
		if key != "deleted_at" && key != "deleted_by" {
			t.Errorf("After has key %q, want only deleted_at/deleted_by: a column this UPDATE never touched cannot truthfully appear in After", key)
		}
	}
	at, ok := got.After["deleted_at"]
	atPtr, isTime := at.(*time.Time)
	if !ok || !isTime || atPtr != nil {
		t.Errorf("After[\"deleted_at\"] = %#v (ok=%v), want an explicit nil *time.Time -- the restored row's deleted_at is NULL", at, ok)
	}
	if got.After["deleted_by"] != "" {
		t.Errorf("After[\"deleted_by\"] = %v, want \"\" -- the restored row's deleted_by is the empty string", got.After["deleted_by"])
	}

	// Ground truth: the restored row must agree with the captured After.
	rowFound, rowDeletedAt, rowDeletedBy := rawSoftDeletableWidgetRow(t, db, w.ID)
	if !rowFound {
		t.Fatal("row physically gone after Restore(), want it still present")
	}
	if rowDeletedAt != nil {
		t.Errorf("row deleted_at = %v after Restore(), want nil (restored row is live again)", rowDeletedAt)
	}
	if rowDeletedBy != "" {
		t.Errorf("row deleted_by = %q after Restore(), want \"\"", rowDeletedBy)
	}
}

// TestAuditCapturePlugin_SoftDelete_DoubleDelete_PublishesNothing pins the
// RowsAffected == 0 guard in capture(): a second Repository.Delete against an
// already-soft-deleted row matches no row (the row is hidden behind its own
// deleted_at IS NOT NULL scope), and a write that matched nothing changed no
// row state — publishing would fabricate a second, false deletion record
// carrying a fresh deleted_at the real row does not have. The Repository
// surfaces the same nothing-matched situation as ErrRecordNotFound, so
// skipping the event loses no signal.
func TestAuditCapturePlugin_SoftDelete_DoubleDelete_PublishesNothing(t *testing.T) {
	bus := &capturedBus{}
	db := openAuditCaptureTestDB(t, bus)
	if err := db.Exec(testutil.SoftDeletableWidgetTableSQL).Error; err != nil {
		t.Fatalf("create soft_deletable_widgets table: %v", err)
	}
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1"})

	repo := dbkit.NewRepository[testutil.SoftDeletableWidget](db)
	w := &testutil.SoftDeletableWidget{ID: "sdw1", Name: "gadget"}
	if err := repo.Create(ctx, w); err != nil {
		t.Fatalf("seed Create() error = %v", err)
	}
	if err := repo.Delete(ctx, w.ID); err != nil {
		t.Fatalf("first Delete() (soft-delete) error = %v", err)
	}
	err := repo.Delete(ctx, w.ID)
	if !isRecordNotFound(err) {
		t.Fatalf("second Delete() error = %v, want ErrRecordNotFound (the row is already soft-deleted, so nothing matched)", err)
	}

	if events := bus.captured(); len(events) != 2 {
		t.Errorf("captured %d events, want exactly 2 (create, then the one real soft-delete) -- the double delete matched no row and must publish nothing", len(events))
	}
}

// TestAuditCapturePlugin_UpdateMatchingNoRows_PublishesNothing pins the same
// RowsAffected == 0 guard for the full-record Update path: an Update whose
// WHERE matched nothing (here: a Repository.Update of a model that was never
// created) changed no row state, and the Repository reports the nothing-
// matched situation to its caller as ErrRecordNotFound — capture must not add
// a fabricated affirmative After for a row the write never touched.
func TestAuditCapturePlugin_UpdateMatchingNoRows_PublishesNothing(t *testing.T) {
	bus := &capturedBus{}
	db := openAuditCaptureTestDB(t, bus)
	if err := db.Exec(testutil.SoftDeletableWidgetTableSQL).Error; err != nil {
		t.Fatalf("create soft_deletable_widgets table: %v", err)
	}
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	repo := dbkit.NewRepository[testutil.SoftDeletableWidget](db)
	w := &testutil.SoftDeletableWidget{ID: "sdw1", Name: "gadget"}
	if err := repo.Create(ctx, w); err != nil {
		t.Fatalf("seed Create() error = %v", err)
	}
	ghost := &testutil.SoftDeletableWidget{ID: "sdw-never-created", Name: "ghost"}
	err := repo.Update(ctx, ghost)
	if !isRecordNotFound(err) {
		t.Fatalf("Update() of a never-created id error = %v, want ErrRecordNotFound (nothing matched)", err)
	}

	if events := bus.captured(); len(events) != 1 {
		t.Errorf("captured %d events, want exactly 1 (the seed create) -- an Update that matched no row must publish nothing", len(events))
	}
}

// TestAuditCapturePlugin_DeleteMatchingNoRows_PublishesNothing pins the same
// RowsAffected == 0 guard for the hard-Delete path: a Repository.Delete of an
// id that was never created matches no row, the Repository reports
// ErrRecordNotFound, and capture must not publish a delete event claiming a
// row was removed when no row state changed.
func TestAuditCapturePlugin_DeleteMatchingNoRows_PublishesNothing(t *testing.T) {
	bus := &capturedBus{}
	db := openAuditCaptureTestDB(t, bus)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	repo := dbkit.NewRepository[testutil.Widget](db)
	w := &testutil.Widget{ID: "w1", Name: "gadget"}
	if err := repo.Create(ctx, w); err != nil {
		t.Fatalf("seed Create() error = %v", err)
	}
	err := repo.Delete(ctx, "w-never-created")
	if !isRecordNotFound(err) {
		t.Fatalf("Delete() of a never-created id error = %v, want ErrRecordNotFound (nothing matched)", err)
	}

	if events := bus.captured(); len(events) != 1 {
		t.Errorf("captured %d events, want exactly 1 (the seed create) -- a Delete that matched no row must publish nothing", len(events))
	}
}

// auditCaptureHardDeletePurpose is the SystemPurpose the hard-delete capture
// tests grant themselves, registered from the test process exactly as
// tenancy's own system-context tests do (these tests exercise the gate, not
// the legitimacy of a grant).
const auditCaptureHardDeletePurpose pkgcore.SystemPurpose = "dbkit.test.audit_capture_hard_delete"

// auditCaptureHardDeleteCtx layers a granted system context onto base —
// which carries the tenant and actor the writes below use, so the caller
// identity captured on the event stays the same user across the whole
// create/soft-delete/hard-delete flow — returning the context a
// Repository.HardDelete call requires. RegisterSystemPurpose is idempotent
// and mutex-guarded, so the registration here is a no-op from the second
// call on.
func auditCaptureHardDeleteCtx(t *testing.T, base context.Context) context.Context {
	t.Helper()
	pkgcore.RegisterSystemPurpose(auditCaptureHardDeletePurpose)
	elevated, err := pkgcore.WithSystemContext(base, pkgcore.SystemReason{
		Actor:   "audit-capture-test",
		Purpose: auditCaptureHardDeletePurpose,
		Ticket:  "dbkit-audit-capture-hard-delete-test",
	})
	if err != nil {
		t.Fatalf("WithSystemContext() error = %v", err)
	}
	return elevated
}

// TestAuditCapturePlugin_HardDelete_ClassifiesAsDelete pins the audit
// semantics of Repository[T].HardDelete (hard_delete.go): the method is a
// genuine physical DELETE, so the write-capture plugin must classify it as
// Operation "delete" with After nil — the row is gone, so there is no
// after-image to record — distinctly from the Update semantics the same
// row's soft-delete step carried a moment earlier. This is the design's
// mandated pair (docs/internal/04-data-and-tenancy.md's delete-semantics
// section, §4: soft-delete captures as Update with the deleted_at diff,
// hard-delete captures as Delete for a vanished row, and the landing round
// must pin both by explicit test assertion). It is also the regression guard
// for the same section's named pitfall: HardDelete must never hand-emit a
// second, hand-rolled Delete event on top of the automatic capture — the
// count assertion below fails the moment a duplicate event appears, and the
// capturedBus itself loudly rejects any payload that is not a
// WriteCapturedEvent.
func TestAuditCapturePlugin_HardDelete_ClassifiesAsDelete(t *testing.T) {
	bus := &capturedBus{}
	db := openAuditCaptureTestDB(t, bus)
	if err := db.Exec(testutil.SoftDeletableWidgetTableSQL).Error; err != nil {
		t.Fatalf("create soft_deletable_widgets table: %v", err)
	}

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1"})

	repo := dbkit.NewRepository[testutil.SoftDeletableWidget](db)
	w := &testutil.SoftDeletableWidget{ID: "sdw1", Name: "gadget"}
	if err := repo.Create(ctx, w); err != nil {
		t.Fatalf("seed Create() error = %v", err)
	}
	if err := repo.Delete(ctx, w.ID); err != nil {
		t.Fatalf("Delete() (soft-delete) error = %v", err)
	}
	if err := repo.HardDelete(auditCaptureHardDeleteCtx(t, ctx), w.ID); err != nil {
		t.Fatalf("HardDelete() error = %v", err)
	}

	events := bus.captured()
	if len(events) != 3 {
		t.Fatalf("captured %d events, want exactly 3 (create, soft-delete, hard-delete) -- a HardDelete that hand-emitted its own duplicate Delete event on top of the automatic capture would publish a fourth", len(events))
	}
	// The design's contrast pair, both pinned on this one row: the
	// soft-delete step just above was captured as "update" (probed in depth
	// by TestAuditCapturePlugin_SoftDelete_ClassifiesAsUpdateWithRealDiff),
	// and the hard-delete step must be captured as "delete" — the two halves
	// of the delete semantics leave differently classified trails.
	if got := events[1]; got.Operation != "update" {
		t.Errorf("soft-delete Operation = %q, want %q (the preamble event must be the soft-delete's update before the hard-delete's delete is asserted)", got.Operation, "update")
	}
	got := events[2]
	if got.Operation != "delete" {
		t.Errorf("HardDelete Operation = %q, want %q -- HardDelete is a physical DELETE and must be captured with Delete semantics, never Update", got.Operation, "delete")
	}
	if got.ResourceID != w.ID {
		t.Errorf("ResourceID = %q, want %q (extracted from HardDelete's WHERE clause, exactly as Delete's physical branch is)", got.ResourceID, w.ID)
	}
	if got.After != nil {
		t.Errorf("After = %+v, want nil for a delete: the row is physically gone after HardDelete, so there is no after-image to record", got.After)
	}
	if got.Before != nil {
		t.Errorf("Before = %+v, want nil -- the automatic capture mechanism never reads a pre-write snapshot (see WriteCapturedEvent.Before's own doc comment)", got.Before)
	}
	if got.TenantID != "tenant-a" {
		t.Errorf("TenantID = %q, want %q", got.TenantID, "tenant-a")
	}
	if got.Actor.ID != "user-1" {
		t.Errorf("Actor.ID = %q, want %q (the capture reads the actor from the write's context, system context or not)", got.Actor.ID, "user-1")
	}
	if got.Table != "soft_deletable_widgets" {
		t.Errorf("Table = %q, want %q", got.Table, "soft_deletable_widgets")
	}
	if got.ResourceType != "soft_deletable_widget" {
		t.Errorf("ResourceType = %q, want %q", got.ResourceType, "soft_deletable_widget")
	}

	// Ground truth: the physical row must be gone, agreeing with the captured
	// Operation/After-nil pair — an event that says "delete" while the row
	// still sat in the table would be a lie about what happened.
	if found, _, _ := rawSoftDeletableWidgetRow(t, db, w.ID); found {
		t.Fatal("row still physically present after HardDelete; the captured delete event must describe a real physical erasure")
	}
}

// TestAuditCapturePlugin_HardDelete_NoMatchingRow_PublishesNothing pins the
// RowsAffected == 0 guard in capture() for HardDelete's no-match path: a
// HardDelete of an id no row carries — never created, or already erased by
// an earlier HardDelete — changed no row state, the Repository reports
// ErrRecordNotFound, and capture must not publish a delete event claiming a
// row vanished when none did. It is the HardDelete twin of the existing
// TestAuditCapturePlugin_DeleteMatchingNoRows_PublishesNothing, warranted
// here because HardDelete is a separately gated entry whose audit behaviour
// this round is pinning wholesale.
func TestAuditCapturePlugin_HardDelete_NoMatchingRow_PublishesNothing(t *testing.T) {
	bus := &capturedBus{}
	db := openAuditCaptureTestDB(t, bus)
	if err := db.Exec(testutil.SoftDeletableWidgetTableSQL).Error; err != nil {
		t.Fatalf("create soft_deletable_widgets table: %v", err)
	}

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	repo := dbkit.NewRepository[testutil.SoftDeletableWidget](db)
	w := &testutil.SoftDeletableWidget{ID: "sdw1", Name: "gadget"}
	if err := repo.Create(ctx, w); err != nil {
		t.Fatalf("seed Create() error = %v", err)
	}

	err := repo.HardDelete(auditCaptureHardDeleteCtx(t, ctx), "sdw-never-created")
	if !isRecordNotFound(err) {
		t.Fatalf("HardDelete() of a never-created id error = %v, want ErrRecordNotFound (nothing matched)", err)
	}

	if events := bus.captured(); len(events) != 1 {
		t.Errorf("captured %d events, want exactly 1 (the seed create) -- a HardDelete that matched no row must publish nothing", len(events))
	}
}

// TestAuditCapturePlugin_HardDelete_SystemContextAlone_DoesNotAttribute
// pins the mechanism behind the attribution obligation hard_delete.go's doc
// comment and go/dbkit/AGENTS.md's "Hard deletion" section record: the
// system-context gate checks presence only and never invents an identity —
// pkgcore.WithSystemContext stores just the SystemReason (whose Actor is a
// bare string naming who the grant was for), and audit_capture.go reads the
// event's Actor from pkgcore.ActorFromContext on the write's context. A
// HardDelete performed on a system context that was never given an actor
// therefore lands attributed to the zero Actor — which is exactly why those
// docs tell callers to layer pkgcore.WithActor before entering system
// context, rather than expecting the gate to attribute for them. If a
// future round decides the capture should fall back to SystemReason.Actor
// after all, this test fails — deliberately — until that fallback ships
// with its own documented semantics, so the warning can never quietly drift
// out of sync with the mechanism it warns about.
func TestAuditCapturePlugin_HardDelete_SystemContextAlone_DoesNotAttribute(t *testing.T) {
	bus := &capturedBus{}
	db := openAuditCaptureTestDB(t, bus)
	if err := db.Exec(testutil.SoftDeletableWidgetTableSQL).Error; err != nil {
		t.Fatalf("create soft_deletable_widgets table: %v", err)
	}

	// The tenant-scoped context deliberately carries no actor: the caller
	// enters system context without ever layering pkgcore.WithActor, exactly
	// the shape the docs above warn about.
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	repo := dbkit.NewRepository[testutil.SoftDeletableWidget](db)
	w := &testutil.SoftDeletableWidget{ID: "sdw1", Name: "gadget"}
	if err := repo.Create(ctx, w); err != nil {
		t.Fatalf("seed Create() error = %v", err)
	}
	if err := repo.HardDelete(auditCaptureHardDeleteCtx(t, ctx), w.ID); err != nil {
		t.Fatalf("HardDelete() error = %v", err)
	}

	events := bus.captured()
	if len(events) != 2 {
		t.Fatalf("captured %d events, want exactly 2 (create, hard-delete)", len(events))
	}
	got := events[1]
	if got.Operation != "delete" {
		t.Errorf("HardDelete Operation = %q, want %q", got.Operation, "delete")
	}
	if got.Actor != (pkgcore.Actor{}) {
		t.Errorf("Actor = %+v, want the zero Actor -- a system context supplies no actor, and the capture never invents one from SystemReason.Actor; this is the corner hard_delete.go's attribution obligation warns callers about", got.Actor)
	}
	if got.ResourceID != w.ID {
		t.Errorf("ResourceID = %q, want %q", got.ResourceID, w.ID)
	}
	// Ground truth: the erasure itself happened (this is not a refusal-path
	// test) — the row is physically gone even though its erasure record could
	// not be attributed.
	if found, _, _ := rawSoftDeletableWidgetRow(t, db, w.ID); found {
		t.Fatal("row still physically present after HardDelete")
	}
}

// auditCaptureOmitWidget is the fixture for the Omit-only scoping tests
// below: a Widget-shaped, tenant-scoped, auditable model with one extra
// data column (Label). testutil.Widget itself cannot host the shape those
// tests need — a payload that at once carries an omitted column, a written
// column, and an unwritten zero-valued column — because with only Name and
// Value there is no way to keep one data column zero while writing the
// other; the extra column makes the zero-value-skip half of the scoping
// rule testable against real row state. It is deliberately local to this
// file (the precedent nonAuditableFlag and auditCaptureSecretWidget
// already set): only the two tests below use it.
type auditCaptureOmitWidget struct {
	ID       string `gorm:"primaryKey;size:26"`
	TenantID string `gorm:"primaryKey;size:26;not null"`
	Name     string `gorm:"size:255;not null"`
	Value    int    `gorm:"not null;default:0"`
	Label    string `gorm:"size:255;not null"`
}

func (w auditCaptureOmitWidget) GetTenantID() pkgcore.TenantID {
	return pkgcore.TenantID(w.TenantID)
}

func (w auditCaptureOmitWidget) AuditResourceType() string { return "audit_capture_omit_widget" }

// auditCaptureOmitWidgetTableSQL creates the audit_capture_omit_widgets
// table backing auditCaptureOmitWidget, hand-mirrored from the widgets
// DDL (createWidgetsTable above) plus the extra label column.
const auditCaptureOmitWidgetTableSQL = `CREATE TABLE audit_capture_omit_widgets (
	id        VARCHAR(26)  NOT NULL,
	tenant_id VARCHAR(26)  NOT NULL,
	name      VARCHAR(255) NOT NULL,
	value     INTEGER      NOT NULL DEFAULT 0,
	label     VARCHAR(255) NOT NULL,
	PRIMARY KEY (tenant_id, id)
)`

// rawAuditCaptureOmitWidgetRow reads id's raw name/value/label columns
// directly through db.Raw, bypassing every GORM callback — the "what
// actually landed in the database" ground truth the Omit-only scoping
// tests cross-check their captured After against, twin of
// rawSoftDeletableWidgetRow. found is false when no row with id exists.
func rawAuditCaptureOmitWidgetRow(t *testing.T, db *gorm.DB, id string) (found bool, name string, value int, label string) {
	t.Helper()
	row := db.Raw(`SELECT name, value, label FROM audit_capture_omit_widgets WHERE id = ?`, id).Row()
	if err := row.Scan(&name, &value, &label); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, "", 0, ""
		}
		t.Fatalf("raw audit_capture_omit_widgets lookup for id %q: %v", id, err)
	}
	return true, name, value, label
}

func TestAuditCapturePlugin_OmitOnlyStructUpdate_CapturesOnlyTheColumnsItWrites(t *testing.T) {
	bus := &capturedBus{}
	db := openAuditCaptureTestDB(t, bus)
	if err := db.Exec(auditCaptureOmitWidgetTableSQL).Error; err != nil {
		t.Fatalf("create audit_capture_omit_widgets table: %v", err)
	}
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	seed := &auditCaptureOmitWidget{ID: "ow1", TenantID: "tenant-a", Name: "gadget", Value: 1, Label: "old"}
	if err := db.WithContext(ctx).Create(seed).Error; err != nil {
		t.Fatalf("seed Create() error = %v", err)
	}

	// An Omit list with no Select list is GORM's "update the whole record
	// except the omitted columns" shape — but an Updates(struct) with no
	// Select list assigns only the payload's non-zero fields (GORM's
	// classic zero-value skip), and in this canonical Model == Dest shape
	// the primary keys never enter the SET: their payload values select
	// the row instead. The payload below exercises all three faces at
	// once: Name is non-zero but omitted, Value is zero and not omitted,
	// Label is non-zero and not omitted. The UPDATE must assign exactly
	// Label — so After must be exactly {label: "new"}, never a fabricated
	// claim about Name (payload says "renamed", the row still says
	// "gadget"), Value (payload says 0, the row still says 1), or the
	// primary keys.
	payload := &auditCaptureOmitWidget{ID: "ow1", TenantID: "tenant-a", Name: "renamed", Value: 0, Label: "new"}
	if err := db.WithContext(ctx).Model(payload).Omit("name").Where("id = ?", "ow1").Where("tenant_id = ?", "tenant-a").Updates(payload).Error; err != nil {
		t.Fatalf("Omit-only Updates() error = %v", err)
	}

	events := bus.captured()
	if len(events) != 2 {
		t.Fatalf("captured %d events, want 2 (create then update)", len(events))
	}
	got := events[1]
	if got.Operation != "update" {
		t.Errorf("Operation = %q, want %q", got.Operation, "update")
	}
	if len(got.After) != 1 || got.After["label"] != "new" {
		t.Errorf("After = %+v, want exactly {label: %q} -- the Omit-only UPDATE assigned only Label: Name was omitted, Value was zero on the payload (so never assigned), and the primary keys select the row instead of entering the SET", got.After, "new")
	}

	// Ground-truth cross-check: the row must agree with the captured After
	// on every captured column, and the columns After deliberately does
	// not carry must still hold their pre-write values — proving they were
	// really not written, not merely not claimed.
	rowFound, rowName, rowValue, rowLabel := rawAuditCaptureOmitWidgetRow(t, db, "ow1")
	if !rowFound {
		t.Fatal("row ow1 missing after the update")
	}
	if rowName != "gadget" {
		t.Errorf("row name = %q, want %q -- the omitted column must not have been written even though the payload carried %q", rowName, "gadget", "renamed")
	}
	if rowValue != 1 {
		t.Errorf("row value = %d, want 1 -- a zero-valued payload column is never assigned by an Omit-only struct update, so the row must keep its prior value", rowValue)
	}
	if rowLabel != "new" {
		t.Errorf("row label = %q, want %q (the one column the update did write)", rowLabel, "new")
	}
}

func TestAuditCapturePlugin_OmitOnlyMapUpdate_CapturesOnlyTheColumnsItWrites(t *testing.T) {
	bus := &capturedBus{}
	db := openAuditCaptureTestDB(t, bus)
	if err := db.Exec(auditCaptureOmitWidgetTableSQL).Error; err != nil {
		t.Fatalf("create audit_capture_omit_widgets table: %v", err)
	}
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	seed := &auditCaptureOmitWidget{ID: "ow2", TenantID: "tenant-a", Name: "gadget", Value: 1, Label: "old"}
	if err := db.WithContext(ctx).Create(seed).Error; err != nil {
		t.Fatalf("seed Create() error = %v", err)
	}

	// A map payload has no zero-value skip — GORM assigns exactly the
	// map's keys, minus the omitted ones. The map below carries a
	// deliberately-renamed and omitted Name alongside a zero Value and a
	// changed Label: the UPDATE must assign exactly Value and Label, and
	// After must say so — value: 0 included, since unlike a struct
	// payload, a zero map value really is written.
	m := map[string]any{"name": "renamed", "value": 0, "label": "mapped"}
	if err := db.WithContext(ctx).Model(&auditCaptureOmitWidget{ID: "ow2", TenantID: "tenant-a"}).Omit("name").Where("id = ?", "ow2").Where("tenant_id = ?", "tenant-a").Updates(m).Error; err != nil {
		t.Fatalf("Omit-only map Updates() error = %v", err)
	}

	events := bus.captured()
	if len(events) != 2 {
		t.Fatalf("captured %d events, want 2 (create then update)", len(events))
	}
	got := events[1]
	if got.Operation != "update" {
		t.Errorf("Operation = %q, want %q", got.Operation, "update")
	}
	if len(got.After) != 2 {
		t.Fatalf("After = %+v, want exactly the 2 map keys the UPDATE wrote (value, label) -- the omitted name key must not appear even though the map carried it", got.After)
	}
	if afterValue, ok := got.After["value"]; !ok || afterValue != 0 {
		t.Errorf("After[\"value\"] = %v (ok=%v), want 0 -- a zero map value really is written (a map payload has no zero-value skip) and must be captured", afterValue, ok)
	}
	if got.After["label"] != "mapped" {
		t.Errorf("After[\"label\"] = %v, want %q", got.After["label"], "mapped")
	}

	rowFound, rowName, rowValue, rowLabel := rawAuditCaptureOmitWidgetRow(t, db, "ow2")
	if !rowFound {
		t.Fatal("row ow2 missing after the update")
	}
	if rowName != "gadget" {
		t.Errorf("row name = %q, want %q -- the omitted column must not have been written even though the map carried %q", rowName, "gadget", "renamed")
	}
	if rowValue != 0 {
		t.Errorf("row value = %d, want 0 (the zero map value really was written)", rowValue)
	}
	if rowLabel != "mapped" {
		t.Errorf("row label = %q, want %q", rowLabel, "mapped")
	}
}

// auditCapturePlatformRecord is a platform-domain Auditable fixture that
// deliberately does NOT implement dbkit.TenantScoped -- the shape
// root CLAUDE.md's four-data-domain table gives identity and platform
// tables (users, sessions, platform-level plan definitions), mirroring
// go/dbkit/audit's own AuditEvent, which the module's own doc comment
// records as "deliberately does not implement TenantScoped". It is local to
// this file (the nonAuditableFlag/auditCaptureSecretWidget/
// auditCaptureOmitWidget precedent above), used only by
// TestAuditCapturePlugin_NonTenantScopedModel_DoesNotInheritContextTenant.
type auditCapturePlatformRecord struct {
	ID   string `gorm:"primaryKey;size:26"`
	Name string `gorm:"size:255;not null"`
}

// AuditResourceType satisfies dbkit.Auditable. auditCapturePlatformRecord
// carries no GetTenantID method at all, so it does not satisfy
// dbkit.TenantScoped -- confirmed by its absence from the compile-time
// assertions below, the reverse-and-equally-important property
// nonAuditableFlag already proves for the opposite combination.
func (auditCapturePlatformRecord) AuditResourceType() string { return "platform_record" }

var _ dbkit.Auditable = auditCapturePlatformRecord{}

// createAuditCapturePlatformRecordsTable creates the table backing
// auditCapturePlatformRecord: deliberately no tenant_id column at all,
// matching a real platform/identity table's shape.
func createAuditCapturePlatformRecordsTable(t *testing.T, db *gorm.DB) {
	t.Helper()
	err := db.Exec(`CREATE TABLE audit_capture_platform_records (
		id   VARCHAR(26)  NOT NULL PRIMARY KEY,
		name VARCHAR(255) NOT NULL
	)`).Error
	if err != nil {
		t.Fatalf("create audit_capture_platform_records table: %v", err)
	}
}

// TestAuditCapturePlugin_NonTenantScopedModel_DoesNotInheritContextTenant is
// the regression for dbkit-tenancy P2-2's second failure shape: a model
// implementing Auditable but NOT dbkit.TenantScoped -- a platform or
// identity-domain model per root CLAUDE.md's four-data-domain table --
// written under some tenant ctx (a job or admin operation that rebuilt
// tenant ctx for a reason entirely unrelated to this platform-level write)
// must not have that ctx's tenant show up on its captured event at all: the
// row itself has no real tenant, so the truthful WriteCapturedEvent.TenantID
// is empty, never whatever the ctx happened to carry.
//
// Before this fix, capture built evt.TenantID directly from
// pkgcore.TenantFromContext(db.Statement.Context) unconditionally, with no
// check on whether the written model was even TenantScoped at all -- so
// this exact write would have wrongly captured TenantID = "tenant-a".
func TestAuditCapturePlugin_NonTenantScopedModel_DoesNotInheritContextTenant(t *testing.T) {
	bus := &capturedBus{}
	db := openAuditCaptureTestDB(t, bus)
	createAuditCapturePlatformRecordsTable(t, db)

	// The write's ctx carries a real tenant -- exactly the shape the audit
	// text describes: a platform-domain write that happens to run under
	// some tenant's ctx for reasons unrelated to the row itself (this test
	// does not need to construct why; it only needs ctx to carry one).
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	rec := &auditCapturePlatformRecord{ID: "rec1", Name: "seed"}
	if err := db.WithContext(ctx).Create(rec).Error; err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	events := bus.captured()
	if len(events) != 1 {
		t.Fatalf("captured %d events, want exactly 1", len(events))
	}
	got := events[0]
	if got.Operation != "create" {
		t.Errorf("Operation = %q, want %q", got.Operation, "create")
	}
	if got.TenantID != "" {
		t.Errorf(`TenantID = %q, want "" -- a platform-domain Auditable model must never inherit `+
			"the ctx tenant it merely happened to be written under (dbkit-tenancy P2-2)", got.TenantID)
	}
}

// auditCaptureTenantModelWidget is a TenantScoped, Auditable fixture whose
// tenant_id column is NOT part of the primary key -- ID alone is --
// embedding dbkit.TenantModel exactly the way that type's own doc comment
// describes as its intended use ("the narrower case where a plain, non-key
// tenant_id column is genuinely enough"), unlike testutil.Widget's
// composite (tenant_id, id) primary key, which every real tenant-scoped
// table in this codebase is required to use instead (backend coding
// standard §5). That distinction is load-bearing for
// TestAuditCapturePlugin_ModelArgumentTenantDiffersFromContext_TrustsContextAndWarns
// below: see that test's own doc comment for why a composite-primary-key
// model cannot reach the scenario it targets at all. Local to this file,
// like every other fixture above.
type auditCaptureTenantModelWidget struct {
	ID string `gorm:"primaryKey;size:26"`
	dbkit.TenantModel
	Name string `gorm:"size:255;not null"`
}

func (auditCaptureTenantModelWidget) AuditResourceType() string { return "tenant_model_widget" }

var (
	_ dbkit.TenantScoped = auditCaptureTenantModelWidget{}
	_ dbkit.Auditable    = auditCaptureTenantModelWidget{}
)

// createAuditCaptureTenantModelWidgetsTable creates the table backing
// auditCaptureTenantModelWidget: id alone is the primary key, tenant_id an
// ordinary NOT NULL column -- the inverse of createWidgetsTable's composite
// key, deliberately.
func createAuditCaptureTenantModelWidgetsTable(t *testing.T, db *gorm.DB) {
	t.Helper()
	err := db.Exec(`CREATE TABLE audit_capture_tenant_model_widgets (
		id        VARCHAR(26)  NOT NULL PRIMARY KEY,
		tenant_id VARCHAR(26)  NOT NULL,
		name      VARCHAR(255) NOT NULL
	)`).Error
	if err != nil {
		t.Fatalf("create audit_capture_tenant_model_widgets table: %v", err)
	}
}

// TestAuditCapturePlugin_ModelArgumentTenantDiffersFromContext_TrustsContextAndWarns
// is the regression for dbkit-tenancy P2-2's first failure shape, resolved
// the opposite way its own audit text initially assumed: a TenantScoped
// model's captured event trusts the write's ctx tenant, not the model
// argument's own GetTenantID() value, because investigation (see
// stampTenantID's own doc comment in audit_capture.go) found ctx to be the
// value tenantScopePlugin -- always co-installed with the audit-capture
// plugin by Open, on the exact same connection -- actually enforces via the
// statement's WHERE clause, while a decoupled .Model(...) argument can
// disagree with it and still have the write succeed.
//
// Why this needs auditCaptureTenantModelWidget rather than testutil.Widget:
// GORM's own ConvertToAssignments (gorm.io/gorm/callbacks/update.go) folds
// every non-zero primary-key field of a Model argument into the statement's
// WHERE clause whenever Model != Dest (a map payload, as this test uses,
// always is this) -- confirmed empirically while writing this test.
// Against testutil.Widget, whose primary key is the composite
// (tenant_id, id) every real tenant-scoped table is required to use, a
// stale Model argument's own tenant_id therefore becomes a second,
// self-defeating WHERE condition alongside tenantScopeBeforeUpdate's
// ctx-derived one -- the two together match nothing at all, so no
// mismatched event is ever captured through that shape: a composite-key
// model's own field genuinely cannot disagree with ctx on a write that
// still succeeds. auditCaptureTenantModelWidget's tenant_id is deliberately
// NOT part of its primary key (only ID is), so GORM folds just "id = ..."
// from the Model argument, leaving tenantScopeBeforeUpdate's ctx-derived
// tenant_id condition as the sole tenant filter -- the row actually matched
// and written is therefore ctx's real tenant, even though the Model
// argument's own GetTenantID() claims a different one.
func TestAuditCapturePlugin_ModelArgumentTenantDiffersFromContext_TrustsContextAndWarns(t *testing.T) {
	bus := &capturedBus{}
	db := openAuditCaptureTestDB(t, bus)
	createAuditCaptureTenantModelWidgetsTable(t, db)
	ctxA := pkgcore.WithTenant(context.Background(), "tenant-a")

	w := &auditCaptureTenantModelWidget{ID: "tmw1", Name: "gadget"}
	if err := db.WithContext(ctxA).Create(w).Error; err != nil {
		t.Fatalf("seed Create() error = %v", err)
	}

	logBuf := captureSlogDefault(t)

	staleModel := &auditCaptureTenantModelWidget{ID: "tmw1", TenantModel: dbkit.TenantModel{TenantID: "tenant-b"}}
	res := db.WithContext(ctxA).Model(staleModel).Updates(map[string]any{"name": "renamed"})
	if res.Error != nil {
		t.Fatalf("Updates() error = %v", res.Error)
	}
	if res.RowsAffected != 1 {
		t.Fatalf("RowsAffected = %d, want 1 -- the WHERE clause must still have matched the real "+
			"tenant-a row despite the decoupled Model argument claiming tenant-b", res.RowsAffected)
	}

	events := bus.captured()
	if len(events) != 2 {
		t.Fatalf("captured %d events, want 2 (create then update)", len(events))
	}
	got := events[1]
	if got.Operation != "update" {
		t.Errorf("Operation = %q, want %q", got.Operation, "update")
	}
	if got.TenantID != "tenant-a" {
		t.Errorf(`TenantID = %q, want "tenant-a" -- ctx is the value tenantScopeBeforeUpdate actually `+
			"bound into the WHERE clause that matched this row, so it must win over the decoupled "+
			"Model argument's own stale tenant-b field", got.TenantID)
	}
	if !strings.Contains(logBuf.String(), "dbkit: captured write's model argument tenant differs from context tenant") {
		t.Errorf("expected a logged mismatch warning naming the disagreement, log = %q", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "model_tenant_id=tenant-b") {
		t.Errorf("expected the mismatch warning to name the disagreeing model_tenant_id=tenant-b, log = %q", logBuf.String())
	}
}
