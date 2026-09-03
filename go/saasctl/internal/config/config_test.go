package config

import (
	"fmt"
	"strings"
	"testing"
)

// TestRunWithoutACommandPrintsTheGroupUsageOnStdout: the bare invocation
// is help -- group usage on stdout, exit 0 -- matching the root command's
// convention.
func TestRunWithoutACommandPrintsTheGroupUsageOnStdout(t *testing.T) {
	code, stdout, stderr := runGroup(t, nil)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if stdout != usage {
		t.Errorf("stdout = %q, want the group usage", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

// TestRunHelpFormsPrintTheGroupUsageOnStdout: help, -h and --help are all
// help -- group usage on stdout, exit 0 -- regardless of their position.
func TestRunHelpFormsPrintTheGroupUsageOnStdout(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}} {
		code, stdout, stderr := runGroup(t, args)
		if code != 0 {
			t.Errorf("Run(%v) exit code = %d, want 0", args, code)
		}
		if stdout != usage {
			t.Errorf("Run(%v) stdout = %q, want the group usage", args, stdout)
		}
		if stderr != "" {
			t.Errorf("Run(%v) stderr = %q, want empty", args, stderr)
		}
	}
}

// TestRunUnknownSubcommandIsAUsageError: an unknown subcommand gets the
// root command's unknown-command treatment -- a naming message plus the
// group usage on stderr, exit 2.
func TestRunUnknownSubcommandIsAUsageError(t *testing.T) {
	code, stdout, stderr := runGroup(t, []string{"frobnicate"})
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	want := fmt.Sprintf("saasctl config: unknown command %q\n\n%s", "frobnicate", usage)
	if stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

// TestRunPrintHelpPrintsThePrintUsageOnStderr: the subcommand's own -h is
// handled by its flag set -- usage on stderr, exit 0.
func TestRunPrintHelpPrintsThePrintUsageOnStderr(t *testing.T) {
	code, stdout, stderr := runGroup(t, []string{"print", "-h"})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if stderr != printUsage {
		t.Errorf("stderr = %q, want the print usage", stderr)
	}
}

// TestRunPrintUnknownFlagIsAUsageError: an unknown flag on the print
// subcommand is a parse error -- the flag package's message plus the
// print usage on stderr, exit 2.
func TestRunPrintUnknownFlagIsAUsageError(t *testing.T) {
	code, _, stderr := runGroup(t, []string{"print", "-frobnicate"})
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "flag provided but not defined: -frobnicate") {
		t.Errorf("stderr = %q, want the flag package's unknown-flag message", stderr)
	}
	if !strings.Contains(stderr, "Usage: saasctl config print [go.mod]") {
		t.Errorf("stderr = %q, want the print usage", stderr)
	}
}

// runGroup invokes the config group's Run with args and returns its exit
// code and the two captured output streams.
func runGroup(t *testing.T, args []string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut strings.Builder
	code = Run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}
