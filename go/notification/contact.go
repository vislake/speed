package notification

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/i18n"
	"github.com/vislake/speed/go/ratelimit"
)

// tableVerifiedContacts is the verified_contacts table name, shared by the
// model's TableName and by the migration's own header comments.
const tableVerifiedContacts = "verified_contacts"

// Contact statuses, the closed vocabulary of the verified_contacts.status
// column. The transitions between them are the consent state machine
// ContactService enforces:
//
//	pending --verify--> verified --unsubscribe--> unsubscribed
//	pending --resend--> pending           --hard failure--> bounced
//	pending --attest--> verified (business-attested, no code)
//
// unsubscribed and bounced are terminal per contact: the R8 adjudication
// makes an unsubscribe permanent for the contact as a whole (type-scoped
// opt-out is a deferred later-round shape and verified_contacts has no
// type_key column), and a bounced contact has proven its address cannot
// receive messages at all. Delivery refuses both before any transport is
// touched (EnsureDeliverable).
const (
	ContactStatusPending      = "pending"
	ContactStatusVerified     = "verified"
	ContactStatusUnsubscribed = "unsubscribed"
	ContactStatusBounced      = "bounced"
)

// Contact consent-by sources, the closed vocabulary of the
// verified_contacts.consent_by column.
const (
	ContactConsentByDoubleOptIn      = "double_opt_in"
	ContactConsentByBusinessAttested = "business_attested"
)

// ContactAddressSerializerName is the gorm serializer name registered by the
// host (through dbkit.RegisterEncryptedSerializer) for the encrypted address
// column of VerifiedContact. The name is exported because the serializer
// registry is process-global and the host -- not this module -- must perform
// the registration: the module's own unit tests register it as a test helper
// (see contact_test.go), and the reference app will register it with a real
// key at bootstrap. It follows org's precedent: an exported
// <module>_<entity>_enc name the host binds once, before any contact row is
// read or written.
//
// The blind-index key that makes the encrypted address queryable lives on
// the indexers the host injects through WithContactEmailIndexer /
// WithContactPhoneIndexer, and must never be the encryption key (the F8
// design rule: separate index key and cipher key, because a key compromise
// must not silently hand over both confidentiality and queryability).
const ContactAddressSerializerName = "notification_address_enc"

