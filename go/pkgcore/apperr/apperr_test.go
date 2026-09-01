package apperr

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"sync"
	"testing"
)

// testCode is a representative "<module>.<reason>" code used across the tests.
const testCode = "billing.subscription_not_found"

// errSentinel stands in for a sentinel error defined by a lower layer, such as
// sql.ErrNoRows, that a caller wants to match through errors.Is.
var errSentinel = errors.New("underlying storage failure")

// otherError is an unrelated error implementation used to prove that As does
// not match error types it does not own.
type otherError struct{}

func (otherError) Error() string { return "some other error" }

func TestConstructors_PrefillStatusAndCode(t *testing.T) {
	tests := []struct {
		name       string
		construct  func(string) *Error
		wantStatus int
	}{
		{name: "NotFound", construct: NotFound, wantStatus: http.StatusNotFound},
		{name: "Invalid", construct: Invalid, wantStatus: http.StatusBadRequest},
		{name: "Conflict", construct: Conflict, wantStatus: http.StatusConflict},
		{name: "Unauthorized", construct: Unauthorized, wantStatus: http.StatusUnauthorized},
		{name: "Forbidden", construct: Forbidden, wantStatus: http.StatusForbidden},
		{name: "Internal", construct: Internal, wantStatus: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.construct(testCode)

			if got.Status != tt.wantStatus {
				t.Errorf("Status = %d, want %d", got.Status, tt.wantStatus)
			}
			if got.Code != testCode {
				t.Errorf("Code = %q, want %q", got.Code, testCode)
			}
			if got.Params != nil {
				t.Errorf("Params = %v, want nil on a fresh error", got.Params)
			}
			if unwrapped := got.Unwrap(); unwrapped != nil {
				t.Errorf("Unwrap() = %v, want nil on a fresh error", unwrapped)
			}
		})
	}
}

func TestConstructors_ReturnIndependentInstances(t *testing.T) {
	first := NotFound(testCode).WithParam("id", "sub_01")
	second := NotFound(testCode)

	if second.Params != nil {
		t.Fatalf("second.Params = %v, want nil: constructors must not share state", second.Params)
	}
	if first == second {
		t.Fatal("constructors returned the same pointer twice")
	}
}

func TestError_WithParam(t *testing.T) {
	type param struct {
		key   string
		value any
	}

	tests := []struct {
		name   string
		params []param
		want   map[string]any
	}{
		{
			name:   "no params leaves the map nil",
			params: nil,
			want:   nil,
		},
		{
			name:   "first param initializes the nil map",
			params: []param{{key: "id", value: "sub_01HZ"}},
			want:   map[string]any{"id": "sub_01HZ"},
		},
		{
			name: "chained params accumulate",
			params: []param{
				{key: "id", value: "sub_01HZ"},
				{key: "plan", value: "pro"},
				{key: "seats", value: 5},
			},
			want: map[string]any{"id": "sub_01HZ", "plan": "pro", "seats": 5},
		},
		{
			name: "repeated key keeps the last value",
			params: []param{
				{key: "id", value: "sub_old"},
				{key: "id", value: "sub_new"},
			},
			want: map[string]any{"id": "sub_new"},
		},
		{
			name:   "nil value is recorded",
			params: []param{{key: "reason", value: nil}},
			want:   map[string]any{"reason": nil},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := Invalid(testCode)

			got := base
			for _, p := range tt.params {
				next := got.WithParam(p.key, p.value)
				if next == got {
					t.Fatalf("WithParam(%q) returned the receiver, want a derived instance", p.key)
				}
				got = next
			}

			if !maps.Equal(got.Params, tt.want) {
				t.Errorf("Params = %v, want %v", got.Params, tt.want)
			}
			if base.Params != nil {
				t.Errorf("base.Params = %v, want nil: WithParam must not touch the receiver", base.Params)
			}
			if got.Code != testCode {
				t.Errorf("Code = %q, want %q: WithParam must not alter the code", got.Code, testCode)
			}
			if got.Status != http.StatusBadRequest {
				t.Errorf("Status = %d, want %d: WithParam must not alter the status", got.Status, http.StatusBadRequest)
			}
		})
	}
}

