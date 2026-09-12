package pkgcore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// ErrDuplicateConfigKey is returned when two modules register the same configuration key.
var ErrDuplicateConfigKey = errors.New("pkgcore: duplicate config key")

// ErrInvalidConfigItem is returned when a registered configuration item's
// fields contradict one another: an unknown Type, a Default, Min or Max of
// the wrong Go type, Min or Max on a string or bool item, Min above Max, a
// Default outside the item's declared range, or Sensitive and Public both
// set. Such a declaration could never be stored or served coherently, so it
// is rejected at registration rather than surfacing later as a runtime
// failure. Nothing is registered when the call returns this error.
var ErrInvalidConfigItem = errors.New("pkgcore: invalid config item")

// ErrDuplicateFeatureFlag is returned when two modules register the same feature flag key.
var ErrDuplicateFeatureFlag = errors.New("pkgcore: duplicate feature flag")

// ErrDuplicatePermission is returned when the same permission string is registered twice.
var ErrDuplicatePermission = errors.New("pkgcore: duplicate permission")

// ErrDuplicateJobType is returned when two modules register a handler for the same job type.
var ErrDuplicateJobType = errors.New("pkgcore: duplicate job type")

// ErrDuplicateNotificationType is returned when the same notification type key is registered twice.
var ErrDuplicateNotificationType = errors.New("pkgcore: duplicate notification type")

// ErrDuplicateEventType is returned when two modules declare that they publish
// the same domain event type.
var ErrDuplicateEventType = errors.New("pkgcore: duplicate published event type")

// ErrDuplicateAuditAction is returned when the same audit action is registered twice.
var ErrDuplicateAuditAction = errors.New("pkgcore: duplicate audit action")

// ErrDuplicateRetentionParticipant is returned when two RetentionParticipant
// registrations share the same Name.
var ErrDuplicateRetentionParticipant = errors.New("pkgcore: duplicate retention participant")

// ErrDuplicatePeriodicTask is returned when two PeriodicTask registrations
// share the same Type. Two modules owning one task type is a bug rather
// than a merge -- the type selects the handler that runs the task, and a
// single type with two schedules would enqueue one queue row for two
// modules' work.
var ErrDuplicatePeriodicTask = errors.New("pkgcore: duplicate periodic task type")

// ErrInvalidPeriodicTask is returned when a registered periodic task's
// fields contradict one another: an empty Type, a non-positive Every, an
// unknown Scope, an empty KeyPrefix, a Platform-scope declaration without
// its PlatformTenant sentinel, or a PerTenant declaration carrying one. A
// declaration shaped like that could never be enqueued coherently -- an
// empty type would dead-letter every window, a zero window would widen the
// key every tick, a scope typo would leave the declaration silently
// unscheduled -- so it is refused at registration rather than accepted and
// discovered at tick time. Nothing is registered when the call returns
// this error.
var ErrInvalidPeriodicTask = errors.New("pkgcore: invalid periodic task")

// ErrNilMiddleware is returned when a nil middleware is added to the
// Middleware seat. A nil entry could never wrap a handler -- the chain that
// applies it would panic at assembly time -- so it is refused at
// registration rather than surfacing later as a startup crash. Nothing is
// registered when the call returns this error.
var ErrNilMiddleware = errors.New("pkgcore: nil middleware")

// ErrNilRetentionSweep is returned when a RetentionParticipant is registered
// without its Sweep callback. Sweep is mandatory at registration: the
// retention sweep calls each registered participant's Sweep once per
// (tenant, participant) pair, so a participant whose Sweep were nil could
// never release its own soft-deleted rows -- its tenant data would be
// retained forever while the sweep reported success. Such a declaration is
// refused at registration rather than accepted and silently skipped at
// sweep time. Nothing is registered when the call returns this error.
var ErrNilRetentionSweep = errors.New("pkgcore: retention participant has no Sweep callback")

// ErrNilRetentionErase is returned when a RetentionParticipant is
// registered without its Erase callback. Erase is mandatory at
// registration, exactly as Sweep is: the erasure orchestration calls each
// registered participant's Erase once per right-to-erasure request, so a
// participant whose Erase were nil would leave its rows for the subject
// untouched while the request answered full success to the data subject --
// a silently incomplete answer to a legally-deadlined obligation, the
// right-to-erasure twin of the false success a nil Sweep would produce for
// a retention sweep. Such a declaration is refused at registration rather
// than accepted and silently skipped at erasure time. Nothing is
// registered when the call returns this error.
var ErrNilRetentionErase = errors.New("pkgcore: retention participant has no Erase callback")

// ErrDependencyCycle is returned when the dependency graph the assembly
// resolves -- the Requires edges among the selected components -- forms a
// cycle. The error names the cycle path.
var ErrDependencyCycle = errors.New("pkgcore: dependency cycle")

// ErrUnresolvedFeatureDependency is returned when a feature flag depends on a flag nobody registered.
var ErrUnresolvedFeatureDependency = errors.New("pkgcore: unresolved feature flag dependency")

// ErrCapabilityUnsatisfied is returned by the assembly's Prepare stage when a
// selected component does not declare every Capability the composition's
// deployment mode requires. The error names the component, the missing
// capability bits and the mode, so a misdeclared implementation is
// identifiable at startup rather than at first use.
var ErrCapabilityUnsatisfied = errors.New("pkgcore: implementation does not satisfy the deployment mode's required capability")

// errNilFeatureRegistrar guards ValidateFeatureGraph against a nil seat.
var errNilFeatureRegistrar = errors.New("pkgcore: no feature registrar to validate")

