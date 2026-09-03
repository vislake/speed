package dbkit_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
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

func TestAuditCapturePlugin_PublishFailure_FailsTheWriteLoudly(t *testing.T) {
	publishErr := errors.New("bus unavailable")
	bus := &capturedBus{fail: publishErr}
	db := openAuditCaptureTestDB(t, bus)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	w := &testutil.Widget{ID: "w1", Name: "gadget"}
	err := db.WithContext(ctx).Create(w).Error
	if err == nil {
		t.Fatal("Create() error = nil, want a loud failure when the audit publish itself fails (docs/internal/10-compliance-and-audit.md: an audit-write failure must alert, never silently drop)")
	}
	appErr, ok := apperr.As(err)
	if !ok || appErr.Code != "dbkit.audit_capture_publish_failed" {
		t.Errorf("Create() error = %v, want an *apperr.Error coded dbkit.audit_capture_publish_failed", err)
	}
	if !errors.Is(err, publishErr) {
		t.Errorf("Create() error = %v, want it to wrap the bus's own publish error", err)
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
