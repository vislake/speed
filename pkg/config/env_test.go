package config

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

type envOptions struct {
	Addr    string
	Retries int
	Hosts   []string
	Colour  bool
	FlagsOn string
}

// envManifest declares one module under the MYAPP prefix, with a pinned name
// that owes nothing to the prefix and one item the environment never gives.
func envManifest(t *testing.T, prefix string) *manifest {
	t.Helper()
	defaults := envOptions{Addr: ":8080", Retries: 3, Hosts: []string{"a"}}
	return collect(t,
		identifying("host", HostIdentity{Prefix: prefix}),
		declaring("server", Schema{
			Namespace: "server",
			Mounts:    []Mount{{Value: &defaults}},
			Items: map[string]Item{
				"colour":   {Origins: OriginEnv, EnvName: "NO_COLOR"},
				"flags-on": {Origins: OriginFlag, FlagName: "flags-on"},
			},
		}),
	)
}

func TestEnvOverridesPrimary(t *testing.T) {
	m := envManifest(t, "MYAPP")
	d := newData(m)
	if err := d.applyPrimary(m, map[string]any{"server": map[string]any{"addr": ":7000"}}); err != nil {
		t.Fatalf("applying the primary source failed: %v", err)
	}
	if _, err := d.applyEnv(m, []string{"MYAPP_SERVER__ADDR=:9090"}); err != nil {
		t.Fatalf("applying the environment failed: %v", err)
	}
	if got := d.values["server.addr"].value; got != ":9090" {
		t.Fatalf("server.addr holds %#v, want the environment's value", got)
	}
}

func TestEnvLayerRecordsProvenance(t *testing.T) {
	m := envManifest(t, "MYAPP")
	d := newData(m)
	if _, err := d.applyEnv(m, []string{"MYAPP_SERVER__ADDR=:9090"}); err != nil {
		t.Fatalf("applying the environment failed: %v", err)
	}
	if got := d.values["server.addr"].layer; got != layerEnv {
		t.Fatalf("server.addr records %v, want the environment layer", got)
	}
	if !d.given("server.addr") {
		t.Fatal("a value the environment gave does not count as given, and a required item " +
			"satisfied by it alone would fail")
	}
	if d.given("server.retries") {
		t.Fatal("an item the environment did not give counts as given")
	}
}

// TestUnknownPrefixedEnvPassesAndIsCollected pins the one origin that does not
// fail on a key it does not know. The process environment is not the host's
// alone: an orchestrator injects <PREFIX>_SERVICE_HOST by service name, and
// treating that as a startup failure would let a deployment action unrelated
// to configuration kill the process.
func TestUnknownPrefixedEnvPassesAndIsCollected(t *testing.T) {
	m := envManifest(t, "MYAPP")
	d := newData(m)
	unclaimed, err := d.applyEnv(m, []string{
		"MYAPP_SERVICE_HOST=10.0.0.1",
		"MYAPP_PORT=8080",
		"MYAPP_SERVER__ADDR=:9090",
		"MYAPP_CONFIG=file:///a.yaml",
		"PATH=/usr/bin",
	})
	if err != nil {
		t.Fatalf("a variable under the prefix that no item reads failed the startup: %v", err)
	}
	want := []string{"MYAPP_PORT", "MYAPP_SERVICE_HOST"}
	if !slices.Equal(unclaimed, want) {
		t.Fatalf("the diagnostics list is %v, want %v: the claimed name and the locator are not "+
			"in it, and unrelated variables outside the prefix are not either", unclaimed, want)
	}
}

// TestItemWithoutEnvOriginIsNotOverridden pins that an item not declaring the
// environment derives no name, so a variable that looks like its name takes
// the pass-and-report path rather than overriding it.
func TestItemWithoutEnvOriginIsNotOverridden(t *testing.T) {
	m := envManifest(t, "MYAPP")
	d := newData(m)
	unclaimed, err := d.applyEnv(m, []string{"MYAPP_SERVER__FLAGS_ON=given"})
	if err != nil {
		t.Fatalf("applying the environment failed: %v", err)
	}
	if got := d.values["server.flags-on"].value; got != "" {
		t.Fatalf("server.flags-on holds %#v, want the default: the item does not read the "+
			"environment", got)
	}
	if !slices.Contains(unclaimed, "MYAPP_SERVER__FLAGS_ON") {
		t.Fatalf("the diagnostics list is %v, and a variable no item reads belongs in it", unclaimed)
	}
}

// TestPinnedEnvNameWorksWithoutPrefix pins that a pinned name is not built out
// of the prefix and therefore does not depend on there being one.
func TestPinnedEnvNameWorksWithoutPrefix(t *testing.T) {
	m := envManifest(t, "")
	d := newData(m)
	unclaimed, err := d.applyEnv(m, []string{"NO_COLOR=true", "MYAPP_SERVER__ADDR=:9090"})
	if err != nil {
		t.Fatalf("applying the environment failed: %v", err)
	}
	if got := d.values["server.colour"].value; got != true {
		t.Fatalf("server.colour holds %#v, want the pinned variable's value", got)
	}
	if got := d.values["server.addr"].value; got != ":8080" {
		t.Fatalf("server.addr holds %#v, want the default: with no prefix there is no derived "+
			"name to match", got)
	}
	if len(unclaimed) != 0 {
		t.Fatalf("the diagnostics list is %v, and with no prefix there is no scan domain", unclaimed)
	}
}

func TestStringListFromEnvReplaces(t *testing.T) {
	m := envManifest(t, "MYAPP")
	d := newData(m)
	if _, err := d.applyEnv(m, []string{"MYAPP_SERVER__HOSTS=x,y,z"}); err != nil {
		t.Fatalf("applying the environment failed: %v", err)
	}
	want := []string{"x", "y", "z"}
	if got := d.values["server.hosts"].value; !reflect.DeepEqual(got, want) {
		t.Fatalf("server.hosts holds %#v, want %#v", got, want)
	}
}

func TestEnvTypeMismatch(t *testing.T) {
	m := envManifest(t, "MYAPP")
	d := newData(m)
	_, err := d.applyEnv(m, []string{"MYAPP_SERVER__RETRIES=many"})
	if !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("an unconvertible variable returned %v, want ErrTypeMismatch", err)
	}
	if !strings.Contains(err.Error(), "server.retries") {
		t.Fatalf("the rejection reads %q, which does not name the path", err)
	}
}
