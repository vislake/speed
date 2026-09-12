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
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore/internal/componentfixtures/locales"
	"github.com/vislake/speed/go/pkgcore/internal/componentfixtures/migrations"
)

// stageLog records fixture callback invocations and close counts.
type stageLog struct {
	mu     sync.Mutex
	events []string
}

// record appends one event.
func (l *stageLog) record(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

// all returns the recorded events in order.
func (l *stageLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// count returns how many recorded events equal event.
func (l *stageLog) count(event string) int {
	n := 0
	for _, e := range l.all() {
		if e == event {
			n++
		}
	}
	return n
}

// newTestRegistry returns a registry with the given components registered.
func newTestRegistry(t *testing.T, components ...Component) *ComponentRegistry {
	t.Helper()
	reg := NewComponentRegistry()
	for _, c := range components {
		if err := reg.Register(c); err != nil {
			t.Fatalf("Register(%q) = %v, want nil", c.Name, err)
		}
	}
	return reg
}

// testComposition builds a composition configuration selecting the given
// (name, value) pairs in order.
func testComposition(entries ...configEntry) ComponentConfig {
	block := ComponentConfig{}
	for _, e := range entries {
		block = block.With(e.key, e.value)
	}
	return NewComponentConfig(nil).With("components", block)
}

// assertPanicContains runs fn and fails t when it does not panic with a
// value whose rendering contains want.
func assertPanicContains(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatalf("call did not panic, want a panic containing %q", want)
		}
		if got := fmt.Sprint(recovered); !strings.Contains(got, want) {
			t.Errorf("panic %q does not contain %q", got, want)
		}
	}()
	fn()
}

// recordingComponent returns a component that records every callback in log
// and produces product from New.
func recordingComponent(log *stageLog, name, module string, product any, hooks func(*Component)) Component {
	c := Component{
		Name:   name,
		Module: module,
		Prepare: func(context.Context, *ComponentRegistry) error {
			log.record(name + ".prepare")
			return nil
		},
		New: func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) {
			log.record(name + ".new")
			return product, nil
		},
		Verify: func(context.Context, *ComponentRegistry, any) error {
			log.record(name + ".verify")
			return nil
		},
		Init: func(context.Context, *ComponentRegistry, any) error {
			log.record(name + ".init")
			return nil
		},
		Start: func(context.Context, *ComponentRegistry, any) error {
			log.record(name + ".start")
			return nil
		},
		Stop: func(context.Context, *ComponentRegistry, any) error {
			log.record(name + ".stop")
			return nil
		},
		Close: func(context.Context, *ComponentRegistry, any) error {
			log.record(name + ".close")
			return nil
		},
	}
	if hooks != nil {
		hooks(&c)
	}
	return c
}

func TestPutGetStructuralMatch(t *testing.T) {
	reg := NewComponentRegistry()

	// One match: a concrete pointer.
	reg.Put(&compTokenA{})
	if v, err := Get[*compTokenA](reg); err != nil || v == nil {
		t.Fatalf("Get[*compTokenA] = (%v, %v), want the put value", v, err)
	}

	// Interface target: a put value implementing the interface matches.
	reg.Put(compSpreadImpl{})
	if v, err := Get[compSpreader](reg); err != nil || v.spread() != "spread" {
		t.Fatalf("Get[compSpreader] = (%v, %v), want the implementing value", v, err)
	}

	// Zero matches: named-missing.
	_, err := Get[*compTokenB](reg)
	if !errors.Is(err, ErrMissingRequirement) {
		t.Fatalf("Get[*compTokenB] = %v, want ErrMissingRequirement", err)
	}
	if !strings.Contains(err.Error(), "pkgcore.compTokenB") {
		t.Errorf("missing error %q does not name the type", err)
	}

	// Multiple matches: ambiguous, listing the candidates.
	reg.Put(&compTokenB{})
	reg.Put(&compTokenB{})
	_, err = Get[*compTokenB](reg)
	if !errors.Is(err, ErrAmbiguousProvider) {
		t.Fatalf("Get for two values = %v, want ErrAmbiguousProvider", err)
	}
	if !strings.Contains(err.Error(), "2 put values") || !strings.Contains(err.Error(), "pkgcore.compTokenB") {
		t.Errorf("ambiguity error %q does not list the values", err)
	}

	// Put(nil) panics.
	assertPanicContains(t, "Put requires a non-nil value", func() { reg.Put(nil) })
}

// typedNilProduct is the New callback of a component whose construction
// "succeeds" with a typed nil pointer -- the shape an interface-typed return
// turns silently non-nil, so every nil check that compares the interface
// against nil lets it through.
func typedNilProduct() (any, error) {
	var product *compTokenA
	return product, nil
}

// TestTypedNilPointersAreAbsent pins the nil rule at the three boundaries a
// product crosses: Put, the Construct stage's product delivery and Build. A
// typed nil pointer is the absence it is, never a present value -- so a
// consumer can never read a present-but-nil value out of the by-type context
// and panic at first use far from the wiring error.
func TestTypedNilPointersAreAbsent(t *testing.T) {
	ctx := context.Background()

	t.Run("Put refuses a typed nil", func(t *testing.T) {
		reg := NewComponentRegistry()
		assertPanicContains(t, "Put requires a non-nil value", func() { reg.Put((*compTokenA)(nil)) })
		if _, err := Get[*compTokenA](reg); !errors.Is(err, ErrMissingRequirement) {
			t.Fatalf("Get after a refused typed-nil Put = %v, want ErrMissingRequirement", err)
		}
	})

	t.Run("a typed-nil product fails Construct", func(t *testing.T) {
		reg := newTestRegistry(t, recordingComponent(&stageLog{}, "typednil", "", nil, func(c *Component) {
			c.New = func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) { return typedNilProduct() }
		}))
		reg.Put(testComposition(configEntry{key: "typednil", value: nil}))
		if err := reg.Prepare(ctx); err != nil {
			t.Fatalf("Prepare = %v", err)
		}
		err := reg.Construct(ctx)
		if !errors.Is(err, ErrComponentFailed) || !strings.Contains(err.Error(), "returned no product") {
			t.Fatalf("Construct = %v, want ErrComponentFailed naming the absent product", err)
		}
		if _, err := Get[*compTokenA](reg); !errors.Is(err, ErrMissingRequirement) {
			t.Fatalf("Get after the failed Construct = %v, want ErrMissingRequirement; a typed nil must not be readable as a present value", err)
		}
	})

	t.Run("a typed-nil product fails Build", func(t *testing.T) {
		reg := newTestRegistry(t, recordingComponent(&stageLog{}, "typednilbuild", "", nil, func(c *Component) {
			c.New = func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) { return typedNilProduct() }
		}))
		reg.Put(testComposition(configEntry{key: "typednilbuild", value: nil}))
		if err := reg.Prepare(ctx); err != nil {
			t.Fatalf("Prepare = %v", err)
		}
		if built, err := Build[*compTokenA](ctx, reg, "typednilbuild", nil); err == nil {
			t.Fatalf("Build = (%v, nil), want an error for the typed-nil product", built)
		}
	})
}

