package http

import (
	"errors"
	"io"
	"log/slog"
	nethttp "net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/vislake/speed/pkg/core"
	"github.com/vislake/speed/pkg/log"
)

// assembleEndpoint registers through the real surface and assembles the chain
// the way Serve does, so what these tests exercise is the path a run takes. Its
// engine is the bare multiplexer, an engine that answers no attribution.
func assembleEndpoint(t *testing.T, s endpointSettings, injected, logger *slog.Logger,
	register func(Endpoint),
) (nethttp.Handler, error) {
	t.Helper()
	return assembleEndpointOn(t, nethttp.NewServeMux(), s, injected, logger, register)
}

// assembleEndpointOn is assembleEndpoint with the engine under the test's
// control, for the tests whose subject is what the assembly asks the engine.
func assembleEndpointOn(t *testing.T, engine Engine, s endpointSettings, injected, logger *slog.Logger,
	register func(Endpoint),
) (nethttp.Handler, error) {
	t.Helper()
	r := newRouter([]endpointSettings{s}, func() Engine { return nethttp.NewServeMux() })
	r.open()
	register(endpointOf(t, r, s.name))
	r.seal()
	return r.endpoints[s.name].assemble(engine, injected, logger)
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

// TestRouteConflictNamesBothPatternsAndTheEndpoint pins what the failure has to
// carry. The registration surface does not know who registered either pattern,
// so the patterns themselves are the only route back to the two calls, and one
// of them alone sends the reader looking for a second registration they cannot
// find.
//
// The two patterns here are different strings, which is what makes the
// assertion discriminating: with one pattern registered twice, naming only the
// one being mounted would read the same as naming both. The fragment asserted
// is the pair standing together in this module's own sentence, not merely both
// appearing somewhere in a message that ends with the engine's own words.
//
// The engine answers the attribution, which every engine the seam is made for
// is asked for; an assembly that did not ask would fall to the one-pattern
// failure this test is here to rule out.
func TestRouteConflictNamesBothPatternsAndTheEndpoint(t *testing.T) {
	logger, _ := newRecordingLogger()
	engine := attributingEngine{nethttp.NewServeMux()}
	_, err := assembleEndpointOn(t, engine, testSettings("public"), nil, logger, func(e Endpoint) {
		e.Route("GET /things/{id}", nethttp.NotFoundHandler())
		e.Route("GET /things/{name}", nethttp.NotFoundHandler())
	})
	if !errors.Is(err, ErrRouteConflict) {
		t.Fatalf("two patterns matching the same requests did not report ErrRouteConflict: %v", err)
	}
	mustContain(t, err.Error(), `"GET /things/{id}" and "GET /things/{name}"`,
		"both patterns, named together, which is the whole of the way back to the two calls")
	mustContain(t, err.Error(), `endpoint "public"`, "the endpoint the clash is on")
}

// TestConflictWithoutAttributionSaysSo pins the failure for an engine that
// refuses a mount and answers no attribution: it has to say the refusal was
// not attributed, rather than name the one pattern this module is sure about as
// though that were the verdict.
//
// The discriminating assertion is that sentence. The engine here is the bare
// multiplexer, which answers nothing; an assembly that worked the pair out on
// its own — the shape this module used to have — writes the pair instead and
// fails here, while an implementation that reads the engine's own complaint
// fails on the pattern it names.
func TestConflictWithoutAttributionSaysSo(t *testing.T) {
	logger, _ := newRecordingLogger()
	_, err := assembleEndpoint(t, testSettings("public"), nil, logger, func(e Endpoint) {
		e.Route("GET /things/{id}", nethttp.NotFoundHandler())
		e.Route("GET /things/{name}", nethttp.NotFoundHandler())
	})
	if !errors.Is(err, ErrRouteConflict) {
		t.Fatalf("two patterns matching the same requests did not report ErrRouteConflict: %v", err)
	}
	mustContain(t, err.Error(), "did not attribute the refusal", "that the engine named no pair")
	mustContain(t, err.Error(), `"GET /things/{name}"`, "the pattern the engine refused")
	mustContain(t, err.Error(), `endpoint "public"`, "the endpoint the clash is on")
}

// TestConflictAnsweredWithNoPairSaysSo is the same statement for the engine
// that does take the attribution on and comes back with nothing: an empty
// answer is no pair either, and the failure must not present it as one.
func TestConflictAnsweredWithNoPairSaysSo(t *testing.T) {
	logger, _ := newRecordingLogger()
	engine := unattributableEngine{nethttp.NewServeMux()}
	_, err := assembleEndpointOn(t, engine, testSettings("public"), nil, logger, func(e Endpoint) {
		e.Route("GET /things/{id}", nethttp.NotFoundHandler())
		e.Route("GET /things/{name}", nethttp.NotFoundHandler())
	})
	if !errors.Is(err, ErrRouteConflict) {
		t.Fatalf("two patterns matching the same requests did not report ErrRouteConflict: %v", err)
	}
	mustContain(t, err.Error(), "did not attribute the refusal", "that no pair was named")
	mustContain(t, err.Error(), `"GET /things/{name}"`, "the pattern the engine refused")
}

// TestConstraintThroughUseOrdersAgainstItsProvider pins that a constraint
// declared the only way a registrant can declare one — through Use — reaches
// the layer it points at.
//
// The two Order values are deliberately the wrong way round: left to Order
// alone the chain is "tenant" then "auth", and that is exactly what an
// implementation whose provider index stays empty produces, whatever anyone
// registers. Provides is what the After resolves against, so the constraint
// wins and "auth" comes out first.
func TestConstraintThroughUseOrdersAgainstItsProvider(t *testing.T) {
	tr := &trace{}
	logger, _ := newRecordingLogger()
	handler := mustAssemble(t, testSettings("public"), nil, logger, func(e Endpoint) {
		e.Use(Middleware{
			Name:     "auth",
			Order:    10,
			Provides: []core.Token{(*capAuth)(nil)},
			Wrap:     noteLayer(tr, "auth"),
		})
		e.Use(Middleware{
			Name:  "tenant",
			Order: 0,
			After: []core.Token{(*capAuth)(nil)},
			Wrap:  noteLayer(tr, "tenant"),
		})
		e.Route("GET /things", nethttp.NotFoundHandler())
	})

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(nethttp.MethodGet, "/things", nil))

	want := []string{"auth", "tenant"}
	if got := tr.seen(); !equalStrings(got, want) {
		t.Errorf("the chain ran in the order %v, want %v: After outranks Order, and %v is what "+
			"comes out when the constraint finds nothing to bind to", got, want,
			[]string{"tenant", "auth"})
	}
}

