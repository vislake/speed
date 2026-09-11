package billing

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// casTestRow is a minimal entity for driving casTransition directly: the
// helper is generic over the row type, and a synthetic row keeps these
// tests about the round loop itself rather than about Subscription or
// Invoice.
type casTestRow struct{ status string }

// TestCasTransition_LostRoundReappliesFromTheFreshStatus pins the retry
// contract: a round whose guarded UPDATE matched zero rows re-reads, the
// move is re-validated against the FRESH status, and only the round that
// wins runs applied -- exactly once, never on a re-read.
func TestCasTransition_LostRoundReappliesFromTheFreshStatus(t *testing.T) {
	statuses := []string{"created", "past_due"}
	reads := 0
	var legalFrom, casFrom []string
	appliedCalls := 0

	got, err := casTransition(context.Background(), "row-1", "active", transitionOps[casTestRow]{
		noun: "widget",
		read: func(_ context.Context, id string) (*casTestRow, error) {
			if id != "row-1" {
				t.Errorf("read id = %q, want row-1", id)
			}
			status := statuses[reads]
			reads++
			return &casTestRow{status: status}, nil
		},
		status: func(row *casTestRow) string { return row.status },
		legal: func(from, to string) bool {
			legalFrom = append(legalFrom, from)
			return from == "created" || from == "past_due"
		},
		invalid: ErrInvalidSubscriptionTransition,
		cas: func(_ context.Context, id, from, to string) (bool, error) {
			casFrom = append(casFrom, from)
			// The first round loses to a concurrent transition; the second
			// wins.
			return len(casFrom) == 2, nil
		},
		applied: func(row *casTestRow, from, to string) *casTestRow {
			appliedCalls++
			row.status = to
			return row
		},
	})
	if err != nil {
		t.Fatalf("casTransition: %v", err)
	}
	if reads != 2 || len(casFrom) != 2 {
		t.Errorf("reads = %d, cas calls = %d, want 2 and 2 (one lost round, then a win)", reads, len(casFrom))
	}
	if len(legalFrom) != 2 || legalFrom[0] != "created" || legalFrom[1] != "past_due" {
		t.Errorf("legal saw %v, want the re-validated fresh status past_due on the second round", legalFrom)
	}
	if casFrom[0] != "created" || casFrom[1] != "past_due" {
		t.Errorf("cas guarded from %v, want created then the fresh past_due", casFrom)
	}
	if appliedCalls != 1 {
		t.Errorf("applied ran %d times, want exactly 1 (only the winning round)", appliedCalls)
	}
	if got == nil || got.status != "active" {
		t.Errorf("result = %+v, want the row with status active", got)
	}
}

// TestCasTransition_IllegalMoveRefusedWithFromAndTo pins the refusal path:
// a move the entity's table rejects is refused with the entity's coded
// error carrying the from/to params, before any guarded UPDATE runs.
func TestCasTransition_IllegalMoveRefusedWithFromAndTo(t *testing.T) {
	casCalls := 0
	_, err := casTransition(context.Background(), "row-1", "void", transitionOps[casTestRow]{
		noun:    "widget",
		read:    func(context.Context, string) (*casTestRow, error) { return &casTestRow{status: "paid"}, nil },
		status:  func(row *casTestRow) string { return row.status },
		legal:   func(from, to string) bool { return false },
		invalid: ErrInvalidInvoiceTransition,
		cas: func(context.Context, string, string, string) (bool, error) {
			casCalls++
			return true, nil
		},
		applied: func(row *casTestRow, from, to string) *casTestRow { return row },
	})
	if !apperr.HasCode(err, ErrInvalidInvoiceTransition.Code) {
		t.Fatalf("illegal move: err = %v, want %s", err, ErrInvalidInvoiceTransition.Code)
	}
	if casCalls != 0 {
		t.Errorf("cas ran %d times on an illegal move, want 0", casCalls)
	}
	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("illegal move: err = %T, want *apperr.Error", err)
	}
	if appErr.Params["from"] != "paid" || appErr.Params["to"] != "void" {
		t.Errorf("Params = %v, want from=paid to=void", appErr.Params)
	}
}

