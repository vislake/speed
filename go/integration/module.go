package integration

import (
	"context"
	"embed"
	"errors"
	"net/http"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"

	"github.com/vislake/speed/go/integration/locales"
	"github.com/vislake/speed/go/integration/migrations"
)

// moduleName is integration's pkgcore.Module.Name(). It is also the module
// name dbkit.MigrationRegistry.Register keys its dependency graph on.
const moduleName = "integration"

// MaxAPIKeyLifetime is both the forced expiry ceiling Service.Create
// enforces on every request (ErrExpiryExceedsMaximum past it) and the
// default it applies when a request names no ExpiresAt at all, per the
// design doc's forced-expiry-ceiling rule (defaulting to one year) -- the
// ceiling and the unspecified-request default are declared as the same one
// year, not two numbers that happen to agree.
//
// It is a package-level named constant, not a dynamic configuration item,
// for the identical reason go/rbac's DefaultCacheTTL gives: reading one
// from go/config would add a dependency edge this module's position in
// docs/internal/01-architecture.md's graph does not have, paid for by every
// consumer that boots this module without config. A host that genuinely
// needs a different ceiling is a later round's WithMaxAPIKeyLifetime
// option, not a reason to wire config in now.
const MaxAPIKeyLifetime = 365 * 24 * time.Hour

// Permission strings this module declares for its API key (round 1) and
// webhook subscription (round 2) management surfaces. Like go/rbac's own
// PermissionRead/PermissionManage, this module does not check them itself
// -- it declares the vocabulary; enforcement is whatever authorization
// layer the host wires in front of a future HTTP surface (neither round
// ships mounted routes; see AGENTS.md).
const (
	// PermissionRead covers listing a tenant's API keys.
	PermissionRead = "integration:apikey:read"

	// PermissionManage covers creating, rotating and revoking API keys.
	PermissionManage = "integration:apikey:manage"

	// PermissionWebhookRead covers listing a tenant's webhook subscriptions
	// and their delivery log -- round 2's counterpart of PermissionRead,
	// following the identical "integration:<entity>:<verb>" naming
	// convention.
	PermissionWebhookRead = "integration:webhook:read"

	// PermissionWebhookManage covers creating, updating and deleting
	// webhook subscriptions -- round 2's counterpart of PermissionManage.
	PermissionWebhookManage = "integration:webhook:manage"
)

// Audit actions this module contributes, following the
// "<module>.<entity>.<action>" convention (backend coding standard §8).
// Service calls audit.Emit under each of these after the corresponding
// mutation commits; see service.go.
const (
	// AuditActionAPIKeyCreate is emitted after Service.Create persists a
	// new key.
	AuditActionAPIKeyCreate = "integration.apikey.create"

	// AuditActionAPIKeyRotate is emitted after Service.Rotate persists the
	// replacement key and revokes its predecessor.
	AuditActionAPIKeyRotate = "integration.apikey.rotate"

	// AuditActionAPIKeyRevoke is emitted after Service.Revoke persists a
	// revocation.
	AuditActionAPIKeyRevoke = "integration.apikey.revoke"

	// AuditActionWebhookSubscriptionCreate is emitted after
	// Service.CreateWebhookSubscription persists a new subscription.
	AuditActionWebhookSubscriptionCreate = "integration.webhook_subscription.create"

	// AuditActionWebhookSubscriptionUpdate is emitted after
	// Service.UpdateWebhookSubscription persists a change.
	AuditActionWebhookSubscriptionUpdate = "integration.webhook_subscription.update"

	// AuditActionWebhookSubscriptionDelete is emitted after
	// Service.DeleteWebhookSubscription removes a subscription.
	AuditActionWebhookSubscriptionDelete = "integration.webhook_subscription.delete"
)

// auditActionDecls is every audit action this module declares through
// Register, kept as one slice so Register and any test enumerating "every
// action this module contributes" read from a single place.
var auditActionDecls = []string{
	AuditActionAPIKeyCreate,
	AuditActionAPIKeyRotate,
	AuditActionAPIKeyRevoke,
	AuditActionWebhookSubscriptionCreate,
	AuditActionWebhookSubscriptionUpdate,
	AuditActionWebhookSubscriptionDelete,
}

