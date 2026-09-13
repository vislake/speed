package config

import (
	"fmt"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/pkg/core"
)

type helpOptions struct {
	Salutation string
	Token      string
	TTL        time.Duration
	FileOnly   string
}

// helpManifest declares the shapes the help output has to render: a grouped
// item, an ungrouped one with a short name and a placeholder, a sensitive one,
// and an item that never reaches the command line.
func helpManifest(t *testing.T, modules ...core.Module) *manifest {
	t.Helper()
	defaults := helpOptions{Salutation: "Hello", Token: "s3cr3t", TTL: 5 * time.Minute}
	declaration := declaring("greeter", Schema{
		Namespace: "greeter",
		Mounts:    []Mount{{Value: &defaults}},
		Items: map[string]Item{
			"salutation": {
				Origins:     OriginFlag,
				FlagName:    "salutation",
				FlagShort:   "s",
				Placeholder: "TEXT",
				Description: "the greeting to use",
			},
			"token": {
				Origins:     OriginFlag,
				FlagName:    "remote-token",
				Sensitive:   true,
				Description: "the credential of the remote greeter",
			},
			"ttl": {
				Origins:     OriginFlag,
				FlagName:    "cache-ttl",
				Group:       "Cache",
				Description: "how long an entry lives",
			},
		},
	})
	return collect(t, append(modules, declaration)...)
}

func renderedHelp(t *testing.T, m *manifest) string {
	t.Helper()
	var b strings.Builder
	if err := renderHelp(&b, m); err != nil {
		t.Fatalf("rendering the help output failed: %v", err)
	}
	return b.String()
}

func TestHelpRendersGroupsPlaceholdersAndDefaults(t *testing.T) {
	out := renderedHelp(t, helpManifest(t))
	for _, want := range []string{
		"Options:",
		"Cache:",
		"-s, --salutation TEXT",
		"the greeting to use",
		"(default: Hello)",
		"--cache-ttl VALUE",
		"(default: 5m0s)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the help output does not contain %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "Options:") > strings.Index(out, "Cache:") {
		t.Fatalf("the ungrouped section comes after a named group:\n%s", out)
	}
}

// TestHelpListsOnlyFlagExposedItems pins that an item with no command-line
// origin has no argument to describe and does not appear.
func TestHelpListsOnlyFlagExposedItems(t *testing.T) {
	out := renderedHelp(t, helpManifest(t))
	if strings.Contains(out, "file-only") {
		t.Fatalf("the help output lists an item that reaches no command-line argument:\n%s", out)
	}
}

// TestHelpOmitsSensitiveDefault pins the security rule config carries for
// every module: a sensitive item shows its name and description, never its
// value. That is also why a description is mandatory for one.
func TestHelpOmitsSensitiveDefault(t *testing.T) {
	out := renderedHelp(t, helpManifest(t))
	if !strings.Contains(out, "--remote-token") || !strings.Contains(out, "the credential of the remote greeter") {
		t.Fatalf("the help output does not describe the sensitive item:\n%s", out)
	}
	if strings.Contains(out, "s3cr3t") {
		t.Fatalf("the help output echoes the default of a sensitive item:\n%s", out)
	}
	if strings.Contains(out, "--remote-token VALUE  (default:") {
		t.Fatalf("the help output offers a default for a sensitive item:\n%s", out)
	}
}

// TestHelpListsTheReservedNames pins that the output describes the whole
// command line the parser accepts, this module's own two names included.
func TestHelpListsTheReservedNames(t *testing.T) {
	out := renderedHelp(t, helpManifest(t))
	for _, want := range []string{"Reserved:", "--config LOCATOR", "--help"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the help output does not contain %q:\n%s", want, out)
		}
	}
}

// TestHelpOrderIsStableAcrossRegistrationOrders pins that the output does not
// depend on the order the modules were registered in, which init does not fix.
func TestHelpOrderIsStableAcrossRegistrationOrders(t *testing.T) {
	other := helpOptions{}
	second := declaring("aardvark", Schema{
		Namespace: "aardvark",
		Mounts:    []Mount{{Value: &other}},
		Items: map[string]Item{
			"salutation": {Origins: OriginFlag, FlagName: "aardvark-salutation"},
		},
	})
	host := identifying("host", HostIdentity{Prefix: "MYAPP"})
	first := renderedHelp(t, helpManifest(t, second, host))
	again := renderedHelp(t, helpManifest(t, host, second))
	if first != again {
		t.Fatalf("the help output changed with the registration order:\n%s\n---\n%s", first, again)
	}
}

