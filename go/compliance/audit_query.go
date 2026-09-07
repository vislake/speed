package compliance

import (
	"context"
	"sort"
	"time"

	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"
)

// QueryFilter narrows an AuditQuery read: every non-zero field must match
// for an audit.AuditEvent to be included. All fields are optional (their
// zero value matches everything), so the empty QueryFilter{} returns
// every event the tenant scope already selected, unfiltered.
type QueryFilter struct {
	// Actor, when non-empty, matches AuditEvent.ActorID exactly.
	Actor string
	// OnBehalfOf, when non-empty, matches AuditEvent.OnBehalfOfID exactly
	// -- the impersonation-accountability dimension (P2-1): an event
	// written during an impersonation session carries the impersonated
	// user as Actor and the real administrator behind the session as
	// OnBehalfOf (pkgcore's dual-identity rule), so filtering by
	// OnBehalfOf answers "what did this administrator do through their
	// impersonation sessions" -- a question the Actor dimension alone
	// cannot answer, since the administrator never appears as Actor on
	// such an event. Events carrying no OnBehalfOf identity at all never
	// match a non-empty OnBehalfOf.
	OnBehalfOf string
	// Resource, when non-empty, matches AuditEvent.ResourceType exactly.
	Resource string
	// Action, when non-empty, matches AuditEvent.Action exactly.
	Action string
	// From, when non-zero, excludes events that occurred strictly before it.
	From time.Time
	// To, when non-zero, excludes events that occurred strictly after it.
	To time.Time
	// Success, when non-nil, matches AuditEvent.Success exactly.
	Success *bool
}

// matches reports whether evt satisfies every set field of f.
func (f QueryFilter) matches(evt audit.AuditEvent) bool {
	if f.Actor != "" && evt.ActorID != f.Actor {
		return false
	}
	if f.OnBehalfOf != "" && (evt.OnBehalfOfID == nil || *evt.OnBehalfOfID != f.OnBehalfOf) {
		return false
	}
	if f.Resource != "" && evt.ResourceType != f.Resource {
		return false
	}
	if f.Action != "" && evt.Action != f.Action {
		return false
	}
	if !f.From.IsZero() && evt.OccurredAt.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && evt.OccurredAt.After(f.To) {
		return false
	}
	if f.Success != nil && evt.Success != *f.Success {
		return false
	}
	return true
}

// AuditQuery is compliance's read-only query layer over dbkit/audit.
// Repository's existing, deliberately thin ListByTenant/Get surface --
// the query-and-retention read side docs/internal/10-
// compliance-and-audit.md describes as compliance's own scope. It adds
// no method to audit.Repository itself and therefore cannot add an
// Update or a Delete: it holds only the Repository's existing exported
// methods and filters, in Go, what they return. This round's own scope
// deliberately does not touch go/dbkit, so AuditQuery's filtering runs
// entirely in application code rather than as SQL WHERE clauses -- an
// honest limitation for a tenant with a very large audit trail, recorded
// in AGENTS.md's Known limitations, not hidden behind a query-looking
// method name.
//
// The zero value is not ready to use; construct one with NewAuditQuery.
type AuditQuery struct {
	repo *audit.Repository
}

// NewAuditQuery returns an AuditQuery reading through repo.
func NewAuditQuery(repo *audit.Repository) *AuditQuery {
	return &AuditQuery{repo: repo}
}

// Query returns every audit event for the tenant ctx carries that matches
// filter, newest first -- the tenant-scoped read path: a tenant admin
// sees only their own tenant's events. The tenant to search
// comes from ctx via pkgcore.MustTenantFromContext, never from a caller-
// supplied parameter, per this repository's own API rule against
// accepting a caller-supplied tenant_id; a ctx with no tenant fails
// closed with pkgcore.ErrNoTenant before any read.
func (q *AuditQuery) Query(ctx context.Context, filter QueryFilter) ([]audit.AuditEvent, error) {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return nil, err
	}
	events, err := q.repo.ListByTenant(ctx, string(tenant))
	if err != nil {
		return nil, err
	}
	return filterAndSort(events, filter), nil
}

// QueryAcrossTenants is the platform-admin read path: a platform admin
// may see across tenants. ctx must carry a system
// context (pkgcore.SystemReasonFromContext) -- a cross-tenant audit read
// is exactly the kind of operation the escape hatch exists to gate --
// or this returns ErrAuditQueryRequiresSystemContext before any read.
//
// tenants names every tenant_id to search, the empty string included for
// platform-level events (audit.Repository.ListByTenant's own convention).
// The caller supplies this list explicitly -- typically every tenant a
// TenantLister returned -- because audit.Repository exposes no single
// "every tenant, all at once" read (a deliberate omission of dbkit/audit's
// own round, and this round does not touch dbkit to add one): a genuinely
// unbounded cross-tenant scan is composed here from one ListByTenant call
// per named tenant, so its cost scales with len(tenants), not with a
// dedicated cross-tenant index. Results are merged, filtered by filter,
// and sorted newest first across the whole set.
func (q *AuditQuery) QueryAcrossTenants(ctx context.Context, tenants []string, filter QueryFilter) ([]audit.AuditEvent, error) {
	if _, ok := pkgcore.SystemReasonFromContext(ctx); !ok {
		return nil, ErrAuditQueryRequiresSystemContext
	}
	var all []audit.AuditEvent
	for _, tenant := range tenants {
		events, err := q.repo.ListByTenant(ctx, tenant)
		if err != nil {
			return nil, err
		}
		all = append(all, events...)
	}
	return filterAndSort(all, filter), nil
}

// Get returns the single audit event with the given id, or (nil, nil)
// when no such row exists -- a thin passthrough to audit.Repository.Get.
// Get performs no tenant-scope check of its own: an id is an opaque,
// application-generated UUID (dbkit/audit's own Repository.Insert doc
// comment), never a value a caller could plausibly guess or enumerate,
// mirroring Repository.Get's own identical, deliberate lack of tenant
// filtering.
func (q *AuditQuery) Get(ctx context.Context, id string) (*audit.AuditEvent, error) {
	return q.repo.Get(ctx, id)
}

// filterAndSort returns the events in events matching filter, sorted
// newest first by OccurredAt. The ordering is a total order: events whose
// OccurredAt is identical -- a burst of records within the same instant,
// or timestamps truncated by a coarse clock -- are ordered by their ID,
// descending, as a deterministic tiebreaker. sort.Slice is not a stable
// sort, so without that tiebreak two events sharing an OccurredAt could
// come out in either order depending on the order ListByTenant happened to
// return them in, which would let a caller paging over the returned slice
// reorder same-timestamp events between requests; with it, two queries
// over the same rows always return the same order. ID is a random UUID
// (dbkit/audit's own Repository.Insert), so the tiebreak order itself is
// arbitrary -- it exists for determinism, not to express any recency
// claim within one timestamp.
func filterAndSort(events []audit.AuditEvent, filter QueryFilter) []audit.AuditEvent {
	out := make([]audit.AuditEvent, 0, len(events))
	for _, evt := range events {
		if filter.matches(evt) {
			out = append(out, evt)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OccurredAt.Equal(out[j].OccurredAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].OccurredAt.After(out[j].OccurredAt)
	})
	return out
}
