package admin

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/vislake/speed/go/dbkit/audit"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// newTestEmitter returns an emitter over reg's bus with action declared on
// reg's audit-action seat -- the wiring shape Module.Register's
// attachAudit calls produce.
func newTestEmitter(t *testing.T, reg *pkgcore.ComponentRegistry, action string) auditEmitter {
	t.Helper()
	if err := componenttest.Declare(reg, func(r *pkgcore.ComponentRegistry) error {
		return r.AuditActions.Add(action)
	}); err != nil {
		t.Fatalf("register audit action %q: %v", action, err)
	}
	var e auditEmitter
	e.attach(reg.EventBus(), reg.AuditActions, nil)
	return e
}

// TestAuditEmitter_EmitBestEffort_PublishesThroughBus pins the emitter's
// core contract: with a bus attached, one dbkit audit event goes out
// carrying the Input verbatim -- action, resource, result and diff -- plus
// the identity already layered onto ctx.
func TestAuditEmitter_EmitBestEffort_PublishesThroughBus(t *testing.T) {
	const action = "admin.test.emitted"
	reg := newTestRegistry()
	e := newTestEmitter(t, reg, action)

	var recorded []audit.RecordedEvent
	reg.EventBus().Subscribe(audit.EventRecorded, func(_ context.Context, evt pkgcore.Event) error {
		rec, ok := evt.Payload.(audit.RecordedEvent)
		if !ok {
			t.Errorf("recorded payload type = %T, want audit.RecordedEvent", evt.Payload)
			return nil
		}
		recorded = append(recorded, rec)
		return nil
	})

	ctx := pkgcore.WithActor(context.Background(), pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: "operator-1"})
	e.emitBestEffort(ctx, "admin test emit failed", []any{"tenant_id", "tenant-1"}, audit.Input{
		Action:   action,
		Resource: audit.Resource{Type: "admin.thing", ID: "thing-1"},
		Result:   audit.Result{Success: true},
		Changes:  &audit.Diff{After: map[string]any{"field": "value"}},
	})

	if len(recorded) != 1 {
		t.Fatalf("published %d audit events, want exactly 1", len(recorded))
	}
	rec := recorded[0]
	if rec.Action != action {
		t.Errorf("recorded Action = %q, want %q", rec.Action, action)
	}
	if rec.Resource.Type != "admin.thing" || rec.Resource.ID != "thing-1" {
		t.Errorf("recorded Resource = %+v, want admin.thing/thing-1", rec.Resource)
	}
	if !rec.Result.Success || rec.Result.FailureReason != "" {
		t.Errorf("recorded Result = %+v, want Success true with no failure reason", rec.Result)
	}
	if rec.Changes == nil || rec.Changes.After["field"] != "value" {
		t.Errorf("recorded Changes = %+v, want the Input's diff verbatim", rec.Changes)
	}
	if rec.Actor.ID != "operator-1" || rec.Actor.Type != pkgcore.ActorTypePlatformAdmin {
		t.Errorf("recorded Actor = %+v, want the ctx actor layered by the caller", rec.Actor)
	}
}

// TestAuditEmitter_EmitBestEffort_UnattachedBusSkips pins the
// pre-Register tolerance: an emitter whose bus seat was never attached
// publishes nothing and does not panic.
func TestAuditEmitter_EmitBestEffort_UnattachedBusSkips(t *testing.T) {
	var e auditEmitter
	e.emitBestEffort(context.Background(), "admin test emit failed", nil, audit.Input{Action: "admin.test.unwired"})
}

// TestAuditEmitter_EmitBestEffort_PublishFailureWarnsAndSwallows pins the
// failure posture: a publish Emit refuses (here an action the attached
// catalog never declared, so Emit returns ErrActionNotRegistered) is
// surfaced as a Warn carrying the caller's message, its attrs and the
// error -- never returned and never a panic.
func TestAuditEmitter_EmitBestEffort_PublishFailureWarnsAndSwallows(t *testing.T) {
	reg := newTestRegistry()
	var e auditEmitter
	e.attach(reg.EventBus(), reg.AuditActions, nil)

	var logged bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := obs.WithLogger(context.Background(), logger)

	e.emitBestEffort(ctx, "admin test emit failed", []any{"tenant_id", "tenant-1"}, audit.Input{
		Action: "admin.test.unregistered",
	})

	out := logged.String()
	if !strings.Contains(out, "WARN") {
		t.Errorf("log output = %q, want a WARN-level record for the swallowed publish failure", out)
	}
	if !strings.Contains(out, "admin test emit failed") {
		t.Errorf("log output = %q, want the caller's own message", out)
	}
	if !strings.Contains(out, "tenant-1") {
		t.Errorf("log output = %q, want the caller's own attrs", out)
	}
	if !strings.Contains(out, "not registered") {
		t.Errorf("log output = %q, want the publish error carried by the record", out)
	}
}