// VerifiedContact is one external recipient's consent-gated address inside
// one tenant: a patient's phone number or email address that may receive
// messages only after it has been verified, or has been attested to by a
// business process.
//
// # Data domain
//
// Tenant data (docs/internal/04-data-and-tenancy.md). A contact is
// meaningful only inside the tenant it was created in -- consent is a
// tenant-level record, not a person-level one -- so VerifiedContact
// implements dbkit.TenantScoped, is reached only through
// VerifiedContactRepository (which embeds dbkit.Repository[VerifiedContact]),
// and its isolation is proven by tenancytest.AssertIsolated.
//
// It embeds dbkit.TenantModel for the tenant_id column and the promoted
// GetTenantID method, exactly as InboxMessage and NotificationPreference do
// and for the same reasons (see model.go's doc comment): ID is an
// application-generated UUID, already globally unique on its own, so a
// plain tenant_id column backed by the composite listing index is enough.
// Do NOT redeclare a same-named TenantID field here -- dbkit's
// tenant_scope.go documents how that silently breaks GetTenantID.
//
// # Address and privacy
//
// Address is the recipient's phone number (E.164 form) or email address,
// encrypted at rest under the host-injected dbkit.NewCipher key through the
// notification_address_enc serializer; on read it comes back decrypted.
// It is never queryable and never appears in a WHERE clause. AddressIndex
// is the HMAC-SHA256 blind index of the canonical address form (see
// dbkit.NewBlindIndexer), the only thing lookups, the dedupe unique index
// and the rate limiter ever touch -- the rate-limiter keys name the index
// hex, never the plaintext address (the F8 design rule, mirrored from
// go/authn and go/org). The encryption key and the index keys are separate
// and must never be the same bytes.
//
// Channel is the closed vocabulary of types.go -- ChannelEmail or
// ChannelSMS. A contact is one address on one channel; the in-app channel
// of an inbox recipient is a different concept (in-app recipients are
// users, never contacts, so a contact can never be in_app).
//
// # Status and consent model
//
// Status is ContactStatusPending, ContactStatusVerified,
// ContactStatusUnsubscribed or ContactStatusBounced; ConsentBy is
// double_opt_in or business_attested. A double_opt_in contact is created in
// status pending carrying a hashed verification code and its expiry, and is
// moved to verified by ContactService.VerifyCode when the recipient proves
// possession of the address. A business_attested contact is created already
// verified, with ConsentRef recording which business record the attestation
// came from, and an audit event records the transition (see
// ContactService.CreateContact). Unsubscribed and bounced are terminal, as
// the doc comment on the status constants explains.
//
// ConsentAt records when the contact's consent was obtained (the
// verification moment for double_opt_in, creation for business_attested);
// VerifiedAt records when it entered status verified -- for
// business_attested contacts the two coincide. CodeHash and CodeExpiresAt
// are meaningful only in status pending (see contact_code.go); outside
// pending they remain as inert dead data and the status gate -- never the
// columns -- is what makes a consumed code unusable.
type VerifiedContact struct {
	// ID is an application-generated UUID, never a database-generated one:
	// the backend coding standard forbids gen_random_uuid(), which SQLite
	// has no equivalent for.
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantModel promotes the tenant_id column and the GetTenantID method
	// that satisfies dbkit.TenantScoped -- see the doc comment above for
	// why VerifiedContact embeds it instead of declaring TenantID directly.
	dbkit.TenantModel

	// Channel is ChannelEmail or ChannelSMS (the types.go vocabulary). The
	// channel is part of the contact's identity: the same address verified
	// on two channels is two rows, deduplicated per (tenant, channel,
	// address_index) by the migration's unique index.
	Channel string `gorm:"column:channel;size:16;not null"`

	// Address is the contact's phone number or email address, encrypted at
	// rest; see the doc comment above for the privacy model. The bytes are
	// produced and consumed by the host-registered serializer only, and on
	// read the field holds the decrypted plaintext.
	Address string `gorm:"column:address;serializer:notification_address_enc;not null"`

	// AddressIndex is the blind index of the canonical address form, hex
	// encoded; see the doc comment above.
	AddressIndex string `gorm:"column:address_index;size:64;not null"`

	// Status is one of the ContactStatus constants; see the doc comment
	// above for the state machine.
	Status string `gorm:"column:status;size:16;not null"`

	// ConsentBy is ContactConsentByDoubleOptIn or
	// ContactConsentByBusinessAttested.
	ConsentBy string `gorm:"column:consent_by;size:32;not null"`

	// ConsentRef is empty for a double_opt_in contact, and for a
	// business_attested one the host-supplied reference of the business
	// record that attested the address -- the audit event for the
	// attestation carries it, so an investigator can trace the consent to
	// the intake document that produced it.
	ConsentRef string `gorm:"column:consent_ref;size:128;not null"`

	// ConsentAt is when consent was obtained, nullable in the schema only
	// because a pending contact has not consented yet; VerifiedAt is when
	// the contact entered status verified. Both are written by application
	// code (they are business facts, not row bookkeeping, so
	// autoCreateTime/autoUpdateTime do not own them).
	ConsentAt  *time.Time `gorm:"column:consent_at"`
	VerifiedAt *time.Time `gorm:"column:verified_at"`

	// CodeHash is the SHA-256 of the pending verification code (see
	// contact_code.go), CodeExpiresAt the moment the code stops being
	// usable. Meaningful only in status pending; see the doc comment above
	// for the dead-data rule.
	CodeHash      string     `gorm:"column:code_hash;size:64;not null"`
	CodeExpiresAt *time.Time `gorm:"column:code_expires_at"`

	// CreatedAt and UpdatedAt are populated by gorm's autoCreateTime /
	// autoUpdateTime, never written by application code and never NOW() in
	// a migration (SQLite has no NOW()).
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

// TableName names the verified_contacts table.
func (VerifiedContact) TableName() string { return tableVerifiedContacts }

// compile-time check that VerifiedContact satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = VerifiedContact{}

// VerifiedContactRepository is the verified_contacts data path: the only
// sanctioned way to read and write contact rows.
//
// It is a named type embedding dbkit.Repository[VerifiedContact], exactly
// as PreferenceRepository is for preferences (see preference_repository.go's
// doc comment for the pattern), adding the query shape Repository[T]'s
// minimal surface cannot express: a lookup by (channel, address index),
// the dedupe probe every create runs. The conditional status-transition
// methods themselves (consumePendingCode, stampCode, markUnsubscribed,
// markBounced) live on the service, which composes them on the same
// *gorm.DB through dbkit.WithTenantSession -- they are state-machine
// transitions, not repository reads, and they sit next to the service logic
// that decides them.
//
// ByChannelAndAddressIndex is written the way go/dbkit/AGENTS.md's "Known
// limitations" prescribes: built on the same *gorm.DB the embedded
// Repository was built on, against a TenantScoped destination, so the GORM
// isolation plugin still injects WHERE tenant_id = ? -- and run inside
// dbkit.WithTenantSession, so the PostgreSQL RLS session variable is set for
// it exactly as it is for every promoted method. Nothing in this file
// hand-writes a tenant_id filter, and nothing reaches for db.Table,
// db.Model or db.Raw.
type VerifiedContactRepository struct {
	*dbkit.Repository[VerifiedContact]

	// db is the same connection the embedded Repository was built on, kept
	// so the service's conditional UPDATEs (see contact.go's
	// consumePendingCode and siblings) can be composed on it. Every use
	// routes through WithTenantSession and a TenantScoped destination.
	db *gorm.DB
}

// NewVerifiedContactRepository returns a VerifiedContactRepository backed by
// db. db is expected to come from dbkit.Open, already migrated with this
// module's Migrations() -- see dbkit.Repository's own doc comment for why
// Open specifically.
func NewVerifiedContactRepository(db *gorm.DB) *VerifiedContactRepository {
	return &VerifiedContactRepository{
		Repository: dbkit.NewRepository[VerifiedContact](db),
		db:         db,
	}
}

// ByChannelAndAddressIndex returns the tenant's contact for one channel and
// address index, or (nil, nil) when no row exists.
//
// The nil-and-nil return is this repository's most important contract: an
// absent row is what lets CreateContact start a fresh consent flow, while
// every status an existing row can hold has its own meaning for the caller
// (see ContactService.CreateContact). The tenant comes from ctx; a row of
// another tenant -- even for the same channel and address -- is
// indistinguishable from a row that does not exist.
func (r *VerifiedContactRepository) ByChannelAndAddressIndex(ctx context.Context, channel, addressIndex string) (*VerifiedContact, error) {
	var contact VerifiedContact
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where("channel = ? AND address_index = ?", channel, addressIndex).First(&contact).Error
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &contact, nil
}

// ListForTenant returns the tenant's whole contact roster in the tenant of
// ctx, newest first. The ordering is created_at DESC with id DESC as the
// tiebreak, so two contacts created in the same instant still list
// deterministically -- the roster page the contacts API serves (the spec's
// list-contacts operation) is not paged, so the order is the entire
// contract, and it is the tenant's roster, not one user's: a contact
// belongs to the tenant that verified it, so any identified caller of the
// tenant sees the roster the tenant's staff manages.
func (r *VerifiedContactRepository) ListForTenant(ctx context.Context) ([]VerifiedContact, error) {
	var contacts []VerifiedContact
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Order("created_at DESC, id DESC").Find(&contacts).Error
	})
	return contacts, err
}

// ContactService is the consent-gated external contact ledger: creation
// (double opt-in and business-attested), verification, resend,
// unsubscribe, the bounced marking delivery calls after a hard failure,
// and the deliverability gate delivery re-checks before every send. It
// owns the state machine of VerifiedContact and the rate limits that
// protect every message-sending path.
//
// The service is built by NewContactService and configured by the module
// during Register (see Module.Register): the host seams arrive through
// NewModule's With* options and are validated at Register time, so a
// service that is missing its SMSSender, its mail From, or an address
// indexer can never be reached by a caller. The audit registrar is passed
// separately at attach time -- pkgcore.Registry.AuditActions is an
// interface-typed field, not a method, so it cannot ride in the host
// interface (see module.go).
type ContactService struct {
	repo         *VerifiedContactRepository
	sms          SMSSender
	mailFrom     string
	emailIndexer *dbkit.BlindIndexer
	phoneIndexer *dbkit.BlindIndexer
	host         contactHost
	audit        pkgcore.AuditActionRegistrar
	now          func() time.Time
}

