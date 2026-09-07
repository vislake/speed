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
// outlives a restart of whatever process holds it — the property the
// SurvivesRestart capability bit declares about an implementation when it
// registers (see pkgcore.Capability's doc comment). It is the
// contract-suite form of the capability check: Kernel.Bootstrap compares
// declarations against the deployment mode's requirements and never looks
// at behaviour, so an implementation that declares SurvivesRestart over a
// backing store that does not actually survive a restart sails through
// assembly; this protocol checks the declaration against the behaviour.
//
// The protocol is three steps, driven entirely by the caller: write a
// value through a store built by factory, call restart to stop and start
// whatever holds the data — the real process or container, for an
// integration leg, since only a genuine restart distinguishes durable
// state from state that merely looks durable inside one process — and then
// read the value back through a store built by factory again. factory is
// called twice: once before and once after the restart, and must return a
// store connected to the same backing data both times (for a container
// restart, the post-restart call returns a store over a fresh connection
// to the restarted server). restart is called exactly once, between the
// two factory calls, and must not return until the backing data is ready
// to serve reads again (a reconnect wait, where the client reconnects
// automatically).
//
// A value that comes back missing or different after the restart fails the
// test: the implementation's declared SurvivesRestart is not real. The
// caller decides whether to run this protocol at all — it must be run for
// every implementation that declares SurvivesRestart and must not be run
// for one that honestly does not (kv.memcached, whose register.go documents
// the absence; its leg runs AssertDoesNotSurviveRestart instead) — which
// is what ties the declaration to a verification it cannot fake.
//
// Keys are derived from the test's name (see conformKey), so an
// AssertSurvivesRestart call never collides with another test's keys on a
// shared backing store.
func AssertSurvivesRestart(t *testing.T, factory func() pkgcore.KVStore, restart func()) {
	t.Helper()
	if err := assertSurvivesRestartCore(factory, restart, conformKey(t, "survives-restart")); err != nil {
		t.Error(err)
	}
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
		return errors.New("Get() after the restart found = false, want true: the value written before the restart did not survive it, so the declared SurvivesRestart capability is not real")
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
func AssertDoesNotSurviveRestart(t *testing.T, factory func() pkgcore.KVStore, restart func()) {
	t.Helper()
	if err := assertDoesNotSurviveRestartCore(factory, restart, conformKey(t, "does-not-survive-restart")); err != nil {
		t.Error(err)
	}
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
