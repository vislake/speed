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
	"github.com/vislake/speed/pkg/core"
	"github.com/vislake/speed/pkg/log"
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

// The fixtures below drive a whole assembly: core.New() with a hand-built set
// of modules, and Run advancing every stage the way a host's would. They live
// here rather than in the file of whichever test needed one first, so that the
// stubs are one shape shared instead of several that drift.
//
// A probe's position in the assembly comes from what it declares and from
// nothing else. core promises dependency order; how it linearises what
// dependency order leaves open is its own implementation choice, so no fixture
// here and no test built on one may read an order out of module names.

// stubConfigModule is the module named config, the one name the registry
// recognises: it is constructed ahead of every stance and is an implicit
// dependency of everyone.
//
// The real module's New reads os.Args unconditionally and refuses the
// arguments go test passes a test binary, so an assembly is handed this one
// and reads back the endpoints the test wrote.
func stubConfigModule(cfg moduleConfig) core.Module {
	return core.Module{
		Name:     "config",
		Provides: []core.Provision{{Token: (*config.Reader)(nil)}},
		New: func(context.Context, *core.Registry) (any, error) {
			return &stubReader{cfg: cfg}, nil
		},
	}
}

// stubLogger delivers the Logger capability over a logger the test holds. A
// module carrying it is constructed before this one and stopped after it: the
// dependency on Logger is optional, and an optional requirement whose provider
// is present still draws the ordering edge.
type stubLogger struct{ logger *slog.Logger }

func (s stubLogger) Named(string) *slog.Logger { return s.logger }
func (s stubLogger) Redaction() log.Redaction  { return noRedaction{} }

// noRedaction takes registrations and masks nothing. What an assembly test
// observes of the Logger capability is the ordering it causes, not the masking.
type noRedaction struct{}

func (noRedaction) AddKeys(...string)              {}
func (noRedaction) AddPattern(string, log.Matcher) {}

var _ log.Logger = stubLogger{}

// capA and capB are capabilities a probe module really delivers, product and
// declaration both. A middleware layer that names one of them in Provides is
// then standing for something the assembly actually contains, which is the
// state the constraints have to be exercised in: a layer pointing at a
// capability nobody delivers is the other case, and it drops its constraint.
type capA interface{ standsForA() }

type capB interface{ standsForB() }

type deliveredA struct{}

type deliveredB struct{}

func (deliveredA) standsForA() {}

func (deliveredB) standsForB() {}

var (
	_ capA = deliveredA{}
	_ capB = deliveredB{}
)

// stageProbe is a module a test puts in an assembly to watch a stage from the
// inside, and to make the registrations a real registrant would make.
//
// A nil callback is left off the descriptor. A callback reports a failed
// observation by returning an error, which fails that stage: the goroutine
// running Run is not the test's, so t.Fatal cannot be called from one, and an
// error carries the text out to where the test reads it.
type stageProbe struct {
	name     string
	requires []core.Requirement
	provides []core.Provision
	// product is what New hands back. It has to satisfy every capability in
	// provides, because core checks the product against the declaration as
	// it constructs.
	product any

	onNew     func(*core.Registry) error
	onMigrate func(*core.Registry) error
	onInit    func(*core.Registry) error
	onStart   func(*core.Registry) error
	onServe   func(*core.Registry) error
	onStop    func(*core.Registry) error
	onClose   func(*core.Registry) error
}

// module renders the probe as the descriptor a registry takes.
func (p *stageProbe) module() core.Module {
	stage := func(f func(*core.Registry) error) func(context.Context, *core.Registry, any) error {
		if f == nil {
			return nil
		}
		return func(_ context.Context, reg *core.Registry, _ any) error { return f(reg) }
	}
	return core.Module{
		Name:     p.name,
		Requires: p.requires,
		Provides: p.provides,
		New: func(_ context.Context, reg *core.Registry) (any, error) {
			if p.onNew != nil {
				if err := p.onNew(reg); err != nil {
					return nil, err
				}
			}
			return p.product, nil
		},
		Migrate: stage(p.onMigrate),
		Init:    stage(p.onInit),
		Start:   stage(p.onStart),
		Serve:   stage(p.onServe),
		Stop:    stage(p.onStop),
		Close:   stage(p.onClose),
	}
}

// requiresRouter is the declaration that puts a probe after this module: it is
// constructed, initialised, started and served once this module has been, and
// stopped and closed before it. It is the only thing a test may rely on for
// that position.
func requiresRouter() []core.Requirement {
	return []core.Requirement{{Token: (*Router)(nil)}}
}

// inOrder runs several stage callbacks one after another, for a probe with
// more than one thing to do in a stage.
func inOrder(steps ...func(*core.Registry) error) func(*core.Registry) error {
	return func(reg *core.Registry) error {
		for _, step := range steps {
			if err := step(reg); err != nil {
				return err
			}
		}
		return nil
	}
}

// routeOn is the stage callback that registers one route through the public
// surface, the path a real registrant takes.
func routeOn(endpoint, pattern string, h nethttp.Handler) func(*core.Registry) error {
	return func(reg *core.Registry) error {
		e, err := endpointFrom(reg, endpoint)
		if err != nil {
			return err
		}
		e.Route(pattern, h)
		return nil
	}
}

// useLayer is the stage callback that registers one middleware layer through
// the public surface.
func useLayer(endpoint string, mw Middleware) func(*core.Registry) error {
	return func(reg *core.Registry) error {
		e, err := endpointFrom(reg, endpoint)
		if err != nil {
			return err
		}
		e.Use(mw)
		return nil
	}
}

