package config

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/vislake/speed/pkg/core"
)

// withArgs replaces the process arguments for one case. The loader reads them
// the way the process was started, so a case that drives a whole Run has to
// say what the command line was.
func withArgs(t *testing.T, args ...string) {
	t.Helper()
	saved := os.Args
	t.Cleanup(func() { os.Args = saved })
	os.Args = args
}

// captureStd collects what fn wrote to the two standard streams. Startup
// diagnostics and the help output go there by fixed choice and a host cannot
// redirect them, so a case that wants to read them redirects the process.
func captureStd(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	savedOut, savedErr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("opening a pipe failed: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("opening a pipe failed: %v", err)
	}
	os.Stdout, os.Stderr = outW, errW

	type collected struct {
		text string
	}
	outDone, errDone := make(chan collected, 1), make(chan collected, 1)
	go func() { text, _ := io.ReadAll(outR); outDone <- collected{string(text)} }()
	go func() { text, _ := io.ReadAll(errR); errDone <- collected{string(text)} }()

	defer func() {
		outW.Close()
		errW.Close()
		os.Stdout, os.Stderr = savedOut, savedErr
	}()
	fn()
	outW.Close()
	errW.Close()
	return (<-outDone).text, (<-errDone).text
}

func TestDescriptorNameAndExclusiveProvision(t *testing.T) {
	m := Module()
	if m.Name != "config" {
		t.Fatalf("the descriptor is named %q, and the registry recognises the bootstrap module "+
			"by the name \"config\" alone", m.Name)
	}
	if len(m.Provides) != 1 {
		t.Fatalf("the descriptor delivers %d capabilities, want the reader alone", len(m.Provides))
	}
	provision := m.Provides[0]
	if got := reflect.TypeOf(provision.Token); got != reflect.TypeFor[*Reader]() {
		t.Fatalf("the descriptor delivers %v, want (*Reader)(nil)", got)
	}
	if !provision.Exclusive {
		t.Fatal("the reader is not delivered exclusively, so a second provider would surface at " +
			"the moment somebody takes it up rather than during resolution")
	}
}

// TestDescriptorStatesNoStance pins that this module takes no stance of its
// own: it is constructed before any stance is taken, and resolution counts it
// as enabled.
func TestDescriptorStatesNoStance(t *testing.T) {
	if Module().Prepare != nil {
		t.Fatal("the descriptor carries a Prepare callback, and this module is already " +
			"constructed by the time that stage runs")
	}
	if Module().New == nil {
		t.Fatal("the descriptor carries no New callback, and construction is where the load happens")
	}
}

// TestSelfRegistration pins that importing the package is all it takes: the
// module registers itself, the way every module reaches the available set.
func TestSelfRegistration(t *testing.T) {
	m, found := core.ProcessRegistry.Lookup("config")
	if !found {
		t.Fatal("importing the package did not put the module in the process registry")
	}
	if m.New == nil {
		t.Fatal("the registered descriptor carries no New callback")
	}
}

// TestModuleExportedForManualRegistration pins the other path: a registry
// built with core.New inherits nothing from init and takes the descriptor by
// hand, which is what a test assembling a few modules does.
func TestModuleExportedForManualRegistration(t *testing.T) {
	reg := core.New()
	if _, found := reg.Lookup("config"); found {
		t.Fatal("a fresh registry already carries the module, and it should inherit nothing")
	}
	reg.Register(Module())
	if _, found := reg.Lookup("config"); !found {
		t.Fatal("registering the exported descriptor by hand did not take")
	}
}

type probeOptions struct {
	Salutation string
}

// TestReaderUsableInPrepare drives a whole lifecycle: the module is
// constructed first, and another module takes the reader up in its own stance
// stage and decodes what it declared.
func TestReaderUsableInPrepare(t *testing.T) {
	withArgs(t, "prog")
	defaults := probeOptions{Salutation: "Hello"}
	reg := core.New()
	reg.Register(Module())

	var decoded probeOptions
	var resolveErr error
	reg.Register(core.Module{
		Name:      "probe",
		Resources: []any{Schema{Namespace: "probe", Mounts: []Mount{{Value: &defaults}}}},
		Prepare: func(_ context.Context, reg *core.Registry) (core.Enablement, error) {
			r, err := core.Resolve[Reader](reg)
			if err != nil {
				resolveErr = err
				return core.Enablement{}, nil
			}
			resolveErr = r.Decode("probe", &decoded)
			return core.Enablement{}, nil
		},
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := reg.Run(ctx); err != nil {
		t.Fatalf("running the assembly returned %v, want nil on a clean cancellation", err)
	}
	if resolveErr != nil {
		t.Fatalf("taking the reader up in Prepare failed: %v", resolveErr)
	}
	if decoded.Salutation != "Hello" {
		t.Fatalf("the probe decoded %#v, want the default it declared", decoded)
	}
}

// TestHelpPropagatesThroughRun pins that asking for help travels out of the
// lifecycle as the sentinel: the host recognises it and exits successfully
// rather than the framework leaving through the door on its own.
func TestHelpPropagatesThroughRun(t *testing.T) {
	withArgs(t, "prog", "--help")
	reg := core.New()
	reg.Register(Module())

	var err error
	stdout, _ := captureStd(t, func() {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err = reg.Run(ctx)
	})
	if !errors.Is(err, ErrHelpRequested) {
		t.Fatalf("running with --help returned %v, want an error carrying ErrHelpRequested", err)
	}
	if !strings.Contains(stdout, "--help") {
		t.Fatalf("the help output did not reach standard output:\n%s", stdout)
	}
}

// TestDisabledByMissingRequiredAppearsInDiagnostics is the whole point of the
// required rule living in Decode: a module with no address configured states
// that it is not enabled, the startup carries on, and the diagnostics are the
// one place that says so.
func TestDisabledByMissingRequiredAppearsInDiagnostics(t *testing.T) {
	withArgs(t, "prog")
	reg := core.New()
	reg.Register(Module())
	reg.Register(core.Module{
		Name: "remote",
		Resources: []any{Schema{
			Namespace: "remote",
			Mounts:    []Mount{{Value: &probeOptions{}}},
			Items:     map[string]Item{"salutation": {Required: true}},
		}},
		Prepare: func(_ context.Context, reg *core.Registry) (core.Enablement, error) {
			r, err := core.Resolve[Reader](reg)
			if err != nil {
				return core.Enablement{}, err
			}
			var opts probeOptions
			if err := r.Decode("remote", &opts); errors.Is(err, ErrMissingRequired) {
				return core.Enablement{State: core.StateDisabled, Reason: "no salutation is configured"}, nil
			} else if err != nil {
				return core.Enablement{}, err
			}
			return core.Enablement{State: core.StateEnabled}, nil
		},
	})

	var err error
	_, stderr := captureStd(t, func() {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err = reg.Run(ctx)
	})
	if err != nil {
		t.Fatalf("a module standing down for want of configuration failed the startup: %v", err)
	}
	if !strings.Contains(stderr, "remote") || !strings.Contains(stderr, "no salutation is configured") {
		t.Fatalf("the diagnostics read %q, and a module that is not running has to appear there "+
			"with the reason it gave", stderr)
	}

	state, known := reg.Enablement("remote")
	if !known || state.State != core.StateDisabled {
		t.Fatalf("the registry reports %v for the module that stood down, want StateDisabled", state)
	}
}
