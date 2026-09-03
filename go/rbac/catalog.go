package rbac

import "sort"

// catalog is the frozen snapshot of every resource:action permission the
// modules of one host declared through pkgcore's PermissionRegistrar.
//
// Why a snapshot rather than a live read of the registry: the same reason
// go/config snapshots its configuration schema in Attach. Modules register
// in bootstrap order, so a read taken while registration is still running
// would see a partial set, and a read taken per request would let the
// answer to "is this a real permission" change under a running system.
// Kernel.Bootstrap returns once every module has registered; Attach reads
// the registrar exactly then, exactly once, and the result never changes
// again for the life of the process.
//
// What the catalog is for: rejecting a GRANT of a permission no module
// declared (ErrUnknownPermission). It is deliberately not consulted the
// same way at check time -- an unknown permission simply denies there,
// because a permission check must never error a request open. That
// asymmetry is stated in the module's AGENTS.md and pinned by tests.
//
// The catalog holds bare strings. Growing them into a
// PermissionDecl{Key, Description, Group} record is deferred: the only
// consumer of that metadata is the admin console's auto-rendered
// role-configuration page, and changing pkgcore's frozen
// PermissionRegistrar.Add(perms ...string) is a breaking change for every
// module already calling it.
type catalog struct {
	// known is the membership set Has answers from.
	known map[string]struct{}

	// sorted is the same set in a stable order, so anything that renders
	// or logs the catalog is deterministic. pkgcore's registrar already
	// returns its permissions sorted; sorting again here means the
	// catalog's own ordering guarantee does not depend on that.
	sorted []string
}

// newCatalog freezes perms into a catalog. Duplicates collapse (pkgcore's
// registrar already rejects them at registration time, so a duplicate
// reaching here would be a pkgcore bug, not a caller error worth a second
// error path), and the empty string is dropped: it is not a permission,
// and keeping it would let a grant of "" succeed.
func newCatalog(perms []string) catalog {
	known := make(map[string]struct{}, len(perms))
	for _, p := range perms {
		if p == "" {
			continue
		}
		known[p] = struct{}{}
	}
	sorted := make([]string, 0, len(known))
	for p := range known {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	return catalog{known: known, sorted: sorted}
}

// Has reports whether perm was declared by some module. A catalog built
// from no permissions at all answers false for everything, which is the
// correct fail-closed answer for a host whose modules declared none.
func (c catalog) Has(perm string) bool {
	_, ok := c.known[perm]
	return ok
}

// permissions returns the declared permissions in sorted order. The
// returned slice is a copy, so a caller cannot mutate the frozen snapshot
// through it.
func (c catalog) permissions() []string {
	out := make([]string, len(c.sorted))
	copy(out, c.sorted)
	return out
}
