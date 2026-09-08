package notification

// This file owns the consent ledger's type-scoped opt-out: the finer shape
// contact.go's whole-contact unsubscribe deliberately never covered. A
// whole-contact unsubscribe (verified_contacts.status = unsubscribed) is
// permanent for the contact as a whole -- one address on one channel stops
// receiving everything, whatever the type (AGENTS.md's "Unsubscribe is
// permanent for the contact as a whole" adjudication). The type-scoped
// opt-out is the ledger's answer to "this type, not that one": a verified
// contact narrows itself out of one notification type while staying
// reachable for every other, and delivery honours the narrowing at send
// time exactly as it honours the whole-contact statuses.
//
// The shape the module's own conventions dictate:
//
//   - One row per (tenant, contact, type) in its own tenant-scoped table,
//     never a column on verified_contacts: a contact may opt out of many
//     types, one at a time, and each narrowing is a separate consent fact
//     with its own audit record. The per-(entity, type) row shape is the
//     identical answer notification_preferences already gives for per-user
//     type state (preference.go); a column holding a set would need a
//     read-modify-write on a shared row, the race the module's row-per-fact
//     tables exist to avoid.
//   - Terminal per (contact, type): an opt-out row is written once and
//     never removed by this API, exactly like the whole-contact
//     unsubscribe's status. There is no re-enable path: the ledger's
//     consent facts are the recipient's durable wishes, and re-consent is
//     a fresh contact cycle (a pruned row's facts die with it, the same
//     lifecycle the whole-contact status has).
//   - Written only for a verified contact: a pending contact has no
//     consent to narrow yet, and a whole-unsubscribed or bounced contact
//     is already covered by a broader terminal fact (the statuses refuse
//     with their own errors, mirroring EnsureDeliverable). Written only
//     for a type the live registrar declares AND whose declaration permits
//     opting out (Unsubscribable) -- the identical taxonomy discipline the
//     preference matrix applies to a user's per-type opt-out
//     (preference_service.go's Set), so a type that must reach its
//     recipients is no more switchable-off per type for a contact than for
//     a user (the whole-contact unsubscribe remains the one exit that
//     predates this shape and is not touched by it).
//   - Re-checked at send time: the delivery job's consent gate
//     (EnsureDeliverableForType) probes the opt-out rows fresh on every
//     attempt, exactly as it re-checks the contact's status -- an opt-out
//     landing between enqueue and attempt refuses the delivery, and an
//     already-settled record is never downgraded by a later refusal.
//
// The write transition is audited (AuditActionContactTypeUnsubscribed)
// with the narrowed type named in the change, so the trail can reconstruct
// a contact's consent state after the tenant prunes its contact rows --
// the reason every consent transition on the ledger is audited.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
)

// tableContactTypeUnsubscribes is the contact_type_unsubscribes table name,
// shared by the model's TableName and by the migration's own header
// comments.
const tableContactTypeUnsubscribes = "contact_type_unsubscribes"