// contactHost is the structural subset of pkgcore.Registry this service
// reads at call time: the event bus the audit trail publishes on, the KV
// store the rate limiter is backed by, the resolved mailer, and the merged
// message catalog verification-code messages render from. It is declared
// here as an interface so ContactService never names pkgcore.Registry (and
// never reaches for fields it does not need); *pkgcore.Registry satisfies it
// structurally (see module.go's attachHost), and a hand-built host in a test
// satisfies it too.
//
// Two of the four accessors are nil on a registry the host built directly
// with the three-argument NewRegistry: that form carries no catalog (the
// catalog is installed only after Kernel.Bootstrap walks every module's
// Locales()), while the mailer and the KV store are always present on such
// a registry. Code paths treat a nil Locales or a nil KV store as internal
// configuration errors -- never as a license to skip rendering or to allow
// on error.
type contactHost interface {
	// EventBus returns the bus audit records publish on.
	EventBus() pkgcore.EventBus

	// KVStore returns the store the rate limiter counts against. A nil
	// store fails any rate-limited call closed (ErrInternal): the
	// alternative, allowing a code send or verify without a budget,
	// defeats the control entirely.
	KVStore() pkgcore.KVStore

	// Mailer returns the resolved outbound-mail transport, or nil if the
	// registry was built without one (NewRegistry's three-argument form
	// carries none, though in practice a mailer is always passed). Code
	// that sends email must treat nil as an internal configuration error.
	Mailer() pkgcore.Mailer

	// Locales returns the merged message catalog, or nil if the registry
	// was built directly without going through Kernel.Bootstrap.
	// Verification-code messages are rendered from it at send time --
	// never captured at registration time.
	Locales() *i18n.Catalog
}

// NewContactService returns a ContactService over db, with every seam still
// nil. The module fills the seams during Register -- options first (with
// their required-seam validation), then the host reference and the audit
// registrar through attachHost -- so a service is unusable until its module
// has been registered, and a test that wants a seam-ful service goes
// through that same path (see module_test.go's newTestModule) or assigns
// the fields directly (same package).
func NewContactService(db *gorm.DB) *ContactService {
	return &ContactService{
		repo: NewVerifiedContactRepository(db),
		now:  time.Now,
	}
}

// attachHost binds the service's host reference and audit registrar.
// pkgcore.Registry.AuditActions is an interface-typed field, which cannot
// be declared in an interface; the module therefore passes it alongside the
// host (see Module.Register).
func (s *ContactService) attachHost(host contactHost, auditActions pkgcore.AuditActionRegistrar) {
	s.host = host
	s.audit = auditActions
}

// Audit actions declared by this module on the registry's audit-action
// registrar during Register. The attestation, verification and unsubscribe
// transitions each emit one event through dbkit/audit's declarative Emit
// (see the emit calls below), so every consent-state change is
// reconstructible from the audit trail even though the contact rows
// themselves are tenant data a tenant admin may legitimately prune.
const (
	AuditActionContactAttested     = "notification.contact.attested"
	AuditActionContactVerified     = "notification.contact.verified"
	AuditActionContactUnsubscribed = "notification.contact.unsubscribed"
)

// contactAuditActionDecls is the module's audit-action contribution,
// registered on reg.AuditActions during Register. The slice exists as a
// named variable (rather than inline at the Add call) so the module test can
// pin the exact set.
var contactAuditActionDecls = []string{
	AuditActionContactAttested,
	AuditActionContactVerified,
	AuditActionContactUnsubscribed,
}

// auditResourceContact is the audit.Resource under which every contact
// transition is recorded: the Resource ID is the contact's row ID, and the
// DisplayName the channel plus the address's blind index -- the plaintext
// address must never reach the audit trail (it is PII, and records that
// outlive the row must not carry it).
func auditResourceContact(c *VerifiedContact) audit.Resource {
	return audit.Resource{
		Type:        "contact",
		ID:          c.ID,
		DisplayName: c.Channel + ":" + c.AddressIndex,
	}
}

// List returns the tenant's whole verified-contact roster in the tenant of
// ctx, newest first -- the service half of the contacts API's list
// operation (openapi.yaml). The tenant comes from ctx, never from the
// caller, and the answer is the tenant's roster, not one user's (a contact
// is consent the tenant holds, so any identified caller of the tenant sees
// it). No pagination and no filters exist: the roster is the tenant's
// full external-recipient ledger.
//
// The returned rows still carry their decrypted Address -- the service
// works on the model, whose serializer decrypts the column on read -- so a
// caller rendering an API response must strip it (the handler's
// toContactResponse drops it; the address is PII the API never echoes).
func (s *ContactService) List(ctx context.Context) ([]VerifiedContact, error) {
	contacts, err := s.repo.ListForTenant(ctx)
	if err != nil {
		return nil, errInternal(err)
	}
	return contacts, nil
}

// ContactCreateInput is the host's request to register a contact.
type ContactCreateInput struct {
	// Channel is ChannelEmail or ChannelSMS.
	Channel string

	// Address is the raw address the host holds: an E.164 phone number
	// ("+8613800138000") or an email address. It is normalized and
	// blind-indexed here; the encrypted column stores the normalized form.
	Address string

	// ConsentRef, when non-empty, attests the address by a business
	// process instead of a double opt-in code: the contact is created
	// already verified, and ConsentRef records which business record the
	// attestation came from (an audit event carries it). When empty, the
	// contact is created pending and a verification code is sent to the
	// address synchronously -- the one sanctioned exception to
	// event-driven messaging, rate limited on two dimensions (see
	// contactRateLimits).
	ConsentRef string
}

