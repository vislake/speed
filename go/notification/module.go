package notification

import (
	"embed"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/notification/locales"
	"github.com/vislake/speed/go/notification/migrations"
)

// moduleName is notification's pkgcore.Module.Name(). It is also the key
// dbkit.MigrationRegistry.Register builds its dependency graph on.
const moduleName = "notification"

// Module implements pkgcore.Module for go/notification: the tenant's
// notification inbox, its per-type channel preference matrix, the
// consent-gated external-contact ledger (verified_contacts, platform
// blacklist, send_records), and the outbound-delivery pipeline that turns a
// dispatch into per-channel sends over the queue.
//
// # Wiring
//
// A host constructs one with NewModule and hands it to Kernel.Bootstrap.
// The module's tables live in db, which the host opened through dbkit.Open
// (so it carries the tenant-isolation plugin) and migrated before Bootstrap.
// Register then validates the module's required seams, contributes the
// module's declarations -- its event catalog, its audit actions, its job
// handler -- to the host's registry, and attaches the registrars and the
// host registry the services need. The module performs no I/O of its own
// during registration.
type Module struct {
	// db is the connection the module's tables live in. The inbox,
	// preference, contact and send-record repositories are built over it,
	// sharing one connection exactly as the module's other repositories do.
	db *gorm.DB

	// prefs is the preference matrix's decision layer, built at construction
	// and served to consumers through Preferences(). Its type-taxonomy
	// reference is attached during Register (attachTypes); before that it is
	// nil, which the service treats as an empty taxonomy.
	prefs *PreferenceService

	// contacts is the consent-ledger's decision layer, built at construction
	// and served to consumers through Contacts(). Its host seams arrive
	// through the With* options below and are validated by Register, which
	// then attaches the registry reference and the audit-action registrar
	// through attachHost; before Register, the service's seams are nil and
	// every service method fails closed on them.
	contacts *ContactService

	// deliveries is the outbound-delivery pipeline, built at construction
	// over the preference, contact and inbox services, and served to
	// consumers through Deliveries(). Its queue and resolver seams arrive
	// through the With* options below; its host registry slice is attached
	// during Register (attachHost), so before Register the pipeline can
	// only refuse -- every seam nil check fails closed.
	deliveries *DeliveryService

	// hub is this replica's inbox-announcement fan-out, constructed at
	// NewModule time and subscribed to EventInboxCreated during Register.
	// The platform-staff shell that pushes announcements to browsers or
	// devices is a later round's consumer of hub.Subscribe (hub.go's doc
	// comment); this round's delivery pipeline is the producer side.
	hub *Hub

	// queue is the jobs.Queue delivery jobs are enqueued on (Dispatch) and
	// handled by (the delivery handler Register registers). REQUIRED: a
	// Module without one fails Register with ErrDeliveryQueueRequired. The
	// queue seam of a standalone host is jobs' StandaloneQueue; of a
	// distributed host, its AsynqQueue.
	queue jobs.Queue

	// resolver supplies a user delivery's addresses at send time (see
	// UserAddressResolver). REQUIRED on the same terms as queue
	// (ErrUserAddressResolverRequired): without it the module cannot
	// deliver to a user on the email or SMS channels. The seam keeps the
	// module's own tables free of identity data -- the host's authn half
	// owns addresses, and notification never imports it.
	resolver UserAddressResolver

	// The six required seams the consent ledger and the delivery pipeline
	// run on, filled by the With* options. sms is the SMSSender
	// verification codes go out on when the contact's channel is sms (and
	// the delivery pipeline's SMS channel uses); mailFrom the From address
	// of every email this module composes; emailIndexer and phoneIndexer
	// the blind indexers that make contact addresses queryable without
	// ever storing them in plaintext. Register refuses to boot the module
	// without any of them (ErrSMSSenderRequired, ErrMailFromRequired,
	// ErrContactEmailIndexerRequired, ErrContactPhoneIndexerRequired,
	// ErrDeliveryQueueRequired, ErrUserAddressResolverRequired),
	// imitating org's Register-time validation of its own required seams.
	sms          SMSSender
	mailFrom     string
	emailIndexer *dbkit.BlindIndexer
	phoneIndexer *dbkit.BlindIndexer
}

// Option configures a Module at construction time.
type Option func(*Module)

// WithSMSSender injects the SMSSender verification-code messages use when a
// contact's channel is sms. It is REQUIRED: Register returns
// ErrSMSSenderRequired without one, so a module with no sender fails at
// boot rather than discover the gap on the first code a patient needs.
// NewConsoleSMSSender is the zero-external-dependency implementation (see
// sms.go); a distributed host may hand over any sender satisfying the
// structural interface, the console sender's twin among authn's own.
func WithSMSSender(sender SMSSender) Option {
	return func(m *Module) { m.sms = sender }
}

// WithMailFrom sets the From address of every outbound mail this module
// composes -- email verification codes first among them. It is REQUIRED:
// Register returns ErrMailFromRequired without one (org's own
// WithMailFrom is required on the same terms).
func WithMailFrom(from string) Option {
	return func(m *Module) { m.mailFrom = from }
}

