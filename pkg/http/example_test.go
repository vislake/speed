package http_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vislake/speed/pkg/config"
	jsonformat "github.com/vislake/speed/pkg/config/format/json"
	filesource "github.com/vislake/speed/pkg/config/source/file"
	"github.com/vislake/speed/pkg/core"
	speedhttp "github.com/vislake/speed/pkg/http"
	"github.com/vislake/speed/pkg/http/stdmux"
	"github.com/vislake/speed/pkg/log"
)

// This file is the runnable half of this package's documentation: every
// example below goes through the public surface and stands on a real assembly,
// and what it prints is what that assembly did.
//
// It opens the external test package, and it brings its own stubs rather than
// the internal suite's fixtures. That is also what fixes the naming direction
// here: net/http keeps its own name and the parent package takes the alias,
// which is what the package documentation gives an external file.
//
// The entry point it assembles is the shipped subpackage's, which is where a
// host gets one: importing pkg/http/stdmux registers the module its engine is
// bound to, and an example that built the descriptor by hand would be teaching
// a shape the module does not have.

// exampleDocument is the configuration every example runs on: one listening
// endpoint named public, on an address the operating system picks.
const exampleDocument = `{
  "http": {
    "endpoints": {
      "public": {"address": "127.0.0.1:0"}
    }
  }
}`

// exampleDeadline bounds every wait in this file. It is generous on purpose:
// what it guards against is a wait that never ends, not a slow machine.
const exampleDeadline = 10 * time.Second

// anExampleHost is one assembly running for an example: the registry going
// through the whole lifecycle on its own goroutine, and the handles the
// example needs to end the run.
type anExampleHost struct {
	cancel context.CancelFunc
	result chan error
	dir    string
}

