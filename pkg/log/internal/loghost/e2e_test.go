package main

import (
	"bytes"
	"context"
	"encoding/json"
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

// The cases run the host as a real child process. What they have to prove is
// what a real assembly does: which destination a record reached, what the
// startup wrote to standard error, and the exit status a second provider of
// the capability produces. None of the three exists inside a test binary, and
// a real assembly cannot be started in one at all — the configuration loader
// reads os.Args[1:], which go test has already filled with arguments of its
// own.

// hostBinary is the compiled host every case runs. It is built once, in
// TestMain, because building it is by far the slowest thing here.
var hostBinary string

// runLimit bounds one run of the host. It is generous: exceeding it means the
// host hung, and the case says so with both streams attached.
const runLimit = 60 * time.Second

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "loghost")
	if err != nil {
		fmt.Fprintln(os.Stderr, "creating the build directory failed:", err)
		os.Exit(1)
	}
	hostBinary = filepath.Join(dir, "loghost")
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

// logConfig is the configuration document a case writes, in the shape the log
// module's schema declares. It is marshalled rather than written as text so a
// path never has to be escaped by hand.
type logConfig struct {
	Log logSection `json:"log"`
}

type logSection struct {
	Outputs []logOutput `json:"outputs"`
}

type logOutput struct {
	To     string `json:"to"`
	Format string `json:"format"`
	Path   string `json:"path,omitempty"`
}

// writeConfig writes a config document and returns the locator the file
// transport serves it under.
func writeConfig(t *testing.T, doc any) string {
	t.Helper()
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshalling the config document failed: %v", err)
	}
	path := filepath.Join(t.TempDir(), "loghost.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("writing the config document failed: %v", err)
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}

// runHost runs one scenario to completion.
//
// The environment is exactly what the case gives and never the ambient one: a
// LOGHOST variable on the machine running the tests would otherwise change
// what a case observes.
func runHost(t *testing.T, scenarioName string, args ...string) outcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runLimit)
	defer cancel()

	cmd := exec.CommandContext(ctx, hostBinary, args...)
	cmd.Env = []string{scenarioEnv + "=" + scenarioName}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	code := 0
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("running the host failed: %v\nstdout:\n%s\nstderr:\n%s",
				err, stdout.String(), stderr.String())
		}
		code = exit.ExitCode()
	}
	return outcome{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// requireSuccess asserts that the run left cleanly, printing both streams when
// it did not.
func requireSuccess(t *testing.T, got outcome) {
	t.Helper()
	if got.code != 0 {
		t.Fatalf("the host left with status %d\nstdout:\n%s\nstderr:\n%s",
			got.code, got.stdout, got.stderr)
	}
}

// countOccurrences reports how many lines of a stream carry a piece of text.
func countOccurrences(stream, text string) int {
	n := 0
	for line := range strings.SplitSeq(strings.TrimRight(stream, "\n"), "\n") {
		if strings.Contains(line, text) {
			n++
		}
	}
	return n
}

// readFileConfigured reads back a destination file a case configured, failing
// when the assembly never created it.
func readFileConfigured(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path) //nolint:gosec // the path is the one this case just handed the host.
	if err != nil {
		t.Fatalf("reading the configured destination failed: %v", err)
	}
	return string(body)
}

// Scenario one: no log section in the config source at all.

func TestAbsentLogSectionYieldsOneStdoutTextOutputAtInfo(t *testing.T) {
	// The document is valid and carries no log section, which is a stronger
	// statement than having no config source: the section is absent, not
	// the file. What fills it in is the carrier struct the module declared.
	locator := writeConfig(t, struct{}{})
	got := runHost(t, caseDefault, "--config="+locator)
	requireSuccess(t, got)

	if n := countOccurrences(got.stdout, "msg="+infoMessage); n != 1 {
		t.Errorf("the Info record appears %d times on stdout, want 1:\n%s", n, got.stdout)
	}
	// Text, not JSON: the default format is the one the carrier struct
	// carries, and a JSON record would render the message as "msg":"...".
	if strings.Contains(got.stdout, `"msg"`) {
		t.Errorf("stdout carries JSON records although the default format is text:\n%s", got.stdout)
	}
	if !strings.Contains(got.stdout, "level=INFO") {
		t.Errorf("stdout carries no text-rendered level:\n%s", got.stdout)
	}
	// The default level is info, so the Debug record of the same run is not
	// written anywhere.
	if strings.Contains(got.stdout, debugMessage) {
		t.Errorf("the Debug record reached stdout although the default level is info:\n%s", got.stdout)
	}
	// The rules the application registered in its own New govern the
	// attribute it bound afterwards, on the configured chain of a real
	// assembly.
	if strings.Contains(got.stdout, plaintextValue) {
		t.Errorf("the sensitive value reached stdout in plaintext:\n%s", got.stdout)
	}
	if !strings.Contains(got.stdout, sensitiveKey+"=[REDACTED]") {
		t.Errorf("stdout carries no masked value under %q:\n%s", sensitiveKey, got.stdout)
	}
}

// Scenario two: the only configured destination is a file, and a call site
// with no logger in its context logs anyway.

// fallbackRun assembles a file-only configuration and runs the fallback
// scenario, returning the outcome and the path of the configured file.
func fallbackRun(t *testing.T) (outcome, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.log")
	locator := writeConfig(t, logConfig{Log: logSection{Outputs: []logOutput{
		{To: "file", Format: "json", Path: path},
	}}})
	got := runHost(t, caseFallback, "--config="+locator)
	requireSuccess(t, got)
	return got, path
}

