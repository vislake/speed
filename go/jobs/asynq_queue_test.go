package jobs

import (
	"context"
	"testing"
)

// TestAsynqQueue_RegisterHandler_DuplicateType_Errors mirrors demo_queue_test.go's
// TestRegisterHandler_DuplicateType_Errors: RegisterHandler's duplicate-type
// check is pure map logic with no Redis involved, so it is unit-tested
// directly here rather than only incidentally exercised as setup in
// asynq_worker_test.go's other tests. See that file's newTestAsynqQueue for
// why a bare *AsynqQueue (no NewAsynqQueue, no Redis) is sufficient.
func TestAsynqQueue_RegisterHandler_DuplicateType_Errors(t *testing.T) {
	q := newTestAsynqQueue(t)
	h1 := NewHandlerFunc("dup", func(context.Context, *Job, ProgressFn) (Result, error) { return Result{}, nil })
	h2 := NewHandlerFunc("dup", func(context.Context, *Job, ProgressFn) (Result, error) { return Result{}, nil })

	if err := q.RegisterHandler(h1); err != nil {
		t.Fatalf("first RegisterHandler() error = %v, want nil", err)
	}
	if err := q.RegisterHandler(h2); err == nil {
		t.Fatal("second RegisterHandler() for the same Type error = nil, want ErrDuplicateHandlerType")
	}
	if got := q.handler("dup"); got == nil {
		t.Error("handler(\"dup\") = nil, want the first-registered Handler to remain in effect")
	}
}

func TestAsynqQueue_RegisterHandler_DistinctTypes_BothRegister(t *testing.T) {
	q := newTestAsynqQueue(t)
	a := NewHandlerFunc("a", func(context.Context, *Job, ProgressFn) (Result, error) { return Result{}, nil })
	b := NewHandlerFunc("b", func(context.Context, *Job, ProgressFn) (Result, error) { return Result{}, nil })

	if err := q.RegisterHandler(a); err != nil {
		t.Fatalf("RegisterHandler(a) error = %v", err)
	}
	if err := q.RegisterHandler(b); err != nil {
		t.Fatalf("RegisterHandler(b) error = %v", err)
	}
	if q.handler("a") == nil || q.handler("b") == nil {
		t.Error("both distinct Types should be independently registered")
	}
	if q.handler("no-such-type") != nil {
		t.Error("handler(\"no-such-type\") should be nil")
	}
}
