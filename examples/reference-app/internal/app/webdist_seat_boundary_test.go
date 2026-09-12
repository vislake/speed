package app

// This file pins the layer boundary the WebDistDir form composes -- the
// deployment shape (APP_WEB_DIST set) the release ledger registers: the
// registry's Middleware seat wraps only the chain's output, and the SPA
// file server sits OUTSIDE that layer, so the paths the frontend serves
// itself never traverse the seat's middleware. The instrumentation used to
// be applied at serve time, around the SPA-wrapped face, so statically
// served paths were instrumented then; under this composition they are
// not, and this pin turns red if the shape moves back -- the SPA wrap
// moved under the seat, the seat's application around the chain dropped,
// or a serve-time layer put back outside the file server.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"

	speedapp "github.com/vislake/speed/go/app"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/examples/reference-app/internal/app/demo"
)

// spaProbeName names the test-only component that declares the recording
// middleware on the registry's Middleware seat for this pin.
const spaProbeName = "reference-app.test-spa-seat-probe"

// spaProbe records the request paths that traversed the probe's middleware.
type spaProbe struct {
	mu    sync.Mutex
	paths []string
}

// record notes one request path that reached the probe's layer.
func (p *spaProbe) record(path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paths = append(p.paths, path)
}

// take returns the paths recorded since the last call and resets the record.
func (p *spaProbe) take() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	paths := p.paths
	p.paths = nil
	return paths
}

// spaProbeComponent returns the pin's probe component. Its Init appends the
// recording middleware to the registry's Middleware seat, so the middleware
// is applied exactly where the seat's members are -- by chain.Standard,
// around the finished chain -- and nothing else in the composition is
// disturbed. The probe stands in for the observability middleware here: the
// boundary under test is the seat layer's position relative to the SPA file
// server, and the probe makes that position observable from the test.
func spaProbeComponent(probe *spaProbe) pkgcore.Component {
	return pkgcore.Component{
		Name:         spaProbeName,
		Capabilities: pkgcore.MultiReplicaSafe,
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &hostStep{}, nil
		},
		Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
			return reg.Middleware.Add(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					probe.record(r.URL.Path)
					next.ServeHTTP(w, r)
				})
			})
		},
	}
}

// selectBeforeApp returns the composition with name selected immediately
// before the app component's entry: the plan order follows the selection
// order, and the app component's Init is what composes the face over the
// seat, so the probe's Init turn -- the seat write -- must run first.
func selectBeforeApp(t *testing.T, whole pkgcore.ComponentConfig, name string) pkgcore.ComponentConfig {
	t.Helper()
	raw, ok := whole.Get("components")
	if !ok {
		t.Fatal("the composition carries no components block")
	}
	block, ok := raw.(pkgcore.ComponentConfig)
	if !ok {
		t.Fatalf("the components entry carries a %T, want a ComponentConfig", raw)
	}
	rebuilt := pkgcore.ComponentConfig{}
	inserted := false
	for _, key := range block.Keys() {
		if key == hostComponentPrefix+"app" && !inserted {
			rebuilt = rebuilt.With(name, nil)
			inserted = true
		}
		value, _ := block.Get(key)
		rebuilt = rebuilt.With(key, value)
	}
	if !inserted {
		t.Fatalf("the components block carries no %q entry to insert %q before", hostComponentPrefix+"app", name)
	}
	return whole.With("components", rebuilt)
}