// startExample assembles a host and runs it until the entry point has served.
//
// The assembly is the one a host with a config file builds: the configuration
// module with the JSON format and the file source, the host identity pointing
// at the document above, a logger writing to standard output, and the standard
// library entry point.
//
// registrant runs in the Init stage of a module that declares its dependency
// on Router, so the assembly orders it after the entry point: the registration
// gate is open by the time it is called, which is what lets an example
// register the way a real module does. That module's Serve callback runs once
// the entry point has bound every address, which is what "the assembly is up"
// means here.
//
// extra modules are registered besides that registrant, for the examples that
// watch the assembly rather than take part in it. A non-nil error is a startup
// that failed; nothing is running then.
func startExample(registrant func(*core.Registry) error, extra ...core.Module) (*anExampleHost, error) {
	dir, err := os.MkdirTemp("", "http-example")
	if err != nil {
		return nil, err
	}
	document := filepath.Join(dir, "config.json")
	if err := os.WriteFile(document, []byte(exampleDocument), 0o600); err != nil {
		return nil, err
	}

	// The configuration module reads the command line unconditionally, and the
	// arguments a test binary is started with are not this program's.
	args := os.Args
	os.Args = []string{"example"}
	defer func() { os.Args = args }()

	served := make(chan struct{})
	reg := core.New()
	reg.Register(config.Module())
	reg.Register(jsonformat.Module())
	reg.Register(filesource.Module())
	reg.Register(core.Module{
		Name:      "host",
		Resources: []any{config.HostIdentity{Prefix: "EXAMPLE", DefaultLocator: "file:" + document}},
	})
	reg.Register(exampleLoggerModule())
	reg.Register(stdmux.Module())
	for _, module := range extra {
		reg.Register(module)
	}
	reg.Register(core.Module{
		Name:     "registrant",
		Requires: []core.Requirement{{Token: (*speedhttp.Router)(nil)}},
		Init: func(_ context.Context, reg *core.Registry, _ any) error {
			return registrant(reg)
		},
		Serve: func(context.Context, *core.Registry, any) error {
			close(served)
			return nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- reg.Run(ctx) }()

	select {
	case <-served:
		return &anExampleHost{cancel: cancel, result: result, dir: dir}, nil
	case err := <-result:
		cancel()
		_ = os.RemoveAll(dir)
		return nil, err
	case <-time.After(exampleDeadline):
		cancel()
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("the assembly did not reach the Serve stage within %s", exampleDeadline)
	}
}

// stop ends the run and reports what Run came back with.
func (h *anExampleHost) stop() error {
	h.cancel()
	err := <-h.result
	_ = os.RemoveAll(h.dir)
	return err
}

// exampleLoggerModule delivers the Logger capability over a logger that writes
// every record to standard output.
//
// That is where an example's evidence comes from. What this module reports
// about the assembly it made — the chain it lists per endpoint — is a record,
// and the alternative to reading it is an example that asserts nothing about
// the run it just did.
func exampleLoggerModule() core.Module {
	return core.Module{
		Name:     "examplelogger",
		Provides: []core.Provision{{Token: (*log.Logger)(nil)}},
		New: func(context.Context, *core.Registry) (any, error) {
			return exampleLogger{logger: slog.New(exampleHandler{})}, nil
		},
	}
}

// exampleLogger is the Logger capability over one logger. Named ignores the
// module name: the records this file compares carry the endpoint and the
// chain, neither of which depends on it.
type exampleLogger struct{ logger *slog.Logger }

func (l exampleLogger) Named(string) *slog.Logger { return l.logger }
func (l exampleLogger) Redaction() log.Redaction  { return exampleRedaction{} }

// exampleRedaction takes registrations and masks nothing: nothing these
// examples log carries a value a rule would redact.
type exampleRedaction struct{}

func (exampleRedaction) AddKeys(...string)              {}
func (exampleRedaction) AddPattern(string, log.Matcher) {}

var _ log.Logger = exampleLogger{}

// exampleHandler renders one record per line, level and message first and then
// the attributes in the order they were written.
type exampleHandler struct{}

func (exampleHandler) Enabled(context.Context, slog.Level) bool { return true }

func (exampleHandler) Handle(_ context.Context, r slog.Record) error {
	line := r.Level.String() + " " + r.Message
	r.Attrs(func(a slog.Attr) bool {
		line += fmt.Sprintf(" %s=%v", a.Key, a.Value.Any())
		return true
	})
	fmt.Println(line)
	return nil
}

func (exampleHandler) WithAttrs([]slog.Attr) slog.Handler { return exampleHandler{} }
func (exampleHandler) WithGroup(string) slog.Handler      { return exampleHandler{} }

// ExampleEndpoint_Route registers a route: what a module does in its Init
// callback. It takes the Router capability up, looks an endpoint up by the
// name configuration gave it, and binds a pattern to a handler.
func ExampleEndpoint_Route() {
	host, err := startExample(func(reg *core.Registry) error {
		router, err := core.Resolve[speedhttp.Router](reg)
		if err != nil {
			return err
		}
		endpoint, err := router.Endpoint("public")
		if err != nil {
			return err
		}
		endpoint.Route("GET /things", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = speedhttp.WriteJSON(w, speedhttp.StatusFor(nil), map[string]string{"id": "1"})
		}))
		return nil
	})
	if err != nil {
		fmt.Println("startup failed:", err)
		return
	}
	defer func() { _ = host.stop() }()

	// Output:
	// INFO an HTTP endpoint's chain is assembled endpoint=public routes=1 chain=http:base
}

// ExampleEndpoint_Use registers two middleware layers on one endpoint.
//
// Provides and After are what make a constraint land: Provides says which
// capability a layer stands for on the chain, and After places this layer
// inside the middleware of that capability. A layer that names an After
// without saying what it stands for has nothing to land on.
func ExampleEndpoint_Use() {
	// The capabilities the two layers name. A real layer points at a
	// capability some module delivers; what matters here is that the layer
	// standing for one says so itself.
	type (
		recovery interface{ recovers() }
		tracing  interface{ traces() }
	)

	host, err := startExample(func(reg *core.Registry) error {
		router, err := core.Resolve[speedhttp.Router](reg)
		if err != nil {
			return err
		}
		endpoint, err := router.Endpoint("public")
		if err != nil {
			return err
		}
		// trace stands for tracing and belongs inside the recovery layer.
		// Its Order asks for the front of the chain; the After constraint
		// is what overrides that.
		endpoint.Use(speedhttp.Middleware{
			Name:     "trace",
			Provides: []core.Token{(*tracing)(nil)},
			After:    []core.Token{(*recovery)(nil)},
			Order:    -100,
			Wrap:     func(next http.Handler) http.Handler { return next },
		})
		// recover stands for recovery itself, so trace's constraint lands
		// on it and nowhere else.
		endpoint.Use(speedhttp.Middleware{
			Name:     "recover",
			Provides: []core.Token{(*recovery)(nil)},
			Wrap:     func(next http.Handler) http.Handler { return next },
		})
		return nil
	})
	if err != nil {
		fmt.Println("startup failed:", err)
		return
	}
	defer func() { _ = host.stop() }()

	// Output:
	// INFO an HTTP endpoint's chain is assembled endpoint=public routes=0 chain=http:base -> recover -> trace
}

