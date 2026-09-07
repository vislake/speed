package compliance

// This file is the export manifest's retention/cleanup story -- the answer
// to "who deletes the stored ExportManifest objects, and when?". Export
// itself deletes a manifest the moment delivery through go/sharing fails
// (see export.go: an un-shareable copy of a tenant's complete data must
// not persist, and admin retries must not accumulate dumps), so the only
// manifests left in the object store are successfully delivered ones. A
// delivered manifest is anchored to the delivery share minted against it:
// the share dies at its expiry (exportDeliveryExpiry, 24 hours by
// default), and once it is gone the stored manifest is unreachable dead
// weight -- but the pkgcore.ObjectStore seam has no listing primitive
// (pkgcore/objectstore.go: PutObject/GetObject/DeleteObject only), so no
// sweep could enumerate the "compliance/exports/" prefix to reap it. The
// durable record of what was stored is the AuditActionExportRequest audit
// event every completed Export leaves in dbkit/audit's append-only
// audit_events table (Changes.After carries object_key and
// share_expires_at), and reading that record back is how this module's own
// cleanup finds its objects: Module.Register registers the
// compliance.export_manifests retention participant below, whose Sweep --
// running on the module's own per-tenant retention sweep, the mechanism
// RetentionService already ships and hosts schedule -- reaps every stored
// manifest whose delivery share's expiry has fallen past the sweep's
// cutoff, exactly the way a row-based participant reaps soft-deleted rows
// whose deleted_at has fallen past it.
//
// The sweep boundary is the tenant's own retention window, by design: a
// manifest is deleted once its share has been expired for longer than the
// window the operator configured for expired data generally
// (ConfigDefaultRetentionWindow), so the retention of export objects
// follows the same operator-tunable policy as every soft-deleted row,
// never a second, export-specific schedule. Deleting earlier -- the moment
// the share expires -- was considered and rejected for exactly the reason
// rows are not deleted the moment they are soft-deleted: the retention
// window is this module's configured answer to "how long does expired data
// survive before the periodic sweep reaps it", and an export object past
// its delivery window is expired data like any other.
//
// The audit trail is append-only, so a reaped export event stays on the
// table forever and a re-run of the sweep must report 0 for already-
// deleted objects, matching every row-based participant's documented
// retry-convergence contract: the sweep probes each candidate object with
// GetObject first and only counts a DeleteObject that removed something
// that was there. Only events with Result.Success true are candidates --
// a delivery-failed event's object was deleted by Export itself (or never
// tracked at all), and an export whose audit record failed to write is
// precisely the ErrAuditRecordFailed case Export already surfaces for
// operator attention.
//
// A sweep reads the tenant's whole audit trail to find its candidates --
// the same O(rows for the tenant) honest limitation AuditQuery documents
// for its own application-code filtering; the object keys an event may
// name are additionally confined to that same tenant's own
// compliance/exports/ prefix before any object is touched, so a malformed
// or hostile event inside one tenant's trail can never reach another
// tenant's objects (or any other key in the shared store).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"
)

// exportManifestsParticipantName is the pkgcore.RetentionParticipant Name
// go/compliance registers for its own export-manifest cleanup, and the
// name under which the participant's reaped counts appear in
// SweepResult.Reaped and the sweep's own audit event. The module's own
// participant is registered by Module.Register, ahead of any host-added
// participant, so a host that wanted to register its own sweep logic under
// this name would collide with the module's own -- the Name is reserved,
// like the module's audit actions and permissions.
const exportManifestsParticipantName = "compliance.export_manifests"

// exportDeliveryAfter is the slice of one AuditActionExportRequest event's
// Changes.After document the export-manifest cleanup reads back -- the two
// fields emitExportAudit records on a successful delivery. Unknown fields
// (participants, errors, share_id) are ignored by encoding/json.
type exportDeliveryAfter struct {
	ObjectKey      string    `json:"object_key"`
	ShareExpiresAt time.Time `json:"share_expires_at"`
}