func TestFallbackDoesNotReachTheConfiguredFile(t *testing.T) {
	got, path := fallbackRun(t)

	// The fallback goes to the process logger, which writes to standard
	// output and knows nothing about this assembly. An implementation that
	// fell back to "the chain most recently assembled" would put the record
	// in the file — the most plausible-looking way to get this wrong.
	if n := countOccurrences(readFileConfigured(t, path), orphanMessage); n != 0 {
		t.Errorf("the record written with no logger in context reached the configured file %d times\nstdout:\n%s",
			n, got.stdout)
	}
}

func TestFallbackAppearsOnStdout(t *testing.T) {
	got, _ := fallbackRun(t)

	// Together with the case above this is the whole answer to where the
	// record went: one says it did not reach the configured destination,
	// this one says where it did go.
	if n := countOccurrences(got.stdout, orphanMessage); n != 1 {
		t.Errorf("the record written with no logger in context appears %d times on stdout, want 1:\n%s",
			n, got.stdout)
	}
}

func TestNamedLoggerReachesTheConfiguredFile(t *testing.T) {
	got, path := fallbackRun(t)
	body := readFileConfigured(t, path)

	// The positive control. Without it the two cases above could both pass
	// because the chain was never connected at all.
	if n := countOccurrences(body, injectedMessage); n != 1 {
		t.Errorf("the injected logger's record appears %d times in the configured file, want 1:\n%s",
			n, body)
	}
	if !strings.Contains(body, `"module":"`+appModuleName+`"`) {
		t.Errorf("the configured file carries no record from the named logger:\n%s", body)
	}
	if n := countOccurrences(got.stdout, injectedMessage); n != 0 {
		t.Errorf("the injected logger's record also reached stdout %d times, want 0:\n%s",
			n, got.stdout)
	}
}

// Scenario three: an output list that is there and empty.

func TestExplicitEmptyOutputsWritesOneNotice(t *testing.T) {
	locator := writeConfig(t, logConfig{Log: logSection{Outputs: []logOutput{}}})
	got := runHost(t, caseEmptyOutputs, "--config="+locator)
	requireSuccess(t, got)

	// An empty list is legal and means no records are written anywhere.
	// Said out loud once, because a logging module producing nothing is
	// indistinguishable from a broken one when seen from outside.
	if n := countOccurrences(got.stderr, "log: "); n != 1 {
		t.Errorf("the startup wrote %d notices from the log module, want exactly 1:\n%s",
			n, got.stderr)
	}
	if strings.Contains(got.stdout, silencedMessage) {
		t.Errorf("a record reached stdout although the output list is empty:\n%s", got.stdout)
	}
	// The distinction this case exists for: an absent outputs key keeps the
	// default output and writes no notice, and only the real decoding path
	// can tell the two apart.
	if strings.Contains(got.stderr, silencedMessage) {
		t.Errorf("a record reached stderr although the output list is empty:\n%s", got.stderr)
	}
}

// Scenario four: a second provider of the logging capability that stays
// enabled.

func TestSecondEnabledLoggerProviderFailsTheStartup(t *testing.T) {
	got := runHost(t, caseRivalProvider)

	if got.code == 0 {
		t.Fatalf("the startup succeeded with two enabled providers of the logging capability\nstdout:\n%s\nstderr:\n%s",
			got.stdout, got.stderr)
	}
	// errors.Is does not survive the process boundary, so the host judged
	// the error itself and printed a fixed word.
	if !strings.Contains(got.stdout, exclusiveMarker) {
		t.Errorf("the startup failed for another reason than two enabled providers\nstdout:\n%s\nstderr:\n%s",
			got.stdout, got.stderr)
	}
	// This is the only end-to-end evidence of the stance: a module claiming
	// exclusivity and stating StateAuto would have stood down here, and the
	// run would have succeeded with the rival's logger in place.
	if !strings.Contains(got.stderr, rivalModuleName) {
		t.Errorf("the failure does not name the rival provider:\n%s", got.stderr)
	}
}

// Scenario five: the process logger and the configured chain write to the same
// file descriptor at the same time.

func TestConcurrentDefaultAndConfiguredWritesDoNotInterleave(t *testing.T) {
	locator := writeConfig(t, logConfig{Log: logSection{Outputs: []logOutput{
		{To: "stdout", Format: "json"},
	}}})
	got := runHost(t, caseConcurrent, "--config="+locator)
	requireSuccess(t, got)

	lines := strings.Split(strings.TrimRight(got.stdout, "\n"), "\n")
	if len(lines) != 2*concurrentRecords {
		t.Errorf("stdout carries %d lines, want %d", len(lines), 2*concurrentRecords)
	}
	payload := strings.Repeat("x", payloadSize)
	for i, line := range lines {
		if strings.HasPrefix(line, "{") {
			var record struct {
				Msg     string `json:"msg"`
				Payload string `json:"payload"`
			}
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatalf("line %d is not a whole JSON record: %v\n%.200s", i, err, line)
			}
			if record.Msg != chainMessage || record.Payload != payload {
				t.Fatalf("line %d is a JSON record with the wrong content: msg=%q payload length %d",
					i, record.Msg, len(record.Payload))
			}
			continue
		}
		// The other writer is the process logger, which renders text.
		if !strings.HasPrefix(line, "time=") || !strings.HasSuffix(line, "payload="+payload) {
			t.Fatalf("line %d is not a whole text record:\n%.200s", i, line)
		}
		if !strings.Contains(line, "msg="+bootstrapMessage) {
			t.Fatalf("line %d is a text record with the wrong content:\n%.200s", i, line)
		}
	}
}
