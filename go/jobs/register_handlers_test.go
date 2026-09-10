package jobs

// register_handlers_test.go is the suite for register_handlers.go's
// RegisterHandlers -- the drain primitive Wire composes. It pins the
// properties a host handing over a declaration map depends on: every entry
// lands on the queue as the very handler declared, a nil or empty map is a
// no-op, a non-handler entry is refused by job type before anything registers
// after it, and a type the queue already holds is refused with
// RegisterHandler's own duplicate sentinel.

import (
	"context"
	"strings"
	"testing"

	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// stubHandler is a Handler carrying its own job type, comparable by pointer
// identity so the suite can prove the queue holds the exact declaration it
// was handed.
type stubHandler struct{ jobType string }

func (h *stubHandler) Type() string { return h.jobType }

func (h *stubHandler) Handle(context.Context, *Job, ProgressFn) (Result, error) {
	return Result{}, nil
}

// TestRegisterHandlers_RegistersEveryDeclaredHandler is the core contract:
// after one call, every entry of the map is on the queue, keyed by the
// handler's own Type.
func TestRegisterHandlers_RegistersEveryDeclaredHandler(t *testing.T) {
	q := NewStandaloneQueue(dbtest.NewSQLite(t))
	first := &stubHandler{jobType: "reg.first"}
	second := &stubHandler{jobType: "reg.second"}

	if err := RegisterHandlers(q, map[string]any{"reg.first": first, "reg.second": second}); err != nil {
		t.Fatalf("RegisterHandlers() error = %v", err)
	}

	if got := q.handler("reg.first"); got != Handler(first) {
		t.Errorf("queue handler for %q = %#v, want the declared first handler", "reg.first", got)
	}
	if got := q.handler("reg.second"); got != Handler(second) {
		t.Errorf("queue handler for %q = %#v, want the declared second handler", "reg.second", got)
	}
}

// TestRegisterHandlers_NilAndEmptyMap_AreNoOps pins the empty case: a host
// whose declarations are nil or empty wires no handler and gets no error.
func TestRegisterHandlers_NilAndEmptyMap_AreNoOps(t *testing.T) {
	q := NewStandaloneQueue(dbtest.NewSQLite(t))

	if err := RegisterHandlers(q, nil); err != nil {
		t.Errorf("RegisterHandlers(nil map) error = %v, want nil", err)
	}
	if err := RegisterHandlers(q, map[string]any{}); err != nil {
		t.Errorf("RegisterHandlers(empty map) error = %v, want nil", err)
	}
	if len(q.handlers) != 0 {
		t.Errorf("queue carries %d handlers, want none", len(q.handlers))
	}
}

// TestRegisterHandlers_RefusesEntryThatIsNotAHandler pins the refusal
// contract: an entry that is not a jobs.Handler is a wiring bug, reported
// with the offending job type and the offending Go type -- and, because
// entries register in ascending job-type order, "reg.bad" sorts before
// "reg.good", so the failure lands before any entry registers at all.
func TestRegisterHandlers_RefusesEntryThatIsNotAHandler(t *testing.T) {
	q := NewStandaloneQueue(dbtest.NewSQLite(t))

	err := RegisterHandlers(q, map[string]any{
		"reg.good": &stubHandler{jobType: "reg.good"},
		"reg.bad":  "not a handler",
	})
	if err == nil {
		t.Fatal("RegisterHandlers() = nil, want an error for an entry that is not a jobs.Handler")
	}
	if !strings.Contains(err.Error(), "reg.bad") {
		t.Errorf("RegisterHandlers() error = %q, want it to name the offending job type %q", err, "reg.bad")
	}
	if !strings.Contains(err.Error(), "string") {
		t.Errorf("RegisterHandlers() error = %q, want it to name the offending Go type", err)
	}
	if len(q.handlers) != 0 {
		t.Errorf("queue carries %d handlers after the refusal, want none -- reg.good sorts after reg.bad", len(q.handlers))
	}
}

// TestRegisterHandlers_FailsInAscendingJobTypeOrder pins the ordering
// contract through its two consequences: with several bad entries the failure
// is deterministic (the ascending-first one), and registration is not
// transactional (an entry sorting before the failing one stays registered).
func TestRegisterHandlers_FailsInAscendingJobTypeOrder(t *testing.T) {
	q := NewStandaloneQueue(dbtest.NewSQLite(t))
	registeredAlready := &stubHandler{jobType: "reg.aaa"}

	err := RegisterHandlers(q, map[string]any{
		"reg.zzz": "not a handler",
		"reg.mmm": 42,
		"reg.aaa": registeredAlready,
	})
	if err == nil {
		t.Fatal("RegisterHandlers() = nil, want an error for the malformed entries")
	}
	if !strings.Contains(err.Error(), "reg.mmm") {
		t.Errorf("RegisterHandlers() error = %q, want the ascending-first bad entry %q", err, "reg.mmm")
	}
	if got := q.handler("reg.aaa"); got != Handler(registeredAlready) {
		t.Errorf("queue handler for %q = %#v, want the entry that sorts before the failure to stay registered", "reg.aaa", got)
	}
}

// TestRegisterHandlers_DuplicateType_IsRefused pins the duplicate contract: a
// type the queue already holds -- here a host-owned registration made
// directly -- is refused with RegisterHandler's own sentinel, wrapped and
// naming the job type, and the first registration wins.
func TestRegisterHandlers_DuplicateType_IsRefused(t *testing.T) {
	q := NewStandaloneQueue(dbtest.NewSQLite(t))
	original := &stubHandler{jobType: "reg.dup"}
	if err := q.RegisterHandler(original); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	err := RegisterHandlers(q, map[string]any{"reg.dup": &stubHandler{jobType: "reg.dup"}})
	if !apperr.HasCode(err, ErrDuplicateHandlerType.Code) {
		t.Fatalf("RegisterHandlers() error = %v, want the duplicate-handler refusal", err)
	}
	if !strings.Contains(err.Error(), "reg.dup") {
		t.Errorf("RegisterHandlers() error = %q, want it to name the duplicated job type", err)
	}
	if got := q.handler("reg.dup"); got != Handler(original) {
		t.Errorf("queue handler for %q = %#v, want the first registration to win", "reg.dup", got)
	}
}

// TestRegisterHandlers_NilQueue_Errors pins the wiring mistake that would
// otherwise panic: a nil queue returns an error naming what is missing.
func TestRegisterHandlers_NilQueue_Errors(t *testing.T) {
	err := RegisterHandlers(nil, map[string]any{"reg.x": &stubHandler{jobType: "reg.x"}})
	if err == nil {
		t.Error("RegisterHandlers() with a nil queue = nil, want an error")
	}
}
