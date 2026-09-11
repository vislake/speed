package audit

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit/migrations"

	// Blank-imported for its init side effect: registers dbkit.DialectSQLite
	// so the dbkit.Open call below has a driver to build from.
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
	"github.com/vislake/speed/go/pkgcore"
)

// fakeAuditModule is a minimal module used only by this package's
// own tests, to feed the package's embedded migrations to
// dbkit.MigrationRegistry without depending on the persister
// Module (see doc.go). Only Name and Migrations are ever read by
// MigrationRegistry.Apply here; DependsOn, Locales, OpenAPISpec and
// Register exist solely to satisfy the module contract, mirroring dbkit's
// own migrations_test.go fakeModule.
type fakeAuditModule struct{}

func (fakeAuditModule) Name() string                              { return "audit" }
func (fakeAuditModule) DependsOn() []string                       { return nil }
func (fakeAuditModule) Migrations() embed.FS                      { return migrations.FS }
func (fakeAuditModule) Locales() embed.FS                         { return embed.FS{} }
func (fakeAuditModule) OpenAPISpec() []byte                       { return nil }
func (fakeAuditModule) Register(*pkgcore.ComponentRegistry) error { return nil }

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

// TestRepository_InsertIdempotent_DuplicateID_IsANoOp proves the
// Repository-level half of Module's multi-replica deduplication (see
// AGENTS.md's "Multi-replica delivery" section): in distributed
// deployment mode with more than one replica, a broker-backed bus
// delivers every event to every replica once each, so Module's
// subscribers (module.go's onWriteCaptured, onRecorded,
// onSystemContextEntered) independently call Insert once per replica for
// the SAME logical event. A plain Insert would generate a fresh random ID
// per call (its default when evt.ID is left empty), so N replicas would
// produce N rows for one real action; inserting the same evt.ID twice
// through InsertIdempotent succeeds both times but persists exactly one
// row. module_test.go's
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

