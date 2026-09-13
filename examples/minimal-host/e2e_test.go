package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The cases run the host as a real child process rather than calling into it.
// What this example has to prove is that a main assembled out of the module
// mechanism runs: the exit status, the two output streams and the reaction to
// a signal are all part of that, and none of them exists inside a test binary
// that rewrote os.Args on itself.

// hostBinary is the compiled program every case runs. It is built once, in
// TestMain, because building it is the slowest thing here by far.
var hostBinary string

// waitLimit bounds how long a case waits for the host to become ready or to
// leave. It is generous: exceeding it means the host hung, and the case says
// so with both streams attached rather than with a timeout of the test binary.
const waitLimit = 15 * time.Second

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "minimal-host")
	if err != nil {
		fmt.Fprintln(os.Stderr, "creating the build directory failed:", err)
		os.Exit(1)
	}
	hostBinary = filepath.Join(dir, "minimal-host")
	if out, err := exec.Command("go", "build", "-o", hostBinary, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building the host failed: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// outcome is what one run of the host produced.
type outcome struct {
	stdout string
	stderr string
	code   int
}

// runToCompletion runs the host and waits for it to leave on its own.
func runToCompletion(t *testing.T, env []string, args ...string) outcome {
	t.Helper()
	return runHost(t, env, args, false)
}

// runUntilReady runs the host, waits for the ready marker, then interrupts it
// and waits for it to leave.
func runUntilReady(t *testing.T, env []string, args ...string) outcome {
	t.Helper()
	return runHost(t, env, args, true)
}

// runHost drives one run. The environment is exactly what the case gives and
// never the ambient one: a MINIHOST variable on the machine running the tests
// would otherwise change what a case observes.
func runHost(t *testing.T, env []string, args []string, interrupt bool) outcome {
	t.Helper()
	cmd := exec.Command(hostBinary, args...)
	cmd.Env = append([]string{}, env...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("opening the host's stdout failed: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the host failed: %v", err)
	}

	// stdout is read as it arrives rather than collected at the end: the
	// interrupt may only be sent once the host says it is ready, and the
	// lines written after it are what the shutdown order is read from.
	var collected bytes.Buffer
	ready := make(chan struct{})
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		scanner := bufio.NewScanner(stdout)
		signalled := false
		for scanner.Scan() {
			collected.WriteString(scanner.Text())
			collected.WriteByte('\n')
			if !signalled && scanner.Text() == readyMarker {
				signalled = true
				close(ready)
			}
		}
	}()

	fail := func(format string, args ...any) {
		t.Helper()
		_ = cmd.Process.Kill()
		<-drained
		_ = cmd.Wait()
		t.Fatalf(format+"\nstdout:\n%s\nstderr:\n%s", append(args, collected.String(), stderr.String())...)
	}

	left := false
	if interrupt {
		select {
		case <-ready:
		case <-drained:
			left = true
		case <-time.After(waitLimit):
			fail("the host never printed %q within %s", readyMarker, waitLimit)
		}
		if !left {
			if err := cmd.Process.Signal(os.Interrupt); err != nil {
				fail("interrupting the host failed: %v", err)
			}
		}
	}

	select {
	case <-drained:
	case <-time.After(waitLimit):
		fail("the host did not leave within %s of the interrupt", waitLimit)
	}

	code := 0
	if err := cmd.Wait(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("waiting for the host failed: %v", err)
		}
		code = exit.ExitCode()
	}
	return outcome{stdout: collected.String(), stderr: stderr.String(), code: code}
}

// locator is the config locator of a file under testdata, in the shape the
// file transport serves: a URI with no host and an absolute path.
func locator(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("resolving %s failed: %v", name, err)
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}

// requireContains asserts that a stream carries a piece of text.
func requireContains(t *testing.T, stream, text, what string) {
	t.Helper()
	if !strings.Contains(stream, text) {
		t.Errorf("%s does not carry %q:\n%s", what, text, stream)
	}
}

// requireAbsent asserts that a stream carries no trace of a piece of text.
func requireAbsent(t *testing.T, stream, text, what string) {
	t.Helper()
	if strings.Contains(stream, text) {
		t.Errorf("%s carries %q and should not:\n%s", what, text, stream)
	}
}

// lineIndex gives the position of a line in the output, failing the case when
// it never appeared.
func lineIndex(t *testing.T, lines []string, want string) int {
	t.Helper()
	for i, line := range lines {
		if line == want {
			return i
		}
	}
	t.Fatalf("the host never printed %q:\n%s", want, strings.Join(lines, "\n"))
	return -1
}

// Demonstration one: the help output describes the command line the assembled
// modules declared, and asking for it is not a failure.

func TestHelpExitsZeroAndListsFlags(t *testing.T) {
	got := runToCompletion(t, nil, "--help")

	if got.code != 0 {
		t.Errorf("asking for help left with status %d, and it is not a failure", got.code)
	}
	requireContains(t, got.stdout, greeterGroup+":", "the help output")
	requireContains(t, got.stdout, "--salutation WORD", "the help output")
	requireContains(t, got.stdout, "--remote-addr HOST:PORT", "the help output")
	requireContains(t, got.stdout, "the word the built-in greeter opens with", "the help output")
	requireContains(t, got.stdout, `(default: "`+localDefaults.Salutation+`")`, "the help output")
	requireContains(t, got.stdout, "--config LOCATOR", "the help output")
	// Nothing was read: the help output does not depend on a config source
	// being reachable, so a locator pointing nowhere changes nothing.
	if got.stderr != "" {
		t.Errorf("asking for help wrote to stderr:\n%s", got.stderr)
	}
}

