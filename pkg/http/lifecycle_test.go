package http

import (
	"context"
	"errors"
	"fmt"
	"net"
	nethttp "net/http"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/pkg/core"
	"github.com/vislake/speed/pkg/log"
)

// The tests in this file drive the module through core.New() and Run, so that
// what they observe is the assembly a host gets rather than a callback called
// by hand. The pieces they are built out of are in testsupport_test.go.
//
// Two rules hold throughout. Wherever a probe has to be on a definite side of
// this module, the side is given by an explicit dependency declaration: core
// promises dependency order and nothing more, and how it linearises what that
// leaves open is an implementation choice of its own, not a contract to pin.
// And every wait carries its own deadline, because go test's global timeout
// turns one blocked assertion into a whole-binary panic minutes later with
// nothing saying which.

// TestServeDoesNotBlockRun pins the half of the Serve contract that only an
// assembly can show: the callback returns, so the driver reaches the modules
// behind it and then the running state.
//
// The probe declares a requirement on the Router, which is what places it
// after this module; it is not "the probe whose name sorts later". A Serve
// that ran its accept loop in the calling goroutine would never let the probe
// run at all, and the wait below would end at its deadline.
func TestServeDoesNotBlockRun(t *testing.T) {
	served := make(chan struct{}, 1)
	var (
		mu        sync.Mutex
		accepting bool
		status    int
	)

	after := &stageProbe{
		name:     "probe.requires-the-router",
		requires: requiresRouter(),
		onInit: routeOn("public", "GET /things", nethttp.HandlerFunc(
			func(w nethttp.ResponseWriter, _ *nethttp.Request) {
				w.WriteHeader(nethttp.StatusTeapot)
			})),
		onServe: func(reg *core.Registry) error {
			r, err := routerFrom(reg)
			if err != nil {
				return err
			}
			addr, err := boundAddr(r, "public")
			if err != nil {
				return err
			}
			code, err := getStatus(context.Background(), addr, "/things")
			if err != nil {
				return fmt.Errorf("the endpoint refused a request from the Serve callback "+
					"of the module behind it: %w", err)
			}
			mu.Lock()
			accepting, status = r.endpoints["public"].Accepting(), code
			mu.Unlock()
			served <- struct{}{}
			return nil
		},
	}

	a := startAssembly(t, stubConfigModule(oneEndpoint("public")), testModule(), after.module())
	waitFor(t, served, "the Serve callback of the module that requires the Router")

	mu.Lock()
	gotAccepting, gotStatus := accepting, status
	mu.Unlock()
	if !gotAccepting {
		t.Error("the endpoint was not accepting by the time the next module served, so the " +
			"address was not bound in the synchronous part of Serve")
	}
	if gotStatus != nethttp.StatusTeapot {
		t.Errorf("the endpoint answered %d from the next module's Serve, not the %d the "+
			"registered handler writes", gotStatus, nethttp.StatusTeapot)
	}
	if err := a.stop(t); err != nil {
		t.Errorf("a run that served and was then cancelled came back with %v", err)
	}
}

