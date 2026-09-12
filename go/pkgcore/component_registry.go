package pkgcore

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"

	"github.com/vislake/speed/go/pkgcore/i18n"
)

// component_registry.go carries the assembly half of the config-driven
// component assembly: the ComponentRegistry, its by-type value store (Put
// and Get), its nine declaration seats, and the eight-stage lifecycle the
// engine drives over it. The descriptor half lives in component.go, the
// structured configuration in config.go, and the Prepare stage's
// parse-resolve-validate-plan pipeline in component_assembly.go.

// ErrUnknownComponent is returned when a name does not resolve: the
// composition configuration selects a name no registry carries (the
// did-you-mean suggestion comes from the registered names), or Build is
// asked for a name the assembly has not selected. The error names the
// component.
var ErrUnknownComponent = errors.New("pkgcore: unknown component")

// ErrMissingRequirement is returned when something required is absent: a
// selected component requires a contract token no selected component
// provides and no uniquely-available registered component can satisfy (the
// error lists the candidates), the composition configuration itself has not
// been put before Prepare, or a Get call finds no value assignable to the
// requested type.
var ErrMissingRequirement = errors.New("pkgcore: missing requirement")

// ErrAmbiguousProvider is returned when one single-value reading meets more
// than one provider: several selected components provide the same required
// token, several registered components could satisfy a token being
// auto-pulled, or several put values are assignable to the type a Get call
// requests. The error lists the providers, so the remedy -- keep one -- is
// actionable.
var ErrAmbiguousProvider = errors.New("pkgcore: ambiguous provider")

// ErrComponentFailed is returned when a component callback fails in any
// stage from Construct on: the error names the stage, the component, the
// cause, and every component that was rolled back in reverse dependency
// order. The rollback itself -- closing every constructed component exactly
// once -- has already run when this error is returned, so a host never has
// to attempt a partial teardown of its own.
var ErrComponentFailed = errors.New("pkgcore: component failed")

// ErrStageViolation is returned when the lifecycle or a declaration seat is
// used out of order: a stage method called before the stage it requires, a
// stage run twice, a write to a declaration seat outside the Init stage, or
// any stage attempted after Close. The error names the stage or seat the
// call needed and the stage the assembly is actually in.
var ErrStageViolation = errors.New("pkgcore: stage violation")

// ErrInvalidAsset is returned when a selected component's embedded asset
// fails the Prepare stage's validation -- locale key-set parity, the shape
// of a migration set, or an OpenAPI fragment that is not a usable document.
// The error names the component and wraps the cause.
var ErrInvalidAsset = errors.New("pkgcore: invalid component asset")

// Stage identifies the lifecycle position of the assembly: one of the eight
// stages the component lifecycle walks through, or StageIdle before the
// first stage has run.
//
// The type is exported because the current stage is part of the assembly
// contract rather than a registry internal: a component that owns a
// declaration face of its own -- a write surface whose gate is "writes only
// while this stage runs", the shape this registry's built-in seats take --
// enforces that gate by comparing the reading of (*ComponentRegistry).Stage
// against the stage it allows, instead of re-deriving the lifecycle
// position from its own bookkeeping.
type Stage string

const (
	// StageIdle is the assembly before any stage has run (and after a failed
	// Prepare, which leaves the registry as it was). Its value is empty, so
	// a zero Stage also reads as idle; String renders it as "not started".
	StageIdle Stage = ""
	// StagePrepare is the first stage: the loader's configuration load, the
	// assembly plan and its validation, then the components' Prepare
	// callbacks.
	StagePrepare Stage = "prepare"
	// StageConstruct is the second stage: every planned component's New
	// callback, in dependency order.
	StageConstruct Stage = "construct"
	// StageVerify is the third stage: the database component applies the
	// assembled migrations first, then every constructed component checks
	// its own preconditions.
	StageVerify Stage = "verify"
	// StageInit is the fourth stage: the declaration seats open, the
	// components' Init callbacks declare, wire and publish, and the closing
	// validation runs.
	StageInit Stage = "init"
	// StageStart is the fifth stage: the components' Start callbacks --
	// workers, schedulers, seeds, everything that starts the component up
	// without admitting outside traffic.
	StageStart Stage = "start"
	// StageServe is the sixth stage: the components' Serve callbacks -- the
	// entry points that accept external requests (HTTP listeners, queue
	// consumption, scheduler triggering) -- after every Start callback of
	// the assembly has completed.
	StageServe Stage = "serve"
	// StageStop is the seventh stage: the non-blocking stop notification, in
	// the two beats Stop documents.
	StageStop Stage = "stop"
	// StageClose is the eighth stage: the release pass, waiting out the
	// drain Stop announced.
	StageClose Stage = "close"
)

// String renders the stage name used in error text: the stage's own value,
// and "not started" for StageIdle (whose empty value would otherwise render
// as nothing).
func (s Stage) String() string {
	if s == StageIdle {
		return "not started"
	}
	return string(s)
}

// plannedComponent is one member of the assembled set: the selected
// descriptor, the configuration its New receives, and whether auto-pull
// selected it.
type plannedComponent struct {
	component Component
	cfg       ComponentConfig
	auto      bool
}

// constructedEntry is one constructed member, recorded in construction
// order; reversal gives the Stop and Close order.
type constructedEntry struct {
	name      string
	component Component
	instance  any
}

// seatSet holds the in-memory registrar behind each declaration seat once
// the Init stage has opened the seats. A zero seatSet means the seats have
// not been opened: reads see nothing declared, writes are refused before
// they get here.
type seatSet struct {
	config        ConfigSchemaRegistrar
	features      FeatureRegistrar
	permissions   PermissionRegistrar
	jobs          JobHandlerRegistrar
	notifications NotificationRegistrar
	events        EventRegistrar
	auditActions  AuditActionRegistrar
	retention     RetentionRegistrar
	schedules     PeriodicTaskRegistrar
}

// ComponentRegistry is the core data structure of the assembly: one instance
// answers which components exist (the registration map), which components
// the application is made of (the assembly plan, in dependency order), and
// where the build stands (the lifecycle state). One instance per assembly --
// tests and repeated in-process assemblies each create their own, so their
// registrations, values and declaration seats never leak into one another.
//
// The nine declaration seats are the fields below; they accept writes
// only while the Init stage runs, and a write outside it is refused naming
// the stage. The by-type value context is Put and Get; the assembly puts
// every component's product there as it constructs it. The HTTP declaration
// faces -- routes and platform middleware -- are not registry seats: they
// belong to the http component (go/app/httpserve) and are consumed through
// its product, pkgcore.RouteRegistrar and pkgcore.MiddlewareRegistrar.
type ComponentRegistry struct {
	// Config receives the runtime configuration schema components declare.
	Config ConfigSchemaRegistrar
	// Features receives the feature flags components declare.
	Features FeatureRegistrar
	// Permissions receives the resource:action permissions components define.
	Permissions PermissionRegistrar
	// Jobs receives the asynchronous job handlers components provide.
	Jobs JobHandlerRegistrar
	// Notifications receives the notification types components can emit.
	Notifications NotificationRegistrar
	// Events receives the domain events components publish and the
	// subscriptions they install on the assembled EventBus value.
	Events EventRegistrar
	// AuditActions receives the audit action enumeration components define.
	AuditActions AuditActionRegistrar
	// Retention receives the retention-sweep, right-to-erasure and
	// data-export participants components register.
	Retention RetentionRegistrar
	// Schedules receives the periodic tasks components declare.
	Schedules PeriodicTaskRegistrar

	mu sync.RWMutex

	// byName and order are the registration info: every component this
	// instance knows, keyed by name, with the registration order that
	// orders the Prepare callbacks.
	byName map[string]Component
	order  []string

	// values is the by-type context Put fills and Get reads.
	values []any

	// plan is the assembly info: the selected components in dependency
	// order, written by a successful Prepare. It stays unexported; Build,
	// MemberNames, Members and Assets are its public readers.
	plan       []plannedComponent
	planByName map[string]int

	// cur is the stage currently running (or the last one entered);
	// prepared through served record stage completion, and closed records
	// the terminal state Close establishes.
	cur         Stage
	prepared    bool
	constructed bool
	verified    bool
	inited      bool
	started     bool
	served      bool
	closed      bool

	// seats is the registrar set behind the seats, non-nil once Init has
	// opened them; seatsOpen is the gate the seat write methods check.
	seats     seatSet
	seatsOpen bool

	// constructedEntries records every constructed member in construction
	// order; closeRolled and closeErr cache the exactly-once Close result
	// (including a rollback's).
	constructedEntries []constructedEntry
	closeRolled        []string
	closeErr           error

	// preparing is the planned member whose Prepare callback is currently
	// running. The stage drives one callback at a time, so a Prepare
	// callback reads its own resolved configuration through
	// OwnComponentConfig without naming itself -- a host that selects a
	// renamed copy of the component still gets the copy's block.
	preparing *plannedComponent
}