// MountedRoute is one HTTP handler mounted at a path by a module.
type MountedRoute struct {
	// Path is the prefix the handler was mounted at, for example "/api/v1/billing".
	Path string
	// Handler serves every request below Path.
	Handler http.Handler
	// Access is the authorization decision recorded for the route: whether
	// it is deliberately public, or which permission each request must
	// hold. A module's own mount records none -- the decision belongs to
	// the host that wires the authorization layer -- and the route table
	// that enforces complete coverage (rbac.GuardRoutes) fills it on the
	// routes it returns, so every served route carries the decision it was
	// admitted under. See RouteAccess for the declaration's exact contract.
	Access RouteAccess
}

// RouteAccess declares the authorization decision for one mounted route:
// either the route is deliberately public, or every request to it must hold
// the permission Permission selects.
//
// The zero value declares nothing, and a route built from zero is served
// only by a path that does not enforce decisions. The route table a host
// wires into rbac.GuardRoutes refuses it at startup rather than admitting
// the route with no decision: an explicit Public declaration and an omitted
// one must never look alike, because only the first is an answer.
//
// Exactly one of the two forms must be declared. A decision setting both is
// contradictory -- no check could honor "public and permission-gated" at
// once -- and is refused where the table is validated, as is a public
// decision carrying a subject resolver or an exemption: both only take
// effect inside a permission check the public form never performs.
type RouteAccess struct {
	// Public declares the route deliberately reachable without a permission
	// check: an anonymous surface by design (a login page's public
	// configuration, a share-link page), or a route whose handler carries
	// its own per-operation identity check. It is a positive declaration,
	// never a default -- the zero RouteAccess is refused, not treated as
	// public.
	Public bool

	// Permission selects the permission string each request to the route
	// must hold, in the canonical "<resource>:<action>" form
	// (rbac.Permission composes it). A module with more than one gated
	// entity declares three-segment names ("<module>:<entity>:<verb>", for
	// example "integration:apikey:read"), which rbac.SplitPermission reads
	// at the last separator: the resource half is everything before it
	// ("integration:apikey"), the action the last segment.
	//
	// It is a function rather than one fixed string because a route's
	// required permission routinely varies by the request's own shape: a
	// whole-module mount serving reads and writes selects the read
	// permission on GET/HEAD and the write permission otherwise. It must be
	// a pure function of the request's ROUTE -- method and path -- never of
	// a header, query parameter or body field, because a permission the
	// caller controls is one the caller can choose. Returning anything that
	// is not a well-formed permission denies the request. Nil when the
	// route is public.
	Permission RoutePermission
}

// RoutePermission selects the permission a request to a route must hold,
// per RouteAccess.Permission's contract.
type RoutePermission func(*http.Request) string

// ConfigItem describes a single configuration key contributed by a module.
// The admin console form and the generated configuration reference are both
// derived from these declarations, so Description is not optional in practice.
//
// Declarations are validated when they are registered (see
// ConfigSchemaRegistrar.Add): the fields must describe one coherent value,
// or the whole registration call is rejected. Min and Max interpret the
// declared Type's ordering -- int and duration values are comparable, string
// and bool values are not -- and a Default outside its declared range cannot
// be registered.
type ConfigItem struct {
	// Key is the dotted configuration key, for example "billing.invoice_retry_limit".
	Key string
	// Type names the value type. The set is closed: "string", "int", "bool"
	// or "duration".
	Type string
	// Default is the value used when neither the environment nor a tenant
	// overrides the key. Nil is legal and means the module serves no value
	// until one is set; when present, it must be a Go value of the declared
	// Type's kind (string for "string", int or int64 for "int", bool for
	// "bool", time.Duration for "duration").
	Default any
	// Sensitive marks secrets, which are redacted in logs, in the admin
	// console and in the docs. A sensitive item is never served on a public
	// endpoint, so Sensitive and Public cannot both be true.
	Sensitive bool
	// Description is the English text shown in the admin console and the configuration reference.
	Description string
	// Group buckets related keys together in the admin console form and the
	// configuration reference. Free-form; modules conventionally use their
	// own name (for example "billing").
	Group string
	// Public marks values that the unauthenticated public configuration
	// endpoint serves to a resolved tenant (brand name, login methods,
	// announcement copy). Never set together with Sensitive.
	Public bool
	// Min is the smallest value a configuration write may set, interpreted
	// per Type. Only int and duration items may declare it. Nil means no
	// lower bound.
	Min any
	// Max is the largest value a configuration write may set, interpreted
	// per Type. Only int and duration items may declare it. Nil means no
	// upper bound.
	Max any
}

// FeatureFlag describes a feature toggle contributed by a module.
type FeatureFlag struct {
	// Key is the flag identifier, for example "billing.dunning".
	Key string
	// Default is the value used when no tenant override exists.
	Default bool
	// Description is the English text shown in the admin console.
	Description string
	// DependsOn lists the Key of every flag that must be enabled for this
	// one to have an effect. ValidateFeatureGraph resolves these.
	DependsOn []string
}

