package compliance

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/vislake/speed/go/dbkit/audit"
)

// ReportFormat selects RenderAuditReport's output encoding.
type ReportFormat string

const (
	// ReportFormatCSV renders one row per audit.AuditEvent -- a header row
	// first, then one row per event, in auditReportCSVHeader's column
	// order -- using encoding/csv's RFC 4180 quoting, so a field
	// containing a comma, a double quote or a newline is quoted (and its
	// internal quotes doubled) exactly as that standard requires.
	ReportFormatCSV ReportFormat = "csv"

	// ReportFormatJSON renders events as a JSON array of audit.AuditEvent
	// values, in encoding/json's ordinary exported-field-name shape (the
	// same shape AuditEvent already uses everywhere else in this
	// codebase -- it carries no report-specific struct of its own).
	ReportFormatJSON ReportFormat = "json"
)

// auditReportCSVHeader is RenderAuditReport's CSV column order -- every
// field AuditEvent carries, flattened the same way AuditEvent's own
// exported columns already are. has_on_behalf_of is a real column of its
// own, not left implicit: OnBehalfOf is a genuinely nullable Actor (see
// audit.AuditEvent's own doc comment on why NULL, not an empty-string
// sentinel, means "no impersonation"), and a CSV cell has no way to
// distinguish an empty string from an absent one on its own -- an
// explicit boolean column is what makes the round trip lossless rather
// than collapsing "no impersonation" and "impersonated by an actor with
// empty fields" into the same three blank cells.
var auditReportCSVHeader = []string{
	"id",
	"actor_type", "actor_id", "actor_display_name",
	"has_on_behalf_of", "on_behalf_of_type", "on_behalf_of_id", "on_behalf_of_display_name",
	"action",
	"resource_type", "resource_id", "resource_display_name",
	"success", "failure_reason",
	"changes",
	"tenant_id", "ip", "user_agent", "trace_id",
	"occurred_at",
}

// RenderAuditReport renders events -- typically an AuditQuery.Query or
// AuditQuery.QueryAcrossTenants result -- as a formatted audit report in
// format, returning the rendered bytes. It is a pure function: no I/O, no
// database access and no dependency on AuditQuery itself, so it composes
// with any []audit.AuditEvent a caller already has in hand (a real query
// result, a filtered subset of one, or a hand-built slice in a test).
//
// RenderAuditReport does no pagination and enforces no tenant scope of
// its own -- both are AuditQuery.Query/QueryAcrossTenants's own job,
// upstream of this call; RenderAuditReport renders exactly the events it
// is given, in the order it is given them.
//
// format must be ReportFormatCSV or ReportFormatJSON; any other value
// (including the zero value) returns ErrUnsupportedReportFormat before
// anything is rendered.
func RenderAuditReport(events []audit.AuditEvent, format ReportFormat) ([]byte, error) {
	switch format {
	case ReportFormatJSON:
		return renderAuditReportJSON(events)
	case ReportFormatCSV:
		return renderAuditReportCSV(events)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedReportFormat, format)
	}
}

// renderAuditReportJSON marshals events as a JSON array, normalizing a nil
// slice to an empty array ("[]") rather than JSON "null" -- a report with
// zero matching events is still a well-formed, parseable report, not an
// absent value.
func renderAuditReportJSON(events []audit.AuditEvent) ([]byte, error) {
	if events == nil {
		events = []audit.AuditEvent{}
	}
	b, err := json.Marshal(events)
	if err != nil {
		// json.Marshal only fails on a value it cannot represent at all
		// (a channel, a function, a cyclic map) -- not a realistic shape
		// for audit.AuditEvent's plain fields -- but the error is
		// propagated rather than assumed impossible, per this
		// repository's own error-handling discipline.
		return nil, fmt.Errorf("compliance: render JSON audit report: %w", err)
	}
	return b, nil
}

// renderAuditReportCSV writes events as CSV: auditReportCSVHeader first,
// then one row per event via auditEventCSVRow, through encoding/csv's
// Writer -- which is what supplies the RFC 4180 quoting a field
// containing a comma, a double quote or a newline needs (a raw JSON
// Changes value routinely contains both commas and quotes, which is
// exactly the case this delegation exists to get right rather than
// hand-rolling).
func renderAuditReportCSV(events []audit.AuditEvent) ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)

	if err := w.Write(auditReportCSVHeader); err != nil {
		return nil, fmt.Errorf("compliance: write CSV audit report header: %w", err)
	}
	for _, evt := range events {
		if err := w.Write(auditEventCSVRow(evt)); err != nil {
			return nil, fmt.Errorf("compliance: write CSV audit report row %q: %w", evt.ID, err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, fmt.Errorf("compliance: flush CSV audit report: %w", err)
	}
	return buf.Bytes(), nil
}

// auditEventCSVRow flattens evt into one CSV row matching
// auditReportCSVHeader's column order field for field. OccurredAt is
// rendered as UTC RFC3339Nano, matching the wire convention
// dbkit/audit/module.go's own timeFromWire already uses for the identical
// reason: nanosecond precision round-trips exactly, and UTC removes any
// ambiguity a local offset could introduce.
func auditEventCSVRow(evt audit.AuditEvent) []string {
	hasOnBehalfOf := "false"
	var onBehalfOfType, onBehalfOfID, onBehalfOfDisplayName string
	if ob, ok := evt.OnBehalfOf(); ok {
		hasOnBehalfOf = "true"
		onBehalfOfType = string(ob.Type)
		onBehalfOfID = ob.ID
		onBehalfOfDisplayName = ob.DisplayName
	}

	return []string{
		evt.ID,
		evt.ActorType, evt.ActorID, evt.ActorDisplayName,
		hasOnBehalfOf, onBehalfOfType, onBehalfOfID, onBehalfOfDisplayName,
		evt.Action,
		evt.ResourceType, evt.ResourceID, evt.ResourceDisplayName,
		strconv.FormatBool(evt.Success), evt.FailureReason,
		string(evt.Changes),
		evt.TenantID, evt.IP, evt.UserAgent, evt.TraceID,
		evt.OccurredAt.UTC().Format(time.RFC3339Nano),
	}
}
