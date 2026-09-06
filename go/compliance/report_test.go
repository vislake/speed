package compliance

import (
	"encoding/csv"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/datatypes"

	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"
)

// mustMarshalChanges JSON-marshals v and returns it as a datatypes.JSON
// value, for building a realistic Changes column that -- like any real
// JSON object with more than one key -- already contains the commas and
// double quotes this file's CSV escaping tests care about.
func mustMarshalChanges(t *testing.T, v any) datatypes.JSON {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal(%#v): %v", v, err)
	}
	return datatypes.JSON(b)
}

// sampleAuditEventsForReport returns a small, deliberately awkward set of
// audit.AuditEvent values for RenderAuditReport's round-trip tests: one
// plain event with no impersonation and no diff, one with impersonation
// (OnBehalfOf) set, and one whose Action/FailureReason/Changes fields all
// carry commas, double quotes and (for FailureReason) an embedded newline
// -- exactly the characters CSV must quote/escape and JSON already
// handles for free.
func sampleAuditEventsForReport(t *testing.T) []audit.AuditEvent {
	t.Helper()

	plain := audit.AuditEvent{
		Action:     "notes.note.create",
		TenantID:   "tenant-a",
		IP:         "203.0.113.5",
		UserAgent:  "curl/8.0",
		TraceID:    "trace-1",
		OccurredAt: time.Date(2026, 1, 2, 3, 4, 5, 111000000, time.UTC),
	}
	plain.SetActor(pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1", DisplayName: "Ada"})
	plain.SetResource(audit.Resource{Type: "note", ID: "note-1", DisplayName: "Meeting notes"})
	plain.SetResult(audit.Result{Success: true})

	impersonated := audit.AuditEvent{
		Action:     "org.member.remove",
		TenantID:   "tenant-b",
		OccurredAt: time.Date(2026, 1, 2, 3, 4, 6, 222000000, time.UTC),
	}
	impersonated.SetActor(pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-2", DisplayName: "Bob"})
	impersonated.SetOnBehalfOf(&pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: "admin-1", DisplayName: "Carol"})
	impersonated.SetResource(audit.Resource{Type: "org.member", ID: "member-1", DisplayName: "Removed member"})
	impersonated.SetResult(audit.Result{Success: true})

	tricky := audit.AuditEvent{
		Action:     `billing.plan.change, urgent`,
		TenantID:   "tenant-c",
		OccurredAt: time.Date(2026, 1, 2, 3, 4, 7, 333000000, time.UTC),
	}
	tricky.SetActor(pkgcore.Actor{Type: pkgcore.ActorTypeSystem, ID: "system", DisplayName: `the "system" actor`})
	tricky.SetResource(audit.Resource{Type: "billing.plan", ID: "plan-1", DisplayName: "Pro, Annual"})
	tricky.SetResult(audit.Result{Success: false, FailureReason: "timeout, retrying\nfailed again with \"quotes\""})
	tricky.Changes = mustMarshalChanges(t, map[string]any{
		"before": map[string]any{"name": "a,b"},
		"after":  map[string]any{"name": `c"d`},
	})

	return []audit.AuditEvent{plain, impersonated, tricky}
}

// normalizeChanges collapses a nil/empty Changes value and JSON's literal
// "null" (what a nil datatypes.JSON value marshals to, and what its
// UnmarshalJSON stores back verbatim -- gorm.io/datatypes.JSON's own
// MarshalJSON/UnmarshalJSON pass the raw "null" bytes through rather than
// special-casing them back to a nil slice) to the same empty string, so a
// round trip through JSON is not falsely reported as lossy for the "no
// diff recorded" case every event without a Changes value already
// represents identically before and after.
func normalizeChanges(j datatypes.JSON) string {
	s := string(j)
	if s == "" || s == "null" {
		return ""
	}
	return s
}

// assertAuditEventsEqual compares got against want field by field,
// including OccurredAt (via time.Equal, since a JSON/CSV round trip can
// change a time.Time's internal representation -- monotonic reading,
// Location -- without changing the instant it names) and OnBehalfOf (via
// the reconstructed pkgcore.Actor pair, including its presence/absence).
func assertAuditEventsEqual(t *testing.T, got, want []audit.AuditEvent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d", len(got), len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		label := w.Action
		if g.Action != w.Action {
			t.Errorf("event %d: Action = %q, want %q", i, g.Action, w.Action)
		}
		if g.ActorType != w.ActorType || g.ActorID != w.ActorID || g.ActorDisplayName != w.ActorDisplayName {
			t.Errorf("event %d (%s): Actor = %+v, want %+v", i, label, g.Actor(), w.Actor())
		}
		gob, gok := g.OnBehalfOf()
		wob, wok := w.OnBehalfOf()
		if gok != wok || gob != wob {
			t.Errorf("event %d (%s): OnBehalfOf = (%+v, %v), want (%+v, %v)", i, label, gob, gok, wob, wok)
		}
		if g.ResourceType != w.ResourceType || g.ResourceID != w.ResourceID || g.ResourceDisplayName != w.ResourceDisplayName {
			t.Errorf("event %d (%s): Resource = %+v, want %+v", i, label, g.Resource(), w.Resource())
		}
		if g.Success != w.Success || g.FailureReason != w.FailureReason {
			t.Errorf("event %d (%s): Result = %+v, want %+v", i, label, g.Result(), w.Result())
		}
		if normalizeChanges(g.Changes) != normalizeChanges(w.Changes) {
			t.Errorf("event %d (%s): Changes = %q, want %q", i, label, g.Changes, w.Changes)
		}
		if g.TenantID != w.TenantID {
			t.Errorf("event %d (%s): TenantID = %q, want %q", i, label, g.TenantID, w.TenantID)
		}
		if g.IP != w.IP || g.UserAgent != w.UserAgent || g.TraceID != w.TraceID {
			t.Errorf("event %d (%s): IP/UserAgent/TraceID = %q/%q/%q, want %q/%q/%q", i, label, g.IP, g.UserAgent, g.TraceID, w.IP, w.UserAgent, w.TraceID)
		}
		if !g.OccurredAt.Equal(w.OccurredAt) {
			t.Errorf("event %d (%s): OccurredAt = %v, want %v", i, label, g.OccurredAt, w.OccurredAt)
		}
	}
}