// NotificationType describes one kind of notification a module can emit.
// The user-facing notification preference matrix is rendered from these.
type NotificationType struct {
	// Key is the notification identifier, for example "billing.invoice_paid".
	Key string
	// Group buckets related notification types together in the preference matrix.
	Group string
	// DefaultChannels lists the delivery channels used when the recipient has no preference.
	DefaultChannels []string
	// RecipientVisibleParams names the parameters a dispatch of this type may
	// carry to its recipient: the keys of the interpolation values the type's
	// own templates reference (see notification's Dispatch.Params). Anything
	// NOT named here is delivery-internal context that must never reach the
	// recipient, and notification enforces that on the two boundaries it
	// owns: DeliveryService.Dispatch refuses a dispatch carrying any
	// parameter outside this list, and the delivery path narrows any payload
	// that nevertheless reaches it (a job enqueued before this declaration
	// existed, say) down to this list before rendering copy or persisting
	// the inbox row. An EMPTY list is a real declaration: the type's copy is
	// not parameterized, so its dispatches may carry no parameters at all.
	//
	// Nil is the legacy value meaning "no restriction declared": a type that
	// predates the annotation keeps accepting any parameters, byte for byte
	// as before. Every newly declared or edited type should state its list
	// explicitly -- the empty list included -- so the recipient-visible
	// surface is decided by declaration, never by whatever a dispatch
	// happens to carry.
	RecipientVisibleParams []string
	// Unsubscribable reports whether recipients may opt out. Transactional
	// notifications such as verification codes are not unsubscribable.
	Unsubscribable bool
}

// EventDecl declares one domain event a module publishes. The declarations
// form the catalog integration maps onto its versioned public event schema,
// and they let observability and compliance enumerate which domain facts exist
// without subscribing to each of them first.
type EventDecl struct {
	// Type is the routing key the event is published under, following
	// <module>.<entity>.<action>, for example "billing.invoice.paid".
	Type string
	// PayloadType names the concrete type carried in Event.Payload, for
	// example "billing.InvoicePaid", so that a subscriber knows what to
	// type-assert and the public event schema knows what to map.
	PayloadType string
	// Description is the English text shown in the generated event catalog.
	Description string
}

// RouteRegistrar collects the HTTP handlers modules mount.
type RouteRegistrar interface {
	// Mount attaches handler to path. Duplicate paths are not rejected here
	// because the routing implementation decides how it resolves overlaps.
	Mount(path string, handler http.Handler)
	// Routes returns every route mounted so far, in mount order.
	Routes() []MountedRoute
}

// MiddlewareRegistrar collects the platform-wide middleware components
// declare for the assembled chain's outermost layer.
//
// It serves the one need a component's own route-subtree middleware cannot:
// a middleware that must wrap EVERY request the assembled chain handles,
// including requests the platform's own authn or tenancy layers will refuse
// before any route is reached. A component that needs to wrap only the
// routes it mounts itself does not use this seat: net/http.Handler composes
// directly, so the component wraps its own handler before calling
// reg.Routes.Mount. This seat exists for the middlewares that must stand
// outside the fixed chain itself.
//
// # Boundary: outside the fixed chain, never inside it
//
// The platform's fixed order -- go/app/chain's Chain: authn.Middleware
// outermost, the AdminRoutes and AuthnRoutes branches split out
// structurally, then tenancy.Middleware with its pre-auth allowlist and the
// protected face -- is not reachable through this seat. MiddlewareRegistrar
// offers no way to insert inside that order: the middleware registered here
// is applied only by go/app/chain.Standard, which wraps it around the
// finished chain.Chain output, so a registered middleware is the OUTERMOST
// layer of everything, authn included. A host that composes its handler
// without Standard (a direct Chain call, or no chain at all) applies this
// seat's middleware itself if it wants the layer.
//
// A middleware standing at that layer runs outside authentication: it
// receives the raw, unauthenticated request, before any authn.Principal or
// tenant context exists. It must therefore do stateless,
// request-content-insensitive bypass work only -- tracing, metrics, panic
// recovery. A component that reads tenant or identity information here is
// misusing the seat, and code review rejects it.
type MiddlewareRegistrar interface {
	// Add registers middleware, applied in registration order: the first
	// added wraps outermost. A nil entry is rejected with an error wrapping
	// ErrNilMiddleware, because it could never wrap a handler; nothing is
	// registered when the call returns an error.
	Add(mw ...func(http.Handler) http.Handler) error
	// Middlewares returns every registered middleware, in registration order.
	Middlewares() []func(http.Handler) http.Handler
}

// ConfigSchemaRegistrar collects the configuration schema modules declare.
//
// The items registered here are values editable at runtime -- per-tenant rows
// in the configs table, served and changed while the process runs. Keys a
// process resolves once at startup belong to the separate bootstrap layer
// (pkgcore.BootstrapKey, the component descriptor's BootstrapKeys field);
// one dotted key belongs to exactly one of the two layers, never both.
type ConfigSchemaRegistrar interface {
	// Add registers configuration items, validating every declaration
	// first. An item whose fields contradict one another -- an unknown
	// Type, a Default, Min or Max of the wrong Go type, Min or Max on a
	// string or bool item, Min above Max, a Default outside the declared
	// range, or Sensitive and Public both set -- is rejected with an error
	// wrapping ErrInvalidConfigItem, because such a declaration could never
	// be stored or served coherently. A key already registered (by an
	// earlier call, or twice within this one) is rejected with an error
	// wrapping ErrDuplicateConfigKey, because two modules owning one key is
	// a bug rather than a merge. Nothing is registered when the call
	// returns an error.
	Add(items ...ConfigItem) error
	// Items returns every configuration item registered so far, in registration order.
	Items() []ConfigItem
}