// CreateContact registers one external contact and returns it.
//
// The address is normalized (E.164 for phones, lowercased for email) and
// blind-indexed; a duplicate (same tenant, channel and address index)
// resolves by returning the existing row unchanged, whatever status it
// holds -- nothing is ever re-sent to an address that already has a consent
// record, an unsubscribed address is not silently re-registered (its
// unsubscribe is permanent; the R8 per-contact rule), and a bounced address
// is not given a fresh flow until a later round ships re-proving
// (AGENTS.md records the deferral). A host that attests an address which
// already exists as a pending double-opt-in row gets that pending row back,
// unchanged: an attestation never overwrites an in-flight verification.
//
// For a double_opt_in create, the rate limits are checked BEFORE the
// pending row is created -- a rate-limited create fails with
// ErrContactRateLimited and creates nothing, so the ledger cannot be
// flooded by a patient hammering creates at registration time. The code
// send happens after the row commit; if the send fails the row is revoked
// (deleted) and ErrContactCodeDeliveryFailed is returned -- a pending
// contact whose code the patient could never receive must not exist.
//
// For a business_attested create the contact is written already verified
// and an audit event records the attestation with its ConsentRef.
func (s *ContactService) CreateContact(ctx context.Context, in ContactCreateInput) (*VerifiedContact, error) {
	channel, err := validateContactChannel(in.Channel)
	if err != nil {
		return nil, err
	}
	normalized, err := normalizeContactAddress(channel, in.Address)
	if err != nil {
		return nil, err
	}
	index, err := s.indexer(channel).Index(normalized)
	if err != nil {
		return nil, errInternal(err)
	}

	// Dedupe against an existing row before anything is written or sent.
	existing, err := s.repo.ByChannelAndAddressIndex(ctx, channel, index)
	if err != nil {
		return nil, errInternal(err)
	}
	if existing != nil {
		return existing, nil
	}

	now := s.now()
	contact := &VerifiedContact{
		ID:           uuid.NewString(),
		Channel:      channel,
		Address:      normalized,
		AddressIndex: index,
		Status:       ContactStatusPending,
		ConsentBy:    ContactConsentByDoubleOptIn,
	}

	if in.ConsentRef != "" {
		contact.Status = ContactStatusVerified
		contact.ConsentBy = ContactConsentByBusinessAttested
		contact.ConsentRef = in.ConsentRef
		contact.ConsentAt = &now
		contact.VerifiedAt = &now
		if err := s.repo.Create(ctx, contact); err != nil {
			return nil, errInternal(err)
		}
		if err := s.emit(ctx, AuditActionContactAttested, contact, audit.Result{Success: true}); err != nil {
			return nil, err
		}
		return contact, nil
	}

	// Double opt-in: rate limit first, then create the pending row.
	if err := s.checkCodeSendLimit(ctx, index); err != nil {
		return nil, err
	}
	if err := s.repo.Create(ctx, contact); err != nil {
		return nil, errInternal(err)
	}
	if err := s.sendCode(ctx, contact); err != nil {
		// The code never reached the patient; a pending row without a
		// deliverable code must not survive.
		if derr := s.repo.Delete(ctx, contact.ID); derr != nil {
			return nil, errInternal(err) // the row stays; surface the send failure
		}
		return nil, err
	}
	return contact, nil
}

// VerifyCodeInput is the host's request to verify one pending contact.
type VerifyCodeInput struct {
	// ContactID names the contact row to verify.
	ContactID string

	// Code is the code the recipient typed in. A wrong code, an expired
	// code and a replayed code are deliberately indistinguishable (all
	// fail with ErrContactCodeInvalid): telling the caller which one it
	// was hands an attacker a free oracle on whether a code is still live.
	Code string
}

// VerifyCode verifies one pending contact against the code the recipient
// typed in.
//
// The status is gated first -- only a pending contact can be verified, and
// the gate doubles as the "already handled" answer: an unsubscribed or
// bounced contact answers with its own error, a verified one answers
// ErrContactCodeInvalid (the same answer a wrong code gets, so replaying
// the whole flow teaches an attacker nothing). Non-pending callers never
// burn rate-limit budget.
//
// A pending contact then pays the verify rate limits -- verification is the
// brute-force surface of a 6-digit code, so every attempt counts against a
// per-address and per-tenant budget (see contactRateLimits) and a limit
// failure fails closed with ErrContactRateLimited -- before the code is
// checked: expiry first, then the hash, both answering identically. The
// code is then consumed compare-and-swap style: an UPDATE that only moves a
// pending row whose stored hash still equals the typed code's hash decides
// the transition in the database. A row that fails the CAS is re-read to
// tell the caller where it actually landed (verified: a concurrent consume
// won, answered as a replay; unsubscribed or bounced: that status's error;
// still pending: a concurrent resend stamped a fresh code, answered as a
// wrong code).
//
// The one-time property needs no extra columns: the same UPDATE that moves
// the row out of pending writes ConsentAt and VerifiedAt, and the status
// gate is the single-use mechanism (see contact_code.go).
func (s *ContactService) VerifyCode(ctx context.Context, in VerifyCodeInput) (*VerifiedContact, error) {
	contact, err := s.repo.FindByID(ctx, in.ContactID)
	if err != nil {
		if isRecordNotFound(err) {
			return nil, ErrContactNotFound
		}
		return nil, errInternal(err)
	}

	switch contact.Status {
	case ContactStatusUnsubscribed:
		return nil, ErrContactUnsubscribed
	case ContactStatusBounced:
		return nil, ErrContactBounced
	case ContactStatusVerified:
		// A replay of the whole flow; same answer as a wrong code.
		return nil, ErrContactCodeInvalid
	case ContactStatusPending:
		// The only status that can be verified; fall through.
	default:
		return nil, errInternal(fmt.Errorf("notification: unknown contact status %q", contact.Status))
	}

	if err = s.checkCodeVerifyLimit(ctx, contact.AddressIndex); err != nil {
		return nil, err
	}

	// The typed code is checked for shape, then expiry, then hash -- all
	// three failures answering identically (the "never say which part was
	// wrong" rule), before the database is touched.
	if in.Code == "" {
		return nil, ErrContactCodeInvalid
	}
	if contact.CodeExpiresAt == nil || contact.CodeExpiresAt.Before(s.now()) {
		return nil, ErrContactCodeInvalid
	}
	if !contactCodeHashesEqual(contact.CodeHash, hashContactCode(in.Code)) {
		return nil, ErrContactCodeInvalid
	}

	moved, at, err := s.consumePendingCode(ctx, contact.ID, contact.CodeHash)
	if err != nil {
		return nil, errInternal(err)
	}
	if !moved {
		// The CAS lost: something moved the row between our read and our
		// update. Re-read to tell the caller where it landed.
		fresh, err := s.repo.FindByID(ctx, in.ContactID)
		if err != nil {
			return nil, errInternal(err)
		}
		switch fresh.Status {
		case ContactStatusVerified:
			return nil, ErrContactCodeInvalid // a concurrent consume won
		case ContactStatusUnsubscribed:
			return nil, ErrContactUnsubscribed
		case ContactStatusBounced:
			return nil, ErrContactBounced
		default:
			// Still pending: a concurrent resend stamped a fresh code.
			return nil, ErrContactCodeInvalid
		}
	}

	// The CAS moved the row, and the in-memory contact predates it. Sync
	// the three fields the transition changed onto the returned snapshot --
	// stamped with the same timestamp the UPDATE wrote -- so the caller
	// never receives a verified row wearing its pending face (Unsubscribe
	// syncs contact.Status onto its snapshot the same way after
	// markUnsubscribed).
	contact.Status = ContactStatusVerified
	contact.ConsentAt = &at
	contact.VerifiedAt = &at

	if err := s.emit(ctx, AuditActionContactVerified, contact, audit.Result{Success: true}); err != nil {
		return nil, err
	}
	return contact, nil
}

