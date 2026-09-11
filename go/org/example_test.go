package org_test

// Runnable documentation for org's public API, mirroring
// go/dbkit/example_test.go's convention: this example is compiled AND
// executed by `go test`, so a change to org's public API that breaks the
// documented usage fails the build rather than only rotting in prose.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/componenttest"

	"github.com/vislake/speed/go/org"
)

// Example builds the shape the reference app actually needs -- a dental
// group, a region beneath it, and a store beneath that -- and then queries
// the tree both ways: down, for everything beneath a node, and up, for a
// node's chain of ancestors.
//
// Note what never appears: a tenant identifier passed to any org call. The
// tenant travels in the context and nowhere else, which is what makes it
// impossible for a caller to name someone else's tenant.
func Example() {
	ctx := context.Background()

	// A real host opens PostgreSQL in the distributed deployment mode
	// (dbkit.DialectPostgres). SQLite keeps this example self-contained
	// under `go test`, with no external service required -- which is exactly
	// what the standalone deployment mode does in production too.
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:org_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	// Migrations are versioned SQL, applied through dbkit's registry. There
	// is no AutoMigrate anywhere in this codebase.
	module := org.NewModule(db)
	registry := dbkit.NewMigrationRegistry()
	if regErr := registry.Register(module); regErr != nil {
		fmt.Println("register migrations:", regErr)
		return
	}
	if applyErr := registry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations:", applyErr)
		return
	}

	// The tenant comes from the context. In a served request it is put there
	// by tenancy.Middleware, from the access token's claims.
	ctx = pkgcore.WithTenant(ctx, "acme-dental")
	tree := module.Tree()

	// Kind is the tenant's own business vocabulary; org does not enumerate
	// the legal values, which is the point of an arbitrary-depth tree.
	root, err := tree.CreateRoot(ctx, "Acme Dental", "group")
	if err != nil {
		fmt.Println("create root:", err)
		return
	}
	region, err := tree.CreateChild(ctx, root.ID, "North Region", "region")
	if err != nil {
		fmt.Println("create region:", err)
		return
	}
	store, err := tree.CreateChild(ctx, region.ID, "Store 7", "store")
	if err != nil {
		fmt.Println("create store:", err)
		return
	}

	// Down: one indexed prefix scan returns the node and everything beneath
	// it, however deep the tree runs. No recursive query is involved.
	subtree, err := tree.Subtree(ctx, root.ID)
	if err != nil {
		fmt.Println("subtree:", err)
		return
	}
	fmt.Println("subtree of the group:")
	for _, node := range subtree {
		fmt.Printf("  depth %d  %-14s (%s)\n", node.Depth, node.Name, node.Kind)
	}

	// Up: the ancestor chain is read from the node's own materialized path,
	// so it costs one query no matter how deep the node sits.
	ancestors, err := tree.Ancestors(ctx, store.ID)
	if err != nil {
		fmt.Println("ancestors:", err)
		return
	}
	fmt.Println("ancestors of Store 7, root first:")
	for _, node := range ancestors {
		fmt.Printf("  %s\n", node.Name)
	}

	// Deleting a node that still has children needs an explicit cascade:
	// org never re-parents orphans, because that silently widens the data
	// scope of everyone bound beneath the deleted node.
	if delErr := tree.Delete(ctx, region.ID, false); delErr != nil {
		// The API returns a structured code, never a localized sentence:
		// go/org/locales holds the zh-CN and en-US text for every code.
		if appErr, ok := apperr.As(delErr); ok {
			fmt.Println("delete without cascade:", appErr.Code)
		}
	}

	// A leaf node deletes cleanly -- and a delete is a mark-delete, not a
	// physical DELETE: the row survives, hidden from ordinary reads, and
	// Restore brings it back with its original data intact. This is the
	// "oops, get it back" scenario mark-delete exists for.
	if delErr := tree.Delete(ctx, store.ID, false); delErr != nil {
		fmt.Println("delete store:", delErr)
		return
	}
	if _, getErr := tree.Get(ctx, store.ID); getErr != nil {
		if appErr, ok := apperr.As(getErr); ok {
			fmt.Println("get after delete:", appErr.Code)
		}
	}
	restored, restoreErr := tree.Restore(ctx, store.ID)
	if restoreErr != nil {
		fmt.Println("restore store:", restoreErr)
		return
	}
	fmt.Println("restored:", restored.Name)

	// Output:
	// subtree of the group:
	//   depth 0  Acme Dental    (group)
	//   depth 1  North Region   (region)
	//   depth 2  Store 7        (store)
	// ancestors of Store 7, root first:
	//   Acme Dental
	//   North Region
	// delete without cascade: org.node_has_children
	// get after delete: org.node_not_found
	// restored: Store 7
}

