package rbac

import (
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// Service is rbac's runtime handle: the value Module.Attach returns once
// Kernel.Bootstrap has finished and the permission catalog has frozen.
//
// It holds everything a decision needs and nothing a decision could
// mutate: the frozen catalog, the three repositories, the host's optional
// SubtreeResolver, the event bus the invalidation events travel on, and
// the cache lifetime. Constructing one performs no I/O; Attach is the only
// constructor, because a Service built any other way would carry a catalog
// nobody froze.
//
// It is the type that will implement the Authorizer interface (Can,
// ListPermissions, AssignRole, RevokeRole, DataScope). Those methods, the
// grant path and the cache arrive with this module's evaluation block; as
// of this change the type exists so the wiring seam -- Register, then
// Bootstrap, then Attach, then a handle the host keeps -- is complete and
// testable end to end, and so the host's wiring never has to change again
// when the behavior lands.
type Service struct {
	// catalog is the frozen snapshot of every permission the host's
	// modules declared, taken in Attach. See catalog.go for why it is a
	// snapshot rather than a live registry read.
	catalog catalog

	// roles, rolePermissions and bindings are the module's three
	// repositories, all built on the one *gorm.DB the Module was
	// constructed with.
	roles           *RoleRepository
	rolePermissions *RolePermissionRepository
	bindings        *RoleBindingRepository

	// subtree is the host's organization-tree seam, nil when no host wired
	// one. Nil is a supported configuration, not a broken one: a host with
	// no organization module still runs, because a tenant-wide binding
	// needs no path resolution at all. What nil must never do is widen a
	// node-scoped binding into a tenant-wide one -- an unresolvable
	// narrowing denies (see scope.go and errors.go's ErrSubtreeUnresolved).
	subtree SubtreeResolver

	// bus is the registry's event bus, on which grant changes are
	// published so other replicas invalidate their own caches.
	bus pkgcore.EventBus

	// cacheTTL is the anti-loss expiry of the process-local decision
	// cache: the events above are the primary invalidation path, and this
	// bounds how long a decision can be wrong if one is ever lost. It is a
	// constructor option rather than a dynamic configuration item on
	// purpose -- reading one would add an rbac -> config edge the
	// dependency graph does not have.
	cacheTTL time.Duration
}
