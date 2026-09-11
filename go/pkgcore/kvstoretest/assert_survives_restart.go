package kvstoretest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// AssertSurvivesRestart verifies that data written through a KVStore
// outlives a restart of whatever service holds it — the property the
// SurvivesRestart capability bit declares about an implementation when it
// registers (see pkgcore.Capability's doc comment). It is the
// contract-suite form of the capability check: the assembly compares
// declarations against the deployment mode's requirements and never looks
// at behaviour, so an implementation that declares SurvivesRestart over a
// backing store that does not actually survive a restart sails through
// assembly; this protocol checks the declaration against the behaviour.
//
// caps must carry the capability bits the implementation under test
// declares about itself — the same bits its register.go init (or the
// host's WithKVStore call) declares, which the package's own
// register_test.go pins against the registry — and it must declare
// SurvivesRestart: this protocol exists to verify that bit, so a call
// whose caps lack it is refused outright rather than run over an
// implementation that never made the claim (a caps-less protocol call is
// how a declaration and its verification silently come apart, which is
// exactly the gap this signature closes). AssertDoesNotSurviveRestart is
// the counterpart for an implementation that honestly does not declare the
// bit.
//
// The protocol is three steps, driven entirely by the caller: write a
// value through a store built by factory, call restart to stop and start
// whatever holds the data — the real process or container, for an
// integration leg, since only a genuine restart of the state-holding
// service distinguishes durable state from state that merely looks durable
// inside one process (restarting the application process alone proves
// nothing: a JetStream bucket on memory storage loses every key when the
// NATS server itself restarts, never when the client process does) — and
// then read the value back through a store built by factory again. factory
// is called twice: once before and once after the restart, and must return
// a store connected to the same backing data both times (for a container
// restart, the post-restart call returns a store over a fresh connection
// to the restarted server). restart is called exactly once, between the
// two factory calls, and must not return until the backing data is ready
// to serve reads again (a reconnect wait, where the client reconnects
// automatically).
//
// A value that comes back missing or different after the restart fails the
// test: the implementation's declared SurvivesRestart is not real. The
// caller decides whether to run this protocol at all only through the caps
// it passes — run it, with SurvivesRestart declared, for every
// implementation that declares the bit, and run AssertDoesNotSurviveRestart
// instead for one that honestly does not (kv.memcached, whose register.go
// documents the absence; its leg runs the negative protocol against a real
// container restart) — which is what ties the declaration to a
// verification it cannot fake.
//
// Keys are derived from the test's name (see conformKey), so an
// AssertSurvivesRestart call never collides with another test's keys on a
// shared backing store.
func AssertSurvivesRestart(t *testing.T, caps pkgcore.Capability, factory func() pkgcore.KVStore, restart func()) {
	t.Helper()
	if err := checkSurvivesRestartCall(caps, factory, restart); err != nil {
		t.Fatal(err)
		return
	}
	if err := assertSurvivesRestartCore(factory, restart, conformKey(t, "survives-restart")); err != nil {
		t.Errorf("%v", err)
	}
}

// checkSurvivesRestartCall validates the pieces of an
// AssertSurvivesRestart call that cannot be validated at the type level:
// caps must declare SurvivesRestart (this protocol exists to verify that
// bit, so a call whose caps lack it is refused outright rather than run
// over an implementation that never made the claim — a caps-less protocol
// call is how a declaration and its verification silently come apart,
// which is exactly the gap the caps parameter closes), and factory and
// restart must be non-nil. It returns an error instead of failing a test
// directly so the wrapper's own tests can pin each refusal.
func checkSurvivesRestartCall(caps pkgcore.Capability, factory func() pkgcore.KVStore, restart func()) error {
	if !caps.Has(pkgcore.SurvivesRestart) {
		return fmt.Errorf("AssertSurvivesRestart caps = %v, want SurvivesRestart declared: the survives-restart protocol verifies a SurvivesRestart declaration, so it must be called with the capability bits the implementation declares (an implementation that does not declare the bit runs AssertDoesNotSurviveRestart instead)", caps)
	}
	if factory == nil {
		return errors.New("AssertSurvivesRestart factory is nil, want a store factory connected to the backing data under test")
	}
	if restart == nil {
		return errors.New("AssertSurvivesRestart restart is nil, want a closure that stops and starts the state-holding service: a SurvivesRestart declaration must be verified against a genuine restart of that service, never against nothing")
	}
	return nil
}

