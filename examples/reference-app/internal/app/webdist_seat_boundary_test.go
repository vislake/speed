package app

// This file pins the layer boundary the WebDistDir form composes -- the
// deployment shape (APP_WEB_DIST set) the release ledger registers: the
// platform middleware declared through the http component's middleware
// face wraps only the chain's output, and the SPA file server sits OUTSIDE
// that layer (it is the link policy's outer wrapper), so the paths the
// frontend serves itself never traverse the middleware. The instrumentation
// used to be applied at serve time, around the SPA-wrapped face, so
// statically served paths were instrumented then; under this composition
// they are not, and this pin turns red if the shape moves back -- the SPA
// wrap moved under the middleware layer, the layer's application around the
// chain dropped, or a serve-time layer put back outside the file server.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	speedapp "github.com/vislake/speed/go/app"
	"github.com/vislake/speed/go/app/httpserve"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/examples/reference-app/internal/app/demo"
)

// spaProbeName names the test-only component that declares the recording
// middleware on the http component's middleware face for this pin.
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
// recording middleware to the http component's middleware face, so the
// middleware is applied exactly where the face's members are -- by
// chain.Standard, around the finished chain -- and nothing else in the
// composition is disturbed. The probe stands in for the observability
// middleware here: the boundary under test is the layer's position relative
// to the SPA file server, and the probe makes that position observable from
// the test.
func spaProbeComponent(probe *spaProbe) pkgcore.Component {
	return pkgcore.Component{
		Name:         spaProbeName,
		Capabilities: pkgcore.MultiReplicaSafe,
		Requires: []pkgcore.Requirement{
			{Token: (*pkgcore.MiddlewareRegistrar)(nil), Optional: true},
		},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &hostStep{}, nil
		},
		Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
			registrar, ok, err := pkgcore.GetOptional[pkgcore.MiddlewareRegistrar](reg)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			return registrar.Add(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					probe.record(r.URL.Path)
					next.ServeHTTP(w, r)
				})
			})
		},
	}
}

// selectBeforeApp returns the composition with name selected immediately
// before the standard chain's selector entry ("rbac"): the plan order
// follows the selection order for tied components, and the component's Init
// turn (the face write) must run before the http component's Serve stage
// reads the accumulated middleware -- every Init turn does, so the position
// only keeps the fixture's declaration order readable.
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
// (observability selected, the listener opened on an ephemeral port) with a
// frontend directory configured, one probe component added to the
// middleware face. Requests are driven both through the composed handler
// and over the live listener -- asserting the boundary from both sides: a
// chain-routed request (/healthz) traverses the middleware layer, while the
// SPA's own answers (a built asset and the client-route index fallback) do
// not. The listener-side requests also pin that serve time adds no layer of
// its own, so the SPA stays the outermost composition over the composed
// face.
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
		t.Fatalf("register the middleware probe: %v", err)
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

	face, err := pkgcore.Get[*httpserve.Face](reg)
	if err != nil {
		t.Fatalf("read the composed face: %v", err)
	}
	if !face.Listening() {
		t.Fatal("the live drive composed the face without opening its listener")
	}
	if face.Handler() == nil {
		t.Fatal("the composed face carries no handler")
	}

	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		face.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	// The chain's own traffic traverses the middleware layer: /healthz is
	// one of the SPA's declared server paths and pre-auth allowlisted in
	// the chain, so it is answered by the mux's liveness handler THROUGH
	// the layer chain.Standard applied around the chain's output.
	if rec := get(obs.HealthzPath); rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("GET %s = %d %q, want the liveness handler's 200", obs.HealthzPath, rec.Code, rec.Body.String())
	}
	if got := probe.take(); !slices.Equal(got, []string{obs.HealthzPath}) {
		t.Errorf("the middleware layer saw %v for the chain-served %s request, want exactly that path: the layer must wrap the chain output", got, obs.HealthzPath)
	}

	// The SPA's own traffic does not: a real built asset and the client-route
	// index fallback are both answered by the file server sitting OUTSIDE the
	// middleware layer, so neither may reach the probe.
	if rec := get("/assets/app.js"); rec.Code != http.StatusOK || rec.Body.String() != assetBody {
		t.Fatalf("GET /assets/app.js = %d %q, want the fixture asset's 200", rec.Code, rec.Body.String())
	}
	if got := probe.take(); len(got) != 0 {
		t.Errorf("the middleware layer saw %v for the SPA-served asset request, want none: the SPA file server must sit OUTSIDE the layer", got)
	}
	if rec := get("/deep/client/route"); rec.Code != http.StatusOK || rec.Body.String() != indexBody {
		t.Fatalf("GET /deep/client/route = %d %q, want the SPA index fallback's 200", rec.Code, rec.Body.String())
	}
	if got := probe.take(); len(got) != 0 {
		t.Errorf("the middleware layer saw %v for the SPA fallback request, want none: the SPA file server must sit OUTSIDE the layer", got)
	}

	// Serve time adds no layer of its own: the same boundary holds for
	// requests answered by the live listener -- the served handler is the
	// composed face itself, so a chain-routed request traverses the layer
	// and an SPA-served one does not, over the socket exactly as in
	// process.
	fetch := func(path string) (int, string) {
		t.Helper()
		resp, fetchErr := http.Get("http://" + face.Addr() + path)
		if fetchErr != nil {
			t.Fatalf("GET %s over the listener: %v", path, fetchErr)
		}
		defer func() { _ = resp.Body.Close() }()
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			t.Fatalf("read the %s response body: %v", path, readErr)
		}
		return resp.StatusCode, string(body)
	}
	if code, body := fetch(obs.HealthzPath); code != http.StatusOK || body != "ok" {
		t.Fatalf("GET %s over the listener = %d %q, want the liveness handler's 200", obs.HealthzPath, code, body)
	}
	if got := probe.take(); !slices.Equal(got, []string{obs.HealthzPath}) {
		t.Errorf("the middleware layer saw %v for the chain-served listener request, want exactly that path", got)
	}
	if code, body := fetch("/assets/app.js"); code != http.StatusOK || body != assetBody {
		t.Fatalf("GET /assets/app.js over the listener = %d %q, want the fixture asset's 200", code, body)
	}
	if got := probe.take(); len(got) != 0 {
		t.Errorf("the middleware layer saw %v for the SPA-served listener request, want none", got)
	}
}
