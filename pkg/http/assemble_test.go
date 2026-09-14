package http

import (
	"errors"
	"io"
	"log/slog"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/vislake/speed/pkg/log"
)

// assembleEndpoint registers through the real surface and assembles the chain
// the way Serve does, so what these tests exercise is the path a run takes.
func assembleEndpoint(t *testing.T, s endpointSettings, injected, logger *slog.Logger,
	register func(Endpoint),
) (nethttp.Handler, error) {
	t.Helper()
	r := newRouter([]endpointSettings{s})
	r.open()
	register(endpointOf(t, r, s.name))
	r.seal()
	return r.endpoints[s.name].assemble(nethttp.NewServeMux(), injected, logger)
}

// mustAssemble is assembleEndpoint for the tests whose subject is the chain
// rather than a failure to build one.
func mustAssemble(t *testing.T, s endpointSettings, injected, logger *slog.Logger,
	register func(Endpoint),
) nethttp.Handler {
	t.Helper()
	handler, err := assembleEndpoint(t, s, injected, logger, register)
	if err != nil {
		t.Fatalf("assembling endpoint %q: %v", s.name, err)
	}
	return handler
}

// trace records what each layer saw, in the order the layers ran.
type trace struct {
	mu     sync.Mutex
	visits []string
}

func (tr *trace) note(name string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.visits = append(tr.visits, name)
}

func (tr *trace) seen() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]string(nil), tr.visits...)
}

// noteLayer builds a layer that records that it ran and passes the request on.
func noteLayer(tr *trace, name string) func(nethttp.Handler) nethttp.Handler {
	return func(next nethttp.Handler) nethttp.Handler {
		return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
			tr.note(name)
			next.ServeHTTP(w, r)
		})
	}
}

// TestRouteConflictIsStartupFailure pins the conversion at the mounting point.
// The engines this seam is built for report a clash by panicking, and left
// alone that takes the process down with a stack trace during a stage whose
// failures are supposed to end the startup in an orderly way.
func TestRouteConflictIsStartupFailure(t *testing.T) {
	logger, _ := newRecordingLogger()
	_, err := assembleEndpoint(t, testSettings("public"), nil, logger, func(e Endpoint) {
		e.Route("GET /things", nethttp.NotFoundHandler())
		e.Route("GET /things", nethttp.NotFoundHandler())
	})
	if !errors.Is(err, ErrRouteConflict) {
		t.Fatalf("two registrations of one pattern did not report ErrRouteConflict: %v", err)
	}
	mustContain(t, err.Error(), "GET /things", "the pattern the registrations clash on")
	mustContain(t, err.Error(), `"public"`, "the endpoint the clash is on")
}

// TestMiddlewareCycleIsStartupFailure pins that the ordering's verdict reaches
// the assembly rather than being swallowed into a chain with layers missing.
func TestMiddlewareCycleIsStartupFailure(t *testing.T) {
	logger, _ := newRecordingLogger()
	r := newRouter([]endpointSettings{testSettings("public")})
	r.open()
	e := r.endpoints["public"]
	// The constraints are put on the recorded layers directly: a registrant
	// cannot yet say which capability its layer delivers, so a cycle cannot
	// be built through the surface alone.
	e.layers = []layer{
		registered("a", 0, tokens((*capAuth)(nil)), tokens((*capTenant)(nil)), nil),
		registered("b", 0, tokens((*capTenant)(nil)), tokens((*capAuth)(nil)), nil),
	}
	r.seal()

	_, err := e.assemble(nethttp.NewServeMux(), nil, logger)
	if !errors.Is(err, ErrMiddlewareCycle) {
		t.Fatalf("a cycle in the constraints did not fail the assembly: %v", err)
	}
}

// TestChainOrderIsOutermostFirst pins that the chain is wrapped in the order
// the ordering gives, rather than in registration order or its reverse.
func TestChainOrderIsOutermostFirst(t *testing.T) {
	tr := &trace{}
	logger, _ := newRecordingLogger()
	handler := mustAssemble(t, testSettings("public"), nil, logger, func(e Endpoint) {
		// Registered innermost-first and with the preferences deciding, so
		// an implementation wrapping in registration order gets the
		// opposite of the right answer.
		e.Use(Middleware{Name: "inner", Order: 10, Wrap: noteLayer(tr, "inner")})
		e.Use(Middleware{Name: "middle", Order: 0, Wrap: noteLayer(tr, "middle")})
		e.Use(Middleware{Name: "outer", Order: -10, Wrap: noteLayer(tr, "outer")})
		e.Route("GET /things", nethttp.HandlerFunc(func(nethttp.ResponseWriter, *nethttp.Request) {
			tr.note("handler")
		}))
	})

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(nethttp.MethodGet, "/things", nil))

	want := []string{"outer", "middle", "inner", "handler"}
	if got := tr.seen(); !equalStrings(got, want) {
		t.Errorf("the chain ran in the order %v, and the ordering gives %v", got, want)
	}
}

