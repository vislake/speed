package notes

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// TestNote_GetTenantID_ReturnsEmbeddedTenantModelValue is a no-database
// sanity check that Note's promoted GetTenantID method (from embedding
// dbkit.TenantModel) returns the value held by that embedded TenantModel
// -- confirming the embedding itself is wired the way model.go's doc
// comment on Note says it is.
//
// It does NOT guard against the shadowed-TenantID-field footgun dbkit's
// own tenant_scope.go doc comment on TenantModel warns about (repeated on
// model.go's Note doc comment), despite an earlier version of this
// comment claiming it did. The struct literal below sets TenantModel's
// embedded TenantID directly, through a keyed "TenantModel:
// dbkit.TenantModel{TenantID: ...}" literal, so it never goes through
// GORM's schema/scan machinery -- exactly where that regression
// manifests, since GORM's schema parser resolves a same-named field
// redeclared on Note (the shadow) in place of the one promoted from
// TenantModel, per ordinary Go field-selector rules. Confirmed
// empirically: adding such a shadowing field to Note leaves this test
// passing, since GetTenantID and this test's literal still agree on
// TenantModel's own copy of the field -- the mismatch against what GORM
// actually populates on a real row never enters the picture.
//
// The regression is instead caught by TestRepository_AssertIsolated
// (repository_test.go), which drives Create and FindByID through a real
// migrated SQLite database via dbkit.Repository[Note] -- the path where
// GORM's field resolution actually runs -- and, more generally, by
// dbkit's own
// TestTenantModel_ShadowingPromotedFieldToAddPrimaryKey_BreaksFindByIDForTheOwningTenant
// (go/dbkit/tenant_scope_tenantmodel_test.go). Both fail under the
// shadowing regression; this test does not, and is not meant to.
func TestNote_GetTenantID_ReturnsEmbeddedTenantModelValue(t *testing.T) {
	n := Note{
		ID:          "note-1",
		TenantModel: dbkit.TenantModel{TenantID: "tenant-acme"},
		Text:        "hello",
	}

	if got, want := n.GetTenantID(), pkgcore.TenantID("tenant-acme"); got != want {
		t.Fatalf("GetTenantID() = %q, want %q", got, want)
	}
}

// TestNote_ImplementsTenantScoped is a runtime-checkable companion to
// model.go's compile-time `var _ dbkit.TenantScoped = Note{}` assertion --
// redundant with it, but unlike that line, a test failure here shows
// up in `go test`'s own output instead of only a build error, which is
// easier for a reader to spot in CI.
func TestNote_ImplementsTenantScoped(t *testing.T) {
	var _ dbkit.TenantScoped = Note{}
}

// TestNote_AuditResourceType_ReturnsNote is a runtime-checkable companion
// to model.go's compile-time `var _ dbkit.Auditable = Note{}` assertion,
// pinning the exact resource-type string dbkit's automatic write-capture
// plugin labels a Note write's WriteCapturedEvent with on any connection
// whose capture scope admits Note. On this app's own shared connection
// the bus is wired but Note is deliberately left off the Open call's
// Options.AuditModels scope, so no automatic capture happens here -- the
// note trail runs through the declarative audit.Emit path instead (see
// model.go's AuditResourceType doc comment for the full shape, and
// server_test.go's TestBuildServer_NoteCreate_PersistsAuditEvent for the
// end-to-end proof of that path).
func TestNote_AuditResourceType_ReturnsNote(t *testing.T) {
	var n Note
	if got, want := n.AuditResourceType(), "note"; got != want {
		t.Fatalf("AuditResourceType() = %q, want %q", got, want)
	}
}

// notesCapturedBus is a pkgcore.EventBus test double that records every
// WriteCapturedEvent published to it, mirroring go/dbkit's own
// audit_capture_test.go capturedBus one layer down (which this package
// cannot import, being an unexported type of dbkit's external test
// package). Publish is synchronous, which is what makes the assertions
// below deterministic: the write-capture plugin publishes after the
// write's transaction commits, inside the same Create call, so by the time
// repo.Create returns every event this test must see has been recorded.
type notesCapturedBus struct {
	mu     sync.Mutex
	events []dbkit.WriteCapturedEvent
}

