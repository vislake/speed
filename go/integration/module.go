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

//go:embed api/openapi.yaml
var openAPISpecYAML []byte

// apiPath is the common prefix this module's HTTP routes are mounted at --
// the API-key operations and the webhook-subscription CRUD, recent-
// deliveries listing and restore alike share the one mount (see Register
// below). It must agree with the "paths:" keys of this module's
// own OpenAPI fragment (api/openapi.yaml): every one of them starts with
// this prefix, and Handler's inner mux (built by api.HandlerFromMux, see
// handler.go) registers each spec path as an ABSOLUTE net/http pattern --
// mounting at apiPath here only tells the host's outer mux which requests
// to hand to Handler at all, exactly as org's and storage's identical
// apiPath constants do for their own routes.
const apiPath = "/api/v1/integration"

// moduleName is integration's pkgcore.Module.Name(). It is also the module
// name dbkit.MigrationRegistry.Register keys its dependency graph on.
const moduleName = "integration"

// MaxAPIKeyLifetime is both the forced expiry ceiling Service.Create
// enforces on every request (ErrExpiryExceedsMaximum past it) and the
// default it applies when a request names no ExpiresAt at all, per the
// design's forced-expiry-ceiling rule (defaulting to one year) -- the
// ceiling and the unspecified-request default are declared as the same one
// year, not two numbers that happen to agree.
//
// It is the lifetime every Service uses unless its host configured a
// different one through WithMaxAPIKeyLifetime: keeping the value a plain
// package constant (rather than a go/config item) avoids a dependency edge
// this module's position in the module graph does not have, paid for by
// every consumer that boots this module without config -- and the
// host-configurable override is an ordinary NewModule option, exactly like
// every other seam in this file.
const MaxAPIKeyLifetime = 365 * 24 * time.Hour

// Permission strings this module declares for its API-key and webhook-
// subscription management surfaces. Like go/rbac's own PermissionRead/
// PermissionManage, this module does not check them itself -- it declares
// the vocabulary; enforcement is whatever authorization layer the host
// wires in front of the HTTP surface.
const (
	// PermissionRead covers listing a tenant's API keys.
	PermissionRead = "integration:apikey:read"

	// PermissionManage covers creating, rotating and revoking API keys.
	PermissionManage = "integration:apikey:manage"

	// PermissionWebhookRead covers listing a tenant's webhook subscriptions
	// and their delivery log -- the webhook counterpart of PermissionRead,
	// following the identical "integration:<entity>:<verb>" naming
	// convention.
	PermissionWebhookRead = "integration:webhook:read"

	// PermissionWebhookManage covers creating, updating and deleting
	// webhook subscriptions -- the webhook counterpart of PermissionManage.
	PermissionWebhookManage = "integration:webhook:manage"
)

// Audit actions this module contributes, following the
// "<module>.<entity>.<action>" convention. Service calls audit.Emit under
// each of these after the corresponding mutation commits; see service.go.
const (
	// AuditActionAPIKeyCreate is emitted after Service.Create persists a
	// new key.
	AuditActionAPIKeyCreate = "integration.apikey.create"

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
	// Service.DeleteWebhookSubscription mark-deletes a subscription.
	AuditActionWebhookSubscriptionDelete = "integration.webhook_subscription.delete"

	// AuditActionWebhookSubscriptionRestore is emitted after
	// Service.RestoreWebhookSubscription undoes a mark-delete -- the
	// symmetric counterpart AuditActionWebhookSubscriptionDelete's own
	// adoption of dbkit.SoftDeletable makes possible, following go/org's
	// and go/rbac's identical Create/Update/Delete-plus-Restore audit
	// symmetry.
	AuditActionWebhookSubscriptionRestore = "integration.webhook_subscription.restore"
)

