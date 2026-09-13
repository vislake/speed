package log

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// fileOutputs is a resolved output per path, in JSON so a test can read a
// record back line by line. Rotation and pruning are off: these tests are
// about the chain, not about what a destination does with the bytes.
func fileOutputs(paths ...string) []resolvedOutput {
	outputs := make([]resolvedOutput, 0, len(paths))
	for _, p := range paths {
		outputs = append(outputs, resolvedOutput{to: destFile, format: formatJSON, path: p})
	}
	return outputs
}

// newTestChain assembles a chain over the given files and releases its
// references when the test ends, so no reference outlives the test that took
// it.
func newTestChain(t *testing.T, level slog.Level, paths ...string) *chain {
	t.Helper()
	assembled, err := newChain(resolvedConfig{level: level, outputs: fileOutputs(paths...)})
	if err != nil {
		t.Fatalf("assembling a chain over %v failed: %v", paths, err)
	}
	t.Cleanup(assembled.release)
	return assembled
}

// branchesOf walks a chain down to its branches, asserting the layer order on
// the way: the level at the head, then redaction, then the fan-out.
func branchesOf(t *testing.T, assembled *chain) []slog.Handler {
	t.Helper()
	head, ok := assembled.handler.(*levelHandler)
	if !ok {
		t.Fatalf("the head of the chain is %T, want the level layer", assembled.handler)
	}
	redaction, ok := head.next.(*redactHandler)
	if !ok {
		t.Fatalf("below the level layer sits %T, want the redaction layer", head.next)
	}
	fanout, ok := redaction.next.(*fanoutHandler)
	if !ok {
		t.Fatalf("below the redaction layer sits %T, want the fan-out", redaction.next)
	}
	return fanout.branches
}

// TestDefaultConfigChainSharesTheBootstrapStdoutWriter pins the deduplication
// at its most common case: the configuration a host gets by writing no log
// section at all is exactly one output to standard output, and that output
// must join the writer the bootstrap chain already holds.
//
// The judgement is identity and reference count, not similar behaviour: two
// writers over fd 1 look identical until two records longer than PIPE_BUF
// interleave on the pipe a container's standard output is.
func TestDefaultConfigChainSharesTheBootstrapStdoutWriter(t *testing.T) {
	resolved, err := configDefaults.resolve()
	if err != nil {
		t.Fatalf("the prototype configuration does not resolve: %v", err)
	}

	assembled, err := newChain(resolved)
	if err != nil {
		t.Fatalf("assembling the default chain failed: %v", err)
	}
	t.Cleanup(assembled.release)

	if len(assembled.writers) != 1 {
		t.Fatalf("the default chain opened %d writers, want the one standard output", len(assembled.writers))
	}
	if assembled.writers[0] != bootstrapStdoutWriter {
		t.Errorf("the stdout branch writes through %p, want the bootstrap writer %p",
			assembled.writers[0], bootstrapStdoutWriter)
	}
	if got := refsFor("stdout"); got != 2 {
		t.Errorf("standard output has %d references, want the bootstrap one plus this assembly", got)
	}
}

// TestReleaseKeepsTheBootstrapReference pins that closing an assembly does not
// take the default logger away. The root package's reference is never given
// up, so the count never reaches zero and the writer is never marked closed.
func TestReleaseKeepsTheBootstrapReference(t *testing.T) {
	resolved, err := configDefaults.resolve()
	if err != nil {
		t.Fatalf("the prototype configuration does not resolve: %v", err)
	}
	assembled, err := newChain(resolved)
	if err != nil {
		t.Fatalf("assembling the default chain failed: %v", err)
	}

	assembled.release()

	if got := refsFor("stdout"); got != 1 {
		t.Errorf("standard output has %d references after the release, want the bootstrap one", got)
	}
	if !registered("stdout") {
		t.Fatal("releasing the assembly deregistered standard output")
	}
	bootstrapStdoutWriter.mu.Lock()
	closed := bootstrapStdoutWriter.closed
	bootstrapStdoutWriter.mu.Unlock()
	if closed {
		t.Fatal("releasing the assembly closed the bootstrap writer")
	}

	out := captureBootstrapStdout(t, func() { Default().Info("after the assembly closed") })
	if !strings.Contains(out, "after the assembly closed") {
		t.Errorf("the default logger stopped writing after an assembly was released: %q", out)
	}
}