// FeatureRegistrar collects the feature flags modules declare.
type FeatureRegistrar interface {
	// Add registers feature flags. It returns an error wrapping
	// ErrDuplicateFeatureFlag on a repeated Key, because one flag owned by
	// two modules leaves its default value decided by registration order.
	// Nothing is registered when the call returns an error.
	//
	// Flag dependencies are deliberately not resolved here, because a module
	// may legitimately depend on a flag owned by a module that registers
	// later; ValidateFeatureGraph checks the whole graph once every module
	// has registered.
	Add(flags ...FeatureFlag) error
	// Flags returns every feature flag registered so far, in registration order.
	Flags() []FeatureFlag
}

// PermissionRegistrar collects the resource:action permissions modules define.
type PermissionRegistrar interface {
	// Add registers permission strings. It returns an error wrapping
	// ErrDuplicatePermission if one was already registered, since a
	// duplicate is almost always a copy-paste mistake across modules.
	// Nothing is registered when the call returns an error.
	Add(perms ...string) error
	// Permissions returns every registered permission, sorted.
	Permissions() []string
}

// JobHandlerRegistrar collects the asynchronous job handlers modules provide.
type JobHandlerRegistrar interface {
	// Handle registers handler for jobType. The handler is typed as any
	// because the jobs module, which owns the real handler interface, sits
	// above pkgcore in the dependency graph and cannot be referenced here.
	// It returns an error wrapping ErrDuplicateJobType on a repeated jobType.
	Handle(jobType string, handler any) error
	// Handlers returns every registered handler keyed by job type.
	Handlers() map[string]any
}

// NotificationRegistrar collects the notification types modules can emit.
type NotificationRegistrar interface {
	// Add registers notification types. It returns an error wrapping
	// ErrDuplicateNotificationType on a repeated Key. Nothing is registered
	// when the call returns an error.
	Add(types ...NotificationType) error
	// Types returns every notification type registered so far, in registration order.
	Types() []NotificationType
}

// EventRegistrar collects the domain events modules publish and the
// subscriptions they install on the shared bus.
type EventRegistrar interface {
	// Publishes declares the domain events the calling module emits. It
	// returns an error wrapping ErrDuplicateEventType on a repeated Type,
	// because exactly one module owns each event type. Nothing is registered
	// when the call returns an error. Declaring an event is a documentation
	// and mapping contract, not a precondition for publishing it.
	Publishes(events ...EventDecl) error
	// Published returns every declared domain event, in declaration order.
	Published() []EventDecl
	// Subscribe registers h for eventType. It returns nothing because
	// several modules subscribing to one event is the expected pattern,
	// not a conflict.
	Subscribe(eventType string, h EventHandler)
	// Bus returns the EventBus the subscriptions are installed on, so that a
	// publisher reaches the handlers subscribers registered.
	Bus() EventBus
}

// AuditActionRegistrar collects the audit action enumeration modules define.
type AuditActionRegistrar interface {
	// Add registers audit actions. It returns an error wrapping
	// ErrDuplicateAuditAction on a repeated action. Nothing is registered
	// when the call returns an error.
	Add(actions ...string) error
	// Actions returns every registered audit action, sorted.
	Actions() []string
}

// SubjectRef identifies whose data a right-to-erasure request names: one
// subject inside exactly one tenant. Both fields are mandatory -- an
// erasure request with no tenant could never be scoped correctly by a
// RetentionParticipant.Erase callback, whose own dbkit.Repository[T].
// HardDelete stays strictly tenant-bound even past its system-context gate
// (see go/dbkit's hard_delete.go), and an erasure request with no subject
// has nothing to erase.
type SubjectRef struct {
	// TenantID is the tenant the subject belongs to.
	TenantID TenantID
	// SubjectID is the subject's identifier within TenantID -- typically a
	// user id, in whatever form the registering participant's own model
	// stores it as a foreign reference (an ID reference per this
	// repository's cross-module-FK rule, never a struct import).
	SubjectID string
}

