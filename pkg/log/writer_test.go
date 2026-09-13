package log

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// recordingSink stands in for a destination in the tests that are about the
// writer rather than about where the bytes end up.
type recordingSink struct {
	// err, when set, is what every write fails with.
	err error
	// taken is what reached the sink.
	taken []string
	// started counts the calls to startBackground.
	started int
	// stopped counts the calls to stopBackground.
	stopped int
	// released counts the calls to release.
	released int
}

func (s *recordingSink) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	s.taken = append(s.taken, string(p))
	return len(p), nil
}

func (s *recordingSink) startBackground(sync.Locker) { s.started++ }
func (s *recordingSink) stopBackground()             { s.stopped++ }
func (s *recordingSink) release() error              { s.released++; return nil }

// captureStderr collects what this package writes to os.Stderr while fn runs.
// Reporting goes there directly, so the test swaps the stream rather than
// reaching for a hook in the product code.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating a pipe failed: %v", err)
	}
	original := os.Stderr
	os.Stderr = w
	collected := make(chan string, 1)
	go func() {
		text, _ := io.ReadAll(r)
		collected <- string(text)
	}()

	fn()

	os.Stderr = original
	w.Close()
	text := <-collected
	r.Close()
	return text
}

// refsFor reads a destination's reference count, 0 when it is not registered.
func refsFor(id string) int {
	rootMu.Lock()
	defer rootMu.Unlock()

	entry, ok := destinationWriters[id]
	if !ok {
		return 0
	}
	return entry.refs
}

// registered reports whether a destination has an entry at all.
func registered(id string) bool {
	rootMu.Lock()
	defer rootMu.Unlock()

	_, ok := destinationWriters[id]
	return ok
}

// openOne takes the writer of a single output and releases it when the test
// ends, so that no test leaves a reference behind for the next one.
func openOne(t *testing.T, o resolvedOutput) *destWriter {
	t.Helper()
	writers, release, err := openWriters([]resolvedOutput{o})
	if err != nil {
		t.Fatalf("opening the writer of %+v failed: %v", o, err)
	}
	t.Cleanup(release)
	return writers[0]
}

// fileOutput is a file output with the rotation and pruning switched off,
// which is what the tests about the registration want: they are about which
// writer an output gets, not about what it does with the bytes.
func fileOutput(path string) resolvedOutput {
	return resolvedOutput{to: destFile, format: formatJSON, path: path}
}

// TestBootstrapStdoutWriterIsRegisteredAtInit pins that the root package takes
// the standard output writer while the package initialises, and holds exactly
// one reference to it.
//
// That reference is what makes fd 1 carry a single writer: the bootstrap chain
// and a configured stdout output share it, and no Close can take it away,
// because the count never reaches zero.
func TestBootstrapStdoutWriterIsRegisteredAtInit(t *testing.T) {
	id := destinationID(resolvedOutput{to: destStdout})
	if !registered(id) {
		t.Fatalf("standard output is not registered after package initialisation")
	}
	if got := refsFor(id); got != 1 {
		t.Errorf("standard output has %d references, want the root package's single one "+
			"(an assembly that failed to release its own would also show up here)", got)
	}
	if bootstrapStdoutWriter == nil {
		t.Fatal("the root package holds no bootstrap writer")
	}
	if _, ok := bootstrapStdoutWriter.sink.(streamSink); !ok {
		t.Errorf("the bootstrap writer's sink is %T, want the standard output stream", bootstrapStdoutWriter.sink)
	}
}

// TestDefaultConfigOpensTheBootstrapStdoutWriter is the structural half of
// "one writer per destination": the configuration a host gets by writing no log
// section at all is one output to standard output, and opening it hands back
// the very object the bootstrap chain writes through.
//
// The assertion is object identity and the reference count, not behaviour: one
// writer and two writers produce the same output in a test that simply writes
// a few records. The interleaving two of them cause only shows up on a pipe,
// under concurrency, for a record longer than PIPE_BUF — which is to say it
// would pass every time while the defect was there.
func TestDefaultConfigOpensTheBootstrapStdoutWriter(t *testing.T) {
	cfg, err := configDefaults.resolve()
	if err != nil {
		t.Fatalf("the configuration a host gets without a log section does not resolve: %v", err)
	}
	if len(cfg.outputs) != 1 || cfg.outputs[0].to != destStdout {
		t.Fatalf("the default configuration is %+v, want exactly one output to standard output", cfg.outputs)
	}

	id := destinationID(cfg.outputs[0])
	before := refsFor(id)
	writers, release, err := openWriters(cfg.outputs)
	if err != nil {
		t.Fatalf("opening the default configuration's outputs failed: %v", err)
	}
	if writers[0] != bootstrapStdoutWriter {
		t.Errorf("the default configuration opened its own writer on standard output (%p); "+
			"the bootstrap chain holds %p, and fd 1 now carries two writers and two locks",
			writers[0], bootstrapStdoutWriter)
	}
	if got := refsFor(id); got != before+1 {
		t.Errorf("standard output has %d references after the assembly, want %d", got, before+1)
	}

	// Releasing the assembly leaves the root package's reference, so
	// Default() goes on writing where it always did.
	release()
	if got := refsFor(id); got != before {
		t.Errorf("standard output has %d references after the release, want %d", got, before)
	}
	if !registered(id) {
		t.Fatal("releasing the assembly deregistered standard output, taking Default() with it")
	}
	if bootstrapStdoutWriter.closed {
		t.Error("releasing the assembly closed the bootstrap writer, so Default() now drops its records")
	}
}