func (b *notesCapturedBus) Subscribe(string, pkgcore.EventHandler) {}

func (b *notesCapturedBus) Publish(_ context.Context, evt pkgcore.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	payload, ok := evt.Payload.(dbkit.WriteCapturedEvent)
	if !ok {
		return fmt.Errorf("notes_test: unexpected payload type %T", evt.Payload)
	}
	b.events = append(b.events, payload)
	return nil
}

func (b *notesCapturedBus) captured() []dbkit.WriteCapturedEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]dbkit.WriteCapturedEvent, len(b.events))
	copy(out, b.events)
	return out
}

var _ pkgcore.EventBus = (*notesCapturedBus)(nil)

// TestNote_AuditCapture_DefaultScope_CapturesTextRedacted pins the
// model-side half of the note-body plaintext protection (model.go's
// audit:"redact" tag on Note.Text, and its AuditResourceType doc
// comment): when a host wires dbkit.Options.AuditBus on a connection
// without restricting Options.AuditModels -- nil is dbkit's documented
// default capture semantics, under which every Auditable model written
// through the connection is captured, whole column values included -- a
// Note create must still never carry the note's body text into the
// captured payload that lands in the append-only audit trail. Note.Text
// is plaintext-sensitive tenant content (in this app's domain, the real
// content this placeholder field stands in for is patient data), so the
// tag must make the captured "text" column travel as the "[redacted]"
// marker: the key stays, because a diff reader must still see the write
// touched the column, while the plaintext appears nowhere in the payload
// (the exact payload the go/dbkit/audit persister serializes into
// audit_events.changes).
//
// The test lives in this model's own test file, not in cmd/server's,
// because the protection under test is declared on the model itself: it
// must hold on ANY connection whose capture scope admits Note, which is
// precisely the shape this app's own host-side exclusion (Note left off
// internal/app's Options.AuditModels list) cannot vouch for -- the tag is
// the layer that survives that one host list line being dropped or
// relaxed. It drives a real Note create through this package's real,
// migrated Repository over a real migrated SQLite file (the
// newAuditCaptureRepository harness), so it exercises the real capture
// plugin against the real model end to end. Before the tag existed on
// Note.Text, this test failed with the note body captured verbatim.
func TestNote_AuditCapture_DefaultScope_CapturesTextRedacted(t *testing.T) {
	bus := &notesCapturedBus{}
	repo := newAuditCaptureRepository(t, bus)

	const (
		tenantID = "tenant-a"
		body     = "note body that must never reach the audit trail verbatim"
		creator  = "user-1"
	)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID(tenantID))
	ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: creator})

	note := &Note{ID: uuid.NewString(), Text: body, CreatorUserID: creator}
	if err := repo.Create(ctx, note); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	events := bus.captured()
	if len(events) != 1 {
		t.Fatalf("captured %d WriteCapturedEvents, want exactly 1 (the default capture scope must capture the Note create)", len(events))
	}
	evt := events[0]
	if evt.ResourceType != "note" || evt.Operation != "create" || evt.Table != "notes" || evt.TenantID != tenantID {
		t.Fatalf("captured event = (resource %q, operation %q, table %q, tenant %q), want (note, create, notes, %q)",
			evt.ResourceType, evt.Operation, evt.Table, evt.TenantID, tenantID)
	}

	got, ok := evt.After["text"]
	if !ok {
		t.Fatalf("After = %+v, want a \"text\" key (the tag redacts the value; it does not drop the column from the diff)", evt.After)
	}
	if gotStr, isStr := got.(string); !isStr || gotStr != "[redacted]" {
		t.Errorf("After[\"text\"] = %#v, want the redacted marker \"[redacted]\" (must never be the note body %q)", got, body)
	}
	for key, value := range evt.After {
		if valueStr, isStr := value.(string); isStr && valueStr == body {
			t.Errorf("After[%q] carries the note body verbatim into the captured payload", key)
		}
	}
	// Control: the marker is per-field, never a whole-model opt-out.
	// creator_user_id is an ordinary column and is captured with its real
	// value, proving the write really was captured with live values and
	// the marker on "text" is the tag's doing.
	if got := evt.After["creator_user_id"]; got != creator {
		t.Errorf("After[\"creator_user_id\"] = %#v, want %q (an unredacted control column must be captured normally)", got, creator)
	}
}

