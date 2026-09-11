package app

// The composition pipeline's own edge and refusal paths: the flag walk's
// skips and refusals, the text spellings the pipeline normalizes, the
// pass-through an unregistered selection takes, and the one-override rule.
// The layering semantics themselves are pinned by loader_test.go's Load
// tests; these are the branches those paths do not reach.

import (
	"context"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// TestFlagComposition_RefusesAFlagNamingAnUnregisteredComponent pins the
// refusal that carries the registered set: a composition flag whose leading
// name matches no registered component fails the load, naming the flag's
// remainder and listing what is registered.
func TestFlagComposition_RefusesAFlagNamingAnUnregisteredComponent(t *testing.T) {
	var host testHostConfig
	reg := loaderTestRegistry(t, probeComponent("probe"))
	err := Load(context.Background(), reg, LoadSpec{
		Host:    &host,
		Options: testConfigOptions(),
		Args:    []string{"--composition.components.ghost.token=1"},
	})
	if err == nil {
		t.Fatal("Load() with a flag naming an unregistered component error = nil, want a refusal")
	}
	for _, fragment := range []string{`"ghost.token"`, "not registered", "probe"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("refusal misses %q: %v", fragment, err)
		}
	}
}

// TestFlagComposition_SkipsPositionalArgumentsAndRefusesAMissingValue pins
// the walk's two non-selection paths: a positional argument is skipped
// without disturbing the selections around it, and a selection flag as the
// last argument has no value to read and fails the load.
func TestFlagComposition_SkipsPositionalArgumentsAndRefusesAMissingValue(t *testing.T) {
	var host testHostConfig
	reg := loaderTestRegistry(t, probeComponent("probe"))
	if err := Load(context.Background(), reg, LoadSpec{
		Host:    &host,
		Options: testConfigOptions(),
		Args:    []string{"positional", "--composition.components.probe.token=from-flag"},
	}); err != nil {
		t.Fatalf("Load() with a positional argument error = %v, want it skipped", err)
	}
	probe := asComponentConfig(mustGet(t, componentBlockOf(t, compositionOf(t, reg)), "probe"))
	if token, _ := probe.Get("token"); token != "from-flag" {
		t.Errorf("probe.token = %v, want the selection after the positional argument to land", token)
	}

	var otherHost testHostConfig
	err := Load(context.Background(), loaderTestRegistry(t, probeComponent("probe")), LoadSpec{
		Host:    &otherHost,
		Options: testConfigOptions(),
		Args:    []string{"--composition.components.probe.token"},
	})
	if err == nil || !strings.Contains(err.Error(), "needs a value") {
		t.Fatalf("Load() with a valueless flag error = %v, want the missing-value refusal", err)
	}
}

// TestLoad_ReadsTheStrictAndTrueSpellingsFromTheirTextSources pins the two
// text spellings the pipeline normalizes: a strict value from a text source
// lands as the boolean, and a selection spelled "true" reads as its boolean
// instead of the string.
func TestLoad_ReadsTheStrictAndTrueSpellingsFromTheirTextSources(t *testing.T) {
	t.Setenv("TEST_COMPOSITION__STRICT", "true")
	t.Setenv("TEST_COMPOSITION__COMPONENTS__PROBE", "true")
	var host testHostConfig
	reg := loaderTestRegistry(t, probeComponent("probe"))
	if err := Load(context.Background(), reg, LoadSpec{
		Host:    &host,
		Options: testConfigOptions(),
		Args:    []string{},
	}); err != nil {
		t.Fatalf("Load() with the text spellings error = %v", err)
	}

	composition := compositionOf(t, reg)
	if strict, _ := composition.Get("strict"); strict != true {
		t.Errorf("strict = %v (%T), want the text source's true", strict, strict)
	}
	if probe, _ := componentBlockOf(t, composition).Get("probe"); probe != true {
		t.Errorf("components.probe = %v (%T), want the text source's true", probe, probe)
	}
}

// TestLoad_LeavesASelectionThatNamesNothingUnchanged pins the pass-through: a
// selection key resolving to no registered component is carried into the
// composition unchanged, so the plan -- which owns unknown-selection
// reporting, with the registered set and a suggestion -- sees exactly what
// the host wrote.
func TestLoad_LeavesASelectionThatNamesNothingUnchanged(t *testing.T) {
	t.Setenv("TEST_COMPOSITION__COMPONENTS__GHOST__TOKEN", "1")
	var host testHostConfig
	reg := loaderTestRegistry(t)
	if err := Load(context.Background(), reg, LoadSpec{
		Host:    &host,
		Options: testConfigOptions(),
		Args:    []string{},
	}); err != nil {
		t.Fatalf("Load() with an unregistered selection error = %v, want it carried to the plan", err)
	}

	ghost := asComponentConfig(mustGet(t, componentBlockOf(t, compositionOf(t, reg)), "ghost"))
	if token, _ := ghost.Get("token"); token != "1" {
		t.Errorf("ghost.token = %v, want the unregistered selection's block passed through", token)
	}
}

// TestLoad_RefusesTwoCodeOverrideLayers pins the one-override rule: an
// assembly has one code-override layer, so a registry carrying two fails the
// load naming the layer rather than silently picking one.
func TestLoad_RefusesTwoCodeOverrideLayers(t *testing.T) {
	var host testHostConfig
	reg := loaderTestRegistry(t)
	reg.Put(CompositionOverrides{Config: pkgcore.ComponentConfig{}.With("components",
		pkgcore.ComponentConfig{}.With("probe", nil))})
	reg.Put(CompositionOverrides{Config: pkgcore.ComponentConfig{}.With("components",
		pkgcore.ComponentConfig{}.With("other", nil))})

	err := Load(context.Background(), reg, LoadSpec{Host: &host, Options: testConfigOptions(), Args: []string{}})
	if err == nil || !strings.Contains(err.Error(), "code-override") {
		t.Fatalf("Load() with two code-override layers error = %v, want one naming the layer", err)
	}
}