// TestTwoOutputsToOneFileShareOneWriter pins the identity of a file
// destination: a relative path, an absolute one and a symbolic link pointing at
// the file all name one destination, and one destination has one writer.
//
// Two writers over one file would each rotate and each prune, deleting the
// other's archives.
func TestTwoOutputsToOneFileShareOneWriter(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	relative := openOne(t, fileOutput("app.log"))
	link := filepath.Join(dir, "link.log")
	if err := os.Symlink(filepath.Join(dir, "app.log"), link); err != nil {
		t.Fatalf("creating the symbolic link failed: %v", err)
	}
	absolute := openOne(t, fileOutput(filepath.Join(dir, "app.log")))
	through := openOne(t, fileOutput(link))

	if absolute != relative {
		t.Errorf("the absolute spelling opened a second writer (%p) over the file %p already has", absolute, relative)
	}
	if through != relative {
		t.Errorf("the symbolic link opened a second writer (%p) over the file %p already has", through, relative)
	}
	if got := refsFor(relative.id); got != 3 {
		t.Errorf("the file destination has %d references, want one per output, so 3", got)
	}
}

// TestStdoutDedupIgnoresUnusedFileParams pins that the parameter comparison
// covers what the destination uses. A stream uses no rotation parameter, so two
// stdout outputs that disagree on one still share a writer rather than failing
// the startup over a field neither of them reads.
func TestStdoutDedupIgnoresUnusedFileParams(t *testing.T) {
	first := openOne(t, resolvedOutput{to: destStdout, format: formatText, maxSizeMB: 100, maxFiles: 10})
	second := openOne(t, resolvedOutput{to: destStdout, format: formatJSON, maxSizeMB: 5, maxFiles: 1})

	if first != second || first != bootstrapStdoutWriter {
		t.Errorf("two stdout outputs got the writers %p and %p, want both to be the bootstrap writer %p",
			first, second, bootstrapStdoutWriter)
	}
	if got := refsFor(first.id); got != 3 {
		t.Errorf("standard output has %d references, want the root package's one plus one per output, so 3", got)
	}
}

// TestConflictingFileParamsOnOneDestination pins that one writer has one set of
// parameters: a second output over the same file with different rotation
// parameters fails the startup instead of quietly inheriting the first set.
func TestConflictingFileParamsOnOneDestination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	openOne(t, resolvedOutput{to: destFile, format: formatJSON, path: path, maxSizeMB: 100, maxFiles: 10, maxAgeDays: 30})

	_, _, err := openWriters([]resolvedOutput{
		{to: destFile, format: formatText, path: path, maxSizeMB: 5, maxFiles: 10, maxAgeDays: 30},
	})
	if !errors.Is(err, ErrConflictingOutput) {
		t.Fatalf("a second output with different parameters gave %v, want ErrConflictingOutput", err)
	}
	if !strings.Contains(err.Error(), resolvePath(path)) {
		t.Errorf("the error %q does not name the destination %s", err, resolvePath(path))
	}
	if !strings.Contains(err.Error(), "max-size-mb") {
		t.Errorf("the error %q does not name the field the two outputs disagree on", err)
	}
	if strings.Contains(err.Error(), "max-files") {
		t.Errorf("the error %q names max-files, which the two outputs agree on", err)
	}
}

