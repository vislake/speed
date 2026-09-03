package storage

import (
	"context"
	"errors"

	"github.com/vislake/speed/go/pkgcore"
)

// This file is the module's shared home for the two host seams every
// runtime reads and for the domain-event vocabulary those runtimes publish
// on.
//
// The seams: storage services never import infrastructure. Bytes live in
// whatever pkgcore.ObjectStore the registry was bootstrapped with (a local
// directory in standalone deployments, S3 in distributed ones), and domain
// events go out on whatever pkgcore.EventBus the registry carries. Each
// service holds the slice of the registry it needs through the embedded
// serviceHost below -- read at call time, never captured, so a revoke or a
// replacement is honored by the next call -- and every seam access fails
// closed when the registry was never attached or carries no such thing.
//
// The vocabulary: the payload structs the two object events carry live here
// next to the seam machinery because events.go is the file module.go and
// the runtime services share. module.go declares the events themselves
// (EventObjectCompleted, EventObjectDeleted, and their catalog entries in
// objectEventDecls); the services publish them.

// hostSeams is the slice of *pkgcore.Registry the module's services read.
// Declaring the two accessors as an interface keeps every service testable
// against fakes and honest about its host dependency: ObjectService needs a
// store for bytes and a bus for the completion event, LifecycleService the
// same pair for deletion and sweeping, DeriveService the store for reading
// originals and writing derivatives -- and nothing else the registry
// carries.
type hostSeams interface {
	// ObjectStore returns the store the registry was bootstrapped with.
	ObjectStore() pkgcore.ObjectStore
	// EventBus returns the bus the registry's Events registrar installs
	// subscriptions on.
	EventBus() pkgcore.EventBus
}

// compile-time check that the concrete registry satisfies the seam.
var _ hostSeams = (*pkgcore.Registry)(nil)

// serviceHost is embedded by every storage service (ObjectService,
// DeriveService, LifecycleService). It carries the registry slice the
// service reads at call time and the two guarded accessors every service
// shares: requireStore and publish. Embedding one type keeps the
// fail-closed seam discipline in one place instead of three copies that
// could drift.
type serviceHost struct {
	// host is nil until Module.Register hands the registry over (attach);
	// before then every seam access fails closed.
	host hostSeams
}

// attach wires the registry the module registered against into the service.
// It is called exactly once, by Module.Register, and performs no I/O: it
// stores the registry, and the store and bus are read from it at call time
// so the service always uses the registry's final wiring.
func (h *serviceHost) attach(reg *pkgcore.Registry) { h.host = reg }

// requireStore returns the object store the registry resolved, or fails
// closed: no registry attached yet, or a registry carrying no store, means
// no bytes can go anywhere, and the service says so with
// storage.store_unavailable before attempting anything (calling a method
// on a nil ObjectStore would panic, which is why the seam is guarded here
// rather than at the call sites).
func (h *serviceHost) requireStore() (pkgcore.ObjectStore, error) {
	if h.host == nil {
		return nil, ErrStoreUnavailable
	}
	if st := h.host.ObjectStore(); st != nil {
		return st, nil
	}
	return nil, ErrStoreUnavailable
}

// publish sends one domain event on the registry's bus. The registry is
// read at call time, so publishing uses the bus the module registered its
// event catalog on. Callers treat a returned error as a logged, recovered
// anomaly: the durable fact the event announces was already committed
// before publishing was attempted.
func (h *serviceHost) publish(ctx context.Context, evt pkgcore.Event) error {
	if h.host == nil {
		return errors.New("storage: no host registry wired")
	}
	bus := h.host.EventBus()
	if bus == nil {
		return errors.New("storage: registry carries no event bus")
	}
	return bus.Publish(ctx, evt)
}

// ObjectCompletedPayload is the JSON payload of EventObjectCompleted.
// ObjectID is the object's id, Size its finalized byte size, and MIME the
// media type the bytes were validated to carry.
type ObjectCompletedPayload struct {
	ObjectID string `json:"object_id"`
	Size     int64  `json:"size"`
	MIME     string `json:"mime"`
}

// ObjectDeletedPayload is the JSON payload of EventObjectDeleted. ObjectID
// is the id of the deleted object.
type ObjectDeletedPayload struct {
	ObjectID string `json:"object_id"`
}
