package rbac

import "context"

// Authorizer is the authorization surface every consumer of this module
// programs against. *Service implements it; a host that wants to fake
// authorization in its own tests can implement it too, which is why the
// middleware and the reference app take this interface rather than the
// concrete type.
//
// Its shape is the one docs/internal/05-identity-and-access.md pins --
// Can, ListPermissions and AssignRole with exactly those signatures --
// plus two deliberate additions that document itself implies but does not
// spell out:
//
//   - RevokeRole, because a grant that can only be created is not an
//     access-control system; the document's sketch shows the happy path,
//     not the whole lifecycle.
//   - DataScope, because that document's organization-tree section
//     requires data to be shown according to both the permission and the
//     subtree in scope -- a row-filtering requirement Can deliberately
//     cannot answer (see the DataScope type's own doc comment for why the
//     two questions are separate).
//
// EVERY method takes the Subject explicitly rather than reading it from
// ctx. That is the module's defining boundary made structural: whoever
// authenticates assembles Subject{TenantID, UserID} and passes it in, so
// rbac never needs to know how identity was established and never imports
// authn. WithSubject/SubjectFromContext exist for the HTTP middleware to
// carry a Subject across a handler boundary, not as an alternative input
// to these calls.
type Authorizer interface {
	// Can reports whether sub holds the permission "<resource>:<action>"
	// ANYWHERE within its own tenant, ignoring organization-tree scope.
	//
	// It is the coarse gate: true means the request may proceed, not that
	// every row is visible. A handler that returns tenant data must also
	// call DataScope and filter with it -- see DataScope's doc comment for
	// the case where the two answers deliberately differ.
	//
	// Deny by default: a subject with no matching grant gets (false, nil),
	// never an error, and an unknown permission denies rather than
	// erroring. An error means the decision could not be made at all (the
	// subject was incomplete, or storage failed), and a caller must treat
	// it as a denial.
	Can(ctx context.Context, sub Subject, action, resource string) (bool, error)

	// ListPermissions returns every permission sub holds anywhere within
	// its own tenant, sorted and de-duplicated. Organization-tree scope is
	// flattened away exactly as in Can, so the list answers "what may this
	// user do somewhere", which is what a signed-in client needs to decide
	// which navigation entries to render.
	//
	// It is the call authn's /me handler makes; rbac exposes no endpoint of
	// its own for it (see Module.OpenAPISpec).
	ListPermissions(ctx context.Context, sub Subject) ([]string, error)

	// AssignRole grants sub the role named by its key, at scope. A
	// tenant-wide Scope grants it across the whole tenant; a Scope naming a
	// node grants it over that node's subtree only.
	//
	// It does NOT check whether the caller is allowed to grant roles. rbac
	// is the decision engine, not its own gatekeeper -- see the
	// PermissionManage doc comment for why that is deliberate.
	AssignRole(ctx context.Context, sub Subject, role string, scope Scope) error

	// RevokeRole withdraws the grant AssignRole created, at exactly the
	// same scope. Revoking a grant that is not there returns
	// ErrBindingNotFound rather than succeeding silently; see the method's
	// implementation comment for why revocation is strict where assignment
	// is idempotent.
	RevokeRole(ctx context.Context, sub Subject, role string, scope Scope) error

	// DataScope reports over which slice of the organization tree sub holds
	// "<resource>:<action>", for row-level filtering. See the DataScope
	// type.
	DataScope(ctx context.Context, sub Subject, action, resource string) (DataScope, error)
}

// Permission composes the canonical "<resource>:<action>" permission string
// docs/internal/05-identity-and-access.md fixes as the single permission
// naming convention. Modules declare permissions in that form on
// reg.Permissions during Register, Can takes the two halves separately, and
// the HTTP middleware takes the composed string -- this function is what
// keeps a host from hand-concatenating a third spelling.
//
// It performs no validation: a resource or action that is empty or itself
// contains a colon simply produces a string no module ever declared, which
// the frozen catalog rejects at grant time and denies at check time. That
// is the fail-closed outcome, so there is nothing here to guard.
func Permission(resource, action string) string {
	return resource + ":" + action
}

// compile-time check that *Service is the Authorizer.
var _ Authorizer = (*Service)(nil)
