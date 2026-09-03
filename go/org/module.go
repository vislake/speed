package org

import (
	"embed"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/org/locales"
	"github.com/vislake/speed/go/org/migrations"
)

// moduleName is org's pkgcore.Module.Name(). It is also the key
// dbkit.MigrationRegistry.Register builds its dependency graph on.
const moduleName = "org"

// The permissions org contributes to the platform's permission catalog.
// Enforcement belongs to rbac, which decides which role holds which of
// these; org only declares that they exist and what they are called.
const (
	// PermissionRead covers reading the organization tree and its members.
	PermissionRead = "org:read"
	// PermissionManage covers creating, renaming, moving and deleting nodes.
	PermissionManage = "org:manage"
	// PermissionInviteMember covers inviting a person into the tenant.
	PermissionInviteMember = "org:invite_member"
	// PermissionRemoveMember covers removing a member from the tenant.
	PermissionRemoveMember = "org:remove_member"
)

// The audit actions org contributes to the audit vocabulary. Every
// structural change to a tenant's tree is auditable, because a node move
// silently changes what its members can see.
const (
	AuditActionNodeCreate = "org.node.create"
	AuditActionNodeRename = "org.node.rename"
	AuditActionNodeMove   = "org.node.move"
	AuditActionNodeDelete = "org.node.delete"
)

// The domain events org publishes about its tree. Names follow
// pkgcore.EventDecl's <module>.<entity>.<action> convention.
//
// org.node.moved matters to more subscribers than it looks: a move changes
// every descendant's materialized path, which is the dimension rbac's
// path-prefix policies and every subtree-scoped listing are written against.
// A subscriber that caches anything keyed on a node path must invalidate on
// this event.
const (
	EventNodeCreated = "org.node.created"
	EventNodeMoved   = "org.node.moved"
	EventNodeDeleted = "org.node.deleted"
)

// nodeEventDecls is the catalog entry for each of the three tree events.
//
// They are declared here, in the module's single Register call, because
// pkgcore.Registry is where the platform's event catalog is assembled --
// observability, compliance and integration enumerate the declarations
// without subscribing to any of them. The publishing side is wired in this
// module's membership block, alongside the member events; a declaration
// without a publisher yet is a catalog entry, never a promise this module
// has already emitted one.
var nodeEventDecls = []pkgcore.EventDecl{
	{
		Type:        EventNodeCreated,
		PayloadType: "org.NodeCreated",
		Description: "A node was added to a tenant's organization tree.",
	},
	{
		Type:        EventNodeMoved,
		PayloadType: "org.NodeMoved",
		Description: "A node was re-parented, changing the materialized path of its whole subtree.",
	},
	{
		Type:        EventNodeDeleted,
		PayloadType: "org.NodeDeleted",
		Description: "A node, and any subtree beneath it, was removed from a tenant's organization tree.",
	},
}

// Module implements pkgcore.Module for go/org: a tenant's organization tree
// and, in this module's later blocks, the memberships and invitations bound
// to it.
type Module struct {
	// db is the connection org's tables live in. It is opened and migrated
	// by the host before Register is ever called; the module performs no
	// I/O of its own during registration.
	db *gorm.DB

	// tree is the tree runtime hosts reach through Tree(). Constructing it
	// opens nothing -- NewRepository only wraps db -- so it is built here
	// rather than in Register, which must not perform I/O.
	tree *TreeService
}

// NewModule returns a Module whose tables live in db. Constructing a Module
// performs no I/O: opening and migrating db is the host's responsibility,
// done once at startup before Bootstrap ever calls Register.
func NewModule(db *gorm.DB) *Module {
	return &Module{db: db, tree: NewTreeService(db)}
}

// Tree returns the module's TreeService, the only sanctioned way to change a
// tenant's organization tree.
func (m *Module) Tree() *TreeService { return m.tree }

// Name implements pkgcore.Module.
func (m *Module) Name() string { return moduleName }

// DependsOn implements pkgcore.Module: nothing.
//
// This is a real answer, not a stub. org sits above dbkit and tenancy in
// docs/internal/01-architecture.md's graph, but neither is a
// pkgcore.Module -- they are libraries the host wires, and DependsOn
// enumerates only modules in the bootstrap set. org must also NOT depend on
// authn: it learns about users from a domain event and an id, never from
// authn's Go types, which is the canonical module-boundary example the root
// CLAUDE.md gives. Naming authn here would make org unbootable in a host
// that does not run authn, and would invert the direction that example
// exists to protect.
func (m *Module) DependsOn() []string { return nil }

// Migrations implements pkgcore.Module.
func (m *Module) Migrations() embed.FS { return migrations.FS }

// Locales implements pkgcore.Module: the descriptions of org's error codes,
// in both supported languages with identical id sets.
func (m *Module) Locales() embed.FS { return locales.FS }

// OpenAPISpec implements pkgcore.Module: nil for now. org's spec fragment
// and the server interface generated from it land with this module's API
// block, spec first as the contract rule requires.
func (m *Module) OpenAPISpec() []byte { return nil }

// Register implements pkgcore.Module. Per the interface's contract it only
// declares -- no database call, no outbound call, nothing that touches m.db.
// It contributes org's permissions, its audit vocabulary and its event
// catalog entries.
//
// It deliberately does NOT declare the authn.user.created event org
// subscribes to in its membership block: that event is authn's to declare,
// and declaring it here as well would collide with authn's own registration
// the moment both modules boot in one host.
func (m *Module) Register(reg *pkgcore.Registry) error {
	if err := reg.Permissions.Add(
		PermissionRead,
		PermissionManage,
		PermissionInviteMember,
		PermissionRemoveMember,
	); err != nil {
		return err
	}
	if err := reg.AuditActions.Add(
		AuditActionNodeCreate,
		AuditActionNodeRename,
		AuditActionNodeMove,
		AuditActionNodeDelete,
	); err != nil {
		return err
	}
	return reg.Events.Publishes(nodeEventDecls...)
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