// TestBranchHandlersDoNotFilterByLevel pins that the level is judged once, at
// the head, and that no branch carries a threshold of its own.
//
// The assertion has to reach the branch's own Enabled: slog never re-checks the
// level inside Handle, so a branch built with a level would filter nothing
// today and start filtering the moment the fan-out honoured its branches'
// Enabled — a chain with two places a record can be dropped, and the debug
// records a host asked for depending on which one the assembly set.
func TestBranchHandlersDoNotFilterByLevel(t *testing.T) {
	dir := t.TempDir()
	first, second := filepath.Join(dir, "first.log"), filepath.Join(dir, "second.log")
	assembled := newTestChain(t, slog.LevelDebug, first, second)

	ctx := context.Background()
	for i, branch := range branchesOf(t, assembled) {
		if !branch.Enabled(ctx, allLevels) {
			t.Errorf("branch %d filters by a level of its own", i)
		}
	}

	slog.New(assembled.handler).Debug("a debug record")

	for _, path := range []string{first, second} {
		if out := read(t, path); !strings.Contains(out, "a debug record") {
			t.Errorf("%s did not get the debug record: %q", filepath.Base(path), out)
		}
	}
}

// TestRecordIsRedactedOnceForManyOutputs pins the order of the layers:
// redaction above the fan-out, so a record is judged once however many outputs
// there are. A layer per branch would multiply the cost of every registered
// matcher by the number of destinations.
func TestRecordIsRedactedOnceForManyOutputs(t *testing.T) {
	isolateRules(t)
	var calls atomic.Int64
	processRedaction.AddPattern("counting", func(string) []Span {
		calls.Add(1)
		return nil
	})

	dir := t.TempDir()
	assembled := newTestChain(t, slog.LevelInfo,
		filepath.Join(dir, "a.log"), filepath.Join(dir, "b.log"), filepath.Join(dir, "c.log"))

	slog.New(assembled.handler).Info("three outputs", "value", "a string to judge")

	if got := calls.Load(); got != 1 {
		t.Errorf("the matcher ran %d times for one record over three outputs, want 1", got)
	}
}

// TestFormalChainLevelIsImmuneToBootstrapLevelVar pins that a chain's level is
// settled when it is assembled.
//
// A chain built over the package-level LevelVar passes every other level test
// here, and then a second module's Prepare — or this module's own, in a second
// assembly — silently re-filters a chain that is already running.
func TestFormalChainLevelIsImmuneToBootstrapLevelVar(t *testing.T) {
	keepBootstrapLevel(t)
	path := filepath.Join(t.TempDir(), "immune.log")
	assembled := newTestChain(t, slog.LevelInfo, path)

	bootstrapLevel.Set(slog.LevelDebug)

	lg := slog.New(assembled.handler)
	lg.Debug("must not be written")
	lg.Info("must be written")

	out := read(t, path)
	if strings.Contains(out, "must not be written") {
		t.Errorf("moving the bootstrap level re-filtered an assembled chain: %q", out)
	}
	if !strings.Contains(out, "must be written") {
		t.Errorf("the chain stopped writing records above its own level: %q", out)
	}
}

// TestTwoChainsFilterByTheirOwnConfiguredLevel pins the other side of the
// same rule: two assemblies coexist, each filtering by the level it was built
// with.
func TestTwoChainsFilterByTheirOwnConfiguredLevel(t *testing.T) {
	dir := t.TempDir()
	debugPath, warnPath := filepath.Join(dir, "debug.log"), filepath.Join(dir, "warn.log")
	debugChain := newTestChain(t, slog.LevelDebug, debugPath)
	warnChain := newTestChain(t, slog.LevelWarn, warnPath)

	slog.New(debugChain.handler).Info("an info record")
	slog.New(warnChain.handler).Info("an info record")

	if out := read(t, debugPath); !strings.Contains(out, "an info record") {
		t.Errorf("the chain configured at debug dropped an info record: %q", out)
	}
	if out := read(t, warnPath); strings.Contains(out, "an info record") {
		t.Errorf("the chain configured at warn wrote an info record: %q", out)
	}
}

// TestUnknownDestinationOrFormatFailsAssembly pins that a name outside a closed
// set fails the startup naming the sentinel, and that the failure leaves no
// writer behind: a leaked reference would have the next assembly of that
// destination report a parameter conflict against parameters nobody uses.
func TestUnknownDestinationOrFormatFailsAssembly(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.log")

	for _, tc := range []struct {
		name string
		bad  resolvedOutput
		want error
	}{
		{
			name: "format",
			bad:  resolvedOutput{to: destFile, format: "yaml", path: filepath.Join(dir, "bad.log")},
			want: ErrUnknownFormat,
		},
		{
			name: "destination",
			bad:  resolvedOutput{to: "syslog", format: formatJSON, path: filepath.Join(dir, "bad.log")},
			want: ErrUnknownDestination,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := resolvedConfig{
				level:   slog.LevelInfo,
				outputs: append(fileOutputs(good), tc.bad),
			}
			assembled, err := newChain(cfg)
			if err == nil {
				assembled.release()
				t.Fatalf("an output naming an unknown %s assembled without an error", tc.name)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("the assembly failed with %v, want %v", err, tc.want)
			}
			if registered(destinationID(fileOutputs(good)[0])) {
				t.Error("the output ahead of the bad one kept its writer after the failed assembly")
			}
		})
	}
}