// TestNote_ImplementsSoftDeletable is a runtime-checkable companion to
// model.go's compile-time `var _ dbkit.SoftDeletable = Note{}` assertion,
// mirroring TestNote_ImplementsTenantScoped's identical rationale.
func TestNote_ImplementsSoftDeletable(t *testing.T) {
	var _ dbkit.SoftDeletable = Note{}
}

// TestNote_GetDeletedAt_ReturnsFieldValue is a no-database sanity check
// that Note's GetDeletedAt method returns exactly the DeletedAt field it
// was given, mirroring TestNote_GetTenantID_ReturnsEmbeddedTenantModelValue's
// role for GetTenantID.
func TestNote_GetDeletedAt_ReturnsFieldValue(t *testing.T) {
	if got := (Note{}).GetDeletedAt(); got != nil {
		t.Fatalf("GetDeletedAt() on a zero-valued Note = %v, want nil", got)
	}

	now := time.Now()
	n := Note{DeletedAt: &now}
	got := n.GetDeletedAt()
	if got == nil || !got.Equal(now) {
		t.Fatalf("GetDeletedAt() = %v, want %v", got, now)
	}
}

// TestNote_FieldCaptureClasses_CoverEveryColumn is the field-granularity
// companion of org's TestModule_AuditableModels_CaptureClasses_CoverEveryField
// (go/org/module_test.go), applied to this module's one Auditable
// model. Note is deliberately outside internal/app's Options.AuditModels
// scope -- this app's note trail runs through audit.Emit instead (see
// Note.AuditResourceType's own doc comment) -- but the model keeps the
// dbkit.Auditable marker precisely because host wiring is not the
// protection: a host that wires capture with a nil or relaxed scope
// captures every Auditable model written through its connection whole, so
// Note.Text carries the audit:"redact" capture opt-out tag as the
// model-side protection that survives any wiring. This gate makes that
// protection field-complete: every schema column of Note must be
// auto-redacted by the capture mechanism (a gorm serializer, or the
// audit:"redact" tag) or be listed in judgedCaptured below with a written
// judgment that recording the column's plaintext in the audit trail is
// right. A plaintext-sensitive column added to Note with no tag and no
// judgment entry fails here instead of having its values land verbatim in
// the changes column of every captured row -- the column go/admin serves
// to any admin:audit_read holder across every tenant and compliance's
// RenderAuditReport exports verbatim (go/dbkit/audit/model.go's Changes
// doc comment names both exits).
//
// The auto-redaction criteria are read off the same struct tags dbkit's
// fieldValuesMap reads (go/dbkit/audit_capture.go), so a field classified
// auto-redacted here is exactly a field the mechanism captures as
// "[redacted]" -- the serializer half is tag-declared by construction,
// since dbkit's RegisterEncryptedSerializer mechanism is the only way a
// serializer exists in this ecosystem. If the mechanism's criteria ever
// change, this test and go/dbkit's own audit_capture_test.go change
// together.
func TestNote_FieldCaptureClasses_CoverEveryColumn(t *testing.T) {
	// The third class, written down: every non-auto-redacted column of
	// Note, keyed "<model>.<GoField>" as fmt %T prints the model, with the
	// judgment that its plaintext belongs in the trail. A new column
	// without a line here fails below.
	judgedCaptured := map[string]string{
		"notes.Note.ID":            "application-generated UUID naming the note -- identifier vocabulary, never content",
		"notes.Note.TenantID":      "owning-tenant attribution, the row's own first-class dimension -- never personal data",
		"notes.Note.CreatorUserID": "opaque user id attributing the note's creator -- the compliance subject key retention and erasure operate on; attribution, the trail's purpose, never a display name",
		"notes.Note.CreatedAt":     "auto-maintained timestamp (gorm autoCreateTime) -- never application content",
		"notes.Note.DeletedAt":     "soft-delete marker timestamp, written through dbkit's own reflection-based soft-delete path -- lifecycle metadata, never content",
		"notes.Note.DeletedBy":     "opaque user id of the deleting principal -- attribution, the trail's purpose, never a display name",
	}

	seen := map[string]bool{}
	model := fmt.Sprintf("%T", Note{})
	for _, field := range captureColumnFields(Note{}) {
		key := model + "." + field.goName
		seen[key] = true
		if captureColumnAutoRedacts(field.sf) {
			if reason, ok := judgedCaptured[key]; ok {
				t.Errorf("%s is auto-redacted by the capture mechanism (serializer or audit:\"redact\" tag) but still listed in judgedCaptured (%q) -- a stale judgment entry, not a class", key, reason)
			}
			continue
		}
		reason, ok := judgedCaptured[key]
		if !ok {
			t.Errorf("%s (column %q) has no capture class: it is not auto-redacted (no gorm serializer, no audit:\"redact\" tag) and is not listed in judgedCaptured -- its plaintext would land in every audit row's changes column, an exit readable by any admin:audit_read holder across all tenants", key, field.column)
			continue
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s is listed in judgedCaptured without a written judgment -- the reason IS the judgment, and an empty one records nothing", key)
		}
	}
	for key := range judgedCaptured {
		if !seen[key] {
			t.Errorf("judgedCaptured entry %q names no captured column field of Note -- a stale or misspelled judgment", key)
		}
	}
}

