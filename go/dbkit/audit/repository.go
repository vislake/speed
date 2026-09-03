package audit

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Repository is the audit_events table accessor. It is deliberately a
// thin row accessor over a plain *gorm.DB -- never dbkit.Repository[T] --
// because AuditEvent is platform data with a real, non-enforced tenant_id
// column, not tenant-scoped data (see model.go's own doc comment, and
// go/jobs's jobRecord / go/config's row for the same pattern already
// shipped elsewhere in this codebase). Repository needs no
// .Table/.Model/.Raw escape hatch -- Create/Where/Order/Find all suffice --
// and go/dbkit/** is wholesale-allowlisted in
// tools/semgrep_rules/raw-gorm-bypass.yml regardless.
//
// Repository is append-only by construction: it exposes Insert and two
// read methods (Get, ListByTenant) and NO Update or Delete method at all.
// Per docs/internal/10-compliance-and-audit.md, an audit trail an operator
// can edit or remove after the fact is not an audit trail. M1 enforces
// this at the application layer by the simple absence of a mutating
// method -- a property model_test.go's
// TestRepository_HasNoUpdateOrDeleteMethod proves by reflecting over
// Repository's method set, since Go has no way to express "this type
// lacks a method" any other way that fails loudly on a future regression.
// The database-role/trigger backstop against a determined operator with
// raw database access, and the optional hash chain, are both explicitly
// M4 (docs/internal/15-roadmap.md) -- not built here.
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
func (r *Repository) Insert(ctx context.Context, evt *AuditEvent) error {
	if evt.ID == "" {
		evt.ID = uuid.NewString()
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
// minimal read surface B1 ships to prove persistence end to end
// (this package's own tests, and the reference app's proof test) -- the
// full actor/resource/action/time-range/result query API
// docs/internal/10-compliance-and-audit.md describes is M4 (compliance)
// scope, not built here.
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