// TestChainLayerOrderIsLevelThenRedactionThenFanout pins the order the layers
// sit in. Redaction below the fan-out would judge a record once per output, and
// the level below redaction would pay for judging records that are filtered out
// anyway.
func TestChainLayerOrderIsLevelThenRedactionThenFanout(t *testing.T) {
	dir := t.TempDir()
	assembled := newTestChain(t, slog.LevelInfo, filepath.Join(dir, "a.log"), filepath.Join(dir, "b.log"))

	branches := branchesOf(t, assembled)
	if len(branches) != 2 {
		t.Errorf("the fan-out carries %d branches, want one per output", len(branches))
	}
	for i, branch := range branches {
		if _, ok := branch.(*redactHandler); ok {
			t.Errorf("branch %d carries a redaction layer of its own", i)
		}
	}
}

// TestEmptyOutputListAssemblesAnEmptyFanout pins what an explicitly empty
// output list becomes: a chain with no branches, which writes nothing anywhere
// rather than falling back to a stream of its own.
func TestEmptyOutputListAssemblesAnEmptyFanout(t *testing.T) {
	assembled, err := newChain(resolvedConfig{level: slog.LevelInfo})
	if err != nil {
		t.Fatalf("assembling a chain with no outputs failed: %v", err)
	}
	t.Cleanup(assembled.release)

	if len(assembled.writers) != 0 {
		t.Errorf("a chain with no outputs opened %d writers", len(assembled.writers))
	}
	out := captureBootstrapStdout(t, func() { slog.New(assembled.handler).Error("nowhere") })
	if out != "" {
		t.Errorf("a chain with no outputs wrote to standard output: %q", out)
	}
}

// TestEmptyOutputListDisablesTheChainHead pins the short circuit an explicitly
// empty output list gets: the head reports the chain disabled, so a record is
// neither built nor judged.
//
// Whether there is anywhere to write is settled when the chain is assembled, so
// the head does not ask the fan-out at run time — that would add a call per
// record to every ordinary configuration to serve a configuration that writes
// nothing.
//
// Seeing no output does not distinguish a short circuit from a chain that
// simply has no destination, which is why the second assertion reaches the
// redaction layer: without a short circuit the record is built, judged against
// every registered matcher, and only then handed to nobody.
func TestEmptyOutputListDisablesTheChainHead(t *testing.T) {
	isolateRules(t)
	var calls atomic.Int64
	processRedaction.AddPattern("counting", func(string) []Span {
		calls.Add(1)
		return nil
	})

	assembled, err := newChain(resolvedConfig{level: slog.LevelDebug})
	if err != nil {
		t.Fatalf("assembling a chain with no outputs failed: %v", err)
	}
	t.Cleanup(assembled.release)

	head, ok := assembled.handler.(*levelHandler)
	if !ok {
		t.Fatalf("the head of the chain is %T, want the level layer", assembled.handler)
	}
	if head.Enabled(context.Background(), slog.LevelError) {
		t.Error("the head of a chain with no outputs reports the chain enabled")
	}

	slog.New(assembled.handler).Info("nowhere", "value", "a string to judge")

	if got := calls.Load(); got != 0 {
		t.Errorf("the redaction layer ran %d times for a chain with nowhere to write, want 0", got)
	}
}

// TestEmptyOutputListKeepsDerivedLoggersDisabled pins that the short circuit
// survives derivation, which is the only way the product takes a logger:
// Named binds the module name, and slog.Logger.With calls WithAttrs without
// consulting Enabled at all.
//
// It registers no matcher and builds its own chain: WithAttrs judges the
// attributes it binds as it binds them, so a counting matcher here would be
// called by the derivation itself and say nothing about the short circuit.
func TestEmptyOutputListKeepsDerivedLoggersDisabled(t *testing.T) {
	assembled, err := newChain(resolvedConfig{level: slog.LevelDebug})
	if err != nil {
		t.Fatalf("assembling a chain with no outputs failed: %v", err)
	}
	t.Cleanup(assembled.release)

	logger := slog.New(assembled.handler)
	for _, tc := range []struct {
		name    string
		derived *slog.Logger
	}{
		{name: "With", derived: logger.With("k", "v")},
		{name: "WithGroup", derived: logger.WithGroup("g")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.derived.Enabled(context.Background(), slog.LevelError) {
				t.Error("a logger derived from a chain with no outputs reports the chain enabled")
			}
		})
	}
}
