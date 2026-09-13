package log

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// A destination has exactly one writer in the process, and the writers are
// registered here under the destination's identity with a reference count.
//
// Without the deduplication every output would hold a lock of its own over one
// file descriptor. The standard library's handlers each carry a mutex that
// only covers their own Write, and a single Write is not universally atomic: on
// a pipe it is atomic only up to PIPE_BUF, and a container's standard output is
// a pipe, so two writers interleave a JSON record longer than that. Two
// independent rotators over one file would also rotate and prune against each
// other.
//
// The scope of the deduplication is the process rather than one assembly:
// registries built by separate tests can name the same destination, and the
// interleaving would come back through the other registry.
//
// destinationWriters is guarded by rootMu, the root package's mutex over its
// process-level state. The lock order is rootMu first and a destination's own
// lock second; nothing takes them the other way round, because neither the
// write path nor the background prune touches rootMu.
var destinationWriters = map[string]*writerEntry{}

// writerEntry is one destination's registration: its writer, how many
// references are outstanding, and the parameters it was opened with.
type writerEntry struct {
	w      *destWriter
	refs   int
	params fileParams
}

// errWriterClosed is what a write gets once the destination has been closed.
// It is not one of this module's sentinels: a log call cannot see the return of
// Handle at all, so the value is only ever the reason text of a report line.
var errWriterClosed = errors.New("log: the writer of this destination is closed")

// sink is where a destination writer puts the bytes. Implementations carry no
// lock of their own: every method below runs under the writer's lock, which is
// also the lock the file sink's rotation and pruning run under.
type sink interface {
	io.Writer
	// startBackground starts whatever the sink runs on its own schedule,
	// taking mu for each pass. It is called once, before the writer is
	// handed out.
	startBackground(mu sync.Locker)
	// stopBackground stops that work and waits for it to return. The
	// writer's lock is deliberately not held: the background pass takes that
	// same lock, so waiting from under it would deadlock.
	stopBackground()
	// release frees the underlying resources, under the writer's lock and
	// after the writer has been marked closed.
	release() error
}

// destWriter is the one writer of one destination: the lock the writes, the
// rotation and the pruning of that destination share, plus the closed state and
// the failure counting the reporting rule needs somewhere to live.
//
// Standard output and standard error get one of these too rather than the bare
// *os.File. They are destinations like any other, and the failure state has to
// have a home for them as well.
type destWriter struct {
	// id is the destination's identity, both the key in the registration and
	// what a report line names.
	id string
	// silent marks the writer of the standard error destination. A report
	// about it would travel the same way the write that just failed did, so
	// the reporting does not recurse into it.
	silent bool

	mu sync.Mutex
	// closed says the writer has been released. A record arriving afterwards
	// is dropped rather than written: the descriptor may already have been
	// reused by another open in the process, and the bytes would land in an
	// unrelated file.
	closed bool
	// failures counts the writes that did not reach the destination since
	// the last one that did, records dropped after the close included.
	failures int
	// reported says a line has already been written for the current run of
	// failures. A full disk fails every record, and a line each would flood
	// the very stream an operator reads at that moment.
	reported bool
	sink     sink
}

// Write puts one complete record into the destination. slog's handlers each
// assemble a whole line in an internal buffer and write it once, so a call here
// is one record, which is what lets the file sink decide about rotation on the
// call boundary.
func (w *destWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		w.noteFailure(errWriterClosed)
		return 0, errWriterClosed
	}
	n, err := w.sink.Write(p)
	if err != nil {
		w.noteFailure(err)
		return n, err
	}
	w.noteSuccess()
	return n, nil
}

// noteFailure counts a record that did not reach the destination and reports
// the first of a run. The caller holds the lock.
func (w *destWriter) noteFailure(err error) {
	w.failures++
	if w.reported || w.silent {
		return
	}
	w.reported = true
	report("destination %s did not take a record: %v; further failures stay silent until one write succeeds", w.id, err)
}

// noteSuccess closes a run of failures and reports the recovery. The caller
// holds the lock.
func (w *destWriter) noteSuccess() {
	if w.reported {
		report("destination %s took a record again, after %d that it dropped", w.id, w.failures)
		w.reported = false
	}
	w.failures = 0
}

