package audit

import (
	"time"

	"gorm.io/datatypes"

	"github.com/vislake/speed/go/pkgcore"
)

// auditEventsTable is the shared audit_events table name.
const auditEventsTable = "audit_events"

// Resource identifies what an audited action was performed on: the
// flattened form of the "Resource" element of docs/internal/10-compliance-
// and-audit.md's six-element AuditEvent shape (Actor, OnBehalfOf, Action,
// Resource, Result, Changes). Type is a module-owned string such as
// "note" or "org.member" -- there is no closed enumeration, since every
// business module names its own resources.
type Resource struct {
	Type        string
	ID          string
	DisplayName string
}

// Result is the flattened outcome of an audited action: the "Result"
// element of the six-element shape. FailureReason is meaningful only when
// Success is false.
type Result struct {
	Success       bool
	FailureReason string
}

// AuditEvent is one append-only record of "who did what to what, and what
// happened" -- the six-element shape docs/internal/10-compliance-and-audit.
// md defines (Actor, OnBehalfOf, Action, Resource, Result, Changes), plus
// the Context fields (OccurredAt, IP, UserAgent, TraceID, TenantID) every
// record carries regardless of which module produced it.
//
// Every element except OnBehalfOf is flattened directly onto columns
// (ActorType/ID/DisplayName, ResourceType/ID/DisplayName, Success/
// FailureReason) rather than stored as a nested GORM-embedded struct: this
// mirrors the flat-field shape already used by go/config's row and
// go/jobs's jobRecord, and sidesteps the ambiguity of what a nil embedded
// pointer struct should mean to GORM's Create -- SetActor/Actor,
// SetResource/Resource and SetResult/Result convert between the flattened
// columns and pkgcore.Actor / Resource / Result at the package boundary, so
// callers never have to touch the six *_type/_id/_display_name columns
// directly.
//
// Data-domain classification (docs/internal/04-data-and-tenancy.md): an
// audit event is neither purely tenant data nor purely platform data -- a
// tenant-scoped action produces a tenant-attributed record, but a
// platform-level action (a platform admin's tenancy.WithSystemContext
// grant, a cross-tenant admin search) produces one with no tenant at all,
// and an operator must be able to read the latter regardless of which
// tenant, if any, their own request happens to be scoped to. AuditEvent is
// therefore treated as platform data with a real, non-enforced tenant_id
// column -- the same treatment go/jobs's jobRecord and go/config's row
// already get for the identical reason -- and it deliberately does NOT
// implement dbkit.TenantScoped (no GetTenantID method exists on it, and it
// embeds nothing that would promote one): doing so would put it behind
// dbkit's tenant-scoping plugin and dbkit.Repository[T], both of which fail
// a query closed the moment its context carries no tenant, which is
// exactly the platform-level case this table must still serve.
// model_test.go's TestAuditEvent_DoesNotImplementTenantScoped and
// TestAuditEvent_VisibilityDoesNotDependOnTenantContext are the standing
// proof (see that file's own doc comment for why they do not use
// tenancytest.AssertNotTenantScoped, the way go/config and go/jobs do, to
// prove the same property of their own platform-data models). TenantID is
// the empty string, never NULL, on a platform-level record -- the same
// empty-string sentinel go/config's row and go/jobs's jobRecord already
// use, chosen for the same reason documented there: NULLs are distinct in
// a PostgreSQL unique index/lookup, while an empty string collapses
// cleanly under an ordinary equality WHERE.
//
// See Repository's own doc comment for why AuditEvent is queried through a
// plain *gorm.DB, never a dbkit.Repository[T], and for how M1 enforces
// "no UPDATE, no DELETE" on this table at the application layer.
type AuditEvent struct {
	// ID is an application-generated UUID (see Repository.Insert), never a
	// database-generated one -- the backend coding standard (§5) forbids
	// gen_random_uuid().
	ID string `gorm:"column:id;primaryKey;size:36"`

	// ActorType, ActorID and ActorDisplayName are the flattened form of
	// the acting pkgcore.Actor -- who performed the action. Use SetActor
	// and Actor to read and write all three together.
	ActorType        string `gorm:"column:actor_type;size:32;not null"`
	ActorID          string `gorm:"column:actor_id;size:255;not null"`
	ActorDisplayName string `gorm:"column:actor_display_name;size:255;not null"`

	// OnBehalfOfType, OnBehalfOfID and OnBehalfOfDisplayName are the
	// flattened form of an OPTIONAL pkgcore.Actor: the real administrator
	// behind an impersonated Actor above (docs/internal/10-compliance-and-
	// audit.md's dual-identity rule). All three are genuinely nullable
	// columns (NULL, never an empty-string sentinel), so that "no
	// impersonation" is distinguishable from "impersonated by an actor
	// whose fields happen to be empty". Use SetOnBehalfOf and OnBehalfOf to
	// read and write all three together, rather than touching the three
	// fields individually, so they can never end up in a partial
	// (some-nil, some-not) state.
	OnBehalfOfType        *string `gorm:"column:on_behalf_of_type;size:32"`
	OnBehalfOfID          *string `gorm:"column:on_behalf_of_id;size:255"`
	OnBehalfOfDisplayName *string `gorm:"column:on_behalf_of_display_name;size:255"`

	// Action is the audit action string. A persister validates it against
	// a module's registered pkgcore.Registry.AuditActions enumeration at
	// emission time, not here -- Repository has no registrar of its own to
	// check against, and by the time a record reaches Insert it has
	// already been validated once, upstream.
	Action string `gorm:"column:action;size:255;not null"`

	// ResourceType, ResourceID and ResourceDisplayName are the flattened
	// form of Resource -- what the action was performed on. Use
	// SetResource and Resource to read and write all three together.
	ResourceType        string `gorm:"column:resource_type;size:255;not null"`
	ResourceID          string `gorm:"column:resource_id;size:255;not null"`
	ResourceDisplayName string `gorm:"column:resource_display_name;size:255;not null"`

	// Success and FailureReason are the flattened form of Result. Use
	// SetResult and Result to read and write both together.
	// FailureReason is the empty string on a successful action.
	Success       bool   `gorm:"column:success;not null"`
	FailureReason string `gorm:"column:failure_reason;size:1000;not null"`

	// Changes carries a before/after diff as raw JSON text, or is empty
	// when the audited action recorded has no diff to show
	// (docs/internal/10-compliance-and-audit.md's before/after-comparison
	// field).
	// gorm.io/datatypes.JSON is used for its Value/Scan convenience only --
	// the underlying migration column is a plain, portable TEXT (see the
	// migration files' own doc comments), never a native PostgreSQL JSONB
	// column with operator filtering, because this package only ever reads
	// Changes back whole and the backend coding standard forbids
	// PostgreSQL-only JSONB operator filtering regardless.
	Changes datatypes.JSON `gorm:"column:changes"`

	// TenantID is the owning tenant, or the empty-string sentinel for a
	// platform-level event -- see the type's own doc comment above for why
	// this is a real, indexed column AuditEvent deliberately does not
	// expose through dbkit.TenantScoped. It is the first column of the
	// migration's composite index, matching ListByTenant's own query
	// shape.
	TenantID string `gorm:"column:tenant_id;size:64;not null"`

	// IP, UserAgent and TraceID are request-context metadata captured
	// alongside the action; each is the empty string when the action that
	// produced this record had no such context (a background job, an
	// event subscriber reacting to another module's fact).
	IP        string `gorm:"column:ip;size:64;not null"`
	UserAgent string `gorm:"column:user_agent;size:500;not null"`
	TraceID   string `gorm:"column:trace_id;size:64;not null"`

	// OccurredAt is when the action happened. GORM's autoCreateTime
	// populates it on Create when the caller leaves it at its zero value --
	// matching go/config's row.UpdatedAt and examples/reference-app's
	// Note.CreatedAt -- never SQL's NOW(), per the backend coding
	// standard's dual-dialect rule. It is the second column of the
	// migration's composite index (ListByTenant's own query shape).
	OccurredAt time.Time `gorm:"column:occurred_at;autoCreateTime;not null"`
}