// consumePendingCode is the compare-and-swap that moves one pending contact
// to verified. It succeeds only for a row that is still pending and whose
// stored hash still equals wantHash; the UPDATE writes the verified status
// and the two consent timestamps in the same statement, so the transition
// is atomic and the code single-use. The first returned value is whether a
// row actually moved (RowsAffected == 1); the second is the timestamp the
// UPDATE wrote when it did -- the very value VerifyCode syncs onto its
// in-memory snapshot, so the snapshot and the row can never disagree about
// when consent was given.
//
// The WHERE clause names the identity, the current status and the code
// hash; the SET names only non-zero target values, which is all this
// transition needs -- gorm's struct-based Updates writes no zero-valued
// fields, a deliberate property here: every column this transition must
// change is non-zero, and the code columns are left as the inert dead data
// the model's doc comment describes, their reuse blocked by the status
// gate, never by clearing them.
func (s *ContactService) consumePendingCode(ctx context.Context, id, wantHash string) (moved bool, at time.Time, err error) {
	err = dbkit.WithTenantSession(ctx, s.repo.db, func(tx *gorm.DB) error {
		now := s.now()
		res := tx.Where("id = ? AND status = ? AND code_hash = ?", id, ContactStatusPending, wantHash).
			Updates(&VerifiedContact{Status: ContactStatusVerified, ConsentAt: &now, VerifiedAt: &now})
		if res.Error != nil {
			return res.Error
		}
		moved = res.RowsAffected == 1
		at = now
		return nil
	})
	return moved, at, err
}

// ResendCodeInput names the contact whose code should be re-issued.
type ResendCodeInput struct {
	ContactID string
}

// ResendCode issues a fresh code to a still-pending contact.
//
// Only pending contacts can be resent to: an unsubscribed or bounced
// contact answers with its own error, a verified one is refused with
// ErrContactCodeInvalid (there is nothing to verify any more -- and saying
// "already verified" would confirm to a stranger holding only the id that
// the address is live and verified, a disclosure the code never makes).
// Non-pending callers never burn rate-limit budget.
//
// The rate limits are checked first (same budgets as the create-time send;
// see contactRateLimits), then a fresh code is stamped on the row and
// sent. The stamp and the send are deliberately not atomic: if the send
// fails, the new hash stays on the row and the caller gets
// ErrContactCodeDeliveryFailed -- the patient simply has a code they were
// never told, which is harmless because the previous code has already
// expired or was never delivered, and the next resend replaces it again.
// Retrying the resend is the caller's path forward, and it is rate limited
// like every send.
func (s *ContactService) ResendCode(ctx context.Context, in ResendCodeInput) error {
	contact, err := s.repo.FindByID(ctx, in.ContactID)
	if err != nil {
		if isRecordNotFound(err) {
			return ErrContactNotFound
		}
		return errInternal(err)
	}
	switch contact.Status {
	case ContactStatusUnsubscribed:
		return ErrContactUnsubscribed
	case ContactStatusBounced:
		return ErrContactBounced
	case ContactStatusVerified:
		return ErrContactCodeInvalid
	case ContactStatusPending:
		// fall through
	default:
		return errInternal(fmt.Errorf("notification: unknown contact status %q", contact.Status))
	}

	if err := s.checkCodeSendLimit(ctx, contact.AddressIndex); err != nil {
		return err
	}
	if err := s.sendCode(ctx, contact); err != nil {
		return err
	}
	return nil
}

// sendCode stamps a fresh code on contact and sends it over the contact's
// channel, synchronously. The stamp happens first; the code is rendered at
// send time in the platform default locale (see renderContactCode -- the
// recipient's own locale is a later-round shape, as the contact row has no
// locale column and AGENTS.md records the deferral); a send that fails
// leaves the fresh hash on the row as the doc comment of ResendCode
// explains.
func (s *ContactService) sendCode(ctx context.Context, contact *VerifiedContact) error {
	code, err := generateContactCode()
	if err != nil {
		return errInternal(err)
	}
	expiresAt := s.now().Add(contactCodeTTL)
	if err = s.stampCode(ctx, contact.ID, hashContactCode(code), &expiresAt); err != nil {
		return errInternal(err)
	}

	subject, body, err := renderContactCode(s.host.Locales(), contact.Channel, code)
	if err != nil {
		return err
	}
	switch contact.Channel {
	case ChannelSMS:
		if err := s.sms.Send(ctx, SMS{To: contact.Address, Text: body}); err != nil {
			return ErrContactCodeDeliveryFailed.WithCause(err)
		}
	case ChannelEmail:
		mailer := s.host.Mailer()
		if mailer == nil {
			return errInternal(fmt.Errorf("notification: no mailer on registry for an email code send"))
		}
		if err := mailer.Send(ctx, pkgcore.Mail{
			From:    s.mailFrom,
			To:      []string{contact.Address},
			Subject: subject,
			Text:    body,
		}); err != nil {
			return ErrContactCodeDeliveryFailed.WithCause(err)
		}
	default:
		return errInternal(fmt.Errorf("notification: unknown contact channel %q", contact.Channel))
	}
	return nil
}