// Example_membershipAndScope walks the flow a dental group actually goes
// through: place two stores under a group, invite somebody into one of them,
// let them accept, and then ask what each member is allowed to see.
//
// The last part is the seam authorization consumes. org.Scope's methods are
// built from stdlib types only, so rbac declares the identical interface in
// its own package and accepts this implementation structurally -- neither
// module ever imports the other.
func Example_membershipAndScope() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:org_example_members?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	// The invitation address is encrypted at rest and made queryable by an
	// HMAC blind index. Both halves are wired through the module's own
	// constructors, so the indexer's column and the serializer's name never
	// cross this boundary as hand-typed strings (see NewEmailIndexer's and
	// RegisterEmailSerializer's own doc comments for why). The keys below are
	// literals only because this is an example; a host injects them from its
	// own secret store, and they must be DIFFERENT secrets from each other.
	indexer, err := org.NewEmailIndexer([]byte("example-blind-index-key-32-bytes"))
	if err != nil {
		fmt.Println("blind indexer:", err)
		return
	}
	cipher, err := dbkit.NewCipher([]byte("example-email-cipher-key-32bytes"))
	if err != nil {
		fmt.Println("cipher:", err)
		return
	}
	if regErr := org.RegisterEmailSerializer(cipher); regErr != nil {
		fmt.Println("register serializer:", regErr)
		return
	}

	// WithInvitationEmailDisabled keeps this example's output free of the
	// standalone mode's console mailer. A host that lets org deliver the
	// invitation instead wires WithMailFrom and WithInvitationLinkBuilder,
	// and the message goes out through the pkgcore.Mailer seam in the
	// RECIPIENT's language.
	module := org.NewModule(db,
		org.WithEmailIndexer(indexer),
		org.WithInvitationEmailDisabled(),
	)

	registry := dbkit.NewMigrationRegistry()
	if regErr := registry.Register(module); regErr != nil {
		fmt.Println("register migrations:", regErr)
		return
	}
	if applyErr := registry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations:", applyErr)
		return
	}

	// the assembly is what hands the module its host seams -- the event bus,
	// the mailer, the key-value store the rate limiter counts in, and the
	// merged message catalog. Nothing in org reads them before this point.
	if _, bootErr := componenttest.DeclareModules(module); bootErr != nil {
		fmt.Println("bootstrap:", bootErr)
		return
	}

	ctx = pkgcore.WithTenant(ctx, "acme-dental")
	tree, members, invitations := module.Tree(), module.Members(), module.Invitations()

	group, err := tree.CreateRoot(ctx, "Acme Dental", "group")
	if err != nil {
		fmt.Println("create group:", err)
		return
	}
	north, err := tree.CreateChild(ctx, group.ID, "North Store", "store")
	if err != nil {
		fmt.Println("create north:", err)
		return
	}
	if _, err = tree.CreateChild(ctx, group.ID, "South Store", "store"); err != nil {
		fmt.Println("create south:", err)
		return
	}

	// The owner sits at the group; every user id here is an opaque string
	// learned from an authenticated caller or a domain event. org never
	// imports an authn type to hold one.
	if _, err = members.Add(ctx, "user-owner", group.ID); err != nil {
		fmt.Println("add owner:", err)
		return
	}

	// Inviting returns the token exactly once. It is a bearer credential:
	// it belongs in the message addressed to the invitee and nowhere else --
	// no log line, no API response, no event payload.
	invite, err := invitations.Invite(ctx, org.InviteRequest{
		Email:         "dentist@example.test",
		NodeID:        north.ID,
		InviterUserID: "user-owner",
		Locale:        "en-US",
	})
	if err != nil {
		fmt.Println("invite:", err)
		return
	}

	// Acceptance resolves the token inside the tenant the context already
	// carries. The token never names a tenant, so a link that arrives at the
	// wrong tenant's host simply does not match anything.
	if _, err = invitations.Accept(ctx, invite.Token, "user-dentist"); err != nil {
		fmt.Println("accept:", err)
		return
	}

	roster, err := members.List(ctx, group.ID)
	if err != nil {
		fmt.Println("list members:", err)
		return
	}
	fmt.Printf("members under the group: %d\n", len(roster))

	scope := module.Scope()
	for _, user := range []string{"user-owner", "user-dentist", "user-stranger"} {
		visible, scopeErr := scope.MemberNodeIDs(ctx, user)
		if scopeErr != nil {
			fmt.Println("scope:", scopeErr)
			return
		}
		fmt.Printf("%s can see %d node(s)\n", user, len(visible))
	}

	// Output:
	// members under the group: 2
	// user-owner can see 3 node(s)
	// user-dentist can see 1 node(s)
	// user-stranger can see 0 node(s)
}