// NewComponentRegistry returns a ComponentRegistry seeded with a snapshot of
// the package-level global registration: descriptors are immutable data, so
// they are copied by reference and the instance never mutates the global
// set. Host-local components join the instance afterwards through Register.
func NewComponentRegistry() *ComponentRegistry {
	r := &ComponentRegistry{
		byName:     make(map[string]Component),
		planByName: make(map[string]int),
	}

	globalComponents.mu.RLock()
	for _, name := range globalComponents.order {
		r.byName[name] = globalComponents.byName[name]
	}
	r.order = append(r.order, globalComponents.order...)
	globalComponents.mu.RUnlock()

	r.Config = &configSeat{gatedSeatFor(r, "Config", func(s seatSet) ConfigSchemaRegistrar { return s.config })}
	r.Features = &featuresSeat{gatedSeatFor(r, "Features", func(s seatSet) FeatureRegistrar { return s.features })}
	r.Permissions = &permissionsSeat{gatedSeatFor(r, "Permissions", func(s seatSet) PermissionRegistrar { return s.permissions })}
	r.Jobs = &jobsSeat{gatedSeatFor(r, "Jobs", func(s seatSet) JobHandlerRegistrar { return s.jobs })}
	r.Notifications = &notificationsSeat{gatedSeatFor(r, "Notifications", func(s seatSet) NotificationRegistrar { return s.notifications })}
	r.Events = &eventsSeat{gatedSeatFor(r, "Events", func(s seatSet) EventRegistrar { return s.events })}
	r.AuditActions = &auditActionsSeat{gatedSeatFor(r, "AuditActions", func(s seatSet) AuditActionRegistrar { return s.auditActions })}
	r.Retention = &retentionSeat{gatedSeatFor(r, "Retention", func(s seatSet) RetentionRegistrar { return s.retention })}
	r.Schedules = &schedulesSeat{gatedSeatFor(r, "Schedules", func(s seatSet) PeriodicTaskRegistrar { return s.schedules })}
	return r
}

// Register adds c to this instance's registration, after the components the
// instance was seeded with. It returns an error wrapping ErrInvalidComponent
// for a malformed descriptor and one wrapping ErrDuplicateComponent when the
// name is already registered on this instance; nothing is registered when
// the call returns an error.
func (r *ComponentRegistry) Register(c Component) error {
	if err := validateComponent(c); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byName[c.Name]; exists {
		return fmt.Errorf("%w (stage registration): component %q is already registered; component names are unique within one registry", ErrDuplicateComponent, c.Name)
	}
	r.byName[c.Name] = c
	r.order = append(r.order, c.Name)
	return nil
}

// nilValue reports whether v is nil: the untyped nil an interface holds
// directly, or a typed nil pointer. A typed nil is a value whose dynamic type
// is present -- so a type-addressed read matches it -- while the value itself
// is absent, the "present but nil" reading every consumer would only trip
// over at first use.
func nilValue(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

// Put adds v to the by-type value context: a component's product, a runtime
// service published during Init, or a value the host injects before Prepare
// (its own data, or the code-override layer of the configuration chain). All
// consumers read one shared context, addressed by type. Put is append-only,
// so no value is ever displaced by a later one.
//
// A nil v panics, untyped or typed: an untyped nil matches no type and would
// leave a later Get reporting a silent absence, while a typed nil pointer
// matches its type and would leave a later Get reporting a present-but-nil
// value no consumer can use -- the same silent lie with the opposite sign.
func (r *ComponentRegistry) Put(v any) {
	if nilValue(v) {
		panic("pkgcore: Put requires a non-nil value: an untyped nil matches no type, and a typed nil pointer is a present-but-nil value no consumer can use")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values = append(r.values, v)
}

// valueCount returns how many values the by-type context holds. It brackets
// a New call so the values that call put itself can be told apart from the
// values already there.
func (r *ComponentRegistry) valueCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.values)
}

// valuesFrom returns a copy of the values put at or after index i.
func (r *ComponentRegistry) valuesFrom(i int) []any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if i >= len(r.values) {
		return nil
	}
	return append([]any(nil), r.values[i:]...)
}

// Get returns the value in the by-type context that T addresses: exactly one
// put value whose type is assignable to T is the answer. No assignment is an
// error wrapping ErrMissingRequirement naming T; more than one is an error
// wrapping ErrAmbiguousProvider listing the available types. Get runs
// against the live context, so a value Put after construction -- a runtime
// service a component publishes during Init -- is reachable from the moment
// it is put.
func Get[T any](r *ComponentRegistry) (T, error) {
	var zero T
	target := reflect.TypeFor[T]()

	r.mu.RLock()
	defer r.mu.RUnlock()

	var matches []any
	var types []string
	for _, v := range r.values {
		t := reflect.TypeOf(v)
		if t == nil || !t.AssignableTo(target) {
			continue
		}
		matches = append(matches, v)
		types = append(types, t.String())
	}

	switch len(matches) {
	case 1:
		// Assignable implies convertible (assignment is one of Go's
		// conversion cases), so the Convert call and the following type
		// assertion both hold by construction; the checked forms keep a
		// future change to the matching rule from panicking here.
		converted, ok := reflect.ValueOf(matches[0]).Convert(target).Interface().(T)
		if !ok {
			return zero, fmt.Errorf("pkgcore: the put value of type %s did not convert to %s", reflect.TypeOf(matches[0]), target)
		}
		return converted, nil
	case 0:
		return zero, fmt.Errorf("%w: no put value is assignable to %s; put a value of that type before requiring it", ErrMissingRequirement, target)
	default:
		return zero, fmt.Errorf("%w: %d put values are assignable to %s (%s); keep one", ErrAmbiguousProvider, len(matches), target, strings.Join(types, ", "))
	}
}

