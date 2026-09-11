package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// Repository is the audit_events table accessor. It is deliberately a
// thin row accessor over a plain *gorm.DB -- never dbkit.Repository[T] --
// because AuditEvent is platform data with a real, non-enforced tenant_id
// column, not tenant-scoped data (see model.go's own doc comment, and the
// same pattern in go/jobs's jobRecord and go/config's row). Repository
// needs no .Table/.Model/.Raw escape hatch: Create/Where/Order/Find all
// suffice.
//
// Repository is append-only by construction: it exposes Insert and two
// read methods (Get, ListByTenant) and NO Update or Delete method at all.
// An audit trail an operator can edit or remove after the fact is not an
// audit trail; this package enforces that at the application layer by the
// simple absence of a mutating method -- a property model_test.go's
// TestRepository_HasNoUpdateOrDeleteMethod proves by reflecting over
// Repository's method set, since Go has no way to express "this type
// lacks a method" any other way that fails loudly on a regression. A
// second, database-level backstop against a caller that bypasses
// Repository entirely -- a raw connection, a bug, a careless future
// migration -- also exists: migrations/{postgres,sqlite}/0002_append_
// only_enforcement.sql installs a trigger pair on audit_events that
// refuses any UPDATE or DELETE regardless of which role issues it,
// because a restricted role alone could not be provisioned by this
// package. A hash chain over the table is not built here.
type Repository struct {
	db *gorm.DB
}

// NewRepository returns a Repository backed by db. db is expected to come
// from dbkit.Open, with the audit_events table already migrated (see
// migrations/fs.go and dbkit.MigrationRegistry) -- constructing a
// Repository performs no I/O of its own.
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

// ErrEventFieldTooLong is returned by Insert when one of evt's identifier
// or vocabulary fields -- id, actor_type, actor_id, on_behalf_of_type,
// on_behalf_of_id, action, resource_type, resource_id or tenant_id --
// exceeds the width its column declares in the migration files (model.go's
// column-bounds constants). Such a value is a caller bug, never legal
// content: the module and its callers generate UUIDs, and action/type
// strings come from registered vocabularies, all of which are bounded far
// below the columns' widths. Refusing here -- before the write, on both
// dialects alike -- keeps the outcome dialect-identical, where leaving it
// to the database would let SQLite store the row silently and PostgreSQL
// refuse it with 22001. Descriptive columns (display names, failure
// reason, the request-metadata trio) are NOT refused: an
// over-long value there is legal content and is cut instead, with the cut
// recorded in a structured warning (see fitEventToColumns).
var ErrEventFieldTooLong = errors.New("audit: event field exceeds its declared column width")

