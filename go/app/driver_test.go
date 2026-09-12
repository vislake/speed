package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// errStageRefused is the failure a Start callback returns in the rollback
// test.
var errStageRefused = errors.New("test: the stage refused")

// stageRecorderComponent is a component that records every callback it
// receives, so a test can pin the driver's stage order and the rollback.
func stageRecorderComponent(name string, log *[]string) pkgcore.Component {
	record := func(stageName string) { *log = append(*log, name+":"+stageName) }
	return pkgcore.Component{
		Name: name,
		Prepare: func(context.Context, *pkgcore.ComponentRegistry) error {
			record("prepare")
			return nil
		},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			record("new")
			return &testMarker{name: name}, nil
		},
		Verify: func(context.Context, *pkgcore.ComponentRegistry, any) error {
			record("verify")
			return nil
		},
		Init: func(context.Context, *pkgcore.ComponentRegistry, any) error {
			record("init")
			return nil
		},
		Start: func(context.Context, *pkgcore.ComponentRegistry, any) error {
			record("start")
			return nil
		},
		Stop: func(context.Context, *pkgcore.ComponentRegistry, any) error {
			record("stop")
			return nil
		},
		Close: func(context.Context, *pkgcore.ComponentRegistry, any) error {
			record("close")
			return nil
		},
	}
}

// driverLoadSpec returns a load spec over the test fixtures' host target: the
// composition the tests Put selects the components they registered, and the
// loader's options carry the fixtures' declared defaults table.
func driverLoadSpec(t *testing.T, host *testHostConfig) LoadSpec {
	t.Helper()
	*host = testHostConfig{}
	return LoadSpec{
		Host:    host,
		Options: testConfigOptions(),
		Args:    []string{},
	}
}

// TestAssemble_DrivesTheSevenStagesInDependencyOrder pins the driver's shape:
// the loader publishes before anything runs, and the stages run in order,
// per component in dependency order, closing in the reverse.
func TestAssemble_DrivesTheSevenStagesInDependencyOrder(t *testing.T) {
	var host testHostConfig
	spec := driverLoadSpec(t, &host)
	var log []string

	reg := pkgcore.NewComponentRegistry()
	for _, c := range []pkgcore.Component{
		stageRecorderComponent("first", &log),
		stageRecorderComponent("second", &log),
	} {
		if err := reg.Register(c); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	reg.Put(CompositionOverrides{Config: pkgcore.ComponentConfig{}.With("components",
		pkgcore.ComponentConfig{}.With("first", nil).With("second", nil).With("observability", false))})

	if err := Assemble(context.Background(), reg, spec); err != nil {
		t.Fatalf("Assemble() error = %v", err)
	}
	want := []string{
		"first:prepare", "second:prepare",
		"first:new", "second:new",
		"first:verify", "second:verify",
		"first:init", "second:init",
		"first:start", "second:start",
	}
	if !slices.Equal(log, want) {
		t.Fatalf("stage log = %v, want %v", log, want)
	}

	if err := Shutdown(context.Background(), reg); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	want = append(want,
		"second:stop", "first:stop",
		"second:close", "first:close",
	)
	if !slices.Equal(log, want) {
		t.Fatalf("stage log after shutdown = %v, want the reverse-order stop and close appended", log)
	}
}

// TestAssemble_FailuresRollBackThroughClose pins the failure semantics the
// driver inherits from the registry: a failure from Construct on closes every
// constructed component in reverse order, exactly once, and reports the
// rolled-back set.
func TestAssemble_FailuresRollBackThroughClose(t *testing.T) {
	var host testHostConfig
	spec := driverLoadSpec(t, &host)
	var log []string

	refusing := stageRecorderComponent("refusing", &log)
	refusing.Start = func(context.Context, *pkgcore.ComponentRegistry, any) error {
		log = append(log, "refusing:start")
		return errStageRefused
	}
	healthy := stageRecorderComponent("healthy", &log)

	reg := pkgcore.NewComponentRegistry()
	for _, c := range []pkgcore.Component{healthy, refusing} {
		if err := reg.Register(c); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	reg.Put(CompositionOverrides{Config: pkgcore.ComponentConfig{}.With("components",
		pkgcore.ComponentConfig{}.With("healthy", nil).With("refusing", nil).With("observability", false))})

	err := Assemble(context.Background(), reg, spec)
	if err == nil {
		t.Fatal("Assemble() with a failing Start error = nil, want the refusal")
	}
	if !strings.Contains(err.Error(), "rolled back: refusing, healthy") {
		t.Fatalf("Assemble() error = %v, want it to name the rolled-back set in reverse order", err)
	}
	// The rollback closed every constructed component exactly once, and a
	// later Close reports the same cached result without repeating it.
	if closeErr := reg.Close(context.Background()); closeErr != nil {
		t.Fatalf("Close() after the rollback error = %v, want the cached clean rollback", closeErr)
	}
	if countOf(log, "healthy:close") != 1 || countOf(log, "refusing:close") != 1 {
		t.Fatalf("close log = %v, want exactly one close per constructed component", log)
	}
}

// TestAssemble_PrepareFailureConstructsNothing pins the first-stage
// semantics: a Prepare failure leaves the registry exactly as it was, and a
// following Close is a no-op.
func TestAssemble_PrepareFailureConstructsNothing(t *testing.T) {
	var host testHostConfig
	spec := driverLoadSpec(t, &host)

	reg := pkgcore.NewComponentRegistry()
	reg.Put(CompositionOverrides{Config: pkgcore.ComponentConfig{}.With("components",
		pkgcore.ComponentConfig{}.With("observability", false).With("not-registered", nil))})

	err := Assemble(context.Background(), reg, spec)
	if err == nil || !strings.Contains(err.Error(), "not-registered") {
		t.Fatalf("Assemble() with an unregistered selection error = %v, want the unknown-component refusal", err)
	}
	if err := reg.Close(context.Background()); err != nil {
		t.Fatalf("Close() after a failed Prepare error = %v, want nil (nothing was constructed)", err)
	}
}

// TestRunAssembly_NilServeWaitsForCancellationAndShutsDown pins the nil-callback
// shape end to end: with no host serve step the engine waits the context out
// itself, and a caller-cancelled context returns nil once the two-phase
// shutdown ran -- the behavior RunAssembly had before the serve callback
// existed.
func TestRunAssembly_NilServeWaitsForCancellationAndShutsDown(t *testing.T) {
	var host testHostConfig
	spec := driverLoadSpec(t, &host)
	spec.Overrides = &CompositionOverrides{Config: pkgcore.ComponentConfig{}.
		With("components", pkgcore.ComponentConfig{}.With("waiter", nil).With("observability", false))}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	waiter := stageRecorderComponent("waiter", new([]string))
	waiter.Start = func(context.Context, *pkgcore.ComponentRegistry, any) error {
		close(started)
		return nil
	}
	runErr := make(chan error, 1)
	go func() { runErr <- RunAssembly(ctx, spec, nil, waiter) }()

	// Wait for the assembly to be up, then cancel: the cancellation is the
	// whole lifecycle here, nothing listens.
	select {
	case <-started:
	case <-time.After(ShutdownTimeout):
		t.Fatal("RunAssembly never reached the Start stage")
	}
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("RunAssembly() error = %v, want nil after a clean shutdown", err)
		}
	case <-time.After(ShutdownTimeout + 10*time.Second):
		t.Fatal("RunAssembly did not return after its context was cancelled")
	}
}