// RetentionParticipant is one business module's contribution to compliance
// orchestration: a Name plus callbacks a compliance-layer caller invokes --
// never the other way around, and never compliance code touching the
// participant's own table directly. Sweep hard-deletes the participant's
// own model's soft-deleted rows past a cutoff for one tenant (the
// retention-window sweep); Erase hard-deletes the participant's own rows
// for one subject immediately, bypassing the retention window (a
// right-to-erasure request); Export, optional, returns the participant's
// own JSON-serializable data for one tenant (data-portability gathering).
//
// Every callback is expected to already carry its own safety: a
// participant implements Sweep/Erase by calling its own
// dbkit.Repository[T].HardDelete (already tenant-bound and
// system-context-gated), rather than compliance reaching into another
// module's table directly. This is why the registrar lives on
// pkgcore.ComponentRegistry rather than as a method compliance calls on some other
// module's exported type: every business module above compliance in the
// dependency graph can register a participant without compliance ever
// importing it, and compliance never needs to import a business module's
// own repository type either -- the same "no cross-module struct imports,
// only ID references and callbacks" shape the rest of this codebase's
// module-boundary rule already requires.
//
// Where the text of a returned error goes is part of every callback's
// contract, stated here once so a participant author never has to guess.
// The compliance layer keeps the error text itself only where a platform
// operator or the calling process can read it: the per-participant Errors
// map of the returned in-process result on the sweep and erasure paths
// (compliance.SweepResult.Errors / ErasureResult.Errors), the wrapped
// cause of the error compliance itself returns where it wraps one, and
// the structured log at the failure site (behind go/observability's
// redaction layer). It is never recorded verbatim in the compliance audit
// record -- whose changes column is effectively permanent, per
// go/dbkit/audit/emit.go's Diff content contract -- and never serialized
// into a delivered export manifest: both carry the participant's Name
// classified to a "failed" marker instead, and the export manifest is the
// sharper case of the two, because it is delivered to the exporting
// tenant over an unauthenticated, single-view go/sharing link whose
// holder is entitled to read the export's data, never platform-internal
// failure text that can name other subjects, internal object keys or
// infrastructure details. An author writes the error knowing its text
// reaches operators' logs and the in-process results, never an export
// recipient or the audit table.
type RetentionParticipant struct {
	// Name identifies the participant for logging, audit records and
	// duplicate-registration errors -- conventionally the owning module's
	// own Name() plus the model, for example "notes.note".
	Name string

	// Sweep hard-deletes tenant's own soft-deleted rows whose deletion
	// happened at or before cutoff, and reports how many rows it removed.
	// It runs once per (tenant, participant) pair per sweep run, under a
	// context that already carries both tenant and system context -- the
	// participant's own dbkit.Repository[T].HardDelete calls read both
	// straight from ctx, never from a parameter this signature would have
	// to carry separately. Sweep is mandatory at registration: a
	// participant with a nil Sweep is refused with ErrNilRetentionSweep
	// rather than accepted and silently skipped by the sweep, which would
	// retain its tenant data forever while the sweep reported success.
	// The returned error's text follows the type's doc comment's
	// error-text contract: it reaches the returned SweepResult.Errors and
	// the failure-site structured log, and the sweep's audit record
	// classifies it -- never verbatim.
	Sweep func(ctx context.Context, tenant TenantID, cutoff time.Time) (reaped int, err error)

	// Erase immediately hard-deletes every row belonging to subject,
	// bypassing the retention window entirely, and reports how many rows it
	// removed. It runs under a context that already carries subject.
	// TenantID and system context. A participant with no data for subject
	// returns (0, nil) -- never an error -- so that re-running an erasure
	// already partially applied elsewhere converges instead of failing
	// forever; a genuine erasure failure (a transient database error, for
	// example) is the only case that should return a non-nil err. Erase is
	// mandatory at registration, exactly as Sweep is: a participant with a
	// nil Erase is refused with ErrNilRetentionErase rather than accepted
	// and silently skipped by the erasure orchestration -- which would
	// answer a right-to-erasure request with full success while the
	// participant's rows for the subject stayed untouched. A participant
	// with genuinely nothing subject-shaped to erase -- a tenant-wide
	// bundle, for example -- declares exactly that with an explicit Erase
	// returning (0, nil): "nothing to erase" is a fact the participant
	// states, never something the orchestration guesses.
	Erase func(ctx context.Context, subject SubjectRef) (erased int, err error)

	// Export returns the participant's own JSON-serializable data for
	// tenant, for the compliance data-export gathering step. Export is the
	// one callback registration leaves optional -- nil when the
	// participant has not opted into export, a legal, common value, not a
	// misconfiguration. Its absence costs a missing export, never a false
	// success.
	//
	// What the returned data may carry is a contract of the same kind as
	// its error text's, on the same channel: the manifest stores the data
	// and hands it to the exporting tenant's recipient over an
	// unauthenticated, single-view go/sharing link -- the type doc's link
	// "whose holder is entitled to read the export's data" -- and the
	// share rules admit such links leaking, which is exactly why password
	// protection exists for a share. Every field of every row this
	// callback returns is readable by whoever holds the link, with nothing
	// further to authenticate, so the author must make a field-level
	// judgment, in place, about what the export may carry: a capture-time
	// guard such as an audit-trail redaction tag confines data inside its
	// own capture mechanism and never follows the data out of the module.
	// Shipping the tenant's own content in full can be exactly right for a
	// portability export -- the obligation this paragraph states is that
	// the judgment is the author's, recorded where the export is written,
	// never the mechanism's to assume or a later reader's to guess.
	//
	// The returned error's text follows the type's doc comment's
	// error-text contract, and the export path is its sharpest case: the
	// manifest's Errors entry classifies the failure and is itself
	// delivered to the exporting tenant over an unauthenticated share
	// link, so the error text must never reach the manifest -- it goes to
	// the failure-site structured log instead.
	Export func(ctx context.Context, tenant TenantID) (data any, err error)
}

// RetentionRegistrar collects the RetentionParticipant contributions
// business modules register for compliance's retention-sweep,
// right-to-erasure and data-export orchestration.
type RetentionRegistrar interface {
	// Add registers participants. It returns an error wrapping
	// ErrDuplicateRetentionParticipant on a repeated Name, and an error
	// wrapping ErrNilRetentionSweep or ErrNilRetentionErase on a
	// participant missing one of the two callbacks the retention role
	// makes mandatory (Export is the one callback that may be nil).
	// Nothing is registered when the call returns an error.
	Add(participants ...RetentionParticipant) error
	// Participants returns every registered participant, in registration
	// order.
	Participants() []RetentionParticipant
}

// PeriodicScope names which tenants a periodic task is scheduled for.
type PeriodicScope string

const (
	// PeriodicScopePlatform declares a task that runs once per window for
	// the platform as a whole, under the sentinel tenant
	// PeriodicTask.PlatformTenant -- the shape a task over platform data
	// (signing keys, authorities) needs, since there is no single real
	// tenant to enqueue it for.
	PeriodicScopePlatform PeriodicScope = "platform"

	// PeriodicScopePerTenant declares a task that runs once per window per
	// tenant, expanded from the host's tenant lister at tick time -- the
	// shape every task whose work is tenant data needs.
	PeriodicScopePerTenant PeriodicScope = "per_tenant"
)