// ExampleMemberService_TenantsOf answers the question a host's
// authn.MembershipReader must be able to answer at sign-in -- "which
// tenants does this account belong to" -- from org's own rows, the same
// rows every Get/Add/Remove answer, instead of from a host-maintained
// roster that has to be told about every tenant a registration creates.
//
// The question spans tenants by definition, so org serves it only to a
// system context: the caller (here, the example itself) declares the
// purpose it will take the grant for through pkgcore.RegisterSystemPurpose
// -- exactly what a host's Module.Register does -- and obtains the grant
// with pkgcore.WithSystemContext (a business module uses tenancy's audited
// wrapper instead). A caller holding only an organization's own context is
// refused before any database work happens.
func ExampleMemberService_TenantsOf() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:org_example_tenants_of?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	// Registration validates that a host wired the seams the module's
	// invitation half needs; the example wires the same minimal set
	// Example_membershipAndScope uses, with the invitation email disabled so
	// nothing goes out through the console mailer.
	indexer, err := org.NewEmailIndexer([]byte("example-blind-index-key-32-bytes"))
	if err != nil {
		fmt.Println("blind indexer:", err)
		return
	}
	module := org.NewModule(db,
		org.WithEmailIndexer(indexer),
		org.WithInvitationEmailDisabled(),
	)

	registry := dbkit.NewMigrationRegistry()
	if regErr := registry.Register(module); regErr != nil {
		fmt.Println("register migrations:", regErr)
		return
	}
	if applyErr := registry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations:", applyErr)
		return
	}
	if _, bootErr := componenttest.DeclareModules(module); bootErr != nil {
		fmt.Println("bootstrap:", bootErr)
		return
	}

	members := module.Members()
	for _, tenant := range []pkgcore.TenantID{"acme-dental", "globex-dental"} {
		tenantCtx := pkgcore.WithTenant(ctx, tenant)
		root, rootErr := module.Tree().CreateRoot(tenantCtx, "Dental Practice", "group")
		if rootErr != nil {
			fmt.Println("create root:", rootErr)
			return
		}
		if _, addErr := members.Add(tenantCtx, "user-owner", root.ID); addErr != nil {
			fmt.Println("add owner:", addErr)
			return
		}
		if tenant == "globex-dental" {
			if _, addErr := members.Add(tenantCtx, "user-globex-only", root.ID); addErr != nil {
				fmt.Println("add globex member:", addErr)
				return
			}
		}
	}

	// A tenant-scoped caller is refused: one organization's context cannot
	// enumerate a person's memberships in every organization.
	if _, refusalErr := members.TenantsOf(pkgcore.WithTenant(ctx, "acme-dental"), "user-owner"); refusalErr != nil {
		if code, ok := apperr.As(refusalErr); ok {
			fmt.Printf("TenantsOf without system context: %s\n", code.Code)
		}
	}

	// A system-context caller gets the real answer, ordered by tenant id.
	sysCtx, err := pkgcore.WithSystemContext(context.Background(), pkgcore.SystemReason{
		Actor:   "platform-operator",
		Purpose: orgExampleTenantsOfPurpose,
	})
	if err != nil {
		fmt.Println("system context:", err)
		return
	}
	tenants, err := members.TenantsOf(sysCtx, "user-owner")
	if err != nil {
		fmt.Println("tenants of:", err)
		return
	}
	fmt.Printf("user-owner belongs to: %v\n", tenants)

	// Output:
	// TenantsOf without system context: org.system_context_required
	// user-owner belongs to: [acme-dental globex-dental]
}