// GetOptional returns the value in the by-type context that T addresses,
// reporting absence as a fact rather than as an error. No put value
// assignable to T yields (zero, false, nil); exactly one yields the value
// with ok true; every other error Get can report -- an error wrapping
// ErrAmbiguousProvider when several put values are assignable, or the
// single-match conversion failure -- is returned as-is with ok false.
//
// It is the reading for an optional dependency, whose absence has a
// documented default:
//
//	mailer, ok, err := pkgcore.GetOptional[pkgcore.Mailer](reg)
//	if err != nil {
//		return nil, err
//	}
//	if ok {
//		opts = append(opts, WithMailer(mailer))
//	}
//
// The caller branches on ok -- the absent case is a legitimate
// configuration, never an error -- while a true error keeps whatever the
// caller's own contract says, conventionally propagation. Get stays the
// reading for a required dependency, where absence is itself the error.
func GetOptional[T any](r *ComponentRegistry) (T, bool, error) {
	var zero T
	v, err := Get[T](r)
	if err != nil {
		if errors.Is(err, ErrMissingRequirement) {
			return zero, false, nil
		}
		return zero, false, err
	}
	return v, true, nil
}

// Prepare runs the first stage: the assembly reads the composition
// configuration the host put, parses it strictly, expands the selection
// (including auto-pull of uniquely-available providers unless strict is set),
// validates dependencies, configuration keys, capabilities, assets and the
// dependency graph, and writes the resulting plan -- all before anything is
// constructed -- and then runs every selected component's Prepare callback
// in registration order.
//
// A failure returns before any construction, so nothing needs rolling back;
// a failed Prepare leaves the registry as it was, ready to retry with a
// corrected composition.
func (r *ComponentRegistry) Prepare(ctx context.Context) error {
	if err := r.beginStage(StagePrepare); err != nil {
		return err
	}
	if err := r.planAssembly(ctx); err != nil {
		r.abandonStage()
		return err
	}

	for _, name := range r.registeredOrder() {
		p, selected := r.planned(name)
		if !selected || p.component.Prepare == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			r.abandonStage()
			return fmt.Errorf("pkgcore: component %q (stage prepare): %w", name, err)
		}
		r.setPreparing(p)
		err := p.component.Prepare(ctx, r)
		r.clearPreparing()
		if err != nil {
			r.abandonStage()
			return fmt.Errorf("pkgcore: component %q (stage prepare): %w", name, err)
		}
	}

	r.markStageDone(StagePrepare)
	return nil
}

// Construct runs the second stage: every planned component's New callback,
// in dependency order, each product put into the by-type context so the
// components constructed after it can reach it -- except a catalog
// member's product, which stays out of that context because the catalog
// registration is its delivery (Members reads it by name; Get must never
// see it). A New that returns no product fails the stage.
//
// A failure -- here or in any later stage -- closes every constructed
// component in reverse order, exactly once, and returns ErrComponentFailed
// naming the stage, the component, the cause and the rolled-back set.
func (r *ComponentRegistry) Construct(ctx context.Context) error {
	if err := r.beginStage(StageConstruct); err != nil {
		return err
	}

	for _, p := range r.plannedComponents() {
		name := p.component.Name
		if err := ctx.Err(); err != nil {
			return r.failStage(ctx, StageConstruct, name, err)
		}
		before := r.valueCount()
		instance, err := p.component.New(ctx, r, p.cfg)
		if err != nil {
			return r.failStage(ctx, StageConstruct, name, err)
		}
		if nilValue(instance) {
			return r.failStage(ctx, StageConstruct, name, errors.New("the New callback returned no product (a nil or typed nil pointer value)"))
		}
		// The values the New callback put itself are, by construction, this
		// component's own additional deliveries: the stage drives one New at
		// a time, so everything appended to the by-type context between the
		// count above and now was put by this call.
		put := r.valuesFrom(before)
		if err := productDeliversDeclarations(p.component, instance, put); err != nil {
			return r.failStage(ctx, StageConstruct, name, err)
		}
		if err := memberDeliversDeclarations(p.component, instance, put); err != nil {
			return r.failStage(ctx, StageConstruct, name, err)
		}
		r.recordConstructed(p.component, instance)
		if len(p.component.ProvidesMember) == 0 {
			r.Put(instance)
		}
	}

	r.markStageDone(StageConstruct)
	return nil
}

// Verify runs the third stage: every constructed component's Verify callback
// in dependency order. It runs after every product exists, so a database
// component applies the assembled migration set here, before any other
// Verify callback runs.
func (r *ComponentRegistry) Verify(ctx context.Context) error {
	if err := r.beginStage(StageVerify); err != nil {
		return err
	}
	for _, e := range r.constructedList() {
		if err := ctx.Err(); err != nil {
			return r.failStage(ctx, StageVerify, e.name, err)
		}
		if e.component.Verify == nil {
			continue
		}
		if err := e.component.Verify(ctx, r, e.instance); err != nil {
			return r.failStage(ctx, StageVerify, e.name, err)
		}
	}
	r.markStageDone(StageVerify)
	return nil
}

// Init runs the fourth stage: every selected component's declared system
// purposes register first, at the stage's entry and before any Init
// callback runs -- a callback may open a system context under a purpose
// another selected component declared, so the registration cannot wait for
// the closing validation. Then the declaration seats open, every
// constructed component's Init callback runs in dependency order (declaring
// into the seats, wiring dependencies and publishing runtime services with
// Put), the seats close again, and the assembly's closing validation runs
// over the finished declarations: the one-key-one-layer check between the
// selected components' key material (BootstrapKeys declarations and
// ConfigSchema derive fields) and the runtime configuration seat, and the
// feature-graph check (ValidateFeatureGraph over the Features seat).
func (r *ComponentRegistry) Init(ctx context.Context) error {
	if err := r.beginStage(StageInit); err != nil {
		return err
	}

	if err := r.registerSystemPurposes(); err != nil {
		return r.failStage(ctx, StageInit, "", err)
	}

	r.openSeats()
	for _, e := range r.constructedList() {
		if err := ctx.Err(); err != nil {
			return r.failStage(ctx, StageInit, e.name, err)
		}
		if e.component.Init == nil {
			continue
		}
		if err := e.component.Init(ctx, r, e.instance); err != nil {
			return r.failStage(ctx, StageInit, e.name, err)
		}
	}
	r.closeSeats()

	if err := r.validateOneLayerPerKey(); err != nil {
		return r.failStage(ctx, StageInit, "", err)
	}
	if err := ValidateFeatureGraph(r.Features); err != nil {
		return r.failStage(ctx, StageInit, "", err)
	}

	r.markStageDone(StageInit)
	return nil
}

// Start runs the fifth stage: every constructed component's Start callback
// in dependency order, after the closing validation of Init -- workers,
// schedulers, seeds, everything that starts the component up without
// admitting outside traffic. A component whose action begins accepting
// outside traffic belongs to the Serve stage instead, so it runs after
// every component's Start rather than merely after its own dependencies;
// see Serve. A Start failure rolls the assembly back exactly like a
// Construct, Verify or Init failure.
func (r *ComponentRegistry) Start(ctx context.Context) error {
	if err := r.beginStage(StageStart); err != nil {
		return err
	}
	for _, e := range r.constructedList() {
		if err := ctx.Err(); err != nil {
			return r.failStage(ctx, StageStart, e.name, err)
		}
		if e.component.Start == nil {
			continue
		}
		if err := e.component.Start(ctx, r, e.instance); err != nil {
			return r.failStage(ctx, StageStart, e.name, err)
		}
	}
	r.markStageDone(StageStart)
	return nil
}