func TestError_Error(t *testing.T) {
	tests := []struct {
		name string
		err  *Error
		want string
	}{
		{
			name: "code only when there is no cause",
			err:  NotFound(testCode),
			want: testCode,
		},
		{
			name: "code and cause joined by a colon",
			err:  Internal("billing.charge_failed").WithCause(errors.New("connection refused")),
			want: "billing.charge_failed: connection refused",
		},
		{
			name: "params are not leaked into the string form",
			err:  NotFound(testCode).WithParam("id", "sub_01HZ").WithParam("plan", "pro"),
			want: testCode,
		},
		{
			name: "params are not leaked even when a cause is present",
			err:  Internal("billing.charge_failed").WithParam("id", "sub_01HZ").WithCause(errSentinel),
			want: "billing.charge_failed: " + errSentinel.Error(),
		},
		{
			name: "explicit nil cause renders as code only",
			err:  Conflict("billing.duplicate_subscription").WithCause(nil),
			want: "billing.duplicate_subscription",
		},
		{
			name: "nested cause chain is rendered by the direct cause",
			err:  Internal("billing.charge_failed").WithCause(fmt.Errorf("charging card: %w", errSentinel)),
			want: "billing.charge_failed: charging card: " + errSentinel.Error(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestError_UnwrapSupportsErrorsIs(t *testing.T) {
	otherSentinel := errors.New("unrelated sentinel")

	tests := []struct {
		name   string
		err    error
		target error
		want   bool
	}{
		{
			name:   "direct cause matches",
			err:    Internal("billing.charge_failed").WithCause(errSentinel),
			target: errSentinel,
			want:   true,
		},
		{
			name:   "cause reached through an intermediate wrap",
			err:    Internal("billing.charge_failed").WithCause(fmt.Errorf("charging card: %w", errSentinel)),
			target: errSentinel,
			want:   true,
		},
		{
			name:   "apperr wrapped by a caller still reaches the cause",
			err:    fmt.Errorf("service layer: %w", Internal("billing.charge_failed").WithCause(errSentinel)),
			target: errSentinel,
			want:   true,
		},
		{
			name:   "no cause does not match",
			err:    Internal("billing.charge_failed"),
			target: errSentinel,
			want:   false,
		},
		{
			name:   "different sentinel does not match",
			err:    Internal("billing.charge_failed").WithCause(errSentinel),
			target: otherSentinel,
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errors.Is(tt.err, tt.target); got != tt.want {
				t.Errorf("errors.Is(%v, %v) = %t, want %t", tt.err, tt.target, got, tt.want)
			}
		})
	}
}

func TestError_UnwrapReturnsCause(t *testing.T) {
	err := Internal("billing.charge_failed").WithCause(errSentinel)

	// Identity, not errors.Is: Unwrap must return the exact cause it was
	// given, which errors.Is would not distinguish from a deeper match.
	if got := err.Unwrap(); got != errSentinel { //nolint:errorlint // deliberate identity check on Unwrap
		t.Errorf("Unwrap() = %v, want %v", got, errSentinel)
	}
	if got := Internal("billing.charge_failed").Unwrap(); got != nil {
		t.Errorf("Unwrap() = %v, want nil when no cause was set", got)
	}
}

func TestAs(t *testing.T) {
	direct := Conflict("billing.duplicate_subscription")
	wrapped := Conflict("billing.duplicate_subscription")
	deep := NotFound(testCode)
	inner := NotFound(testCode)
	outer := Internal("billing.charge_failed").WithCause(inner)

	tests := []struct {
		name   string
		err    error
		wantOK bool
		want   *Error
	}{
		{
			name:   "direct apperr",
			err:    direct,
			wantOK: true,
			want:   direct,
		},
		{
			name:   "single wrap",
			err:    fmt.Errorf("service layer: %w", wrapped),
			wantOK: true,
			want:   wrapped,
		},
		{
			name:   "double wrap",
			err:    fmt.Errorf("handler: %w", fmt.Errorf("service layer: %w", deep)),
			wantOK: true,
			want:   deep,
		},
		{
			name:   "nested apperr yields the outermost one",
			err:    outer,
			wantOK: true,
			want:   outer,
		},
		{
			name:   "unrelated error type",
			err:    otherError{},
			wantOK: false,
			want:   nil,
		},
		{
			name:   "wrapped unrelated error type",
			err:    fmt.Errorf("service layer: %w", otherError{}),
			wantOK: false,
			want:   nil,
		},
		{
			name:   "plain error",
			err:    errors.New("boom"),
			wantOK: false,
			want:   nil,
		},
		{
			name:   "nil error",
			err:    nil,
			wantOK: false,
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := As(tt.err)

			if ok != tt.wantOK {
				t.Fatalf("As() ok = %t, want %t", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Fatalf("As() error = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAs_PreservesCodeStatusAndParams(t *testing.T) {
	const id = "sub_01HZ"
	original := NotFound(testCode).WithParam("id", id)

	got, ok := As(fmt.Errorf("handler: %w", fmt.Errorf("service layer: %w", original)))
	if !ok {
		t.Fatal("As() ok = false, want true")
	}
	if got.Code != testCode {
		t.Errorf("Code = %q, want %q", got.Code, testCode)
	}
	if got.Status != http.StatusNotFound {
		t.Errorf("Status = %d, want %d", got.Status, http.StatusNotFound)
	}
	if got.Params["id"] != id {
		t.Errorf("Params[\"id\"] = %v, want %q", got.Params["id"], id)
	}
}

// errSubscriptionNotFound is the package-level sentinel the repository's naming
// standard pushes module authors towards ("error values are Err<Reason>"). It
// exists in the test suite so the builders are exercised exactly the way a
// module would use them: declared once, decorated per request.
var errSubscriptionNotFound = NotFound(testCode)

func TestBuilders_DoNotMutateTheReceiver(t *testing.T) {
	t.Run("WithParam leaves the receiver untouched", func(t *testing.T) {
		base := NotFound(testCode)

		derived := base.WithParam("id", "sub_01")

		if derived == base {
			t.Fatal("WithParam returned the receiver, want a derived instance")
		}
		if base.Params != nil {
			t.Errorf("base.Params = %v, want nil", base.Params)
		}
		if derived.Params["id"] != "sub_01" {
			t.Errorf("derived.Params[\"id\"] = %v, want %q", derived.Params["id"], "sub_01")
		}
		if derived.Code != base.Code || derived.Status != base.Status {
			t.Errorf("derived = {%q, %d}, want the receiver's {%q, %d}", derived.Code, derived.Status, base.Code, base.Status)
		}
	})

	t.Run("WithCause leaves the receiver untouched", func(t *testing.T) {
		base := Internal("billing.charge_failed")

		derived := base.WithCause(errSentinel)

		if derived == base {
			t.Fatal("WithCause returned the receiver, want a derived instance")
		}
		if got := base.Unwrap(); got != nil {
			t.Errorf("base.Unwrap() = %v, want nil", got)
		}
		if !errors.Is(derived, errSentinel) {
			t.Error("errors.Is(derived, errSentinel) = false, want true")
		}
	})

	t.Run("siblings derived from one error do not share the Params map", func(t *testing.T) {
		base := NotFound(testCode).WithParam("id", "sub_01")

		pro := base.WithParam("plan", "pro")
		team := base.WithParam("plan", "team")

		if !maps.Equal(base.Params, map[string]any{"id": "sub_01"}) {
			t.Errorf("base.Params = %v, want only the id it was built with", base.Params)
		}
		if !maps.Equal(pro.Params, map[string]any{"id": "sub_01", "plan": "pro"}) {
			t.Errorf("pro.Params = %v, want id and plan=pro", pro.Params)
		}
		if !maps.Equal(team.Params, map[string]any{"id": "sub_01", "plan": "team"}) {
			t.Errorf("team.Params = %v, want id and plan=team", team.Params)
		}
	})

	t.Run("a cause inherited from the receiver survives WithParam", func(t *testing.T) {
		base := Internal("billing.charge_failed").WithCause(errSentinel)

		derived := base.WithParam("id", "sub_01")

		if !errors.Is(derived, errSentinel) {
			t.Error("errors.Is(derived, errSentinel) = false, want the cause to be carried over")
		}
	})
}

// TestSentinel_ConcurrentDecorationIsRaceFree decorates one package-level
// sentinel from many goroutines at once. Under -race this fails if the builders
// write to the receiver; without -race it still catches the quieter bug, where
// one request's parameters leak into another's response body.
func TestSentinel_ConcurrentDecorationIsRaceFree(t *testing.T) {
	const goroutines = 8

	var wg sync.WaitGroup
	start := make(chan struct{})
	decorated := make([]*Error, goroutines)

	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			decorated[i] = errSubscriptionNotFound.
				WithParam("id", fmt.Sprintf("sub_%02d", i)).
				WithCause(fmt.Errorf("request %d: %w", i, errSentinel))
		}(i)
	}
	close(start)
	wg.Wait()

	if errSubscriptionNotFound.Params != nil {
		t.Errorf("sentinel Params = %v, want nil: decorating must not modify the sentinel", errSubscriptionNotFound.Params)
	}
	if got := errSubscriptionNotFound.Unwrap(); got != nil {
		t.Errorf("sentinel cause = %v, want nil: decorating must not modify the sentinel", got)
	}
	if errSubscriptionNotFound.Status != http.StatusNotFound {
		t.Errorf("sentinel Status = %d, want %d", errSubscriptionNotFound.Status, http.StatusNotFound)
	}

	for i, got := range decorated {
		wantID := fmt.Sprintf("sub_%02d", i)
		if !maps.Equal(got.Params, map[string]any{"id": wantID}) {
			t.Errorf("goroutine %d Params = %v, want exactly {id: %q}", i, got.Params, wantID)
		}
		if !errors.Is(got, errSentinel) {
			t.Errorf("goroutine %d lost its cause", i)
		}
		if got.Code != testCode {
			t.Errorf("goroutine %d Code = %q, want %q", i, got.Code, testCode)
		}
	}
}