func TestRenderAuditReport_JSON_RoundTrip(t *testing.T) {
	events := sampleAuditEventsForReport(t)

	b, err := RenderAuditReport(events, ReportFormatJSON)
	if err != nil {
		t.Fatalf("RenderAuditReport(JSON) error = %v", err)
	}

	var parsed []audit.AuditEvent
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("json.Unmarshal(rendered report): %v -- rendered = %s", err, b)
	}
	assertAuditEventsEqual(t, parsed, events)
}

func TestRenderAuditReport_JSON_EmptyEvents_RendersEmptyArray(t *testing.T) {
	b, err := RenderAuditReport(nil, ReportFormatJSON)
	if err != nil {
		t.Fatalf("RenderAuditReport(JSON, nil) error = %v", err)
	}
	if got, want := strings.TrimSpace(string(b)), "[]"; got != want {
		t.Errorf("RenderAuditReport(JSON, nil) = %q, want %q", got, want)
	}
}

// parseAuditReportCSV parses a report RenderAuditReport(..., ReportFormatCSV)
// produced back into []audit.AuditEvent, the mirror image of
// auditEventCSVRow -- kept private to this test file (report.go itself
// ships no parser, per this round's "smallest real, useful shape" scope:
// only a render direction is a real, asked-for API; round-trip
// correctness is what the test needs to prove, not a public CSV reader).
func parseAuditReportCSV(t *testing.T, b []byte) []audit.AuditEvent {
	t.Helper()
	r := csv.NewReader(strings.NewReader(string(b)))
	records, err := r.ReadAll()
	if err != nil {
		t.Fatalf("csv.ReadAll(rendered report): %v -- rendered = %s", err, b)
	}
	if len(records) == 0 {
		t.Fatal("parsed CSV report has no rows at all, want at least a header row")
	}
	if !reflect.DeepEqual(records[0], auditReportCSVHeader) {
		t.Fatalf("CSV header = %v, want %v", records[0], auditReportCSVHeader)
	}

	out := make([]audit.AuditEvent, 0, len(records)-1)
	for _, row := range records[1:] {
		if len(row) != len(auditReportCSVHeader) {
			t.Fatalf("CSV row %v has %d fields, want %d", row, len(row), len(auditReportCSVHeader))
		}
		success, err := strconv.ParseBool(row[12])
		if err != nil {
			t.Fatalf("parse success column %q: %v", row[12], err)
		}
		occurredAt, err := time.Parse(time.RFC3339Nano, row[19])
		if err != nil {
			t.Fatalf("parse occurred_at column %q: %v", row[19], err)
		}

		evt := audit.AuditEvent{
			ID:         row[0],
			Action:     row[8],
			TenantID:   row[15],
			IP:         row[16],
			UserAgent:  row[17],
			TraceID:    row[18],
			OccurredAt: occurredAt,
		}
		evt.SetActor(pkgcore.Actor{Type: pkgcore.ActorType(row[1]), ID: row[2], DisplayName: row[3]})
		evt.SetResource(audit.Resource{Type: row[9], ID: row[10], DisplayName: row[11]})
		evt.SetResult(audit.Result{Success: success, FailureReason: row[13]})
		if row[4] == "true" {
			ob := pkgcore.Actor{Type: pkgcore.ActorType(row[5]), ID: row[6], DisplayName: row[7]}
			evt.SetOnBehalfOf(&ob)
		}
		if row[14] != "" {
			evt.Changes = datatypes.JSON(row[14])
		}
		out = append(out, evt)
	}
	return out
}