// TestRunAssembly_ServeCallbackRunsInTheServeBeat pins the serve callback's
// position: it is called exactly once, after the assembly is up (every Start
// has run) and before the close's first beat, and it is handed the live
// registry -- the component's product is readable from the registry the
// callback receives.
func TestRunAssembly_ServeCallbackRunsInTheServeBeat(t *testing.T) {
	var host testHostConfig
	spec := driverLoadSpec(t, &host)
	spec.Overrides = &CompositionOverrides{Config: pkgcore.ComponentConfig{}.
		With("components", pkgcore.ComponentConfig{}.With("served", nil).With("observability", false))}

	var log []string
	served := stageRecorderComponent("served", &log)
	serveCalls := 0
	serve := func(_ context.Context, reg *pkgcore.ComponentRegistry) error {
		serveCalls++
		product, err := pkgcore.Get[*testMarker](reg)
		if err != nil {
			return err
		}
		log = append(log, "serve:"+product.name)
		return nil
	}

	// A context that never fires: the serve step's return is what ends the
	// serve phase, so RunAssembly must come back on that alone.
	if err := RunAssembly(context.Background(), spec, serve, served); err != nil {
		t.Fatalf("RunAssembly() error = %v", err)
	}
	want := []string{
		"served:prepare", "served:new", "served:verify", "served:init", "served:start",
		"serve:served",
		"served:stop", "served:close",
	}
	if !slices.Equal(log, want) {
		t.Fatalf("serve log = %v, want the callback between every Start and the first Stop: %v", log, want)
	}
	if serveCalls != 1 {
		t.Fatalf("serve calls = %d, want exactly one", serveCalls)
	}
}

