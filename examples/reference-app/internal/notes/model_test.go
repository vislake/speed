package notes

import (
	"context"
	"fmt"
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
// redundant with it today, but unlike that line, a test failure here shows
// up in `go test`'s own output instead of only a build error, which is
// easier for a future reader to spot in CI.
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
// content later milestones' fields stand in for is patient data), so the
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
// cmd/server's Options.AuditModels list) cannot vouch for -- the tag is
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