func TestRenderAuditReport_CSV_RoundTrip(t *testing.T) {
	events := sampleAuditEventsForReport(t)

	b, err := RenderAuditReport(events, ReportFormatCSV)
	if err != nil {
		t.Fatalf("RenderAuditReport(CSV) error = %v", err)
	}

	parsed := parseAuditReportCSV(t, b)
	assertAuditEventsEqual(t, parsed, events)
}

// TestRenderAuditReport_CSV_EscapesCommaAndQuote proves the CSV rendering
// path actually quotes/escapes a field containing a comma or a double
// quote, rather than merely happening to round-trip correctly through a
// parser that could paper over unescaped output. It asserts on the raw
// bytes: a value containing a comma must appear wrapped in double quotes,
// and an embedded double quote must appear doubled, per RFC 4180 --
// exactly what encoding/csv's Writer already guarantees, and what this
// test pins so a future change away from encoding/csv would be caught.
func TestRenderAuditReport_CSV_EscapesCommaAndQuote(t *testing.T) {
	events := sampleAuditEventsForReport(t)
	tricky := events[2]

	b, err := RenderAuditReport(events, ReportFormatCSV)
	if err != nil {
		t.Fatalf("RenderAuditReport(CSV) error = %v", err)
	}
	rendered := string(b)

	// The comma-containing Action must be quoted as a single CSV field,
	// not split into two fields by the comma inside it.
	if !strings.Contains(rendered, `"billing.plan.change, urgent"`) {
		t.Errorf("rendered CSV does not contain the comma-bearing Action quoted as one field; rendered = %s", rendered)
	}
	// The double-quote-containing ActorDisplayName must have its internal
	// quotes doubled, per RFC 4180.
	if !strings.Contains(rendered, `"the ""system"" actor"`) {
		t.Errorf("rendered CSV does not contain the quote-bearing ActorDisplayName correctly doubled; rendered = %s", rendered)
	}
	// The Changes column, a real JSON object, is dense with both commas
	// and quotes -- confirm it is present verbatim (as a quoted field)
	// rather than corrupted.
	if !strings.Contains(rendered, escapeCSVField(string(tricky.Changes))) {
		t.Errorf("rendered CSV does not contain the Changes JSON object correctly quoted; rendered = %s", rendered)
	}
}

// escapeCSVField renders s exactly as encoding/csv.Writer would render it
// as one quoted field (doubling internal quotes, wrapping in double
// quotes) -- used only to build the expected substring in the test above,
// never by report.go itself.
func escapeCSVField(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func TestRenderAuditReport_CSV_EmptyEvents_RendersHeaderOnly(t *testing.T) {
	b, err := RenderAuditReport(nil, ReportFormatCSV)
	if err != nil {
		t.Fatalf("RenderAuditReport(CSV, nil) error = %v", err)
	}
	r := csv.NewReader(strings.NewReader(string(b)))
	records, err := r.ReadAll()
	if err != nil {
		t.Fatalf("csv.ReadAll: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d CSV rows for zero events, want exactly 1 (the header)", len(records))
	}
	if !reflect.DeepEqual(records[0], auditReportCSVHeader) {
		t.Errorf("header row = %v, want %v", records[0], auditReportCSVHeader)
	}
}

func TestRenderAuditReport_UnsupportedFormat_ReturnsError(t *testing.T) {
	_, err := RenderAuditReport(sampleAuditEventsForReport(t), ReportFormat("xml"))
	if !hasCode(err, ErrUnsupportedReportFormat.Code) {
		t.Fatalf("RenderAuditReport(unsupported format) error = %v, want %s", err, ErrUnsupportedReportFormat.Code)
	}
}
