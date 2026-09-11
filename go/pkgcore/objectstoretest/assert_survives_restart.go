package objectstoretest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// AssertSurvivesRestart verifies that an object stored through an
// ObjectStore outlives a restart of whatever service holds it — the
// property the SurvivesRestart capability bit declares about an
// implementation when it registers (see pkgcore.Capability's doc comment).
// It is the contract-suite form of the capability check, the ObjectStore
// twin of kvstoretest.AssertSurvivesRestart: the assembly compares
// declarations against the deployment mode's requirements and never looks
// at behaviour, so an implementation that declares SurvivesRestart over a
// backing store that does not actually survive a restart sails through
// assembly; this protocol checks the declaration against the behaviour.
//
// caps must carry the capability bits the implementation under test
// declares about itself — the same bits its component descriptor declares
// and its component_test.go pins to the package's exported Capabilities
// constant — and it must declare
// SurvivesRestart: this protocol exists to verify that bit, so a call
// whose caps lack it is refused outright rather than run over an
// implementation that never made the claim (a caps-less protocol call is
// how a declaration and its verification silently come apart, which is
// exactly the gap this signature closes).
//
// The protocol is three steps, driven entirely by the caller: store a
// payload through an ObjectStore built by factory, call restart to stop and
// start whatever holds the data — the real process or container, for an
// integration leg, since only a genuine restart of the state-holding
// service distinguishes durable state from state that merely looks durable
// inside one process (restarting the application process alone proves
// nothing, per pkgcore.Capability's own doc comment) — and then read the
// payload back through an ObjectStore built by factory again. factory is
// called twice: once before and once after the restart, and must return a
// store connected to the same backing data both times (for a container
// restart, the post-restart call returns a store over a fresh connection
// to the restarted service). restart is called exactly once, between the
// two factory calls, and must not return until the backing data is ready
// to serve reads again.
//
// A payload that comes back missing or different after the restart fails
// the test: the implementation's declared SurvivesRestart is not real. The
// caller decides whether to run this protocol at all only through the caps
// it passes — run it, with SurvivesRestart declared, for every
// implementation that declares the bit (objectstore.s3's integration leg
// does, against a genuine restart of its real S3-compatible container) —
// which is what ties the declaration to a verification it cannot fake.
//
// Keys are derived from the test's name (see conformKey), so an
// AssertSurvivesRestart call never collides with another test's keys on a
// shared backing store.
func AssertSurvivesRestart(t *testing.T, caps pkgcore.Capability, factory func() pkgcore.ObjectStore, restart func()) {
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
// over an implementation that never made the claim), and factory and
// restart must be non-nil. It returns an error instead of failing a test
// directly so the wrapper's own tests can pin each refusal.
func checkSurvivesRestartCall(caps pkgcore.Capability, factory func() pkgcore.ObjectStore, restart func()) error {
	if !caps.Has(pkgcore.SurvivesRestart) {
		return fmt.Errorf("AssertSurvivesRestart caps = %v, want SurvivesRestart declared: the survives-restart protocol verifies a SurvivesRestart declaration, so it must be called with the capability bits the implementation declares", caps)
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
func assertSurvivesRestartCore(factory func() pkgcore.ObjectStore, restart func(), key string) error {
	payload := []byte("objectstoretest survives-restart payload")

	storeBefore := factory()
	if storeBefore == nil {
		return errors.New("factory() before the restart returned a nil store")
	}
	if err := storeBefore.PutObject(context.Background(), key, bytes.NewReader(payload)); err != nil {
		return fmt.Errorf("PutObject() before the restart error = %w, want nil", err)
	}

	restart()

	storeAfter := factory()
	if storeAfter == nil {
		return errors.New("factory() after the restart returned a nil store")
	}

	reader, err := storeAfter.GetObject(context.Background(), key)
	if err != nil {
		return fmt.Errorf("GetObject() after the restart error = %w, want nil", err)
	}
	defer func(r io.ReadCloser) { _ = r.Close() }(reader)
	got, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("read the object body after the restart: %w", err)
	}
	if !bytes.Equal(got, payload) {
		return fmt.Errorf("GetObject() after the restart object body = %q, want %q: the object stored before the restart of the state-holding service did not survive it, so the declared SurvivesRestart capability is not real", got, payload)
	}

	// The restarted store must be fully operational, not merely readable:
	// store a second payload through it and read it back.
	secondKey := key + ".post-restart"
	secondPayload := []byte("objectstoretest post-restart payload")
	if err = storeAfter.PutObject(context.Background(), secondKey, bytes.NewReader(secondPayload)); err != nil {
		return fmt.Errorf("PutObject() after the restart error = %w, want nil", err)
	}
	reader, err = storeAfter.GetObject(context.Background(), secondKey)
	if err != nil {
		return fmt.Errorf("GetObject() of a post-restart write error = %w, want nil", err)
	}
	defer func(r io.ReadCloser) { _ = r.Close() }(reader)
	got, err = io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("read the post-restart object body: %w", err)
	}
	if !bytes.Equal(got, secondPayload) {
		return fmt.Errorf("GetObject() of a post-restart write object body = %q, want %q: the restarted store is not fully operational", got, secondPayload)
	}
	return nil
}
