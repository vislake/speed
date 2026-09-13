package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

type serverOptions struct {
	Addr     string
	Verbose  bool
	Retries  int
	Hosts    []string
	FileOnly string
}

// flagManifest declares one module whose items cover every command-line shape:
// a string with a short name, a boolean, a number, a list of strings, and an
// item that never reaches the command line at all.
func flagManifest(t *testing.T) *manifest {
	t.Helper()
	defaults := serverOptions{Addr: ":8080", Retries: 3, Hosts: []string{"a"}}
	return collect(t, identifying("host", HostIdentity{Prefix: "MYAPP"}), declaring("server", Schema{
		Namespace: "server",
		Mounts:    []Mount{{Value: &defaults}},
		Items: map[string]Item{
			"addr":    {Origins: OriginFlag | OriginEnv, FlagName: "addr", FlagShort: "a"},
			"verbose": {Origins: OriginFlag, FlagName: "verbose", FlagShort: "v"},
			"retries": {Origins: OriginFlag, FlagName: "retries"},
			"hosts":   {Origins: OriginFlag, FlagName: "hosts"},
		},
	}))
}

func parse(t *testing.T, m *manifest, args ...string) *parsedFlags {
	t.Helper()
	p, err := parseFlags(m, args)
	if err != nil {
		t.Fatalf("parsing %v failed: %v", args, err)
	}
	return p
}

func parseRejects(t *testing.T, m *manifest, args ...string) error {
	t.Helper()
	p, err := parseFlags(m, args)
	if err == nil {
		t.Fatalf("parsing %v produced %#v, want a rejection", args, p.values)
	}
	return err
}

func TestLongFlagBothForms(t *testing.T) {
	m := flagManifest(t)
	for _, args := range [][]string{{"--addr=:9090"}, {"--addr", ":9090"}} {
		p := parse(t, m, args...)
		if got := p.values["server.addr"]; got != ":9090" {
			t.Fatalf("%v gave server.addr %q, want :9090", args, got)
		}
	}
}

func TestShortFlagBothForms(t *testing.T) {
	m := flagManifest(t)
	for _, args := range [][]string{{"-a=:9090"}, {"-a", ":9090"}} {
		p := parse(t, m, args...)
		if got := p.values["server.addr"]; got != ":9090" {
			t.Fatalf("%v gave server.addr %q, want :9090", args, got)
		}
	}
}

// TestBoolFlagWithoutValue pins that a boolean may omit its value, and that
// giving one is written with an equals sign: taking the next word would leave
// no way to tell a value from the argument after it.
func TestBoolFlagWithoutValue(t *testing.T) {
	m := flagManifest(t)
	if got := parse(t, m, "--verbose").values["server.verbose"]; got != "true" {
		t.Fatalf("--verbose gave %q, want true", got)
	}
	if got := parse(t, m, "-v").values["server.verbose"]; got != "true" {
		t.Fatalf("-v gave %q, want true", got)
	}
	if got := parse(t, m, "--verbose=false").values["server.verbose"]; got != "false" {
		t.Fatalf("--verbose=false gave %q, want false", got)
	}
}

func TestBoolFlagDoesNotTakeTheNextWord(t *testing.T) {
	m := flagManifest(t)
	err := parseRejects(t, m, "--verbose", "false")
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("--verbose false returned %v, want ErrUnknownKey: the word after a boolean is "+
			"a positional argument, not its value", err)
	}
}

func TestUnknownFlagRejected(t *testing.T) {
	m := flagManifest(t)
	if err := parseRejects(t, m, "--nope=1"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("an undeclared argument returned %v, want ErrUnknownKey", err)
	}
}

// TestItemWithoutFlagOriginHasNoFlag pins that an item not declaring the
// command line has no argument at all, whatever its path is.
func TestItemWithoutFlagOriginHasNoFlag(t *testing.T) {
	m := flagManifest(t)
	for _, written := range []string{"--file-only=x", "--server-file-only=x", "--server.file-only=x"} {
		if err := parseRejects(t, m, written); !errors.Is(err, ErrUnknownKey) {
			t.Fatalf("%s returned %v, want ErrUnknownKey", written, err)
		}
	}
}

// TestReservedConfigFlagAcceptedAndNotInData pins that the strict parser lets
// this module's own name through, and that the value stays out of the config
// data: were it to go in, the unknown-key check would trip over it first.
func TestReservedConfigFlagAcceptedAndNotInData(t *testing.T) {
	m := flagManifest(t)
	for _, args := range [][]string{{"--config=file:///a.yaml"}, {"--config", "file:///a.yaml"}} {
		p := parse(t, m, args...)
		if !p.hasLocator || p.locator != "file:///a.yaml" {
			t.Fatalf("%v gave locator %q (given: %v), want file:///a.yaml", args, p.locator, p.hasLocator)
		}
		if len(p.values) != 0 {
			t.Fatalf("%v put %#v into the config data, want nothing", args, p.values)
		}
	}
	if p := parse(t, m, "--addr=:1"); p.hasLocator {
		t.Fatal("a command line without --config reports a locator, and an absent locator has to " +
			"stay apart from an empty one")
	}
}

