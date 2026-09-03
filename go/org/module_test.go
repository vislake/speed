package org

import (
	"context"
	"embed"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
)

func TestModule_Identity(t *testing.T) {
	m := NewModule(nil)

	if got := m.Name(); got != "org" {
		t.Errorf("Name() = %q, want %q", got, "org")
	}
	if got := m.DependsOn(); got != nil {
		t.Errorf("DependsOn() = %v, want nil -- org depends on no other pkgcore.Module, authn least of all", got)
	}
	if got := m.OpenAPISpec(); got != nil {
		t.Errorf("OpenAPISpec() = %q, want nil until the module's spec fragment lands", got)
	}
}

// TestModule_Migrations_ExposesBothDialectsAtTheEmbedRoot pins the layout
// dbkit.MigrationRegistry.Apply requires: a postgres/ and a sqlite/
// subdirectory at the FS root, each holding the same file names. A migration
// added to one dialect and forgotten in the other fails here rather than at a
// deployment that happens to run the other engine.
func TestModule_Migrations_ExposesBothDialectsAtTheEmbedRoot(t *testing.T) {
	fs := NewModule(nil).Migrations()

	names := map[string][]string{}
	for _, dialect := range []string{"postgres", "sqlite"} {
		entries, err := fs.ReadDir(dialect)
		if err != nil {
			t.Fatalf("ReadDir(%q): %v", dialect, err)
		}
		if len(entries) == 0 {
			t.Fatalf("%s/ holds no migration files", dialect)
		}
		for _, e := range entries {
			names[dialect] = append(names[dialect], e.Name())
		}
	}
	if len(names["postgres"]) != len(names["sqlite"]) {
		t.Fatalf("dialect file counts differ: postgres %v, sqlite %v", names["postgres"], names["sqlite"])
	}
	for i, name := range names["postgres"] {
		if names["sqlite"][i] != name {
			t.Errorf("migration %q exists in postgres/ but sqlite/ has %q at the same position", name, names["sqlite"][i])
		}
	}
}

// TestModule_Locales_ShipsBothLanguages pins the i18n rule at the module
// boundary: exactly the two languages the catalog serves, no more and no
// fewer, since Kernel.Bootstrap rejects a module that ships a file for a
// language the others do not.
func TestModule_Locales_ShipsBothLanguages(t *testing.T) {
	entries, err := NewModule(nil).Locales().ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	for _, want := range []string{"zh-CN.toml", "en-US.toml"} {
		if !got[want] {
			t.Errorf("locales are missing %s", want)
		}
	}
	if len(entries) != 2 {
		t.Errorf("locales hold %d files, want exactly zh-CN.toml and en-US.toml", len(entries))
	}
}

// TestModule_Register_DeclaresItsSurface bootstraps org through the real
// kernel -- the same path a host takes -- and asserts every declaration
// arrives on the registry. Bootstrapping rather than calling Register against
// a hand-built Registry is deliberate: it also proves org's locale files
// survive i18n.Builder.AddModule's parity validation, which only runs there.
func TestModule_Register_DeclaresItsSurface(t *testing.T) {
	reg := bootstrapTestModule(t)

	t.Run("permissions", func(t *testing.T) {
		assertContainsAll(t, reg.Permissions.Permissions(), []string{
			PermissionRead, PermissionManage, PermissionInviteMember, PermissionRemoveMember,
		})
	})

	t.Run("audit actions", func(t *testing.T) {
		assertContainsAll(t, reg.AuditActions.Actions(), []string{
			AuditActionNodeCreate, AuditActionNodeRename, AuditActionNodeMove, AuditActionNodeDelete,
		})
	})

	t.Run("published events", func(t *testing.T) {
		var types []string
		for _, decl := range reg.Events.Published() {
			types = append(types, decl.Type)
			if decl.PayloadType == "" || decl.Description == "" {
				t.Errorf("event %q is declared without a payload type or description", decl.Type)
			}
		}
		assertContainsAll(t, types, []string{EventNodeCreated, EventNodeMoved, EventNodeDeleted})
	})

	t.Run("no routes are mounted before the spec fragment lands", func(t *testing.T) {
		if got := reg.Routes.Routes(); len(got) != 0 {
			t.Errorf("Register mounted %d route(s); org's HTTP surface is spec-first and lands with its fragment", len(got))
		}
	})

	t.Run("no configuration schema is declared", func(t *testing.T) {
		// org honours its bounds as package constants rather than declaring
		// a dynamic-config schema it cannot read back (it must not import
		// config). Declaring a schema nothing honours would be a lying
		// schema, so the absence is asserted rather than left implicit.
		if got := reg.Config.Items(); len(got) != 0 {
			t.Errorf("Register declared %d config item(s); org declares none it cannot honour", len(got))
		}
	})
}