// TestStopDoesNotMakeLaterModulesWait pins the two beats at assembly level.
// Stop closes the listener and returns; the drain it starts goes on in the
// background, and the modules stopped after this one are not held behind it.
//
// The probe delivers the Logger capability this module optionally requires,
// which is what constructs it first and therefore stops it last. What it sees
// at that moment is the whole of the property: this module's Stop has run, the
// endpoint takes nothing new, and the request still in flight has neither
// finished nor been waited for.
func TestStopDoesNotMakeLaterModulesWait(t *testing.T) {
	gate := newGateHandler(t)
	serving := make(chan struct{}, 1)
	stopped := make(chan struct{}, 1)

	type observation struct {
		accepting, httpStopped, draining, requestFinished bool
	}
	var (
		mu   sync.Mutex
		obs  observation
		addr string
	)

	logger, _ := newRecordingLogger()
	later := &stageProbe{
		name:     "probe.delivers-the-logger",
		provides: []core.Provision{{Token: (*log.Logger)(nil)}},
		product:  stubLogger{logger: logger},
		onStop: func(reg *core.Registry) error {
			r, err := routerFrom(reg)
			if err != nil {
				return err
			}
			e, ok := r.endpoints["public"]
			if !ok {
				return errors.New("endpoint \"public\" is not declared in this assembly")
			}
			httpStopped, draining := listenerState(&e.socket)
			mu.Lock()
			obs = observation{
				accepting:       e.Accepting(),
				httpStopped:     httpStopped,
				draining:        draining,
				requestFinished: len(gate.finished) > 0,
			}
			mu.Unlock()
			stopped <- struct{}{}
			return nil
		},
	}
	registrar := &stageProbe{
		name:     "probe.registers-the-held-route",
		requires: requiresRouter(),
		onInit:   routeOn("public", "GET /things", gate),
		onServe: func(reg *core.Registry) error {
			r, err := routerFrom(reg)
			if err != nil {
				return err
			}
			got, err := boundAddr(r, "public")
			if err != nil {
				return err
			}
			mu.Lock()
			addr = got
			mu.Unlock()
			serving <- struct{}{}
			return nil
		},
	}

	// Two seconds is long enough that the observation below is taken while
	// the drain is still running, and short enough that a test failing
	// before it releases the gate does not sit out the default of half a
	// minute in its cleanup.
	a := startAssembly(t,
		stubConfigModule(endpointsAt(2*time.Second, "public")),
		testModule(), later.module(), registrar.module())
	// Registered after the assembly and therefore run before its cleanup:
	// the cleanup waits for Run, and Run cannot finish while a request is
	// still held.
	t.Cleanup(gate.letGo)

	waitFor(t, serving, "the endpoint to bind")
	mu.Lock()
	at := addr
	mu.Unlock()
	go func() { _, _ = getStatus(context.Background(), at, "/things") }()
	waitFor(t, gate.entered, "the held request to reach the handler")

	a.cancel()
	waitFor(t, stopped, "the Stop callback of the module stopped after http")

	mu.Lock()
	got := obs
	mu.Unlock()
	if !got.httpStopped {
		t.Error("http's Stop had not run on the endpoint by the time the module behind it " +
			"was stopped, so the order this test is built on did not hold")
	}
	if got.accepting {
		t.Error("the endpoint was still accepting requests when the next module was stopped; " +
			"Stop has to have closed the listener before it returns")
	}
	if !got.draining {
		t.Error("the drain had already finished when the next module was stopped, so Stop " +
			"waited for it. Draining belongs to Close: waiting for it in Stop holds back " +
			"the \"stop taking new work\" of every module behind this one")
	}
	if got.requestFinished {
		t.Error("the request held in the handler had finished by the time the next module " +
			"was stopped, so this test never observed a drain in progress")
	}

	gate.letGo()
	if err := a.wait(t); err != nil {
		t.Errorf("a run whose in-flight request was released came back with %v", err)
	}
}