// TestBodyLimitAppliedOutsideEveryLayer pins where the limit is imposed. Put in
// Decode, it would not hold for a layer reading the body, nor for a handler
// that parses the body itself.
func TestBodyLimitAppliedOutsideEveryLayer(t *testing.T) {
	logger, _ := newRecordingLogger()
	s := testSettings("public")
	s.maxBodyBytes = 8

	var readErr error
	var read []byte
	handler := mustAssemble(t, s, nil, logger, func(e Endpoint) {
		e.Use(Middleware{Name: "outermost", Order: -100, Wrap: func(next nethttp.Handler) nethttp.Handler {
			return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
				read, readErr = io.ReadAll(r.Body)
				next.ServeHTTP(w, r)
			})
		}})
		e.Route("POST /things", nethttp.NotFoundHandler())
	})

	body := strings.NewReader(strings.Repeat("x", 64))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(nethttp.MethodPost, "/things", body))

	var tooLarge *nethttp.MaxBytesError
	if !errors.As(readErr, &tooLarge) {
		t.Fatalf("the outermost layer read %d bytes with error %v; the limit was not imposed outside it",
			len(read), readErr)
	}
	if tooLarge.Limit != s.maxBodyBytes {
		t.Errorf("the limit imposed was %d, and the endpoint is configured with %d",
			tooLarge.Limit, s.maxBodyBytes)
	}
}

// TestNoBodyLimitIsLeftUnlimited pins the switch-off: an explicit 0 means no
// limit, so the body arrives whole.
func TestNoBodyLimitIsLeftUnlimited(t *testing.T) {
	logger, _ := newRecordingLogger()
	s := testSettings("public")
	s.maxBodyBytes = 0

	var read []byte
	var readErr error
	handler := mustAssemble(t, s, nil, logger, func(e Endpoint) {
		e.Route("POST /things", nethttp.HandlerFunc(func(_ nethttp.ResponseWriter, r *nethttp.Request) {
			read, readErr = io.ReadAll(r.Body)
		}))
	})

	body := strings.NewReader(strings.Repeat("x", 64))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(nethttp.MethodPost, "/things", body))

	if readErr != nil || len(read) != 64 {
		t.Errorf("a body of 64 bytes arrived as %d bytes with error %v, and the endpoint sets no limit",
			len(read), readErr)
	}
}

// TestLoggerInjectedOutsideEveryLayer pins the injection point. Injected by a
// middleware instead, the layers outside it would take the process default
// logger out of the context and write somewhere else entirely.
func TestLoggerInjectedOutsideEveryLayer(t *testing.T) {
	logger, _ := newRecordingLogger()
	injected, records := newRecordingLogger()

	var seen *slog.Logger
	handler := mustAssemble(t, testSettings("public"), injected, logger, func(e Endpoint) {
		e.Use(Middleware{Name: "outermost", Order: -100, Wrap: func(next nethttp.Handler) nethttp.Handler {
			return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
				seen = log.FromContext(r.Context())
				next.ServeHTTP(w, r)
			})
		}})
		e.Route("GET /things", nethttp.NotFoundHandler())
	})

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(nethttp.MethodGet, "/things", nil))

	if seen != injected {
		t.Fatal("the outermost layer did not take the injected logger out of the request context")
	}
	seen.Info("written from the outermost layer")
	if len(records.at(slog.LevelInfo)) != 1 {
		t.Error("what the layer took out of the context does not write to the injected destination")
	}
}

// TestNoLoggerModuleLeavesContextUntouched pins the accepted cost of the
// dependency being optional: with no Logger capability nothing is injected and
// nothing fails. Turning this into a startup failure, or into a fallback that
// injects the default logger, is a change to the design, not a fix.
func TestNoLoggerModuleLeavesContextUntouched(t *testing.T) {
	logger, _ := newRecordingLogger()

	var seen *slog.Logger
	handler := mustAssemble(t, testSettings("public"), nil, logger, func(e Endpoint) {
		e.Route("GET /things", nethttp.HandlerFunc(func(_ nethttp.ResponseWriter, r *nethttp.Request) {
			seen = log.FromContext(r.Context())
		}))
	})

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(nethttp.MethodGet, "/things", nil))

	if seen != log.Default() {
		t.Error("something was injected into the request context although no module delivers Logger")
	}
}

// TestAssembledChainIsListed pins that the chain a run really serves is written
// down at startup, in the order it runs. A process whose layer order can only
// be worked out by sending a request through it is one where a mis-ordered
// chain is found by the request that goes wrong.
func TestAssembledChainIsListed(t *testing.T) {
	logger, records := newRecordingLogger()
	mustAssemble(t, testSettings("public"), nil, logger, func(e Endpoint) {
		e.Use(Middleware{Name: "inner", Order: 10, Wrap: passThrough})
		e.Use(Middleware{Name: "outer", Order: -10, Wrap: passThrough})
		e.Route("GET /things", nethttp.NotFoundHandler())
	})

	written := strings.Join(records.at(slog.LevelInfo), "\n")
	mustContain(t, written, "chain=http:base -> outer -> inner", "the chain in the order it runs")
	mustContain(t, written, "endpoint=public", "the endpoint the chain belongs to")
}

// TestWrapReturningNilPanicsNamingTheLayer pins the hole a layer can leave in
// the chain. Left alone, the request dies on a nil handler with nothing to say
// which layer produced it.
func TestWrapReturningNilPanicsNamingTheLayer(t *testing.T) {
	logger, _ := newRecordingLogger()
	text := wantPanic(t, "assembling a chain with a layer whose Wrap returns nil", func() {
		mustAssemble(t, testSettings("public"), nil, logger, func(e Endpoint) {
			e.Use(Middleware{Name: "hollow", Wrap: func(nethttp.Handler) nethttp.Handler { return nil }})
		})
	})
	mustContain(t, text, "hollow", "the layer that returned nothing")
}

// equalStrings compares two sequences element by element.
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