func TestGetOptional(t *testing.T) {
	reg := NewComponentRegistry()

	// One match: a concrete pointer, the typical optional dependency.
	put := &compTokenA{}
	reg.Put(put)
	if v, ok, err := GetOptional[*compTokenA](reg); err != nil || !ok || v != put {
		t.Fatalf("GetOptional[*compTokenA] = (%v, %v, %v), want the put value with ok", v, ok, err)
	}

	// Zero matches: absence is (zero, false, nil), never an error -- the
	// optional dependency's documented default is the caller's to apply.
	if v, ok, err := GetOptional[*compTokenB](reg); v != nil || ok || err != nil {
		t.Fatalf("GetOptional[*compTokenB] = (%v, %v, %v), want (nil, false, nil)", v, ok, err)
	}

	// A non-pointer target reports its type's zero value on absence, not nil.
	if v, ok, err := GetOptional[compTokenA](reg); v != (compTokenA{}) || ok || err != nil {
		t.Fatalf("GetOptional[compTokenA] = (%v, %v, %v), want the zero struct with ok false", v, ok, err)
	}

	// Several matches: a true error, returned as-is rather than folded
	// into absence, so a caller that propagates fails the wiring loudly.
	reg.Put(&compTokenB{})
	reg.Put(&compTokenB{})
	v, ok, err := GetOptional[*compTokenB](reg)
	if v != nil || ok || !errors.Is(err, ErrAmbiguousProvider) {
		t.Fatalf("GetOptional for two values = (%v, %v, %v), want (nil, false, ErrAmbiguousProvider)", v, ok, err)
	}
	if !strings.Contains(err.Error(), "pkgcore.compTokenB") {
		t.Errorf("ambiguity error %q does not list the values", err)
	}
}

func TestComponentRegistryRegisterAndIsolation(t *testing.T) {
	a := Component{Name: "test.isolated.a", New: func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) { return &compTokenA{}, nil }}
	reg := newTestRegistry(t, a)

	if err := reg.Register(a); !errors.Is(err, ErrDuplicateComponent) {
		t.Fatalf("duplicate Register = %v, want ErrDuplicateComponent", err)
	}
	if err := reg.Register(Component{Name: ""}); !errors.Is(err, ErrInvalidComponent) {
		t.Fatalf("Register of a malformed component = %v, want ErrInvalidComponent", err)
	}

	other := NewComponentRegistry()
	if _, ok := other.registered(a.Name); ok {
		t.Errorf("a second registry sees the first registry's local component %q", a.Name)
	}
}

func TestSeatsClosedOutsideInit(t *testing.T) {
	reg := NewComponentRegistry()

	err := reg.Config.Add(ConfigItem{Key: "test.seat.key", Type: "string", Description: "d"})
	if !errors.Is(err, ErrStageViolation) {
		t.Fatalf("seat write before Init = %v, want ErrStageViolation", err)
	}
	for _, want := range []string{`"Config" seat`, "Init", "current stage is not started"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("seat error %q does not carry %q", err, want)
		}
	}

	// Reads before Init see nothing declared.
	if items := reg.Config.Items(); items != nil {
		t.Errorf("Config.Items() before Init = %v, want nil", items)
	}
	if routes := reg.Routes.Routes(); routes != nil {
		t.Errorf("Routes.Routes() before Init = %v, want nil", routes)
	}

	// A write from inside a Construct callback is refused naming the stage
	// running.
	var seatErr error
	log := &stageLog{}
	writer := recordingComponent(log, "seatwriter", "", &compTokenA{}, func(c *Component) {
		inner := c.New
		c.New = func(ctx context.Context, reg *ComponentRegistry, cfg ComponentConfig) (any, error) {
			seatErr = reg.Config.Add(ConfigItem{Key: "test.seat.key2", Type: "string", Description: "d"})
			return inner(ctx, reg, cfg)
		}
	})
	reg2 := newTestRegistry(t, writer)
	reg2.Put(testComposition(configEntry{key: "seatwriter", value: nil}))
	if err := reg2.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare = %v", err)
	}
	if err := reg2.Construct(context.Background()); err != nil {
		t.Fatalf("Construct = %v", err)
	}
	if !errors.Is(seatErr, ErrStageViolation) || !strings.Contains(seatErr.Error(), "current stage is construct") {
		t.Errorf("seat write during Construct = %v, want ErrStageViolation naming construct", seatErr)
	}

	// After Init completes the seats are closed again.
	reg3 := newTestRegistry(t, recordingComponent(&stageLog{}, "seatclosed", "", &compTokenB{}, nil))
	reg3.Put(testComposition(configEntry{key: "seatclosed", value: nil}))
	if err := runStages(context.Background(), reg3); err != nil {
		t.Fatalf("stages = %v", err)
	}
	if err := reg3.Config.Add(ConfigItem{Key: "test.seat.key3", Type: "string", Description: "d"}); !errors.Is(err, ErrStageViolation) {
		t.Errorf("seat write after Init = %v, want ErrStageViolation", err)
	}
}

// runStages drives Prepare through Start.
func runStages(ctx context.Context, reg *ComponentRegistry) error {
	if err := reg.Prepare(ctx); err != nil {
		return err
	}
	if err := reg.Construct(ctx); err != nil {
		return err
	}
	if err := reg.Verify(ctx); err != nil {
		return err
	}
	if err := reg.Init(ctx); err != nil {
		return err
	}
	return reg.Start(ctx)
}

func TestSeatVoidWritesPanicOutsideInit(t *testing.T) {
	reg := NewComponentRegistry()
	assertPanicContains(t, `"Routes" seat`, func() {
		reg.Routes.Mount("/x", http.NotFoundHandler())
	})
	assertPanicContains(t, `"Events" seat`, func() {
		reg.Events.Subscribe("test.event", func(context.Context, Event) error { return nil })
	})
}

