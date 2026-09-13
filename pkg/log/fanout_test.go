package log

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// failingHandler is a branch whose destination refuses the record.
type failingHandler struct{ err error }

func (failingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h failingHandler) Handle(context.Context, slog.Record) error { return h.err }

func (h failingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h failingHandler) WithGroup(string) slog.Handler { return h }

// TestFanoutDoesNotAliasAcrossBranches pins that each branch gets a copy.
//
// The number of attributes is varied because the sharing only exists in some
// shapes: a record holds its first five attributes inline and spills the rest
// into a slice, and it is that slice the copies share. The assertion is made
// after both branches have appended to the record they kept, because that is
// when the sharing shows — at the moment a branch receives the record, its
// length is unchanged either way, so an implementation that hands the same
// record to everyone passes any assertion made earlier.
func TestFanoutDoesNotAliasAcrossBranches(t *testing.T) {
	for count := 6; count <= 12; count++ {
		t.Run(fmt.Sprintf("attrs=%d", count), func(t *testing.T) {
			first, second := newCapture(), newCapture()
			fanout := newFanoutHandler([]slog.Handler{first, second})

			record := slog.NewRecord(time.Now(), slog.LevelInfo, "alias", 0)
			inline := make([]slog.Attr, 0, 5)
			for i := range 5 {
				inline = append(inline, slog.Int(fmt.Sprintf("inline%d", i), i))
			}
			record.AddAttrs(inline...)
			for i := 5; i < count; i++ {
				record.AddAttrs(slog.Int(fmt.Sprintf("spill%d", i), i))
			}

			if err := fanout.Handle(context.Background(), record); err != nil {
				t.Fatalf("Handle: %v", err)
			}

			firstKept, secondKept := first.state.last(t), second.state.last(t)
			firstKept.AddAttrs(slog.String("first-mark", "1"))
			secondKept.AddAttrs(slog.String("second-mark", "1"))

			assertMarks(t, "the record the first branch kept", firstKept, "first-mark", "second-mark")
			assertMarks(t, "the record the second branch kept", secondKept, "second-mark", "first-mark")
		})
	}
}

// TestFanoutContinuesAfterABranchFails pins that the outputs are independent:
// a destination that is full or gone does not take the others down with it.
//
// The failure is joined into the return rather than dropped. slog discards
// what Handle gives back, but inside this package the value is the only way a
// caller learns a branch refused the record.
func TestFanoutContinuesAfterABranchFails(t *testing.T) {
	refused := errors.New("this destination refused the record")
	survivor := newCapture()
	fanout := newFanoutHandler([]slog.Handler{failingHandler{err: refused}, survivor})

	err := fanout.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "fan", 0))

	if got := recordCount(survivor); got != 1 {
		t.Errorf("the branch after the failing one took %d records, want 1", got)
	}
	if !errors.Is(err, refused) {
		t.Errorf("Handle swallowed the branch failure, returned %v", err)
	}
}

// TestFanoutPropagatesWithGroupToEveryBranch pins that a derived logger reaches
// every output with the same binding, and that no branch picks up another's.
func TestFanoutPropagatesWithGroupToEveryBranch(t *testing.T) {
	var first, second bytes.Buffer
	opts := &slog.HandlerOptions{Level: allLevels}
	fanout := newFanoutHandler([]slog.Handler{
		slog.NewTextHandler(&first, opts),
		slog.NewTextHandler(&second, opts),
	})

	lg := slog.New(fanout).WithGroup("request").With("id", "r-7")
	lg.Info("served")

	for _, b := range []struct {
		name string
		out  string
	}{{"first", first.String()}, {"second", second.String()}} {
		if !strings.Contains(b.out, "request.id=r-7") {
			t.Errorf("the %s branch did not get the derivation: %s", b.name, b.out)
		}
		if lines := strings.Count(b.out, "\n"); lines != 1 {
			t.Errorf("the %s branch wrote %d lines, want the one record: %s", b.name, lines, b.out)
		}
	}
}

// TestFanoutWithNoBranchesIsDisabled pins what an explicitly empty output list
// assembles into: a chain that reports nothing as enabled, so records are not
// even built.
func TestFanoutWithNoBranchesIsDisabled(t *testing.T) {
	fanout := newFanoutHandler(nil)
	if fanout.Enabled(context.Background(), slog.LevelError) {
		t.Error("a fan-out with no branches reports a record as enabled")
	}
	if err := fanout.Handle(context.Background(),
		slog.NewRecord(time.Now(), slog.LevelError, "nowhere", 0)); err != nil {
		t.Errorf("handling a record with no branches failed: %v", err)
	}
}

// TestFanoutEmptyDerivationsReturnTheReceiver pins the Handler contract on both
// derivations, which is also what keeps an empty binding from allocating a
// branch per output.
func TestFanoutEmptyDerivationsReturnTheReceiver(t *testing.T) {
	fanout := newFanoutHandler([]slog.Handler{newCapture()})
	if got := fanout.WithAttrs(nil); got != slog.Handler(fanout) {
		t.Errorf("WithAttrs(nil) returned %p, want the receiver %p", got, fanout)
	}
	if got := fanout.WithGroup(""); got != slog.Handler(fanout) {
		t.Errorf("WithGroup(\"\") returned %p, want the receiver %p", got, fanout)
	}
}
