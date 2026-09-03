package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runCLI invokes run with captured output.
func runCLI(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// TestRunNoArgumentsPrintsUsage: a bare `saasctl` prints the root usage to
// stdout and exits 0 -- help, not an error, so a shell alias or a CI step
// that discovers the CLI can probe it harmlessly.
func TestRunNoArgumentsPrintsUsage(t *testing.T) {
	code, stdout, stderr := runCLI(t, nil)
	if code != 0 {
		t.Errorf("run() = %d, want 0", code)
	}
	if !strings.Contains(stdout, "Usage: saasctl <command>") {
		t.Error("bare invocation does not print the root usage to stdout")
	}
	if stderr != "" {
		t.Errorf("bare invocation wrote to stderr: %q", stderr)
	}
}

// TestRunHelpCommands: help, -h and --help all print the root usage to
// stdout and exit 0, matching the flag package's own -h convention.
func TestRunHelpCommands(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		arg := arg
		t.Run(arg, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, []string{arg})
			if code != 0 {
				t.Errorf("run(%q) = %d, want 0", arg, code)
			}
			if !strings.Contains(stdout, "Usage: saasctl <command>") {
				t.Errorf("run(%q) does not print the root usage to stdout", arg)
			}
			if stderr != "" {
				t.Errorf("run(%q) wrote to stderr: %q", arg, stderr)
			}
		})
	}
}

// TestRunUnknownCommand: an unrecognized command is a usage error -- the
// unknown-command line plus the root usage go to stderr, and the exit code
// is 2.
func TestRunUnknownCommand(t *testing.T) {
	code, stdout, stderr := runCLI(t, []string{"frobnicate"})
	if code != 2 {
		t.Errorf("run(unknown) = %d, want 2", code)
	}
	if !strings.Contains(stderr, "unknown command \"frobnicate\"") {
		t.Errorf("stderr does not name the unknown command: %q", stderr)
	}
	if !strings.Contains(stderr, "Usage: saasctl <command>") {
		t.Error("stderr does not carry the root usage")
	}
	if stdout != "" {
		t.Errorf("usage error wrote to stdout: %q", stdout)
	}
}

// TestRunPlannedCommandsNotImplemented: db and config exist on the usage
// surface but land with later milestones. Each is refused with a one-line
// not-implemented message on stderr and exit code 1 -- the execution-error
// code, so a script that treats 2 as "invocation mistake" can distinguish
// "right shape, not built yet" from "malformed".
func TestRunPlannedCommandsNotImplemented(t *testing.T) {
	for _, arg := range []string{"db", "config"} {
		arg := arg
		t.Run(arg, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, []string{arg})
			if code != 1 {
				t.Errorf("run(%q) = %d, want 1", arg, code)
			}
			if !strings.Contains(stderr, "not implemented") {
				t.Errorf("stderr does not say not implemented: %q", stderr)
			}
			if stdout != "" {
				t.Errorf("run(%q) wrote to stdout: %q", arg, stdout)
			}
		})
	}
}

// TestRunNewDispatchesThroughCLI drives `saasctl new` end to end through
// the root dispatch -- the one place main.go and the new command meet --
// against a fabricated speed checkout: exit 0, the go.mod written with
// the target's base name as the module path, and the selection's own
// server.go composed. The new command's own behavior matrix lives in
// internal/new's suite; this test only proves the dispatch reaches it.
func TestRunNewDispatchesThroughCLI(t *testing.T) {
	root := t.TempDir()
	content := "go 1.25.0\n\nuse ./go/pkgcore\n\nuse ./go/dbkit\n"
	if err := os.WriteFile(filepath.Join(root, "go.work"), []byte(content), 0o644); err != nil {
		t.Fatalf("write fake go.work: %v", err)
	}
	target := filepath.Join(t.TempDir(), "cli-app")
	code, stdout, stderr := runCLI(t, []string{"new", "--speed-root", root, target})
	if code != 0 {
		t.Fatalf("run(new) = %d, want 0; stderr:\n%s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("run(new) wrote to stderr on success: %q", stderr)
	}
	if !strings.Contains(stdout, "Wrote "+filepath.Join(target, "go.mod")) {
		t.Error("stdout does not report writing the go.mod")
	}
	mod, err := os.ReadFile(filepath.Join(target, "go.mod"))
	if err != nil {
		t.Fatalf("read materialized go.mod: %v", err)
	}
	if !strings.HasPrefix(string(mod), "module cli-app\n") {
		t.Errorf("go.mod module line is not %q, got %q", "module cli-app", firstLineOf(mod))
	}
}

// TestRunUpgradeDispatchesThroughCLI drives `saasctl upgrade` through the
// root dispatch -- the one place main.go and the upgrade command meet --
// against a real go.mod in a temp directory: exit 0 and the speed require
// rewritten to the target version, with the single-line require form
// exercised here (the block form is covered by internal/upgrade's own
// suite). The upgrade command's behavior matrix lives there; this test only
// proves the dispatch reaches it.
func TestRunUpgradeDispatchesThroughCLI(t *testing.T) {
	const version = "v0.9.0"
	path := filepath.Join(t.TempDir(), "go.mod")
	mod := "module cli-app\n\ngo 1.25.0\n\nrequire github.com/vislake/speed/go/authn v0.0.0-00010101000000-000000000000\n"
	if err := os.WriteFile(path, []byte(mod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	code, stdout, stderr := runCLI(t, []string{"upgrade", "--version", version, path})
	if code != 0 {
		t.Fatalf("run(upgrade) = %d, want 0; stderr:\n%s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("run(upgrade) wrote to stderr on success: %q", stderr)
	}
	if !strings.Contains(stdout, "Rewrote 1 github.com/vislake/speed/go/* require lines to "+version+" in "+path) {
		t.Errorf("stdout does not report the rewrite: %q", stdout)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rewritten go.mod: %v", err)
	}
	if want := "require github.com/vislake/speed/go/authn " + version + "\n"; !strings.Contains(string(got), want) {
		t.Errorf("rewritten go.mod lacks %q:\n%s", want, got)
	}
}

func firstLineOf(content []byte) string {
	s := string(content)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
