package pkgcore

import (
	"fmt"
	"os"
)

// ObjectStoreRegistry mirrors EventBusRegistry for the "objectstore" seam
// ("objectstore.s3" registers through the objectstore/s3 subpackage).
var ObjectStoreRegistry = newBuiltinObjectStoreRegistry()

func newBuiltinObjectStoreRegistry() *SeamRegistry[ObjectStore] {
	r := NewSeamRegistry[ObjectStore]()
	mustRegister(r, Registration[ObjectStore]{
		// Known limitation of the one-Registration-one-capability-set shape
		// versus a capability that depends on Config: Capabilities is
		// unconditionally 0 while
		// localObjectStoreFromConfig has two durability modes -- a
		// throwaway MkdirTemp root (honestly 0: the random root dies with
		// the process that created it, since nothing after a restart can
		// find it again, and it is the shape every config-less preset build
		// takes) and a host-supplied persistent cfg["directory"] whose
		// objects genuinely outlive a process restart (the directory
		// outlasts the process), over which the warnIfNotDurable startup
		// banner names a loss that does not exist. The registration is
		// deliberately not split into two names: under-declaring is the
		// safe direction (warnIfNotDurable treats SurvivesRestart
		// and Stateless equivalently, no deployment mode requires the bit,
		// and a host with a persistent directory can inject the store
		// directly with WithObjectStore(store, SurvivesRestart) when it
		// wants the banner gone), and splitting would change the name space
		// hosts and Presets already pin.
		Name:         "objectstore.local",
		Capabilities: 0,
		New:          localObjectStoreFromConfig,
	})
	return r
}

// localObjectStoreFromConfig adapts Config onto NewLocalObjectStore. Unlike
// the SMTP and S3 seams, a directory is always constructible: cfg["directory"]
// names a persistent one when the host wants objects to survive a restart,
// and an empty value falls back to a fresh private temporary directory --
// the same throwaway-by-default behaviour the standalone composition's
// local store has.
func localObjectStoreFromConfig(cfg Config) (ObjectStore, error) {
	directory := cfg["directory"]
	if directory == "" {
		created, err := os.MkdirTemp("", "pkgcore-object-store-*")
		if err != nil {
			return nil, fmt.Errorf("pkgcore: builtin objectstore.local seam: %w", err)
		}
		// The temp directory was created by this registration and is owned
		// by it: the returned value's Close() error removes the directory
		// again, so a Kernel.Shutdown (or a failed Bootstrap, which closes
		// what it resolved) does not leak the throwaway tree. A store over
		// a host-supplied cfg["directory"] never carries a closer: that
		// directory is the host's data, which nothing here may delete.
		store := NewLocalObjectStore(created)
		return &closableObjectStore{ObjectStore: store, removeRoot: func() error {
			return os.RemoveAll(created)
		}}, nil
	}
	return NewLocalObjectStore(directory), nil
}

// closableObjectStore is the value "objectstore.local"'s registration
// returns when it created the store's directory itself: the store itself
// (whose promoted methods satisfy ObjectStore) plus the Close() error method
// that removes the temporary directory the registration created, per the
// Registration-level resource-ownership contract. A host that calls
// NewLocalObjectStore itself keeps owning its directory, exactly as that
// constructor's own doc comment promises.
type closableObjectStore struct {
	ObjectStore
	removeRoot func() error
}

// Close removes the temporary directory the registration created. Nothing
// may use the store after Close; a host shuts its seams down last.
func (s *closableObjectStore) Close() error {
	if s.removeRoot != nil {
		return s.removeRoot()
	}
	return nil
}
