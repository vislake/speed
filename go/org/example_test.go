package org_test

// Runnable documentation for org's public API, mirroring
// go/dbkit/example_test.go's convention: this example is compiled AND
// executed by `go test`, so a change to org's public API that breaks the
// documented usage fails the build rather than only rotting in prose.

import (
	"context"
	"fmt"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"

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

	// Output:
	// subtree of the group:
	//   depth 0  Acme Dental    (group)
	//   depth 1  North Region   (region)
	//   depth 2  Store 7        (store)
	// ancestors of Store 7, root first:
	//   Acme Dental
	//   North Region
	// delete without cascade: org.node_has_children
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
	// HMAC blind index. The key below is a literal only because this is an
	// example; a host injects it from its own secret store, and it must be a
	// DIFFERENT secret from the encryption key.
	indexer, err := dbkit.NewBlindIndexer("email_index", []byte("example-blind-index-key-32-bytes"), dbkit.NormalizeEmail)
	if err != nil {
		fmt.Println("blind indexer:", err)
		return
	}
	cipher, err := dbkit.NewCipher([]byte("example-email-cipher-key-32bytes"))
	if err != nil {
		fmt.Println("cipher:", err)
		return
	}
	dbkit.RegisterEncryptedSerializer(org.EmailSerializerName, cipher)

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

	// Bootstrap is what hands the module its host seams -- the event bus,
	// the mailer, the key-value store the rate limiter counts in, and the
	// merged message catalog. Nothing in org reads them before this point.
	if _, bootErr := pkgcore.NewKernel(pkgcore.DeploymentModeStandalone).Bootstrap(ctx, module); bootErr != nil {
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
