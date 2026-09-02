package pkgcore

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/vislake/speed/go/pkgcore/i18n"
)

// ErrDuplicateConfigKey is returned when two modules register the same configuration key.
var ErrDuplicateConfigKey = errors.New("pkgcore: duplicate config key")

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

// ErrDuplicateModuleName is returned when two modules in a bootstrap set report the same Name.
var ErrDuplicateModuleName = errors.New("pkgcore: duplicate module name")

// ErrDependencyCycle is returned when module dependencies form a cycle.
var ErrDependencyCycle = errors.New("pkgcore: module dependency cycle")

// ErrMissingDependency is returned when a module depends on a module that is not being bootstrapped.
var ErrMissingDependency = errors.New("pkgcore: missing module dependency")

// ErrUnresolvedFeatureDependency is returned when a feature flag depends on a flag nobody registered.
var ErrUnresolvedFeatureDependency = errors.New("pkgcore: unresolved feature flag dependency")

// ErrMissingDistributedEventBus is returned by Bootstrap when a Kernel assembled
// for DeploymentModeDistributed has no EventBus wired in. There is no
// distributed implementation to fall back on, and falling back to the
// in-memory bus would give every replica a private bus, so assembly fails
// instead.
var ErrMissingDistributedEventBus = errors.New("pkgcore: distributed deployment mode requires an explicit event bus")

// ErrMissingDistributedKVStore is returned by Bootstrap when a Kernel assembled
// for DeploymentModeDistributed has no KVStore wired in. There is no
// distributed implementation to fall back on, and falling back to the
// in-memory store would give every replica a private store, so assembly
// fails instead.
var ErrMissingDistributedKVStore = errors.New("pkgcore: distributed deployment mode requires an explicit key-value store")

// ErrMissingDistributedMailer is returned by Bootstrap when a Kernel assembled
// for DeploymentModeDistributed has no Mailer wired in. There is no
// distributed implementation to fall back on, and falling back to the console
// mailer would print every message to a replica's stdout where nobody reads
// it, so assembly fails instead.
var ErrMissingDistributedMailer = errors.New("pkgcore: distributed deployment mode requires an explicit mailer")

// ErrMissingDistributedObjectStore is returned by Bootstrap when a Kernel
// assembled for DeploymentModeDistributed has no ObjectStore wired in. There
// is no distributed implementation to fall back on, and falling back to the
// local file system store would give every replica its own private disk and
// split the objectspace silently, so assembly fails instead.
var ErrMissingDistributedObjectStore = errors.New("pkgcore: distributed deployment mode requires an explicit object store")

// errNilFeatureRegistrar guards ValidateFeatureGraph against an unwired registry.
var errNilFeatureRegistrar = errors.New("pkgcore: registry has no feature registrar")

// Module is the contract every speed module implements so that the Kernel can
// assemble it into the host application. A module declares its identity, its
// dependencies and its embedded assets, and contributes everything else
// through a single Register call.
//
// Migrations returns a plain embed.FS rather than a dbkit type on purpose:
// pkgcore is the dependency floor of the monorepo and dbkit depends on it, so
// referencing a dbkit type here would create an import cycle between modules.
// The dbkit migration registry consumes the embed.FS and applies the dialect
// convention (migrations/{postgres,sqlite}) on top of it.
type Module interface {
	// Name returns the module's unique identifier, for example "billing".
	Name() string

	// DependsOn returns the Name of every module that must be registered
	// before this one. Names that are not part of the bootstrap set make
	// Bootstrap fail rather than silently skipping the dependency.
	DependsOn() []string

	// Migrations returns the module's versioned SQL migrations, one
	// subdirectory per SQL dialect.
	Migrations() embed.FS

	// Locales returns the module's translation resources, one
	// <language>.toml file per language, embedded flat under the locale
	// file contract documented in the i18n package. Kernel.Bootstrap feeds
	// it to i18n.Builder.AddModule while it assembles the merged message
	// catalog, so a module that renders no content returns an empty
	// embed.FS and a module that ships files must ship one file for every
	// language the catalog serves (zh-CN.toml and en-US.toml in M0) and
	// none for any other, prefix its ids with its Name and keep every
	// language's id set identical.
	Locales() embed.FS

	// OpenAPISpec returns the module's OpenAPI contract fragment, which the
	// host merges into the application-wide specification.
	OpenAPISpec() []byte

	// Register declares everything the module contributes to the host:
	// routes, configuration schema, feature flags, permissions, job
	// handlers, notification types, event subscriptions and audit actions.
	// It must not perform I/O; it only declares.
	Register(reg *Registry) error
}