// orgExampleTenantsOfPurpose is the system purpose this example declares
// for its own TenantsOf grant, the same declaration a host makes once at
// boot for the purposes its glue takes system contexts for.
var orgExampleTenantsOfPurpose = pkgcore.SystemPurpose("org.example.tenants_of")

// ExampleTreeService_EnsureRoot shows the boot-time half of "a tenant has a
// tree": one call creates the root on the first boot and returns the stored
// one on every boot after, so a restart re-asserts instead of colliding. The
// name and kind are consulted only when a root genuinely has to be created.
func ExampleTreeService_EnsureRoot() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:org_example_ensure_root?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}
	module := org.NewModule(db)
	registry := dbkit.NewMigrationRegistry()
	if regErr := registry.Register(module); regErr != nil {
		fmt.Println("register migrations:", regErr)
		return
	}
	if applyErr := registry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations:", applyErr)
		return
	}

	ctx = pkgcore.WithTenant(ctx, "acme-dental")
	tree := module.Tree()

	created, err := tree.EnsureRoot(ctx, "Acme Dental", "group")
	if err != nil {
		fmt.Println("first ensure:", err)
		return
	}
	fmt.Printf("first call created: %s (%s)\n", created.Name, created.Kind)

	// The second call -- the next boot -- passes a different name on
	// purpose: an existing root is never renamed by an ensure, its stored
	// name and kind win.
	again, err := tree.EnsureRoot(ctx, "Renamed Practice", "workspace")
	if err != nil {
		fmt.Println("second ensure:", err)
		return
	}
	fmt.Printf("second call kept: %s (%s), same node: %t\n", again.Name, again.Kind, again.ID == created.ID)

	// Output:
	// first call created: Acme Dental (group)
	// second call kept: Acme Dental (group), same node: true
}

