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
	if !errors.Is(err, ErrMalformedCommandLine) {
		t.Fatalf("--verbose false returned %v, want ErrMalformedCommandLine: the word after a "+
			"boolean is a positional argument, not its value", err)
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

// TestFlagNameWithoutFlagOriginRejected pins the second half of the
// unknown-key rule on the command line: the manifest has the name, but the
// item does not take the command line as an origin, so the value is refused
// rather than applied. Accepting it would make an argument the help output
// does not list and yet changes the configuration.
func TestFlagNameWithoutFlagOriginRejected(t *testing.T) {
	defaults := serverOptions{Addr: ":8080"}
	m := collect(t, identifying("host", HostIdentity{Prefix: "MYAPP"}), declaring("server", Schema{
		Namespace: "server",
		Mounts:    []Mount{{Value: &defaults}},
		Items: map[string]Item{
			"addr": {Origins: OriginPrimary, FlagName: "addr", FlagShort: "a"},
		},
	}))
	for _, args := range [][]string{{"--addr=from-cli"}, {"-a", "from-cli"}} {
		err := parseRejects(t, m, args...)
		if !errors.Is(err, ErrUnknownKey) {
			t.Fatalf("%v returned %v, want ErrUnknownKey", args, err)
		}
		if !strings.Contains(err.Error(), "does not accept from the command line") {
			t.Fatalf("%v returned %q, which does not say the item refuses this origin", args, err)
		}
		if !strings.Contains(err.Error(), "server.addr") {
			t.Fatalf("%v returned %q, which does not name the item", args, err)
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

// TestNoDoubleDashTerminator pins that there is no argument terminator: its
// only use in the common convention is to let a positional argument through,
// and those are refused here, so what it would really do is swallow everything
// after it without a word.
func TestNoDoubleDashTerminator(t *testing.T) {
	m := flagManifest(t)
	err := parseRejects(t, m, "--addr=:9090", "--", "--nope", "whatever")
	if !errors.Is(err, ErrMalformedCommandLine) {
		t.Fatalf("a terminator returned %v, want ErrMalformedCommandLine", err)
	}
	if !strings.Contains(err.Error(), "terminator") {
		t.Fatalf("the rejection reads %q, which does not say what was wrong", err)
	}
}

func TestPositionalArgumentRejected(t *testing.T) {
	m := flagManifest(t)
	err := parseRejects(t, m, "serve")
	if !errors.Is(err, ErrMalformedCommandLine) {
		t.Fatalf("a positional argument returned %v, want ErrMalformedCommandLine", err)
	}
	if !strings.Contains(err.Error(), "positional") {
		t.Fatalf("the rejection reads %q, which does not say what was wrong", err)
	}
}

// TestShortFlagClusteringRejected pins that -av is the syntax being wrong
// rather than a name nobody declared: clustering pays off in a tool whose
// short names are dense, and these are declared piecemeal by separate modules.
func TestShortFlagClusteringRejected(t *testing.T) {
	m := flagManifest(t)
	err := parseRejects(t, m, "-av")
	if !errors.Is(err, ErrMalformedCommandLine) {
		t.Fatalf("-av returned %v, want ErrMalformedCommandLine", err)
	}
	if err := parseRejects(t, m, "-av=1"); !errors.Is(err, ErrMalformedCommandLine) {
		t.Fatalf("-av=1 returned %v, want ErrMalformedCommandLine", err)
	}
}

func TestArgumentEndsWithoutItsValue(t *testing.T) {
	m := flagManifest(t)
	for _, args := range [][]string{{"--addr"}, {"-a"}, {"--config"}} {
		err := parseRejects(t, m, args...)
		if !errors.Is(err, ErrMalformedCommandLine) {
			t.Fatalf("%v returned %v, want ErrMalformedCommandLine", args, err)
		}
		if !strings.Contains(err.Error(), "takes a value") {
			t.Fatalf("%v returned %q, which does not say the argument needs a value", args, err)
		}
	}
}

// TestHelpFlagRejectsANonBooleanValue pins the reserved name to the same rule
// as any other boolean: a value it cannot read is the syntax being wrong, not
// a request for help.
func TestHelpFlagRejectsANonBooleanValue(t *testing.T) {
	m := flagManifest(t)
	err := parseRejects(t, m, "--help=maybe")
	if !errors.Is(err, ErrMalformedCommandLine) {
		t.Fatalf("--help=maybe returned %v, want ErrMalformedCommandLine", err)
	}
}

// TestUndeclaredLongNameStaysUnknownKey guards the narrowing from taking over
// the ground ErrUnknownKey holds: --nope is a name the syntax admits and no
// item declares.
func TestUndeclaredLongNameStaysUnknownKey(t *testing.T) {
	m := flagManifest(t)
	if err := parseRejects(t, m, "--nope"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("--nope returned %v, want ErrUnknownKey", err)
	}
}

// TestUndeclaredSingleCharShortNameStaysUnknownKey is the short-name half of
// the same guard: -z is one character, so the syntax is fine and the name is
// simply not declared.
func TestUndeclaredSingleCharShortNameStaysUnknownKey(t *testing.T) {
	m := flagManifest(t)
	if err := parseRejects(t, m, "-z"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("-z returned %v, want ErrUnknownKey", err)
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