// Serve runs the sixth stage: every constructed component's Serve callback
// in dependency order, as one round that begins only after every Start
// callback of the assembly has completed. The stage's members are the
// entry points that accept external requests -- HTTP listening, queue
// consumption, scheduler triggering, every action that starts admitting
// outside traffic into the process.
//
// It is a stage of its own rather than part of Start because dependency
// order cannot express its requirement: an entry's traffic can reach any
// component, not only the components the entry declares as dependencies,
// so the entry must run after everyone -- "after all Starts" -- while a
// dependency edge only ever says "after my dependencies". A component that
// declares no Serve callback is unaffected by the stage. A Serve failure
// rolls the assembly back exactly like a Start failure.
func (r *ComponentRegistry) Serve(ctx context.Context) error {
	if err := r.beginStage(StageServe); err != nil {
		return err
	}
	for _, e := range r.constructedList() {
		if err := ctx.Err(); err != nil {
			return r.failStage(ctx, StageServe, e.name, err)
		}
		if e.component.Serve == nil {
			continue
		}
		if err := e.component.Serve(ctx, r, e.instance); err != nil {
			return r.failStage(ctx, StageServe, e.name, err)
		}
	}
	r.markStageDone(StageServe)
	return nil
}

// Stop runs the seventh stage: the non-blocking stop notification, in two
// beats. The first beat reaches the components that declared a Serve
// callback -- the entry points stop accepting new requests first, so the
// requests in flight drain against a still-complete system -- and only then
// does the second beat reach every other constructed component. Each beat
// runs in reverse dependency order among its own members. Failures are
// ignored in both beats, because a notification that cannot be delivered
// must not keep the Close that waits for the drain from running; Stop
// releases nothing itself.
func (r *ComponentRegistry) Stop(ctx context.Context) error {
	if err := r.checkNotClosed(StageStop); err != nil {
		return err
	}
	r.setStage(StageStop)

	notify := func(e constructedEntry) {
		if e.component.Stop == nil {
			return
		}
		_ = e.component.Stop(ctx, r, e.instance)
	}
	entries := r.constructedList()
	// First beat: the entry points -- the components that declared Serve --
	// in reverse dependency order.
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].component.Serve != nil {
			notify(entries[i])
		}
	}
	// Second beat: every remaining constructed component, in reverse
	// dependency order.
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].component.Serve == nil {
			notify(entries[i])
		}
	}
	return nil
}

// Close runs the eighth stage: every constructed component's release in
// reverse dependency order, waiting out the drain Stop announced. It runs
// exactly once -- a second call reports the first call's result -- and
// aggregates the failures of every release rather than stopping at the
// first. The registry also calls Close itself when any stage from Construct
// on fails, so a failed assembly is always fully torn down before its error
// reaches the host; a host that joins reg.Close(ctx) into the failure it is
// handling observes the same, already computed result.
func (r *ComponentRegistry) Close(ctx context.Context) error {
	r.mu.Lock()
	if r.closed {
		err := r.closeErr
		r.mu.Unlock()
		return err
	}
	r.closed = true
	r.seatsOpen = false
	r.cur = StageClose
	entries := append([]constructedEntry(nil), r.constructedEntries...)
	r.mu.Unlock()

	rolled, err := r.closeEntries(ctx, entries)

	r.mu.Lock()
	r.closeRolled = rolled
	r.closeErr = err
	r.mu.Unlock()
	return err
}

// closeEntries releases entries in reverse construction order, aggregating
// failures, and returns the names that were released. A component whose
// descriptor declares a Close callback is released through it, and the
// callback owns the release entirely. A component that declares none is
// released through its product's own Close() error when it has one -- the
// standard io.Closer declaration of resource ownership -- so an
// implementation whose constructor builds the resources it owns needs no
// adapter callback on its descriptor. A product without a Close method has
// nothing to release and is skipped.
func (r *ComponentRegistry) closeEntries(ctx context.Context, entries []constructedEntry) ([]string, error) {
	var rolled []string
	var errs []error
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		var closeErr error
		if e.component.Close != nil {
			closeErr = e.component.Close(ctx, r, e.instance)
		} else if closable, ok := e.instance.(io.Closer); ok {
			closeErr = closable.Close()
		} else {
			continue
		}
		rolled = append(rolled, e.name)
		if closeErr != nil {
			errs = append(errs, fmt.Errorf("pkgcore: component %q (stage close): %w", e.name, closeErr))
		}
	}
	return rolled, errors.Join(errs...)
}

// failStage rolls the assembly back after a post-Construct failure and
// returns the four-element error: stage, component (or "the assembly" for a
// validation with no single owner), cause, and the rolled-back set. The
// Close errors of the rollback join the result, so nothing a Close callback
// reports is lost on the failure path.
func (r *ComponentRegistry) failStage(ctx context.Context, s Stage, name string, cause error) error {
	_ = r.Close(ctx) // rollback: exactly once, cached for any later Close call

	r.mu.RLock()
	rolled, closeErr := r.closeRolled, r.closeErr
	r.mu.RUnlock()

	owner := "the assembly"
	if name != "" {
		owner = fmt.Sprintf("component %q", name)
	}
	stageErr := fmt.Errorf("%w (stage %s): %s: %w; rolled back: %s", ErrComponentFailed, s, owner, cause, joinNames(rolled))
	if closeErr != nil {
		return errors.Join(stageErr, closeErr)
	}
	return stageErr
}

// plannedComponents returns a copy of the assembly plan in dependency order.
func (r *ComponentRegistry) plannedComponents() []plannedComponent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]plannedComponent(nil), r.plan...)
}

// planned returns the planned entry for name.
func (r *ComponentRegistry) planned(name string) (plannedComponent, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	i, ok := r.planByName[name]
	if !ok {
		return plannedComponent{}, false
	}
	return r.plan[i], true
}

// registeredOrder returns the registration order.
func (r *ComponentRegistry) registeredOrder() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// setPreparing records the planned member whose Prepare callback is about
// to run.
func (r *ComponentRegistry) setPreparing(p plannedComponent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.preparing = &p
}

// clearPreparing clears the current Prepare callback's member.
func (r *ComponentRegistry) clearPreparing() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.preparing = nil
}

// OwnComponentConfig returns the resolved configuration block of the
// component whose Prepare callback is currently running: the
// components.<name> subtree the assembly resolved for exactly that member
// of the plan, under whatever name the host selected it.
//
// It is the reading a Prepare callback uses to see its own configuration
// without naming itself. A component package ships one descriptor whose
// callbacks are bound to the package, but a host may register a renamed
// copy of that descriptor; a literal components.<package-name> lookup would
// then silently read the original's block -- absent, or worse, another
// assembly's -- while this reading follows the copy. Outside a Prepare
// callback it fails with ErrStageViolation, because no component's
// configuration is "current" anywhere else; New receives its block as its
// cfg parameter instead.
func OwnComponentConfig(r *ComponentRegistry) (ComponentConfig, error) {
	r.mu.RLock()
	p := r.preparing
	r.mu.RUnlock()
	if p == nil {
		return ComponentConfig{}, fmt.Errorf("%w: no component Prepare callback is running; a component reads its own configuration through OwnComponentConfig only inside its Prepare callback, and New receives its resolved block as the cfg parameter", ErrStageViolation)
	}
	return p.cfg, nil
}

// registered returns the component registered under name on this instance.
func (r *ComponentRegistry) registered(name string) (Component, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.byName[name]
	return c, ok
}

// constructedList returns a copy of the constructed entries in construction
// order.
func (r *ComponentRegistry) constructedList() []constructedEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]constructedEntry(nil), r.constructedEntries...)
}

// recordConstructed appends one constructed member.
func (r *ComponentRegistry) recordConstructed(c Component, instance any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.constructedEntries = append(r.constructedEntries, constructedEntry{name: c.Name, component: c, instance: instance})
}

