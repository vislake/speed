package billing

import (
	"context"
	"fmt"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// maxTransitionAttempts bounds how many read-validate-write rounds one
// lifecycle transition may spend before giving up. Each round is one read
// plus one guarded UPDATE; a round loses only when a concurrent transition
// commits between the two, and burning the whole budget therefore requires
// five consecutive, precisely-timed losses to other writers -- a livelock
// guard against a pathological adversary, not a bound any realistic
// two-caller race can hit (the losing side of an ordinary race reapplies
// on its next round). Exhaustion returns a plain error rather than a
// fabricated lifecycle answer.
const maxTransitionAttempts = 5

// transitionOps carries the per-entity pieces one lifecycle transition's
// retry rounds need: the row type, its own read path, its legal-move
// table, its coded refusal, its guarded UPDATE, and the side effects a
// winning round produces. casTransition below owns the round loop itself.
type transitionOps[E any] struct {
	// noun names the entity kind in the exhaustion error ("subscription",
	// "invoice").
	noun string
	// read loads the row fresh for the round, not-found already mapped to
	// the entity's own coded error.
	read func(ctx context.Context, id string) (*E, error)
	// status reads the row's current status.
	status func(row *E) string
	// legal reports whether the move from -> to is allowed by the
	// entity's own legal-transition table.
	legal func(from, to string) bool
	// invalid is the entity's coded refusal for a move legal() rejected;
	// casTransition adds the from/to params.
	invalid *apperr.Error
	// cas attempts ONE guarded status transition: an UPDATE whose WHERE
	// carries both the row id and the status the move was validated from,
	// RowsAffected as the arbiter -- the compare-and-swap shape
	// SubscriptionRepository.compareAndSetStatus and
	// InvoiceRepository.setStatusIf both implement. A false result means
	// the row no longer carried the validated-from status (a concurrent
	// transition won), never an error.
	cas func(ctx context.Context, id, from, to string) (bool, error)
	// applied runs the winning round's own side effects -- publishing the
	// status-changed event, recording the transition metrics -- and
	// returns the row with its status already moved to `to`.
	applied func(row *E, from, to string) *E
}

// casTransition drives one lifecycle transition's read-validate-write
// rounds, shared by SubscriptionService.transition and
// InvoiceRepository.setStatus: every round reads the row, validates the
// move against the entity's table, and applies it with a guarded UPDATE
// (ops.cas), RowsAffected deciding the winner. This is what makes a
// terminal status genuinely terminal under concurrency: two racing
// transitions that both validated from the same status cannot both commit
// -- the loser's guard misses because the row no longer carries the status
// it validated from. A lost round re-reads and re-attempts from the fresh
// status while the move stays legal, so an ordinary race converges instead
// of failing; the move is refused with the entity's own coded error only
// when the fresh status genuinely makes it illegal. Each call that wins a
// round runs ops.applied exactly once, never on a re-read, and exhaustion
// after maxTransitionAttempts consecutive losses returns a plain error
// naming the entity, the target status and the attempt budget rather than
// a fabricated lifecycle answer.
func casTransition[E any](ctx context.Context, id string, to string, ops transitionOps[E]) (*E, error) {
	for attempt := 1; attempt <= maxTransitionAttempts; attempt++ {
		row, err := ops.read(ctx, id)
		if err != nil {
			return nil, err
		}
		from := ops.status(row)
		if !ops.legal(from, to) {
			return nil, ops.invalid.
				WithParam("from", from).
				WithParam("to", to)
		}
		applied, err := ops.cas(ctx, id, from, to)
		if err != nil {
			return nil, err
		}
		if applied {
			return ops.applied(row, from, to), nil
		}
	}
	return nil, fmt.Errorf(
		"billing: %s %q transition to %q did not settle after %d attempts (concurrent transitions kept winning)",
		ops.noun, id, to, maxTransitionAttempts)
}