// close marks the writer closed and releases its resources. Closing is not a
// barrier: a logger taken through Named is bound straight onto the chain, and a
// goroutine part-way through a write is under no synchronisation with the
// shutdown. Marking first and releasing second is what keeps those records off
// a descriptor that has already been handed back.
func (w *destWriter) close() {
	w.sink.stopBackground()

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return
	}
	w.closed = true
	if err := w.sink.release(); err != nil {
		report("destination %s did not close cleanly: %v", w.id, err)
	}
}

// report writes one line to os.Stderr.
//
// Reporting is fixed there and does not go through this module's writers: a
// writer that is failing has no way to carry the news of its own failure, and
// taking its lock could block on the very fault being reported. The cost is
// that a configured stderr destination has two parties on fd 2, which stays
// atomic on a pipe for lines this short.
func report(format string, args ...any) {
	//nolint:errcheck // os.Stderr is the report sink itself, so a failed write
	// has nowhere left to be reported.
	fmt.Fprintf(os.Stderr, "log: "+format+"\n", args...)
}

// streamSink writes to one of the process's standard streams.
type streamSink struct{ f *os.File }

func (s streamSink) Write(p []byte) (int, error) { return s.f.Write(p) }

// startBackground does nothing: a stream has neither rotation nor archives.
func (streamSink) startBackground(sync.Locker) {}

// stopBackground does nothing, for the same reason.
func (streamSink) stopBackground() {}

// release leaves the descriptor open. fd 1 and fd 2 belong to the process, not
// to this module: the reporting path writes to standard error after the writers
// are gone, and so does whatever else the host runs.
func (streamSink) release() error { return nil }

// fileParams is the parameter set a destination is opened with.
type fileParams struct {
	maxSizeMB  int
	maxFiles   int
	maxAgeDays int
}

// paramsOf reads the parameters the destination actually uses. A stream uses
// none of them, so two stdout outputs that disagree on max-size-mb still share
// one writer: the comparison is over what the destination uses, not over what
// the host happened to write down.
func paramsOf(o resolvedOutput) fileParams {
	if o.to != destFile {
		return fileParams{}
	}
	return fileParams{maxSizeMB: o.maxSizeMB, maxFiles: o.maxFiles, maxAgeDays: o.maxAgeDays}
}

// differencesFrom names the parameters the two sets disagree on, in the
// configuration's own spelling, and returns the empty string when they agree.
// One destination has one writer, and one writer has one set of parameters.
func (p fileParams) differencesFrom(other fileParams) string {
	var diffs []string
	for _, d := range [...]struct {
		field      string
		open, want int
	}{
		{"max-size-mb", p.maxSizeMB, other.maxSizeMB},
		{"max-files", p.maxFiles, other.maxFiles},
		{"max-age-days", p.maxAgeDays, other.maxAgeDays},
	} {
		if d.open != d.want {
			diffs = append(diffs, fmt.Sprintf("%s is %d and this output asks for %d", d.field, d.open, d.want))
		}
	}
	return strings.Join(diffs, ", ")
}

// destinationID is the identity two outputs are judged the same destination by.
//
// Standard output and standard error are one identity each. A file is its path
// with the symbolic links resolved, made absolute, so a relative spelling, an
// absolute one and a link all name the one file. The file usually does not
// exist yet on a first run, and the directory is resolved instead; when even
// that fails the path is compared as given, which leaves two spellings of one
// file looking like two destinations. That residue is recorded rather than
// chased with further probing of the file system.
func destinationID(o resolvedOutput) string {
	switch o.to {
	case destStdout:
		return "stdout"
	case destStderr:
		return "stderr"
	default:
		return "file:" + resolvePath(o.path)
	}
}

// resolvePath evaluates a file destination's path as far as the file system
// allows: absolute first, then the links along it, and failing that the links
// along its directory, which is the ordinary case because the log file does not
// exist yet on a first run.
func resolvePath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	dir, base := filepath.Split(abs)
	if resolvedDir, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Join(resolvedDir, base)
	}
	return abs
}