func TestHelpHidesSensitiveDefault(t *testing.T) {
	got := runToCompletion(t, nil, "--help")

	requireContains(t, got.stdout, "--remote-token TOKEN", "the help output")
	requireContains(t, got.stdout, "the credential the greeting service authenticates with",
		"the help output")
	requireAbsent(t, got.stdout, remoteDefaults.Token, "the help output")
}

func TestHelpIsAnsweredBeforeAnUnreachableSourceIsRead(t *testing.T) {
	got := runToCompletion(t, nil, "--config=file:///nowhere/minimal-host.yaml", "--help")

	if got.code != 0 {
		t.Errorf("asking for help left with status %d although the source is unreachable", got.code)
	}
	requireContains(t, got.stdout, "--salutation WORD", "the help output")
}

// Demonstration two: the primary config source and the environment reach the
// same input item, and the environment wins.

func TestPrimarySourceValueTakesEffect(t *testing.T) {
	got := runUntilReady(t, nil, "--config="+locator(t, "local.yaml"))

	if got.code != 0 {
		t.Errorf("the host left with status %d\nstderr:\n%s", got.code, got.stderr)
	}
	requireContains(t, got.stdout, appModuleName+": greeting: from-file, world", "stdout")
	requireContains(t, got.stdout, appModuleName+": endpoint: local", "stdout")
}

func TestFileThenEnvOverride(t *testing.T) {
	env := []string{"MINIHOST_GREETER__REMOTE__ADDR=from-env:9999"}
	got := runUntilReady(t, env, "--config="+locator(t, "remote.yaml"))

	if got.code != 0 {
		t.Errorf("the host left with status %d\nstderr:\n%s", got.code, got.stderr)
	}
	requireContains(t, got.stdout, appModuleName+": endpoint: from-env:9999", "stdout")
	requireAbsent(t, got.stdout, "from-file:1234", "stdout")
}

func TestLocatorFromEnvVar(t *testing.T) {
	env := []string{"MINIHOST_CONFIG=" + locator(t, "remote.yaml")}
	got := runUntilReady(t, env)

	if got.code != 0 {
		t.Errorf("the host left with status %d\nstderr:\n%s", got.code, got.stderr)
	}
	requireContains(t, got.stdout, appModuleName+": endpoint: from-file:1234", "stdout")
}

func TestUnknownPrefixedEnvIsDiagnosedNotFatal(t *testing.T) {
	env := []string{"MINIHOST_SERVICE_HOST=10.0.0.1"}
	got := runUntilReady(t, env)

	if got.code != 0 {
		t.Errorf("a variable under the prefix that no item reads left the host with status %d",
			got.code)
	}
	requireContains(t, got.stdout, readyMarker, "stdout")
	requireContains(t, got.stderr, "MINIHOST_SERVICE_HOST", "the startup diagnostics")
}

// Demonstration three: two providers of one capability, and the built-in one
// stands down as soon as the other is configured.

func TestLocalRunsWhenRemoteUnconfigured(t *testing.T) {
	got := runUntilReady(t, nil)

	if got.code != 0 {
		t.Errorf("the host left with status %d\nstderr:\n%s", got.code, got.stderr)
	}
	requireContains(t, got.stdout, appModuleName+": endpoint: local", "stdout")
	requireContains(t, got.stderr, `module "`+remoteModuleName+`" is not enabled`,
		"the startup diagnostics")
	requireContains(t, got.stderr, "no connection address is configured",
		"the startup diagnostics")
	requireAbsent(t, got.stderr, `module "`+localModuleName+`" is not enabled`,
		"the startup diagnostics")
}

func TestRemoteWinsAndLocalStandsDown(t *testing.T) {
	got := runUntilReady(t, nil, "--remote-addr=cli:7000")

	if got.code != 0 {
		t.Errorf("the host left with status %d\nstderr:\n%s", got.code, got.stderr)
	}
	requireContains(t, got.stdout, appModuleName+": endpoint: cli:7000", "stdout")
	requireContains(t, got.stderr, `module "`+localModuleName+`" is not enabled`,
		"the startup diagnostics")
	requireContains(t, got.stderr, "already has an explicitly enabled provider",
		"the startup diagnostics")
	requireContains(t, got.stderr, `"`+remoteModuleName+`"`, "the startup diagnostics")
	requireAbsent(t, got.stderr, `module "`+remoteModuleName+`" is not enabled`,
		"the startup diagnostics")
}

// Demonstration four: cancelling the context stops and closes every module in
// reverse dependency order.

func TestSigintTriggersReverseOrderShutdown(t *testing.T) {
	got := runUntilReady(t, nil)

	if got.code != 0 {
		t.Errorf("an interrupt is the ordinary way to stop, and the host left with status %d",
			got.code)
	}
	lines := strings.Split(strings.TrimRight(got.stdout, "\n"), "\n")
	appStop := lineIndex(t, lines, appModuleName+": stop")
	greeterStop := lineIndex(t, lines, localModuleName+": stop")
	appClose := lineIndex(t, lines, appModuleName+": close")
	greeterClose := lineIndex(t, lines, localModuleName+": close")

	// The application requires the greeter capability, so the greeter is
	// constructed first and released last, in both stages.
	if appStop > greeterStop {
		t.Errorf("the consumer stopped after its provider: app at %d, greeter at %d\n%s",
			appStop, greeterStop, got.stdout)
	}
	if appClose > greeterClose {
		t.Errorf("the consumer closed after its provider: app at %d, greeter at %d\n%s",
			appClose, greeterClose, got.stdout)
	}
	// Stop drains and Close releases, so nothing is released while another
	// module may still be draining.
	if max(appStop, greeterStop) > min(appClose, greeterClose) {
		t.Errorf("a module closed before every module had stopped:\n%s", got.stdout)
	}
}
