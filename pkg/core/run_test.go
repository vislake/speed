package core_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/pkg/core"
)

// Reader stands in for the capability the config module delivers.
type Reader interface{ Value(key string) string }

type reader struct{}

func (reader) Value(key string) string { return "value:" + key }

// probe records the callbacks the driver made, in order.
type probe struct {
	mu     sync.Mutex
	events []string
}

func (p *probe) record(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, fmt.Sprintf(format, args...))
}

func (p *probe) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.events)
}

func (p *probe) has(event string) bool {
	return slices.Contains(p.snapshot(), event)
}

func (p *probe) waitFor(t *testing.T, event string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !p.has(event) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q; recorded %v", event, p.snapshot())
		}
		time.Sleep(time.Millisecond)
	}
}

func (p *probe) indexOf(t *testing.T, event string) int {
	t.Helper()
	i := slices.Index(p.snapshot(), event)
	if i < 0 {
		t.Fatalf("event %q never happened; recorded %v", event, p.snapshot())
	}
	return i
}

// module builds a module that records every stage it takes part in.
func (p *probe) module(name string) core.Module {
	return core.Module{
		Name: name,
		New: func(context.Context, *core.Registry) (any, error) {
			p.record("new %s", name)
			return &box{id: name}, nil
		},
		Migrate: func(context.Context, *core.Registry, any) error {
			p.record("migrate %s", name)
			return nil
		},
		Init: func(context.Context, *core.Registry, any) error {
			p.record("init %s", name)
			return nil
		},
		Start: func(context.Context, *core.Registry, any) error {
			p.record("start %s", name)
			return nil
		},
		Serve: func(context.Context, *core.Registry, any) error {
			p.record("serve %s", name)
			return nil
		},
		Stop: func(context.Context, *core.Registry, any) error {
			p.record("stop %s", name)
			return nil
		},
		Close: func(context.Context, *core.Registry, any) error {
			p.record("close %s", name)
			return nil
		},
	}
}

// configModule builds a module under the one name the registry recognises.
func (p *probe) configModule() core.Module {
	m := p.module("config")
	m.Provides = []core.Provision{{Token: (*Reader)(nil), Exclusive: true}}
	m.New = func(context.Context, *core.Registry) (any, error) {
		p.record("new config")
		return reader{}, nil
	}
	return m
}

func register(reg *core.Registry, mods ...core.Module) {
	for _, m := range mods {
		reg.Register(m)
	}
}

// runUntilServed drives a registry to the point where every named event has
// happened, then cancels and returns what Run returned.
func runUntilServed(t *testing.T, reg *core.Registry, p *probe, events ...string) error {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- reg.Run(ctx) }()

	for _, event := range events {
		p.waitFor(t, event)
	}
	select {
	case err := <-done:
		t.Fatalf("Run returned %v before the context was cancelled", err)
	default:
	}

	cancel()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
		return nil
	}
}

// runToFailure drives a registry that is expected to fail during startup.
func runToFailure(t *testing.T, reg *core.Registry) error {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- reg.Run(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run succeeded where a startup failure was expected")
		}
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Run blocked where a startup failure was expected")
		return nil
	}
}

// captureStderr collects what core writes to os.Stderr while fn runs.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating a pipe failed: %v", err)
	}
	original := os.Stderr
	os.Stderr = w
	collected := make(chan string, 1)
	go func() {
		text, _ := io.ReadAll(r)
		collected <- string(text)
	}()

	fn()

	os.Stderr = original
	w.Close()
	text := <-collected
	r.Close()
	return text
}

func TestPhaseOrderNewMigrateInitStartServe(t *testing.T) {
	p := &probe{}
	reg := core.New()
	register(reg, p.module("only"))

	if err := runUntilServed(t, reg, p, "serve only"); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	want := []string{"new only", "migrate only", "init only", "start only", "serve only", "stop only", "close only"}
	if got := p.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("the driver ran %v, want %v", got, want)
	}
}

func TestMigrateRunsAfterEveryNewAndBeforeAnyInit(t *testing.T) {
	p := &probe{}
	reg := core.New()
	provider := p.module("aaa-provider")
	provider.Provides = []core.Provision{{Token: (*Cache)(nil)}}
	consumer := p.module("bbb-consumer")
	consumer.Requires = []core.Requirement{{Token: (*Cache)(nil)}}
	register(reg, provider, consumer)

	if err := runUntilServed(t, reg, p, "serve bbb-consumer"); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	lastNew := p.indexOf(t, "new bbb-consumer")
	firstMigrate := p.indexOf(t, "migrate aaa-provider")
	lastMigrate := p.indexOf(t, "migrate bbb-consumer")
	firstInit := p.indexOf(t, "init aaa-provider")
	if firstMigrate < lastNew {
		t.Errorf("Migrate started before every New finished: %v", p.snapshot())
	}
	if firstInit < lastMigrate {
		t.Errorf("Init started before every Migrate finished: %v", p.snapshot())
	}
}

