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
			return &transitionMarker{name: name}, nil
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

// TestRunAssembly_WaitsForCancellationAndShutsDown pins the sugar end to end:
// a caller-cancelled context returns nil once the two-phase shutdown ran.
func TestRunAssembly_WaitsForCancellationAndShutsDown(t *testing.T) {
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
	go func() { runErr <- RunAssembly(ctx, spec, waiter) }()

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