// ContactTypeUnsubscribe is one verified contact's terminal opt-out of one
// notification type, inside one tenant: the row that makes the delivery
// gate refuse "this contact, this type" while every other type still
// reaches the contact.
//
// # Data domain
//
// Tenant data (docs/internal/04-data-and-tenancy.md). An opt-out is
// meaningful only inside the tenant the contact belongs to -- consent is a
// tenant-level record -- so ContactTypeUnsubscribe implements
// dbkit.TenantScoped, is reached only through
// ContactTypeUnsubscribeRepository (which embeds
// dbkit.Repository[ContactTypeUnsubscribe]), and its isolation is proven by
// tenancytest.AssertIsolated.
//
// It embeds dbkit.TenantModel for the tenant_id column and the promoted
// GetTenantID method, exactly as VerifiedContact does and for the same
// reasons (see contact.go's doc comment): ID is an application-generated
// UUID, already globally unique on its own, so a plain tenant_id column
// backed by the composite unique index is enough.
//
// # Row identity
//
// ContactID names the verified_contacts row the opt-out narrows, a plain
// unenforced string column under the codebase's no-foreign-key rule -- the
// identical treatment notification_preferences.recipient_user_id gives a
// reference its table cannot constrain. The row's lifecycle is the
// contact's: a tenant that prunes a contact row (legitimate, recorded in
// the audit trail) prunes its opt-out facts with it, exactly as it prunes
// the contact's whole-contact consent status.
//
// TypeKey names the notification type the contact opted out of, following
// the <module>.<entity>.<action> convention types.go pins. The type's
// declaration is read from the live registrar at write time, never
// denormalized here.
//
// # Semantics
//
// A row exists only for a contact that was verified when the opt-out was
// written and only for a type whose declaration permits opting out
// (Unsubscribable true) -- see this file's doc comment. Rows are never
// updated or deleted by this module: an opt-out is terminal per
// (contact, type), and the delivery gate only ever probes for existence.
type ContactTypeUnsubscribe struct {
	// ID is an application-generated UUID, never a database-generated one:
	// the backend coding standard forbids gen_random_uuid(), which SQLite
	// has no equivalent for.
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantModel promotes the tenant_id column and the GetTenantID method
	// that satisfies dbkit.TenantScoped -- see the doc comment above for
	// why ContactTypeUnsubscribe embeds it instead of declaring TenantID
	// directly.
	dbkit.TenantModel

	// ContactID is the verified_contacts row id this opt-out narrows (see
	// the doc comment above for the no-foreign-key rule).
	ContactID string `gorm:"column:contact_id;size:36;not null"`

	// TypeKey is the notification type the contact opted out of (see the
	// doc comment above).
	TypeKey string `gorm:"column:type_key;size:128;not null"`

	// CreatedAt and UpdatedAt are populated by gorm's autoCreateTime /
	// autoUpdateTime, never written by application code and never NOW() in
	// a migration (SQLite has no NOW()).
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

// TableName names the contact_type_unsubscribes table.
func (ContactTypeUnsubscribe) TableName() string { return tableContactTypeUnsubscribes }

// compile-time check that ContactTypeUnsubscribe satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = ContactTypeUnsubscribe{}

// ContactTypeUnsubscribeRepository is the contact_type_unsubscribes data
// path: the only sanctioned way to read and write type-scoped opt-out
// rows.
//
// It is a named type embedding dbkit.Repository[ContactTypeUnsubscribe],
// exactly as VerifiedContactRepository is for contacts (see contact.go's
// doc comment for the pattern), adding the query shape Repository[T]'s
// minimal surface cannot express: the per-(contact, type) existence probe
// both the write path's dedupe and the delivery gate's refusal run. The
// probe follows ByChannelAndAddressIndex's shape -- built on the same
// *gorm.DB the embedded Repository was built on, against a TenantScoped
// destination, inside dbkit.WithTenantSession, so the isolation plugin and
// the PostgreSQL RLS session variable apply exactly as they do to every
// promoted method.
type ContactTypeUnsubscribeRepository struct {
	*dbkit.Repository[ContactTypeUnsubscribe]

	// db is the same connection the embedded Repository was built on, kept
	// so the probe can be composed on it.
	db *gorm.DB
}

// NewContactTypeUnsubscribeRepository returns a
// ContactTypeUnsubscribeRepository backed by db. db is expected to come
// from dbkit.Open, already migrated with this module's Migrations() -- see
// dbkit.Repository's own doc comment for why Open specifically.
func NewContactTypeUnsubscribeRepository(db *gorm.DB) *ContactTypeUnsubscribeRepository {
	return &ContactTypeUnsubscribeRepository{
		Repository: dbkit.NewRepository[ContactTypeUnsubscribe](db),
		db:         db,
	}
}

// ByContactAndType returns the tenant's type-scoped opt-out for one
// contact and type, or (nil, nil) when none exists.
//
// The nil-and-nil return is the probe's whole contract: absence means the
// contact is still reachable for the type (the delivery gate's pass), and
// the write path's dedupe. The tenant comes from ctx; a row of another
// tenant -- even for the same contact id and type -- is indistinguishable
// from a row that does not exist.
func (r *ContactTypeUnsubscribeRepository) ByContactAndType(ctx context.Context, contactID, typeKey string) (*ContactTypeUnsubscribe, error) {
	var row ContactTypeUnsubscribe
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where("contact_id = ? AND type_key = ?", contactID, typeKey).First(&row).Error
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &row, nil
}

// UnsubscribeTypeInput names the contact and the type being narrowed out.
type UnsubscribeTypeInput struct {
	// ContactID names the verified contact whose consent is being narrowed.
	ContactID string

	// TypeKey names the notification type the contact opts out of. It must
	// be declared on the live registrar (ErrTypeNotFound otherwise) and its
	// declaration must permit opting out -- Unsubscribable true
	// (ErrContactTypeOptoutNotAllowed otherwise).
	TypeKey string
}

// UnsubscribeType records that one verified contact opts out of one
// notification type -- terminal per (contact, type), the finer shape of
// the whole-contact Unsubscribe (contact.go). See this file's doc comment
// for the design and for why the write is refused on every status but
// verified: a pending contact has no consent to narrow
// (ErrContactNotVerified), a whole-unsubscribed or bounced contact is
// already covered by the broader terminal fact (its own status error), a
// contact id no row of the tenant holds answers ErrContactNotFound, a type
// nobody declared answers ErrTypeNotFound, and a declared type whose
// declaration forbids opting out answers ErrContactTypeOptoutNotAllowed --
// the identical taxonomy refusals the preference matrix applies to a
// user's per-type opt-out (preference_service.go's Set).
//
// The transition is idempotent: opting out of a type the contact already
// opted out of succeeds without a second row and without a second audit
// event -- the idempotent repeat is not a state change, the same rule
// whole-contact Unsubscribe already follows. The actual transition commits
// first, then emits its audit action
// (AuditActionContactTypeUnsubscribed) with the narrowed type named in the
// change; an emit failure returns an internal error exactly as every other
// consent transition's does (the transition happened, its outliving record
// did not -- see contact.go's emit doc).
func (s *ContactService) UnsubscribeType(ctx context.Context, in UnsubscribeTypeInput) error {
	contact, err := s.repo.FindByID(ctx, in.ContactID)
	if err != nil {
		if isRecordNotFound(err) {
			return ErrContactNotFound
		}
		return errInternal(err)
	}
	switch contact.Status {
	case ContactStatusVerified:
		// The only status a type-scoped opt-out may narrow; fall through.
	case ContactStatusPending:
		return ErrContactNotVerified
	case ContactStatusUnsubscribed:
		return ErrContactUnsubscribed
	case ContactStatusBounced:
		return ErrContactBounced
	default:
		return errInternal(fmt.Errorf("notification: unknown contact status %q", contact.Status))
	}

	typ, err := s.lookupType(in.TypeKey)
	if err != nil {
		return err
	}
	if !typ.Unsubscribable {
		return ErrContactTypeOptoutNotAllowed.WithParam("type_key", in.TypeKey)
	}

	// Upsert-free terminal write with a bounded duplicate-key retry: the
	// probe-then-create pair below cannot close the race between two
	// first-writes of the same (contact, type) on its own, so the unique
	// index is the backstop, and a lost race -- at most once, on the very
	// first attempt's create -- re-reads and answers the idempotent
	// success the winner's transition already earned. Exactly the
	// preference matrix's own upsert loop shape (preference_service.go's
	// Set), without the update half: a terminal opt-out row is never
	// rewritten.
	created := false
	for attempt := 0; attempt < 2; attempt++ {
		existing, err := s.typeUnsubs.ByContactAndType(ctx, contact.ID, in.TypeKey)
		if err != nil {
			return errInternal(err)
		}
		if existing != nil {
			// The repeat is not a state change; no audit event.
			return nil
		}
		row := &ContactTypeUnsubscribe{
			ID:        uuid.NewString(),
			ContactID: contact.ID,
			TypeKey:   in.TypeKey,
		}
		if err := s.typeUnsubs.Create(ctx, row); err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) && attempt == 0 {
				continue // another writer recorded the opt-out between our read and our create
			}
			return errInternal(err)
		}
		created = true
		break
	}
	if !created {
		return errInternal(errors.New("notification: type-scoped opt-out write did not converge after a duplicate-key retry"))
	}

	// The transition committed; record it with the narrowed type named --
	// an identifier, never PII, so it is safe in the append-only trail.
	if err := s.emit(ctx, AuditActionContactTypeUnsubscribed, contact, audit.Result{Success: true},
		&audit.Diff{After: map[string]any{"type_key": in.TypeKey}}); err != nil {
		return err
	}
	return nil
}