// TestRegistrationOutsideInitPanicsInAssembly pins the gate from inside a real
// assembly, on both of its sides at once.
//
// The probe requires the Router, so its New runs after this module's New and
// before any Migrate, and its Start runs after this module's Start. Those are
// exactly the two moments the gate is shut, and each panic has to say which
// side it is and what to do about it: a registration that only panicked with
// "not allowed" would leave the registrant to guess the stage.
func TestRegistrationOutsideInitPanicsInAssembly(t *testing.T) {
	var (
		mu        sync.Mutex
		fromNew   string
		fromStart string
	)
	// registerLate makes one registration and hands back the panic text
	// instead of letting it take the assembly down.
	//
	// The two calls use patterns of their own, so that an implementation
	// letting both through is caught by the two assertions below rather
	// than by the route conflict one repeated pattern would raise.
	registerLate := func(reg *core.Registry, pattern string) (text string, err error) {
		e, err := endpointFrom(reg, "public")
		if err != nil {
			return "", err
		}
		defer func() {
			if raised := recover(); raised != nil {
				text = fmt.Sprint(raised)
			}
		}()
		e.Route(pattern, nethttp.NotFoundHandler())
		return "", nil
	}

	outside := &stageProbe{
		name:     "probe.registers-outside-init",
		requires: requiresRouter(),
		onNew: func(reg *core.Registry) error {
			text, err := registerLate(reg, "GET /from-new")
			if err != nil {
				return err
			}
			mu.Lock()
			fromNew = text
			mu.Unlock()
			return nil
		},
		onStart: func(reg *core.Registry) error {
			text, err := registerLate(reg, "GET /from-start")
			if err != nil {
				return err
			}
			mu.Lock()
			fromStart = text
			mu.Unlock()
			return nil
		},
	}

	a := startAssembly(t, stubConfigModule(oneEndpoint("public")), testModule(), outside.module())
	if err := a.stop(t); err != nil {
		t.Fatalf("the assembly failed instead of carrying on past the two recovered "+
			"registrations: %v", err)
	}

	mu.Lock()
	early, late := fromNew, fromStart
	mu.Unlock()
	if early == "" {
		t.Error("registering from New, before any module reached Init, did not panic: the " +
			"surface returns nothing, so a registration it drops is a route nobody can reach")
	} else {
		assertNames(t, early, "\"public\"", "Route", "before the Init stage", "Init callback")
	}
	if late == "" {
		t.Error("registering from Start, after the Init stage had ended, did not panic: the " +
			"chains are assembled in Serve, so that registration would reach no chain")
	} else {
		assertNames(t, late, "\"public\"", "Route", "after the Init stage", "Init callback")
	}
}

// TestBindFailureRollsBackBoundEndpoints pins that a bind failure ends the
// startup and that the rollback really gives the ports back. All the endpoints
// share one module lifecycle, so one of them failing has to release the ones
// already listening, or a restart fails on an address the dead process holds.
func TestBindFailureRollsBackBoundEndpoints(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("taking a port to collide with: %v", err)
	}
	defer taken.Close()

	var (
		mu       sync.Mutex
		captured *router
	)
	watch := &stageProbe{
		name:     "probe.captures-the-router",
		requires: requiresRouter(),
		onInit: func(reg *core.Registry) error {
			r, rerr := routerFrom(reg)
			if rerr != nil {
				return rerr
			}
			mu.Lock()
			captured = r
			mu.Unlock()
			return nil
		},
	}

	// The endpoints are bound in name order, so "a" comes up and "b" is the
	// one that collides.
	cfg := moduleConfig{Endpoints: map[string]endpointConfig{
		"a": {Address: "127.0.0.1:0"},
		"b": {Address: taken.Addr().String()},
	}}
	a := startAssembly(t, stubConfigModule(cfg), testModule(), watch.module())

	// stop rather than wait: a startup that fails comes back on its own, so
	// the cancellation changes nothing here, while an implementation that
	// let this configuration come up reports what it returned instead of
	// sitting out the deadline with nothing to say.
	runErr := a.stop(t)
	if !errors.Is(runErr, ErrListen) {
		t.Fatalf("a startup whose second endpoint cannot bind came back with %v, which is "+
			"not a bind failure", runErr)
	}
	mustContain(t, runErr.Error(), "\"b\"", "the endpoint that could not bind")
	mustContain(t, runErr.Error(), taken.Addr().String(), "the address it could not take")

	mu.Lock()
	r := captured
	mu.Unlock()
	if r == nil {
		t.Fatal("the probe never captured the router, so nothing below was observed")
	}
	addr, err := boundAddr(r, "a")
	if err != nil {
		t.Fatalf("the endpoint bound before the failing one: %v", err)
	}
	if r.endpoints["a"].Accepting() {
		t.Error("the endpoint that did bind is still accepting after the rollback")
	}
	reopened, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the port endpoint %q had bound was not released by the rollback: %v", "a", err)
	}
	reopened.Close()
}