// requiredHelpManifest declares the two shapes the required rule has to
// render: a plain required item, and one that is required and sensitive at
// once. No item in the rest of the code base is both, so this case brings its
// own fixture.
func requiredHelpManifest(t *testing.T) *manifest {
	t.Helper()
	type requiredOptions struct {
		Addr  string
		Token string
	}
	defaults := requiredOptions{Token: "s3cr3t"}
	return collect(t, declaring("greeter", Schema{
		Namespace: "greeter",
		Mounts:    []Mount{{Value: &defaults}},
		Items: map[string]Item{
			"addr": {
				Origins:     OriginFlag,
				FlagName:    "addr",
				Placeholder: "HOST:PORT",
				Description: "the address of the greeting service",
				Required:    true,
			},
			"token": {
				Origins:     OriginFlag,
				FlagName:    "token",
				Placeholder: "TOKEN",
				Description: "the credential of the remote greeter",
				Required:    true,
				Sensitive:   true,
			},
		},
	}))
}

// TestHelpMarksRequiredItemsInsteadOfDefaults pins that a required item shows
// what it asks of the reader. A column showing an empty value would read as
// "leave it out and get the empty string", while leaving it out really means
// the module never comes up.
func TestHelpMarksRequiredItemsInsteadOfDefaults(t *testing.T) {
	line := helpLineFor(t, renderedHelp(t, requiredHelpManifest(t)), "--addr")
	if !strings.Contains(line, "(required)") {
		t.Fatalf("the required item renders as %q, which does not mark it required", line)
	}
	if strings.Contains(line, "(default:") {
		t.Fatalf("the required item renders as %q, which still offers a default", line)
	}
}

// TestHelpMarksARequiredSensitiveItem pins the order the two rules are applied
// in: an item that is both would otherwise show neither a default nor a
// marker, leaving the reader with nothing to go on at all.
func TestHelpMarksARequiredSensitiveItem(t *testing.T) {
	out := renderedHelp(t, requiredHelpManifest(t))
	line := helpLineFor(t, out, "--token")
	if !strings.Contains(line, "(required)") {
		t.Fatalf("the required sensitive item renders as %q, which does not mark it required", line)
	}
	if strings.Contains(out, "s3cr3t") {
		t.Fatalf("the help output echoes the default of a sensitive item:\n%s", out)
	}
}

// helpLineFor picks the rendered line of one argument out of the help output.
func helpLineFor(t *testing.T, out, argument string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, argument+" ") || strings.HasSuffix(line, argument) {
			return line
		}
	}
	t.Fatalf("the help output has no line for %s:\n%s", argument, out)
	return ""
}

// booleanHelpOptions carries the two boolean shapes the help output has to
// render the same way: a plain one, and one behind a pointer.
type booleanHelpOptions struct {
	Verbose bool
	Trace   *bool
}

func booleanHelpManifest(t *testing.T) *manifest {
	t.Helper()
	var defaults booleanHelpOptions
	return collect(t, declaring("greeter", Schema{
		Namespace: "greeter",
		Mounts:    []Mount{{Value: &defaults}},
		Items: map[string]Item{
			"verbose": {Origins: OriginFlag, FlagName: "verbose", Description: "say more"},
			"trace":   {Origins: OriginFlag, FlagName: "trace", Description: "record the calls"},
		},
	}))
}

// TestHelpGivesEitherBooleanNoPlaceholder pins that the help output looks
// through the pointer exactly as the parser does. Rendering "--trace VALUE"
// would advertise a spelling the parser refuses, and the two ends have to
// agree or the documented command line is not the accepted one.
func TestHelpGivesEitherBooleanNoPlaceholder(t *testing.T) {
	out := renderedHelp(t, booleanHelpManifest(t))
	for _, name := range []string{"--verbose", "--trace"} {
		if line := helpLineFor(t, out, name); strings.Contains(line, "VALUE") {
			t.Fatalf("the help output renders %s as %q, offering a value the parser does not "+
				"take", name, line)
		}
	}
}