// assertSurvivesRestartCore is AssertSurvivesRestart's error-returning
// core, so the wrapper's own tests can drive the protocol against a
// deliberately non-surviving store and require the error (see
// assert_survives_restart_test.go).
func assertSurvivesRestartCore(factory func() pkgcore.KVStore, restart func(), key string) error {
	payload := []byte("kvstoretest survives-restart payload")

	storeBefore := factory()
	if storeBefore == nil {
		return errors.New("factory() before the restart returned a nil store")
	}
	if err := storeBefore.Set(context.Background(), key, payload, 0); err != nil {
		return fmt.Errorf("Set() before the restart error = %w, want nil", err)
	}

	restart()

	storeAfter := factory()
	if storeAfter == nil {
		return errors.New("factory() after the restart returned a nil store")
	}

	got, found, err := storeAfter.Get(context.Background(), key)
	if err != nil {
		return fmt.Errorf("Get() after the restart error = %w, want nil", err)
	}
	if !found {
		return errors.New("Get() after the restart found = false, want true: the value written before the restart of the state-holding service did not survive it, so the declared SurvivesRestart capability is not real")
	}
	if !bytes.Equal(got, payload) {
		return fmt.Errorf("Get() after the restart value = %q, want %q", got, payload)
	}

	// The restarted store must be fully operational, not merely readable:
	// write a second value through it and read it back.
	secondKey := key + ".post-restart"
	secondPayload := []byte("kvstoretest post-restart payload")
	if err = storeAfter.Set(context.Background(), secondKey, secondPayload, 0); err != nil {
		return fmt.Errorf("Set() after the restart error = %w, want nil", err)
	}
	got, found, err = storeAfter.Get(context.Background(), secondKey)
	if err != nil || !found || !bytes.Equal(got, secondPayload) {
		return fmt.Errorf("Get() of a post-restart write = (%q, %t, %w), want (%q, true, nil): the restarted store is not fully operational", got, found, err, secondPayload)
	}
	return nil
}

// AssertDoesNotSurviveRestart is AssertSurvivesRestart's counterpart for an
// implementation that honestly does not declare SurvivesRestart: it
// verifies that a value written before the restart is gone afterwards,
// pinning the factual basis of the non-declaration so that a future change
// that declares SurvivesRestart over the same backing store contradicts a
// verified loss rather than an unexamined assumption. The protocol is
// otherwise identical: factory is called once before and once after
// restart, which the caller runs exactly once between the two calls.
//
// caps must carry the capability bits the implementation declares, and it
// must NOT declare SurvivesRestart: the negative protocol's verified loss
// would directly contradict a SurvivesRestart declaration, so a call whose
// caps declare the bit is refused outright — an implementation that
// declares SurvivesRestart runs AssertSurvivesRestart instead.
func AssertDoesNotSurviveRestart(t *testing.T, caps pkgcore.Capability, factory func() pkgcore.KVStore, restart func()) {
	t.Helper()
	if err := checkDoesNotSurviveRestartCall(caps, factory, restart); err != nil {
		t.Fatal(err)
		return
	}
	if err := assertDoesNotSurviveRestartCore(factory, restart, conformKey(t, "does-not-survive-restart")); err != nil {
		t.Errorf("%v", err)
	}
}

// checkDoesNotSurviveRestartCall mirrors checkSurvivesRestartCall for the
// negative protocol: caps must NOT declare SurvivesRestart — the negative
// protocol's verified loss would directly contradict a SurvivesRestart
// declaration, so a call whose caps declare the bit is refused outright —
// and factory and restart must be non-nil.
func checkDoesNotSurviveRestartCall(caps pkgcore.Capability, factory func() pkgcore.KVStore, restart func()) error {
	if caps.Has(pkgcore.SurvivesRestart) {
		return fmt.Errorf("AssertDoesNotSurviveRestart caps = %v, want SurvivesRestart absent: the does-not-survive protocol pins the factual basis of an honest non-declaration, so it must be called only with the capability bits an implementation that never declares SurvivesRestart declares (one that declares it runs AssertSurvivesRestart instead)", caps)
	}
	if factory == nil {
		return errors.New("AssertDoesNotSurviveRestart factory is nil, want a store factory connected to the backing data under test")
	}
	if restart == nil {
		return errors.New("AssertDoesNotSurviveRestart restart is nil, want a closure that stops and starts the state-holding service")
	}
	return nil
}

// assertDoesNotSurviveRestartCore is AssertDoesNotSurviveRestart's
// error-returning core, mirroring assertSurvivesRestartCore.
func assertDoesNotSurviveRestartCore(factory func() pkgcore.KVStore, restart func(), key string) error {
	storeBefore := factory()
	if storeBefore == nil {
		return errors.New("factory() before the restart returned a nil store")
	}
	if err := storeBefore.Set(context.Background(), key, []byte("ephemeral"), 0); err != nil {
		return fmt.Errorf("Set() before the restart error = %w, want nil", err)
	}
	if _, found, err := storeBefore.Get(context.Background(), key); err != nil || !found {
		return fmt.Errorf("Get() before the restart = (found=%v, err=%w), want (true, nil)", found, err)
	}

	restart()

	storeAfter := factory()
	if storeAfter == nil {
		return errors.New("factory() after the restart returned a nil store")
	}
	if _, found, err := storeAfter.Get(context.Background(), key); err != nil {
		return fmt.Errorf("Get() after the restart error = %w, want nil", err)
	} else if found {
		return errors.New("Get() after the restart found = true, want false: this implementation does not declare SurvivesRestart, so its data must not be expected to survive a restart")
	}
	return nil
}
