package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/vislake/speed/go/saasctl/internal/appconfig"
	"github.com/vislake/speed/go/saasctl/internal/project"
)

// defaultModPath is the go.mod the print command reads when the command
// line names none: the working directory's, matching the sibling
// commands' default.
const defaultModPath = "go.mod"

// redactedMarker stands in for a key variable's bytes in the rendered
// value column. A key is a secret: whatever the environment holds, the
// output carries only this marker -- never the key itself, and never a
// hint of its shape.
const redactedMarker = "[redacted]"

// printUsage is the print subcommand's help text.
const printUsage = `Usage: saasctl config print [go.mod]

Show how a project's bootstrap configuration resolves: read the go.mod
(the [go.mod] argument, defaulting to ./go.mod) for the project's app
name, resolve the project's bootstrap environment the way the generated
app's configFromEnv resolves it, and render the five resolved values with
each one's provenance -- which variable carried a value, and which fell
back to its default.

The five bootstrap variables:

  SPEED_DEPLOYMENT_MODE   the deployment mode (standalone or distributed)
  PORT                    the HTTP port
  SPEED_DB_PATH           the SQLite database path
  SPEED_CONFIG_KEY        the configuration master key (64 hex characters)
  SPEED_ORG_INDEX_KEY     the org blind-index key (64 hex characters)

The two key variables are secrets: their values never print, only a
[redacted] marker in their place, whatever the environment holds.

Examples:

  saasctl config print
  saasctl config print /path/to/project/go.mod

Exit codes: 0 success or help, 2 usage error, 1 execution error.
`

// runPrint parses the print invocation and reports the rendered rows on
// stdout, following the migrate subcommand's conventions: a flag parse
// error is a usage error (exit 2), help is success (exit 0), an execution
// failure is reported on stderr (exit 1).
func runPrint(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("print", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		_, _ = fmt.Fprint(stderr, printUsage)
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	modPath := defaultModPath
	if paths := flags.Args(); len(paths) > 1 {
		return usageError(stderr, fmt.Errorf("expected at most one go.mod path, got %d", len(paths)))
	} else if len(paths) == 1 {
		modPath = paths[0]
	}
	rendered, err := print(modPath)
	if err != nil {
		return reportError(stderr, err)
	}
	_, _ = fmt.Fprint(stdout, rendered)
	return 0
}

// usageError reports a malformed invocation: the message plus the print
// usage on stderr, exit code 2.
func usageError(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "saasctl config print: %v\n\n%s", err, printUsage)
	return 2
}

// reportError reports a failed execution: one line on stderr, exit code
// 1.
func reportError(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "saasctl config print: %v\n", err)
	return 1
}

// print resolves the bootstrap configuration of the project at modPath --
// the go.mod supplies the app name, the environment supplies the five
// variables, appconfig.Load resolves each to the value the generated app
// would boot on -- and renders one line per value: the label, the
// resolved value, and its provenance. The two key variables render as
// [redacted] in the value column whatever the environment holds.
func print(modPath string) (string, error) {
	proj, err := project.Read(modPath)
	if err != nil {
		return "", err
	}
	cfg, err := appconfig.Load(proj.AppName, os.LookupEnv)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	write := func(label, value, source string) {
		fmt.Fprintf(&b, "%-16s %-12s %s\n", label, value, source)
	}
	write("deployment mode", string(cfg.DeploymentMode),
		provenance(appconfig.DeploymentModeEnv, cfg.DeploymentModeFromEnv, string(cfg.DeploymentMode)))
	write("port", cfg.Port, provenance(appconfig.PortEnv, cfg.PortFromEnv, cfg.Port))
	write("sqlite path", cfg.SQLitePath, provenance(appconfig.DBPathEnv, cfg.SQLitePathFromEnv, cfg.SQLitePath))
	write("config key", redactedMarker, provenance(appconfig.ConfigKeyEnv, cfg.ConfigKeyFromEnv, ""))
	write("org index key", redactedMarker, provenance(appconfig.OrgIndexKeyEnv, cfg.OrgIndexKeyFromEnv, ""))
	return b.String(), nil
}

// provenance describes where one resolved value came from: the variable
// that carried it, or the default it fell back to. An empty defaultValue
// marks a key variable, whose default is only ever named as the
// development default -- the key bytes themselves are secrets and never
// rendered anywhere in the output.
func provenance(envName string, fromEnv bool, defaultValue string) string {
	if fromEnv {
		return fmt.Sprintf("from %s", envName)
	}
	if defaultValue == "" {
		return "unset or empty (development default)"
	}
	return fmt.Sprintf("unset or empty (default %s)", defaultValue)
}