// TableName pins AuditEvent to the audit_events table, so it does not
// depend on GORM's pluralization of the type name.
func (AuditEvent) TableName() string { return auditEventsTable }

// SetActor populates the three Actor* columns from a.
func (e *AuditEvent) SetActor(a pkgcore.Actor) {
	e.ActorType = string(a.Type)
	e.ActorID = a.ID
	e.ActorDisplayName = a.DisplayName
}

// Actor reconstructs the acting pkgcore.Actor from the row's three Actor*
// columns.
func (e AuditEvent) Actor() pkgcore.Actor {
	return pkgcore.Actor{
		Type:        pkgcore.ActorType(e.ActorType),
		ID:          e.ActorID,
		DisplayName: e.ActorDisplayName,
	}
}

// SetOnBehalfOf populates the three OnBehalfOf* columns from a, or clears
// all three back to NULL when a is nil -- see the field doc comments above
// for why the three are never set individually.
func (e *AuditEvent) SetOnBehalfOf(a *pkgcore.Actor) {
	if a == nil {
		e.OnBehalfOfType, e.OnBehalfOfID, e.OnBehalfOfDisplayName = nil, nil, nil
		return
	}
	actorType := string(a.Type)
	id := a.ID
	displayName := a.DisplayName
	e.OnBehalfOfType = &actorType
	e.OnBehalfOfID = &id
	e.OnBehalfOfDisplayName = &displayName
}

// OnBehalfOf reconstructs the impersonation Actor from the row's three
// OnBehalfOf* columns. ok is false when the row carries no impersonation
// (the columns are NULL), in which case the first result is the zero
// pkgcore.Actor.
func (e AuditEvent) OnBehalfOf() (pkgcore.Actor, bool) {
	if e.OnBehalfOfType == nil {
		return pkgcore.Actor{}, false
	}
	return pkgcore.Actor{
		Type:        pkgcore.ActorType(*e.OnBehalfOfType),
		ID:          derefOrEmpty(e.OnBehalfOfID),
		DisplayName: derefOrEmpty(e.OnBehalfOfDisplayName),
	}, true
}

// SetResource populates the three Resource* columns from r.
func (e *AuditEvent) SetResource(r Resource) {
	e.ResourceType = r.Type
	e.ResourceID = r.ID
	e.ResourceDisplayName = r.DisplayName
}

// Resource reconstructs Resource from the row's three Resource* columns.
func (e AuditEvent) Resource() Resource {
	return Resource{Type: e.ResourceType, ID: e.ResourceID, DisplayName: e.ResourceDisplayName}
}

// SetResult populates the Success and FailureReason columns from r.
func (e *AuditEvent) SetResult(r Result) {
	e.Success = r.Success
	e.FailureReason = r.FailureReason
}

// Result reconstructs Result from the row's Success and FailureReason
// columns.
func (e AuditEvent) Result() Result {
	return Result{Success: e.Success, FailureReason: e.FailureReason}
}

// derefOrEmpty returns *s, or "" when s is nil.
func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