// TestEmptyProvidesStaysLegal pins the design's word that a layer standing for
// nothing is a legal registration: it is ordered with the rest and it is in the
// chain. Making Provides required — the obvious way to make every constraint
// land — would fail here, and it is a change to the design rather than a fix.
func TestEmptyProvidesStaysLegal(t *testing.T) {
	tr := &trace{}
	logger, _ := newRecordingLogger()
	handler := mustAssemble(t, testSettings("public"), nil, logger, func(e Endpoint) {
		e.Use(Middleware{Name: "anonymous", Order: 0, Wrap: noteLayer(tr, "anonymous")})
		e.Use(Middleware{
			Name:     "auth",
			Order:    10,
			Provides: []core.Token{(*capAuth)(nil)},
			Wrap:     noteLayer(tr, "auth"),
		})
		e.Route("GET /things", nethttp.NotFoundHandler())
	})

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(nethttp.MethodGet, "/things", nil))

	want := []string{"anonymous", "auth"}
	if got := tr.seen(); !equalStrings(got, want) {
		t.Errorf("the chain ran in the order %v, want %v: a layer that names no capability of "+
			"its own is still ordered and still runs", got, want)
	}
}

// TestUnlandedConstraintIsReported pins the diagnostic the design requires.
//
// One assembly carries three shapes: "tenant" points at a capability a layer on
// this endpoint stands for, "audit" points at one nobody stands for, and
// "recover" stands for the capability it names and is the only layer that does.
// The assembly carries on through all three — a capability absent from the
// process is a legal configuration — so each of the two unplaced constraints
// has to be visible, located, and told apart from the other, because the two
// are answered by different edits.
func TestUnlandedConstraintIsReported(t *testing.T) {
	logger, records := newRecordingLogger()
	_, err := assembleEndpoint(t, testSettings("public"), nil, logger, func(e Endpoint) {
		e.Use(Middleware{
			Name:     "auth",
			Provides: []core.Token{(*capAuth)(nil)},
			Wrap:     passThrough,
		})
		e.Use(Middleware{
			Name:  "tenant",
			After: []core.Token{(*capAuth)(nil)},
			Wrap:  passThrough,
		})
		e.Use(Middleware{
			Name:  "audit",
			After: []core.Token{(*capQuota)(nil)},
			Wrap:  passThrough,
		})
		e.Use(Middleware{
			Name:     "recover",
			Provides: []core.Token{(*capRecovery)(nil)},
			After:    []core.Token{(*capRecovery)(nil)},
			Wrap:     passThrough,
		})
		e.Route("GET /things", nethttp.NotFoundHandler())
	})
	if err != nil {
		t.Fatalf("a constraint that placed nothing must not fail the assembly: %v", err)
	}

	warned := records.at(slog.LevelWarn)
	if len(warned) == 0 {
		t.Fatal("nothing was written about the constraints that placed nothing, so a module " +
			"absent from the process and a module that forgot its Provides look the same")
	}
	written := strings.Join(warned, "\n")
	if strings.Contains(written, "tenant") {
		t.Errorf("the report lists tenant, whose constraint found a provider and placed it. "+
			"Listing the constraints that worked buries the ones that did not:\n%s", written)
	}

	var absent, self string
	for _, line := range warned {
		switch {
		case strings.Contains(line, "middleware=audit"):
			absent = line
		case strings.Contains(line, "middleware=recover"):
			self = line
		}
	}
	if absent == "" || self == "" {
		t.Fatalf("the diagnostics were\n%s\nwant one line each for audit and recover", written)
	}

	mustContain(t, absent, typeName(reflect.TypeOf((*capQuota)(nil)).Elem()),
		"the capability nobody stands for")
	mustContain(t, absent, "After[0]", "which declaration on that layer")
	mustContain(t, absent, `endpoint=public`, "the endpoint the declaration was made on")
	mustContain(t, absent, "cause="+string(causeNoProvider),
		"the cause of a capability no layer on this endpoint stands for")

	mustContain(t, self, typeName(reflect.TypeOf((*capRecovery)(nil)).Elem()),
		"the capability the declaring layer stands for on its own")
	mustContain(t, self, "cause="+string(causeSelfOnly),
		"the cause of a layer standing only for the capability it names: nobody is absent here, "+
			"the declaration is what is wrong")
}