// TestCasTransition_BudgetExhaustionReturnsPlainError pins the exhaustion
// semantics: maxTransitionAttempts consecutive lost rounds end the loop
// with a plain, uncoded error naming the entity, the target and the budget
// -- never a fabricated lifecycle answer, and applied never runs.
func TestCasTransition_BudgetExhaustionReturnsPlainError(t *testing.T) {
	reads, casCalls, appliedCalls := 0, 0, 0
	_, err := casTransition(context.Background(), "row-1", "active", transitionOps[casTestRow]{
		noun: "widget",
		read: func(context.Context, string) (*casTestRow, error) {
			reads++
			return &casTestRow{status: "created"}, nil
		},
		status:  func(row *casTestRow) string { return row.status },
		legal:   func(from, to string) bool { return true },
		invalid: ErrInvalidSubscriptionTransition,
		cas: func(context.Context, string, string, string) (bool, error) {
			casCalls++
			return false, nil
		},
		applied: func(row *casTestRow, from, to string) *casTestRow {
			appliedCalls++
			return row
		},
	})
	if err == nil {
		t.Fatal("budget exhaustion: err = nil, want the did-not-settle error")
	}
	if reads != maxTransitionAttempts || casCalls != maxTransitionAttempts {
		t.Errorf("reads = %d, cas calls = %d, want %d each", reads, casCalls, maxTransitionAttempts)
	}
	if appliedCalls != 0 {
		t.Errorf("applied ran %d times with no winning round, want 0", appliedCalls)
	}
	if _, ok := apperr.As(err); ok {
		t.Errorf("budget exhaustion: err = %v, want a plain (uncoded) error", err)
	}
	want := "widget \"row-1\" transition to \"active\" did not settle after 5 attempts"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("err = %q, want it to contain %q", err.Error(), want)
	}
}

// TestCasTransition_ReadAndCasErrorsAbort pins the conflict
// classification: an error from either the read or the guarded UPDATE ends
// the loop immediately with that error -- only a zero-row UPDATE (applied
// == false) is a lost round worth retrying.
func TestCasTransition_ReadAndCasErrorsAbort(t *testing.T) {
	readErr := errors.New("read exploded")
	casErr := errors.New("cas exploded")

	_, err := casTransition(context.Background(), "row-1", "active", transitionOps[casTestRow]{
		noun:    "widget",
		read:    func(context.Context, string) (*casTestRow, error) { return nil, readErr },
		status:  func(row *casTestRow) string { return row.status },
		legal:   func(from, to string) bool { return true },
		invalid: ErrInvalidSubscriptionTransition,
		cas:     func(context.Context, string, string, string) (bool, error) { return false, nil },
		applied: func(row *casTestRow, from, to string) *casTestRow { return row },
	})
	if !errors.Is(err, readErr) {
		t.Errorf("read error: err = %v, want it to carry the read error through", err)
	}

	reads := 0
	_, err = casTransition(context.Background(), "row-1", "active", transitionOps[casTestRow]{
		noun: "widget",
		read: func(context.Context, string) (*casTestRow, error) {
			reads++
			return &casTestRow{status: "created"}, nil
		},
		status:  func(row *casTestRow) string { return row.status },
		legal:   func(from, to string) bool { return true },
		invalid: ErrInvalidSubscriptionTransition,
		cas:     func(context.Context, string, string, string) (bool, error) { return false, casErr },
		applied: func(row *casTestRow, from, to string) *casTestRow { return row },
	})
	if !errors.Is(err, casErr) {
		t.Errorf("cas error: err = %v, want it to carry the cas error through", err)
	}
	if reads != 1 {
		t.Errorf("reads = %d after a cas error on round 1, want 1 (no further rounds)", reads)
	}
}
