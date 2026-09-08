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
	// internal quotes doubled) exactly as that standard requires. Every
	// data cell whose content begins with a spreadsheet-formula character
	// (= + - @ tab or carriage return) is additionally single-quote-
	// prefixed (protectCSVCell), so no cell a hostile subject or request
	// header managed to plant in the report can execute as a formula when
	// the file is opened in a spreadsheet application.
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
		// propagated rather than assumed impossible.
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
// auditReportCSVHeader's column order field for field, every cell passed
// through protectCSVCell first so a cell whose content begins with a
// spreadsheet-formula character cannot execute as a formula when the
// report is opened in a spreadsheet application. OccurredAt is rendered as
// UTC RFC3339Nano, matching the wire convention dbkit/audit/module.go's own
// timeFromWire already uses for the identical reason: nanosecond precision
// round-trips exactly, and UTC removes any ambiguity a local offset could
// introduce.
func auditEventCSVRow(evt audit.AuditEvent) []string {
	hasOnBehalfOf := "false"
	var onBehalfOfType, onBehalfOfID, onBehalfOfDisplayName string
	if ob, ok := evt.OnBehalfOf(); ok {
		hasOnBehalfOf = "true"
		onBehalfOfType = string(ob.Type)
		onBehalfOfID = ob.ID
		onBehalfOfDisplayName = ob.DisplayName
	}

	row := []string{
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
	for i := range row {
		row[i] = protectCSVCell(row[i])
	}
	return row
}

// protectCSVCell returns s prefixed with a single quote when s begins with
// one of the spreadsheet-formula characters (OWASP's CSV-injection list:
// = + - @ tab and carriage return), and s unchanged otherwise. A cell a
// spreadsheet would otherwise interpret as a formula -- an actor display
// name, a failure reason or a user agent crafted by the very subject an
// audit report records are exactly such cells -- becomes inert text instead
// (a leading single quote is text, never a formula, in every mainstream
// spreadsheet), while the report itself stays RFC 4180-valid: the quote is
// an ordinary character encoding/csv's Writer quotes further only as that
// standard requires.
//
// Protection is deliberately uniform across every data column rather than
// whitelisted per "user-influenced" column: a column's provenance is a
// maintenance liability (the next column added to auditReportCSVHeader
// inherits the guarantee for free), and the prefix costs nothing where no
// legitimate value of this report begins with a formula character anyway --
// ids, timestamps, booleans and the module's own action strings never do.
// The one honest cost is documented, not hidden: a protected cell does not
// round-trip byte-identically through this module's own test parser (its
// "true" content sits behind the prefixing quote, which a machine reader
// that knows this convention strips exactly once). Refusing such a cell
// outright was considered and rejected: a report's whole export would then
// fail over one hostile field, handing the attacker denial of service over
// a document's content.
func protectCSVCell(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	default:
		return s
	}
}