// constructedInstance returns the product recorded for the constructed
// component named name, and whether that component has been constructed.
func (r *ComponentRegistry) constructedInstance(name string) (any, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.constructedEntries {
		if e.name == name {
			return e.instance, true
		}
	}
	return nil, false
}

// beginStage validates that s may run now and records it as the current
// stage.
func (r *ComponentRegistry) beginStage(s Stage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkStageOrder(s); err != nil {
		return err
	}
	r.cur = s
	return nil
}

// checkNotClosed refuses a stage once the registry is closed.
func (r *ComponentRegistry) checkNotClosed(s Stage) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.closed {
		return nil
	}
	return fmt.Errorf("%w: the %s stage cannot run: the assembly is closed", ErrStageViolation, s)
}

// checkStageOrder reports whether s may run given the completed stages,
// naming the stage the call needed and the state observed. The caller holds
// the write lock.
func (r *ComponentRegistry) checkStageOrder(s Stage) error {
	if r.closed {
		return fmt.Errorf("%w: the %s stage cannot run: the assembly is closed", ErrStageViolation, s)
	}
	need := func(ok bool, requirement string) error {
		if !ok {
			return fmt.Errorf("%w: the %s stage requires %s; current stage is %s", ErrStageViolation, s, requirement, r.cur)
		}
		return nil
	}
	switch s {
	case StagePrepare:
		if r.prepared {
			return fmt.Errorf("%w: the Prepare stage already ran", ErrStageViolation)
		}
	case StageConstruct:
		if err := need(r.prepared, "a completed Prepare stage"); err != nil {
			return err
		}
		if r.constructed {
			return fmt.Errorf("%w: the Construct stage already ran", ErrStageViolation)
		}
	case StageVerify:
		if err := need(r.constructed, "a completed Construct stage"); err != nil {
			return err
		}
		if r.verified {
			return fmt.Errorf("%w: the Verify stage already ran", ErrStageViolation)
		}
	case StageInit:
		if err := need(r.verified, "a completed Verify stage"); err != nil {
			return err
		}
		if r.inited {
			return fmt.Errorf("%w: the Init stage already ran", ErrStageViolation)
		}
	case StageStart:
		if err := need(r.inited, "a completed Init stage"); err != nil {
			return err
		}
		if r.started {
			return fmt.Errorf("%w: the Start stage already ran", ErrStageViolation)
		}
	case StageServe:
		if err := need(r.started, "a completed Start stage"); err != nil {
			return err
		}
		if r.served {
			return fmt.Errorf("%w: the Serve stage already ran", ErrStageViolation)
		}
	}
	return nil
}

// markStageDone records a completed stage and advances the current stage
// marker.
func (r *ComponentRegistry) markStageDone(s Stage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch s {
	case StagePrepare:
		r.prepared = true
	case StageConstruct:
		r.constructed = true
	case StageVerify:
		r.verified = true
	case StageInit:
		r.inited = true
	case StageStart:
		r.started = true
	case StageServe:
		r.served = true
	}
	r.cur = s
}

// abandonStage rolls the stage marker back so a failed Prepare leaves the
// registry as it was, ready for a retry with a corrected composition.
func (r *ComponentRegistry) abandonStage() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cur = StageIdle
}

// setStage records the stage currently running without completion
// bookkeeping (Stop is a terminal notification, not a gated stage).
func (r *ComponentRegistry) setStage(s Stage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cur = s
}

// openSeats builds the registrar set behind the declaration seats and opens
// them. It runs at the start of Init; before it, the seats are closed and
// writes are refused naming the stage.
func (r *ComponentRegistry) openSeats() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seats = seatSet{
		config:        &memoryConfigRegistrar{keys: make(map[string]struct{})},
		features:      &memoryFeatureRegistrar{keys: make(map[string]struct{})},
		permissions:   &memoryPermissionRegistrar{perms: make(map[string]struct{})},
		jobs:          &memoryJobRegistrar{handlers: make(map[string]any)},
		notifications: &memoryNotificationRegistrar{keys: make(map[string]struct{})},
		events:        &memoryEventRegistrar{types: make(map[string]struct{})},
		auditActions:  &memoryAuditActionRegistrar{actions: make(map[string]struct{})},
		retention:     &memoryRetentionRegistrar{names: make(map[string]struct{})},
		schedules:     &memoryScheduleRegistrar{types: make(map[string]struct{})},
	}
	r.seatsOpen = true
}

// closeSeats closes the declaration seats again once every Init callback has
// run, so the closing validation and every later stage read declarations no
// component can still add to.
func (r *ComponentRegistry) closeSeats() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seatsOpen = false
}

// seatFor returns the registrar set behind the seats for a write: it fails
// with ErrStageViolation naming the seat and the stage when the seats are
// closed, which is every moment outside the Init stage.
func (r *ComponentRegistry) seatFor(seat string) (seatSet, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.seatsOpen {
		return r.seats, nil
	}
	if r.closed {
		return seatSet{}, fmt.Errorf("%w: the %q seat accepts writes only during the Init stage: the assembly is closed", ErrStageViolation, seat)
	}
	return seatSet{}, fmt.Errorf("%w: the %q seat accepts writes only during the Init stage; current stage is %s", ErrStageViolation, seat, r.cur)
}

// seatRead returns the registrar set behind the seats for a read. Before the
// seats open, nothing has been declared, so the zero set answers reads with
// empty collections instead of failing: reads are legal at any point, writes
// are not.
func (r *ComponentRegistry) seatRead() seatSet {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.seats
}

// validateOneLayerPerKey refuses key material declared on two layers: a
// selected component's BootstrapKeys declaration or ConfigSchema derive
// field (the process-start layer, resolved by the loader before construction)
// and an item declared on the runtime Config seat. One dotted key carries
// one meaning, one default and one edit surface, so key material on both
// layers fails the Init stage's closing validation rather than leaving an
// operator unable to tell which layer a change hits.
//
// The rule covers key material alone, not every schema field: a component's
// ordinary configuration field may deliberately share a path with a runtime
// item of the same name (the construction-time value its own runtime reads
// fall back to, for example), while key material has no fallback semantics
// to chain across the layers.
func (r *ComponentRegistry) validateOneLayerPerKey() error {
	runtimeKeys := make(map[string]struct{})
	for _, item := range r.Config.Items() {
		runtimeKeys[item.Key] = struct{}{}
	}

	var conflicts []error
	for _, p := range r.plannedComponents() {
		for _, key := range p.component.BootstrapKeys {
			if _, both := runtimeKeys[key.Key]; !both {
				continue
			}
			conflicts = append(conflicts, fmt.Errorf(
				"component %q declares bootstrap key %q, which the runtime configuration seat also carries",
				p.component.Name, key.Key,
			))
		}
		for _, keyPath := range deriveKeyPaths(p.component) {
			if _, both := runtimeKeys[keyPath]; !both {
				continue
			}
			conflicts = append(conflicts, fmt.Errorf(
				"component %q declares key material at %q, which the runtime configuration seat also carries",
				p.component.Name, keyPath,
			))
		}
	}
	if len(conflicts) == 0 {
		return nil
	}
	return fmt.Errorf("pkgcore: key declared on two layers: %w", errors.Join(conflicts...))
}