// stampCode writes a fresh code hash and expiry onto a pending contact's
// row. Only a pending row can be stamped -- sendCode is only reachable from
// paths that gated on pending -- so the WHERE clause names the status and
// RowsAffected is asserted to be one.
func (s *ContactService) stampCode(ctx context.Context, id, hash string, expiresAt *time.Time) error {
	return dbkit.WithTenantSession(ctx, s.repo.db, func(tx *gorm.DB) error {
		res := tx.Where("id = ? AND status = ?", id, ContactStatusPending).
			Updates(&VerifiedContact{CodeHash: hash, CodeExpiresAt: expiresAt})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("notification: stampCode moved %d rows for contact %s", res.RowsAffected, id)
		}
		return nil
	})
}

// UnsubscribeInput names the contact whose consent is being withdrawn.
type UnsubscribeInput struct {
	ContactID string
}

// Unsubscribe permanently unsubscribes one contact: any status moves to
// unsubscribed and delivery refuses the address from then on
// (ErrContactUnsubscribed at the deliverability gate, before any transport
// is touched). The transition is idempotent -- an already unsubscribed
// contact unsubscribes again successfully, returning the existing row,
// because the record that matters ("this address opted out") already
// exists; the audit event fires once per actual transition, never on the
// idempotent repeat.
//
// The R8 permanence rule: an unsubscribe is for the contact as a whole, on
// every channel it might later be registered on, and it is not reversible
// through this API. Type-scoped opt-out is a deferred later-round shape
// (verified_contacts has no type_key column).
func (s *ContactService) Unsubscribe(ctx context.Context, in UnsubscribeInput) (*VerifiedContact, error) {
	contact, err := s.repo.FindByID(ctx, in.ContactID)
	if err != nil {
		if isRecordNotFound(err) {
			return nil, ErrContactNotFound
		}
		return nil, errInternal(err)
	}
	if contact.Status == ContactStatusUnsubscribed {
		return contact, nil
	}

	moved, err := s.markUnsubscribed(ctx, contact.ID)
	if err != nil {
		return nil, errInternal(err)
	}
	if !moved {
		// Lost the CAS to a concurrent transition; answer with where the
		// row actually stands rather than the stale picture we hold.
		fresh, err := s.repo.FindByID(ctx, in.ContactID)
		if err != nil {
			return nil, errInternal(err)
		}
		if fresh.Status == ContactStatusUnsubscribed {
			return fresh, nil
		}
		return nil, errInternal(fmt.Errorf("notification: unsubscribe lost the CAS; contact %s is %s", contact.ID, fresh.Status))
	}

	contact.Status = ContactStatusUnsubscribed
	if err := s.emit(ctx, AuditActionContactUnsubscribed, contact, audit.Result{Success: true}); err != nil {
		return nil, err
	}
	return contact, nil
}

// markUnsubscribed is the compare-and-swap that moves any non-unsubscribed
// contact to unsubscribed. RowsAffected == 1 means an actual transition
// (the audit event fires only then); 0 means the row already was
// unsubscribed.
func (s *ContactService) markUnsubscribed(ctx context.Context, id string) (bool, error) {
	moved := false
	err := dbkit.WithTenantSession(ctx, s.repo.db, func(tx *gorm.DB) error {
		res := tx.Where("id = ? AND status <> ?", id, ContactStatusUnsubscribed).
			Updates(&VerifiedContact{Status: ContactStatusUnsubscribed})
		if res.Error != nil {
			return res.Error
		}
		moved = res.RowsAffected == 1
		return nil
	})
	return moved, err
}

// MarkBounced marks one verified contact as bounced -- called by the
// delivery path (the notification.deliver job) after a transport reports a
// hard, permanent failure for the address (a dead number, a rejecting
// mailbox). Delivery gate-checks contact status before every send (see
// EnsureDeliverable), so a bounced contact stops receiving anything
// immediately. Bounced is terminal in this round: re-proving an address
// that bounced is a later-round remediation (AGENTS.md records the
// deferral). The call is idempotent for a contact that already bounced.
func (s *ContactService) MarkBounced(ctx context.Context, contactID string) error {
	contact, err := s.repo.FindByID(ctx, contactID)
	if err != nil {
		if isRecordNotFound(err) {
			return ErrContactNotFound
		}
		return errInternal(err)
	}
	if contact.Status == ContactStatusBounced {
		return nil
	}
	moved, err := s.markBounced(ctx, contact.ID)
	if err != nil {
		return errInternal(err)
	}
	if !moved {
		// A concurrent transition won the CAS. Whatever status the row
		// landed in, delivery's own re-check will refuse or allow by the
		// row's actual status, so there is nothing to reconcile here.
		return nil
	}
	return nil
}

// markBounced moves any non-bounced contact to bounced (compare-and-swap;
// RowsAffected semantics as in markUnsubscribed).
func (s *ContactService) markBounced(ctx context.Context, id string) (bool, error) {
	moved := false
	err := dbkit.WithTenantSession(ctx, s.repo.db, func(tx *gorm.DB) error {
		res := tx.Where("id = ? AND status <> ?", id, ContactStatusBounced).
			Updates(&VerifiedContact{Status: ContactStatusBounced})
		if res.Error != nil {
			return res.Error
		}
		moved = res.RowsAffected == 1
		return nil
	})
	return moved, err
}