// TestOpenFailureIsUnavailableAndLeavesNoEntry pins the rollback of a failed
// assembly. The references taken for the outputs ahead of the failing one are
// given back, and the destination that could not be opened leaves no entry: a
// leak there would have the next assembly of the same destination report
// ErrConflictingOutput over parameters nobody holds.
func TestOpenFailureIsUnavailableAndLeavesNoEntry(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-directory", "app.log")
	stdoutID := destinationID(resolvedOutput{to: destStdout})
	before := refsFor(stdoutID)

	_, _, err := openWriters([]resolvedOutput{
		{to: destStdout, format: formatText},
		fileOutput(missing),
	})
	if !errors.Is(err, ErrOutputUnavailable) {
		t.Fatalf("an output under a directory that does not exist gave %v, want ErrOutputUnavailable", err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("the error %q does not name the path that could not be opened", err)
	}
	// The cause travels with the class. ErrOutputUnavailable covers a missing
	// directory, a mode that forbids the write and a path already taken, and
	// the host's remedy differs for each; flattening the error underneath into
	// text would leave it reading the message to tell them apart.
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the error %q does not carry fs.ErrNotExist, so the host cannot tell a missing directory from a permission denial", err)
	}
	if got := refsFor(stdoutID); got != before {
		t.Errorf("standard output has %d references after the failed assembly, want the %d it had before it",
			got, before)
	}
	if registered(destinationID(fileOutput(missing))) {
		t.Error("the destination that could not be opened is registered, so the next assembly will compare parameters against it")
	}
}

// TestWriterCanBeReopenedWithNewParamsAfterRelease pins the other side of the
// parameter comparison: its scope is the writer's lifetime. Once the last
// reference is gone and the writer is deregistered, another assembly opens the
// same file with parameters of its own.
func TestWriterCanBeReopenedWithNewParamsAfterRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	first := resolvedOutput{to: destFile, format: formatJSON, path: path, maxSizeMB: 100, maxFiles: 10, maxAgeDays: 30}
	writers, release, err := openWriters([]resolvedOutput{first})
	if err != nil {
		t.Fatalf("opening the file destination failed: %v", err)
	}
	id := writers[0].id
	release()
	if registered(id) {
		t.Fatal("the file destination is still registered after its last reference went")
	}

	second := resolvedOutput{to: destFile, format: formatJSON, path: path, maxSizeMB: 5, maxFiles: 2, maxAgeDays: 1}
	reopened, release, err := openWriters([]resolvedOutput{second})
	if err != nil {
		t.Fatalf("reopening the file with different parameters gave %v, want it to succeed", err)
	}
	t.Cleanup(release)

	rf, ok := reopened[0].sink.(*rotatingFile)
	if !ok {
		t.Fatalf("the file destination's sink is %T, want a rotating file", reopened[0].sink)
	}
	if rf.maxSize != int64(5)<<20 || rf.maxFiles != 2 || rf.maxAgeDays != 1 {
		t.Errorf("the reopened writer runs on %d bytes / %d files / %d days, want the second output's parameters",
			rf.maxSize, rf.maxFiles, rf.maxAgeDays)
	}
}

// TestClosedWriterDropsAndCounts pins that a record arriving after the close is
// dropped rather than written. The descriptor may already have been reused by
// another open in the process, and the bytes would land in an unrelated file.
func TestClosedWriterDropsAndCounts(t *testing.T) {
	sink := &recordingSink{}
	w := &destWriter{id: "test", sink: sink}

	captureStderr(t, func() { w.close() })
	if sink.stopped != 1 {
		t.Errorf("closing called stopBackground %d times, want once", sink.stopped)
	}
	if sink.released != 1 {
		t.Errorf("closing called release %d times, want once", sink.released)
	}

	captureStderr(t, func() {
		for range 3 {
			if _, err := w.Write([]byte("a record\n")); !errors.Is(err, errWriterClosed) {
				t.Errorf("writing to a closed destination gave %v, want it to report the closed writer", err)
			}
		}
	})
	if len(sink.taken) != 0 {
		t.Errorf("the closed destination took %d records, want none to reach the descriptor", len(sink.taken))
	}
	if w.failures != 3 {
		t.Errorf("the writer counted %d failures, want the 3 records it dropped", w.failures)
	}
}

