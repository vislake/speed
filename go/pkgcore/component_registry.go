package pkgcore

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"

	"github.com/vislake/speed/go/pkgcore/i18n"
)

// component_registry.go carries the assembly half of the config-driven
// component assembly: the ComponentRegistry, its by-type value store (Put
// and Get), its eleven declaration seats, and the seven-stage lifecycle the
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

// stage names one of the seven lifecycle stages the assembly walks through.
type stage int

const (
	stageIdle stage = iota
	stagePrepare
	stageConstruct
	stageVerify
	stageInit
	stageStart
	stageStop
	stageClose
)

// String renders the stage name used in error text.
func (s stage) String() string {
	switch s {
	case stagePrepare:
		return "prepare"
	case stageConstruct:
		return "construct"
	case stageVerify:
		return "verify"
	case stageInit:
		return "init"
	case stageStart:
		return "start"
	case stageStop:
		return "stop"
	case stageClose:
		return "close"
	default:
		return "not started"
	}
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
	routes        RouteRegistrar
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
// The ten declaration seats are the fields below; they accept writes only
// while the Init stage runs, and a write outside it is refused naming the
// stage. The by-type value context is Put and Get; the assembly puts every
// component's product there as it constructs it.
type ComponentRegistry struct {
	// Routes receives the HTTP handlers components mount.
	Routes RouteRegistrar
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
	// MemberNames and Assets are its public readers.
	plan       []plannedComponent
	planByName map[string]int

	// cur is the stage currently running (or the last one entered);
	// prepared through started record stage completion, and closed records
	// the terminal state Close establishes.
	cur         stage
	prepared    bool
	constructed bool
	verified    bool
	inited      bool
	started     bool
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

	r.Routes = &routeSeat{reg: r}
	r.Config = &configSeat{reg: r}
	r.Features = &featuresSeat{reg: r}
	r.Permissions = &permissionsSeat{reg: r}
	r.Jobs = &jobsSeat{reg: r}
	r.Notifications = &notificationsSeat{reg: r}
	r.Events = &eventsSeat{reg: r}
	r.AuditActions = &auditActionsSeat{reg: r}
	r.Retention = &retentionSeat{reg: r}
	r.Schedules = &schedulesSeat{reg: r}
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

// Put adds v to the by-type value context: a component's product, a runtime
// service published during Init, or a value the host injects before Prepare
// (its own data, or the code-override layer of the configuration chain). All
// consumers read one shared context, addressed by type. Put is append-only,
// so no value is ever displaced by a later one.
//
// A nil v panics: nil matches no type, so it could only ever produce a Get
// that reports a type missing while a put was made -- the silent absence the
// by-type context exists to prevent.
func (r *ComponentRegistry) Put(v any) {
	if v == nil {
		panic("pkgcore: Put requires a non-nil value: nil matches no type and would leave a later Get reporting a silent absence")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values = append(r.values, v)
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
	if err := r.beginStage(stagePrepare); err != nil {
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
		if err := p.component.Prepare(ctx, r); err != nil {
			r.abandonStage()
			return fmt.Errorf("pkgcore: component %q (stage prepare): %w", name, err)
		}
	}

	r.markStageDone(stagePrepare)
	return nil
}

// Construct runs the second stage: every planned component's New callback,
// in dependency order, each product put into the by-type context so the
// components constructed after it can reach it. A New that returns no
// product fails the stage.
//
// A failure -- here or in any later stage -- closes every constructed
// component in reverse order, exactly once, and returns ErrComponentFailed
// naming the stage, the component, the cause and the rolled-back set.
func (r *ComponentRegistry) Construct(ctx context.Context) error {
	if err := r.beginStage(stageConstruct); err != nil {
		return err
	}

	for _, p := range r.plannedComponents() {
		name := p.component.Name
		if err := ctx.Err(); err != nil {
			return r.failStage(ctx, stageConstruct, name, err)
		}
		instance, err := p.component.New(ctx, r, p.cfg)
		if err != nil {
			return r.failStage(ctx, stageConstruct, name, err)
		}
		if instance == nil {
			return r.failStage(ctx, stageConstruct, name, errors.New("the New callback returned no product"))
		}
		if err := productSatisfiesDeclarations(p.component, instance); err != nil {
			return r.failStage(ctx, stageConstruct, name, err)
		}
		r.recordConstructed(p.component, instance)
		r.Put(instance)
	}

	r.markStageDone(stageConstruct)
	return nil
}

// Verify runs the third stage: every constructed component's Verify callback
// in dependency order. It runs after every product exists, so a database
// component applies the assembled migration set here, before any other
// Verify callback runs.
func (r *ComponentRegistry) Verify(ctx context.Context) error {
	if err := r.beginStage(stageVerify); err != nil {
		return err
	}
	for _, e := range r.constructedList() {
		if err := ctx.Err(); err != nil {
			return r.failStage(ctx, stageVerify, e.name, err)
		}
		if e.component.Verify == nil {
			continue
		}
		if err := e.component.Verify(ctx, r, e.instance); err != nil {
			return r.failStage(ctx, stageVerify, e.name, err)
		}
	}
	r.markStageDone(stageVerify)
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
// selected components' BootstrapKeys and the runtime configuration seat,
// and the feature-graph check (ValidateFeatureGraph over the Features
// seat).
func (r *ComponentRegistry) Init(ctx context.Context) error {
	if err := r.beginStage(stageInit); err != nil {
		return err
	}

	if err := r.registerSystemPurposes(); err != nil {
		return r.failStage(ctx, stageInit, "", err)
	}

	r.openSeats()
	for _, e := range r.constructedList() {
		if err := ctx.Err(); err != nil {
			return r.failStage(ctx, stageInit, e.name, err)
		}
		if e.component.Init == nil {
			continue
		}
		if err := e.component.Init(ctx, r, e.instance); err != nil {
			return r.failStage(ctx, stageInit, e.name, err)
		}
	}
	r.closeSeats()

	if err := r.validateOneLayerPerKey(); err != nil {
		return r.failStage(ctx, stageInit, "", err)
	}
	if err := ValidateFeatureGraph(r.Features); err != nil {
		return r.failStage(ctx, stageInit, "", err)
	}

	r.markStageDone(stageInit)
	return nil
}

// Start runs the fifth stage: every constructed component's Start callback
// in dependency order, after the closing validation of Init -- listeners,
// workers, schedulers. A Start failure rolls the assembly back exactly like
// a Construct, Verify or Init failure.
func (r *ComponentRegistry) Start(ctx context.Context) error {
	if err := r.beginStage(stageStart); err != nil {
		return err
	}
	for _, e := range r.constructedList() {
		if err := ctx.Err(); err != nil {
			return r.failStage(ctx, stageStart, e.name, err)
		}
		if e.component.Start == nil {
			continue
		}
		if err := e.component.Start(ctx, r, e.instance); err != nil {
			return r.failStage(ctx, stageStart, e.name, err)
		}
	}
	r.markStageDone(stageStart)
	return nil
}

// Stop runs the sixth stage: the non-blocking stop notification to every
// constructed component, in reverse dependency order. Failures are ignored,
// because a notification that cannot be delivered must not keep the Close
// that waits for the drain from running; Stop releases nothing itself.
func (r *ComponentRegistry) Stop(ctx context.Context) error {
	if err := r.checkNotClosed(stageStop); err != nil {
		return err
	}
	r.setStage(stageStop)

	entries := r.constructedList()
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.component.Stop == nil {
			continue
		}
		_ = e.component.Stop(ctx, r, e.instance)
	}
	return nil
}

// Close runs the seventh stage: every constructed component's Close callback
// in reverse dependency order, waiting out the drain Stop announced. It runs
// exactly once -- a second call reports the first call's result -- and
// aggregates the failures of every Close callback rather than stopping at
// the first. The registry also calls Close itself when any stage from
// Construct on fails, so a failed assembly is always fully torn down before
// its error reaches the host; a host that joins reg.Close(ctx) into the
// failure it is handling observes the same, already computed result.
func (r *ComponentRegistry) Close(ctx context.Context) error {
	r.mu.Lock()
	if r.closed {
		err := r.closeErr
		r.mu.Unlock()
		return err
	}
	r.closed = true
	r.seatsOpen = false
	r.cur = stageClose
	entries := append([]constructedEntry(nil), r.constructedEntries...)
	r.mu.Unlock()

	rolled, err := r.closeEntries(ctx, entries)

	r.mu.Lock()
	r.closeRolled = rolled
	r.closeErr = err
	r.mu.Unlock()
	return err
}

// closeEntries closes entries in reverse construction order, aggregating
// failures, and returns the names whose Close callback ran.
func (r *ComponentRegistry) closeEntries(ctx context.Context, entries []constructedEntry) ([]string, error) {
	var rolled []string
	var errs []error
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.component.Close == nil {
			continue
		}
		rolled = append(rolled, e.name)
		if err := e.component.Close(ctx, r, e.instance); err != nil {
			errs = append(errs, fmt.Errorf("pkgcore: component %q (stage close): %w", e.name, err))
		}
	}
	return rolled, errors.Join(errs...)
}

// failStage rolls the assembly back after a post-Construct failure and
// returns the four-element error: stage, component (or "the assembly" for a
// validation with no single owner), cause, and the rolled-back set. The
// Close errors of the rollback join the result, so nothing a Close callback
// reports is lost on the failure path.
func (r *ComponentRegistry) failStage(ctx context.Context, s stage, name string, cause error) error {
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

// beginStage validates that s may run now and records it as the current
// stage.
func (r *ComponentRegistry) beginStage(s stage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkStageOrder(s); err != nil {
		return err
	}
	r.cur = s
	return nil
}

// checkNotClosed refuses a stage once the registry is closed.
func (r *ComponentRegistry) checkNotClosed(s stage) error {
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
func (r *ComponentRegistry) checkStageOrder(s stage) error {
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
	case stagePrepare:
		if r.prepared {
			return fmt.Errorf("%w: the Prepare stage already ran", ErrStageViolation)
		}
	case stageConstruct:
		if err := need(r.prepared, "a completed Prepare stage"); err != nil {
			return err
		}
		if r.constructed {
			return fmt.Errorf("%w: the Construct stage already ran", ErrStageViolation)
		}
	case stageVerify:
		if err := need(r.constructed, "a completed Construct stage"); err != nil {
			return err
		}
		if r.verified {
			return fmt.Errorf("%w: the Verify stage already ran", ErrStageViolation)
		}
	case stageInit:
		if err := need(r.verified, "a completed Verify stage"); err != nil {
			return err
		}
		if r.inited {
			return fmt.Errorf("%w: the Init stage already ran", ErrStageViolation)
		}
	case stageStart:
		if err := need(r.inited, "a completed Init stage"); err != nil {
			return err
		}
		if r.started {
			return fmt.Errorf("%w: the Start stage already ran", ErrStageViolation)
		}
	}
	return nil
}

// markStageDone records a completed stage and advances the current stage
// marker.
func (r *ComponentRegistry) markStageDone(s stage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch s {
	case stagePrepare:
		r.prepared = true
	case stageConstruct:
		r.constructed = true
	case stageVerify:
		r.verified = true
	case stageInit:
		r.inited = true
	case stageStart:
		r.started = true
	}
	r.cur = s
}

// abandonStage rolls the stage marker back so a failed Prepare leaves the
// registry as it was, ready for a retry with a corrected composition.
func (r *ComponentRegistry) abandonStage() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cur = stageIdle
}

// setStage records the stage currently running without completion
// bookkeeping (Stop is a terminal notification, not a gated stage).
func (r *ComponentRegistry) setStage(s stage) {
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
		routes:        &memoryRouteRegistrar{},
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

// validateOneLayerPerKey refuses a key declared on two layers: a selected
// component's BootstrapKeys declaration (the process-start layer, resolved
// by the loader before construction) and an item declared on the runtime
// Config seat. One dotted key carries one meaning, one default and one edit
// surface, so a key on both layers fails the Init stage's closing validation
// rather than leaving an operator unable to tell which layer a change hits.
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
	}
	if len(conflicts) == 0 {
		return nil
	}
	return fmt.Errorf("pkgcore: key declared on two layers: %w", errors.Join(conflicts...))
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

// productSatisfiesDeclarations reports whether instance -- the product a
// component's New just returned -- matches at least one of the component's
// Provides declarations when it declares any. A product matching none of
// them means the descriptor declared products the component does not
// produce: every requirement resolution resting on that declaration would
// fail only later, at Get time, so the construction fails here instead,
// naming the cause while the component is still on the stack.
func productSatisfiesDeclarations(c Component, instance any) error {
	if len(c.Provides) == 0 {
		return nil
	}
	product := reflect.TypeOf(instance)
	for _, declared := range c.Provides {
		if productMatchesToken(product, reflect.TypeOf(declared)) {
			return nil
		}
	}
	types := make([]string, 0, len(c.Provides))
	for _, declared := range c.Provides {
		types = append(types, tokenDisplay(reflect.TypeOf(declared)))
	}
	return fmt.Errorf("the product %s matches none of the component's Provides declarations (%s)", product, strings.Join(types, ", "))
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
	value, ok := instance.(T)
	if !ok {
		return zero, fmt.Errorf("pkgcore: component %q built a %s, which is not assignable to %s", name, typeName(reflect.TypeOf(instance)), reflect.TypeFor[T]())
	}
	return value, nil
}

// currentStage reads the current stage marker for error text.
func (r *ComponentRegistry) currentStage() stage {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cur
}

// MountedRoutes returns the routes declared on the Routes seat, the same
// reading Registry.Routes.Routes() answers on the module Registry. It is the
// route source a middleware chain derives its partition from, so a consumer
// that takes either registry shape (a *Registry or a *ComponentRegistry) can
// read the mounted routes uniformly. Before the Init stage nothing has been
// declared, so it returns nil.
func (r *ComponentRegistry) MountedRoutes() []MountedRoute {
	if r.Routes == nil {
		return nil
	}
	return r.Routes.Routes()
}

// The ten seat accessors below answer the Registrar view over this registry:
// each returns the seat stored in the struct's own field, so a declaration
// body written against Registrar declares into exactly these seats -- the
// write gate (writes only while the Init stage runs) inside each seat, and
// nothing else. The accessors open no path the seats themselves do not
// already answer: reads go through the same seat read methods, writes
// through the same gated write methods, with the same refusal outside Init.

// RoutesSeat returns the Routes seat.
func (r *ComponentRegistry) RoutesSeat() RouteRegistrar { return r.Routes }

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
// carries none, mirroring the module Registry's nil-for-absent contract. It
// is a Get sugar, so it follows the bus's own resolution rule: exactly one
// put value assignable to EventBus is the answer.
func (r *ComponentRegistry) EventBus() EventBus {
	if r.Events == nil {
		return nil
	}
	return r.Events.Bus()
}

// KVStore returns the assembled KVStore value from the by-type context, or
// nil when the assembly carries none, mirroring the module Registry's
// nil-for-absent contract.
func (r *ComponentRegistry) KVStore() KVStore {
	kv, err := Get[KVStore](r)
	if err != nil {
		return nil
	}
	return kv
}

// Mailer returns the assembled Mailer value from the by-type context, or nil
// when the assembly carries none, mirroring the module Registry's
// nil-for-absent contract.
func (r *ComponentRegistry) Mailer() Mailer {
	mailer, err := Get[Mailer](r)
	if err != nil {
		return nil
	}
	return mailer
}

// ObjectStore returns the assembled ObjectStore value from the by-type
// context, or nil when the assembly carries none, mirroring the module
// Registry's nil-for-absent contract.
func (r *ComponentRegistry) ObjectStore() ObjectStore {
	store, err := Get[ObjectStore](r)
	if err != nil {
		return nil
	}
	return store
}

// Locales returns the merged message catalog from the by-type context, or
// nil when the assembly carries none, mirroring the module Registry's
// nil-for-absent contract.
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
// module, in dependency order -- the stable reading a directory-style
// module's consumer enumerates its members with. It returns an empty slice
// when no selected component implements the module.
func MemberNames(r *ComponentRegistry, module string) []string {
	var names []string
	for _, p := range r.plannedComponents() {
		if p.component.Module == module {
			names = append(names, p.component.Name)
		}
	}
	return names
}

// Asset is one selected component's embedded assets: the material a host or
// a database component consumes, gathered under the component's name.
type Asset struct {
	// Name is the owning component's name.
	Name string
	// Module is the owning component's module name (Component.Module): the
	// key a migration ledger records the component's Migrations set under,
	// so the log stays stable when a host renames or overrides the
	// component. Empty for a component implementing no module.
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

// The ten seat implementations below wrap the in-memory registrars with the
// stage gate. Each write method asks seatFor first and refuses outside the
// Init stage; where the registrar interface cannot report an error
// (RouteRegistrar.Mount, EventRegistrar.Subscribe, both void), the refusal
// is a panic instead -- silently dropping a declaration is the one outcome
// worse than a loud failure at startup, and a write outside Init is a wiring
// error, not a runtime condition. Read methods answer from the opened seats
// or, before Init, report nothing declared.

// routeSeat is the Routes seat.
type routeSeat struct{ reg *ComponentRegistry }

func (s *routeSeat) Mount(path string, handler http.Handler) {
	seats, err := s.reg.seatFor("Routes")
	if err != nil {
		panic(err)
	}
	seats.routes.Mount(path, handler)
}

func (s *routeSeat) Routes() []MountedRoute {
	seats := s.reg.seatRead()
	if seats.routes == nil {
		return nil
	}
	return seats.routes.Routes()
}

// configSeat is the Config seat.
type configSeat struct{ reg *ComponentRegistry }

func (s *configSeat) Add(items ...ConfigItem) error {
	seats, err := s.reg.seatFor("Config")
	if err != nil {
		return err
	}
	return seats.config.Add(items...)
}

func (s *configSeat) Items() []ConfigItem {
	seats := s.reg.seatRead()
	if seats.config == nil {
		return nil
	}
	return seats.config.Items()
}

// featuresSeat is the Features seat.
type featuresSeat struct{ reg *ComponentRegistry }

func (s *featuresSeat) Add(flags ...FeatureFlag) error {
	seats, err := s.reg.seatFor("Features")
	if err != nil {
		return err
	}
	return seats.features.Add(flags...)
}

func (s *featuresSeat) Flags() []FeatureFlag {
	seats := s.reg.seatRead()
	if seats.features == nil {
		return nil
	}
	return seats.features.Flags()
}

// permissionsSeat is the Permissions seat.
type permissionsSeat struct{ reg *ComponentRegistry }

func (s *permissionsSeat) Add(perms ...string) error {
	seats, err := s.reg.seatFor("Permissions")
	if err != nil {
		return err
	}
	return seats.permissions.Add(perms...)
}

func (s *permissionsSeat) Permissions() []string {
	seats := s.reg.seatRead()
	if seats.permissions == nil {
		return nil
	}
	return seats.permissions.Permissions()
}

// jobsSeat is the Jobs seat.
type jobsSeat struct{ reg *ComponentRegistry }

func (s *jobsSeat) Handle(jobType string, handler any) error {
	seats, err := s.reg.seatFor("Jobs")
	if err != nil {
		return err
	}
	return seats.jobs.Handle(jobType, handler)
}

func (s *jobsSeat) Handlers() map[string]any {
	seats := s.reg.seatRead()
	if seats.jobs == nil {
		return nil
	}
	return seats.jobs.Handlers()
}

// notificationsSeat is the Notifications seat.
type notificationsSeat struct{ reg *ComponentRegistry }

func (s *notificationsSeat) Add(types ...NotificationType) error {
	seats, err := s.reg.seatFor("Notifications")
	if err != nil {
		return err
	}
	return seats.notifications.Add(types...)
}

func (s *notificationsSeat) Types() []NotificationType {
	seats := s.reg.seatRead()
	if seats.notifications == nil {
		return nil
	}
	return seats.notifications.Types()
}

// eventsSeat is the Events seat. Its declarations live in an in-memory
// registrar, while Subscribe and Bus resolve the assembled EventBus value
// from the by-type context at the moment they are called: the bus is a
// component product, constructed before Init runs, so a subscription
// installed during Init lands on the same bus a publisher reaches later.
// Subscribe on an assembly with no EventBus value panics, for the same
// void-method reason the seat gate panics.
type eventsSeat struct{ reg *ComponentRegistry }

func (s *eventsSeat) Publishes(events ...EventDecl) error {
	seats, err := s.reg.seatFor("Events")
	if err != nil {
		return err
	}
	return seats.events.Publishes(events...)
}

func (s *eventsSeat) Published() []EventDecl {
	seats := s.reg.seatRead()
	if seats.events == nil {
		return nil
	}
	return seats.events.Published()
}

func (s *eventsSeat) Subscribe(eventType string, h EventHandler) {
	if _, err := s.reg.seatFor("Events"); err != nil {
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
type auditActionsSeat struct{ reg *ComponentRegistry }

func (s *auditActionsSeat) Add(actions ...string) error {
	seats, err := s.reg.seatFor("AuditActions")
	if err != nil {
		return err
	}
	return seats.auditActions.Add(actions...)
}

func (s *auditActionsSeat) Actions() []string {
	seats := s.reg.seatRead()
	if seats.auditActions == nil {
		return nil
	}
	return seats.auditActions.Actions()
}

// retentionSeat is the Retention seat.
type retentionSeat struct{ reg *ComponentRegistry }

func (s *retentionSeat) Add(participants ...RetentionParticipant) error {
	seats, err := s.reg.seatFor("Retention")
	if err != nil {
		return err
	}
	return seats.retention.Add(participants...)
}

func (s *retentionSeat) Participants() []RetentionParticipant {
	seats := s.reg.seatRead()
	if seats.retention == nil {
		return nil
	}
	return seats.retention.Participants()
}

// schedulesSeat is the Schedules seat.
type schedulesSeat struct{ reg *ComponentRegistry }

func (s *schedulesSeat) Add(decls ...PeriodicTask) error {
	seats, err := s.reg.seatFor("Schedules")
	if err != nil {
		return err
	}
	return seats.schedules.Add(decls...)
}

func (s *schedulesSeat) Declarations() []PeriodicTask {
	seats := s.reg.seatRead()
	if seats.schedules == nil {
		return nil
	}
	return seats.schedules.Declarations()
}