// fitEventToColumns enforces audit_events' own column widths on evt,
// immediately before the INSERT, so no caller-supplied value can make the
// same Insert succeed on one dialect and fail on the other. It runs at
// this one choke point -- every row this table ever receives passes
// through Repository.Insert, whether it was authored by Emit's
// RecordedEvent, the write-capture plugin's WriteCapturedEvent, the
// tenancy system-context subscriber, or a direct Insert caller -- because
// no upstream site knows the full set of sources that will ever feed the
// table (model.go's column-bounds constants document the wider-than-target
// pairings the audit column review actually found).
//
// The two column classes get two treatments (see model.go's constants for
// the rationale):
//
//   - Identifier and vocabulary fields are REFUSED with
//     ErrEventFieldTooLong when over width. A cut there would rewrite the
//     record's own identity -- a truncated id does not deduplicate
//     (InsertIdempotent) or read back (Get), and a truncated tenant or
//     resource id misidentifies the record -- so an over-wide value is
//     surfaced as the caller bug it is, identically on both dialects,
//     rather than being silently rewritten or left to PostgreSQL's 22001.
//
//   - Descriptive fields -- ActorDisplayName, OnBehalfOfDisplayName,
//     ResourceDisplayName, FailureReason, IP, UserAgent, TraceID -- are
//     CUT to their column's width, and each cut is recorded in a
//     structured warning (slog.Default().WarnContext, the same
//     alert-not-fail path auditPublishFailed uses in go/dbkit/audit_
//     capture.go -- this package sits below go/observability and cannot
//     depend on it, so log/slog directly, mirroring that function's
//     documented reasoning). The warning names the field, the column
//     width, the value's original length and the row context (event id,
//     action, tenant), so the truncation -- an integrity loss that would
//     otherwise be unrecordable once the database had refused or stored
//     silently -- leaves an operator-recoverable trace. A database-layer
//     truncation has no such trace by construction, which is why the cut
//     happens here and not in the schema.
//
// The cut itself is the shared write-boundary fit, dbkit.FitColumnValue:
// rune-safe, and sanitizing invalid UTF-8 runs to the Unicode replacement
// character first -- a UTF-8-encoded PostgreSQL database refuses raw
// invalid bytes with 22021, and dropping them could concatenate two
// arbitrary byte runs into a different valid value.
func fitEventToColumns(ctx context.Context, evt *AuditEvent) error {
	refusals := []struct {
		field, column string
		value         string
		runes         int
	}{
		{"ID", "id", evt.ID, idColumnRunes},
		{"ActorType", "actor_type", evt.ActorType, actorTypeColumnRunes},
		{"ActorID", "actor_id", evt.ActorID, actorIDColumnRunes},
		{"OnBehalfOfType", "on_behalf_of_type", derefOrEmpty(evt.OnBehalfOfType), onBehalfOfTypeColumnRunes},
		{"OnBehalfOfID", "on_behalf_of_id", derefOrEmpty(evt.OnBehalfOfID), onBehalfOfIDColumnRunes},
		{"Action", "action", evt.Action, actionColumnRunes},
		{"ResourceType", "resource_type", evt.ResourceType, resourceTypeColumnRunes},
		{"ResourceID", "resource_id", evt.ResourceID, resourceIDColumnRunes},
		{"TenantID", "tenant_id", evt.TenantID, tenantIDColumnRunes},
	}
	for _, c := range refusals {
		if n := utf8.RuneCountInString(c.value); n > c.runes {
			return fmt.Errorf("%w: field %s (%s) is %d characters; the column holds at most %d",
				ErrEventFieldTooLong, c.field, c.column, n, c.runes)
		}
	}

	cuts := []struct {
		field, column string
		dst           *string
		runes         int
	}{
		{"ActorDisplayName", "actor_display_name", &evt.ActorDisplayName, actorDisplayNameColumnRunes},
		{"OnBehalfOfDisplayName", "on_behalf_of_display_name", evt.OnBehalfOfDisplayName, onBehalfOfDisplayNameColumnRunes},
		{"ResourceDisplayName", "resource_display_name", &evt.ResourceDisplayName, resourceDisplayNameColumnRunes},
		{"FailureReason", "failure_reason", &evt.FailureReason, failureReasonColumnRunes},
		{"IP", "ip", &evt.IP, ipColumnRunes},
		{"UserAgent", "user_agent", &evt.UserAgent, userAgentColumnRunes},
		{"TraceID", "trace_id", &evt.TraceID, traceIDColumnRunes},
	}
	for _, c := range cuts {
		if c.dst == nil {
			continue
		}
		fitted, cut := dbkit.FitColumnValue(*c.dst, c.runes)
		if fitted == *c.dst {
			continue
		}
		reason := "invalid_utf8"
		if cut {
			reason = "column_width"
		}
		slog.Default().WarnContext(ctx, "audit: audit event field value changed to fit its column",
			"field", c.field,
			"column", c.column,
			"original_chars", utf8.RuneCountInString(*c.dst),
			"limit_chars", c.runes,
			"reason", reason,
			"action", evt.Action,
			"tenant_id", evt.TenantID,
			"event_id", evt.ID,
		)
		*c.dst = fitted
	}
	return nil
}