// exportManifestsParticipant returns the pkgcore.RetentionParticipant that
// owns the export-manifest side of the module's retention story -- see this
// file's own header comment. Its Sweep reaps tenant's stored export
// manifests whose delivery share expired at or before cutoff, and its
// Erase answers every right-to-erasure request with an explicit
// nothing-to-erase -- (0, nil) -- since a manifest is a tenant-wide
// bundle, never a single subject's rows: destroying it on one subject's
// request would destroy other subjects' delivered packages, so a subject
// erasure must not touch it. Registration makes Erase mandatory
// (pkgcore.ErrNilRetentionErase); stating the nothing-to-erase fact
// explicitly is what keeps an erasure request from reporting full success
// while silently skipping this participant. It has no Export of its own.
// repo is the same *audit.Repository the module reads its export events
// through, and store the ObjectStore Export wrote the manifests to.
func exportManifestsParticipant(repo *audit.Repository, store pkgcore.ObjectStore) pkgcore.RetentionParticipant {
	return pkgcore.RetentionParticipant{
		Name: exportManifestsParticipantName,
		Sweep: func(ctx context.Context, tenant pkgcore.TenantID, cutoff time.Time) (int, error) {
			return sweepExportManifests(ctx, repo, store, tenant, cutoff)
		},
		// See the function's doc comment: nothing subject-shaped lives in
		// a delivered manifest, so the module's declared erasure answer
		// is an explicit (0, nil) -- never a nil Erase, which the
		// registrar refuses and which would otherwise read as a silent
		// skip in the erasure audit.
		Erase: func(context.Context, pkgcore.SubjectRef) (int, error) {
			return 0, nil
		},
	}
}

// sweepExportManifests reaps tenant's stored export manifests whose
// delivery share expired at or before cutoff, and reports how many objects
// it actually deleted. See this file's header comment for the mechanism
// (the tenant's own AuditActionExportRequest events are the record of what
// was stored and when its share dies) and for the retry-convergence rule
// this implements (an object already deleted by an earlier pass reports
// nothing on the next). It runs under the sweep's system context; the
// reads and deletes it performs carry no tenant of their own, since the
// audit trail is platform data and the object store is key-addressed --
// the tenant argument is the boundary, applied to both the events listed
// (ListByTenant) and the keys touched (the prefix guard below).
func sweepExportManifests(ctx context.Context, repo *audit.Repository, store pkgcore.ObjectStore, tenant pkgcore.TenantID, cutoff time.Time) (int, error) {
	events, err := repo.ListByTenant(ctx, string(tenant))
	if err != nil {
		return 0, err
	}
	prefix := fmt.Sprintf("compliance/exports/%s/", tenant)
	reaped := 0
	for _, evt := range events {
		if evt.Action != AuditActionExportRequest || !evt.Success {
			continue
		}
		var changes struct {
			After *exportDeliveryAfter `json:"After"`
		}
		if err := json.Unmarshal(evt.Changes, &changes); err != nil || changes.After == nil {
			// An export event whose Changes cannot be read names no
			// re-claimable object -- skip it rather than failing the whole
			// tenant's sweep over one unreadable record.
			continue
		}
		delivery := *changes.After
		// The tenant being swept is the boundary: only keys under this
		// tenant's own compliance/exports/ prefix may be touched, never a
		// key a malformed event planted under another tenant's prefix (or
		// elsewhere in a store other modules share).
		if !strings.HasPrefix(delivery.ObjectKey, prefix) {
			continue
		}
		if delivery.ShareExpiresAt.IsZero() || delivery.ShareExpiresAt.After(cutoff) {
			continue
		}
		// Probe before deleting, so a re-run over already-reaped events
		// reports 0 -- the documented retry-convergence contract every
		// participant's Sweep callback keeps.
		r, err := store.GetObject(ctx, delivery.ObjectKey)
		if errors.Is(err, pkgcore.ErrObjectNotFound) {
			continue
		}
		if err != nil {
			return reaped, err
		}
		if err := r.Close(); err != nil {
			return reaped, fmt.Errorf("compliance: close export manifest probe %q: %w", delivery.ObjectKey, err)
		}
		if err := store.DeleteObject(ctx, delivery.ObjectKey); err != nil {
			return reaped, err
		}
		reaped++
	}
	return reaped, nil
}