// TestRunAssembly_ServeFailureStillClosesAndJoins pins the serve failure
// semantics: a failing serve does not skip the two-beat close -- every
// constructed component still stops and closes, exactly once -- and both
// failures come back joined, so neither the serve error nor a close error is
// lost on the caller's exit path.
func TestRunAssembly_ServeFailureStillClosesAndJoins(t *testing.T) {
	errCloseRefused := errors.New("test: the close refused")
	errServeRefused := errors.New("test: the serve refused")

	var host testHostConfig
	spec := driverLoadSpec(t, &host)
	spec.Overrides = &CompositionOverrides{Config: pkgcore.ComponentConfig{}.
		With("components", pkgcore.ComponentConfig{}.With("closer", nil).With("observability", false))}

	var log []string
	closer := stageRecorderComponent("closer", &log)
	closer.Close = func(context.Context, *pkgcore.ComponentRegistry, any) error {
		log = append(log, "closer:close")
		return errCloseRefused
	}

	err := RunAssembly(context.Background(), spec, func(context.Context, *pkgcore.ComponentRegistry) error {
		return errServeRefused
	}, closer)
	if !errors.Is(err, errServeRefused) {
		t.Fatalf("RunAssembly() error = %v, want it to carry the serve refusal", err)
	}
	if !errors.Is(err, errCloseRefused) {
		t.Fatalf("RunAssembly() error = %v, want it to carry the close refusal the serve failure must not swallow", err)
	}
	if !slices.Equal(log, []string{"closer:prepare", "closer:new", "closer:verify", "closer:init", "closer:start", "closer:stop", "closer:close"}) {
		t.Fatalf("close log = %v, want the full two-beat close after a failed serve", log)
	}
}

// TestRunAssembly_ServeStepNeverRunsWhenTheAssemblyFails pins the pairing the
// other way: a serve callback sits after Assemble, so a refused assembly
// never reaches it.
func TestRunAssembly_ServeStepNeverRunsWhenTheAssemblyFails(t *testing.T) {
	serveCalled := false
	err := RunAssembly(context.Background(), LoadSpec{Options: testConfigOptions(), Args: []string{}},
		func(context.Context, *pkgcore.ComponentRegistry) error {
			serveCalled = true
			return nil
		})
	if err == nil || !strings.Contains(err.Error(), "LoadSpec.Host") {
		t.Fatalf("RunAssembly() with no host target error = %v, want the loader's refusal", err)
	}
	if serveCalled {
		t.Fatal("the serve callback ran although the assembly never came up")
	}
}

// TestAssemble_PropagatesAFailureFromEveryStage pins the driver's failure
// propagation for the stages between Construct and Init: each refusal comes
// back as-is, after the registry's own rollback ran.
func TestAssemble_PropagatesAFailureFromEveryStage(t *testing.T) {
	for _, stage := range []string{"construct", "verify", "init"} {
		t.Run(stage, func(t *testing.T) {
			var host testHostConfig
			spec := driverLoadSpec(t, &host)
			var log []string
			failing := stageRecorderComponent("failing", &log)
			switch stage {
			case "construct":
				failing.New = func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
					return nil, errStageRefused
				}
			case "verify":
				failing.Verify = func(context.Context, *pkgcore.ComponentRegistry, any) error { return errStageRefused }
			case "init":
				failing.Init = func(context.Context, *pkgcore.ComponentRegistry, any) error { return errStageRefused }
			}

			reg := pkgcore.NewComponentRegistry()
			if err := reg.Register(failing); err != nil {
				t.Fatalf("register: %v", err)
			}
			reg.Put(CompositionOverrides{Config: pkgcore.ComponentConfig{}.With("components",
				pkgcore.ComponentConfig{}.With("failing", nil).With("observability", false))})

			err := Assemble(context.Background(), reg, spec)
			if err == nil || !strings.Contains(err.Error(), "the stage refused") {
				t.Fatalf("Assemble() with a failing %s error = %v, want the stage's own refusal", stage, err)
			}
		})
	}
}

// TestAssemble_PropagatesALoadRefusal pins the order the entry enforces: a
// spec the loader refuses fails the assembly before any stage runs.
func TestAssemble_PropagatesALoadRefusal(t *testing.T) {
	err := Assemble(context.Background(), pkgcore.NewComponentRegistry(), LoadSpec{Options: testConfigOptions(), Args: []string{}})
	if err == nil || !strings.Contains(err.Error(), "LoadSpec.Host") {
		t.Fatalf("Assemble() with no host target error = %v, want the loader's refusal", err)
	}
}

// TestRunAssembly_PropagatesItsEntryRefusals pins both failures the sugar
// hands back before it ever waits: a malformed extra component at
// registration, and a spec the assembly refuses.
func TestRunAssembly_PropagatesItsEntryRefusals(t *testing.T) {
	var host testHostConfig
	if err := RunAssembly(context.Background(), driverLoadSpec(t, &host), nil, pkgcore.Component{Name: ""}); err == nil {
		t.Fatal("RunAssembly() with a nameless extra component error = nil, want the registration refusal")
	}

	err := RunAssembly(context.Background(), LoadSpec{Options: testConfigOptions(), Args: []string{}}, nil)
	if err == nil || !strings.Contains(err.Error(), "LoadSpec.Host") {
		t.Fatalf("RunAssembly() with no host target error = %v, want the assembly's refusal", err)
	}
}

// countOf counts occurrences in a log slice.
func countOf(log []string, entry string) int {
	count := 0
	for _, got := range log {
		if got == entry {
			count++
		}
	}
	return count
}