// captureSlogDefault swaps slog.Default() for a text handler writing into
// a buffer this returns, restoring the previous default logger on test
// cleanup. It is this package's twin of go/dbkit/audit_capture_test.go's
// own captureSlogDefault (each test package needs its own copy, since a
// helper living in one package's _test.go is unreachable from the other) --
// itself modeled on go/pkgcore/registry_test.go's identical swap. slog's
// default logger is process-global, so a test using this must not run in
// parallel with another that also touches it; none of this file's tests
// call t.Parallel.
func captureSlogDefault(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// TestRepository_Insert_CutsOverWideDescriptiveFieldsToTheirColumnBounds
// pins the write-boundary enforcement repository.go's Insert applies to
// audit_events' descriptive columns (see fitEventToColumns' doc comment
// and model.go's column-bounds constants for why the cut happens in Go
// rather than being left to the database): under PostgreSQL an over-long
// value fails the INSERT with error 22001 and the whole record is lost,
// while SQLite does not enforce VARCHAR lengths at all -- the two dialects
// disagreeing about the same row is exactly the divergence the cut
// removes. SQLite cannot show the 22001 half of the story, so, mirroring
// go/sharing's identical "SQLite-only tier cannot see an over-long value
// fail" note (model.go there), this test pins the Go-side cut directly:
// the stored row never carries more than the column's rune bound, the cut
// is rune-safe (a 300-rune multibyte value is cut at 255 runes, not 255
// bytes), a value at the exact bound survives verbatim, an invalid-UTF-8
// value is sanitized to the replacement character (PostgreSQL refuses raw
// invalid bytes with 22021), and every changed value is recorded in a
// structured warning -- the truncation trace repository.go's own doc
// comment promises. The PostgreSQL leg of the same proof -- where the
// refusal is a genuine 22001 -- lives in
// integration_test/postgres_column_bounds_test.go.
func TestRepository_Insert_CutsOverWideDescriptiveFieldsToTheirColumnBounds(t *testing.T) {
	db := openAuditTestDB(t)
	repo := NewRepository(db)
	logs := captureSlogDefault(t)
	ctx := context.Background()

	// 300 three-byte runes: cutting at 255 bytes would split a character
	// and store garbage; cutting at 255 runes must store exactly 255.
	longURL := strings.Repeat("€", 300)
	longActor := strings.Repeat("a", 300)
	longReason := strings.Repeat("r", 1200)
	longUA := strings.Repeat("u", 700)
	evt := sampleEvent()
	evt.SetResource(Resource{Type: "integration.webhook_subscription", ID: "wh-1", DisplayName: longURL})
	evt.SetActor(pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: "admin-1", DisplayName: longActor})
	evt.SetOnBehalfOf(&pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1", DisplayName: strings.Repeat("b", 300)})
	evt.SetResult(Result{Success: false, FailureReason: longReason})
	evt.IP = strings.Repeat("1", 100)
	evt.UserAgent = longUA
	evt.TraceID = strings.Repeat("t", 100)
	if err := repo.Insert(ctx, evt); err != nil {
		t.Fatalf("Insert(over-wide descriptive fields) error = %v, want nil (the fields must be cut, not the row refused)", err)
	}

	got, err := repo.Get(ctx, evt.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got == nil {
		t.Fatal("Get() = nil, want the inserted event")
	}

	if n := utf8.RuneCountInString(got.ResourceDisplayName); n != resourceDisplayNameColumnRunes {
		t.Errorf("stored resource_display_name is %d runes, want the %d-rune bound (a rune-safe cut, not a byte cut)", n, resourceDisplayNameColumnRunes)
	}
	if got.ResourceDisplayName != string([]rune(longURL)[:resourceDisplayNameColumnRunes]) {
		t.Error("stored resource_display_name is not the original value cut at the rune bound")
	}
	if n := utf8.RuneCountInString(got.ActorDisplayName); n != actorDisplayNameColumnRunes {
		t.Errorf("stored actor_display_name is %d runes, want %d", n, actorDisplayNameColumnRunes)
	}
	if n := utf8.RuneCountInString(got.FailureReason); n != failureReasonColumnRunes {
		t.Errorf("stored failure_reason is %d runes, want %d", n, failureReasonColumnRunes)
	}
	if n := utf8.RuneCountInString(got.IP); n != ipColumnRunes {
		t.Errorf("stored ip is %d runes, want %d", n, ipColumnRunes)
	}
	if n := utf8.RuneCountInString(got.UserAgent); n != userAgentColumnRunes {
		t.Errorf("stored user_agent is %d runes, want %d", n, userAgentColumnRunes)
	}
	if n := utf8.RuneCountInString(got.TraceID); n != traceIDColumnRunes {
		t.Errorf("stored trace_id is %d runes, want %d", n, traceIDColumnRunes)
	}
	onBehalfOf, ok := got.OnBehalfOf()
	if !ok {
		t.Fatal("stored on_behalf_of is absent, want it present")
	}
	if n := utf8.RuneCountInString(onBehalfOf.DisplayName); n != onBehalfOfDisplayNameColumnRunes {
		t.Errorf("stored on_behalf_of_display_name is %d runes, want %d", n, onBehalfOfDisplayNameColumnRunes)
	}

	// Every cut must have left a trace: one warning per changed field,
	// naming the field and its limit.
	logText := logs.String()
	for _, want := range []string{
		"audit: audit event field value changed to fit its column",
		"actor_display_name", "on_behalf_of_display_name", "resource_display_name",
		"failure_reason", "ip", "user_agent", "trace_id",
		"reason=column_width",
	} {
		if !strings.Contains(logText, want) {
			t.Errorf("truncation warning log does not mention %q; got:\n%s", want, logText)
		}
	}

	// A value exactly at its bound must survive verbatim, with no warning.
	boundary := strings.Repeat("c", resourceDisplayNameColumnRunes)
	atBound := sampleEvent()
	atBound.SetResource(Resource{Type: "note", ID: "note-1", DisplayName: boundary})
	if boundErr := repo.Insert(ctx, atBound); boundErr != nil {
		t.Fatalf("Insert(value exactly at the bound) error = %v", boundErr)
	}
	readBound, readErr := repo.Get(ctx, atBound.ID)
	if readErr != nil {
		t.Fatalf("Get(boundary) error = %v", readErr)
	}
	if readBound.ResourceDisplayName != boundary {
		t.Errorf("value at the exact bound was changed to %d runes, want it verbatim", utf8.RuneCountInString(readBound.ResourceDisplayName))
	}
	after := logs.String()
	if strings.Count(after, "audit: audit event field value changed to fit its column") != strings.Count(logText, "audit: audit event field value changed to fit its column") {
		t.Error("an at-the-bound value produced a truncation warning, want none")
	}
}

// TestRepository_Insert_SanitizesInvalidUTF8InDescriptiveFields pins the
// invalid-UTF-8 half of dbkit.FitColumnValue: a UTF-8-encoded PostgreSQL
// database refuses raw invalid bytes with error 22021 (SQLite again stores
// them silently), so the write boundary sanitizes each consecutive invalid
// run to a single replacement character (U+FFFD) -- never by dropping the
// bytes, which could concatenate two arbitrary byte runs into a different
// valid value -- and records the change in a structured warning.
func TestRepository_Insert_SanitizesInvalidUTF8InDescriptiveFields(t *testing.T) {
	db := openAuditTestDB(t)
	repo := NewRepository(db)
	logs := captureSlogDefault(t)
	ctx := context.Background()

	// "\xff\xfe" is one consecutive run of invalid bytes.
	evt := sampleEvent()
	evt.SetResource(Resource{Type: "note", ID: "note-1", DisplayName: "a\xff\xfeb"})
	if err := repo.Insert(ctx, evt); err != nil {
		t.Fatalf("Insert(invalid UTF-8) error = %v, want nil (the value must be sanitized, not the row refused)", err)
	}
	got, err := repo.Get(ctx, evt.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	want := "a�b" // one replacement character for the whole invalid run
	if got.ResourceDisplayName != want {
		t.Errorf("stored resource_display_name = %q, want %q (one U+FFFD per consecutive invalid run)", got.ResourceDisplayName, want)
	}
	if !strings.Contains(logs.String(), "reason=invalid_utf8") {
		t.Errorf("sanitization warning not recorded; got:\n%s", logs.String())
	}
}

// TestRepository_Insert_RefusesOverWideIdentifierFields pins the
// identifier/vocabulary half of fitEventToColumns: an over-wide value in
// one of the fields that identify the record or its subject -- the fields
// whose silent cutting would rewrite the record's own identity -- is a
// caller bug and is refused with ErrEventFieldTooLong before the write,
// on every dialect alike (leaving it to the database would let SQLite
// store the row silently while PostgreSQL refuses it with 22001). The
// refusal must also leave no row behind.
func TestRepository_Insert_RefusesOverWideIdentifierFields(t *testing.T) {
	db := openAuditTestDB(t)
	repo := NewRepository(db)
	ctx := context.Background()

	cases := []struct {
		name   string
		field  string
		mutate func(evt *AuditEvent)
	}{
		{"ID", "ID", func(evt *AuditEvent) { evt.ID = strings.Repeat("i", idColumnRunes+1) }},
		{"ActorID", "ActorID", func(evt *AuditEvent) {
			evt.SetActor(pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: strings.Repeat("u", actorIDColumnRunes+1), DisplayName: "Ada"})
		}},
		{"ResourceID", "ResourceID", func(evt *AuditEvent) {
			evt.SetResource(Resource{Type: "note", ID: strings.Repeat("n", resourceIDColumnRunes+1), DisplayName: "x"})
		}},
		{"Action", "Action", func(evt *AuditEvent) { evt.Action = strings.Repeat("a", actionColumnRunes+1) }},
		{"TenantID", "TenantID", func(evt *AuditEvent) { evt.TenantID = strings.Repeat("t", tenantIDColumnRunes+1) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evt := sampleEvent()
			tc.mutate(evt)
			err := repo.Insert(ctx, evt)
			if err == nil {
				t.Fatalf("Insert(over-wide %s) error = nil, want ErrEventFieldTooLong", tc.field)
			}
			if !errors.Is(err, ErrEventFieldTooLong) {
				t.Fatalf("Insert(over-wide %s) error = %v, want it to wrap ErrEventFieldTooLong", tc.field, err)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("Insert(over-wide %s) error %q does not name the offending field", tc.field, err)
			}
			if rows, listErr := repo.ListByTenant(ctx, "tenant-a"); listErr != nil {
				t.Fatalf("ListByTenant() error = %v", listErr)
			} else if len(rows) != 0 {
				t.Errorf("ListByTenant() returned %d rows after the refusal, want 0 (the refusal must happen before any write)", len(rows))
			}
		})
	}

	// The descriptive columns must NOT be refused by the same enforcement:
	// an over-wide display name is legal content and is cut, not refused
	// (covered in depth by the cut test above -- this guards the boundary
	// between the two classes).
	legal := sampleEvent()
	legal.SetResource(Resource{Type: "note", ID: "note-1", DisplayName: strings.Repeat("d", resourceDisplayNameColumnRunes+1)})
	if err := repo.Insert(ctx, legal); err != nil {
		t.Errorf("Insert(over-wide display name) error = %v, want nil -- descriptive fields are cut, never refused", err)
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

// TestAppendOnlyTrigger_SQLite_RejectsRawUpdateAndDelete is the SQLite leg
// of the database-level append-only proof: migrations/sqlite/0002_append_
// only_enforcement.sql's two BEFORE UPDATE/DELETE triggers must refuse a
// raw statement issued directly against audit_events, entirely bypassing
// Repository (which never had an Update or Delete method to bypass in the
// first place -- this test proves the guarantee holds even for a caller
// that skips Repository, and even Go application code, altogether: a raw
// *sql.DB obtained from the same *gorm.DB openAuditTestDB already applied
// both migration files to).
//
// The PostgreSQL leg of the identical proof lives in
// integration_test/postgres_append_only_test.go, since a real PostgreSQL
// server needs Docker via testcontainers -- this file needs none, matching
// the rest of this package's SQLite-first unit tier.
func TestAppendOnlyTrigger_SQLite_RejectsRawUpdateAndDelete(t *testing.T) {
	ctx := context.Background()
	db := openAuditTestDB(t)
	repo := NewRepository(db)

	evt := sampleEvent()
	if err := repo.Insert(ctx, evt); err != nil {
		t.Fatalf("Insert() error = %v, want nil -- INSERT must be completely unaffected by the append-only triggers", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get underlying *sql.DB: %v", err)
	}

	t.Run("RawUpdate_Rejected", func(t *testing.T) {
		_, err := sqlDB.ExecContext(ctx, `UPDATE audit_events SET action = ? WHERE id = ?`, "tampered.action", evt.ID)
		if err == nil {
			t.Fatal("raw UPDATE against audit_events succeeded, want the append-only trigger to reject it")
		}

		// The row itself must be completely unchanged -- not merely that
		// the statement returned an error, but that nothing was mutated
		// before the trigger aborted it.
		got, getErr := repo.Get(ctx, evt.ID)
		if getErr != nil {
			t.Fatalf("Get() after the rejected UPDATE error = %v", getErr)
		}
		if got == nil || got.Action != evt.Action {
			t.Fatalf("Get() after the rejected UPDATE = %+v, want the row unchanged (Action = %q)", got, evt.Action)
		}
	})

	t.Run("RawDelete_Rejected", func(t *testing.T) {
		_, err := sqlDB.ExecContext(ctx, `DELETE FROM audit_events WHERE id = ?`, evt.ID)
		if err == nil {
			t.Fatal("raw DELETE against audit_events succeeded, want the append-only trigger to reject it")
		}

		got, getErr := repo.Get(ctx, evt.ID)
		if getErr != nil {
			t.Fatalf("Get() after the rejected DELETE error = %v", getErr)
		}
		if got == nil {
			t.Fatal("row is gone after the rejected DELETE, want it still physically present")
		}
	})
}

// TestAppendOnlyTrigger_SQLite_RejectsInsertOrReplace proves the append-only
// triggers hold even against SQLite's legacy "INSERT OR REPLACE" upsert
// form, not only against a plain UPDATE/DELETE.
//
// SQLite only fires a table's DELETE triggers for the implicit
// conflict-row removal INSERT OR REPLACE performs when the connection's
// PRAGMA recursive_triggers is ON; it defaults OFF, so without the pragma
// an "INSERT OR REPLACE" against an existing id silently overwrites the
// row's columns with no error and no trigger firing at all -- reachable
// only via raw SQL that bypasses Repository (which has no Update or
// Delete method to begin with), but exactly the threat class the
// append-only triggers exist to stop. go/dbkit/dialect/sqlite's
// withRecursiveTriggers folds "_pragma=recursive_triggers(1)" into every
// SQLite DSN dbkit.Open opens; on a connection without it this test fails
// (the row silently becomes "tampered.action" with no error), on
// dbkit.Open's connections it passes.
func TestAppendOnlyTrigger_SQLite_RejectsInsertOrReplace(t *testing.T) {
	ctx := context.Background()
	db := openAuditTestDB(t)
	repo := NewRepository(db)

	evt := sampleEvent()
	if err := repo.Insert(ctx, evt); err != nil {
		t.Fatalf("Insert() error = %v, want nil", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get underlying *sql.DB: %v", err)
	}

	_, err = sqlDB.ExecContext(ctx,
		`INSERT OR REPLACE INTO audit_events (`+
			`id, actor_type, actor_id, actor_display_name, `+
			`on_behalf_of_type, on_behalf_of_id, on_behalf_of_display_name, `+
			`action, resource_type, resource_id, resource_display_name, `+
			`success, failure_reason, changes, tenant_id, ip, user_agent, trace_id, occurred_at`+
			`) SELECT `+
			`id, actor_type, actor_id, actor_display_name, `+
			`on_behalf_of_type, on_behalf_of_id, on_behalf_of_display_name, `+
			`'tampered.action', resource_type, resource_id, resource_display_name, `+
			`success, failure_reason, changes, tenant_id, ip, user_agent, trace_id, occurred_at `+
			`FROM audit_events WHERE id = ?`,
		evt.ID)
	if err == nil {
		t.Fatal("raw INSERT OR REPLACE against audit_events succeeded, want the append-only DELETE trigger to reject its implicit conflict-row removal")
	}

	got, getErr := repo.Get(ctx, evt.ID)
	if getErr != nil {
		t.Fatalf("Get() after the rejected INSERT OR REPLACE error = %v", getErr)
	}
	if got == nil || got.Action != evt.Action {
		t.Fatalf("Get() after the rejected INSERT OR REPLACE = %+v, want the row unchanged (Action = %q)", got, evt.Action)
	}
}

// TestAppendOnlyTrigger_SQLite_InsertStillWorks re-proves, as its own
// explicitly named test (rather than folding this into the test above
// alone), that a fresh Insert after the triggers are installed -- not just
// the one Insert the rejection test above happens to perform first -- is
// unaffected: the append-only enforcement must never make the table
// write-only-once or otherwise interfere with the one write path
// Repository is actually meant to support.
func TestAppendOnlyTrigger_SQLite_InsertStillWorks(t *testing.T) {
	ctx := context.Background()
	db := openAuditTestDB(t)
	repo := NewRepository(db)

	for i := 0; i < 3; i++ {
		evt := sampleEvent()
		if err := repo.Insert(ctx, evt); err != nil {
			t.Fatalf("Insert() #%d error = %v, want nil", i, err)
		}
		got, err := repo.Get(ctx, evt.ID)
		if err != nil {
			t.Fatalf("Get() #%d error = %v", i, err)
		}
		if got == nil {
			t.Fatalf("Get() #%d = nil, want the just-inserted row back", i)
		}
	}
}
