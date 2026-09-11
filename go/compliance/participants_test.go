package compliance

import (
	"errors"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// TestParticipantFailureReason pins the shared failure-reason rendering
// the sweep, erasure and export paths (and go/admin's audit-export leg)
// all use: names sorted and joined after the "participants failed: "
// prefix, "" for an empty set, and -- the content rule this module's
// audit and manifest surfaces depend on -- never the error text. The
// error value type differs across callers (map[string]error on the sweep
// and erasure paths, map[string]string on an ExportManifest), so both are
// pinned to the identical rendering.
func TestParticipantFailureReason(t *testing.T) {
	if got := ParticipantFailureReason[string](nil); got != "" {
		t.Errorf("ParticipantFailureReason(nil) = %q, want the empty string", got)
	}
	if got := ParticipantFailureReason(map[string]error{}); got != "" {
		t.Errorf("ParticipantFailureReason(empty) = %q, want the empty string", got)
	}

	// Keys are inserted in reverse-sorted order so a rendering that
	// depended on map iteration order could not pass by luck.
	carving := errors.New("scan compliance/exports/tenant-a/x.json failed: object store timeout")
	reason := ParticipantFailureReason(map[string]error{"zeta.rows": carving, "alpha.rows": carving})
	const want = "participants failed: alpha.rows, zeta.rows"
	if reason != want {
		t.Errorf("ParticipantFailureReason(error values) = %q, want %q", reason, want)
	}
	if strings.Contains(reason, carving.Error()) {
		t.Errorf("the error text was carved into the failure reason %q -- the map's keys are the only rendering input", reason)
	}

	classified := ParticipantFailureReason(map[string]string{"zeta.rows": participantErrorMarker, "alpha.rows": participantErrorMarker})
	if classified != want {
		t.Errorf("ParticipantFailureReason(classification values) = %q, want the same rendering %q", classified, want)
	}
}

// TestParticipantFailureClassification pins the classification-only map
// every audit event's Changes["errors"] entry carries: one entry per
// failed participant, the name keyed to the caller's own marker (so the
// erasure path's erasureAuditErrorMarker travels through the same helper
// as the sweep and export paths' participantErrorMarker), and empty input
// producing an empty map. A hostile error value must never leak into the
// result -- this map is what gets written into the append-only audit
// record.
func TestParticipantFailureClassification(t *testing.T) {
	carving := errors.New("erase subject-9 rows failed: connection refused")
	classified := participantFailureClassification(
		map[string]error{"testutil.carving": carving, "testutil.healthy": nil}, erasureAuditErrorMarker)
	if len(classified) != 2 {
		t.Fatalf("classification entries = %d, want 2 (one per name)", len(classified))
	}
	for name, got := range classified {
		if got != erasureAuditErrorMarker {
			t.Errorf("classification[%q] = %q, want the marker %q -- never the error text", name, got, erasureAuditErrorMarker)
		}
	}
	if _, ok := classified["testutil.healthy"]; !ok {
		t.Errorf("classification = %v, want an entry for every name in the input map", classified)
	}

	if classified := participantFailureClassification(map[string]error(nil), participantErrorMarker); len(classified) != 0 {
		t.Errorf("participantFailureClassification(nil) = %v, want an empty map", classified)
	}
}

// TestEachParticipant pins the shared pass driver's contract: participants
// are walked in registration order, a participant whose call reports
// called=false is skipped without reaching record (the Export opt-out, and
// the defensive nil-callback skip on the sweep and erasure passes), and
// one participant's error neither stops the walk nor withholds the
// callback's reported value -- the count-on-error semantics SweepTenant's
// and Erase's record closures are built on.
func TestEachParticipant(t *testing.T) {
	participants := []pkgcore.RetentionParticipant{
		{Name: "first"},
		{Name: "skipped"},
		{Name: "second"},
		{Name: "third"},
	}
	carving := errors.New("hard delete failed")

	type outcome struct {
		name  string
		value int
		err   error
	}
	var got []outcome
	eachParticipant(participants,
		func(p pkgcore.RetentionParticipant) (int, bool, error) {
			switch p.Name {
			case "skipped":
				return 0, false, nil
			case "first":
				return 2, true, carving
			default:
				return 1, true, nil
			}
		},
		func(p pkgcore.RetentionParticipant, value int, err error) {
			got = append(got, outcome{p.Name, value, err})
		},
	)

	if len(got) != 3 {
		t.Fatalf("recorded outcomes = %d (%v), want 3 -- the uncalled participant must be skipped, the rest must all run", len(got), got)
	}
	if got[0].name != "first" || got[1].name != "second" || got[2].name != "third" {
		t.Errorf("walk order = %q, %q, %q, want registration order first, second, third", got[0].name, got[1].name, got[2].name)
	}
	if got[0].value != 2 || !errors.Is(got[0].err, carving) {
		t.Errorf("first outcome = (%d, %v), want (2, the callback's error) -- the reported value must survive the error", got[0].value, got[0].err)
	}
	if got[1].value != 1 || got[1].err != nil || got[2].value != 1 || got[2].err != nil {
		t.Errorf("later outcomes = (%d, %v), (%d, %v), want (1, nil) each -- an earlier failure must not stop the walk",
			got[1].value, got[1].err, got[2].value, got[2].err)
	}
}