// EnsureDeliverable is the consent gate delivery re-checks before every
// send to an external contact -- the send-time status recheck of the R8
// adjudication, and the seam the notification.deliver job calls with every
// message it is about to transport. Only a verified contact passes; every
// other status refuses with its own error, so a message can never ride to a
// transport on consent that lapsed between enqueue and delivery:
//
//   - verified       -- pass, the contact is returned for the send;
//   - pending        -- ErrContactNotVerified: consent was never proved;
//   - unsubscribed   -- ErrContactUnsubscribed: consent was withdrawn;
//   - bounced        -- ErrContactBounced: the address rejects messages;
//   - unknown id     -- ErrContactNotFound.
//
// The delivery job maps the refusals on the same split: a pending contact
// or an unknown id returns the refusal for the queue's bounded
// retry-and-dead-letter horizon -- a verification landing inside the
// horizon lets the job deliver itself, where unsubscribed and bounced are
// terminal and the job records them as skipped -- which is why each status
// carries its own error rather than one blanket refusal. The unsubscribed
// and bounced errors additionally carry the contact's channel in their
// "channel" parameter -- the skip record the job writes for them is per
// channel, and the job has no other way to learn the channel of a contact
// the gate refused to return.
func (s *ContactService) EnsureDeliverable(ctx context.Context, contactID string) (*VerifiedContact, error) {
	contact, err := s.repo.FindByID(ctx, contactID)
	if err != nil {
		if isRecordNotFound(err) {
			return nil, ErrContactNotFound
		}
		return nil, errInternal(err)
	}
	switch contact.Status {
	case ContactStatusVerified:
		return contact, nil
	case ContactStatusPending:
		return nil, ErrContactNotVerified
	case ContactStatusUnsubscribed:
		return nil, ErrContactUnsubscribed.WithParam("channel", contact.Channel)
	case ContactStatusBounced:
		return nil, ErrContactBounced.WithParam("channel", contact.Channel)
	default:
		return nil, errInternal(fmt.Errorf("notification: unknown contact status %q", contact.Status))
	}
}

// renderContactCode renders the verification-code message for one channel
// in the platform default locale, from the host's merged catalog.
//
// The code travels inside the message as {{.code}}, its lifetime as
// {{.minutes}} (contactCodeMinutes), so the template text and the
// constants that govern the code cannot drift. The ids live in this
// module's own bundle (locales/) under notification.contact.verify_code.*
// -- the verification code is this module's message, not a notification
// type's copy, so it does not follow render.go's <type_key>.<part>
// convention for declared types.
//
// The locale is fixed at the platform default: the contact row carries no
// locale, and negotiating the recipient's language is deferred to the
// later-round reconciliation R10 records (AGENTS.md carries the deferral).
// Every failure -- a nil catalog, an unknown locale, a missing id -- is
// ErrInternal.WithCause, never a fallback to another language.
func renderContactCode(catalog *i18n.Catalog, channel, code string) (subject, body string, err error) {
	if catalog == nil {
		return "", "", ErrInternal.WithCause(errors.New("notification: render contact code called with no catalog"))
	}
	params := map[string]any{
		"code":    code,
		"minutes": contactCodeMinutes,
	}
	switch channel {
	case ChannelSMS:
		body, err = catalog.Lookup(platformDefaultLocale, "notification.contact.verify_code.sms", params)
		if err != nil {
			return "", "", ErrInternal.WithCause(fmt.Errorf("notification: render contact code sms: %w", err))
		}
		return "", body, nil
	case ChannelEmail:
		subject, err = catalog.Lookup(platformDefaultLocale, "notification.contact.verify_code.email.subject", params)
		if err != nil {
			return "", "", ErrInternal.WithCause(fmt.Errorf("notification: render contact code email subject: %w", err))
		}
		body, err = catalog.Lookup(platformDefaultLocale, "notification.contact.verify_code.email.body", params)
		if err != nil {
			return "", "", ErrInternal.WithCause(fmt.Errorf("notification: render contact code email body: %w", err))
		}
		return subject, body, nil
	default:
		return "", "", ErrInternal.WithCause(fmt.Errorf("notification: render contact code for unknown channel %q", channel))
	}
}

// platformDefaultLocale is the locale verification-code messages render in
// when the recipient's own language is unknown -- the platform default, the
// same zh-CN default the web side's createI18n falls back to last.
const platformDefaultLocale = "zh-CN"

// contactRateLimits is the module's rate-limit table, mirroring org's
// package-constant pattern. Every entry is one dimension of a
// two-dimensional budget per code send or verify; all limits fail closed (a
// denied attempt returns ErrContactRateLimited before any side effect; a
// limiter that errors returns ErrInternal). The per-address dimensions key
// on the blind index hex, never the plaintext address; the per-tenant
// dimensions key on the tenant id. Codes are 6 digits (one million
// combinations) with a 5-minute TTL (see contact_code.go): the verify
// budgets bound how many guesses an attacker can try inside one code's
// lifetime, and the send budgets bound how many codes one tenant can push
// at one address or in total in a day.
//
// The buckets are deliberately separate namespaces per flow (a resend
// burns the send budget, never the verify budget): one patient who typed a
// code wrong several times must not be prevented from requesting a fresh
// code by the same budget that is supposed to slow down an attacker.
type contactRateLimit struct {
	key  string
	rate int
	per  time.Duration
}

const (
	// contactCodeSendDailyPerAddress is how many verification-code
	// messages one address may receive per day. Five is generous for a
	// patient (create, a resend or two, a retry) and keeps an attacker
	// who controls an address from using it to burn a tenant's budget.
	contactCodeSendDailyPerAddress = 5
	// contactCodeSendDailyPerTenant is how many verification-code messages
	// one tenant may send per day across all addresses. Twenty keeps a
	// compromised tenant credential from becoming an SMS cannon while
	// leaving a busy clinic headroom.
	contactCodeSendDailyPerTenant = 20
	// contactCodeVerifyPerAddress is how many verify attempts one address
	// may make per code lifetime. Ten guesses per code lifetime at
	// one-in-a-million per guess is the brute-force bound the design
	// promises for a 6-digit code.
	contactCodeVerifyPerAddress = 10
	// contactCodeVerifyPerTenant bounds verify attempts per tenant per
	// code lifetime as the tenant-wide backstop of the per-address budget.
	contactCodeVerifyPerTenant = 100
	// contactRateLimitWindow is the budget window the verify limits use:
	// the code TTL itself, so every budget reads "per code lifetime".
	contactRateLimitWindow = 5 * time.Minute
	// contactRateLimitDay is the budget window the send limits use: one
	// day, the coarsest honest bound for "how many codes may go out".
	contactRateLimitDay = 24 * time.Hour
)

// contactRateLimitKey prefixes the KV keys so this module's budgets can
// never collide with another module's (org, authn and the rest each carry
// their own prefix).
const contactRateLimitKey = "notification.contact-code."

