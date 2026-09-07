package objectstoretest

import (
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// TestAssertSurvivesRestart_PassesForAStoreWhoseDataOutlivesItsRestart
// drives the protocol against the local store over one directory that every
// factory call reopens — the model of a genuinely durable store on a tiny
// scale (store, "restart", read back through a fresh handle over the same
// backing data) — and requires it to pass.
func TestAssertSurvivesRestart_PassesForAStoreWhoseDataOutlivesItsRestart(t *testing.T) {
	dir := t.TempDir()
	// restart is a no-op: the directory never goes away, the way a durable
	// backend's data does not go away when its service restarts.
	if err := assertSurvivesRestartCore(func() pkgcore.ObjectStore {
		return pkgcore.NewLocalObjectStore(dir)
	}, func() {}, conformKey(t, "survives-teeth-pass")); err != nil {
		t.Fatalf("AssertSurvivesRestart rejected a store whose data outlives its restart: %v", err)
	}
}

// TestAssertSurvivesRestart_RejectsAStoreWhoseDataDiesWithItsProcess pins
// that the protocol can fail: the factory here returns a fresh local store
// over a fresh throwaway directory on every call — the shape of an
// implementation whose data lives only inside one process, which is exactly
// what declaring SurvivesRestart over such a backend would claim
// dishonestly (the default objectstore.local's throwaway-temp-directory
// shape, on a unit-test scale). The protocol must reject it.
func TestAssertSurvivesRestart_RejectsAStoreWhoseDataDiesWithItsProcess(t *testing.T) {
	err := assertSurvivesRestartCore(func() pkgcore.ObjectStore {
		return pkgcore.NewLocalObjectStore(t.TempDir())
	}, func() {}, conformKey(t, "survives-teeth-reject"))
	if err == nil {
		t.Fatal("AssertSurvivesRestart accepted a store whose data dies with its process: the protocol has no teeth")
	}
}

// The three tests below pin the call-shape guards the caps parameter and
// the nil checks enforce: the protocol must be called with caps that
// declare SurvivesRestart, and with a non-nil factory and restart closure.
// These guards are what make "the declaration alone, with no protocol run,
// and a protocol run with the wrong declaration" both refuse themselves
// instead of silently verifying nothing.

func TestCheckSurvivesRestartCall_RefusesCapsWithoutSurvivesRestart(t *testing.T) {
	factory := func() pkgcore.ObjectStore { return pkgcore.NewLocalObjectStore(t.TempDir()) }
	restart := func() {}
	for _, caps := range []pkgcore.Capability{0, pkgcore.MultiReplicaSafe} {
		if err := checkSurvivesRestartCall(caps, factory, restart); err == nil {
			t.Errorf("checkSurvivesRestartCall(%v) accepted caps that do not declare SurvivesRestart, want a refusal", caps)
		}
	}
}

func TestCheckSurvivesRestartCall_AcceptsCapsDeclaringSurvivesRestart(t *testing.T) {
	if err := checkSurvivesRestartCall(pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart,
		func() pkgcore.ObjectStore { return pkgcore.NewLocalObjectStore(t.TempDir()) }, func() {}); err != nil {
		t.Errorf("checkSurvivesRestartCall(MultiReplicaSafe|SurvivesRestart) error = %v, want nil", err)
	}
}

func TestCheckSurvivesRestartCall_RefusesNilFactoryAndNilRestart(t *testing.T) {
	caps := pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart
	restart := func() {}
	if err := checkSurvivesRestartCall(caps, nil, restart); err == nil {
		t.Error("checkSurvivesRestartCall() accepted a nil factory, want a refusal")
	}
	if err := checkSurvivesRestartCall(caps, func() pkgcore.ObjectStore { return pkgcore.NewLocalObjectStore(t.TempDir()) }, nil); err == nil {
		t.Error("checkSurvivesRestartCall() accepted a nil restart, want a refusal: a SurvivesRestart declaration must be verified against a genuine restart of the state-holding service, never against nothing")
	}
}