func TestSeatsAcceptWritesDuringInit(t *testing.T) {
	log := &stageLog{}
	content := recordingComponent(log, "seatcontent", "", &compTokenA{}, func(c *Component) {
		c.Init = func(ctx context.Context, reg *ComponentRegistry, instance any) error {
			reg.Routes.Mount("/seatcontent", http.NotFoundHandler())
			if err := reg.Config.Add(ConfigItem{Key: "seatcontent.item", Type: "string", Description: "d"}); err != nil {
				return err
			}
			if err := reg.Features.Add(FeatureFlag{Key: "seatcontent.flag"}); err != nil {
				return err
			}
			if err := reg.Permissions.Add("seatcontent:read"); err != nil {
				return err
			}
			if err := reg.Jobs.Handle("seatcontent.job", func() {}); err != nil {
				return err
			}
			if err := reg.Notifications.Add(NotificationType{Key: "seatcontent.notif", Group: "seatcontent"}); err != nil {
				return err
			}
			if err := reg.Events.Publishes(EventDecl{Type: "seatcontent.event", PayloadType: "seatcontent.Payload"}); err != nil {
				return err
			}
			if err := reg.AuditActions.Add("seatcontent.audited"); err != nil {
				return err
			}
			if err := reg.Retention.Add(RetentionParticipant{
				Name:  "seatcontent.retention",
				Sweep: func(context.Context, TenantID, time.Time) (int, error) { return 0, nil },
				Erase: func(context.Context, SubjectRef) (int, error) { return 0, nil },
			}); err != nil {
				return err
			}
			return reg.Schedules.Add(PeriodicTask{
				Type:      "seatcontent.task",
				Every:     time.Minute,
				Scope:     PeriodicScopePerTenant,
				KeyPrefix: "seatcontent:",
			})
		}
	})

	reg := newTestRegistry(t, content)
	reg.Put(testComposition(configEntry{key: "seatcontent", value: nil}))
	if err := runStages(context.Background(), reg); err != nil {
		t.Fatalf("stages = %v", err)
	}

	if got := len(reg.Routes.Routes()); got != 1 {
		t.Errorf("Routes.Routes() = %d entries, want 1", got)
	}
	if got := len(reg.Config.Items()); got != 1 {
		t.Errorf("Config.Items() = %d entries, want 1", got)
	}
	if got := len(reg.Features.Flags()); got != 1 {
		t.Errorf("Features.Flags() = %d entries, want 1", got)
	}
	if perms := reg.Permissions.Permissions(); !reflect.DeepEqual(perms, []string{"seatcontent:read"}) {
		t.Errorf("Permissions() = %v", perms)
	}
	if got := reg.Jobs.Handlers(); len(got) != 1 {
		t.Errorf("Jobs.Handlers() = %v, want 1 entry", got)
	}
	if got := len(reg.Notifications.Types()); got != 1 {
		t.Errorf("Notifications.Types() = %d entries, want 1", got)
	}
	if got := len(reg.Events.Published()); got != 1 {
		t.Errorf("Events.Published() = %d entries, want 1", got)
	}
	if actions := reg.AuditActions.Actions(); !reflect.DeepEqual(actions, []string{"seatcontent.audited"}) {
		t.Errorf("AuditActions() = %v", actions)
	}
	if got := len(reg.Retention.Participants()); got != 1 {
		t.Errorf("Retention.Participants() = %d entries, want 1", got)
	}
	if got := len(reg.Schedules.Declarations()); got != 1 {
		t.Errorf("Schedules.Declarations() = %d entries, want 1", got)
	}
}

func TestStageOrderViolations(t *testing.T) {
	ctx := context.Background()
	reg := newTestRegistry(t, recordingComponent(&stageLog{}, "stageorder", "", &compTokenA{}, nil))
	reg.Put(testComposition(configEntry{key: "stageorder", value: nil}))

	if err := reg.Construct(ctx); !errors.Is(err, ErrStageViolation) || !strings.Contains(err.Error(), "requires a completed Prepare stage") {
		t.Errorf("Construct before Prepare = %v, want ErrStageViolation", err)
	}
	if err := reg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare = %v", err)
	}
	if err := reg.Prepare(ctx); !errors.Is(err, ErrStageViolation) || !strings.Contains(err.Error(), "already ran") {
		t.Errorf("second Prepare = %v, want ErrStageViolation", err)
	}
	if err := reg.Verify(ctx); !errors.Is(err, ErrStageViolation) || !strings.Contains(err.Error(), "requires a completed Construct stage") {
		t.Errorf("Verify before Construct = %v, want ErrStageViolation", err)
	}
	if err := reg.Construct(ctx); err != nil {
		t.Fatalf("Construct = %v", err)
	}
	if err := reg.Init(ctx); !errors.Is(err, ErrStageViolation) || !strings.Contains(err.Error(), "requires a completed Verify stage") {
		t.Errorf("Init before Verify = %v, want ErrStageViolation", err)
	}
	if err := reg.Start(ctx); !errors.Is(err, ErrStageViolation) || !strings.Contains(err.Error(), "requires a completed Init stage") {
		t.Errorf("Start before Init = %v, want ErrStageViolation", err)
	}
	if err := reg.Close(ctx); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := reg.Verify(ctx); !errors.Is(err, ErrStageViolation) || !strings.Contains(err.Error(), "closed") {
		t.Errorf("stage after Close = %v, want ErrStageViolation naming the closed assembly", err)
	}
}

func TestStageCallOrderGolden(t *testing.T) {
	ctx := context.Background()
	log := &stageLog{}

	// bbb requires aaa's product, and registration order is the reverse of
	// dependency order, so the golden pins both orders at once.
	aaa := recordingComponent(log, "aaa", "aaa", &compTokenA{}, func(c *Component) {
		c.Provides = []any{(*compTokenA)(nil)}
	})
	bbb := recordingComponent(log, "bbb", "bbb", &compTokenB{}, func(c *Component) {
		c.Requires = []Requirement{{Token: (*compTokenA)(nil)}}
		c.Provides = []any{(*compTokenB)(nil)}
	})

	reg := newTestRegistry(t, bbb, aaa)
	reg.Put(testComposition(
		configEntry{key: "bbb", value: nil},
		configEntry{key: "aaa", value: nil},
	))

	if err := runStages(ctx, reg); err != nil {
		t.Fatalf("stages = %v", err)
	}
	if err := reg.Stop(ctx); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	if err := reg.Close(ctx); err != nil {
		t.Fatalf("Close = %v", err)
	}

	want := []string{
		"bbb.prepare", "aaa.prepare",
		"aaa.new", "bbb.new",
		"aaa.verify", "bbb.verify",
		"aaa.init", "bbb.init",
		"aaa.start", "bbb.start",
		"bbb.stop", "aaa.stop",
		"bbb.close", "aaa.close",
	}
	if got := log.all(); !reflect.DeepEqual(got, want) {
		t.Errorf("stage call order =\n  %v\nwant\n  %v", got, want)
	}

	// The plan order is the dependency order.
	assertPlanOrder(t, reg, []string{"aaa", "bbb"})

	// MemberNames answers in dependency order.
	if got := MemberNames(reg, "bbb"); !reflect.DeepEqual(got, []string{"bbb"}) {
		t.Errorf("MemberNames(bbb) = %v", got)
	}
}

// assertPlanOrder fails t unless the registry's plan holds exactly names, in
// order.
func assertPlanOrder(t *testing.T, reg *ComponentRegistry, names []string) {
	t.Helper()
	var got []string
	for _, p := range reg.plannedComponents() {
		got = append(got, p.component.Name)
	}
	if !reflect.DeepEqual(got, names) {
		t.Errorf("plan order = %v, want %v", got, names)
	}
}