func TestMigrateFollowsDependencyOrder(t *testing.T) {
	p := &probe{}
	reg := core.New()
	provider := p.module("zzz-provider")
	provider.Provides = []core.Provision{{Token: (*Cache)(nil)}}
	consumer := p.module("aaa-consumer")
	consumer.Requires = []core.Requirement{{Token: (*Cache)(nil)}}
	register(reg, consumer, provider)

	if err := runUntilServed(t, reg, p, "serve aaa-consumer"); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	if p.indexOf(t, "migrate zzz-provider") > p.indexOf(t, "migrate aaa-consumer") {
		t.Fatalf("Migrate ignored the dependency order: %v", p.snapshot())
	}
}

// TestServeRunsAfterEveryStart pins what dependency order cannot express: an
// entry point needs "after everyone", not "after my dependencies".
func TestServeRunsAfterEveryStart(t *testing.T) {
	p := &probe{}
	reg := core.New()
	register(reg, p.module("one"), p.module("two"), p.module("three"))

	if err := runUntilServed(t, reg, p, "serve one", "serve three", "serve two"); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	events := p.snapshot()
	lastStart := 0
	firstServe := len(events)
	for i, event := range events {
		if strings.HasPrefix(event, "start ") && i > lastStart {
			lastStart = i
		}
		if strings.HasPrefix(event, "serve ") && i < firstServe {
			firstServe = i
		}
	}
	if firstServe < lastStart {
		t.Fatalf("a Serve ran before the last Start: %v", events)
	}
}