// deriveKeyPaths returns the final key paths of a component's ConfigSchema
// derive fields: the component's declared key material, resolved under the
// namespace prefix its registration declares like every other schema field.
// A schema the prepare stage already validated cannot fail to analyze here;
// a nil schema declares nothing.
func deriveKeyPaths(c Component) []string {
	fields, err := analyzeConfigSchema(c.ConfigSchema)
	if err != nil {
		return nil
	}
	prefix, err := configKeyPrefix(c.Name, c.ConfigNamespace)
	if err != nil {
		return nil
	}
	var paths []string
	for _, f := range fields {
		if f.derive {
			paths = append(paths, prefix+f.key)
		}
	}
	return paths
}

// registerSystemPurposes collects every selected component's
// SystemPurposes declarations and registers them together: a purpose two
// components both declare fails before anything is registered (the set
// WithSystemContext accepts must be exactly the set the assembly agreed
// on), and a clean set is registered in plan order.
func (r *ComponentRegistry) registerSystemPurposes() error {
	owner := make(map[SystemPurpose]string)
	var duplicated []error
	for _, p := range r.plannedComponents() {
		for _, purpose := range p.component.SystemPurposes {
			if first, exists := owner[purpose]; exists {
				duplicated = append(duplicated, fmt.Errorf(
					"system purpose %q is declared by both component %q and component %q",
					purpose, first, p.component.Name,
				))
				continue
			}
			owner[purpose] = p.component.Name
		}
	}
	if len(duplicated) > 0 {
		return fmt.Errorf("pkgcore: duplicate system purpose declarations: %w", errors.Join(duplicated...))
	}
	for _, p := range r.plannedComponents() {
		for _, purpose := range p.component.SystemPurposes {
			RegisterSystemPurpose(purpose)
		}
	}
	return nil
}

// productDeliversDeclarations asserts the delivery promise a component's
// Provides declarations make: for every declared token, the values this
// construction delivered -- the product New returned, plus the values New
// itself put into the by-type context (put carries them) -- must include
// one that satisfies it. A declaration without a matching delivery means
// the descriptor promised a product this component never produces: every
// requirement resolution resting on that declaration would fail only
// later, at Get time (or be silently mis-served by another component's
// value), so the construction fails here instead, naming every undelivered
// declaration while the component is still on the stack.
func productDeliversDeclarations(c Component, instance any, put []any) error {
	if len(c.Provides) == 0 {
		return nil
	}
	product := reflect.TypeOf(instance)
	var undelivered []string
	for _, declared := range c.Provides {
		token := reflect.TypeOf(declared)
		if productMatchesToken(product, token) {
			continue
		}
		delivered := false
		for _, v := range put {
			if productMatchesToken(reflect.TypeOf(v), token) {
				delivered = true
				break
			}
		}
		if !delivered {
			undelivered = append(undelivered, tokenDisplay(token))
		}
	}
	if len(undelivered) == 0 {
		return nil
	}
	return fmt.Errorf("the construction delivered neither the product %s nor a value put during New for the declared token(s) %s; a Provides declaration promises a construction-time delivery, so a declaration the component cannot deliver must be removed (or the delivering value put inside New)", product, strings.Join(undelivered, ", "))
}

// memberDeliversDeclarations asserts the delivery promise a component's
// ProvidesMember declarations make: the product New returned must satisfy
// every declared token, and nothing the New call put into the by-type
// context may satisfy one -- a catalog member's delivery is the catalog
// registration itself (Members reads the product by name), so a member
// value inside the single-value context would put a token there the
// contract keeps out of every by-type reading. A declaration whose product
// does not answer it is a descriptor promising a contract this component
// never delivers, refused here while the component is still on the stack.
func memberDeliversDeclarations(c Component, instance any, put []any) error {
	if len(c.ProvidesMember) == 0 {
		return nil
	}
	product := reflect.TypeOf(instance)
	var undelivered, leaked []string
	for _, declared := range c.ProvidesMember {
		token := reflect.TypeOf(declared)
		if !productMatchesToken(product, token) {
			undelivered = append(undelivered, tokenDisplay(token))
		}
		for _, v := range put {
			if productMatchesToken(reflect.TypeOf(v), token) {
				leaked = append(leaked, tokenDisplay(token))
				break
			}
		}
	}
	if len(undelivered) > 0 {
		return fmt.Errorf("the construction delivered no product satisfying the declared catalog token(s) %s; a ProvidesMember declaration promises a member of that contract type, so the product must satisfy it", strings.Join(undelivered, ", "))
	}
	if len(leaked) > 0 {
		return fmt.Errorf("the construction put a value satisfying the declared catalog token(s) %s into the by-type context; a catalog member is read by name (Members, Build), never through the single-value context, so New must not put it", strings.Join(leaked, ", "))
	}
	return nil
}

// Build constructs a selected component by name: the directory-style member
// access path, both for the once-per-assembly forms (Init enumerating a
// module's members into a map) and for per-call construction (a request
// building a provider from its own credentials). override carries the
// configuration the member's New receives; nil uses the member's resolved
// configuration. Build calls New on the spot and does not put the result --
// a per-call construction produces a value for the caller, not an assembly
// product -- so it neither disturbs the by-type context nor participates in
// the rollback. The product must be assignable to T.
func Build[T any](ctx context.Context, r *ComponentRegistry, name string, override *ComponentConfig) (T, error) {
	var zero T
	p, selected := r.planned(name)
	if !selected {
		if _, exists := r.registered(name); exists {
			return zero, fmt.Errorf("%w (stage %s): component %q is not selected in this assembly; select it in the composition configuration", ErrUnknownComponent, r.currentStage(), name)
		}
		return zero, fmt.Errorf("%w (stage %s): component %q is not registered; registered components: %s", ErrUnknownComponent, r.currentStage(), name, joinNames(r.registeredOrder()))
	}

	cfg := p.cfg
	if override != nil {
		cfg = *override
	}
	instance, err := p.component.New(ctx, r, cfg)
	if err != nil {
		return zero, fmt.Errorf("pkgcore: component %q (build): %w", name, err)
	}
	if nilValue(instance) {
		return zero, fmt.Errorf("pkgcore: component %q built no product: New returned a nil or typed nil pointer value", name)
	}
	value, ok := instance.(T)
	if !ok {
		return zero, fmt.Errorf("pkgcore: component %q built a %s, which is not assignable to %s", name, typeName(reflect.TypeOf(instance)), reflect.TypeFor[T]())
	}
	return value, nil
}

// ComponentCapabilities returns the capabilities the selected component
// named name declares on its descriptor: Capabilities is the declaration
// the Prepare stage compares against the deployment mode's requirement, and
// this is its independent read, so a caller that needs the declaration
// alongside a built product reads it here instead of Build growing a second
// return value.
//
// name resolves exactly as Build's does: against the assembly plan the
// Prepare stage wrote, so the reading follows the selection the composition
// actually made, under whatever name the host selected the component by. An
// unselected name that is registered, and a name nothing registered, fail
// with ErrUnknownComponent naming the case, exactly as Build's two cases do.
// A selected component that declares no bits reads as the zero Capability,
// never an error: declaring nothing is a declaration.
func ComponentCapabilities(r *ComponentRegistry, name string) (Capability, error) {
	p, selected := r.planned(name)
	if !selected {
		if _, exists := r.registered(name); exists {
			return 0, fmt.Errorf("%w (stage %s): component %q is not selected in this assembly; select it in the composition configuration", ErrUnknownComponent, r.currentStage(), name)
		}
		return 0, fmt.Errorf("%w (stage %s): component %q is not registered; registered components: %s", ErrUnknownComponent, r.currentStage(), name, joinNames(r.registeredOrder()))
	}
	return p.component.Capabilities, nil
}