func TestRollbackOnVerifyFailureClosesOnce(t *testing.T) {
	ctx := context.Background()
	log := &stageLog{}
	boom := errors.New("boom")

	aaa := recordingComponent(log, "aaa", "", &compTokenA{}, nil)
	bbb := recordingComponent(log, "bbb", "", &compTokenB{}, func(c *Component) {
		c.Verify = func(context.Context, *ComponentRegistry, any) error {
			log.record("bbb.verify")
			return boom
		}
	})

	reg := newTestRegistry(t, aaa, bbb)
	reg.Put(testComposition(
		configEntry{key: "aaa", value: nil},
		configEntry{key: "bbb", value: nil},
	))
	if err := reg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare = %v", err)
	}
	if err := reg.Construct(ctx); err != nil {
		t.Fatalf("Construct = %v", err)
	}

	err := reg.Verify(ctx)
	if !errors.Is(err, ErrComponentFailed) {
		t.Fatalf("Verify = %v, want ErrComponentFailed", err)
	}
	if !errors.Is(err, boom) {
		t.Errorf("Verify error %v does not wrap the cause", err)
	}
	for _, want := range []string{"(stage verify)", `component "bbb"`, "boom", "rolled back: bbb, aaa"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry %q", err, want)
		}
	}
	if got := log.count("aaa.close"); got != 1 {
		t.Errorf("aaa closed %d times, want exactly once", got)
	}
	if got := log.count("bbb.close"); got != 1 {
		t.Errorf("bbb closed %d times, want exactly once", got)
	}

	// A host that joins Close into the failure observes the cached,
	// already-computed result: the callbacks do not run again.
	if err := reg.Close(ctx); err != nil {
		t.Errorf("Close after rollback = %v, want the cached nil", err)
	}
	if got := log.count("aaa.close") + log.count("bbb.close"); got != 2 {
		t.Errorf("closes after a second Close = %d, want no more than the first pass", got)
	}
}

func TestRollbackOnConstructFailure(t *testing.T) {
	ctx := context.Background()

	t.Run("later component fails, earlier is rolled back", func(t *testing.T) {
		log := &stageLog{}
		aaa := recordingComponent(log, "aaa", "", &compTokenA{}, nil)
		bbb := recordingComponent(log, "bbb", "", &compTokenB{}, func(c *Component) {
			c.New = func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) {
				return nil, errors.New("dial tcp: refused")
			}
		})

		reg := newTestRegistry(t, aaa, bbb)
		reg.Put(testComposition(
			configEntry{key: "aaa", value: nil},
			configEntry{key: "bbb", value: nil},
		))
		if err := reg.Prepare(ctx); err != nil {
			t.Fatalf("Prepare = %v", err)
		}
		err := reg.Construct(ctx)
		if !errors.Is(err, ErrComponentFailed) {
			t.Fatalf("Construct = %v, want ErrComponentFailed", err)
		}
		for _, want := range []string{"(stage construct)", `component "bbb"`, "dial tcp: refused", "rolled back: aaa"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not carry %q", err, want)
			}
		}
		if got := log.count("aaa.close"); got != 1 {
			t.Errorf("aaa closed %d times, want exactly once", got)
		}
	})

	t.Run("first component fails, nothing was constructed", func(t *testing.T) {
		reg := newTestRegistry(t, recordingComponent(&stageLog{}, "firstfail", "", &compTokenA{}, func(c *Component) {
			c.New = func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) {
				return nil, errors.New("no product")
			}
		}))
		reg.Put(testComposition(configEntry{key: "firstfail", value: nil}))
		if err := reg.Prepare(ctx); err != nil {
			t.Fatalf("Prepare = %v", err)
		}
		err := reg.Construct(ctx)
		if !errors.Is(err, ErrComponentFailed) || !strings.Contains(err.Error(), "rolled back: none") {
			t.Errorf("Construct = %v, want ErrComponentFailed with an empty rollback set", err)
		}
	})

	t.Run("New returning nil is refused", func(t *testing.T) {
		reg := newTestRegistry(t, recordingComponent(&stageLog{}, "nilproduct", "", nil, func(c *Component) {
			c.New = func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) { return nil, nil }
		}))
		reg.Put(testComposition(configEntry{key: "nilproduct", value: nil}))
		if err := reg.Prepare(ctx); err != nil {
			t.Fatalf("Prepare = %v", err)
		}
		err := reg.Construct(ctx)
		if !errors.Is(err, ErrComponentFailed) || !strings.Contains(err.Error(), "returned no product") {
			t.Errorf("Construct = %v, want ErrComponentFailed naming the nil product", err)
		}
	})
}

func TestCloseAggregatesErrorsInReverseOrder(t *testing.T) {
	ctx := context.Background()
	log := &stageLog{}
	errA := errors.New("close a failed")
	errB := errors.New("close b failed")

	aaa := recordingComponent(log, "aaa", "", &compTokenA{}, func(c *Component) {
		c.Close = func(context.Context, *ComponentRegistry, any) error {
			log.record("aaa.close")
			return errA
		}
	})
	bbb := recordingComponent(log, "bbb", "", &compTokenB{}, func(c *Component) {
		c.Close = func(context.Context, *ComponentRegistry, any) error {
			log.record("bbb.close")
			return errB
		}
	})

	reg := newTestRegistry(t, aaa, bbb)
	reg.Put(testComposition(
		configEntry{key: "aaa", value: nil},
		configEntry{key: "bbb", value: nil},
	))
	if err := runStages(ctx, reg); err != nil {
		t.Fatalf("stages = %v", err)
	}

	err := reg.Close(ctx)
	if !errors.Is(err, errA) || !errors.Is(err, errB) {
		t.Fatalf("Close = %v, want both close failures aggregated", err)
	}
	order := []string{}
	for _, e := range log.all() {
		if strings.HasSuffix(e, ".close") {
			order = append(order, e)
		}
	}
	if want := []string{"bbb.close", "aaa.close"}; !reflect.DeepEqual(order, want) {
		t.Errorf("close order = %v, want reverse dependency order %v", order, want)
	}

	again := reg.Close(ctx)
	if !errors.Is(again, errA) || !errors.Is(again, errB) {
		t.Errorf("second Close = %v, want the cached aggregate", again)
	}
	if got := log.count("aaa.close") + log.count("bbb.close"); got != 2 {
		t.Errorf("close callbacks ran %d times, want exactly once each", got)
	}
}

func TestStopIgnoresFailuresAndDoesNotClose(t *testing.T) {
	ctx := context.Background()
	log := &stageLog{}
	aaa := recordingComponent(log, "aaa", "", &compTokenA{}, func(c *Component) {
		c.Stop = func(context.Context, *ComponentRegistry, any) error {
			log.record("aaa.stop")
			return errors.New("stop notification failed")
		}
	})

	reg := newTestRegistry(t, aaa)
	reg.Put(testComposition(configEntry{key: "aaa", value: nil}))
	if err := runStages(ctx, reg); err != nil {
		t.Fatalf("stages = %v", err)
	}
	if err := reg.Stop(ctx); err != nil {
		t.Fatalf("Stop = %v, want nil: stop failures are ignored", err)
	}
	if got := log.count("aaa.close"); got != 0 {
		t.Errorf("Stop closed %d components, want none", got)
	}
	if err := reg.Close(ctx); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if got := log.count("aaa.close"); got != 1 {
		t.Errorf("Close after Stop closed %d times, want exactly once", got)
	}
}

// productCloser is a component product that owns a resource the way an
// implementation built from its own configuration does: the ownership is
// declared by the standard Close() error alone, with no adapter on the
// descriptor.
type productCloser struct {
	log  *stageLog
	name string
	err  error
}

