package testutil

import "github.com/vislake/speed/go/pkgcore"

// FixedRegistrar is a pkgcore.RetentionRegistrar that reports exactly the
// participants it was constructed with, in the order given, and accepts
// Add as a no-op. It stands in for the real registrar in the one case the
// real registrar always refuses: a hand-built pkgcore.RetentionParticipant
// whose Sweep or Erase callback is nil (Add refuses those with
// pkgcore.ErrNilRetentionSweep / ErrNilRetentionErase). compliance's pass
// driver keeps a defensive skip for exactly that shape, and driving it
// takes a registrar that can hand such a participant back -- this one.
type FixedRegistrar struct {
	List []pkgcore.RetentionParticipant
}

// Add implements pkgcore.RetentionRegistrar; it appends to List rather
// than enforcing the real registrar's registration rules, which is the
// point of the fixture.
func (r *FixedRegistrar) Add(participants ...pkgcore.RetentionParticipant) error {
	r.List = append(r.List, participants...)
	return nil
}

// Participants implements pkgcore.RetentionRegistrar.
func (r *FixedRegistrar) Participants() []pkgcore.RetentionParticipant { return r.List }

var _ pkgcore.RetentionRegistrar = (*FixedRegistrar)(nil)
