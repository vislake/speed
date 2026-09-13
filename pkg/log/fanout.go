package log

import (
	"context"
	"errors"
	"log/slog"
)

// fanoutHandler hands one record to every branch, one branch per configured
// output. It sits below the redaction layer, so a record is judged once
// however many outputs there are.
type fanoutHandler struct{ branches []slog.Handler }

// newFanoutHandler collects the branches of an assembly. A chain with no
// branches is legal: an explicitly empty output list means this process writes
// no records.
func newFanoutHandler(branches []slog.Handler) *fanoutHandler {
	return &fanoutHandler{branches: branches}
}

// Enabled reports whether any branch would take the record. Nothing in the
// chain asks it: the head judges the level once, and an empty output list is
// stopped there by a decision taken when the chain was assembled. It is here
// because the Handler contract asks for it.
func (h *fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, b := range h.branches {
		if b.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle gives each branch its own copy of the record.
//
// The copy is required. The copies of a slog.Record share the array their
// attributes spill into, and the Handler contract does not stop an
// implementation from adding to the record it was handed, so two branches
// appending to one record would write over each other. Clone only clips a
// slice, so the cost does not grow with the number of attributes.
//
// The branches are independent: one that fails does not stop the rest, and its
// error is joined into the return rather than dropped — slog itself discards
// what Handle returns, but a caller inside this package can still see it.
func (h *fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, b := range h.branches {
		if err := b.Handle(ctx, r.Clone()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// WithAttrs derives one branch per existing branch, so the bound attributes
// reach every output and no branch inherits another's state.
func (h *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	derived := make([]slog.Handler, len(h.branches))
	for i, b := range h.branches {
		derived[i] = b.WithAttrs(attrs)
	}
	return &fanoutHandler{branches: derived}
}

// WithGroup opens the group on every branch, for the same reason.
func (h *fanoutHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	derived := make([]slog.Handler, len(h.branches))
	for i, b := range h.branches {
		derived[i] = b.WithGroup(name)
	}
	return &fanoutHandler{branches: derived}
}
