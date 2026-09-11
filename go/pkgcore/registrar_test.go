package pkgcore

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestRegistrarSeatsAreTheHeldObjects pins the pure-view property on both
// registry shapes: every seat accessor returns the very registrar the
// registry holds, so a declaration made through the accessor is
// indistinguishable from one made through the registry's own field.
func TestRegistrarSeatsAreTheHeldObjects(t *testing.T) {
	t.Parallel()

	moduleReg := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())
	componentReg := NewComponentRegistry()

	checks := []struct {
		seat string
		same bool
	}{
		{"Routes", moduleReg.RoutesSeat() == moduleReg.Routes && componentReg.RoutesSeat() == componentReg.Routes},
		{"Config", moduleReg.ConfigSeat() == moduleReg.Config && componentReg.ConfigSeat() == componentReg.Config},
		{"Features", moduleReg.FeaturesSeat() == moduleReg.Features && componentReg.FeaturesSeat() == componentReg.Features},
		{"Permissions", moduleReg.PermissionsSeat() == moduleReg.Permissions && componentReg.PermissionsSeat() == componentReg.Permissions},
		{"Jobs", moduleReg.JobsSeat() == moduleReg.Jobs && componentReg.JobsSeat() == componentReg.Jobs},
		{"Notifications", moduleReg.NotificationsSeat() == moduleReg.Notifications && componentReg.NotificationsSeat() == componentReg.Notifications},
		{"Events", moduleReg.EventsSeat() == moduleReg.Events && componentReg.EventsSeat() == componentReg.Events},
		{"AuditActions", moduleReg.AuditActionsSeat() == moduleReg.AuditActions && componentReg.AuditActionsSeat() == componentReg.AuditActions},
		{"Retention", moduleReg.RetentionSeat() == moduleReg.Retention && componentReg.RetentionSeat() == componentReg.Retention},
		{"Schedules", moduleReg.SchedulesSeat() == moduleReg.Schedules && componentReg.SchedulesSeat() == componentReg.Schedules},
	}
	for _, check := range checks {
		if !check.same {
			t.Errorf("%s: an accessor returned a registrar other than the one the registry holds", check.seat)
		}
	}
}

// TestRegistrarValueAccessorsFollowTheContext pins the ComponentRegistry's
// value accessors: each reads the by-type context under its own type and
// answers nil when the assembly carries none, the contract the module
// Registry's own accessors state, while EventBus stays derived from the
// Events seat's bus lookup.
func TestRegistrarValueAccessorsFollowTheContext(t *testing.T) {
	t.Parallel()

	empty := NewComponentRegistry()
	if empty.EventBus() != nil || empty.KVStore() != nil || empty.Mailer() != nil || empty.ObjectStore() != nil || empty.Locales() != nil {
		t.Fatal("a registry with an empty by-type context answered a non-nil value")
	}

	reg := NewComponentRegistry()
	bus := NewMemoryEventBus()
	kv := NewMemoryKVStore()
	mailer := NewConsoleMailer()
	reg.Put(bus)
	reg.Put(kv)
	reg.Put(mailer)

	if reg.EventBus() != EventBus(bus) {
		t.Error("EventBus() is not the put bus")
	}
	if reg.KVStore() != KVStore(kv) {
		t.Error("KVStore() is not the put store")
	}
	if reg.Mailer() != Mailer(mailer) {
		t.Error("Mailer() is not the put mailer")
	}
}

// TestRegistrarAccessorsShareTheSeatGate pins that the accessors reach the
// seats behind the one gate: outside the Init stage a write through an
// accessor is refused with the seat's own refusal, text for text, and a void
// write panics with the seat's own panic -- the accessor adds a shape, never
// a way around the gate.
func TestRegistrarAccessorsShareTheSeatGate(t *testing.T) {
	t.Parallel()

	reg := NewComponentRegistry()
	item := ConfigItem{Key: "registrar.item", Type: "string", Description: "d"}

	viaField := reg.Config.Add(item)
	viaAccessor := reg.ConfigSeat().Add(item)
	if viaField == nil || viaAccessor == nil {
		t.Fatalf("writes outside Init = (%v, %v), want two refusals", viaField, viaAccessor)
	}
	if !errors.Is(viaAccessor, ErrStageViolation) {
		t.Errorf("accessor refusal = %v, want ErrStageViolation", viaAccessor)
	}
	if viaField.Error() != viaAccessor.Error() {
		t.Errorf("refusals differ:\n field: %v\naccessor: %v", viaField, viaAccessor)
	}

	panicked := func(fn func()) (recovered any) {
		defer func() { recovered = recover() }()
		fn()
		return nil
	}
	viaFieldPanic := fmt.Sprint(panicked(func() { reg.Routes.Mount("/gate", http.NotFoundHandler()) }))
	viaAccessorPanic := fmt.Sprint(panicked(func() { reg.RoutesSeat().Mount("/gate", http.NotFoundHandler()) }))
	if viaAccessorPanic == "nil" || viaAccessorPanic != viaFieldPanic {
		t.Errorf("void-write panics differ:\n field: %v\naccessor: %v", viaFieldPanic, viaAccessorPanic)
	}
}