// TestRunBlocksOnlyAfterEveryServeReturned pins that Serve must not block: a
// Serve that never returned would starve every entry point after it.
func TestRunBlocksOnlyAfterEveryServeReturned(t *testing.T) {
	p := &probe{}
	reg := core.New()
	register(reg, p.module("one"), p.module("two"), p.module("three"))

	if err := runUntilServed(t, reg, p, "serve one", "serve two", "serve three"); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestCancelReturnsNil(t *testing.T) {
	p := &probe{}
	reg := core.New()
	register(reg, p.module("only"))

	if err := runUntilServed(t, reg, p, "serve only"); err != nil {
		t.Fatalf("cancellation returned %v, want nil: it is the normal way to stop", err)
	}
}

func TestCloseFailureReturned(t *testing.T) {
	p := &probe{}
	reg := core.New()
	failure := errors.New("release refused")
	m := p.module("only")
	m.Close = func(context.Context, *core.Registry, any) error {
		p.record("close only")
		return failure
	}
	register(reg, m)

	err := runUntilServed(t, reg, p, "serve only")
	if !errors.Is(err, failure) {
		t.Fatalf("Run returned %v, want the cleanup failure", err)
	}
}

// TestRollbackCoversAllConstructedInstances pins that rollback reaches the
// instances constructed earlier that never got as far as the failing stage.
func TestRollbackCoversAllConstructedInstances(t *testing.T) {
	p := &probe{}
	reg := core.New()
	m1 := p.module("m1")
	m1.Provides = []core.Provision{{Token: (*Cache)(nil)}}
	m2 := p.module("m2")
	m2.Requires = []core.Requirement{{Token: (*Cache)(nil)}}
	m2.Provides = []core.Provision{{Token: (*Store)(nil)}}
	m3 := p.module("m3")
	m3.Requires = []core.Requirement{{Token: (*Store)(nil)}}
	failure := errors.New("wiring refused")
	m3.Init = func(context.Context, *core.Registry, any) error {
		p.record("init m3")
		return failure
	}
	register(reg, m1, m2, m3)

	err := runToFailure(t, reg)
	if !errors.Is(err, failure) {
		t.Fatalf("Run returned %v, want the Init failure", err)
	}
	events := p.snapshot()
	for _, want := range []string{"stop m1", "close m1", "stop m2", "close m2", "stop m3", "close m3"} {
		if !slices.Contains(events, want) {
			t.Errorf("rollback skipped %q; recorded %v", want, events)
		}
	}
	if slices.Contains(events, "init m1") && !slices.Contains(events, "start m1") {
		// m1 was initialised but never started, and still gets both
		// callbacks: that is the contract Stop and Close carry.
		t.Log("m1 rolled back from an initialised but unstarted state")
	}
}

func TestDependencyFailureClosesConfigModule(t *testing.T) {
	p := &probe{}
	reg := core.New()
	consumer := p.module("consumer")
	consumer.Requires = []core.Requirement{{Token: (*Cache)(nil)}}
	register(reg, p.configModule(), consumer)

	err := runToFailure(t, reg)
	if !errors.Is(err, core.ErrMissingProvider) {
		t.Fatalf("Run returned %v, want ErrMissingProvider", err)
	}
	if !p.has("close config") {
		t.Fatalf("the config module was not closed; recorded %v", p.snapshot())
	}
	if p.has("new consumer") {
		t.Fatalf("a business module was constructed after dependency resolution failed: %v", p.snapshot())
	}
}

func TestPrepareErrorAbortsAndClosesConfig(t *testing.T) {
	p := &probe{}
	reg := core.New()
	failure := errors.New("cannot read my configuration")
	m := p.module("stubborn")
	m.Prepare = func(context.Context, *core.Registry) (core.Enablement, error) {
		return core.Enablement{}, failure
	}
	register(reg, p.configModule(), m)

	err := runToFailure(t, reg)
	if !errors.Is(err, failure) {
		t.Fatalf("Run returned %v, want the Prepare failure", err)
	}
	if !p.has("close config") {
		t.Fatalf("the config module was not closed; recorded %v", p.snapshot())
	}
}

func TestUndeliveredCapability(t *testing.T) {
	p := &probe{}
	reg := core.New()
	m := p.module("liar")
	m.Provides = []core.Provision{{Token: (*Reader)(nil)}}
	register(reg, m)

	err := runToFailure(t, reg)
	if !errors.Is(err, core.ErrUndeliveredCapability) {
		t.Fatalf("Run returned %v, want ErrUndeliveredCapability", err)
	}
	for _, want := range []string{"liar", "Reader"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if !p.has("close liar") {
		t.Fatalf("the constructed instance was not rolled back; recorded %v", p.snapshot())
	}
}

// TestConfigConstructedBeforeOtherPrepares pins the bootstrap: every module
// reads its own configuration while stating whether it is enabled.
func TestConfigConstructedBeforeOtherPrepares(t *testing.T) {
	p := &probe{}
	reg := core.New()
	m := p.module("dependent")
	m.Prepare = func(_ context.Context, r *core.Registry) (core.Enablement, error) {
		cfg, err := core.Resolve[Reader](r)
		if err != nil {
			p.record("prepare dependent failed: %v", err)
			return core.Enablement{}, nil
		}
		p.record("prepare dependent read %s", cfg.Value("key"))
		return core.Enablement{}, nil
	}
	register(reg, p.configModule(), m)

	if err := runUntilServed(t, reg, p, "serve dependent"); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	if !p.has("prepare dependent read value:key") {
		t.Fatalf("the config instance was not available in Prepare; recorded %v", p.snapshot())
	}
	if p.indexOf(t, "new config") > p.indexOf(t, "prepare dependent read value:key") {
		t.Fatalf("the config module was constructed after another module's Prepare: %v", p.snapshot())
	}
}

func TestConfigSkippedInNewPhase(t *testing.T) {
	p := &probe{}
	reg := core.New()
	register(reg, p.configModule(), p.module("other"))

	if err := runUntilServed(t, reg, p, "serve other"); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	count := 0
	for _, event := range p.snapshot() {
		if event == "new config" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("the config module was constructed %d times, want once: %v", count, p.snapshot())
	}
}

// TestConfigClosesLast pins that configuration stays readable all the way
// through the shutdown.
func TestConfigClosesLast(t *testing.T) {
	p := &probe{}
	reg := core.New()
	register(reg, p.configModule(), p.module("aaa"), p.module("zzz"))

	if err := runUntilServed(t, reg, p, "serve aaa", "serve zzz"); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	events := p.snapshot()
	if events[len(events)-1] != "close config" {
		t.Fatalf("the last event is %q, want close config: %v", events[len(events)-1], events)
	}
}

// TestNoConfigModuleStillRuns pins the config module as the answer to the
// startup cycle rather than a precondition of assembly.
func TestNoConfigModuleStillRuns(t *testing.T) {
	p := &probe{}
	reg := core.New()
	register(reg, p.module("alone"))

	if err := runUntilServed(t, reg, p, "serve alone"); err != nil {
		t.Fatalf("Run returned %v, want a registry without a config module to run", err)
	}
}

func TestNoConfigModuleYieldsMissingProviderOnReader(t *testing.T) {
	p := &probe{}
	reg := core.New()
	m := p.module("needs-config")
	m.Prepare = func(_ context.Context, r *core.Registry) (core.Enablement, error) {
		_, err := core.Resolve[Reader](r)
		if !errors.Is(err, core.ErrMissingProvider) {
			t.Errorf("taking up the reader returned %v, want ErrMissingProvider", err)
		}
		p.record("prepare saw: %v", err)
		return core.Enablement{}, nil
	}
	register(reg, m)

	if err := runUntilServed(t, reg, p, "serve needs-config"); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

// TestModuleWithoutNewGetsNilInstance pins the contract for a module that only
// declares resources: every later stage receives nil.
func TestModuleWithoutNewGetsNilInstance(t *testing.T) {
	p := &probe{}
	reg := core.New()
	m := core.Module{
		Name:      "resources-only",
		Resources: []any{"a declaration core never interprets"},
		Init: func(_ context.Context, _ *core.Registry, instance any) error {
			if instance != nil {
				t.Errorf("Init received %v, want nil for a module without New", instance)
			}
			p.record("init resources-only")
			return nil
		},
		Start: func(_ context.Context, _ *core.Registry, instance any) error {
			if instance != nil {
				t.Errorf("Start received %v, want nil for a module without New", instance)
			}
			p.record("start resources-only")
			return nil
		},
		Serve: func(context.Context, *core.Registry, any) error {
			p.record("serve resources-only")
			return nil
		},
	}
	register(reg, m)

	if err := runUntilServed(t, reg, p, "serve resources-only"); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

// TestPrepareSeesOnlyConfigInstance pins that business instances do not exist
// yet while stances are being taken.
func TestPrepareSeesOnlyConfigInstance(t *testing.T) {
	p := &probe{}
	reg := core.New()
	provider := p.module("provider")
	provider.Provides = []core.Provision{{Token: (*Cache)(nil)}}
	consumer := p.module("consumer")
	consumer.Prepare = func(_ context.Context, r *core.Registry) (core.Enablement, error) {
		_, err := core.Resolve[Cache](r)
		if !errors.Is(err, core.ErrMissingProvider) {
			t.Errorf("taking up a business capability in Prepare returned %v, want ErrMissingProvider", err)
		}
		p.record("prepare consumer saw missing provider")
		return core.Enablement{}, nil
	}
	register(reg, p.configModule(), provider, consumer)

	if err := runUntilServed(t, reg, p, "serve consumer"); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	if !p.has("prepare consumer saw missing provider") {
		t.Fatalf("the consumer's Prepare did not run; recorded %v", p.snapshot())
	}
}

func TestSecondRunPanics(t *testing.T) {
	p := &probe{}
	reg := core.New()
	register(reg, p.module("only"))

	if err := runUntilServed(t, reg, p, "serve only"); err != nil {
		t.Fatalf("the first Run returned %v", err)
	}
	msg := recoverMessage(t, func() {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_ = reg.Run(ctx)
	})
	if !strings.Contains(msg, "more than once") {
		t.Fatalf("panic %q does not explain that Run is single-use", msg)
	}
}

// TestRunEmitsDiagnosticsAfterResolution pins the one observable surface this
// framework has: a module that is not running says so on stderr.
func TestRunEmitsDiagnosticsAfterResolution(t *testing.T) {
	p := &probe{}
	reg := core.New()
	off := p.module("switched-off")
	off.Prepare = func(context.Context, *core.Registry) (core.Enablement, error) {
		return core.Enablement{State: core.StateDisabled, Reason: "no connection address configured"}, nil
	}
	register(reg, p.module("running"), off)

	var runErr error
	text := captureStderr(t, func() {
		runErr = runUntilServed(t, reg, p, "serve running")
	})
	if runErr != nil {
		t.Fatalf("Run returned %v", runErr)
	}
	for _, want := range []string{"core: ", "switched-off", "no connection address configured"} {
		if !strings.Contains(text, want) {
			t.Errorf("stderr %q does not contain %q", text, want)
		}
	}
	if strings.Contains(text, "\"running\"") {
		t.Errorf("stderr %q lists a module that is enabled", text)
	}
	if p.has("new switched-off") {
		t.Errorf("a disabled module was constructed: %v", p.snapshot())
	}
	if got, ok := reg.Enablement("switched-off"); !ok || got.State != core.StateDisabled {
		t.Errorf("Enablement reported %+v/%v for a disabled module", got, ok)
	}
}