// ErrAlreadyAttached reports a second Attach call on one Module.
var ErrAlreadyAttached = apperr.Internal("integration.already_attached")

// Module implements pkgcore.Module for go/integration.
//
// It declares its own permissions and audit actions during Register, and
// builds the runtime Service during Attach, mirroring the two-phase shape
// go/rbac and go/config both use: Register only declares (per
// pkgcore.Module's own contract, "must not perform I/O"), and Attach is
// where the seams a host wired through the New Module options actually get
// used.
type Module struct {
	// db is the *gorm.DB this module's one table lives in. It is opened and
	// migrated by the host before Register is ever called; the module
	// itself performs no I/O until Attach.
	db *gorm.DB

	// permissions is the mandatory PermissionLister seam (see seams.go and
	// WithPermissionLister). Left nil is a legal Module construction --
	// Attach does not refuse it -- but Service.Create then refuses any
	// request naming a non-empty Scopes with ErrPermissionListerUnavailable,
	// since a security check with nothing to check against must fail
	// closed, not silently skip itself.
	permissions PermissionLister

	// membership is the optional MembershipChecker seam (see seams.go and
	// WithMembershipChecker). Nil is a fully legal, permanent configuration:
	// it only blanks List's cosmetic CreatorLeft flag, never a security
	// property.
	membership MembershipChecker

	// clock is the injectable time source tests override through
	// withClock; production always uses the zero value's time.Now.
	clock func() time.Time

	// The round-2 fields below back the outbound-webhook surface. See
	// WithEventMapping, WithWebhookQueue and their doc comments for what
	// each is, and eventmapping.go's EventMapping doc comment for why
	// eventMappings is a NewModule-time Option rather than a
	// pkgcore.Registry field.
	eventMappings []EventMapping
	mappingIndex  eventMappingIndex
	queue         jobs.Queue

	// httpClient and urlValidator are unexported test-only overrides,
	// mirroring withClock's own "never for a host to call" contract -- see
	// their With-less doc comments below for why webhook delivery and
	// creation-time SSRF validation each need one.
	httpClient   *http.Client
	urlValidator func(ctx context.Context, url string) error

	// service is the Service Attach produced, nil until then. It is what
	// makes a second Attach detectable, and it is what Module's own
	// forwarding wrappers (handleDomainEvent below, webhookDeliveryHandler)
	// read once Attach has run.
	service *Service
}

// Option configures a Module built by NewModule.
type Option func(*Module)

// WithPermissionLister wires the host's permission-listing seam. See
// PermissionLister's own doc comment in seams.go for why this is a
// structurally-typed seam rather than a direct go/rbac import, and for a
// concrete wiring example over a real rbac.Service.
func WithPermissionLister(l PermissionLister) Option {
	return func(m *Module) { m.permissions = l }
}

// WithMembershipChecker wires the host's optional "is this user still a
// member" seam. See MembershipChecker's own doc comment in seams.go for why
// this is optional where WithPermissionLister is not.
func WithMembershipChecker(c MembershipChecker) Option {
	return func(m *Module) { m.membership = c }
}

// withClock overrides the Service's time source. Unexported: it exists for
// this module's own tests (expiry-ceiling and rotation-timing assertions
// that would otherwise race the wall clock), never for a host to call.
func withClock(now func() time.Time) Option {
	return func(m *Module) { m.clock = now }
}

// WithEventMapping declares one or more business modules' internal-to-
// public event schema mappings. See eventmapping.go's EventMapping doc
// comment for the full design rationale (why this is a Module Option
// rather than a pkgcore.Registry field or a business-module import) and for
// the exact contract each EventMapping must satisfy.
//
// Every WithEventMapping call is additive (later calls append rather than
// replace), so a host composing several business modules' mappings can call
// it once per module if that reads more clearly than collecting a slice
// itself.
//
// A duplicate InternalType across every declared mapping, or a mapping
// missing a required field, fails Register with ErrDuplicateEventMapping or
// ErrInvalidEventMapping respectively -- see buildEventMappingIndex.
func WithEventMapping(mappings ...EventMapping) Option {
	return func(m *Module) { m.eventMappings = append(m.eventMappings, mappings...) }
}