// TestModule_Register_DoesNotDeclareAuthnsEvent is the guard for the trap
// that would only surface once the authn module ships: declaring another
// module's event type collides at bootstrap, so org must subscribe to
// authn.user.created without ever declaring it.
func TestModule_Register_DoesNotDeclareAuthnsEvent(t *testing.T) {
	reg := bootstrapTestModule(t)
	for _, decl := range reg.Events.Published() {
		if decl.Type == "authn.user.created" {
			t.Fatal("org declared authn.user.created; only authn may declare it, or both modules collide at bootstrap")
		}
		if got := decl.Type[:4]; got != "org." {
			t.Errorf("org declared event %q, which is not in org's own namespace", decl.Type)
		}
	}
}

// TestModule_Register_CoexistsWithAnotherModule proves org bootstraps
// alongside a module that declares its own permissions, audit actions and
// events -- the real host shape -- rather than only in isolation.
func TestModule_Register_CoexistsWithAnotherModule(t *testing.T) {
	reg, err := pkgcore.NewKernel(pkgcore.DeploymentModeStandalone).
		Bootstrap(context.Background(), newWiredModule(t, nil), neighbourModule{})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	assertContainsAll(t, reg.Permissions.Permissions(), []string{PermissionRead, "neighbour:read"})
}

// TestModule_Register_PerformsNoIO calls Register with a nil database, which
// is what pkgcore.Module's "it only declares" contract requires to be safe:
// any database call inside Register would panic here.
func TestModule_Register_PerformsNoIO(t *testing.T) {
	m := newWiredModule(t, nil)
	reg := pkgcore.NewRegistry(
		pkgcore.NewMemoryEventBus(),
		pkgcore.NewMemoryKVStore(),
		pkgcore.NewConsoleMailer(),
	)
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register against a nil database: %v", err)
	}
}

func TestModule_Tree_ReturnsAUsableService(t *testing.T) {
	m := newWiredModule(t, newTestDB(t))
	ctx := tenantCtx("tenant-a")

	root, err := m.Tree().CreateRoot(ctx, "Acme Dental", "group")
	if err != nil {
		t.Fatalf("CreateRoot through the module's tree: %v", err)
	}
	if root.ID == "" {
		t.Error("the module's tree produced a node with no id")
	}
	first, second := m.Tree(), m.Tree()
	if first != second {
		t.Error("Tree() returns a different service on each call; hosts hold onto it")
	}
}

// neighbourModule stands in for any other module a host bootstraps next to
// org. It declares a disjoint surface, so a collision would mean org claimed
// a name outside its namespace.
type neighbourModule struct{}

func (neighbourModule) Name() string         { return "neighbour" }
func (neighbourModule) DependsOn() []string  { return nil }
func (neighbourModule) Migrations() embed.FS { return embed.FS{} }
func (neighbourModule) Locales() embed.FS    { return embed.FS{} }
func (neighbourModule) OpenAPISpec() []byte  { return nil }
func (neighbourModule) Register(reg *pkgcore.Registry) error {
	if err := reg.Permissions.Add("neighbour:read"); err != nil {
		return err
	}
	return reg.AuditActions.Add("neighbour.thing.do")
}

var _ pkgcore.Module = neighbourModule{}

// assertContainsAll fails t unless got holds every entry in want.
func assertContainsAll(t *testing.T, got, want []string) {
	t.Helper()
	set := make(map[string]bool, len(got))
	for _, v := range got {
		set[v] = true
	}
	for _, v := range want {
		if !set[v] {
			t.Errorf("%q is missing from %v", v, got)
		}
	}
}

// newWiredModule builds an org Module with the wiring Register requires --
// the blind indexer, a sender address and a link builder -- plus whatever the
// caller adds. Every test that bootstraps or registers goes through it, so
// the required-options rule is asserted in exactly one place (see
// TestModule_Register_RefusesAnIndexerlessBoot) rather than re-tested by
// accident everywhere else.
func newWiredModule(t *testing.T, db *gorm.DB, opts ...Option) *Module {
	t.Helper()
	wired := append([]Option{
		WithEmailIndexer(newTestEmailIndexer(t)),
		WithMailFrom(testMailFrom),
		WithInvitationLinkBuilder(testLinkBuilder),
	}, opts...)
	return NewModule(db, wired...)
}