// TestCloseAggregatesPerEndpointDrainFailures pins that Close waits for every
// endpoint and reports all of them. One endpoint timing out neither shortens
// another's wait nor hides it: a shutdown report naming one of two stuck
// endpoints sends its reader after the wrong configuration key.
func TestCloseAggregatesPerEndpointDrainFailures(t *testing.T) {
	gateA, gateB := newGateHandler(t), newGateHandler(t)
	serving := make(chan struct{}, 1)
	var (
		mu           sync.Mutex
		addrA, addrB string
	)

	hold := &stageProbe{
		name:     "probe.holds-a-request-on-each-endpoint",
		requires: requiresRouter(),
		onInit: inOrder(
			routeOn("a", "GET /things", gateA),
			routeOn("b", "GET /things", gateB),
		),
		onServe: func(reg *core.Registry) error {
			r, err := routerFrom(reg)
			if err != nil {
				return err
			}
			gotA, err := boundAddr(r, "a")
			if err != nil {
				return err
			}
			gotB, err := boundAddr(r, "b")
			if err != nil {
				return err
			}
			mu.Lock()
			addrA, addrB = gotA, gotB
			mu.Unlock()
			serving <- struct{}{}
			return nil
		},
	}

	a := startAssembly(t,
		stubConfigModule(endpointsAt(100*time.Millisecond, "a", "b")),
		testModule(), hold.module())
	waitFor(t, serving, "both endpoints to bind")

	mu.Lock()
	atA, atB := addrA, addrB
	mu.Unlock()
	go func() { _, _ = getStatus(context.Background(), atA, "/things") }()
	go func() { _, _ = getStatus(context.Background(), atB, "/things") }()
	waitFor(t, gateA.entered, "a request in flight on endpoint \"a\"")
	waitFor(t, gateB.entered, "a request in flight on endpoint \"b\"")

	a.cancel()
	runErr := a.wait(t)
	if !errors.Is(runErr, ErrDrainTimeout) {
		t.Fatalf("a shutdown with a stuck request on each endpoint came back with %v, which "+
			"is not a drain timeout", runErr)
	}
	mustContain(t, runErr.Error(), "endpoint \"a\"", "the first endpoint that did not drain")
	mustContain(t, runErr.Error(), "endpoint \"b\"", "the second endpoint that did not drain")
	mustContain(t, runErr.Error(), "drain-timeout", "the key whose value the reader has to raise")
}

// TestMiddlewareCycleFailsARealAssembly pins that a cycle between two layers
// registered by two modules ends the startup.
//
// It is the whole path rather than the solver: each probe really delivers the
// capability its layer stands for, each registers through the public Use, and
// each points at the other's capability. With Provides absent from the
// registration surface, neither constraint reaches a provider, both are
// dropped, the graph has no edge at all and this assembly starts up clean —
// which is the state this test exists to keep from coming back.
func TestMiddlewareCycleFailsARealAssembly(t *testing.T) {
	first := &stageProbe{
		name:     "probe.delivers-a",
		requires: requiresRouter(),
		provides: []core.Provision{{Token: (*capA)(nil)}},
		product:  deliveredA{},
		onInit: useLayer("public", Middleware{
			Name:     "layer-a",
			Provides: []core.Token{(*capA)(nil)},
			After:    []core.Token{(*capB)(nil)},
			Wrap:     passThrough,
		}),
	}
	second := &stageProbe{
		name:     "probe.delivers-b",
		requires: requiresRouter(),
		provides: []core.Provision{{Token: (*capB)(nil)}},
		product:  deliveredB{},
		onInit: useLayer("public", Middleware{
			Name:     "layer-b",
			Provides: []core.Token{(*capB)(nil)},
			After:    []core.Token{(*capA)(nil)},
			Wrap:     passThrough,
		}),
	}

	a := startAssembly(t, stubConfigModule(oneEndpoint("public")),
		testModule(), first.module(), second.module())

	// stop rather than wait, for the reason given in the bind failure test:
	// an assembly that wrongly comes up then reports the nil it returned.
	runErr := a.stop(t)
	if !errors.Is(runErr, ErrMiddlewareCycle) {
		t.Fatalf("two layers each declared inside the other started up with %v; the "+
			"constraints are hard, so no order satisfies them and the startup has to end",
			runErr)
	}
	mustContain(t, runErr.Error(), "\"layer-a\"", "one member of the cycle")
	mustContain(t, runErr.Error(), "\"layer-b\"", "the other member of the cycle")
	mustContain(t, runErr.Error(), "\"public\"", "the endpoint the cycle is on")
}

