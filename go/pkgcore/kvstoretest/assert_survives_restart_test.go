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