// WithContactEmailIndexer injects the blind indexer email contact addresses
// are normalized and indexed with (dbkit.NewBlindIndexer over
// dbkit.NormalizeEmail). It is REQUIRED: Register returns
// ErrContactEmailIndexerRequired without one, so a module that cannot index
// an email address can never store one. The indexer's key must never be the
// encryption key of the module's address cipher (the F8 design rule;
// contact.go's doc comment spells it out).
func WithContactEmailIndexer(indexer *dbkit.BlindIndexer) Option {
	return func(m *Module) { m.emailIndexer = indexer }
}

// WithContactPhoneIndexer is the SMS twin of WithContactEmailIndexer: the
// blind indexer phone contacts are indexed with (dbkit.NewBlindIndexer over
// dbkit.NormalizePhoneE164), REQUIRED on the same terms
// (ErrContactPhoneIndexerRequired).
func WithContactPhoneIndexer(indexer *dbkit.BlindIndexer) Option {
	return func(m *Module) { m.phoneIndexer = indexer }
}

// WithDeliveryQueue injects the jobs.Queue the module's outbound deliveries
// run on: DeliveryService.Dispatch enqueues one notification.deliver job on
// it, and the handler Module.Register registers (reg.Jobs.Handle) is
// executed by its workers. It is REQUIRED: Register returns
// ErrDeliveryQueueRequired without one, so a module with no queue fails at
// boot rather than have every dispatch refuse at run time. The queue of a
// standalone host is jobs' StandaloneQueue; of a distributed host, its
// AsynqQueue -- whichever the host's own wiring chose, passed straight
// through.
func WithDeliveryQueue(queue jobs.Queue) Option {
	return func(m *Module) { m.queue = queue }
}

// WithUserAddressResolver injects the seam user deliveries resolve their
// recipients' addresses through (see UserAddressResolver). It is REQUIRED:
// Register returns ErrUserAddressResolverRequired without one, so a module
// that cannot reach a user on the email or SMS channels fails at boot
// rather than dead-letter every such delivery.
func WithUserAddressResolver(resolver UserAddressResolver) Option {
	return func(m *Module) { m.resolver = resolver }
}

// NewModule returns a Module whose tables live in db. Constructing a Module
// performs no I/O: opening and migrating db is the host's responsibility,
// done once at startup before Bootstrap ever calls Register. The required
// seams -- the consent ledger's four (see the With* option docs) plus the
// delivery pipeline's queue and user-address resolver -- arrive through the
// With* options; a Module missing any of them fails Register (see the
// option docs), never a call.
func NewModule(db *gorm.DB, opts ...Option) *Module {
	m := &Module{
		db:       db,
		prefs:    NewPreferenceService(db),
		contacts: NewContactService(db),
	}
	for _, opt := range opts {
		opt(m)
	}
	m.deliveries = newDeliveryService(db, m.prefs, m.contacts)
	m.deliveries.queue = m.queue
	m.deliveries.resolver = m.resolver
	m.deliveries.sms = m.sms
	m.deliveries.mailFrom = m.mailFrom
	m.hub = NewHub()
	return m
}

// Preferences returns the module's preference service -- the matrix's only
// sanctioned read/write face. A host hands this to its HTTP handler once
// Bootstrap has run, so the service's type-taxonomy reference is attached
// (Register) by the time any caller reaches it.
func (m *Module) Preferences() *PreferenceService {
	return m.prefs
}

// Contacts returns the module's contact service -- the consent ledger's
// only sanctioned read/write face, and the deliverability gate the
// delivery job calls before every send to an external contact. A host
// hands this to its HTTP handler and its queue handlers once Bootstrap has
// run, so the service's seams are validated and attached (Register) by the
// time any caller reaches it.
func (m *Module) Contacts() *ContactService {
	return m.contacts
}

// Deliveries returns the module's delivery pipeline -- the only sanctioned
// way to enqueue an outbound delivery (DeliveryService.Dispatch), and the
// jobs.Handler the module registered for its delivery jobs. A host calls
// this once Bootstrap has run: the pipeline's queue and resolver seams were
// validated by Register, and its registry slice is attached, by the time any
// caller reaches it.
func (m *Module) Deliveries() *DeliveryService {
	return m.deliveries
}

// Name implements pkgcore.Module.
func (m *Module) Name() string { return moduleName }

// DependsOn implements pkgcore.Module: nothing.
//
// This is a real answer, not a stub. notification sits above jobs, config
// and dbkit in docs/internal/01-architecture.md's graph, but none of those
// is a pkgcore.Module -- they are libraries the host wires, and DependsOn
// enumerates only modules in the bootstrap set. notification must also NOT
// depend on authn, rbac or org: it learns about users, memberships and
// authorization from domain events and opaque ids, never from their Go
// types, and every id its tables store cites exactly that rule. Naming any
// of them here would make notification unbootable in a host that does not
// run them, and would invert the event-driven direction the module exists
// to serve.
func (m *Module) DependsOn() []string { return nil }

// Migrations implements pkgcore.Module.
func (m *Module) Migrations() embed.FS { return migrations.FS }