// MountedRoute is one HTTP handler mounted at a path by a module.
type MountedRoute struct {
	// Path is the prefix the handler was mounted at, for example "/api/v1/billing".
	Path string
	// Handler serves every request below Path.
	Handler http.Handler
}

// ConfigItem describes a single configuration key contributed by a module.
// The admin console form and the generated configuration reference are both
// derived from these declarations, so Description is not optional in practice.
type ConfigItem struct {
	// Key is the dotted configuration key, for example "billing.invoice_retry_limit".
	Key string
	// Type names the value type, for example "string", "int", "bool" or "duration".
	Type string
	// Default is the value used when neither the environment nor a tenant overrides the key.
	Default any
	// Sensitive marks secrets, which are redacted in logs, in the admin console and in the docs.
	Sensitive bool
	// Description is the English text shown in the admin console and the configuration reference.
	Description string
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

// ConfigSchemaRegistrar collects the configuration schema modules declare.
type ConfigSchemaRegistrar interface {
	// Add registers configuration items. It returns an error wrapping
	// ErrDuplicateConfigKey if a key was already registered, because two
	// modules owning one key is a bug rather than a merge. Nothing is
	// registered when the call returns an error.
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

// Registry aggregates every registration surface a module can contribute to.
// New cross-cutting mechanisms are added as a field here rather than as a
// method on Module, so that adding one does not force all modules to
// reimplement the interface, which under lockstep versioning would be a
// breaking change for every consumer at once.
type Registry struct {
	// Routes receives the HTTP handlers modules mount.
	Routes RouteRegistrar
	// Config receives the configuration schema modules declare.
	Config ConfigSchemaRegistrar
	// Features receives the feature flags modules declare.
	Features FeatureRegistrar
	// Permissions receives the resource:action permissions modules define.
	Permissions PermissionRegistrar
	// Jobs receives the asynchronous job handlers modules provide.
	Jobs JobHandlerRegistrar
	// Notifications receives the notification types modules can emit.
	Notifications NotificationRegistrar
	// Events receives the domain events modules publish and the
	// subscriptions they install.
	Events EventRegistrar
	// AuditActions receives the audit action enumeration modules define.
	AuditActions AuditActionRegistrar

	// kv is the KVStore the registry was built with. EventBus is derived from
	// the Events registrar because Events.Subscribe must install handlers on
	// the exact bus a publisher uses, so substituting Events has to carry the
	// bus along with it; nothing in Registry installs anything onto a
	// KVStore, so there is no registrar for kv to live inside, and it is kept
	// directly on Registry instead. It is unexported so that KVStore() is the
	// only way to reach it, mirroring the read-only role EventBus() plays for
	// bus.
	kv KVStore

	// mailer is the Mailer the registry was built with. It is kept directly on
	// Registry for the same reason kv is: nothing in Registry installs
	// anything onto a Mailer, so no registrar would have anything to hold. It
	// is unexported so that Mailer() is the only way to reach it.
	mailer Mailer

	// objectStore is the ObjectStore the kernel wired the registry to. It is
	// kept directly on Registry for the same reason kv and mailer are, and it
	// is unexported so that ObjectStore() is the only way to reach it. It is
	// deliberately not a NewRegistry parameter: the object-store seam
	// post-dates the constructor, whose three-argument signature is frozen,
	// so Bootstrap installs the store it resolved after NewRegistry returns,
	// and a registry built directly with NewRegistry has none.
	objectStore ObjectStore

	// locales is the merged message catalog Bootstrap assembled from every
	// registered module's Locales() embed.FS, built like objectStore by
	// post-dating NewRegistry's three-argument signature rather than joining
	// it. It is unexported so that Locales() is the only way to reach it,
	// and it is nil on a registry built directly with NewRegistry, exactly
	// like objectStore is.
	locales *i18n.Catalog
}

// NewRegistry returns a Registry whose registrars are the in-memory default
// implementations, whose event seam is bus, whose key-value seam is kv, and
// whose mail seam is mailer. The defaults are complete enough to run and to
// test against right now, and they double as the standalone deployment mode's
// implementations, so no separate test double is needed.
//
// bus, kv and mailer are parameters rather than built-in defaults because
// they are the seams with a real distributed implementation: the caller
// decides which bus the modules subscribe to, which store they read and
// write, and which transport their emails leave through. Kernel.Bootstrap is
// the normal way to build a Registry, because it picks the implementations
// that match the deployment mode. A nil bus, a nil kv or a nil mailer
// panics: each is an unrecoverable wiring error at startup. The alternative
// for bus is a registry that accepts every subscription and silently drops
// every event; the alternative for kv is a registry whose KVStore() looks
// wired but panics the first caller that actually uses it; the alternative
// for mailer is a registry whose Mailer() sends nothing and reports success.
// The object-store seam is not a parameter either: it post-dates this
// constructor, whose signature is frozen, so ObjectStore() is nil on a
// registry built here and is only ever set by Bootstrap.
func NewRegistry(bus EventBus, kv KVStore, mailer Mailer) *Registry {
	if bus == nil {
		panic("pkgcore: NewRegistry requires a non-nil EventBus")
	}
	if kv == nil {
		panic("pkgcore: NewRegistry requires a non-nil KVStore")
	}
	if mailer == nil {
		panic("pkgcore: NewRegistry requires a non-nil Mailer")
	}
	return &Registry{
		Routes:        &memoryRouteRegistrar{},
		Config:        &memoryConfigRegistrar{keys: make(map[string]struct{})},
		Features:      &memoryFeatureRegistrar{keys: make(map[string]struct{})},
		Permissions:   &memoryPermissionRegistrar{perms: make(map[string]struct{})},
		Jobs:          &memoryJobRegistrar{handlers: make(map[string]any)},
		Notifications: &memoryNotificationRegistrar{keys: make(map[string]struct{})},
		Events:        &memoryEventRegistrar{bus: bus, types: make(map[string]struct{})},
		AuditActions:  &memoryAuditActionRegistrar{actions: make(map[string]struct{})},
		kv:            kv,
		mailer:        mailer,
	}
}

// EventBus returns the bus the registry's Events registrar installs
// subscriptions on, so that the host publishes domain events into the same bus
// modules subscribed to. It is derived from Events rather than stored
// alongside it, so substituting Events moves publishers and subscribers
// together instead of splitting them. It returns nil for a Registry with no
// Events registrar, which only a zero-value Registry has.
func (r *Registry) EventBus() EventBus {
	if r.Events == nil {
		return nil
	}
	return r.Events.Bus()
}

// KVStore returns the key-value store the registry was built with.
//
// Unlike EventBus, this is not derived from a registrar: no registrar on
// Registry installs subscriptions, handlers or anything else onto a KVStore,
// so there is nothing for kv to live inside, and it is stored directly on the
// struct instead. It returns nil for a zero-value Registry, which only tests
// construct.
func (r *Registry) KVStore() KVStore {
	return r.kv
}

// Mailer returns the mailer the registry was built with, the transport the
// modules' outgoing email leaves through. Like KVStore, it is stored directly
// on the struct because no registrar installs anything onto it. It returns
// nil for a zero-value Registry, which only tests construct.
func (r *Registry) Mailer() Mailer {
	return r.mailer
}

// ObjectStore returns the object store the registry was wired to, the store
// the modules' files leave through. Like KVStore and Mailer, it is stored
// directly on the struct because no registrar installs anything onto it.
// Unlike them it is not a NewRegistry parameter -- the seam post-dates that
// constructor, whose three-argument signature is frozen -- so it returns nil
// for a Registry built directly with NewRegistry and only ever carries a
// store once Bootstrap has resolved one. A module that needs the store must
// therefore be exercised through a bootstrapped Registry, never through a
// hand-built one: calling a method on a nil ObjectStore panics.
func (r *Registry) ObjectStore() ObjectStore {
	return r.objectStore
}

// Locales returns the merged message catalog Bootstrap assembled from every
// registered module's Locales() embed.FS, the catalog a module queries to
// render the backend-generated content it sends to a recipient in that
// recipient's locale. Like ObjectStore it is not a NewRegistry parameter --
// the i18n seam post-dates that constructor, whose three-argument signature
// is frozen -- so it returns nil for a Registry built directly with
// NewRegistry and only ever carries a catalog once Bootstrap has merged
// every module's locale files. A module that needs the catalog must
// therefore be exercised through a bootstrapped Registry, never through a
// hand-built one: calling a method on a nil *i18n.Catalog panics.
func (r *Registry) Locales() *i18n.Catalog {
	return r.locales
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

type memoryConfigRegistrar struct {
	mu    sync.Mutex
	keys  map[string]struct{}
	items []ConfigItem
}

func (r *memoryConfigRegistrar) Add(items ...ConfigItem) error {
	r.mu.Lock()
	defer r.mu.Unlock()
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

// Kernel assembles modules into a running application for one deployment mode.
// The deployment mode selects the infrastructure implementations the assembled
// Registry is wired to; kernel assembly is the only place allowed to branch on
// it, which is why the implementations are injected here rather than chosen by
// a module.
type Kernel struct {
	deploymentMode DeploymentMode
	bus            EventBus
	kv             KVStore
	mailer         Mailer
	objectStore    ObjectStore
}

// KernelOption injects an infrastructure implementation into a Kernel.
// Distributed implementations are supplied this way because the kernel would
// otherwise have to construct them from deployment configuration it does not
// carry: a broker address, credentials, a registry of hosts. The host, which
// reads its own configuration, builds the implementation and injects it; the
// kernel stays the only place that picks the standalone defaults.
type KernelOption func(*Kernel)

// WithEventBus wires bus into every Registry the kernel bootstraps, in place of
// the deployment mode's default. It is the seam a distributed-mode host uses
// to supply its broker-backed EventBus: DeploymentModeDistributed has no
// built-in implementation, so Bootstrap fails without it. A nil bus leaves
// the deployment mode's default in place.
func WithEventBus(bus EventBus) KernelOption {
	return func(k *Kernel) {
		if bus == nil {
			return
		}
		k.bus = bus
	}
}

// WithKVStore wires store into every Registry the kernel bootstraps, in place
// of the deployment mode's default. It is the seam a distributed-mode host
// uses to supply its Redis-backed KVStore: DeploymentModeDistributed has no
// built-in implementation, so Bootstrap fails without it. A nil store leaves
// the deployment mode's default in place.
func WithKVStore(store KVStore) KernelOption {
	return func(k *Kernel) {
		if store == nil {
			return
		}
		k.kv = store
	}
}

// WithMailer wires mailer into every Registry the kernel bootstraps, in place
// of the deployment mode's default. It is the seam a distributed-mode host
// uses to supply its SMTP-backed Mailer: DeploymentModeDistributed has no
// built-in implementation, so Bootstrap fails without it. A nil mailer leaves
// the deployment mode's default in place.
func WithMailer(mailer Mailer) KernelOption {
	return func(k *Kernel) {
		if mailer == nil {
			return
		}
		k.mailer = mailer
	}
}

// WithObjectStore wires store into every Registry the kernel bootstraps, in
// place of the deployment mode's default. It is the seam a distributed-mode
// host uses to supply its S3-backed ObjectStore: DeploymentModeDistributed
// has no built-in implementation, so Bootstrap fails without it. A
// DeploymentModeStandalone host that must keep objects across restarts
// injects its own local store here too, because the standalone default is a
// throwaway directory. A nil store leaves the deployment mode's default in
// place.
func WithObjectStore(store ObjectStore) KernelOption {
	return func(k *Kernel) {
		if store == nil {
			return
		}
		k.objectStore = store
	}
}

// NewKernel returns a Kernel that assembles modules for the given deployment
// mode, with opts supplying the implementations the deployment mode itself
// cannot build. An unknown deployment mode is reported by Bootstrap rather
// than here, because that is where the wiring which depends on it happens.
func NewKernel(mode DeploymentMode, opts ...KernelOption) *Kernel {
	k := &Kernel{deploymentMode: mode}
	for _, opt := range opts {
		opt(k)
	}
	return k
}

// eventBus returns the EventBus the assembled Registry is wired to: the
// injected one when the host supplied it, and the in-memory bus for
// DeploymentModeStandalone, which is single-process by design.
// DeploymentModeDistributed reports an error instead of falling back, because
// the in-memory bus would give every replica a private bus and split event
// delivery silently.
func (k *Kernel) eventBus() (EventBus, error) {
	if k.bus != nil {
		return k.bus, nil
	}
	switch k.deploymentMode {
	case DeploymentModeStandalone:
		return NewMemoryEventBus(), nil
	case DeploymentModeDistributed:
		return nil, fmt.Errorf("%w: wire one with WithEventBus", ErrMissingDistributedEventBus)
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidDeploymentMode, k.deploymentMode)
	}
}

// kvStore returns the KVStore the assembled Registry is wired to: the
// injected one when the host supplied it, and the in-memory store for
// DeploymentModeStandalone, which is single-process by design.
// DeploymentModeDistributed reports an error instead of falling back, because
// the in-memory store would give every replica a private store and split
// state silently.
func (k *Kernel) kvStore() (KVStore, error) {
	if k.kv != nil {
		return k.kv, nil
	}
	switch k.deploymentMode {
	case DeploymentModeStandalone:
		return NewMemoryKVStore(), nil
	case DeploymentModeDistributed:
		return nil, fmt.Errorf("%w: wire one with WithKVStore", ErrMissingDistributedKVStore)
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidDeploymentMode, k.deploymentMode)
	}
}

// resolveMailer returns the Mailer the assembled Registry is wired to: the
// injected one when the host supplied it, and the console mailer for
// DeploymentModeStandalone, which has no real transport by design.
// DeploymentModeDistributed reports an error instead of falling back, because
// the console mailer would print every message to a replica's stdout where
// nobody reads it. It parallels eventBus and kvStore; the field it resolves
// is named mailer, which a method of the same name could not be.
func (k *Kernel) resolveMailer() (Mailer, error) {
	if k.mailer != nil {
		return k.mailer, nil
	}
	switch k.deploymentMode {
	case DeploymentModeStandalone:
		return NewConsoleMailer(), nil
	case DeploymentModeDistributed:
		return nil, fmt.Errorf("%w: wire one with WithMailer", ErrMissingDistributedMailer)
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidDeploymentMode, k.deploymentMode)
	}
}

// resolveObjectStore returns the ObjectStore the assembled Registry is wired
// to: the injected one when the host supplied it, and a fresh private
// temporary directory for DeploymentModeStandalone. The standalone default
// is deliberately throwaway: the deployment mode keeps nothing across
// restarts by itself, and a host that must keep objects injects a
// NewLocalObjectStore of its own directory with WithObjectStore.
// DeploymentModeDistributed reports an error instead of falling back, because
// a local store would give every replica its own private disk and split the
// objectspace silently. It parallels eventBus, kvStore and resolveMailer; the
// field it resolves is named objectStore, which a method of the same name
// could not be.
func (k *Kernel) resolveObjectStore() (ObjectStore, error) {
	if k.objectStore != nil {
		return k.objectStore, nil
	}
	switch k.deploymentMode {
	case DeploymentModeStandalone:
		directory, err := os.MkdirTemp("", "pkgcore-object-store-*")
		if err != nil {
			return nil, fmt.Errorf("pkgcore: standalone object store: %w", err)
		}
		return NewLocalObjectStore(directory), nil
	case DeploymentModeDistributed:
		return nil, fmt.Errorf("%w: wire one with WithObjectStore", ErrMissingDistributedObjectStore)
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidDeploymentMode, k.deploymentMode)
	}
}