// TestConstraintLandsOnItsProviderInARealAssembly pins the opposite direction:
// a constraint that does land decides the order, and Order does not get to
// overrule it.
//
// The two layers are given Order values pointing the other way, so a chain
// built by sorting on Order alone comes out reversed. That is what an
// assembly whose registration surface cannot say what a layer stands for
// falls back to, and it is the failure this pins against. Which of the two
// probes registers first does not enter into it: the constraint fixes the
// order either way, which is why no dependency is declared between them.
func TestConstraintLandsOnItsProviderInARealAssembly(t *testing.T) {
	tr := &trace{}
	serving := make(chan struct{}, 1)
	var (
		mu   sync.Mutex
		addr string
	)

	provider := &stageProbe{
		name:     "probe.delivers-a",
		requires: requiresRouter(),
		provides: []core.Provision{{Token: (*capA)(nil)}},
		product:  deliveredA{},
		onInit: inOrder(
			routeOn("public", "GET /things", nethttp.HandlerFunc(
				func(w nethttp.ResponseWriter, _ *nethttp.Request) {
					w.WriteHeader(nethttp.StatusNoContent)
				})),
			useLayer("public", Middleware{
				Name:     "outer-by-constraint",
				Provides: []core.Token{(*capA)(nil)},
				// A large Order asks for the innermost
				// position, which the constraint denies it.
				Order: 100,
				Wrap:  noteLayer(tr, "outer-by-constraint"),
			}),
		),
	}
	constrained := &stageProbe{
		name:     "probe.delivers-b",
		requires: requiresRouter(),
		provides: []core.Provision{{Token: (*capB)(nil)}},
		product:  deliveredB{},
		onInit: useLayer("public", Middleware{
			Name:     "inner-by-constraint",
			Provides: []core.Token{(*capB)(nil)},
			After:    []core.Token{(*capA)(nil)},
			// A small Order asks for the outermost position.
			Order: -100,
			Wrap:  noteLayer(tr, "inner-by-constraint"),
		}),
		onServe: func(reg *core.Registry) error {
			r, err := routerFrom(reg)
			if err != nil {
				return err
			}
			got, err := boundAddr(r, "public")
			if err != nil {
				return err
			}
			mu.Lock()
			addr = got
			mu.Unlock()
			serving <- struct{}{}
			return nil
		},
	}

	a := startAssembly(t, stubConfigModule(oneEndpoint("public")),
		testModule(), provider.module(), constrained.module())
	waitFor(t, serving, "the endpoint to bind")

	mu.Lock()
	at := addr
	mu.Unlock()
	code, err := getStatus(context.Background(), at, "/things")
	if err != nil {
		t.Fatalf("the bound endpoint refused a request: %v", err)
	}
	if code != nethttp.StatusNoContent {
		t.Errorf("the chain answered %d, not the %d the handler behind it writes",
			code, nethttp.StatusNoContent)
	}

	want := []string{"outer-by-constraint", "inner-by-constraint"}
	if got := tr.seen(); !equalStrings(got, want) {
		t.Errorf("the request went through %v; After places the second layer inside the "+
			"layer standing for the capability it names, so it has to be %v whatever the "+
			"two Order values ask for", got, want)
	}
	if err := a.stop(t); err != nil {
		t.Errorf("a run that served and was then cancelled came back with %v", err)
	}
}