// captureColumnField is one schema-column leaf of an Auditable model as
// the write-capture plugin sees it: the model type flattened through its
// anonymous embedded structs (dbkit.TenantModel first among them), exactly
// the shape go/dbkit/audit_capture.go's fieldValuesMap walks. The helpers
// below mirror go/org/module_test.go's identically named ones, deliberately
// kept per-module: a shared home would be new public test-support API on
// dbkit (whose test-support package dbtest is scoped to dual-dialect
// database helpers), while each model-hosting module keeps its own gate
// next to its own models, the same shape org's model-level
// AuditableModels() gate already sets.
type captureColumnField struct {
	goName string // the leaf's own Go name ("TenantID", never the embedder chain)
	column string // the gorm column: option when the field declares one, else ""
	sf     reflect.StructField
}

// captureColumnFields enumerates every schema-column leaf of v's type:
// fields gorm would give a DBName, flattened through anonymous embedded
// structs and skipping unexported fields and gorm:"-" fields -- the
// mechanism's own empty-DBName skip, mirrored here so a gorm:"-" field
// (a value deliberately kept off the schema) is never demanded to carry a
// capture judgment it can never need.
func captureColumnFields(v any) []captureColumnField {
	var out []captureColumnField
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" {
				continue
			}
			if f.Anonymous && f.Type.Kind() == reflect.Struct {
				// GORM flattens an anonymous embedded struct into its own
				// fields (TenantModel's TenantID), so the gate must too:
				// the plugin captures the promoted column, not the embedder.
				walk(f.Type)
				continue
			}
			if f.Tag.Get("gorm") == "-" {
				continue
			}
			out = append(out, captureColumnField{
				goName: f.Name,
				column: gormColumnName(f),
				sf:     f,
			})
		}
	}
	walk(reflect.TypeOf(v))
	return out
}

// gormColumnName returns the column: option of f's gorm tag, or "" when
// the field declares none (gorm would then derive a name from the field's
// Go name -- the column still exists and the field still needs a class).
func gormColumnName(f reflect.StructField) string {
	for _, opt := range strings.Split(f.Tag.Get("gorm"), ";") {
		if rest, ok := strings.CutPrefix(opt, "column:"); ok {
			return rest
		}
	}
	return ""
}

// captureColumnAutoRedacts reports whether dbkit's write-capture plugin
// captures f's column as "[redacted]" on its own, by either of the two
// automatic criteria go/dbkit/audit_capture.go's fieldValuesMap applies:
// the audit:"redact" capture opt-out tag, or a GORM serializer declared
// through the gorm tag. The two are read off the same struct tags the
// mechanism itself reads (see the gate's doc comment above for why that
// mirror is exact in this ecosystem).
func captureColumnAutoRedacts(f reflect.StructField) bool {
	if v, ok := f.Tag.Lookup("audit"); ok && v == "redact" {
		return true
	}
	for _, opt := range strings.Split(f.Tag.Get("gorm"), ";") {
		if strings.HasPrefix(opt, "serializer:") {
			return true
		}
	}
	return false
}