// ExampleMemberService_EnsureRootSeat shows placing a person into a tenant
// before any organization tree exists: one call ensures the tree root and
// the person's seat at it, and a repeat -- the next boot -- creates nothing
// twice.
func ExampleMemberService_EnsureRootSeat() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:org_example_ensure_root_seat?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}
	module := org.NewModule(db)
	registry := dbkit.NewMigrationRegistry()
	if regErr := registry.Register(module); regErr != nil {
		fmt.Println("register migrations:", regErr)
		return
	}
	if applyErr := registry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations:", applyErr)
		return
	}

	ctx = pkgcore.WithTenant(ctx, "acme-dental")
	members := module.Members()

	seat, err := members.EnsureRootSeat(ctx, "user-owner", "Acme Dental", "group")
	if err != nil {
		fmt.Println("first ensure:", err)
		return
	}
	root, err := module.Tree().Root(ctx)
	if err != nil {
		fmt.Println("root:", err)
		return
	}
	fmt.Printf("seat at the root: %t, active: %t\n", seat.NodeID == root.ID, seat.IsActive())

	again, err := members.EnsureRootSeat(ctx, "user-owner", "Acme Dental", "group")
	if err != nil {
		fmt.Println("second ensure:", err)
		return
	}
	fmt.Printf("same seat: %t\n", again.ID == seat.ID)

	// Output:
	// seat at the root: true, active: true
	// same seat: true
}

func init() {
	pkgcore.RegisterSystemPurpose(orgExampleTenantsOfPurpose)
}

// ExampleSubjectResolverFunc wires org's caller-identity seam with a
// closure: the adapter gives a plain (r *http.Request) (string, bool)
// function the Subject method WithSubjectResolver takes, so a host whose
// resolver is one closure needs no type declaration. A real resolver must
// derive the caller from a source the server itself verified; this example
// reads a header only to keep the shape visible.
func ExampleSubjectResolverFunc() {
	resolver := org.SubjectResolverFunc(func(r *http.Request) (string, bool) {
		userID := r.Header.Get("X-Verified-User")
		return userID, userID != ""
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Verified-User", "u-42")
	userID, ok := resolver.Subject(req)
	fmt.Println(userID, ok)
	// Output: u-42 true
}

// ExampleFeatureGateFunc wires org's feature-flag seam with a closure: the
// adapter gives a plain (ctx, key) (bool, error) function the IsEnabled
// method WithFeatureGate takes, so a host needs no type declaration -- and
// a reader that already has that exact signature, such as the config
// module's lazy Handle, converts as a plain method value
// (org.FeatureGateFunc(handle.IsEnabled)).
func ExampleFeatureGateFunc() {
	hostFlags := map[string]bool{
		org.FeatureInvitations:     true,
		org.FeatureInvitationEmail: false,
	}
	gate := org.FeatureGateFunc(func(_ context.Context, key string) (bool, error) {
		enabled, ok := hostFlags[key]
		if !ok {
			return false, fmt.Errorf("no such flag: %s", key)
		}
		return enabled, nil
	})

	ctx := context.Background()
	invitations, err := gate.IsEnabled(ctx, org.FeatureInvitations)
	if err != nil {
		panic(err)
	}
	email, err := gate.IsEnabled(ctx, org.FeatureInvitationEmail)
	if err != nil {
		panic(err)
	}
	fmt.Println("invitations:", invitations)
	fmt.Println("invitation_email:", email)

	// Output:
	// invitations: true
	// invitation_email: false
}

// ExampleRegisterEmailSerializer registers the Invitation.Email column's
// cipher through the module's own registrar and builds the matching blind
// indexer through the module's own constructor: the module owns both the
// GORM serializer name and the index column, so neither crosses the host's
// wiring as a hand-typed string. A nil cipher is refused rather than
// registered -- a model whose serializer silently did nothing would store
// invitation addresses in plaintext.
func ExampleRegisterEmailSerializer() {
	cipher, err := dbkit.NewCipher([]byte("example-email-cipher-key-32bytes"))
	if err != nil {
		fmt.Println("cipher:", err)
		return
	}
	if err := org.RegisterEmailSerializer(cipher); err != nil {
		fmt.Println("register:", err)
		return
	}
	if _, err := org.NewEmailIndexer([]byte("example-blind-index-key-32-bytes")); err != nil {
		fmt.Println("indexer:", err)
		return
	}
	if err := org.RegisterEmailSerializer(nil); err != nil {
		fmt.Println("the nil cipher is refused")
	}
	// Output: the nil cipher is refused
}