// DeploymentMode returns the deployment mode the kernel assembles modules for.
func (k *Kernel) DeploymentMode() DeploymentMode { return k.deploymentMode }

// Bootstrap sorts modules into dependency order, registers each of them into
// one shared Registry, and validates the resulting feature flag graph.
//
// It fails, without registering anything further, on a deployment mode whose
// infrastructure implementations are not wired in, on a dependency cycle, on a
// dependency that is not part of modules, on a duplicate module name, on the
// first Register error, and on an unresolved feature flag dependency. ctx must
// be non-nil; cancelling it stops the bootstrap between modules.
func (k *Kernel) Bootstrap(ctx context.Context, modules ...Module) (*Registry, error) {
	// The deployment mode's wiring is resolved first so that a misconfigured
	// distributed-mode host fails at startup rather than after a partial
	// assembly.
	bus, err := k.eventBus()
	if err != nil {
		return nil, err
	}
	kv, err := k.kvStore()
	if err != nil {
		return nil, err
	}
	mailer, err := k.resolveMailer()
	if err != nil {
		return nil, err
	}
	objectStore, err := k.resolveObjectStore()
	if err != nil {
		return nil, err
	}

	ordered, err := sortModulesByDependency(modules)
	if err != nil {
		return nil, err
	}

	reg := NewRegistry(bus, kv, mailer)
	// The object-store seam post-dates NewRegistry's three-argument
	// signature, so the resolved store is installed here, before any module
	// registers and can reach it.
	reg.objectStore = objectStore

	// The message catalog is assembled alongside registration: each module's
	// locale resources are validated and merged before the module itself
	// registers, so a malformed file set or a parity break between a
	// module's languages fails the bootstrap at the module that owns it.
	// The frozen catalog is installed
	// only after every module has registered, so reg.Locales() is still nil
	// inside Register, and calling a method on that nil catalog panics. The
	// catalog becomes reachable through reg only once registration
	// completes: a module that renders catalog content must read it at
	// runtime, from the handlers or jobs it registered, never from Register
	// itself.
	localesBuilder := i18n.NewBuilder()
	for _, module := range ordered {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("pkgcore: bootstrap stopped before registering module %q: %w", module.Name(), err)
		}
		if err := localesBuilder.AddModule(module.Name(), module.Locales()); err != nil {
			return nil, fmt.Errorf("pkgcore: module %q has invalid locale resources: %w", module.Name(), err)
		}
		if err := module.Register(reg); err != nil {
			return nil, fmt.Errorf("pkgcore: module %q failed to register: %w", module.Name(), err)
		}
	}
	reg.locales = localesBuilder.Build()

	if err := ValidateFeatureGraph(reg); err != nil {
		return nil, err
	}
	return reg, nil
}

