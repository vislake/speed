package dbkit_test

import (
	"context"
	"crypto/sha256"
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
// (docs/internal/04-data-and-tenancy.md, "删除语义" §4's own named pitfall).
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