func (c *productCloser) Close() error {
	c.log.record(c.name + ".product.close")
	return c.err
}

// TestCloseReleasesCallbacklessProducts pins the registry's release rule for
// a component that declares no Close callback: its product's own Close()
// error is the ownership declaration -- the same structural contract
// Registration documents for a value resolved through SeamRegistry.Build --
// so the close stage must run it, exactly once and in reverse construction
// order, route its failure into the aggregated Close result under the
// component's name, and record the component in the rollback set as any
// other released member. A declared callback supersedes the fallback, and a
// product without a closer is skipped.
func TestCloseReleasesCallbacklessProducts(t *testing.T) {
	ctx := context.Background()

	t.Run("released in reverse order without a declared callback", func(t *testing.T) {
		log := &stageLog{}
		aaa := recordingComponent(log, "aaa", "", &productCloser{log: log, name: "aaa"}, func(c *Component) { c.Close = nil })
		bbb := recordingComponent(log, "bbb", "", &productCloser{log: log, name: "bbb"}, func(c *Component) { c.Close = nil })

		reg := newTestRegistry(t, aaa, bbb)
		reg.Put(testComposition(
			configEntry{key: "aaa", value: nil},
			configEntry{key: "bbb", value: nil},
		))
		if err := runStages(ctx, reg); err != nil {
			t.Fatalf("stages = %v", err)
		}
		if err := reg.Close(ctx); err != nil {
			t.Fatalf("Close = %v, want the products released", err)
		}
		order := []string{}
		for _, e := range log.all() {
			if strings.HasSuffix(e, ".product.close") {
				order = append(order, e)
			}
		}
		if want := []string{"bbb.product.close", "aaa.product.close"}; !reflect.DeepEqual(order, want) {
			t.Errorf("release order = %v, want reverse construction order %v", order, want)
		}

		// The exactly-once result is cached like any other Close result.
		if err := reg.Close(ctx); err != nil {
			t.Errorf("second Close = %v, want the cached nil", err)
		}
		if got := log.count("aaa.product.close") + log.count("bbb.product.close"); got != 2 {
			t.Errorf("products released %d times, want exactly once each", got)
		}
	})

	t.Run("declared callback supersedes the product closer", func(t *testing.T) {
		log := &stageLog{}
		// The fixture keeps its default Close callback; the product's own
		// Close must not run on top of it, so its record would be the
		// failure.
		ccc := recordingComponent(log, "ccc", "", &productCloser{log: log, name: "ccc"}, nil)

		reg := newTestRegistry(t, ccc)
		reg.Put(testComposition(configEntry{key: "ccc", value: nil}))
		if err := runStages(ctx, reg); err != nil {
			t.Fatalf("stages = %v", err)
		}
		if err := reg.Close(ctx); err != nil {
			t.Fatalf("Close = %v", err)
		}
		if got := log.count("ccc.close"); got != 1 {
			t.Errorf("declared callback ran %d times, want exactly once", got)
		}
		if got := log.count("ccc.product.close"); got != 0 {
			t.Errorf("product Close ran %d times behind a declared callback, want none", got)
		}
	})

	t.Run("product failure is aggregated under the component name", func(t *testing.T) {
		log := &stageLog{}
		boom := errors.New("release failed")
		ddd := recordingComponent(log, "ddd", "", &productCloser{log: log, name: "ddd", err: boom}, func(c *Component) { c.Close = nil })

		reg := newTestRegistry(t, ddd)
		reg.Put(testComposition(configEntry{key: "ddd", value: nil}))
		if err := runStages(ctx, reg); err != nil {
			t.Fatalf("stages = %v", err)
		}
		err := reg.Close(ctx)
		if !errors.Is(err, boom) {
			t.Fatalf("Close = %v, want the product's failure wrapped", err)
		}
		for _, want := range []string{`component "ddd"`, "(stage close)"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Close error %q does not carry %q", err, want)
			}
		}
		if again := reg.Close(ctx); !errors.Is(again, boom) {
			t.Errorf("second Close = %v, want the cached failure", again)
		}
		if got := log.count("ddd.product.close"); got != 1 {
			t.Errorf("product released %d times, want exactly once", got)
		}
	})

	t.Run("rollback releases callbackless products too", func(t *testing.T) {
		log := &stageLog{}
		aaa := recordingComponent(log, "aaa", "", &productCloser{log: log, name: "aaa"}, func(c *Component) { c.Close = nil })
		bbb := recordingComponent(log, "bbb", "", &compTokenB{}, func(c *Component) {
			c.New = func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) {
				return nil, errors.New("dial tcp: refused")
			}
		})

		reg := newTestRegistry(t, aaa, bbb)
		reg.Put(testComposition(
			configEntry{key: "aaa", value: nil},
			configEntry{key: "bbb", value: nil},
		))
		if err := reg.Prepare(ctx); err != nil {
			t.Fatalf("Prepare = %v", err)
		}
		err := reg.Construct(ctx)
		if !errors.Is(err, ErrComponentFailed) {
			t.Fatalf("Construct = %v, want ErrComponentFailed", err)
		}
		if !strings.Contains(err.Error(), "rolled back: aaa") {
			t.Errorf("Construct error %q does not record the released product in its rollback set", err)
		}
		if got := log.count("aaa.product.close"); got != 1 {
			t.Errorf("aaa's product released %d times during rollback, want exactly once", got)
		}
	})

	t.Run("product without a closer is skipped", func(t *testing.T) {
		log := &stageLog{}
		eee := recordingComponent(log, "eee", "", &compTokenA{}, func(c *Component) { c.Close = nil })
		reg := newTestRegistry(t, eee)
		reg.Put(testComposition(configEntry{key: "eee", value: nil}))
		if err := runStages(ctx, reg); err != nil {
			t.Fatalf("stages = %v", err)
		}
		if err := reg.Close(ctx); err != nil {
			t.Errorf("Close = %v, want nil for a product with nothing to release", err)
		}
	})

	t.Run("Stop does not release products", func(t *testing.T) {
		log := &stageLog{}
		fff := recordingComponent(log, "fff", "", &productCloser{log: log, name: "fff"}, func(c *Component) { c.Close = nil })
		reg := newTestRegistry(t, fff)
		reg.Put(testComposition(configEntry{key: "fff", value: nil}))
		if err := runStages(ctx, reg); err != nil {
			t.Fatalf("stages = %v", err)
		}
		if err := reg.Stop(ctx); err != nil {
			t.Fatalf("Stop = %v", err)
		}
		if got := log.count("fff.product.close"); got != 0 {
			t.Errorf("Stop released %d products, want none: the release belongs to the close stage", got)
		}
		if err := reg.Close(ctx); err != nil {
			t.Fatalf("Close = %v", err)
		}
		if got := log.count("fff.product.close"); got != 1 {
			t.Errorf("Close after Stop released %d products, want exactly once", got)
		}
	})
}

// buildProduct is the product of the Build fixture component; its tag proves
// which configuration New received.
type buildProduct struct{ tag string }

// buildSchema is the Build fixture component's configuration schema.
type buildSchema struct {
	Tag string `json:"tag"`
}