// TestFailureIsReportedOnceUntilRecovery pins the reporting rule: one line for
// the first of a run of failures, and one when a write gets through again. A
// full disk fails every record, and a line each would flood the stream an
// operator reads at that very moment.
func TestFailureIsReportedOnceUntilRecovery(t *testing.T) {
	sink := &recordingSink{err: errors.New("no space left on device")}
	w := &destWriter{id: "file:/var/log/app.log", sink: sink}

	failing := captureStderr(t, func() {
		for range 5 {
			//nolint:errcheck // the failure is the subject of the test.
			w.Write([]byte("a record\n"))
		}
	})
	if got := strings.Count(failing, "\n"); got != 1 {
		t.Errorf("five consecutive failures wrote %d lines to standard error, want one:\n%s", got, failing)
	}
	if !strings.Contains(failing, w.id) || !strings.Contains(failing, "no space left on device") {
		t.Errorf("the report %q names neither the destination nor the reason", failing)
	}

	sink.err = nil
	recovered := captureStderr(t, func() {
		if _, err := w.Write([]byte("a record\n")); err != nil {
			t.Fatalf("writing after the fault cleared failed: %v", err)
		}
	})
	if got := strings.Count(recovered, "\n"); got != 1 {
		t.Errorf("the recovery wrote %d lines to standard error, want one:\n%s", got, recovered)
	}

	quiet := captureStderr(t, func() {
		if _, err := w.Write([]byte("a record\n")); err != nil {
			t.Fatalf("writing after the recovery failed: %v", err)
		}
	})
	if quiet != "" {
		t.Errorf("a write after the recovery reported %q, want nothing", quiet)
	}
	if w.failures != 0 {
		t.Errorf("the failure count is %d after a write got through, want the run to have ended", w.failures)
	}
}

// TestStderrDestinationFailureDoesNotRecurse pins that the writer of the
// standard error destination stays quiet when it fails: the report would travel
// the way the write that just failed did.
func TestStderrDestinationFailureDoesNotRecurse(t *testing.T) {
	sink := &recordingSink{err: errors.New("broken pipe")}
	w := &destWriter{id: "stderr", silent: true, sink: sink}

	reported := captureStderr(t, func() {
		for range 3 {
			//nolint:errcheck // the failure is the subject of the test.
			w.Write([]byte("a record\n"))
		}
	})
	if reported != "" {
		t.Errorf("the standard error destination reported its own failure: %q", reported)
	}
	if w.failures != 3 {
		t.Errorf("the writer counted %d failures, want 3; staying quiet is not the same as not counting", w.failures)
	}
}

// TestStderrDestinationGetsItsOwnWriter pins that standard error is a
// destination of its own: it does not share the bootstrap writer over fd 1.
func TestStderrDestinationGetsItsOwnWriter(t *testing.T) {
	w := openOne(t, resolvedOutput{to: destStderr, format: formatText})
	if w == bootstrapStdoutWriter {
		t.Fatal("the standard error output got the standard output writer")
	}
	if !w.silent {
		t.Error("the standard error writer reports its own failures, which travel the way the failed write did")
	}
}

// TestReleaseIsIdempotent pins that a second release does not take another
// assembly's reference away. Close runs on the rollback path as well as on the
// shutdown one, so it can be called twice over one assembly.
func TestReleaseIsIdempotent(t *testing.T) {
	id := destinationID(resolvedOutput{to: destStdout})
	before := refsFor(id)
	_, release, err := openWriters([]resolvedOutput{{to: destStdout, format: formatText}})
	if err != nil {
		t.Fatalf("opening standard output failed: %v", err)
	}
	release()
	release()
	if got := refsFor(id); got != before {
		t.Errorf("standard output has %d references after a doubled release, want %d", got, before)
	}
}

// lockProbeSink records whether the writer's lock was free when the writer
// stopped its background work.
type lockProbeSink struct {
	recordingSink
	// mu is the writer's own lock.
	mu *sync.Mutex
	// lockWasFree says the lock was not held at the moment the background
	// work was stopped.
	lockWasFree bool
}

func (s *lockProbeSink) stopBackground() {
	s.stopped++
	if s.mu.TryLock() {
		s.lockWasFree = true
		s.mu.Unlock()
	}
}

// TestCloseStopsBackgroundBeforeTakingTheLock pins the order inside the close:
// the background work is stopped first, and only then is the lock taken.
//
// A file destination's background prune takes that same lock for each pass, so
// waiting for it from under the lock deadlocks whenever a pass happens to be
// in flight. That is a window rather than a certainty, which is why this is
// asserted structurally rather than by hoping a shutdown hangs.
func TestCloseStopsBackgroundBeforeTakingTheLock(t *testing.T) {
	sink := &lockProbeSink{}
	w := &destWriter{id: "file:/var/log/app.log", sink: sink}
	sink.mu = &w.mu

	captureStderr(t, func() { w.close() })

	if sink.stopped != 1 {
		t.Fatalf("closing called stopBackground %d times, want once", sink.stopped)
	}
	if !sink.lockWasFree {
		t.Error("the close held the destination's lock while it stopped the background work, " +
			"which deadlocks against a prune pass waiting for that lock")
	}
}