// WithWebhookQueue injects the jobs.Queue webhook deliveries are enqueued
// on (handleDomainEvent, webhook_delivery.go) and executed by (the handler
// Register registers on reg.Jobs). Unlike round 1's WithPermissionLister,
// an unwired queue does NOT fail Register or Attach: a host that has not
// wired jobs yet can still boot this module and manage subscriptions
// through Service's Create/List/Update/Delete surface -- only
// handleDomainEvent's own enqueue step is affected, and it already treats a
// nil queue as "record the delivery, warn, and stop" rather than a hard
// failure (see enqueueDelivery's own doc comment), the identical
// resilience posture handleDomainEvent itself follows for every other
// failure a domain-event subscriber can hit.
func WithWebhookQueue(queue jobs.Queue) Option {
	return func(m *Module) { m.queue = queue }
}

// withHTTPClient overrides the http.Client webhook delivery attempts send
// through (webhook_delivery.go's attemptDelivery), in place of
// newSafeHTTPClient's SSRF-guarded default. Unexported: it exists for this
// module's own tests, which must be able to deliver to an httptest.Server
// listening on loopback -- exactly the address newSafeHTTPClient's transport
// is built to refuse. Never for a host to call: a production Service always
// uses the guarded default.
func withHTTPClient(client *http.Client) Option {
	return func(m *Module) { m.httpClient = client }
}

// withWebhookURLValidator overrides ValidateWebhookURL for
// Service.CreateWebhookSubscription/UpdateWebhookSubscription. Unexported,
// for the identical reason withHTTPClient exists: this module's own tests
// configure subscriptions pointing at an httptest.Server, which
// ValidateWebhookURL's production behavior would always refuse. Never for a
// host to call.
func withWebhookURLValidator(validate func(ctx context.Context, url string) error) Option {
	return func(m *Module) { m.urlValidator = validate }
}