func TestBuild(t *testing.T) {
	ctx := context.Background()
	log := &stageLog{}
	aaa := Component{
		Name:         "buildme",
		Module:       "buildme",
		ConfigSchema: (*buildSchema)(nil),
		New: func(_ context.Context, _ *ComponentRegistry, cfg ComponentConfig) (any, error) {
			log.record("buildme.new")
			var c buildSchema
			if err := cfg.Decode(&c); err != nil {
				return nil, err
			}
			return &buildProduct{tag: c.Tag}, nil
		},
	}
	unselected := recordingComponent(log, "buildunselected", "", &compTokenB{}, nil)

	reg := newTestRegistry(t, aaa, unselected)
	reg.Put(testComposition(
		configEntry{key: "buildme", value: map[string]any{"tag": "planned"}},
	))
	if err := runStages(ctx, reg); err != nil {
		t.Fatalf("stages = %v", err)
	}

	built, err := Build[*buildProduct](ctx, reg, "buildme", nil)
	if err != nil {
		t.Fatalf("Build = (%v, %v), want a product", built, err)
	}
	if built.tag != "planned" {
		t.Errorf("Build(nil override) tag = %q, want the resolved configuration's %q", built.tag, "planned")
	}

	override := NewComponentConfig(map[string]any{"tag": "override"})
	built, err = Build[*buildProduct](ctx, reg, "buildme", &override)
	if err != nil {
		t.Fatalf("Build with override = (%v, %v), want a product", built, err)
	}
	if built.tag != "override" {
		t.Errorf("Build(override) tag = %q, want %q", built.tag, "override")
	}

	if _, err := Build[*compTokenB](ctx, reg, "buildunselected", nil); !errors.Is(err, ErrUnknownComponent) || !strings.Contains(err.Error(), "not selected") {
		t.Errorf("Build of an unselected component = %v, want ErrUnknownComponent", err)
	}
	if _, err := Build[*compTokenB](ctx, reg, "never.registered", nil); !errors.Is(err, ErrUnknownComponent) || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("Build of an unregistered component = %v, want ErrUnknownComponent", err)
	}
	if _, err := Build[*compTokenB](ctx, reg, "buildme", nil); err == nil {
		t.Error("Build into the wrong type succeeded, want an assignability error")
	}
}

func TestComponentCapabilities(t *testing.T) {
	ctx := context.Background()
	log := &stageLog{}
	declared := recordingComponent(log, "capdeclared", "capdeclared", &compTokenA{}, func(c *Component) {
		c.Capabilities = MultiReplicaSafe | Stateless
	})
	bare := recordingComponent(log, "capbare", "capbare", &compTokenB{}, nil)
	unselected := recordingComponent(log, "capunselected", "capunselected", &compSpreadImpl{}, func(c *Component) {
		c.Capabilities = SurvivesRestart
	})

	reg := newTestRegistry(t, declared, bare, unselected)
	reg.Put(testComposition(
		configEntry{key: "capdeclared", value: nil},
		configEntry{key: "capbare", value: nil},
	))
	if err := runStages(ctx, reg); err != nil {
		t.Fatalf("stages = %v", err)
	}

	caps, err := ComponentCapabilities(reg, "capdeclared")
	if err != nil || caps != MultiReplicaSafe|Stateless {
		t.Errorf("ComponentCapabilities(capdeclared) = (%v, %v), want the declared MultiReplicaSafe|Stateless", caps, err)
	}

	caps, err = ComponentCapabilities(reg, "capbare")
	if err != nil || caps != 0 {
		t.Errorf("ComponentCapabilities(capbare) = (%v, %v), want the zero Capability without an error", caps, err)
	}

	// The read follows the selection, not the registration: an unselected
	// descriptor's declaration is not in effect.
	if _, err := ComponentCapabilities(reg, "capunselected"); !errors.Is(err, ErrUnknownComponent) || !strings.Contains(err.Error(), "not selected") {
		t.Errorf("ComponentCapabilities of an unselected component = %v, want ErrUnknownComponent naming the selection gap", err)
	}
	if _, err := ComponentCapabilities(reg, "never.registered"); !errors.Is(err, ErrUnknownComponent) || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("ComponentCapabilities of an unregistered component = %v, want ErrUnknownComponent naming the registration gap", err)
	}
}

func TestMemberNames(t *testing.T) {
	ctx := context.Background()
	log := &stageLog{}
	gatewayB := recordingComponent(log, "gateway.b", "gateway", &compTokenA{}, nil)
	gatewayA := recordingComponent(log, "gateway.a", "gateway", &compTokenB{}, nil)
	other := recordingComponent(log, "other", "other", &compSpreadImpl{}, nil)

	reg := newTestRegistry(t, gatewayB, gatewayA, other)
	reg.Put(testComposition(
		configEntry{key: "gateway.a", value: nil},
		configEntry{key: "gateway.b", value: nil},
	))
	if err := runStages(ctx, reg); err != nil {
		t.Fatalf("stages = %v", err)
	}

	if got, want := MemberNames(reg, "gateway"), []string{"gateway.a", "gateway.b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("MemberNames(gateway) = %v, want %v", got, want)
	}
	if got := MemberNames(reg, "unselected.module"); len(got) != 0 {
		t.Errorf("MemberNames of an unselected module = %v, want empty", got)
	}
}