// checkCodeSendLimit enforces the two send dimensions (per address, per
// tenant) before a code is generated or a message sent.
func (s *ContactService) checkCodeSendLimit(ctx context.Context, index string) error {
	limits := []contactRateLimit{
		{key: contactRateLimitKey + "send.address." + index, rate: contactCodeSendDailyPerAddress, per: contactRateLimitDay},
		{key: contactRateLimitKey + "send.tenant." + tenantIDString(ctx), rate: contactCodeSendDailyPerTenant, per: contactRateLimitDay},
	}
	return s.checkLimits(ctx, limits)
}

// checkCodeVerifyLimit enforces the two verify dimensions before a typed
// code is checked.
func (s *ContactService) checkCodeVerifyLimit(ctx context.Context, index string) error {
	limits := []contactRateLimit{
		{key: contactRateLimitKey + "verify.address." + index, rate: contactCodeVerifyPerAddress, per: contactRateLimitWindow},
		{key: contactRateLimitKey + "verify.tenant." + tenantIDString(ctx), rate: contactCodeVerifyPerTenant, per: contactRateLimitWindow},
	}
	return s.checkLimits(ctx, limits)
}

// tenantIDString returns the tenant of ctx for use in a rate-limit key, or
// the empty string when ctx carries none -- every rate-limited path here
// runs after a tenant-scoped repository read (which fails closed without a
// tenant), so the empty key is unreachable in practice and is kept only so
// the key builder cannot panic.
func tenantIDString(ctx context.Context) string {
	tid, ok := pkgcore.TenantFromContext(ctx)
	if !ok {
		return ""
	}
	return string(tid)
}

// checkLimits runs one two-dimensional budget: every dimension must allow
// the attempt, and the limiter is built lazily from the host's KV store at
// call time (the org pattern), so a nil store -- a host that never attached
// one -- fails the call closed with ErrInternal rather than allowing it. A
// denial answers ErrContactRateLimited with the dimension and the
// retry-after seconds; a limiter that itself errors answers ErrInternal --
// fail closed, never allow-on-error.
func (s *ContactService) checkLimits(ctx context.Context, limits []contactRateLimit) error {
	if s.host.KVStore() == nil {
		return errInternal(errors.New("notification: contact rate limiting called with no KV store"))
	}
	limiter := ratelimit.New(s.host.KVStore())
	for _, l := range limits {
		decision, err := limiter.Allow(ctx, l.key, ratelimit.Limit{Rate: l.rate, Per: l.per})
		if err != nil {
			return errInternal(err)
		}
		if !decision.Allowed {
			return ErrContactRateLimited.WithParam("dimension", strings.TrimPrefix(l.key, contactRateLimitKey)).
				WithParam("retry_after_seconds", fmt.Sprintf("%d", int(decision.ResetAfter.Seconds())))
		}
	}
	return nil
}

// indexer returns the blind indexer for a channel. The two indexers are
// validated at Register time (ErrContactEmailIndexerRequired /
// ErrContactPhoneIndexerRequired), so by the time a service method runs the
// channel's indexer is non-nil.
func (s *ContactService) indexer(channel string) *dbkit.BlindIndexer {
	if channel == ChannelSMS {
		return s.phoneIndexer
	}
	return s.emailIndexer
}

// validateContactChannel checks a channel against the closed vocabulary.
func validateContactChannel(channel string) (string, error) {
	switch channel {
	case ChannelEmail, ChannelSMS:
		return channel, nil
	default:
		return "", ErrContactInvalidChannel
	}
}

// normalizeContactAddress canonicalizes an address for its channel: E.164
// for phone numbers (dbkit.NormalizePhoneE164), lowercased for email
// (dbkit.NormalizeEmail). An address that fails normalization is invalid,
// never stored -- ErrContactInvalidAddress.
func normalizeContactAddress(channel, address string) (string, error) {
	var (
		normalized string
		err        error
	)
	switch channel {
	case ChannelSMS:
		normalized, err = dbkit.NormalizePhoneE164(address)
	case ChannelEmail:
		normalized, err = dbkit.NormalizeEmail(address)
	}
	if err != nil || normalized == "" {
		return "", ErrContactInvalidAddress.WithCause(err)
	}
	return normalized, nil
}

// emit records one consent transition on the audit trail through
// dbkit/audit's declarative Emit. The transition has already committed when
// the event is emitted; a failure to emit therefore returns an internal
// error to the caller -- the transition happened, but the record that
// outlives it did not land, and the caller must treat the operation as
// failed and investigate. This differs from the reference app's notes
// handler, which logs an emit failure and returns success: notification has
// no logger to hand the failure to (it does not import observability, per
// AGENTS.md's dependency note), so the caller is the only sink. A known
// limitation of this round follows from the same shape: an operation that
// hits the idempotent return path of Unsubscribe (already unsubscribed)
// emits nothing, which is correct -- the idempotent repeat is not a state
// change -- and retries of a failed transition after its audit emit failed
// do not re-emit (the state change itself is not re-run).
func (s *ContactService) emit(ctx context.Context, action string, c *VerifiedContact, result audit.Result) error {
	if err := audit.Emit(ctx, s.host.EventBus(), s.audit, audit.Input{
		Action:   action,
		Resource: auditResourceContact(c),
		Result:   result,
	}); err != nil {
		return errInternal(err)
	}
	return nil
}

// errInternal wraps a database or infrastructure failure as the module's
// internal error, carrying the cause.
func errInternal(err error) error {
	return ErrInternal.WithCause(err)
}

// isRecordNotFound reports whether err is dbkit's absent-row answer: the
// *apperr.Error carrying dbkit.ErrRecordNotFound's code that
// dbkit.Repository.FindByID returns when no row matches (never gorm's own
// sentinel, which dbkit translates before it escapes the repository -- see
// go/dbkit/repository.go's ErrRecordNotFound). Service code therefore
// classifies the apperr code rather than testing errors.Is against the gorm
// sentinel; the one place this module does test the gorm sentinel directly
// is VerifiedContactRepository.ByChannelAndAddressIndex, whose own query
// runs raw through gorm and sees the untranslated error.
func isRecordNotFound(err error) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == dbkit.ErrRecordNotFound.Code
}