// NewModule returns a Module whose table lives in db. Constructing a Module
// performs no I/O -- opening and migrating db is the caller's
// responsibility, done once at startup before Bootstrap ever calls
// Register.
func NewModule(db *gorm.DB, opts ...Option) *Module {
	m := &Module{db: db}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Name implements pkgcore.Module.
func (m *Module) Name() string { return moduleName }

// DependsOn implements pkgcore.Module.
//
// The empty list is deliberate, not an oversight: this module reaches
// permission listing and membership checking through the structurally-typed
// seams in seams.go, never through an import of go/rbac, go/authn or
// go/org, so none of them belongs in this list -- DependsOn names compile-
// time module dependencies for Kernel.Bootstrap's topological ordering, and
// this module has none among the business modules.
func (m *Module) DependsOn() []string { return nil }

// Migrations implements pkgcore.Module.
func (m *Module) Migrations() embed.FS { return migrations.FS }

// Locales implements pkgcore.Module: one message per error code in
// errors.go, in both zh-CN and en-US with identical id sets (the parity
// pkgcore/i18n's Builder.AddModule enforces while Kernel.Bootstrap merges
// the catalog).
func (m *Module) Locales() embed.FS { return locales.FS }

// OpenAPISpec implements pkgcore.Module: nil.
//
// Neither round mounts an HTTP route of its own -- round 1 built a minimal
// Go-level 429 translation (httpguard.go) instead of a full API-key CRUD
// surface, and round 2 draws the identical line for webhook subscription
// management; see AGENTS.md's "Deliberately not in scope" table for both.
func (m *Module) OpenAPISpec() []byte { return nil }

// Register implements pkgcore.Module. Per the interface's own contract
// ("It must not perform I/O; it only declares"), it declares this module's
// permission vocabulary and its audit actions, builds the event-mapping
// index round 2's WithEventMapping declarations feed, subscribes to every
// distinct InternalType that index names (reg.Events.Subscribe performs no
// I/O of its own -- it only registers a callback for later), and registers
// this module's webhook-delivery job handler on reg.Jobs. It touches
// neither the database nor the network.
func (m *Module) Register(reg *pkgcore.Registry) error {
	if err := reg.Permissions.Add(PermissionRead, PermissionManage, PermissionWebhookRead, PermissionWebhookManage); err != nil {
		return err
	}
	if err := reg.AuditActions.Add(auditActionDecls...); err != nil {
		return err
	}

	idx, err := buildEventMappingIndex(m.eventMappings)
	if err != nil {
		return err
	}
	m.mappingIndex = idx
	for internalType := range idx.byInternal {
		reg.Events.Subscribe(internalType, m.handleDomainEvent)
	}

	return reg.Jobs.Handle(jobTypeWebhookDeliver, webhookDeliveryHandler{module: m})
}

// handleDomainEvent is the pkgcore.EventHandler Register subscribes for
// every declared EventMapping's InternalType. It forwards to Service's own
// handleDomainEvent once Attach has built one; a domain event published
// before Attach ran (unusual -- Attach happens at startup, before any
// application traffic that could publish a business event) is logged and
// swallowed rather than panicking on a nil Service, following this whole
// file's "never fail the publisher" resilience posture.
func (m *Module) handleDomainEvent(ctx context.Context, evt pkgcore.Event) error {
	if m.service == nil {
		obs.FromContext(ctx).Warn("integration ignored a domain event published before Module.Attach",
			"event_type", evt.Type)
		return nil
	}
	return m.service.handleDomainEvent(ctx, evt)
}

// webhookDeliveryHandler adapts Module onto jobs.Handler and
// jobs.FailureHook for the webhook delivery job type, forwarding both calls
// to the Service Attach built -- the identical "Module method registered
// during Register, Service built later during Attach" split
// handleDomainEvent uses, needed for the same reason: reg.Jobs.Handle must
// be called during Register, before a Service exists to hand it directly.
type webhookDeliveryHandler struct {
	module *Module
}

// Type implements jobs.Handler.
func (h webhookDeliveryHandler) Type() string { return jobTypeWebhookDeliver }

// Handle implements jobs.Handler.
func (h webhookDeliveryHandler) Handle(ctx context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
	if h.module.service == nil {
		return jobs.Result{}, errors.New("integration: webhook delivery job ran before Module.Attach")
	}
	return h.module.service.handleDeliveryJob(ctx, job)
}

// OnFailure implements jobs.FailureHook.
func (h webhookDeliveryHandler) OnFailure(ctx context.Context, job *jobs.Job, cause error) {
	if h.module.service == nil {
		return
	}
	h.module.service.onWebhookDeliveryDeadLetter(ctx, job, cause)
}

// compile-time checks that webhookDeliveryHandler satisfies both jobs
// interfaces it is registered for.
var (
	_ jobs.Handler     = webhookDeliveryHandler{}
	_ jobs.FailureHook = webhookDeliveryHandler{}
)

// Attach builds and returns the runtime Service. It must be called exactly
// once, after Kernel.Bootstrap has returned, with the registry Bootstrap
// produced -- the same convention go/rbac's Attach documents, for the same
// reason: only after Bootstrap has every module registered is
// reg.Events.Bus() and reg.AuditActions wired to the real, final set every
// other module contributed.
//
// A second Attach on the same Module fails with ErrAlreadyAttached.
func (m *Module) Attach(reg *pkgcore.Registry) (*Service, error) {
	if m.service != nil {
		return nil, ErrAlreadyAttached
	}
	if reg == nil {
		return nil, errors.New("integration: Attach requires a non-nil *pkgcore.Registry (pass the registry Kernel.Bootstrap returned)")
	}
	if m.db == nil {
		return nil, errors.New("integration: Attach requires the database NewModule was built with (its db argument must not be nil)")
	}

	clock := m.clock
	if clock == nil {
		clock = time.Now
	}

	svc := &Service{
		repo:         NewAPIKeyRepository(m.db),
		permissions:  m.permissions,
		membership:   m.membership,
		bus:          reg.Events.Bus(),
		auditActions: reg.AuditActions,
		now:          clock,

		webhookRepo:  NewWebhookSubscriptionRepository(m.db),
		deliveryRepo: NewWebhookDeliveryRepository(m.db),
		queue:        m.queue,
		mappings:     m.mappingIndex,
		httpClient:   m.httpClient,
		urlValidator: m.urlValidator,
	}

	m.service = svc
	return svc, nil
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
