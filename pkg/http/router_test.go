package http

import (
	"errors"
	"fmt"
	nethttp "net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/vislake/speed/pkg/core"
)

// testRouter builds a router over the named endpoints, with the gate still
// shut, which is the state New leaves it in.
func testRouter(names ...string) *router {
	settings := make([]endpointSettings, 0, len(names))
	for _, name := range names {
		settings = append(settings, testSettings(name))
	}
	return newRouter(settings, func() Engine { return nethttp.NewServeMux() })
}

// endpointOf takes an endpoint out, failing the test if it is not there.
func endpointOf(t *testing.T, r *router, name string) Endpoint {
	t.Helper()
	e, err := r.Endpoint(name)
	if err != nil {
		t.Fatalf("endpoint %q: %v", name, err)
	}
	return e
}

// mustContain fails with the whole text when a fragment the message has to
// carry is missing.
func mustContain(t *testing.T, text, fragment, why string) {
	t.Helper()
	if !strings.Contains(text, fragment) {
		t.Errorf("the message does not carry %q, which is %s.\nThe message was: %s", fragment, why, text)
	}
}

// TestRouteBeforeGateOpensPanics pins the closed half of the gate. A
// registration made before the Init stage is a wiring mistake, and the surface
// returns nothing, so the panic is the only way it can be reported at all.
func TestRouteBeforeGateOpensPanics(t *testing.T) {
	r := testRouter("public")
	e := endpointOf(t, r, "public")

	text := wantPanic(t, "Route before the gate opened", func() {
		e.Route("GET /things", nethttp.NotFoundHandler())
	})
	mustContain(t, text, "public", "the endpoint the call was made on")
	mustContain(t, text, "Init", "the stage registrations are accepted in")
	mustContain(t, text, "Move the call into the module's Init callback", "the fixing action")
}

// TestUseBeforeGateOpensPanics is the same gate seen from the middleware side.
func TestUseBeforeGateOpensPanics(t *testing.T) {
	r := testRouter("public")
	e := endpointOf(t, r, "public")

	text := wantPanic(t, "Use before the gate opened", func() {
		e.Use(Middleware{Name: "trace", Wrap: passThrough})
	})
	mustContain(t, text, "Use", "the call that was refused")
	mustContain(t, text, "Init", "the stage registrations are accepted in")
}

// TestRegistrationInsideInitSucceeds is the open half: the window the gate
// leaves open really does accept both kinds of registration.
func TestRegistrationInsideInitSucceeds(t *testing.T) {
	r := testRouter("public")
	e := endpointOf(t, r, "public")
	r.open()

	e.Route("GET /things", nethttp.NotFoundHandler())
	e.Use(Middleware{Name: "trace", Wrap: passThrough})

	inner := r.endpoints["public"]
	if len(inner.routes) != 1 || inner.routes[0].pattern != "GET /things" {
		t.Errorf("the route was not recorded: %+v", inner.routes)
	}
	if len(inner.layers) != 1 || inner.layers[0].mw.Name != "trace" {
		t.Errorf("the middleware was not recorded: %+v", inner.layers)
	}
}

// TestRegistrationAfterSealPanics pins the other end of the window. A
// registration arriving once the chains are due to be assembled reaches no
// chain, and being told so is the whole difference between a route that is
// missing and a route that is missing silently.
func TestRegistrationAfterSealPanics(t *testing.T) {
	r := testRouter("public")
	e := endpointOf(t, r, "public")
	r.open()
	r.seal()

	text := wantPanic(t, "Route after the gate was sealed", func() {
		e.Route("GET /things", nethttp.NotFoundHandler())
	})
	mustContain(t, text, "after the Init stage", "which side of the window the call fell on")
	mustContain(t, text, "assembled once in Serve", "why a late registration reaches nothing")

	useText := wantPanic(t, "Use after the gate was sealed", func() {
		e.Use(Middleware{Name: "trace", Wrap: passThrough})
	})
	mustContain(t, useText, "after the Init stage", "which side of the window the call fell on")
}

// TestUnknownEndpointNamesAvailableEndpoints pins the failure a misspelled
// endpoint name produces. It is an error rather than a panic because the
// endpoint set is configuration, and the text lists the names that do exist,
// which is the fixing action.
func TestUnknownEndpointNamesAvailableEndpoints(t *testing.T) {
	r := testRouter("public", "admin")

	got, err := r.Endpoint("piblic")
	if got != nil {
		t.Errorf("an unknown endpoint name handed back an endpoint: %#v", got)
	}
	if !errors.Is(err, ErrUnknownEndpoint) {
		t.Fatalf("the error is not ErrUnknownEndpoint: %v", err)
	}
	mustContain(t, err.Error(), `"piblic"`, "the name that was asked for")
	mustContain(t, err.Error(), `"public"`, "one of the names that do exist")
	mustContain(t, err.Error(), `"admin"`, "the other name that does exist")
}