// openWriters takes one writer per output and hands back the release of this
// assembly's references.
//
// An output that cannot be opened rolls the whole call back: the references
// taken for the outputs ahead of it are given up again. A leak there would have
// the next assembly of the same destination report ErrConflictingOutput over
// parameters nobody is using any more.
func openWriters(outputs []resolvedOutput) ([]*destWriter, func(), error) {
	rootMu.Lock()
	defer rootMu.Unlock()

	taken := make([]*destWriter, 0, len(outputs))
	for _, o := range outputs {
		w, err := acquireWriterLocked(o)
		if err != nil {
			for _, done := range taken {
				releaseWriterLocked(done)
			}
			return nil, nil, err
		}
		taken = append(taken, w)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			rootMu.Lock()
			defer rootMu.Unlock()
			for _, w := range taken {
				releaseWriterLocked(w)
			}
		})
	}
	return taken, release, nil
}

// acquireWriterLocked joins the destination's existing writer or opens it. The
// caller holds rootMu.
func acquireWriterLocked(o resolvedOutput) (*destWriter, error) {
	id := destinationID(o)
	params := paramsOf(o)

	if entry, ok := destinationWriters[id]; ok {
		// The comparison spans the writer's lifetime, not one assembly: a
		// later assembly quietly inheriting the first one's parameters
		// would make one configuration rotate differently depending on the
		// order the assemblies ran in.
		if diff := entry.params.differencesFrom(params); diff != "" {
			return nil, fmt.Errorf("%w: destination %s is open with %s", ErrConflictingOutput, id, diff)
		}
		entry.refs++
		return entry.w, nil
	}

	w, err := openWriter(id, o, params)
	if err != nil {
		return nil, err
	}
	w.sink.startBackground(&w.mu)
	destinationWriters[id] = &writerEntry{w: w, refs: 1, params: params}
	return w, nil
}

// openWriter builds the writer of a destination that is not open yet.
func openWriter(id string, o resolvedOutput, params fileParams) (*destWriter, error) {
	switch o.to {
	case destStdout:
		return &destWriter{id: id, sink: streamSink{os.Stdout}}, nil
	case destStderr:
		return &destWriter{id: id, silent: true, sink: streamSink{os.Stderr}}, nil
	case destFile:
		f, err := openRotatingFile(o.path, params)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrOutputUnavailable, o.path, err)
		}
		return &destWriter{id: id, sink: f}, nil
	}
	return nil, fmt.Errorf("%w: output gives %q", ErrUnknownDestination, o.to)
}

// releaseWriterLocked gives up one reference and closes the writer once the
// last one is gone. The caller holds rootMu, which is the outer of the two
// locks: closing takes the destination's own lock.
func releaseWriterLocked(w *destWriter) {
	entry, ok := destinationWriters[w.id]
	if !ok || entry.w != w {
		return
	}
	entry.refs--
	if entry.refs > 0 {
		return
	}
	delete(destinationWriters, w.id)
	w.close()
}

// bootstrapStdoutWriter is the writer the bootstrap chain writes through, taken
// at package initialisation with a reference the root package never gives up.
//
// It is the same writer a configured stdout output gets, which is the point:
// fd 1 carries one writer and one lock even in the ordinary case, and the
// default configuration — the one a host gets by writing no log section at all
// — is exactly one output to standard output. The reference count therefore
// never reaches zero, so no assembly's Close can take Default() away.
var bootstrapStdoutWriter = registerBootstrapStdout()

// registerBootstrapStdout puts the standard output writer into the
// registration before any assembly runs. Opening a stream cannot fail, since
// the process already holds the descriptor, so this has no error to report at a
// point in the life of the process where there would be nowhere to report it.
func registerBootstrapStdout() *destWriter {
	rootMu.Lock()
	defer rootMu.Unlock()

	id := destinationID(resolvedOutput{to: destStdout})
	w := &destWriter{id: id, sink: streamSink{os.Stdout}}
	destinationWriters[id] = &writerEntry{w: w, refs: 1}
	return w
}