// Insert appends evt to the audit trail. evt.ID is generated -- a
// version-4 UUID, application-side, per the backend coding standard's
// no-gen_random_uuid() rule -- when the caller leaves it empty; evt is
// mutated in place so the caller can read back the generated ID.
// evt.OccurredAt is left to GORM's autoCreateTime when the caller leaves
// it at its zero value.
//
// Insert never updates an existing row: a duplicate, caller-supplied ID
// fails exactly as any other primary-key conflict would, because
// Repository has no notion of "the same event happening again" to
// reconcile -- every call to Insert is a new fact.
//
// Before the row is written, fitEventToColumns enforces this table's own
// column widths in Go -- see that function's doc comment, and model.go's
// column-bounds constants, for why the audit module's own write path is
// where every caller-supplied value is fitted to its column: a value the
// database would refuse with 22001 on PostgreSQL (while SQLite stores it
// silently, its VARCHAR bounds being unenforced) is a dual-dialect
// divergence this write path exists to make impossible. The full account
// -- the wider-than-target source pairings the sweep found (an
// integration webhook URL, a sharing resource_ref, both feeding
// resource_display_name), and the truncate-with-trace vs refuse choice
// per column class -- lives in go/dbkit/audit/AGENTS.md's "Column
// bounds" section.
func (r *Repository) Insert(ctx context.Context, evt *AuditEvent) error {
	if evt.ID == "" {
		evt.ID = uuid.NewString()
	}
	if err := fitEventToColumns(ctx, evt); err != nil {
		return err
	}
	if err := r.db.WithContext(ctx).Create(evt).Error; err != nil {
		return fmt.Errorf("audit: insert audit event: %w", err)
	}
	return nil
}

// InsertIdempotent behaves exactly like Insert, except a duplicate primary
// key -- evt.ID colliding with an already-persisted row -- is treated as
// "this exact event was already recorded" rather than an error: it
// returns nil without inserting a second row, leaving the existing one
// untouched (Repository has no Update method, and this is not one either).
//
// This exists for Module's own event subscribers alone (onWriteCaptured,
// onRecorded, onSystemContextEntered in module.go), which set evt.ID to a
// value deterministically derived from the event's own content before
// calling this method (see module.go's auditDeterministicEventID) rather
// than leaving it for Insert to randomly generate. That derivation is what
// makes the dedup possible: in distributed deployment mode,
// pkgcore.RedisEventBus delivers every event to every replica once each
// (its own doc comment: "each event is delivered to every replica exactly
// once" -- once per replica, not once system-wide, by design, since most
// subscribers are not writing to one shared row), so a single real action
// independently reaches every replica's Module subscriber. Without this
// method, each of those independent deliveries would call Insert with
// evt.ID left empty, each generating its own distinct random UUID, so a
// single note.create would leave N audit_events rows for N replicas
// instead of 1 -- see go/dbkit/audit/AGENTS.md's "Multi-replica delivery"
// section for the full write-up.
//
// Insert itself keeps its stricter, non-idempotent contract for every
// other caller: a caller-supplied duplicate ID from outside this
// package's own deterministic derivation remains a genuine error, since
// Repository has no way to know whether that caller intended idempotent
// retry semantics or made an honest mistake.
func (r *Repository) InsertIdempotent(ctx context.Context, evt *AuditEvent) error {
	err := r.Insert(ctx, evt)
	if err == nil || errors.Is(err, gorm.ErrDuplicatedKey) {
		return nil
	}
	return err
}

// Get returns the audit event with the given id, or (nil, nil) when no
// such row exists -- mirroring go/config's (*store).get convention for
// platform data, rather than dbkit.Repository[T]'s ErrRecordNotFound
// (which presumes a tenant-scoped lookup this table's cross-tenant reads
// are not).
func (r *Repository) Get(ctx context.Context, id string) (*AuditEvent, error) {
	var out AuditEvent
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&out).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("audit: get audit event %q: %w", id, err)
	}
	return &out, nil
}

// ListByTenant returns every audit event recorded for tenantID, newest
// first, using the migration's (tenant_id, occurred_at) index. It is the
// minimal read surface this package ships to prove persistence end to
// end (its own tests, and the reference app's proof test); the full
// actor/resource/action/time-range/result query API is not built here --
// go/compliance's AuditQuery is the closest read surface over this
// table.
//
// tenantID may be the empty string, which returns every platform-level
// event (the empty-tenant_id sentinel go/config's row and go/jobs's
// jobRecord already use). It is not a wildcard for "every tenant, and
// every platform event, together" -- ListByTenant does no tenant-context
// filtering of its own at all (this is platform data, read through a
// plain *gorm.DB with no isolation plugin active on it): the caller names
// exactly the tenant_id value it wants back, empty string included.
func (r *Repository) ListByTenant(ctx context.Context, tenantID string) ([]AuditEvent, error) {
	var out []AuditEvent
	err := r.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Order("occurred_at DESC").
		Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("audit: list audit events for tenant: %w", err)
	}
	return out, nil
}
