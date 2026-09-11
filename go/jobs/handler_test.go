package jobs

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNewHandlerFunc_DelegatesTypeAndHandle(t *testing.T) {
	wantResult := Result{Data: []byte("ok")}
	var gotJob *Job
	var gotPct int
	var gotMsg string

	h := NewHandlerFunc("widgets.resize", func(_ context.Context, job *Job, progress ProgressFn) (Result, error) {
		gotJob = job
		progress(50, "halfway")
		return wantResult, nil
	})

	if got := h.Type(); got != "widgets.resize" {
		t.Errorf("Type() = %q, want %q", got, "widgets.resize")
	}

	job := &Job{ID: "job-1"}
	result, err := h.Handle(context.Background(), job, func(pct int, msg string) {
		gotPct, gotMsg = pct, msg
	})
	if err != nil {
		t.Fatalf("Handle() error = %v, want nil", err)
	}
	if !bytes.Equal(result.Data, wantResult.Data) {
		t.Errorf("Handle() result = %+v, want %+v", result, wantResult)
	}
	if gotJob != job {
		t.Errorf("Handle() did not pass the same *Job through to fn")
	}
	if gotPct != 50 || gotMsg != "halfway" {
		t.Errorf("progress callback = (%d, %q), want (50, %q)", gotPct, gotMsg, "halfway")
	}
}

func TestNewHandlerFunc_PropagatesError(t *testing.T) {
	wantErr := errors.New("boom")
	h := NewHandlerFunc("widgets.resize", func(context.Context, *Job, ProgressFn) (Result, error) {
		return Result{}, wantErr
	})

	_, err := h.Handle(context.Background(), &Job{}, func(int, string) {})
	if !errors.Is(err, wantErr) {
		t.Errorf("Handle() error = %v, want %v", err, wantErr)
	}
}

func TestNewEmptyPayloadHandler_RunsThePassForAnEmptyPayload(t *testing.T) {
	ctxGot := context.Background()
	calls := 0
	h := NewEmptyPayloadHandler("widgets.sweep", func(ctx context.Context) error {
		calls++
		if ctx != ctxGot {
			t.Errorf("run() ctx = %v, want the ctx Handle received", ctx)
		}
		return nil
	})

	if got := h.Type(); got != "widgets.sweep" {
		t.Errorf("Type() = %q, want %q", got, "widgets.sweep")
	}
	if _, err := h.Handle(ctxGot, &Job{Payload: nil}, nil); err != nil {
		t.Fatalf("Handle() error = %v, want nil", err)
	}
	if calls != 1 {
		t.Errorf("run() calls = %d, want 1", calls)
	}
}

func TestNewEmptyPayloadHandler_RefusesANonEmptyPayloadWithoutRunning(t *testing.T) {
	calls := 0
	h := NewEmptyPayloadHandler("widgets.sweep", func(context.Context) error {
		calls++
		return nil
	})

	_, err := h.Handle(context.Background(), &Job{Payload: []byte(`{"unexpected":true}`)}, nil)
	if err == nil {
		t.Fatal("Handle() with a non-empty payload = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "unexpected payload") {
		t.Errorf("Handle() error = %q, want it to name the unexpected payload", err)
	}
	if !strings.Contains(err.Error(), "widgets.sweep") {
		t.Errorf("Handle() error = %q, want it to name the task type", err)
	}
	if calls != 0 {
		t.Errorf("run() calls = %d, want 0 -- a task-shape violation must not run the pass", calls)
	}
}

func TestNewEmptyPayloadHandler_PropagatesTheRunError(t *testing.T) {
	wantErr := errors.New("boom")
	h := NewEmptyPayloadHandler("widgets.sweep", func(context.Context) error {
		return wantErr
	})

	_, err := h.Handle(context.Background(), &Job{}, nil)
	if !errors.Is(err, wantErr) {
		t.Errorf("Handle() error = %v, want %v", err, wantErr)
	}
}

// hookHandler is a minimal Handler that also implements FailureHook, used
// to prove the two interfaces compose: a Handler implementation opts into
// failure compensation simply by also implementing FailureHook, with no
// separate registration mechanism.
type hookHandler struct {
	onFailureCalls int
}

func (*hookHandler) Type() string { return "widgets.resize" }

func (*hookHandler) Handle(context.Context, *Job, ProgressFn) (Result, error) {
	return Result{}, nil
}

func (h *hookHandler) OnFailure(context.Context, *Job, error) {
	h.onFailureCalls++
}

func TestFailureHook_ComposesWithHandler(t *testing.T) {
	h := &hookHandler{}

	var asHandler Handler = h
	hook, ok := asHandler.(FailureHook)
	if !ok {
		t.Fatalf("a Handler that implements OnFailure must satisfy FailureHook via a type assertion")
	}

	hook.OnFailure(context.Background(), &Job{}, errors.New("exhausted"))
	if h.onFailureCalls != 1 {
		t.Errorf("onFailureCalls = %d, want 1", h.onFailureCalls)
	}

	// NewHandlerFunc's returned Handler, by contrast, never implements
	// FailureHook -- there is no fn slot for it -- proving the two
	// mechanisms are independent, not automatically bundled.
	plain := NewHandlerFunc("widgets.resize", func(context.Context, *Job, ProgressFn) (Result, error) {
		return Result{}, nil
	})
	if _, ok := plain.(FailureHook); ok {
		t.Errorf("NewHandlerFunc's Handler unexpectedly implements FailureHook")
	}
}