// ExampleDecode decodes a request body in a handler.
//
// Decode rejects a field the type does not declare — a misspelled name and a
// field from another version of the API are the same thing to it — and it
// calls the value's own Validate when the type has one. StatusFor turns what
// it reports into a status code: 400 for a body that is not the shape this API
// reads, 422 for a body its own rules reject, and the caller writes whatever
// error body its API specifies.
func ExampleDecode() {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var o order
		if err := speedhttp.Decode(r, &o); err != nil {
			_ = speedhttp.WriteJSON(w, speedhttp.StatusFor(err), map[string]string{"error": "the request body was rejected"})
			return
		}
		_ = speedhttp.WriteJSON(w, speedhttp.StatusFor(nil), o)
	})

	// A body the type accepts, one its own rule rejects, and one carrying a
	// field it never declares.
	for _, body := range []string{
		`{"quantity":2}`,
		`{"quantity":0}`,
		`{"quantity":2,"unit":"box"}`,
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(body)))
		fmt.Println(recorder.Code, recorder.Body.String())
	}

	// Output:
	// 200 {"quantity":2}
	// 422 {"error":"the request body was rejected"}
	// 400 {"error":"the request body was rejected"}
}

// order is the request body of the example above. A body type checks itself:
// this package provides the mechanism and no rules at all.
type order struct {
	Quantity int `json:"quantity"`
}

func (o order) Validate() error {
	if o.Quantity < 1 {
		return errors.New("quantity must be at least 1")
	}
	return nil
}

// ExampleWriteJSON writes a response.
//
// Encoding happens before anything is written, so a value that cannot be
// encoded leaves the response untouched. StatusFor(nil) is 200, which is what
// lets the success path and the failure path write the same way.
func ExampleWriteJSON() {
	recorder := httptest.NewRecorder()
	if err := speedhttp.WriteJSON(recorder, speedhttp.StatusFor(nil), map[string]int{"count": 3}); err != nil {
		fmt.Println("writing the response failed:", err)
		return
	}

	response := recorder.Result()
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		fmt.Println("reading the response back failed:", err)
		return
	}

	fmt.Println(response.Status)
	fmt.Println(response.Header.Get("Content-Type"))
	fmt.Println(string(body))

	// Output:
	// 200 OK
	// application/json; charset=utf-8
	// {"count":3}
}

// ExampleSpec declares an OpenAPI fragment as a resource.
//
// The fragment is a static declaration: this module stores it and checks its
// shape at startup, and a reader takes the declarations up with
// core.Resources, each carrying the name of the module that owns it.
func ExampleSpec() {
	reg := core.New()
	reg.Register(core.Module{
		Name: "catalog",
		Resources: []any{speedhttp.Spec{
			Endpoint: "public",
			Document: []byte(`{"openapi":"3.1.0","info":{"title":"Catalog"},"paths":{}}`),
		}},
	})

	for _, declared := range core.Resources[speedhttp.Spec](reg) {
		fmt.Println(declared.Module, declared.Value.Endpoint)
	}

	// Output:
	// catalog public
}

// ExampleSpec_invalidShape declares a fragment whose shape is not usable. The
// shape is checked at startup, and the failure names the module that declared
// it, so the fragment is fixed at its source.
func ExampleSpec_invalidShape() {
	_, err := startExample(func(*core.Registry) error { return nil }, core.Module{
		Name:      "catalog",
		Resources: []any{speedhttp.Spec{Endpoint: "public", Document: []byte(`{"swagger":"2.0"}`)}},
	})
	fmt.Println(errors.Is(err, speedhttp.ErrInvalidSpec))

	// Output:
	// true
}