// Stage returns the stage the assembly is currently in: the stage whose
// callback is running, or the last one entered once a stage has completed.
// Before the first stage -- and after a failed Prepare, which leaves the
// registry as it was -- it reads StageIdle.
//
// This is the read-only reading of the assembly's lifecycle position, and
// it belongs to the assembly contract rather than to the registry's
// internals: a component that owns a declaration face of its own -- a
// write surface gated to one stage, the shape the registry's built-in
// declaration seats take -- enforces that gate by comparing this reading
// against the stage it admits, which is why the gate can live with the
// component that owns the face instead of only with the registry. The
// reading never fails and never blocks a stage; a caller observes the
// position, it cannot move it.
func (r *ComponentRegistry) Stage() Stage {
	return r.currentStage()
}

// currentStage reads the current stage marker for error text.
func (r *ComponentRegistry) currentStage() Stage {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cur
}

// The nine seat accessors below are the declaration face of this registry:
// each returns the seat stored in the struct's own field, so a module's
// declaration body declares into exactly these seats -- the
// write gate (writes only while the Init stage runs) inside each seat, and
// nothing else. The accessors open no path the seats themselves do not
// already answer: reads go through the same seat read methods, writes
// through the same gated write methods, with the same refusal outside Init.

// ConfigSeat returns the Config seat.
func (r *ComponentRegistry) ConfigSeat() ConfigSchemaRegistrar { return r.Config }

// FeaturesSeat returns the Features seat.
func (r *ComponentRegistry) FeaturesSeat() FeatureRegistrar { return r.Features }

// PermissionsSeat returns the Permissions seat.
func (r *ComponentRegistry) PermissionsSeat() PermissionRegistrar { return r.Permissions }

// JobsSeat returns the Jobs seat.
func (r *ComponentRegistry) JobsSeat() JobHandlerRegistrar { return r.Jobs }

// NotificationsSeat returns the Notifications seat.
func (r *ComponentRegistry) NotificationsSeat() NotificationRegistrar { return r.Notifications }

// EventsSeat returns the Events seat.
func (r *ComponentRegistry) EventsSeat() EventRegistrar { return r.Events }

// AuditActionsSeat returns the AuditActions seat.
func (r *ComponentRegistry) AuditActionsSeat() AuditActionRegistrar { return r.AuditActions }

// RetentionSeat returns the Retention seat.
func (r *ComponentRegistry) RetentionSeat() RetentionRegistrar { return r.Retention }

// SchedulesSeat returns the Schedules seat.
func (r *ComponentRegistry) SchedulesSeat() PeriodicTaskRegistrar { return r.Schedules }

// EventBus returns the assembled EventBus value from the by-type context --
// the same value the Events seat subscribes on -- or nil when the assembly
// carries none, keeping the nil-for-absent contract. It
// is a Get sugar, so it follows the bus's own resolution rule: exactly one
// put value assignable to EventBus is the answer.
func (r *ComponentRegistry) EventBus() EventBus {
	if r.Events == nil {
		return nil
	}
	return r.Events.Bus()
}

// KVStore returns the assembled KVStore value from the by-type context, or
// nil when the assembly carries none, keeping the nil-for-absent
// contract.
func (r *ComponentRegistry) KVStore() KVStore {
	kv, err := Get[KVStore](r)
	if err != nil {
		return nil
	}
	return kv
}

// Mailer returns the assembled Mailer value from the by-type context, or nil
// when the assembly carries none, keeping the nil-for-absent contract.
func (r *ComponentRegistry) Mailer() Mailer {
	mailer, err := Get[Mailer](r)
	if err != nil {
		return nil
	}
	return mailer
}

// ObjectStore returns the assembled ObjectStore value from the by-type
// context, or nil when the assembly carries none, keeping the
// nil-for-absent contract.
func (r *ComponentRegistry) ObjectStore() ObjectStore {
	store, err := Get[ObjectStore](r)
	if err != nil {
		return nil
	}
	return store
}

// Locales returns the merged message catalog from the by-type context, or
// nil when the assembly carries none, keeping the nil-for-absent
// contract.
func (r *ComponentRegistry) Locales() *i18n.Catalog {
	catalog, err := Get[*i18n.Catalog](r)
	if err != nil {
		return nil
	}
	return catalog
}

// RegisteredComponents returns every component registered on this instance,
// in registration order: the global snapshot the instance was seeded from
// first, then the descriptors Register added. It is the reading the engine's
// loader resolves every registered component's BootstrapKeys declaration
// over, before the assembly plan exists.
func RegisteredComponents(r *ComponentRegistry) []Component {
	r.mu.RLock()
	defer r.mu.RUnlock()
	components := make([]Component, 0, len(r.order))
	for _, name := range r.order {
		components = append(components, r.byName[name])
	}
	return components
}

// MemberNames returns the names of the selected components implementing
// module, in dependency order -- the stable diagnostic reading of "which
// selected components implement module X", grouped by Component.Module and
// carrying no products. It returns an empty slice when no selected
// component implements the module.
func MemberNames(r *ComponentRegistry, module string) []string {
	var names []string
	for _, p := range r.plannedComponents() {
		if p.component.Module == module {
			names = append(names, p.component.Name)
		}
	}
	return names
}

// Member is one catalog member: the component name it was delivered under,
// and its product.
type Member[T any] struct {
	// Name is the selected component's name -- the key this member answers
	// under and the name Build constructs the same component by.
	Name string
	// Value is the product the member's construction delivered, of the
	// contract type T addresses.
	Value T
}

// Members returns every selected catalog member delivering T, in
// dependency order: the Construct-stage products a consumer enumerates
// once per assembly (a provider map keyed by name, a directory listing).
// A member is a selected component whose ProvidesMember declarations
// address T; the value is the product its construction recorded, so a call
// before the Construct stage finds nothing. Get and GetOptional never
// answer with a member -- its product is not put into the by-type context
// -- and Members never answers with a bound provider's value: the two
// delivery kinds stay apart on the reading side too. Build remains the
// per-call construction of one named member.
func Members[T any](r *ComponentRegistry) []Member[T] {
	target := reflect.TypeFor[T]()
	var members []Member[T]
	for _, p := range r.plannedComponents() {
		if !memberAddressesType(p.component, target) {
			continue
		}
		instance, constructed := r.constructedInstance(p.component.Name)
		if !constructed {
			continue
		}
		t := reflect.TypeOf(instance)
		if t == nil || !t.AssignableTo(target) {
			continue
		}
		value, ok := reflect.ValueOf(instance).Convert(target).Interface().(T)
		if !ok {
			continue
		}
		members = append(members, Member[T]{Name: p.component.Name, Value: value})
	}
	return members
}

// Asset is one selected component's embedded assets: the material a host or
// a database component consumes, gathered under the component's name.
type Asset struct {
	// Name is the owning component's name.
	Name string
	// Module is the owning component's module name (Component.Module): the
	// identity its assets merge under -- a migration ledger records the
	// component's Migrations set under it, and the component's locale
	// resources take it as their id prefix -- so both stay stable when a
	// host renames or overrides the component. Empty for a component
	// implementing no module.
	Module string
	// Migrations is the component's versioned SQL migration set, one
	// subdirectory per dialect; the zero value means the component carries
	// none.
	Migrations embed.FS
	// Locales is the component's translation resources; the zero value
	// means the component renders no message.
	Locales embed.FS
	// OpenAPISpec is the component's OpenAPI document fragment; empty means
	// the component serves no HTTP surface.
	OpenAPISpec []byte
}