// EnsureDeliverableForType is the type-aware half of the consent gate the
// delivery job re-checks before every send to an external contact: it
// answers EnsureDeliverable's status gate, and for a verified contact
// additionally refuses when the contact has a type-scoped opt-out row for
// typeKey (ErrContactTypeUnsubscribed, carrying the contact's channel in
// its "channel" parameter like the two terminal status refusals -- the
// skip record the job settles for it is per channel).
//
// The delivery job calls this with every message it is about to transport,
// in place of the type-agnostic EnsureDeliverable: an opt-out landing
// between enqueue and delivery must refuse the delivery exactly as a
// whole-contact status change does (AGENTS.md's "Every consent and address
// decision is re-checked at send time"). The refusal is terminal -- no
// retry changes the answer -- and the job records it as a skipped send
// under the type-scoped skip reason, never as a retryable failure.
//
// EnsureDeliverable (the type-agnostic gate, unchanged) is this method's
// parent shape: both answer from the contact row's status first, and a
// type-scoped opt-out only ever narrows a verified contact's delivery,
// never answers for the contact's own status.
func (s *ContactService) EnsureDeliverableForType(ctx context.Context, contactID, typeKey string) (*VerifiedContact, error) {
	return s.ensureDeliverable(ctx, contactID, typeKey)
}
