package config

import (
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
		`(default: "Hello")`,
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