// PeriodicTask is one periodic task a module declares it owns: a job type
// plus the window, the tenant scope and the key material a scheduler needs
// to enqueue it on a cadence.
//
// # Declaring means scheduled
//
// A declaration on this seat IS the schedule. A host that runs a
// jobs.Scheduler over the Registry's declarations enqueues every
// declaration on that declaration's own cadence, and the host's single
// switch is whether it starts a scheduler at all -- there is no
// per-declaration approval step and no separate enable flag a host could
// forget to set. A module therefore declares a task exactly where it also
// registers that task's handler (the Jobs seat), so every declaration has
// an executor in the composition that made it; a composition in which the
// task must not run is wired without the queue that handler registration
// needs, and the declaration is not made at all.
//
// A declaration says WHAT runs periodically, WHEN (Every) and FOR WHOM
// (Scope); it carries no payload and no enqueue options, because the task's
// handler reads everything it needs at run time and every enqueue of the
// seven sites this seat was shaped against wants the queue's defaults.
type PeriodicTask struct {
	// Type is the job type the task is enqueued under: the Type() of the
	// jobs.Handler the declaring module registers for it, and the type its
	// own Enqueue* method enqueues.
	Type string

	// Every is the period one schedule window covers, and doubles as the
	// enqueue cadence's floor: the scheduler enqueues the task at most
	// once per Every per subject (once per platform run, or once per
	// tenant), because the enqueue's idempotency key is scoped to the
	// Every-sized window (jobs.ScheduleWindowStart) the tick falls in --
	// same-window ticks collapse onto one job, and a later window resolves
	// a fresh key and runs again. It must be positive.
	Every time.Duration

	// Scope selects which tenants the task is enqueued for:
	// PeriodicScopePlatform (once, under PlatformTenant) or
	// PeriodicScopePerTenant (once per tenant the host's tenant lister
	// returns, each under that tenant's own context).
	Scope PeriodicScope

	// KeyPrefix is the prefix of the idempotency key every enqueue of this
	// task is made under -- the site's own established prefix, which is
	// deliberately not always the task type ("storage.sweep:" for the
	// "storage.expiry_sweep" task). The scheduler composes each enqueue's
	// key as KeyPrefix + the tenant segment + the window start (UTC RFC
	// 3339), and the module's own Enqueue* path must resolve the very same
	// string for the same (type, tenant, window): a scheduler tick and a
	// manual enqueue landing in one window are duplicates of one run and
	// must dedupe onto a single job. It must be non-empty.
	KeyPrefix string

	// PlatformTenant is the fixed sentinel tenant (the "_pki_platform_scan"
	// kind, never a real tenant) a PeriodicScopePlatform task is enqueued
	// under: jobs.Task requires a non-empty TenantID, and a platform-wide
	// task belongs to no single tenant. One sentinel per task type keeps
	// the two platform tasks' Job rows distinguishable wherever they are
	// listed by tenant. It is required for PeriodicScopePlatform and must
	// be empty for PeriodicScopePerTenant, whose tenant each enqueue
	// expands.
	PlatformTenant TenantID
}

// PeriodicTaskRegistrar collects the periodic tasks modules declare -- the
// seat go/jobs's Scheduler reads its schedule from (a host starts one over
// the finished Registry's declarations; see PeriodicTask's own doc comment
// for the declaring-means-scheduled semantics).
type PeriodicTaskRegistrar interface {
	// Add registers periodic tasks, validating every declaration first. A
	// declaration whose fields contradict one another -- an empty Type, a
	// non-positive Every, an unknown Scope, an empty KeyPrefix, a
	// Platform-scope declaration with an empty PlatformTenant, or a
	// PerTenant declaration carrying one -- is rejected with an error
	// wrapping ErrInvalidPeriodicTask, because such a declaration could
	// never be scheduled coherently. A Type already registered (by an
	// earlier call, or twice within this one) is rejected with an error
	// wrapping ErrDuplicatePeriodicTask. Nothing is registered when the
	// call returns an error.
	Add(decls ...PeriodicTask) error
	// Declarations returns every periodic task registered so far, in
	// declaration order.
	Declarations() []PeriodicTask
}

// checkUnique reports whether any key produced by keyOf is already in
// registered or repeated within items, wrapping sentinel in either case. It
// runs before anything is committed so that a rejected call registers nothing.
func checkUnique[T any](registered map[string]struct{}, items []T, keyOf func(T) string, sentinel error) error {
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		key := keyOf(item)
		if _, exists := registered[key]; exists {
			return fmt.Errorf("%w: %q", sentinel, key)
		}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("%w: %q registered twice in one call", sentinel, key)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// sameString is the key accessor for registrars whose items are the keys.
func sameString(s string) string { return s }

type memoryRouteRegistrar struct {
	mu     sync.Mutex
	routes []MountedRoute
}

func (r *memoryRouteRegistrar) Mount(path string, handler http.Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes = append(r.routes, MountedRoute{Path: path, Handler: handler})
}

func (r *memoryRouteRegistrar) Routes() []MountedRoute {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.routes)
}

// memoryMiddlewareRegistrar is the in-memory default implementation of
// MiddlewareRegistrar, mirroring memoryRouteRegistrar's shape (a mutex plus
// an append-only, registration-ordered slice). It refuses a nil entry --
// validation comes before anything is appended, so a rejected call
// registers nothing.
type memoryMiddlewareRegistrar struct {
	mu  sync.Mutex
	mws []func(http.Handler) http.Handler
}

