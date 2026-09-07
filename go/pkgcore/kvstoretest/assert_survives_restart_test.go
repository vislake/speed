package kvstoretest

import (
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// TestAssertSurvivesRestart_PassesForAStoreWhoseDataOutlivesItsRestart
// drives the protocol against the in-memory store with a restart that
// keeps the same backing map — the model of a genuinely durable store on a
// tiny scale (write, "restart", read back through the same data) — and
// requires it to pass.
func TestAssertSurvivesRestart_PassesForAStoreWhoseDataOutlivesItsRestart(t *testing.T) {
	backing := pkgcore.NewMemoryKVStore()
	// restart is a no-op: the single backing store never goes away, the way
	// a durable backend's data does not go away when its process restarts.
	if err := assertSurvivesRestartCore(func() pkgcore.KVStore {
		return backing
	}, func() {}, "kvstoretest:survives-teeth-pass"); err != nil {
		t.Fatalf("AssertSurvivesRestart rejected a store whose data outlives its restart: %v", err)
	}
}

// TestAssertSurvivesRestart_RejectsAStoreWhoseDataDiesWithItsProcess pins
// that the protocol can fail: the factory here returns a fresh in-memory
// store on every call — the shape of an implementation whose data lives
// only inside one process, which is exactly what declaring SurvivesRestart
// over such a backend would claim dishonestly (the defect shape of the
// kv.nats MemoryStorage-adoption case, on a unit-test scale). The protocol
// must reject it.
func TestAssertSurvivesRestart_RejectsAStoreWhoseDataDiesWithItsProcess(t *testing.T) {
	err := assertSurvivesRestartCore(func() pkgcore.KVStore {
		return pkgcore.NewMemoryKVStore()
	}, func() {}, "kvstoretest:survives-teeth-reject")
	if err == nil {
		t.Fatal("AssertSurvivesRestart accepted a store whose data dies with its process: the protocol has no teeth")
	}
}

// TestAssertDoesNotSurviveRestart_PassesForAStoreWhoseDataDiesWithItsProcess
// drives the counterpart protocol against the same fresh-store-per-call
// shape and requires it to pass: an implementation that does not declare
// SurvivesRestart must not be expected to survive, and the protocol that
// pins the loss must accept the loss.
func TestAssertDoesNotSurviveRestart_PassesForAStoreWhoseDataDiesWithItsProcess(t *testing.T) {
	if err := assertDoesNotSurviveRestartCore(func() pkgcore.KVStore {
		return pkgcore.NewMemoryKVStore()
	}, func() {}, "kvstoretest:not-survives-teeth-pass"); err != nil {
		t.Fatalf("AssertDoesNotSurviveRestart rejected a store whose data dies with its process: %v", err)
	}
}

// The four tests below pin the declaration-to-protocol coupling the caps
// parameters enforce: the positive protocol must be called with caps that
// declare SurvivesRestart and the negative one with caps that do not, and
// neither may be called with a nil factory or restart closure. These
// guards are what make "the declaration alone, with no protocol run, and a
// protocol run with the wrong declaration" both refuse themselves instead
// of silently verifying nothing.

func TestCheckSurvivesRestartCall_RefusesCapsWithoutSurvivesRestart(t *testing.T) {
	factory := func() pkgcore.KVStore { return pkgcore.NewMemoryKVStore() }
	restart := func() {}
	for _, caps := range []pkgcore.Capability{0, pkgcore.MultiReplicaSafe} {
		if err := checkSurvivesRestartCall(caps, factory, restart); err == nil {
			t.Errorf("checkSurvivesRestartCall(%v) accepted caps that do not declare SurvivesRestart, want a refusal", caps)
		}
	}
}

func TestCheckSurvivesRestartCall_AcceptsCapsDeclaringSurvivesRestart(t *testing.T) {
	if err := checkSurvivesRestartCall(pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart,
		func() pkgcore.KVStore { return pkgcore.NewMemoryKVStore() }, func() {}); err != nil {
		t.Errorf("checkSurvivesRestartCall(MultiReplicaSafe|SurvivesRestart) error = %v, want nil", err)
	}
}

func TestCheckSurvivesRestartCall_RefusesNilFactoryAndNilRestart(t *testing.T) {
	caps := pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart
	restart := func() {}
	if err := checkSurvivesRestartCall(caps, nil, restart); err == nil {
		t.Error("checkSurvivesRestartCall() accepted a nil factory, want a refusal")
	}
	if err := checkSurvivesRestartCall(caps, func() pkgcore.KVStore { return pkgcore.NewMemoryKVStore() }, nil); err == nil {
		t.Error("checkSurvivesRestartCall() accepted a nil restart, want a refusal: a SurvivesRestart declaration must be verified against a genuine restart of the state-holding service, never against nothing")
	}
}

func TestCheckDoesNotSurviveRestartCall_RefusesCapsDeclaringSurvivesRestart(t *testing.T) {
	factory := func() pkgcore.KVStore { return pkgcore.NewMemoryKVStore() }
	restart := func() {}
	for _, caps := range []pkgcore.Capability{0, pkgcore.MultiReplicaSafe} {
		if err := checkDoesNotSurviveRestartCall(caps, factory, restart); err != nil {
			t.Errorf("checkDoesNotSurviveRestartCall(%v) error = %v, want nil for caps without SurvivesRestart", caps, err)
		}
	}
	if err := checkDoesNotSurviveRestartCall(pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart, factory, restart); err == nil {
		t.Error("checkDoesNotSurviveRestartCall(MultiReplicaSafe|SurvivesRestart) accepted caps declaring SurvivesRestart, want a refusal")
	}
	if err := checkDoesNotSurviveRestartCall(pkgcore.MultiReplicaSafe, nil, restart); err == nil {
		t.Error("checkDoesNotSurviveRestartCall() accepted a nil factory, want a refusal")
	}
	if err := checkDoesNotSurviveRestartCall(pkgcore.MultiReplicaSafe, factory, nil); err == nil {
		t.Error("checkDoesNotSurviveRestartCall() accepted a nil restart, want a refusal")
	}
}