// moduleVisitState tracks where a module sits in the depth-first traversal
// that produces the registration order.
type moduleVisitState int

const (
	moduleUnvisited moduleVisitState = iota
	moduleVisiting
	moduleVisited
)

// sortModulesByDependency returns modules ordered so that every module appears
// after the modules it depends on. Input order breaks ties, which keeps the
// registration order stable across runs.
func sortModulesByDependency(modules []Module) ([]Module, error) {
	byName := make(map[string]Module, len(modules))
	state := make(map[string]moduleVisitState, len(modules))
	for _, module := range modules {
		name := module.Name()
		if _, exists := byName[name]; exists {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateModuleName, name)
		}
		byName[name] = module
		state[name] = moduleUnvisited
	}

	ordered := make([]Module, 0, len(modules))
	path := make([]string, 0, len(modules))

	var visit func(module Module) error
	visit = func(module Module) error {
		name := module.Name()
		switch state[name] {
		case moduleVisited:
			return nil
		case moduleVisiting:
			return fmt.Errorf("%w: %s", ErrDependencyCycle, formatCycle(path, name))
		case moduleUnvisited:
		}

		state[name] = moduleVisiting
		path = append(path, name)
		for _, dep := range module.DependsOn() {
			dependency, ok := byName[dep]
			if !ok {
				return fmt.Errorf("%w: module %q depends on %q, which is not in the module list", ErrMissingDependency, name, dep)
			}
			if err := visit(dependency); err != nil {
				return err
			}
		}
		path = path[:len(path)-1]
		state[name] = moduleVisited
		ordered = append(ordered, module)
		return nil
	}

	for _, module := range modules {
		if err := visit(module); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

// formatCycle renders the traversal path from the first occurrence of name
// back to name, so that the error names the modules that form the cycle.
func formatCycle(path []string, name string) string {
	start := slices.Index(path, name)
	if start < 0 {
		start = 0
	}
	cycle := append(slices.Clone(path[start:]), name)
	return strings.Join(cycle, " -> ")
}

// ValidateFeatureGraph reports whether every feature flag dependency in reg
// resolves to a flag some module actually registered. It runs after all
// modules have registered, because a flag may legitimately depend on one owned
// by a module that registers later. All unresolved dependencies are reported
// together rather than one per run.
func ValidateFeatureGraph(reg *Registry) error {
	if reg == nil || reg.Features == nil {
		return errNilFeatureRegistrar
	}

	flags := reg.Features.Flags()
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