func (r *memoryMiddlewareRegistrar) Add(mw ...func(http.Handler) http.Handler) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range mw {
		if m == nil {
			return ErrNilMiddleware
		}
	}
	r.mws = append(r.mws, mw...)
	return nil
}

func (r *memoryMiddlewareRegistrar) Middlewares() []func(http.Handler) http.Handler {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.mws)
}

type memoryConfigRegistrar struct {
	mu    sync.Mutex
	keys  map[string]struct{}
	items []ConfigItem
}

func (r *memoryConfigRegistrar) Add(items ...ConfigItem) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Validation comes before the duplicate check so a contradictory
	// declaration reports itself as invalid rather than as a collision with
	// whatever an earlier caller registered under the same key. Either way
	// the whole call registers nothing.
	for _, item := range items {
		if err := validateConfigItem(item); err != nil {
			return err
		}
	}
	keyOf := func(item ConfigItem) string { return item.Key }
	if err := checkUnique(r.keys, items, keyOf, ErrDuplicateConfigKey); err != nil {
		return err
	}
	for _, item := range items {
		r.keys[item.Key] = struct{}{}
		r.items = append(r.items, item)
	}
	return nil
}

func (r *memoryConfigRegistrar) Items() []ConfigItem {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.items)
}

type memoryFeatureRegistrar struct {
	mu    sync.Mutex
	keys  map[string]struct{}
	flags []FeatureFlag
}

func (r *memoryFeatureRegistrar) Add(flags ...FeatureFlag) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	keyOf := func(flag FeatureFlag) string { return flag.Key }
	if err := checkUnique(r.keys, flags, keyOf, ErrDuplicateFeatureFlag); err != nil {
		return err
	}
	for _, flag := range flags {
		r.keys[flag.Key] = struct{}{}
		r.flags = append(r.flags, flag)
	}
	return nil
}

func (r *memoryFeatureRegistrar) Flags() []FeatureFlag {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.flags)
}

type memoryPermissionRegistrar struct {
	mu    sync.Mutex
	perms map[string]struct{}
}

func (r *memoryPermissionRegistrar) Add(perms ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := checkUnique(r.perms, perms, sameString, ErrDuplicatePermission); err != nil {
		return err
	}
	for _, perm := range perms {
		r.perms[perm] = struct{}{}
	}
	return nil
}

func (r *memoryPermissionRegistrar) Permissions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Sorted(maps.Keys(r.perms))
}

type memoryJobRegistrar struct {
	mu       sync.Mutex
	handlers map[string]any
}

func (r *memoryJobRegistrar) Handle(jobType string, handler any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.handlers[jobType]; exists {
		return fmt.Errorf("%w: %q", ErrDuplicateJobType, jobType)
	}
	r.handlers[jobType] = handler
	return nil
}

func (r *memoryJobRegistrar) Handlers() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.handlers)
}

type memoryNotificationRegistrar struct {
	mu    sync.Mutex
	keys  map[string]struct{}
	types []NotificationType
}

func (r *memoryNotificationRegistrar) Add(types ...NotificationType) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	keyOf := func(t NotificationType) string { return t.Key }
	if err := checkUnique(r.keys, types, keyOf, ErrDuplicateNotificationType); err != nil {
		return err
	}
	for _, t := range types {
		r.keys[t.Key] = struct{}{}
		r.types = append(r.types, t)
	}
	return nil
}

func (r *memoryNotificationRegistrar) Types() []NotificationType {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.types)
}

// memoryEventRegistrar records the domain events modules declare they publish
// and forwards their subscriptions to the bus the registry was built with, so
// that the declaration catalog and the delivery seam cannot drift apart.
type memoryEventRegistrar struct {
	// bus is fixed at construction: a registrar that could be repointed
	// after modules subscribed would strand their handlers on the old bus.
	bus EventBus

	mu     sync.Mutex
	types  map[string]struct{}
	events []EventDecl
}

func (r *memoryEventRegistrar) Publishes(events ...EventDecl) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	keyOf := func(evt EventDecl) string { return evt.Type }
	if err := checkUnique(r.types, events, keyOf, ErrDuplicateEventType); err != nil {
		return err
	}
	for _, evt := range events {
		r.types[evt.Type] = struct{}{}
		r.events = append(r.events, evt)
	}
	return nil
}

func (r *memoryEventRegistrar) Published() []EventDecl {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

func (r *memoryEventRegistrar) Subscribe(eventType string, h EventHandler) {
	r.bus.Subscribe(eventType, h)
}

func (r *memoryEventRegistrar) Bus() EventBus { return r.bus }

type memoryAuditActionRegistrar struct {
	mu      sync.Mutex
	actions map[string]struct{}
}

func (r *memoryAuditActionRegistrar) Add(actions ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := checkUnique(r.actions, actions, sameString, ErrDuplicateAuditAction); err != nil {
		return err
	}
	for _, action := range actions {
		r.actions[action] = struct{}{}
	}
	return nil
}

func (r *memoryAuditActionRegistrar) Actions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Sorted(maps.Keys(r.actions))
}

// memoryRetentionRegistrar is the in-memory default implementation of
// RetentionRegistrar, mirroring memoryNotificationRegistrar's exact shape
// (a name-uniqueness map plus an append-only, registration-ordered slice).
type memoryRetentionRegistrar struct {
	mu    sync.Mutex
	names map[string]struct{}
	items []RetentionParticipant
}