// auditActionDecls is every audit action this module declares through
// Register, kept as one slice so Register and any test enumerating "every
// action this module contributes" read from a single place.
//
// No AuditActionAPIKeyRotate appears here -- deliberately: Service.Rotate
// records a rotation as its two ordinary create/revoke audit events (see
// that method's own doc comment for why a bespoke rotate event would be
// worse), so a rotate action registered here would be a vocabulary entry
// nothing ever emits -- a dead registration (the same "declaring a code
// for a feature that does not exist is a lying vocabulary" rule errors.go's
// own doc comment applies to error codes).
var auditActionDecls = []string{
	AuditActionAPIKeyCreate,
	AuditActionAPIKeyRevoke,
	AuditActionWebhookSubscriptionCreate,
	AuditActionWebhookSubscriptionUpdate,
	AuditActionWebhookSubscriptionDelete,
	AuditActionWebhookSubscriptionRestore,
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

	// subject is the optional SubjectResolver seam (see seams.go and
	// WithSubjectResolver), read only by Handler's two request-body
	// operations whose payload carries a creator: integration_createAPIKey
	// and integration_createWebhookSubscription. Nil is
	// legal: those two operations then fail closed with
	// ErrSubjectUnresolved rather than guessing a creator; every other
	// operation this fragment mounts is unaffected (see Handler's own doc
	// comment for why only the two Creates need a caller identity at all).
	subject SubjectResolver

	// clock is the injectable time source tests override through
	// withClock; production always uses the zero value's time.Now.
	clock func() time.Time

	// maxLifetime is the expiry ceiling and default lifetime the Service
	// Attach builds applies to every API key it issues -- the
	// WithMaxAPIKeyLifetime option's value. Zero means the host configured
	// nothing and the MaxAPIKeyLifetime package default stands (see that
	// constant's own doc comment for why the ceiling and the default are
	// one number).
	maxLifetime time.Duration

	// The fields below back the outbound-webhook surface. See
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

	// authGuard is the optional rate-limit guard WithAuthenticationGuard
	// wired, applied by AuthMiddleware to authentication attempts BEFORE
	// any Authenticate call (see that option's own doc comment for the
	// layering argument). Nil when unset -- every Module built without the
	// option. AuthMiddleware consumes it when a host calls Middleware(next)
	// and the guarded handler chain is BUILT: the guard wraps the
	// authenticate gate at that moment (middleware.go), and a request SERVED
	// through the returned chain never re-reads the field. That is a
	// deliberate contrast with this Module's genuinely per-request reads --
	// m.service (nil until Attach, which runs after the Register-time chain
	// build, so the per-request closure resolves it only once a request
	// actually arrives; see Register's own "Handler is built here, not in
	// Attach" section) and the event-mapping index (which mapping a
	// delivered event takes is runtime data, knowable only per event, so
	// webhook_delivery.go consults s.mappings.byInternal[evt.Type] on every
	// delivery). authGuard's whole effect is the composition it produced
	// when the chain was built; once Middleware has wrapped the guard around
	// authenticate, nothing a later request could re-read would recompose
	// that already-assembled chain, which is why the field is read exactly
	// once, at build time. There is deliberately no setter that could make a
	// post-construction change look effective.
	authGuard *HTTPGuard

	// service is the Service Attach produced, nil until then. It is what
	// makes a second Attach detectable, and it is what Module's own
	// forwarding wrappers (handleDomainEvent below, webhookDeliveryHandler,
	// and the Handler -- see handler.go's own "Built differently from
	// every other module's Handler" doc comment) read once Attach has run.
	service *Service

	// handler is the spec-generated HTTP surface over the API-key and
	// webhook-subscription operations, built during Register (see
	// Register's own doc comment for why it can be built there even though
	// m.service cannot).
	handler *Handler
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

// WithSubjectResolver wires the host's caller-identity seam for the HTTP
// surface -- read only by Handler's integration_createAPIKey and
// integration_createWebhookSubscription operations (see SubjectResolver's
// own doc comment in seams.go). Optional: a Module built with none simply
// fails those two operations closed with ErrSubjectUnresolved rather than
// guessing a creator; the Service-level APIs (used directly, with no
// Handler in front of them) are entirely unaffected either way.
func WithSubjectResolver(r SubjectResolver) Option {
	return func(m *Module) { m.subject = r }
}

// withClock overrides the Service's time source. Unexported: it exists for
// this module's own tests (expiry-ceiling and rotation-timing assertions
// that would otherwise race the wall clock), never for a host to call.
func withClock(now func() time.Time) Option {
	return func(m *Module) { m.clock = now }
}

// WithMaxAPIKeyLifetime sets the lifetime that governs every API key this
// module issues: the forced expiry CEILING Service.Create enforces on a
// request that asks for a lifetime (ErrExpiryExceedsMaximum past it) and,
// at the same time, the DEFAULT lifetime a request that names no ExpiresAt
// at all receives -- deliberately one number serving both roles, the same
// invariant MaxAPIKeyLifetime's own doc comment states for the package
// default it replaces. A host that needs, say, a 30-day ceiling instead of
// the one-year default configures it here, at construction, where a
// reviewer sees it; a host that configures nothing gets the
// MaxAPIKeyLifetime default unchanged.
//
// Non-positive values are ignored and the default stands, mirroring the
// guard every scalar With* option in this codebase carries (storage's
// WithMaxObjectLifetime, WithUploadTTL): a ceiling of zero or less is
// nonsense, and a value nobody can configure away by accident is a value
// enforcement can trust.
func WithMaxAPIKeyLifetime(lifetime time.Duration) Option {
	return func(m *Module) {
		if lifetime > 0 {
			m.maxLifetime = lifetime
		}
	}
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

// WithAuthenticationGuard wires the rate-limit guard AuthMiddleware applies
// to authentication attempts BEFORE it authenticates anything -- the
// "rate-limit before auth" ordering (middleware.go's own doc comment): a
// request presenting a forged or unrecognized X-API-Key header otherwise
// reaches Service.Authenticate's lookups with no bound at all, because the
// module's HTTPGuard is mounted behind authentication in the classic
// composition and never sees a request that failed to authenticate. A guard
// wired here sits OUTSIDE the authenticate step: it sees every request the
// middleware does, and its three layers are charged before -- and decide
// whether there even is -- an Authenticate call.
//
// The guard's own Extractor runs before any authentication has happened, so
// it cannot read tenant/key identifiers out of request context the way the
// post-auth composition's extractor can (middleware.go's own doc comment
// describes that shape). The module takes no position on what identifiers a
// host derives pre-auth -- LayeredLimiter.Allow treats the empty string as
// an ordinary key, so an extractor returning empty identifiers for a
// not-yet-authenticated request charges the global layer and the shared
// anonymous counters of the tenant/key layers, which is the usual answer
// for anonymous floods; a host may also derive an identifier from the
// request itself (for example the presented key's hash, or a client IP --
// authn's login-limit precedent dimensions its own attempt limits per
// identifier and per IP). Whatever the extractor returns, a request the
// guard denies answers 429 and never reaches Authenticate.
//
// Wired or not, AuthMiddleware's own behavior is unchanged for a Module
// built without this option.
func WithAuthenticationGuard(guard *HTTPGuard) Option {
	return func(m *Module) { m.authGuard = guard }
}

// WithWebhookQueue injects the jobs.Queue this module's jobs tasks are
// enqueued on and executed by: webhook deliveries (handleDomainEvent,
// webhook_delivery.go) and the API-key expiry-sweep task
// (Service.EnqueueAPIKeyExpirySweep, apikey_sweep.go), both handlers
// registered on reg.Jobs during Register. Unlike WithPermissionLister,
// an unwired queue does NOT fail Register or Attach: a host that has not
// wired jobs yet can still boot this module and manage subscriptions
// through Service's Create/List/Update/Delete surface -- only the enqueue
// steps are affected, and they treat a nil queue explicitly:
// handleDomainEvent's own enqueue treats it as "record the delivery, warn,
// and stop" rather than a hard failure (see enqueueDelivery's own doc
// comment), the identical resilience posture handleDomainEvent itself
// follows for every other failure a domain-event subscriber can hit --
// while the two host-invoked schedule points,
// Service.RedeliverWebhookDelivery and Service.EnqueueAPIKeyExpirySweep,
// answer with a plain error naming the missing wiring, since a caller that
// explicitly asked for a delivery or a sweep must learn that nothing can
// run, never silently no-op.
func WithWebhookQueue(queue jobs.Queue) Option {
	return func(m *Module) { m.queue = queue }
}

// WithWebhookURLValidator overrides ValidateWebhookURL for
// Service.CreateWebhookSubscription/UpdateWebhookSubscription's
// creation/update-time SSRF check.
//
// # A deliberate, additive escape hatch for a test or demo host that
// # explicitly opts in -- never a production weakening
//
// A Module built with no WithWebhookURLValidator option gets EXACTLY the
// same behavior this module has always had: ValidateWebhookURL genuinely
// refuses loopback, private, link-local and CGNAT addresses, with no other
// option in this package able to relax that. This seam exists because the
// repository has no way to stand up a "real" receiver an offline test can
// prove a genuine signed HTTP delivery against without hitting exactly the
// class of address SSRF protection exists to refuse: an httptest.Server
// listens on loopback, and a sibling container reached the same way this
// repository's OTHER Docker-backed integration tiers reach theirs (a
// testcontainers-mapped port, or a container-to-container address on a
// Docker bridge network) is either loopback or RFC 1918 private space
// either way. Relaxing this one
// check for one Module instance a test or demo process builds for itself
// is the only way to get a real round trip against a receiver that process
// controls -- mirroring the identical shape pkgcore's own
// WithHTTPSMSSenderClient (pkgcore/sms_http.go) already established
// for its SSRF-guarded SMS gateway client, for the same reason.
//
// This override MUST be wired together with WithWebhookHTTPClient, never
// alone: overriding only this creation-time check while leaving
// newSafeHTTPClient's dial-time re-check in place would simply move the
// refusal from subscription creation to the first delivery attempt, and
// overriding only the HTTP client while leaving this check in place would
// refuse the subscription before a delivery is ever attempted.
//
// Never call this from a production host's own composition. It exists for
// a test or demo process proving delivery against a receiver of its own,
// and every production deployment must leave it unset.
func WithWebhookURLValidator(validate func(ctx context.Context, url string) error) Option {
	return func(m *Module) { m.urlValidator = validate }
}

// WithWebhookHTTPClient overrides the http.Client webhook delivery attempts
// send through (webhook_delivery.go's attemptDelivery), in place of
// newSafeHTTPClient's SSRF-guarded default. See WithWebhookURLValidator's own
// doc comment for why this seam exists, why it must always be wired
// together with WithWebhookURLValidator, and why a production host must
// never call it.
func WithWebhookHTTPClient(client *http.Client) Option {
	return func(m *Module) { m.httpClient = client }
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

// OpenAPISpec implements pkgcore.Module: this module's OpenAPI fragment,
// embedded from api/openapi.yaml, covering the API-key surface
// (Create/List/Rotate/Revoke) and the webhook-subscription management
// surface (Create/List/Update/Delete/Restore plus the recent-deliveries
// listing) on one shared api.ServerInterface. The fragment is the single
// source of this module's HTTP surface -- the api package's generated
// types and ServerInterface (api/integration-server.gen.go, regenerated by
// task api:gen) derive from it, and Handler implements that interface (see
// handler.go) -- so a spec change without a matching handler change cannot
// compile.
func (m *Module) OpenAPISpec() []byte { return openAPISpecYAML }

// Register implements pkgcore.Module. Per the interface's own contract
// ("It must not perform I/O; it only declares"), it declares this module's
// permission vocabulary and its audit actions, builds the event-mapping
// index the WithEventMapping declarations feed, subscribes to every
// distinct InternalType that index names (reg.Events.Subscribe performs no
// I/O of its own -- it only registers a callback for later), registers this
// module's two job handlers on reg.Jobs (the webhook-delivery handler and
// the API-key expiry-sweep handler, see webhook_delivery.go and
// apikey_sweep.go), and mounts the module's HTTP surface (the API-key and
// webhook-subscription operations the OpenAPISpec doc comment above lists).
// It touches neither the database nor the network.
//
// # Handler is built here, not in Attach
//
// Every OTHER module with an HTTP surface (org, storage, notification)
// builds its Handler here from services that already exist -- those
// modules build their Service in NewModule, not Attach. go/integration's own
// Service is built in Attach instead (this module's own established
// shape), which runs strictly after Bootstrap returns --
// too late to build a route reg.Routes.Mount needs NOW, during Register.
// Handler is therefore built here holding a reference to m itself (never to
// m.service, which does not exist yet) and reads m.service AT CALL TIME,
// exactly the forwarding-wrapper technique handleDomainEvent and
// webhookDeliveryHandler already use immediately below for the identical
// reason -- see handler.go's own "Built differently from every other
// module's Handler" doc comment for the full argument.
func (m *Module) Register(reg *pkgcore.ComponentRegistry) error {
	if err := reg.PermissionsSeat().Add(PermissionRead, PermissionManage, PermissionWebhookRead, PermissionWebhookManage); err != nil {
		return err
	}
	if err := reg.AuditActionsSeat().Add(auditActionDecls...); err != nil {
		return err
	}

	idx, err := buildEventMappingIndex(m.eventMappings)
	if err != nil {
		return err
	}
	m.mappingIndex = idx
	for internalType := range idx.byInternal {
		reg.EventsSeat().Subscribe(internalType, m.handleDomainEvent)
	}

	if err := reg.JobsSeat().Handle(jobTypeWebhookDeliver, webhookDeliveryHandler{module: m}); err != nil {
		return err
	}
	if err := reg.JobsSeat().Handle(jobTypeAPIKeyExpirySweep, apiKeyExpirySweepHandler{module: m}); err != nil {
		return err
	}
	// Declare the API-key expiry sweep's periodic schedule alongside its
	// handler: declaring means scheduled, so a host that runs a
	// jobs.Scheduler over the registry's declarations sweeps every
	// tenant's expired keys at the module's own window cadence without
	// writing a schedule point of its own. The declaration follows the
	// handler registration unconditionally, the same way the handler
	// itself does -- no declaration may outlive its executor.
	if err := reg.SchedulesSeat().Add(apiKeyExpirySweepSchedule); err != nil {
		return err
	}

	m.handler = NewHandler(m, m.subject)
	reg.RoutesSeat().Mount(apiPath, m.handler)
	return nil
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
func (m *Module) Attach(reg *pkgcore.ComponentRegistry) (*Service, error) {
	if m.service != nil {
		return nil, ErrAlreadyAttached
	}
	if reg == nil {
		return nil, errors.New("integration: Attach requires a non-nil *pkgcore.ComponentRegistry (pass the registry the assembly drove)")
	}
	if m.db == nil {
		return nil, errors.New("integration: Attach requires the database NewModule was built with (its db argument must not be nil)")
	}

	clock := m.clock
	if clock == nil {
		clock = time.Now
	}

	// A Module with no WithWebhookHTTPClient override delivers through the
	// module-level default client, built once at package init
	// (webhook_guard.go's defaultWebhookHTTPClient) rather than per delivery
	// attempt -- see that var's doc comment for why every delivery sharing
	// one transport matters.
	httpClient := m.httpClient
	if httpClient == nil {
		httpClient = defaultWebhookHTTPClient
	}

	svc := &Service{
		repo:         NewAPIKeyRepository(m.db),
		permissions:  m.permissions,
		membership:   m.membership,
		bus:          reg.EventBus(),
		auditActions: reg.AuditActionsSeat(),
		now:          clock,
		maxLifetime:  m.maxLifetime,

		webhookRepo:  NewWebhookSubscriptionRepository(m.db),
		deliveryRepo: NewWebhookDeliveryRepository(m.db),
		queue:        m.queue,
		mappings:     m.mappingIndex,
		httpClient:   httpClient,
		urlValidator: m.urlValidator,
	}

	m.service = svc
	return svc, nil
}