// helpLevel prints a word for the number it stores, which is a form no source
// takes back. It is here so the default column can be pinned to the number.
type helpLevel int

func (l helpLevel) String() string { return "debug" }

// helpLabel carries its own text form, and that form holds a space while the
// type is neither a string nor a list of strings. It is the shape the quoting
// rule has to reach: a rule reading the field's Go kind leaves this one bare,
// and the reader who copies the line back hands the program two arguments.
type helpLabel struct{ name string }

func (l helpLabel) MarshalText() ([]byte, error) { return []byte(l.name), nil }

func (l *helpLabel) UnmarshalText(text []byte) error {
	l.name = string(text)
	return nil
}

// helpPercent reads itself from text but writes none: it holds 50 and takes
// "50%" back. It is the shape a leaf has when its stored value and its own
// input form are not the same text.
type helpPercent int

func (p *helpPercent) UnmarshalText(text []byte) error {
	digits, ok := strings.CutSuffix(string(text), "%")
	if !ok {
		return fmt.Errorf("a percentage is written as in \"50%%\"")
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return err
	}
	*p = helpPercent(n)
	return nil
}

func (p helpPercent) String() string { return strconv.Itoa(int(p)) + "%" }

// defaultsCorpus carries one field of every shape the default column has to
// render, so one rendering covers them all: the plain scalars, a named integer
// that prints a word of its own, a duration, three types that carry their own
// text form, one that reads text without writing any, both states of a scalar
// pointer, a sensitive unset one, and a list in both of its states.
//
// The texts are chosen for the quoting rule as much as for the types: a value
// needing no quotes, one carrying a space behind a text form, one carrying the
// characters a shell acts on, one carrying a quote of its own, one carrying a
// character with no typeable form at all, and one opening with a dash.
type defaultsCorpus struct {
	Text    string
	Spaced  string
	Tricky  string
	Quoted  string
	Control string
	Offset  int
	Label   helpLabel
	Flag    bool
	Count   int
	Level   helpLevel
	Ratio   float32
	TTL     time.Duration
	Moment  time.Time
	Amount  big.Int
	Share   helpPercent
	Held    *int
	Missing *int
	Secret  *string
	Hosts   []string
	NoHosts []string
}

func defaultsCorpusManifest(t *testing.T) *manifest {
	t.Helper()
	held := 7
	defaults := defaultsCorpus{
		Text:    "Hello",
		Spaced:  "Hello there",
		Tricky:  `a$b;c\d*e`,
		Quoted:  "it's",
		Control: "a\tb",
		Offset:  -42,
		Label:   helpLabel{name: "west wing"},
		Flag:    true,
		Count:   8080,
		Level:   3,
		Ratio:   0.1,
		TTL:     5 * time.Minute,
		Moment:  time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC),
		Amount:  *big.NewInt(42),
		Share:   50,
		Held:    &held,
		Hosts:   []string{"a", "b"},
	}
	items := make(map[string]Item, 20)
	for _, key := range []string{
		"text", "spaced", "tricky", "quoted", "control", "offset", "label",
		"flag", "count", "level", "ratio", "ttl", "moment",
		"amount", "share", "held", "missing", "no-hosts", "hosts",
	} {
		items[key] = Item{Origins: OriginFlag, FlagName: key}
	}
	items["secret"] = Item{
		Origins:     OriginFlag,
		FlagName:    "secret",
		Sensitive:   true,
		Description: "the credential the greeter presents",
	}
	return collect(t, declaring("greeter", Schema{
		Namespace: "greeter",
		Mounts:    []Mount{{Value: &defaults}},
		Items:     items,
	}))
}

// notTypedBack names the arguments the round trip leaves out, with the reason
// each one is left out. A skip is written down rather than silently passed
// over, so the set of shapes that cannot be typed back stays visible. The
// round trip itself runs in help_quoting_test.go, through a real shell.
var notTypedBack = map[string]string{
	"share": "the type reads itself from text and writes none, so the column " +
		"carries Go's own form rather than a value the parser takes back",
	"control": "its text carries a tab, and a command line has no single-line " +
		"literal standing for one, so the column renders an escaped form meant " +
		"to be read rather than copied",
}