// Locales implements pkgcore.Module: the descriptions of notification's
// error codes and its own send-time message templates, in both supported
// languages with identical id sets. The bundle's entries travel under
// notification.* ids only -- the error codes of errors.go and the
// verification-code templates of contact.go (the
// notification.contact.verify_code.* ids renderContactCode renders) --
// never templates for other modules' notification types, which live in the
// declaring modules' own bundles (see render.go's template-id convention).
func (m *Module) Locales() embed.FS { return locales.FS }

// OpenAPISpec implements pkgcore.Module: nil.
//
// notification has no OpenAPI fragment yet, because it has no HTTP surface
// yet: the module's routes arrive in a later round's HTTP block, and the
// fragment -- go/notification/api/openapi.yaml, joining the spec-first
// pipeline of docs/internal/21-api-contract.md -- ships with them. Until
// then a nil spec contributes nothing to the merged document, which is the
// same nothing every fragment-less module contributes today.
func (m *Module) OpenAPISpec() []byte { return nil }

// Register implements pkgcore.Module. Per the interface's contract it only
// declares and wires -- no database call, no outbound call, nothing that
// touches m.db.
//
// Register runs in three phases, each validating what the one before it
// established:
//
//  1. The module's six required seams are validated first. A Module missing
//     any of them -- the consent ledger's SMS sender
//     (ErrSMSSenderRequired), mail From address (ErrMailFromRequired) and
//     either address blind indexer (ErrContactEmailIndexerRequired,
//     ErrContactPhoneIndexerRequired), or the delivery pipeline's queue
//     (ErrDeliveryQueueRequired) and user-address resolver
//     (ErrUserAddressResolverRequired) -- is refused here, at boot, so the
//     failure names the missing seam instead of surfacing later as a
//     nil-panic or a dead verification or delivery channel. This imitates
//     org's Register-time validation of its own required seams. The consent
//     ledger's four transport seams are then attached to the contact
//     service a host reaches through Contacts(), the attachment block in
//     the body below -- a module whose seams only validated, never landed
//     on that service, would hand the host a ledger whose first
//     CreateContact dereferences a nil blind indexer.
//
//  2. The module's declarations are contributed: the event catalog
//     (EventInboxCreated, one declared event) and the consent ledger's
//     audit actions (notification.contact.attested, .verified and
//     .unsubscribed) onto the host's audit-action registrar, so the
//     ledger's every state transition is auditable from the moment the
//     module boots.
//
//  3. The registrars and the module's own machinery are attached: the host
//     registry's notification-type registrar to the preference service
//     (attachTypes, giving it the live taxonomy every preference write
//     validates against), the registry reference plus the audit-action
//     registrar to the contact service (attachHost), the delivery pipeline's
//     job handler (reg.Jobs.Handle on the delivery job type, so the host's
//     queue workers run it) and its own host-registry reference
//     (attachHost, the slice the pipeline reads its event catalog and
//     locales from at send time), and the hub to the inbox-created event
//     (reg.Events.Subscribe), so a delivery that lands an inbox row
//     announces it on this replica's bus. Every attachment only passes
//     already-validated references down; nothing is re-validated here.
//
// No permissions or routes are declared yet: the module has no caller-scoped
// operation and no request path until a later round builds its HTTP surface,
// and each declaration arrives with the producer that needs it, exactly as
// errors.go's doc comment says of error codes.
func (m *Module) Register(reg *pkgcore.Registry) error {
	if m.sms == nil {
		return ErrSMSSenderRequired
	}
	if m.mailFrom == "" {
		return ErrMailFromRequired
	}
	if m.emailIndexer == nil {
		return ErrContactEmailIndexerRequired
	}
	if m.phoneIndexer == nil {
		return ErrContactPhoneIndexerRequired
	}
	if m.queue == nil {
		return ErrDeliveryQueueRequired
	}
	if m.resolver == nil {
		return ErrUserAddressResolverRequired
	}
	// The consent ledger's four transport seams are attached to the contact
	// service Contacts() hands out only once every one of them validated.
	// Options write the module's own fields (NewModule); the checks above
	// refuse a module missing any; and this block is what makes the
	// accessor's doc promise true -- "the service's seams are validated and
	// attached (Register) by the time any caller reaches it". The delivery
	// pipeline's own seams get the same copy in NewModule; the contact
	// service predates the options there (see its constructor), so its copy
	// belongs here, next to the validation that gates it.
	m.contacts.sms = m.sms
	m.contacts.mailFrom = m.mailFrom
	m.contacts.emailIndexer = m.emailIndexer
	m.contacts.phoneIndexer = m.phoneIndexer
	if err := reg.Events.Publishes(inboxEventDecls...); err != nil {
		return err
	}
	if err := reg.AuditActions.Add(contactAuditActionDecls...); err != nil {
		return err
	}
	if err := reg.Jobs.Handle(jobTypeDeliver, m.deliveries); err != nil {
		return err
	}
	m.prefs.attachTypes(reg.Notifications)
	m.contacts.attachHost(reg, reg.AuditActions)
	m.deliveries.attachHost(reg)
	reg.Events.Subscribe(EventInboxCreated, m.hub.HandleEvent)
	return nil
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
