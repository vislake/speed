package compliance

import (
	"fmt"
	"sort"
	"strings"

	"github.com/vislake/speed/go/pkgcore"
)

// This file holds the per-participant mechanics the retention sweep, the
// right-to-erasure request and the data-export gather all share: the pass
// driver that calls every registered participant's callback for one pass
// (eachParticipant), and the two ways a pass's failed-participant set is
// rendered -- the failure-reason summary (ParticipantFailureReason) and
// the classification-only map an audit event's Changes["errors"] entry
// carries (participantFailureClassification).

// ParticipantFailureReason renders the failure-reason summary of a set of
// failed participants: their names, sorted, joined after the
// "participants failed: " prefix. It is one string vocabulary shared by
// all three compliance passes -- the participants parameter of
// ErrSweepPartialFailure/ErrErasurePartialFailure/ErrExportPartialFailure
// and the FailureReason of every audit event the sweep, erasure and export
// paths emit -- and go/admin's audit-export leg renders its own
// admin.audit_export FailureReason through it too, so the events an
// operator reads for one export agree. Empty when failures is empty. V is
// the map's value type (the sweep and erasure paths hold map[string]error,
// an ExportManifest holds map[string]string): only the keys -- the
// participant names -- participate in the rendering.
func ParticipantFailureReason[V any](failures map[string]V) string {
	if len(failures) == 0 {
		return ""
	}
	names := make([]string, 0, len(failures))
	for name := range failures {
		names = append(names, name)
	}
	sort.Strings(names)
	return fmt.Sprintf("participants failed: %s", strings.Join(names, ", "))
}

// participantFailureClassification returns the classification-only record
// an audit event's Changes["errors"] entry stores for a set of failed
// participants: each participant name keyed to marker (participantErrorMarker
// on the sweep and export paths, erasureAuditErrorMarker on the erasure
// path) -- never the error text, whose homes are the in-process result
// maps and the failure-site structured logs (see participantErrorMarker's
// and erasureAuditErrorMarker's doc comments). V is the map's value type;
// only the keys are read.
func participantFailureClassification[V any](failures map[string]V, marker string) map[string]string {
	classified := make(map[string]string, len(failures))
	for name := range failures {
		classified[name] = marker
	}
	return classified
}

// eachParticipant drives one compliance pass over the registered
// participants: it walks participants in registration order, invokes each
// participant's callback for the pass through call, and hands every called
// participant's outcome to record. call reports called=false for a
// participant that does not take part in the pass -- p.Export == nil is
// the documented opt-out, and the same skip covers a nil Sweep or Erase
// callback, which registration already refuses
// (pkgcore.ErrNilRetentionSweep / ErrNilRetentionErase, so the check is
// defensive there) -- and such a participant never reaches record. One
// participant's callback failing never stops the walk: every remaining
// participant still runs (SweepTenant's doc comment states the shared
// partial-failure contract), and each pass's own accounting -- which value
// and error land in which result field, and what is logged -- is exactly
// what its record closure owns.
func eachParticipant[T any](
	participants []pkgcore.RetentionParticipant,
	call func(pkgcore.RetentionParticipant) (value T, called bool, err error),
	record func(pkgcore.RetentionParticipant, T, error),
) {
	for _, p := range participants {
		value, called, err := call(p)
		if !called {
			continue
		}
		record(p, value, err)
	}
}