func TestReservedHelpFlagAccepted(t *testing.T) {
	m := flagManifest(t)
	if !parse(t, m, "--help").help {
		t.Fatal("--help was not recognised")
	}
	if parse(t, m, "--addr=:1").help {
		t.Fatal("a command line without --help asks for help")
	}
}

// TestNoShortHelpFlag pins that short names all come from declarations: the
// reserved names are the two long ones and nothing else.
func TestNoShortHelpFlag(t *testing.T) {
	m := flagManifest(t)
	if err := parseRejects(t, m, "-h"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("-h returned %v, want ErrUnknownKey", err)
	}
}

// TestDoubleDashTerminator pins that nothing after the terminator is parsed,
// so an argument that would otherwise be refused passes by untouched.
func TestDoubleDashTerminator(t *testing.T) {
	m := flagManifest(t)
	p := parse(t, m, "--addr=:9090", "--", "--nope", "whatever")
	if got := p.values["server.addr"]; got != ":9090" {
		t.Fatalf("the arguments before the terminator gave server.addr %q, want :9090", got)
	}
	if len(p.values) != 1 {
		t.Fatalf("the arguments after the terminator produced %#v, want nothing", p.values)
	}
}

func TestPositionalArgumentRejected(t *testing.T) {
	m := flagManifest(t)
	err := parseRejects(t, m, "serve")
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("a positional argument returned %v, want ErrUnknownKey", err)
	}
	if !strings.Contains(err.Error(), "positional") {
		t.Fatalf("the rejection reads %q, which does not say what was wrong", err)
	}
}

// TestShortFlagClusteringUnsupported pins that -av is one name rather than
// two: clustering pays off in a tool whose short names are dense, and these
// are declared piecemeal by separate modules.
func TestShortFlagClusteringUnsupported(t *testing.T) {
	m := flagManifest(t)
	err := parseRejects(t, m, "-av")
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("-av returned %v, want ErrUnknownKey", err)
	}
}

func TestArgumentEndsWithoutItsValue(t *testing.T) {
	m := flagManifest(t)
	for _, args := range [][]string{{"--addr"}, {"-a"}, {"--config"}} {
		err := parseRejects(t, m, args...)
		if !strings.Contains(err.Error(), "takes a value") {
			t.Fatalf("%v returned %q, which does not say the argument needs a value", args, err)
		}
	}
}

// TestRepeatedFlagKeepsTheLast pins that a repeated argument overrides itself,
// the same way a higher layer overrides a lower one.
func TestRepeatedFlagKeepsTheLast(t *testing.T) {
	m := flagManifest(t)
	if got := parse(t, m, "--addr=:1", "--addr=:2").values["server.addr"]; got != ":2" {
		t.Fatalf("a repeated argument gave %q, want the last occurrence", got)
	}
}

func TestStringListFromFlagReplaces(t *testing.T) {
	m := flagManifest(t)
	d := newData(m)
	if err := d.applyFlags(m, parse(t, m, "--hosts=x,y")); err != nil {
		t.Fatalf("applying the command line failed: %v", err)
	}
	want := []string{"x", "y"}
	if got := d.values["server.hosts"].value; !reflect.DeepEqual(got, want) {
		t.Fatalf("server.hosts holds %#v, want %#v: the declared default is replaced, not "+
			"appended to", got, want)
	}
}

// TestFlagOverridesEnv pins the order of the two top layers.
func TestFlagOverridesEnv(t *testing.T) {
	m := flagManifest(t)
	d := newData(m)
	if _, err := d.applyEnv(m, []string{"MYAPP_SERVER__ADDR=:7000"}); err != nil {
		t.Fatalf("applying the environment failed: %v", err)
	}
	if got := d.values["server.addr"].value; got != ":7000" {
		t.Fatalf("server.addr holds %#v after the environment layer, want the variable's value", got)
	}
	if err := d.applyFlags(m, parse(t, m, "--addr=:9090")); err != nil {
		t.Fatalf("applying the command line failed: %v", err)
	}
	if got := d.values["server.addr"].value; got != ":9090" {
		t.Fatalf("server.addr holds %#v, want the command line's value", got)
	}
}

// TestFlagLayerRecordsProvenance pins the record the required rule reads back.
func TestFlagLayerRecordsProvenance(t *testing.T) {
	m := flagManifest(t)
	d := newData(m)
	if err := d.applyFlags(m, parse(t, m, "--addr=:9090")); err != nil {
		t.Fatalf("applying the command line failed: %v", err)
	}
	if got := d.values["server.addr"].layer; got != layerFlag {
		t.Fatalf("server.addr records %v, want the command-line layer", got)
	}
	if got := d.values["server.retries"].layer; got != layerDefault {
		t.Fatalf("an item the command line did not give records %v, want the default layer", got)
	}
}

func TestFlagTypeMismatch(t *testing.T) {
	m := flagManifest(t)
	d := newData(m)
	err := d.applyFlags(m, parse(t, m, "--retries=many"))
	if !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("an unconvertible argument returned %v, want ErrTypeMismatch", err)
	}
	if !strings.Contains(err.Error(), "server.retries") {
		t.Fatalf("the rejection reads %q, which does not name the path", err)
	}
}