// Assets returns the embedded assets of every selected component that
// carries at least one, in dependency order. A database component applies
// the Migrations sets during the Verify stage, each keyed by its owning
// component's Module; Locales merge into the message catalog and
// OpenAPISpec fragments merge into the application specification through
// the host's own steps.
func Assets(r *ComponentRegistry) []Asset {
	var zeroFS embed.FS
	var assets []Asset
	for _, p := range r.plannedComponents() {
		c := p.component
		if c.Migrations == zeroFS && c.Locales == zeroFS && len(c.OpenAPISpec) == 0 {
			continue
		}
		assets = append(assets, Asset{
			Name:        c.Name,
			Module:      c.Module,
			Migrations:  c.Migrations,
			Locales:     c.Locales,
			OpenAPISpec: c.OpenAPISpec,
		})
	}
	return assets
}

// The nine seat implementations below wrap the in-memory registrars with
// the stage gate. The gate itself is implemented once, by gatedSeat, and each
// wrapper instantiates it for the registrar interface it fronts: a write
// asks the registry for the opened registrar behind the seat and is
// refused, naming the seat and the current stage, outside the Init stage;
// a read answers from the opened registrar or, before Init, reports nothing
// declared. Where the registrar interface cannot report an error
// (EventRegistrar.Subscribe, void), the refusal is a panic instead --
// silently dropping a declaration is the one outcome worse than a loud
// failure at startup, and a write outside Init is a wiring error, not a
// runtime condition.

// gatedSeat is one seat's view over the registry: the seat's name for the
// refusal, and the address of the seat's registrar within the registry's
// seat set. It carries the whole gate.
type gatedSeat[S any] struct {
	reg  *ComponentRegistry
	name string
	seat func(seatSet) S
}

// gatedSeatFor returns the gate a seat wrapper embeds.
func gatedSeatFor[S any](r *ComponentRegistry, name string, seat func(seatSet) S) gatedSeat[S] {
	return gatedSeat[S]{reg: r, name: name, seat: seat}
}

// write returns the opened registrar behind the seat for a declaration, or
// the gate's refusal when the seats are closed -- which is every moment
// outside the Init stage.
func (g gatedSeat[S]) write() (S, error) {
	var zero S
	seats, err := g.reg.seatFor(g.name)
	if err != nil {
		return zero, err
	}
	return g.seat(seats), nil
}

// read returns the opened registrar behind the seat for a read: ok is false
// before the seats open, when nothing has been declared (reads are legal at
// any point, writes are not).
func (g gatedSeat[S]) read() (S, bool) {
	var zero S
	registrar := g.seat(g.reg.seatRead())
	if any(registrar) == nil {
		return zero, false
	}
	return registrar, true
}

// configSeat is the Config seat.
type configSeat struct {
	gatedSeat[ConfigSchemaRegistrar]
}

func (s *configSeat) Add(items ...ConfigItem) error {
	registrar, err := s.write()
	if err != nil {
		return err
	}
	return registrar.Add(items...)
}

func (s *configSeat) Items() []ConfigItem {
	registrar, ok := s.read()
	if !ok {
		return nil
	}
	return registrar.Items()
}

// featuresSeat is the Features seat.
type featuresSeat struct{ gatedSeat[FeatureRegistrar] }

func (s *featuresSeat) Add(flags ...FeatureFlag) error {
	registrar, err := s.write()
	if err != nil {
		return err
	}
	return registrar.Add(flags...)
}

func (s *featuresSeat) Flags() []FeatureFlag {
	registrar, ok := s.read()
	if !ok {
		return nil
	}
	return registrar.Flags()
}

// permissionsSeat is the Permissions seat.
type permissionsSeat struct{ gatedSeat[PermissionRegistrar] }

func (s *permissionsSeat) Add(perms ...string) error {
	registrar, err := s.write()
	if err != nil {
		return err
	}
	return registrar.Add(perms...)
}

func (s *permissionsSeat) Permissions() []string {
	registrar, ok := s.read()
	if !ok {
		return nil
	}
	return registrar.Permissions()
}

// jobsSeat is the Jobs seat.
type jobsSeat struct{ gatedSeat[JobHandlerRegistrar] }

func (s *jobsSeat) Handle(jobType string, handler any) error {
	registrar, err := s.write()
	if err != nil {
		return err
	}
	return registrar.Handle(jobType, handler)
}

func (s *jobsSeat) Handlers() map[string]any {
	registrar, ok := s.read()
	if !ok {
		return nil
	}
	return registrar.Handlers()
}

// notificationsSeat is the Notifications seat.
type notificationsSeat struct {
	gatedSeat[NotificationRegistrar]
}

func (s *notificationsSeat) Add(types ...NotificationType) error {
	registrar, err := s.write()
	if err != nil {
		return err
	}
	return registrar.Add(types...)
}

func (s *notificationsSeat) Types() []NotificationType {
	registrar, ok := s.read()
	if !ok {
		return nil
	}
	return registrar.Types()
}

// eventsSeat is the Events seat. Its declarations live in an in-memory
// registrar, while Subscribe and Bus resolve the assembled EventBus value
// from the by-type context at the moment they are called: the bus is a
// component product, constructed before Init runs, so a subscription
// installed during Init lands on the same bus a publisher reaches later.
// Subscribe on an assembly with no EventBus value panics, for the same
// void-method reason the seat gate panics.
type eventsSeat struct{ gatedSeat[EventRegistrar] }

func (s *eventsSeat) Publishes(events ...EventDecl) error {
	registrar, err := s.write()
	if err != nil {
		return err
	}
	return registrar.Publishes(events...)
}

func (s *eventsSeat) Published() []EventDecl {
	registrar, ok := s.read()
	if !ok {
		return nil
	}
	return registrar.Published()
}

func (s *eventsSeat) Subscribe(eventType string, h EventHandler) {
	if _, err := s.write(); err != nil {
		panic(err)
	}
	bus, err := Get[EventBus](s.reg)
	if err != nil {
		panic(fmt.Errorf("pkgcore: the Events seat needs an assembled pkgcore.EventBus value to subscribe on: %w", err))
	}
	bus.Subscribe(eventType, h)
}

func (s *eventsSeat) Bus() EventBus {
	bus, err := Get[EventBus](s.reg)
	if err != nil {
		return nil
	}
	return bus
}

// auditActionsSeat is the AuditActions seat.
type auditActionsSeat struct {
	gatedSeat[AuditActionRegistrar]
}

func (s *auditActionsSeat) Add(actions ...string) error {
	registrar, err := s.write()
	if err != nil {
		return err
	}
	return registrar.Add(actions...)
}

func (s *auditActionsSeat) Actions() []string {
	registrar, ok := s.read()
	if !ok {
		return nil
	}
	return registrar.Actions()
}

// retentionSeat is the Retention seat.
type retentionSeat struct{ gatedSeat[RetentionRegistrar] }

func (s *retentionSeat) Add(participants ...RetentionParticipant) error {
	registrar, err := s.write()
	if err != nil {
		return err
	}
	return registrar.Add(participants...)
}

func (s *retentionSeat) Participants() []RetentionParticipant {
	registrar, ok := s.read()
	if !ok {
		return nil
	}
	return registrar.Participants()
}

// schedulesSeat is the Schedules seat.
type schedulesSeat struct {
	gatedSeat[PeriodicTaskRegistrar]
}

func (s *schedulesSeat) Add(decls ...PeriodicTask) error {
	registrar, err := s.write()
	if err != nil {
		return err
	}
	return registrar.Add(decls...)
}

func (s *schedulesSeat) Declarations() []PeriodicTask {
	registrar, ok := s.read()
	if !ok {
		return nil
	}
	return registrar.Declarations()
}