// bootstrapTestModule bootstraps a fully wired org module through the real
// kernel and returns the registry it registered against. Going through
// Bootstrap rather than calling Register on a hand-built Registry is
// deliberate: it is the only path that also merges the locale files and so
// proves they survive i18n.Builder.AddModule's parity validation.
func bootstrapTestModule(t *testing.T, opts ...Option) *pkgcore.Registry {
	t.Helper()
	reg, err := pkgcore.NewKernel(pkgcore.DeploymentModeStandalone).
		Bootstrap(context.Background(), newWiredModule(t, nil, opts...))
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return reg
}

// TestModule_Register_RefusesAnIndexerlessBoot pins the config.Attach
// precedent: a module whose declared surface includes an encrypted-and-
// queryable column refuses to boot without the key that makes the column
// queryable, rather than starting and failing on the first write.
func TestModule_Register_RefusesAnIndexerlessBoot(t *testing.T) {
	_, err := pkgcore.NewKernel(pkgcore.DeploymentModeStandalone).
		Bootstrap(context.Background(), NewModule(nil))
	if !hasCode(err, ErrEmailIndexerRequired.Code) {
		t.Fatalf("Bootstrap without an email indexer error = %v, want org.email_indexer_required", err)
	}
}

// TestModule_Register_RefusesAMailerlessBootWhileTheEmailIsOn pins the other
// boot-time validation: the invitation email is on by default, and it cannot
// be rendered into something a recipient can act on without a sender address
// and a link builder.
func TestModule_Register_RefusesAMailerlessBootWhileTheEmailIsOn(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
	}{
		{"no sender and no link builder", nil},
		{"a sender but no link builder", []Option{WithMailFrom(testMailFrom)}},
		{"a link builder but no sender", []Option{WithInvitationLinkBuilder(testLinkBuilder)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := append([]Option{WithEmailIndexer(newTestEmailIndexer(t))}, tc.opts...)
			_, err := pkgcore.NewKernel(pkgcore.DeploymentModeStandalone).
				Bootstrap(context.Background(), NewModule(nil, opts...))
			if !hasCode(err, ErrInvitationMailRequired.Code) {
				t.Fatalf("Bootstrap error = %v, want org.invitation_mail_required", err)
			}
		})
	}
}

// TestModule_Register_EmailDisabled_NeedsNoMailWiring pins the escape hatch a
// host uses once something else delivers the invitation: no sender address,
// no link builder, and the boot succeeds.
func TestModule_Register_EmailDisabled_NeedsNoMailWiring(t *testing.T) {
	_, err := pkgcore.NewKernel(pkgcore.DeploymentModeStandalone).
		Bootstrap(context.Background(), NewModule(nil,
			WithEmailIndexer(newTestEmailIndexer(t)),
			WithInvitationEmailDisabled(),
		))
	if err != nil {
		t.Fatalf("Bootstrap with the invitation email disabled: %v", err)
	}
}

// TestModule_Register_DeclaresTheMembershipSurface asserts the declarations
// this block adds arrive on the registry.
func TestModule_Register_DeclaresTheMembershipSurface(t *testing.T) {
	reg := bootstrapTestModule(t)

	wantActions := []string{AuditActionMemberInvite, AuditActionMemberAccept, AuditActionMemberRemove}
	actions := map[string]bool{}
	for _, action := range reg.AuditActions.Actions() {
		actions[action] = true
	}
	for _, want := range wantActions {
		if !actions[want] {
			t.Errorf("audit action %q was not registered", want)
		}
	}

	flags := map[string]pkgcore.FeatureFlag{}
	for _, flag := range reg.Features.Flags() {
		flags[flag.Key] = flag
	}
	invitations, ok := flags[FeatureInvitations]
	if !ok || !invitations.Default {
		t.Errorf("feature flag %q = %+v, want it registered and on by default", FeatureInvitations, invitations)
	}
	email, ok := flags[FeatureInvitationEmail]
	if !ok || !email.Default {
		t.Errorf("feature flag %q = %+v, want it registered and on by default", FeatureInvitationEmail, email)
	}
	if len(email.DependsOn) != 1 || email.DependsOn[0] != FeatureInvitations {
		t.Errorf("%q.DependsOn = %v, want [%q]", FeatureInvitationEmail, email.DependsOn, FeatureInvitations)
	}

	events := map[string]bool{}
	for _, decl := range reg.Events.Published() {
		events[decl.Type] = true
	}
	for _, want := range []string{EventNodeCreated, EventNodeMoved, EventNodeDeleted, EventMemberInvited, EventMemberJoined, EventMemberRemoved} {
		if !events[want] {
			t.Errorf("event %q was not declared", want)
		}
	}
}