// TestWebDistForm_SeatLayerStaysInsideTheSPA pins the WebDistDir form's
// layer boundary over the real deployment composition: the live drive
// (observability selected, the listener started) with a frontend directory
// configured, one probe component added to the seat. Requests are driven
// through the started face's own handler -- no socket traffic, no timing
// dependence -- and assert the boundary from both sides: a chain-routed
// request (/healthz) traverses the seat's layer, while the SPA's own
// answers (a built asset and the client-route index fallback) do not. A
// final check pins that serve time adds no layer of its own, so the SPA
// stays the outermost composition over the composed face.
func TestWebDistForm_SeatLayerStaysInsideTheSPA(t *testing.T) {
	const (
		assetBody = "console.log('built asset');\n"
		indexBody = "<!doctype html><html><body>spa entry</body></html>\n"
	)
	webDist := t.TempDir()
	if err := os.MkdirAll(filepath.Join(webDist, "assets"), 0o755); err != nil {
		t.Fatalf("create the frontend fixture's asset directory: %v", err)
	}
	for _, fixture := range []struct{ path, body string }{
		{filepath.Join(webDist, "index.html"), indexBody},
		{filepath.Join(webDist, "assets", "app.js"), assetBody},
	} {
		if err := os.WriteFile(fixture.path, []byte(fixture.body), 0o644); err != nil {
			t.Fatalf("write the frontend fixture %s: %v", fixture.path, err)
		}
	}

	ctx := context.Background()
	b := newServerBuild(ServerConfig{
		DeploymentMode: pkgcore.DeploymentModeStandalone,
		Port:           "0",
		SQLitePath:     filepath.Join(t.TempDir(), "webdist-seat.db"),
		HostTenants:    demo.DemoHostTenants,
		Memberships:    NewSignInMemberships(),
		WebDistDir:     webDist,
	})
	probe := &spaProbe{}
	components, spec, err := b.assemblyInputs(ctx, true)
	if err != nil {
		t.Fatalf("assemblyInputs(): %v", err)
	}
	spec.Overrides.Config = selectBeforeApp(t, spec.Overrides.Config, spaProbeName)

	reg := pkgcore.NewComponentRegistry()
	if err = reg.Register(spaProbeComponent(probe)); err != nil {
		t.Fatalf("register the seat probe: %v", err)
	}
	for _, c := range components {
		if err = reg.Register(c); err != nil {
			t.Fatalf("register %q: %v", c.Name, err)
		}
	}
	if err = speedapp.Assemble(ctx, reg, spec); err != nil {
		t.Fatalf("Assemble(): %v", err)
	}
	t.Cleanup(func() {
		if shutdownErr := speedapp.Shutdown(context.Background(), reg); shutdownErr != nil {
			t.Errorf("Shutdown(): %v", shutdownErr)
		}
	})

	face, err := pkgcore.Get[*hostFace](reg)
	if err != nil {
		t.Fatalf("read the composed face: %v", err)
	}
	if face.server == nil {
		t.Fatal("the live drive composed the face without starting its listener")
	}

	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		face.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	// The chain's own traffic traverses the seat's layer: /healthz is one of
	// the SPA's declared server paths and pre-auth allowlisted in the chain,
	// so it is answered by the mux's liveness handler THROUGH the layer
	// chain.Standard applied around the chain's output.
	if rec := get(obs.HealthzPath); rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("GET %s = %d %q, want the liveness handler's 200", obs.HealthzPath, rec.Code, rec.Body.String())
	}
	if got := probe.take(); !slices.Equal(got, []string{obs.HealthzPath}) {
		t.Errorf("the seat layer saw %v for the chain-served %s request, want exactly that path: the seat's layer must wrap the chain output", got, obs.HealthzPath)
	}

	// The SPA's own traffic does not: a real built asset and the client-route
	// index fallback are both answered by the file server sitting OUTSIDE the
	// seat layer, so neither may reach the probe.
	if rec := get("/assets/app.js"); rec.Code != http.StatusOK || rec.Body.String() != assetBody {
		t.Fatalf("GET /assets/app.js = %d %q, want the fixture asset's 200", rec.Code, rec.Body.String())
	}
	if got := probe.take(); len(got) != 0 {
		t.Errorf("the seat layer saw %v for the SPA-served asset request, want none: the SPA file server must sit OUTSIDE the seat's layer", got)
	}
	if rec := get("/deep/client/route"); rec.Code != http.StatusOK || rec.Body.String() != indexBody {
		t.Fatalf("GET /deep/client/route = %d %q, want the SPA index fallback's 200", rec.Code, rec.Body.String())
	}
	if got := probe.take(); len(got) != 0 {
		t.Errorf("the seat layer saw %v for the SPA fallback request, want none: the SPA file server must sit OUTSIDE the seat's layer", got)
	}

	// Serve time adds no layer of its own: the started server serves the
	// composed face itself. A serve-time wrap would put a layer back outside
	// the file server -- the shape that instrumented the statically served
	// paths -- so the served handler must be exactly the composed face.
	served, composed := reflect.TypeOf(face.server.Handler), reflect.TypeOf(face.handler)
	if served != composed || reflect.ValueOf(face.server.Handler).Pointer() != reflect.ValueOf(face.handler).Pointer() {
		t.Errorf("the served handler is %v while the composed face is %v; serve time must serve the composed face itself, with no layer added outside the SPA", served, composed)
	}
}