// TestCycleThroughUseFailsTheAssembly pins that a cycle is reachable through
// the public surface at all, and that the ordering's verdict reaches the
// assembly rather than being swallowed into a chain with layers missing.
//
// Both layers are registered with Use, as a registrant would. With the
// provider index empty neither After binds to anything, the graph has no edges
// and the assembly succeeds — which is the observation this replaces.
func TestCycleThroughUseFailsTheAssembly(t *testing.T) {
	logger, _ := newRecordingLogger()
	_, err := assembleEndpoint(t, testSettings("public"), nil, logger, func(e Endpoint) {
		e.Use(Middleware{
			Name:     "a",
			Provides: []core.Token{(*capAuth)(nil)},
			After:    []core.Token{(*capTenant)(nil)},
			Wrap:     passThrough,
		})
		e.Use(Middleware{
			Name:     "b",
			Provides: []core.Token{(*capTenant)(nil)},
			After:    []core.Token{(*capAuth)(nil)},
			Wrap:     passThrough,
		})
		e.Route("GET /things", nethttp.NotFoundHandler())
	})
	if !errors.Is(err, ErrMiddlewareCycle) {
		t.Fatalf("two layers each declaring itself inside the other did not fail the assembly: %v", err)
	}
	mustContain(t, err.Error(), `"a"`, "one member of the cycle")
	mustContain(t, err.Error(), `"b"`, "the other member of the cycle")
	mustContain(t, err.Error(), `endpoint "public"`, "the endpoint the cycle is on")
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

// TestWrapReturningNilFailsTheAssembly pins the hole a layer can leave in the
// chain, and how it is reported. Left alone the request would die on a nil
// handler with nothing to say which layer produced it.
//
// It is a startup failure and not a panic, and both halves are asserted here.
// Assembly runs in Serve: a panic there unwinds past the registry, which then
// never runs the rollback, so the endpoints bound before this one keep their
// sockets and their accept goroutines with nobody left to close them. A
// returned error ends the startup the ordinary way and the rollback runs.
func TestWrapReturningNilFailsTheAssembly(t *testing.T) {
	logger, _ := newRecordingLogger()

	var err error
	func() {
		defer func() {
			if raised := recover(); raised != nil {
				t.Fatalf("assembling panicked with %v; in Serve a panic skips the registry's "+
					"rollback and strands the endpoints already bound", raised)
			}
		}()
		_, err = assembleEndpoint(t, testSettings("public"), nil, logger, func(e Endpoint) {
			e.Use(Middleware{Name: "hollow", Wrap: func(nethttp.Handler) nethttp.Handler { return nil }})
			e.Route("GET /things", nethttp.NotFoundHandler())
		})
	}()

	if !errors.Is(err, ErrChainAssembly) {
		t.Fatalf("a layer whose Wrap returned nil reported %v, want an error wrapping "+
			"ErrChainAssembly", err)
	}
	mustContain(t, err.Error(), `"hollow"`, "the layer that returned nothing")
	mustContain(t, err.Error(), `endpoint "public"`, "the endpoint whose chain has the hole")
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
