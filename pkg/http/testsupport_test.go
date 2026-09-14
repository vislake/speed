package http

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	nethttp "net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/pkg/config"
)

// parseProductionFiles parses this package's production files. The test files
// are excluded: what several tests here pin is the package's own shape, and a
// declaration made by a test is not part of it.
func parseProductionFiles(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory failed: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parsing %s failed: %v", name, err)
		}
		if file.Name.Name != "http" {
			t.Fatalf("%s declares package %s, not http", name, file.Name.Name)
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		t.Fatal("no production file was parsed, so every pin built on this would pass vacuously")
	}
	return fset, files
}

// postWithBody builds a request carrying body, the shape a handler sees.
func postWithBody(body string) *nethttp.Request {
	return httptest.NewRequest(nethttp.MethodPost, "/things", strings.NewReader(body))
}

// spyWriter is a nethttp.ResponseWriter that records what was actually done to
// it. A httptest.ResponseRecorder cannot tell "WriteHeader was never called"
// from "WriteHeader(200) was called", and that is exactly the distinction
// WriteJSON's failure path turns on.
type spyWriter struct {
	header      nethttp.Header
	status      int
	wroteHeader bool
	body        []byte
	writeErr    error
}

func newSpyWriter() *spyWriter {
	return &spyWriter{header: nethttp.Header{}}
}

func (s *spyWriter) Header() nethttp.Header { return s.header }

func (s *spyWriter) WriteHeader(status int) {
	s.wroteHeader = true
	s.status = status
}

func (s *spyWriter) Write(p []byte) (int, error) {
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	s.body = append(s.body, p...)
	return len(p), nil
}

var _ nethttp.ResponseWriter = (*spyWriter)(nil)

// stubReader is a config.Reader handing back a moduleConfig the test wrote.
// The real reader is built by config's own New, which reads os.Args
// unconditionally and refuses the arguments go test passes to a test binary,
// so the callbacks are driven through the interface instead.
type stubReader struct {
	cfg moduleConfig
	err error
	// path records what the caller asked for, so a test can pin that this
	// module decodes its own section rather than somebody else's.
	path string
}

func (s *stubReader) Decode(path string, target any) error {
	s.path = path
	if s.err != nil {
		return s.err
	}
	dst, ok := target.(*moduleConfig)
	if !ok {
		return fmt.Errorf("stub reader: this module decodes into *http.moduleConfig, got %T", target)
	}
	*dst = s.cfg
	return nil
}

var _ config.Reader = (*stubReader)(nil)

// recordingHandler keeps every record written through it, so a test can assert
// what this module said rather than that it said something. It is written to
// from the accept loop's own goroutine, hence the mutex.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func newRecordingLogger() (*slog.Logger, *recordingHandler) {
	h := &recordingHandler{}
	return slog.New(h), h
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// at returns the records written at one level, rendered as message plus
// attributes, which is what an assertion on the text reads.
func (h *recordingHandler) at(level slog.Level) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.records {
		if r.Level != level {
			continue
		}
		text := r.Message
		r.Attrs(func(a slog.Attr) bool {
			text += fmt.Sprintf(" %s=%v", a.Key, a.Value)
			return true
		})
		out = append(out, text)
	}
	return out
}

// gateHandler is a handler a test holds inside: it reports that a request
// arrived and stays there until the test lets go. Two-beat shutdown is only
// observable with a request in flight across the beats.
type gateHandler struct {
	entered  chan struct{}
	release  chan struct{}
	finished chan struct{}
}

func newGateHandler(t *testing.T) *gateHandler {
	t.Helper()
	g := &gateHandler{
		entered:  make(chan struct{}, 16),
		release:  make(chan struct{}),
		finished: make(chan struct{}, 16),
	}
	// The handler is released whatever the test does, so a failing assertion
	// leaves no goroutine parked on the channel for the rest of the binary.
	t.Cleanup(g.letGo)
	return g
}

func (g *gateHandler) ServeHTTP(w nethttp.ResponseWriter, _ *nethttp.Request) {
	g.entered <- struct{}{}
	<-g.release
	w.WriteHeader(nethttp.StatusNoContent)
	g.finished <- struct{}{}
}

// letGo releases every request being held, once, however often it is called.
func (g *gateHandler) letGo() {
	select {
	case <-g.release:
	default:
		close(g.release)
	}
}

// waitFor takes one value from ch, failing the test if none arrives. Every wait
// in these tests carries its own deadline: relying on go test's global timeout
// turns a specific assertion into a whole-binary panic ten minutes later.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(waitDeadline):
		t.Fatalf("timed out after %s waiting for %s", waitDeadline, what)
	}
}

// waitUntil polls a condition to the same deadline, for the states that are
// reached without a channel to wait on.
func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitDeadline)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", waitDeadline, what)
}

// waitDeadline bounds every wait in this package's tests.
const waitDeadline = 5 * time.Second

// testSettings is one endpoint's settings with the timeouts a test wants:
// short enough that a hung assertion fails rather than hangs, long enough that
// a loaded machine does not fail a passing one.
func testSettings(name string) endpointSettings {
	return endpointSettings{
		name:              name,
		address:           "127.0.0.1:0",
		readHeaderTimeout: 5 * time.Second,
		drainTimeout:      5 * time.Second,
		maxBodyBytes:      defaultMaxBodyBytes,
	}
}