// renderedDefault picks the value out of a rendered line, or reports that the
// line carries a marker instead of a default.
func renderedDefault(line string) (string, bool) {
	const opening = "(default: "
	at := strings.Index(line, opening)
	if at < 0 {
		return "", false
	}
	rest := line[at+len(opening):]
	end := strings.LastIndex(rest, ")")
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

// sameDefault compares what the command line produced with the declared
// default. Lists are compared element by element: a nil list and an empty one
// render the same flat text, so what comes back can only match one of them.
func sameDefault(got, want any) bool {
	gv, wv := reflect.ValueOf(got), reflect.ValueOf(want)
	if gv.Kind() == reflect.Slice && wv.Kind() == reflect.Slice {
		if gv.Len() != wv.Len() {
			return false
		}
		for i := range gv.Len() {
			if gv.Index(i).Interface() != wv.Index(i).Interface() {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(got, want)
}

// TestHelpPointerDefaultShowsThePointee pins that a scalar pointer renders
// what it points at. An address is a number that changes every run and names
// no value the reader could give.
func TestHelpPointerDefaultShowsThePointee(t *testing.T) {
	line := helpLineFor(t, renderedHelp(t, defaultsCorpusManifest(t)), "--held")
	if !strings.Contains(line, "(default: 7)") {
		t.Fatalf("the pointer default renders as %q, and its value is 7", line)
	}
	if strings.Contains(line, "0x") {
		t.Fatalf("the pointer default renders as %q, which shows an address", line)
	}
}

// TestHelpTextDefaultUsesTheTypesOwnTextForm pins that a leaf which reads
// itself from a string renders in that same string form. Both of these carry
// their text methods on the pointer receiver, which is the shape a value held
// in an interface hides.
func TestHelpTextDefaultUsesTheTypesOwnTextForm(t *testing.T) {
	out := renderedHelp(t, defaultsCorpusManifest(t))
	moment := helpLineFor(t, out, "--moment")
	if !strings.Contains(moment, "2026-09-13T10:00:00Z") {
		t.Fatalf("the time default renders as %q, and its own text form is RFC 3339", moment)
	}
	if strings.Contains(moment, "+0000 UTC") {
		t.Fatalf("the time default renders as %q, which is Go's printed form", moment)
	}
	amount := helpLineFor(t, out, "--amount")
	if !strings.Contains(amount, "(default: 42)") {
		t.Fatalf("the integer default renders as %q, and its value is 42", amount)
	}
	if strings.Contains(amount, "{false") {
		t.Fatalf("the integer default renders as %q, which shows the fields it holds", amount)
	}
}

// TestHelpNumericDefaultIgnoresAStringerForm pins that a number renders as a
// number. A type printing a word for its value says what the value means, not
// what a source would have to give to produce it.
func TestHelpNumericDefaultIgnoresAStringerForm(t *testing.T) {
	line := helpLineFor(t, renderedHelp(t, defaultsCorpusManifest(t)), "--level")
	if !strings.Contains(line, "(default: 3)") {
		t.Fatalf("the named integer renders as %q, and its value is 3", line)
	}
	if strings.Contains(line, "debug") {
		t.Fatalf("the named integer renders as %q, which is a word no source takes", line)
	}
}

// TestHelpFloatDefaultKeepsItsPrecision pins that a 32-bit float renders at
// its own width. Widened to 64 bits it grows a tail of digits that is not what
// the module declared.
func TestHelpFloatDefaultKeepsItsPrecision(t *testing.T) {
	line := helpLineFor(t, renderedHelp(t, defaultsCorpusManifest(t)), "--ratio")
	if !strings.Contains(line, "(default: 0.1)") {
		t.Fatalf("the float default renders as %q, and its value is 0.1", line)
	}
	if strings.Contains(line, "0.10000000149011612") {
		t.Fatalf("the float default renders as %q, which is the 64-bit reading of it", line)
	}
}

// TestHelpDurationDefaultIsWrittenAsDuration pins the duration form, which is
// also the only form the parser takes: a bare nanosecond count is refused on
// the way in and would advertise a value that cannot be given.
func TestHelpDurationDefaultIsWrittenAsDuration(t *testing.T) {
	line := helpLineFor(t, renderedHelp(t, defaultsCorpusManifest(t)), "--ttl")
	if !strings.Contains(line, "(default: 5m0s)") {
		t.Fatalf("the duration default renders as %q, and it is five minutes", line)
	}
	if strings.Contains(line, "300000000000") {
		t.Fatalf("the duration default renders as %q, which is its nanosecond count", line)
	}
}

// TestHelpStringListDefaultIsCommaSeparated pins the flat form of a list,
// which is the one the command line and the environment give.
func TestHelpStringListDefaultIsCommaSeparated(t *testing.T) {
	out := renderedHelp(t, defaultsCorpusManifest(t))
	hosts := helpLineFor(t, out, "--hosts")
	if !strings.Contains(hosts, "(default: a,b)") {
		t.Fatalf("the list default renders as %q, and its flat form is a,b", hosts)
	}
	if strings.Contains(hosts, "[a b]") {
		t.Fatalf("the list default renders as %q, which is Go's printed form", hosts)
	}
	if empty := helpLineFor(t, out, "--no-hosts"); !strings.Contains(empty, `(default: '')`) {
		t.Fatalf("the empty list renders as %q, and an empty list is written as nothing", empty)
	}
}

// TestHelpQuotesAStringDefaultOnlyWhenItsTextNeedsIt pins that a string is not
// quoted for being a string. The quotes follow the text: a word a command line
// takes as it stands is rendered as it stands, and one the shell would split
// or rewrite is wrapped so the reader copies the whole of it.
func TestHelpQuotesAStringDefaultOnlyWhenItsTextNeedsIt(t *testing.T) {
	out := renderedHelp(t, defaultsCorpusManifest(t))
	for name, want := range map[string]string{
		"text":   "Hello",
		"spaced": `'Hello there'`,
		"quoted": `'it'\''s'`,
	} {
		rendered, ok := renderedDefault(helpLineFor(t, out, "--"+name))
		if !ok {
			t.Fatalf("--%s carries no default at all", name)
		}
		if rendered != want {
			t.Errorf("--%s renders %s, and its text asks for %s", name, rendered, want)
		}
	}
}

// TestHelpKeepsTheResidualFormForATextTypeWithoutAnOutputForm pins what a leaf
// gets when it reads itself from text but writes none: Go's own printed form.
// That form is not one the parser is known to take back, which is why this
// argument stays out of the round trip; the alternatives - the number it
// stores, or a spelling made up here - would both be untrue.
func TestHelpKeepsTheResidualFormForATextTypeWithoutAnOutputForm(t *testing.T) {
	line := helpLineFor(t, renderedHelp(t, defaultsCorpusManifest(t)), "--share")
	if !strings.Contains(line, "(default: 50%)") {
		t.Fatalf("the text-reading type renders as %q, and its printed form is 50%%", line)
	}
}

// TestHelpPrintsNoDefaultForAnUnsetLeaf pins the third form of the column. A
// nil scalar pointer is how a module tells "nobody gave it" from "it was given
// the zero value": the zero would be a lie, and Go's <nil> shows the reader
// neither that there is no default nor what the value would be.
func TestHelpPrintsNoDefaultForAnUnsetLeaf(t *testing.T) {
	line := helpLineFor(t, renderedHelp(t, defaultsCorpusManifest(t)), "--missing")
	if !strings.Contains(line, "(no default)") {
		t.Fatalf("the unset leaf renders as %q, and it has no default", line)
	}
	for _, unwanted := range []string{"<nil>", "(default:", "0"} {
		if strings.Contains(line, unwanted) {
			t.Fatalf("the unset leaf renders as %q, which contains %q", line, unwanted)
		}
	}
}

// TestHelpPrintsNoDefaultForAnUnsetSensitiveLeaf pins that the two rules do
// not cancel each other out. The marker echoes nothing, so withholding it as
// well would leave the reader unable to tell a withheld default from none.
func TestHelpPrintsNoDefaultForAnUnsetSensitiveLeaf(t *testing.T) {
	line := helpLineFor(t, renderedHelp(t, defaultsCorpusManifest(t)), "--secret")
	if !strings.Contains(line, "(no default)") {
		t.Fatalf("the unset sensitive leaf renders as %q, and it has no default", line)
	}
	if strings.Contains(line, "(default:") {
		t.Fatalf("the unset sensitive leaf renders as %q, which offers a default", line)
	}
}