// TestUnknownEndpointWithNoEndpointsSaysSo pins the empty case: "the endpoints
// are" followed by nothing reads as a truncated message rather than as the
// state it describes.
func TestUnknownEndpointWithNoEndpointsSaysSo(t *testing.T) {
	_, err := testRouter().Endpoint("public")
	if !errors.Is(err, ErrUnknownEndpoint) {
		t.Fatalf("the error is not ErrUnknownEndpoint: %v", err)
	}
	mustContain(t, err.Error(), "declares no endpoint at all", "the state the message describes")
}

// TestNilWrapPanics pins the layer that would wrap nothing. Handing back an
// error instead would let a registrant drop it and carry on believing the layer
// is in the chain.
func TestNilWrapPanics(t *testing.T) {
	r := testRouter("public")
	e := endpointOf(t, r, "public")
	r.open()

	text := wantPanic(t, "Use with a nil Wrap", func() {
		e.Use(Middleware{Name: "trace"})
	})
	mustContain(t, text, "trace", "the layer that was registered")
	mustContain(t, text, "nil Wrap", "what was wrong with it")
	mustContain(t, text, "func(http.Handler) http.Handler", "the shape Wrap has to have")
}

// TestNilHandlerPanics is the same class on the route side.
func TestNilHandlerPanics(t *testing.T) {
	r := testRouter("public")
	e := endpointOf(t, r, "public")
	r.open()

	text := wantPanic(t, "Route with a nil handler", func() { e.Route("GET /things", nil) })
	mustContain(t, text, "GET /things", "the pattern that was registered")
}

// TestMalformedPatternPanicsAtTheCall pins that a pattern the engine refuses on
// its own is the call-site mistake it is, named where it was written.
//
// It is knowable from this one registration, with nothing else mounted, which
// is what separates it from a conflict: a conflict is a property of the set and
// only exists once the chain is assembled. Left to be discovered in Serve, a
// malformed pattern comes back as a clash between two registrations, so a
// caller reading ErrRouteConflict goes looking for a second registration that
// is not there.
func TestMalformedPatternPanicsAtTheCall(t *testing.T) {
	for _, c := range []struct {
		pattern string
		reason  string
	}{
		{"/{", "bad wildcard segment"},
		{"GET /things/{id}/{id}", `duplicate wildcard name "id"`},
		{"", "invalid pattern"},
	} {
		t.Run(c.pattern, func(t *testing.T) {
			r := testRouter("public")
			e := endpointOf(t, r, "public")
			r.open()

			text := wantPanic(t, "Route with a malformed pattern", func() {
				e.Route(c.pattern, nethttp.NotFoundHandler())
			})
			mustContain(t, text, fmt.Sprintf("%q", c.pattern), "the pattern that was registered")
			mustContain(t, text, `endpoint "public"`, "the endpoint it was registered on")
			mustContain(t, text, c.reason, "what the engine said is wrong with it")
		})
	}
}

// TestMalformedPatternIsNotARouteConflict is the other half of the
// classification: a single malformed registration must not reach the assembly
// as a clash between two of them.
func TestMalformedPatternIsNotARouteConflict(t *testing.T) {
	r := testRouter("public")
	e := endpointOf(t, r, "public")
	r.open()

	wantPanic(t, "Route with a malformed pattern", func() {
		e.Route("/{", nethttp.NotFoundHandler())
	})
	r.seal()

	if got := len(r.endpoints["public"].routes); got != 0 {
		t.Fatalf("a refused pattern was recorded on the endpoint: %d routes", got)
	}
}

// TestIllegalConstraintTokenPanicsAtTheRegistration pins where an illegal
// capability token is reported. Checked when the chain is ordered instead, the
// panic would name the endpoint but not the call that wrote the declaration,
// and Serve runs long after the registration returned.
func TestIllegalConstraintTokenPanicsAtTheRegistration(t *testing.T) {
	r := testRouter("public")
	e := endpointOf(t, r, "public")
	r.open()

	text := wantPanic(t, "Use with an untyped nil in After", func() {
		e.Use(Middleware{Name: "trace", After: []any{nil}, Wrap: passThrough})
	})
	mustContain(t, text, `endpoint "public"`, "the endpoint the declaration was made on")
	mustContain(t, text, `middleware "trace"`, "the layer that declared it")
	mustContain(t, text, "After[0]", "which declaration in which field")

	if len(r.endpoints["public"].layers) != 0 {
		t.Error("the layer with the illegal declaration was recorded anyway")
	}
}