// TestRegistrarAccessorsWriteTheSameDeclarationsDuringInit pins the other
// half of the gate identity: inside the Init stage, a declaration written
// through an accessor lands in the same registrar a field write would reach,
// and reads see both identically.
func TestRegistrarAccessorsWriteTheSameDeclarationsDuringInit(t *testing.T) {
	t.Parallel()

	content := recordingComponent(&stageLog{}, "regcontent", "", &compTokenA{}, func(c *Component) {
		c.Init = func(_ context.Context, reg *ComponentRegistry, _ any) error {
			if reg.ConfigSeat() != reg.Config {
				return errors.New("ConfigSeat() is not the held registrar")
			}
			if err := reg.ConfigSeat().Add(ConfigItem{Key: "regcontent.item", Type: "string", Description: "d"}); err != nil {
				return err
			}
			if err := reg.JobsSeat().Handle("regcontent.job", func() {}); err != nil {
				return err
			}
			reg.RoutesSeat().Mount("/regcontent", http.NotFoundHandler())
			if err := reg.SchedulesSeat().Add(PeriodicTask{
				Type:      "regcontent.task",
				Every:     time.Minute,
				Scope:     PeriodicScopePerTenant,
				KeyPrefix: "regcontent:",
			}); err != nil {
				return err
			}
			return reg.RetentionSeat().Add(RetentionParticipant{
				Name:  "regcontent.retention",
				Sweep: func(context.Context, TenantID, time.Time) (int, error) { return 0, nil },
				Erase: func(context.Context, SubjectRef) (int, error) { return 0, nil },
			})
		}
	})

	reg := newTestRegistry(t, content)
	reg.Put(testComposition(configEntry{key: "regcontent", value: nil}))
	if err := runStages(context.Background(), reg); err != nil {
		t.Fatalf("stages = %v", err)
	}

	if got := reg.Config.Items(); len(got) != 1 || got[0].Key != "regcontent.item" {
		t.Errorf("Config.Items() = %v, want the accessor's declaration", got)
	}
	if got := reg.Jobs.Handlers(); len(got) != 1 {
		t.Errorf("Jobs.Handlers() = %v, want the accessor's declaration", got)
	}
	if got := reg.Routes.Routes(); len(got) != 1 || got[0].Path != "/regcontent" {
		t.Errorf("Routes.Routes() = %v, want the accessor's declaration", got)
	}
	if got := reg.Schedules.Declarations(); len(got) != 1 {
		t.Errorf("Schedules.Declarations() = %v, want the accessor's declaration", got)
	}
	if got := reg.Retention.Participants(); len(got) != 1 {
		t.Errorf("Retention.Participants() = %v, want the accessor's declaration", got)
	}
}

// TestRegistrarInterfaceIsSatisfiedByBothShapes pins the compile-time
// assertions registrar.go carries in test form: a declaration body written
// against the interface takes either registry, so the value handed to it
// decides which seats the declaration reaches and nothing else.
func TestRegistrarInterfaceIsSatisfiedByBothShapes(t *testing.T) {
	t.Parallel()

	declare := func(reg Registrar) error {
		if err := reg.FeaturesSeat().Add(FeatureFlag{Key: "registrar.flag"}); err != nil {
			return err
		}
		return nil
	}

	moduleReg := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())
	if err := declare(moduleReg); err != nil {
		t.Fatalf("declare(module Registry) = %v", err)
	}
	if flags := moduleReg.Features.Flags(); len(flags) != 1 {
		t.Errorf("module Registry flags = %v, want the declaration", flags)
	}

	componentReg := NewComponentRegistry()
	if err := declare(componentReg); !errors.Is(err, ErrStageViolation) {
		t.Fatalf("declare(ComponentRegistry) outside Init = %v, want ErrStageViolation", err)
	}
}
