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

//go:embed api/openapi.yaml
var openAPISpecYAML []byte

// moduleName is notification's pkgcore.Module.Name(). It is also the key
// dbkit.MigrationRegistry.Register builds its dependency graph on.
const moduleName = "notification"

// apiPath is the common prefix notification's HTTP routes are mounted at
// (see Register below). It must agree with the "paths:" keys of this
// module's own OpenAPI fragment (api/openapi.yaml): every one of them
// starts with this prefix, and Handler's inner mux (built by
// api.HandlerFromMux, see handler.go) registers each spec path as an
// ABSOLUTE net/http pattern -- mounting at apiPath here only tells the
// host's outer mux which requests to hand to Handler at all. One path under
// this prefix is deliberately absent from the fragment: the inbox stream,
// GET /api/v1/notifications/stream -- server-sent events are not an
// OpenAPI 3.0 media type, the fragment's header records the omission, and
// handler.go mounts it on the inner mux by hand alongside the spec paths.
const apiPath = "/api/v1/notifications"

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
	// distributed host, go/jobs/queue/asynq's Queue.
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

	// inbox is the in-app inbox's repository, built at construction over db.
	// The delivery pipeline (delivery.go's DeliveryService) holds the SAME
	// instance -- Module constructs it once and hands it to newDeliveryService
	// -- so the HTTP surface Handler reads and marks is the very repository
	// the pipeline writes, one data path for the inbox rather than two
	// wrappers over one connection that documentation must explain away.
	inbox *Repository

	// subject resolves the HTTP caller's identity for the endpoints
	// handler.go serves (WithSubjectResolver) -- the same seam org declares
	// under the same name, satisfied structurally by whatever
	// authenticating layer the host wires. Nil is a legal, if unusable,
	// wiring: the module's whole HTTP surface is own-data self-service
	// (handler.go's file comment), so every endpoint fails closed with
	// ErrSubjectUnresolved rather than Register refusing to boot -- a host
	// that has not wired an identity layer yet can still boot notification
	// and exercise its Go service faces (Deliveries, Contacts,
	// Preferences) until the seam arrives.
	subject SubjectResolver

	// handler serves notification's HTTP surface. Built by Register, once
	// every Option has already run -- see Register's own doc comment for why
	// this cannot happen in NewModule.
	handler *Handler
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
// encryption key of the module's address cipher (AGENTS.md's "Separate
// index keys from the cipher key" adjudication; contact.go's doc comment
// spells it out).
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
// standalone host is jobs' StandaloneQueue; of a distributed host,
// go/jobs/queue/asynq's Queue -- whichever the host's own wiring chose,
// passed straight through.
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

// WithSubjectResolver injects the seam Handler uses to identify the HTTP
// caller (see SubjectResolver's own doc comment in handler.go), mirroring
// org's option of the same name. The module's whole HTTP surface is
// own-data self-service, so the seam gates every endpoint the handler
// serves -- the per-recipient inbox and preference rows, and the contact
// operations, whose roster is tenant-wide and uses the resolved id as an
// identification gate only (handler.go's file comment). That is exactly
// why an unwired resolver fails every endpoint closed with
// ErrSubjectUnresolved instead of Register refusing to boot: a host
// without an identity layer yet can still boot notification and exercise
// its Go service faces (Deliveries, Contacts, Preferences).
func WithSubjectResolver(resolver SubjectResolver) Option {
	return func(m *Module) { m.subject = resolver }
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
		inbox:    NewRepository(db),
		prefs:    NewPreferenceService(db),
		contacts: NewContactService(db),
	}
	for _, opt := range opts {
		opt(m)
	}
	m.deliveries = newDeliveryService(m.inbox, m.prefs, m.contacts)
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

// OpenAPISpec implements pkgcore.Module: it returns notification's own
// OpenAPI fragment, embedded from api/openapi.yaml. That fragment is the
// single source of this module's API surface -- the api package's generated
// types and ServerInterface (api/notification-server.gen.go, regenerated by
// task api:gen) derive from it, and Handler implements that interface (see
// handler.go) -- per docs/internal/21-api-contract.md's spec-first decision.
// The one route this module serves that the fragment deliberately does not
// carry, the inbox stream, is documented in the fragment's own header and
// mounted by handler.go.
func (m *Module) OpenAPISpec() []byte { return openAPISpecYAML }

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
// No permissions are declared: notification's routes carry no permission
// gate of their own -- who may read a recipient's inbox or a tenant's
// contact roster is a host-side authorization decision (the fixed
// middleware chain's rbac layer), and the module declares no resource its
// consumer would have to invent a permission for. The HTTP surface itself
// is declared here, as Handler's construction and one route mount: built in
// Register -- never in NewModule -- because every Option a caller passed to
// NewModule (WithSubjectResolver above all) has already run by the time
// Register is called, so the handler is built from the host's final
// wiring, and each declaration arrives with the producer that needs it,
// exactly as errors.go's doc comment says of error codes.
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

	// Handler is built here, not in NewModule, deliberately: every Option a
	// caller passed to NewModule -- WithSubjectResolver above all -- has
	// already run by the time Register is called (Bootstrap calls Register
	// only after NewModule has returned), so the resolver Handler is given
	// is whichever one the host actually configured, never a nil one
	// captured before the option ran. The registry slice the handler reads
	// at request time (its catalog, for type-description rendering) is
	// attached under the same rule delivery.go's attachHost documents: read
	// from the registry at call time, never captured here, when
	// reg.Locales() is still nil.
	m.handler = NewHandler(m.inbox, m.prefs, m.contacts, m.hub, m.subject)
	m.handler.attachHost(reg)
	reg.Routes.Mount(apiPath, m.handler)
	return nil
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