// TestIllegalProvidesTokenPanicsNamingTheDeclaration pins that Provides is
// checked at the call, alongside After and Before, and with the same locating
// text: which endpoint, which layer, which position in which field.
//
// It is the field that decides what every other layer's constraints resolve
// against, so a token in it that designates nothing does not merely fail to
// place this layer — it silently removes the target of everybody else's After.
// An implementation that checks After and Before and leaves Provides alone
// panics on none of the four rows below.
func TestIllegalProvidesTokenPanicsNamingTheDeclaration(t *testing.T) {
	var interfaceValue probe
	for _, c := range []struct {
		name  string
		token core.Token
		says  string
	}{
		{name: "an untyped nil", token: nil, says: "untyped nil"},
		{name: "a non-pointer", token: concreteProbe{}, says: "must be a typed nil pointer"},
		{name: "a non-nil pointer", token: &interfaceValue, says: "must be a nil pointer"},
		{name: "a pointer to a concrete type", token: (*concreteProbe)(nil), says: "must point to an interface"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := testRouter("public")
			e := endpointOf(t, r, "public")
			r.open()

			text := wantPanic(t, "Use with "+c.name+" in Provides", func() {
				e.Use(Middleware{
					Name:     "trace",
					Provides: []core.Token{(*probe)(nil), c.token},
					Wrap:     passThrough,
				})
			})
			mustContain(t, text, `endpoint "public"`, "the endpoint the declaration was made on")
			mustContain(t, text, `middleware "trace"`, "the layer that declared it")
			mustContain(t, text, "Provides[1]", "which declaration in which field")
			mustContain(t, text, c.says, "what is wrong with the token")

			if len(r.endpoints["public"].layers) != 0 {
				t.Error("the layer with the illegal declaration was recorded anyway")
			}
		})
	}
}

// TestProvidesIsRecordedOnTheLayer pins the field reaching the graph node the
// ordering reads. Dropped between Use and the record, every constraint on the
// endpoint would find no provider, and the chain would come out ordered by
// Order alone with nothing saying so.
func TestProvidesIsRecordedOnTheLayer(t *testing.T) {
	r := testRouter("public")
	e := endpointOf(t, r, "public")
	r.open()

	e.Use(Middleware{Name: "auth", Provides: []core.Token{(*probe)(nil)}, Wrap: passThrough})

	layers := r.endpoints["public"].layers
	if len(layers) != 1 {
		t.Fatalf("%d layers were recorded, want 1", len(layers))
	}
	if len(layers[0].provides) != 1 {
		t.Fatalf("the recorded layer stands for %v, and the registration named one capability",
			layers[0].provides)
	}
	if got := capabilityKey(layers[0].provides[0], "the recorded layer"); got != reflect.TypeOf((*probe)(nil)).Elem() {
		t.Errorf("the recorded layer stands for %s, and the registration named probe", got)
	}
}

// TestConcurrentRegistrationIsSafe pins the mutex. The design does not assume
// registrations come from one goroutine: Init is driven one module at a time,
// but a registrant may start goroutines of its own inside its callback.
//
// Run under -race, an unsynchronised append shows up here; the count assertion
// catches the lost update that an unsynchronised append also produces without
// the race detector.
func TestConcurrentRegistrationIsSafe(t *testing.T) {
	r := testRouter("public")
	e := endpointOf(t, r, "public")
	r.open()

	const registrants = 8
	const each = 16
	var wg sync.WaitGroup
	for i := range registrants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range each {
				e.Route(fmt.Sprintf("GET /things/%d/%d", i, j), nethttp.NotFoundHandler())
				e.Use(Middleware{Name: fmt.Sprintf("layer-%d-%d", i, j), Wrap: passThrough})
			}
		}()
	}
	wg.Wait()

	inner := r.endpoints["public"]
	if len(inner.routes) != registrants*each {
		t.Errorf("%d routes were registered and %d were recorded", registrants*each, len(inner.routes))
	}
	if len(inner.layers) != registrants*each {
		t.Errorf("%d layers were registered and %d were recorded", registrants*each, len(inner.layers))
	}
	seen := make(map[string]bool, registrants*each)
	for _, l := range inner.layers {
		if seen[l.mw.Name] {
			t.Fatalf("layer %q was recorded twice", l.mw.Name)
		}
		seen[l.mw.Name] = true
	}
}

// TestAcceptingIsFalseBeforeBinding pins the read side of the surface: it is
// not behind the gate, and an endpoint that has not bound its address yet
// reports that it is not accepting rather than panicking or claiming it is.
func TestAcceptingIsFalseBeforeBinding(t *testing.T) {
	r := testRouter("public")
	if endpointOf(t, r, "public").Accepting() {
		t.Error("an endpoint that never bound an address reports that it is accepting requests")
	}
}