// assembly is one registry going through the whole lifecycle on its own
// goroutine, with the two handles a test needs: the cancellation that ends the
// run, and the error Run came back with.
type assembly struct {
	reg    *core.Registry
	cancel context.CancelFunc
	result chan error

	mu       sync.Mutex
	finished bool
	err      error
}

// startAssembly registers the modules, starts Run and hands back the handle.
// The run is ended and waited for whatever the test does, so a failing
// assertion leaves no registry serving for the rest of the binary.
func startAssembly(t *testing.T, modules ...core.Module) *assembly {
	t.Helper()
	reg := core.New()
	for _, m := range modules {
		reg.Register(m)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &assembly{reg: reg, cancel: cancel, result: make(chan error, 1)}
	go func() { a.result <- reg.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		a.wait(t)
	})
	return a
}

// wait takes the result of Run, to this package's deadline. The result is
// kept, so the cleanup may ask for it again after the test already has.
func (a *assembly) wait(t *testing.T) error {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.finished {
		return a.err
	}
	select {
	case err := <-a.result:
		a.finished, a.err = true, err
		return err
	case <-time.After(waitDeadline):
		t.Fatalf("timed out after %s waiting for Run to return", waitDeadline)
		return nil
	}
}

// stop ends the run and reports what Run came back with.
func (a *assembly) stop(t *testing.T) error {
	t.Helper()
	a.cancel()
	return a.wait(t)
}

// routerFrom takes this module's product out of a registry. It reports a
// failure rather than failing a test, because the callers that need it most
// are the probe callbacks, which run on the goroutine driving Run.
func routerFrom(reg *core.Registry) (*router, error) {
	delivered, err := core.Resolve[Router](reg)
	if err != nil {
		return nil, fmt.Errorf("taking up the Router capability: %w", err)
	}
	r, ok := delivered.(*router)
	if !ok {
		return nil, fmt.Errorf("the Router capability is delivered by %T, not by this module", delivered)
	}
	return r, nil
}

// endpointFrom looks one endpoint up through the public surface.
func endpointFrom(reg *core.Registry, name string) (Endpoint, error) {
	r, err := routerFrom(reg)
	if err != nil {
		return nil, err
	}
	e, err := r.Endpoint(name)
	if err != nil {
		return nil, fmt.Errorf("looking up endpoint %q: %w", name, err)
	}
	return e, nil
}

// boundAddr is the address an endpoint really bound, which is where a request
// in these tests goes: the configuration asks for port 0 and the operating
// system picks.
func boundAddr(r *router, name string) (string, error) {
	e, ok := r.endpoints[name]
	if !ok {
		return "", fmt.Errorf("endpoint %q is not declared in this assembly", name)
	}
	addr := e.socket.addr()
	if addr == nil {
		return "", fmt.Errorf("endpoint %q never bound an address", name)
	}
	return addr.String(), nil
}

// listenerState reports what an endpoint's socket looks like at one moment of
// the shutdown: whether Stop has run on it, and whether the drain that Stop
// leaves running in the background is still going.
//
// The pair is what tells the two beats apart. A Stop that waited for its own
// drain would be seen from a later module with the drain already finished.
func listenerState(l *listener) (stopped, draining bool) {
	l.mu.Lock()
	stopping, drained := l.stopping, l.drained
	l.mu.Unlock()
	if drained == nil {
		return stopping, false
	}
	select {
	case <-drained:
		return stopping, false
	default:
		return stopping, true
	}
}

// getStatus issues one GET against a bound endpoint and reports the status it
// answered with. Unlike get, which only needs a request to be in flight, this
// one is for the assertions that turn on what the chain actually answered.
func getStatus(ctx context.Context, addr, path string) (int, error) {
	req, err := nethttp.NewRequestWithContext(ctx, nethttp.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return 0, err
	}
	client := &nethttp.Client{Timeout: waitDeadline}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// attributingEngine is the standard library's multiplexer carrying the
// attribution the seam asks of an implementation subpackage, answered the way
// that multiplexer can answer it: by offering the pair to an engine of its own.
//
// The engine the module really ships with lives in pkg/http/stdmux, which this
// package's tests cannot import without dragging a cycle in. This stands in for
// it, so what the tests here pin is what the assembly does with an answer; the
// answer itself is pinned in that subpackage's own tests.
type attributingEngine struct{ *nethttp.ServeMux }

func (attributingEngine) AttributeRefusal(refused string, mounted []string) (string, bool) {
	for _, candidate := range mounted {
		mux := nethttp.NewServeMux()
		if refusedByMux(mux, candidate) {
			continue
		}
		if refusedByMux(mux, refused) {
			return candidate, true
		}
	}
	return "", false
}

// unattributableEngine refuses a mount the way the real engine does and answers
// the attribution with nothing: a refusal it cannot pin on a single mounted
// pattern. The assembly has to say that rather than present an empty pair as
// the verdict.
type unattributableEngine struct{ *nethttp.ServeMux }

func (unattributableEngine) AttributeRefusal(string, []string) (string, bool) { return "", false }

// refusedByMux mounts the pattern on the multiplexer and reports whether the
// engine refused it.
func refusedByMux(mux *nethttp.ServeMux, pattern string) (refused bool) {
	defer func() { refused = recover() != nil }()
	mux.Handle(pattern, nethttp.NotFoundHandler())
	return false
}

// endpointsAt is a configuration with one endpoint per name, each on a
// loopback port the operating system picks, sharing one drain timeout.
func endpointsAt(drain time.Duration, names ...string) moduleConfig {
	cfg := moduleConfig{Endpoints: make(map[string]endpointConfig, len(names))}
	for _, name := range names {
		cfg.Endpoints[name] = endpointConfig{Address: "127.0.0.1:0", DrainTimeout: &drain}
	}
	return cfg
}