func (r *memoryRetentionRegistrar) Add(participants ...RetentionParticipant) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Validation comes before the duplicate check so a participant with a
	// nil Sweep or a nil Erase reports itself as such rather than as a
	// collision with whatever an earlier caller registered under the same
	// Name. Both callbacks are mandatory at registration, for the same
	// reason in each direction: a participant without a Sweep could never
	// release its own soft-deleted rows, and the sweep would have to skip
	// it silently -- its tenant data retained forever while the sweep
	// reported success; a participant without an Erase would leave its
	// rows for the subject untouched while a right-to-erasure request
	// reported full success to the data subject. Each is refused here
	// instead. Export is the one callback that may be nil. Either way the
	// whole call registers nothing.
	for _, p := range participants {
		if p.Sweep == nil {
			return fmt.Errorf("%w: %q", ErrNilRetentionSweep, p.Name)
		}
		if p.Erase == nil {
			return fmt.Errorf("%w: %q", ErrNilRetentionErase, p.Name)
		}
	}
	keyOf := func(p RetentionParticipant) string { return p.Name }
	if err := checkUnique(r.names, participants, keyOf, ErrDuplicateRetentionParticipant); err != nil {
		return err
	}
	for _, p := range participants {
		r.names[p.Name] = struct{}{}
		r.items = append(r.items, p)
	}
	return nil
}

func (r *memoryRetentionRegistrar) Participants() []RetentionParticipant {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.items)
}

// memoryScheduleRegistrar is the in-memory default implementation of
// PeriodicTaskRegistrar, mirroring memoryRetentionRegistrar's shape (a
// type-uniqueness map plus an append-only, declaration-ordered slice).
type memoryScheduleRegistrar struct {
	mu    sync.Mutex
	types map[string]struct{}
	decls []PeriodicTask
}

func (r *memoryScheduleRegistrar) Add(decls ...PeriodicTask) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Validation comes before the duplicate check so a contradictory
	// declaration reports itself as invalid rather than as a collision with
	// whatever an earlier caller registered under the same Type. Either way
	// the whole call registers nothing.
	for _, decl := range decls {
		if err := validatePeriodicTask(decl); err != nil {
			return err
		}
	}
	keyOf := func(decl PeriodicTask) string { return decl.Type }
	if err := checkUnique(r.types, decls, keyOf, ErrDuplicatePeriodicTask); err != nil {
		return err
	}
	for _, decl := range decls {
		r.types[decl.Type] = struct{}{}
		r.decls = append(r.decls, decl)
	}
	return nil
}

func (r *memoryScheduleRegistrar) Declarations() []PeriodicTask {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.decls)
}

// validatePeriodicTask reports whether decl describes one coherent periodic
// schedule: a non-empty Type, a positive Every, one of the two declared
// scopes, a non-empty KeyPrefix, and a PlatformTenant exactly where the
// scope needs it. See ErrInvalidPeriodicTask's doc comment for why each
// contradiction is refused at registration.
func validatePeriodicTask(decl PeriodicTask) error {
	if decl.Type == "" {
		return fmt.Errorf("%w: empty task type", ErrInvalidPeriodicTask)
	}
	if decl.Every <= 0 {
		return fmt.Errorf("%w: task %q has Every %s, want a positive window", ErrInvalidPeriodicTask, decl.Type, decl.Every)
	}
	if decl.KeyPrefix == "" {
		return fmt.Errorf("%w: task %q has no key prefix", ErrInvalidPeriodicTask, decl.Type)
	}
	switch decl.Scope {
	case PeriodicScopePlatform:
		if decl.PlatformTenant == "" {
			return fmt.Errorf("%w: platform-scope task %q has no platform sentinel tenant", ErrInvalidPeriodicTask, decl.Type)
		}
	case PeriodicScopePerTenant:
		if decl.PlatformTenant != "" {
			return fmt.Errorf("%w: per-tenant task %q carries platform sentinel tenant %q", ErrInvalidPeriodicTask, decl.Type, decl.PlatformTenant)
		}
	default:
		return fmt.Errorf("%w: task %q has scope %q, want %q or %q",
			ErrInvalidPeriodicTask, decl.Type, decl.Scope, PeriodicScopePlatform, PeriodicScopePerTenant)
	}
	return nil
}

// formatCycle renders the traversal path from the first occurrence of name
// back to name, so that the error names the components that form the cycle.
func formatCycle(path []string, name string) string {
	start := slices.Index(path, name)
	if start < 0 {
		start = 0
	}
	cycle := append(slices.Clone(path[start:]), name)
	return strings.Join(cycle, " -> ")
}

// ValidateFeatureGraph reports whether every feature flag dependency in the
// Features seat resolves to a flag some component actually registered. It
// runs after every declaration turn, because a flag may legitimately depend
// on one owned by a component that declares later. All unresolved
// dependencies are reported together rather than one per run.
func ValidateFeatureGraph(features FeatureRegistrar) error {
	if features == nil {
		return errNilFeatureRegistrar
	}

	flags := features.Flags()
	known := make(map[string]struct{}, len(flags))
	for _, flag := range flags {
		known[flag.Key] = struct{}{}
	}

	var unresolved []string
	for _, flag := range flags {
		for _, dep := range flag.DependsOn {
			if _, ok := known[dep]; !ok {
				unresolved = append(unresolved, fmt.Sprintf("%q depends on unregistered flag %q", flag.Key, dep))
			}
		}
	}
	if len(unresolved) > 0 {
		return fmt.Errorf("%w: %s", ErrUnresolvedFeatureDependency, strings.Join(unresolved, "; "))
	}
	return nil
}