// TestModule_Register_SubscribesToTheAuthnEvent proves the subscription is
// installed on the bus a publisher actually reaches, by publishing the event
// the way authn would and observing the effect.
func TestModule_Register_SubscribesToTheAuthnEvent(t *testing.T) {
	m := newWiredModule(t, newInvitationTestDB(t))
	reg, err := pkgcore.NewKernel(pkgcore.DeploymentModeStandalone).
		Bootstrap(context.Background(), m)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	if err := reg.EventBus().Publish(context.Background(), pkgcore.Event{
		Type:     EventUserCreated,
		TenantID: "tenant-a",
		Payload:  map[string]any{"user_id": "u-1"},
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	ctx := tenantCtx("tenant-a")
	if _, err := m.Members().Get(ctx, "u-1"); err != nil {
		t.Errorf("the bootstrapped module did not react to %s: %v", EventUserCreated, err)
	}
}

// TestModule_Options pins each option's effect on the runtime it configures.
func TestModule_Options(t *testing.T) {
	indexer := newTestEmailIndexer(t)
	m := NewModule(nil,
		WithEmailIndexer(indexer),
		WithFeatureGate(fixedGate{}),
		WithMaxDepth(3),
		WithInvitationTTL(time.Hour),
		WithMailFrom(testMailFrom),
		WithInvitationLinkBuilder(testLinkBuilder),
	)
	if m.emailIndexer != indexer || m.invites.indexer != indexer {
		t.Error("WithEmailIndexer did not reach both the module and the invite service")
	}
	if m.invites.gate == nil {
		t.Error("WithFeatureGate did not reach the invite service")
	}
	if m.tree.maxDepth != 3 {
		t.Errorf("tree.maxDepth = %d, want 3", m.tree.maxDepth)
	}
	if m.invites.ttl != time.Hour {
		t.Errorf("invites.ttl = %v, want 1h", m.invites.ttl)
	}
	if m.invites.from != testMailFrom {
		t.Errorf("invites.from = %q, want %q", m.invites.from, testMailFrom)
	}

	// Nonsense values are ignored rather than stored: a tree that cannot
	// hold a child is not a tree, and an invitation that expires before it
	// is read is not an invitation.
	def := NewModule(nil, WithMaxDepth(0), WithInvitationTTL(-time.Hour))
	if def.tree.maxDepth != maxDepth {
		t.Errorf("WithMaxDepth(0) changed maxDepth to %d, want the default %d", def.tree.maxDepth, maxDepth)
	}
	if def.invites.ttl != defaultInvitationTTL {
		t.Errorf("WithInvitationTTL(-1h) changed the ttl to %v, want the default %v", def.invites.ttl, defaultInvitationTTL)
	}
}

// TestModule_Accessors pins that every runtime a host reaches for is wired,
// and that the tree's member guard is one of them -- without it a cascading
// delete could orphan a membership.
func TestModule_Accessors(t *testing.T) {
	m := NewModule(nil, WithEmailIndexer(newTestEmailIndexer(t)))
	if m.Tree() == nil || m.Members() == nil || m.Invitations() == nil || m.Scope() == nil {
		t.Fatal("NewModule left part of the runtime unwired")
	}
	if m.tree.members == nil {
		t.Error("the tree has no member guard; a cascading delete could orphan a membership")
	}
}

// fixedGate is a FeatureGate that answers the same way for every flag.
type fixedGate struct {
	enabled bool
	err     error
}

func (g fixedGate) IsEnabled(_ context.Context, _ string) (bool, error) {
	return g.enabled, g.err
}

// compile-time check that a plain struct with one stdlib-typed method
// satisfies the seam. This is the whole no-import technique: *config.Service
// satisfies FeatureGate exactly this way, and neither module imports the
// other.
var _ FeatureGate = fixedGate{}