func TestInitClosingValidation(t *testing.T) {
	ctx := context.Background()

	t.Run("system purposes are registered together", func(t *testing.T) {
		purposeA := SystemPurpose("test.closing.purpose.a")
		purposeB := SystemPurpose("test.closing.purpose.b")
		a := plainComponent("closing.a", &compTokenA{})
		a.SystemPurposes = []SystemPurpose{purposeA}
		b := plainComponent("closing.b", &compTokenB{})
		b.SystemPurposes = []SystemPurpose{purposeB}

		reg := newTestRegistry(t, a, b)
		reg.Put(testComposition(
			configEntry{key: "closing.a", value: nil},
			configEntry{key: "closing.b", value: nil},
		))
		if err := runStages(ctx, reg); err != nil {
			t.Fatalf("stages = %v", err)
		}
		if !systemPurposeRegistered(purposeA) || !systemPurposeRegistered(purposeB) {
			t.Error("every selected component's declared system purposes must be registered by Init")
		}
	})

	t.Run("duplicate system purposes fail closed", func(t *testing.T) {
		shared := SystemPurpose("test.closing.purpose.shared")
		a := plainComponent("closing.dup.a", &compTokenA{})
		a.SystemPurposes = []SystemPurpose{shared}
		b := plainComponent("closing.dup.b", &compTokenB{})
		b.SystemPurposes = []SystemPurpose{shared}

		reg := newTestRegistry(t, a, b)
		reg.Put(testComposition(
			configEntry{key: "closing.dup.a", value: nil},
			configEntry{key: "closing.dup.b", value: nil},
		))
		err := runStages(ctx, reg)
		if !errors.Is(err, ErrComponentFailed) {
			t.Fatalf("Init = %v, want ErrComponentFailed", err)
		}
		for _, want := range []string{"(stage init)", "system purpose", "closing.dup.a", "closing.dup.b"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not carry %q", err, want)
			}
		}
		if systemPurposeRegistered(shared) {
			t.Error("a purpose a duplicate declaration named was registered despite the failure")
		}
	})

	t.Run("a bootstrap key on the runtime seat fails the assembly", func(t *testing.T) {
		app := plainComponent("closing.boot", &compTokenA{})
		app.BootstrapKeys = []BootstrapKey{{Key: "closing.boot.secret", Format: "hexkey"}}
		seater := plainComponent("closing.seat", &compTokenB{})
		seater.Init = func(_ context.Context, reg *ComponentRegistry, _ any) error {
			return reg.Config.Add(ConfigItem{Key: "closing.boot.secret", Type: "string", Description: "d"})
		}

		reg := newTestRegistry(t, app, seater)
		reg.Put(testComposition(
			configEntry{key: "closing.boot", value: nil},
			configEntry{key: "closing.seat", value: nil},
		))
		err := runStages(ctx, reg)
		if !errors.Is(err, ErrComponentFailed) {
			t.Fatalf("Init = %v, want ErrComponentFailed", err)
		}
		for _, want := range []string{"(stage init)", "two layers", `"closing.boot"`, `"closing.boot.secret"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not carry %q", err, want)
			}
		}
	})
}

// TestSystemPurposesRegisteredAtInitEntry pins the ordering contract host
// boot steps rely on: every selected component's declared purposes are
// registered at the Init stage's entry, before any Init callback runs, so a
// callback can open a system context under a purpose another selected
// component declared.
func TestSystemPurposesRegisteredAtInitEntry(t *testing.T) {
	ctx := context.Background()

	t.Run("another component's Init callback sees the declared purpose", func(t *testing.T) {
		purpose := SystemPurpose("test.initentry.cross_component")
		declarer := plainComponent("initentry.declarer", &compTokenA{})
		declarer.SystemPurposes = []SystemPurpose{purpose}
		user := plainComponent("initentry.user", &compTokenB{})
		var callbackErr error
		user.Init = func(ctx context.Context, _ *ComponentRegistry, _ any) error {
			_, callbackErr = WithSystemContext(ctx, SystemReason{Actor: "test-actor", Purpose: purpose})
			return callbackErr
		}

		reg := newTestRegistry(t, declarer, user)
		reg.Put(testComposition(
			configEntry{key: "initentry.declarer", value: nil},
			configEntry{key: "initentry.user", value: nil},
		))
		if err := runStages(ctx, reg); err != nil {
			t.Fatalf("stages = %v", err)
		}
		if callbackErr != nil {
			t.Errorf("WithSystemContext inside an Init callback = %v, want nil", callbackErr)
		}
	})

	t.Run("the declaring component's own Init callback sees its purpose", func(t *testing.T) {
		purpose := SystemPurpose("test.initentry.self")
		declarer := plainComponent("initentry.self", &compTokenA{})
		declarer.SystemPurposes = []SystemPurpose{purpose}
		declarer.Init = func(context.Context, *ComponentRegistry, any) error {
			if !systemPurposeRegistered(purpose) {
				return fmt.Errorf("system purpose %q is not registered at the declaring component's own Init callback", purpose)
			}
			return nil
		}

		reg := newTestRegistry(t, declarer)
		reg.Put(testComposition(configEntry{key: "initentry.self", value: nil}))
		if err := runStages(ctx, reg); err != nil {
			t.Fatalf("stages = %v", err)
		}
	})
}

func TestAssetsCollectsOnlyCarriersInPlanOrder(t *testing.T) {
	ctx := context.Background()
	withAssets := Component{
		Name:        "locgood",
		Module:      "locgood",
		Migrations:  migrations.FS,
		Locales:     locales.FS,
		OpenAPISpec: []byte("openapi: 3.0.3\ninfo:\n  title: fixture\n"),
		New:         func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) { return &compTokenA{}, nil },
	}
	withoutAssets := Component{
		Name: "noassets",
		New:  func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) { return &compTokenB{}, nil },
	}

	reg := newTestRegistry(t, withoutAssets, withAssets)
	reg.Put(testComposition(
		configEntry{key: "noassets", value: nil},
		configEntry{key: "locgood", value: nil},
	))
	if err := runStages(ctx, reg); err != nil {
		t.Fatalf("stages = %v", err)
	}

	assets := Assets(reg)
	if len(assets) != 1 {
		t.Fatalf("Assets() = %d entries, want 1 (only the carrier)", len(assets))
	}
	if assets[0].Name != "locgood" {
		t.Errorf("Assets()[0].Name = %q, want locgood", assets[0].Name)
	}
	if assets[0].Module != "locgood" {
		t.Errorf("Assets()[0].Module = %q, want locgood: the migration set's ledger key is the module the component implements", assets[0].Module)
	}
	if assets[0].Migrations == (embed.FS{}) || assets[0].Locales == (embed.FS{}) || len(assets[0].OpenAPISpec) == 0 {
		t.Errorf("Assets()[0] = %+v, want all three assets carried", assets[0])
	}
}

func TestConstructProductDeliversProvides(t *testing.T) {
	ctx := context.Background()

	t.Run("a declared token the construction did not deliver fails", func(t *testing.T) {
		log := &stageLog{}
		good1 := recordingComponent(log, "provgood1", "", &compTokenA{}, nil)
		good2 := recordingComponent(log, "provgood2", "", &compTokenB{}, nil)
		mismatched := recordingComponent(log, "provbad", "", &compTokenB{}, func(c *Component) {
			c.Provides = []any{(*compTokenA)(nil)}
		})

		reg := newTestRegistry(t, good1, good2, mismatched)
		reg.Put(testComposition(
			configEntry{key: "provgood1", value: nil},
			configEntry{key: "provgood2", value: nil},
			configEntry{key: "provbad", value: nil},
		))
		if err := reg.Prepare(ctx); err != nil {
			t.Fatalf("Prepare = %v", err)
		}

		err := reg.Construct(ctx)
		if !errors.Is(err, ErrComponentFailed) {
			t.Fatalf("Construct = %v, want ErrComponentFailed", err)
		}
		for _, want := range []string{
			"(stage construct)",
			`component "provbad"`,
			"the construction delivered neither the product *pkgcore.compTokenB nor a value put during New for the declared token(s) pkgcore.compTokenA",
			"rolled back: provgood2, provgood1",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not carry %q", err, want)
			}
		}

		// The two components constructed before the failure are closed
		// exactly once, in reverse construction order; the failing
		// component never became a constructed member, so its Close does
		// not run.
		var closes []string
		for _, e := range log.all() {
			if strings.HasSuffix(e, ".close") {
				closes = append(closes, e)
			}
		}
		if want := []string{"provgood2.close", "provgood1.close"}; !reflect.DeepEqual(closes, want) {
			t.Errorf("close order = %v, want %v", closes, want)
		}
		if got := log.count("provbad.close"); got != 0 {
			t.Errorf("the failing component closed %d times, want 0: it was never constructed", got)
		}
	})

	t.Run("every declaration is asserted, not just one", func(t *testing.T) {
		// The product matches the first declaration; the second one nothing
		// delivered. An at-least-one reading would construct this component
		// and leave the second declaration as a promise no Get could ever
		// keep.
		partial := recordingComponent(&stageLog{}, "provpartial", "", &compTokenA{}, func(c *Component) {
			c.Provides = []any{(*compTokenA)(nil), (*compTokenB)(nil)}
		})

		reg := newTestRegistry(t, partial)
		reg.Put(testComposition(configEntry{key: "provpartial", value: nil}))
		if err := reg.Prepare(ctx); err != nil {
			t.Fatalf("Prepare = %v", err)
		}

		err := reg.Construct(ctx)
		if !errors.Is(err, ErrComponentFailed) {
			t.Fatalf("Construct = %v, want ErrComponentFailed", err)
		}
		for _, want := range []string{
			`component "provpartial"`,
			"pkgcore.compTokenB",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not carry %q", err, want)
			}
		}
	})

	t.Run("a value put inside New delivers a declaration", func(t *testing.T) {
		// The multi-value delivery shape: the product is one declared
		// token, a second construction value is put inside New.
		multi := Component{
			Name:     "provmulti",
			Requires: nil,
			Provides: []any{(*compTokenA)(nil), (*compTokenB)(nil)},
			New: func(_ context.Context, reg *ComponentRegistry, _ ComponentConfig) (any, error) {
				reg.Put(&compTokenB{})
				return &compTokenA{}, nil
			},
		}

		reg := newTestRegistry(t, multi)
		reg.Put(testComposition(configEntry{key: "provmulti", value: nil}))
		if err := reg.Prepare(ctx); err != nil {
			t.Fatalf("Prepare = %v", err)
		}
		if err := reg.Construct(ctx); err != nil {
			t.Fatalf("Construct = %v, want nil", err)
		}
		if _, err := Get[*compTokenB](reg); err != nil {
			t.Errorf("Get[*compTokenB] after construction = %v, want the value put inside New", err)
		}
	})

	t.Run("another component's value does not deliver the declaration", func(t *testing.T) {
		// A earlier component put a *compTokenB; the later component
		// declares (*compTokenB)(nil) but delivers nothing itself. The
		// declaration is still unmet: a promise names this component's own
		// delivery, not whatever answer a Get might find in the shared
		// context.
		earlier := Component{
			Name: "provother",
			New: func(_ context.Context, reg *ComponentRegistry, _ ComponentConfig) (any, error) {
				reg.Put(&compTokenB{})
				return &compTokenA{}, nil
			},
		}
		declares := recordingComponent(&stageLog{}, "provborrow", "", &compTokenA{}, func(c *Component) {
			c.Provides = []any{(*compTokenB)(nil)}
		})

		reg := newTestRegistry(t, earlier, declares)
		reg.Put(testComposition(
			configEntry{key: "provother", value: nil},
			configEntry{key: "provborrow", value: nil},
		))
		if err := reg.Prepare(ctx); err != nil {
			t.Fatalf("Prepare = %v", err)
		}

		err := reg.Construct(ctx)
		if !errors.Is(err, ErrComponentFailed) {
			t.Fatalf("Construct = %v, want ErrComponentFailed", err)
		}
		if !strings.Contains(err.Error(), "pkgcore.compTokenB") {
			t.Errorf("error %q does not name the undelivered declaration", err)
		}
	})

	t.Run("matching products construct", func(t *testing.T) {
		direct := recordingComponent(&stageLog{}, "provdirect", "", &compTokenA{}, func(c *Component) {
			c.Provides = []any{(*compTokenA)(nil)}
		})
		viaInterface := recordingComponent(&stageLog{}, "proviface", "", compSpreadImpl{}, func(c *Component) {
			c.Provides = []any{(*compSpreader)(nil)}
		})

		reg := newTestRegistry(t, direct, viaInterface)
		reg.Put(testComposition(
			configEntry{key: "provdirect", value: nil},
			configEntry{key: "proviface", value: nil},
		))
		if err := reg.Prepare(ctx); err != nil {
			t.Fatalf("Prepare = %v", err)
		}
		if err := reg.Construct(ctx); err != nil {
			t.Fatalf("Construct = %v, want nil", err)
		}
	})

	t.Run("no Provides declarations accept any product", func(t *testing.T) {
		reg := newTestRegistry(t, plainComponent("provnone", &compTokenB{}))
		reg.Put(testComposition(configEntry{key: "provnone", value: nil}))
		if err := reg.Prepare(ctx); err != nil {
			t.Fatalf("Prepare = %v", err)
		}
		if err := reg.Construct(ctx); err != nil {
			t.Fatalf("Construct = %v, want nil", err)
		}
	})
}

// ownConfig is the configuration schema the OwnComponentConfig fixtures
// declare: one string key.
type ownConfig struct {
	Value string `json:"value"`
}

// TestOwnComponentConfigFollowsTheRunningComponent pins the reading a
// Prepare callback uses to see its own resolved block. The callback is
// bound to the package's descriptor, so when a host selects a renamed copy
// of that descriptor, the copy's block -- not the original name's -- must
// be the one the callback reads; a literal components.<package-name>
// lookup would silently miss the copy's configuration entirely.
func TestOwnComponentConfigFollowsTheRunningComponent(t *testing.T) {
	ctx := context.Background()

	var seen []string
	sharedPrepare := func(_ context.Context, reg *ComponentRegistry) error {
		block, err := OwnComponentConfig(reg)
		if err != nil {
			return err
		}
		value, err := Value[string](block, "value")
		if err != nil {
			return err
		}
		seen = append(seen, value)
		return nil
	}

	original := Component{
		Name:         "own.original",
		ConfigSchema: (*ownConfig)(nil),
		Prepare:      sharedPrepare,
		New:          func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) { return &compTokenA{}, nil },
	}
	renamed := original
	renamed.Name = "host.renamed"
	renamed.New = func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) { return &compTokenB{}, nil }

	reg := newTestRegistry(t, original, renamed)
	reg.Put(testComposition(
		configEntry{key: "own.original", value: map[string]any{"value": "original-block"}},
		configEntry{key: "host.renamed", value: map[string]any{"value": "renamed-block"}},
	))
	if err := reg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare = %v", err)
	}

	// Prepare callbacks run in registration order.
	if want := []string{"original-block", "renamed-block"}; !reflect.DeepEqual(seen, want) {
		t.Errorf("the shared Prepare callback read %v, want %v (each component its own block)", seen, want)
	}
}

// TestOwnComponentConfigOutsidePrepareFails pins the reading's stage: no
// component's configuration is "current" outside its Prepare callback, so
// the call fails rather than answering with whichever component ran last.
func TestOwnComponentConfigOutsidePrepareFails(t *testing.T) {
	reg := NewComponentRegistry()
	if _, err := OwnComponentConfig(reg); !errors.Is(err, ErrStageViolation) {
		t.Fatalf("OwnComponentConfig outside a Prepare callback = %v, want ErrStageViolation", err)
	}
}
