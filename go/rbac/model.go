package rbac

import (
	"time"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/dbkit"
)

// Role is one named bundle of permissions inside a single tenant.
//
// Data domain: TENANT DATA. Roles
// are owned by the tenant that defined them -- including the built-in
// owner/admin/member roles, which are seeded per tenant rather than shared
// from a platform-wide template, so that no read this module ever issues
// crosses a tenant boundary. Accordingly Role is dbkit.TenantScoped and is
// reached only through RoleRepository, which embeds dbkit.Repository[Role];
// its isolation is proven by tenancytest.AssertIsolated in
// repository_test.go.
//
// The "system" pseudo-tenant that carries platform-operations grants
// (SystemDomain, see subject.go) is an ordinary tenant id as far as this
// model, the repository, the isolation plugin and row-level security are
// all concerned: its roles are rows with tenant_id = 'system', evaluated by
// the identical code path. There is deliberately no special case anywhere.
type Role struct {
	// ID is an application-generated UUID. It is globally unique on its
	// own, which is why this table's primary key is the id alone and
	// tenant_id is a plain indexed column (see the TenantModel embedding
	// below).
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantModel promotes the tenant_id column and the GetTenantID method
	// that satisfies dbkit.TenantScoped. Never redeclare a same-named
	// TenantID field on this struct to shadow the promoted one: dbkit's
	// tenant_scope.go documents exactly how that silently breaks
	// GetTenantID, and with it every tenant's own FindByID -- not only an
	// attacker's.
	dbkit.TenantModel

	// Key is the role's stable identifier within its tenant ("owner",
	// "admin", "member", or a tenant-defined one). It is unique per tenant
	// (uq_rbac_roles_tenant_key), never globally: two tenants each having
	// their own "admin" is the normal case.
	Key string `gorm:"column:key;size:64;not null"`

	// Builtin marks the roles this module seeds itself. No shipped surface
	// can delete or rename a role, so nothing reads it yet; a built-in role
	// grants exactly the way a tenant-defined one does.
	Builtin bool `gorm:"column:builtin;not null"`

	// DescriptionKey is an i18n message id, never localized text: the
	// backend never stores or returns user-facing prose. It is empty
	// for tenant-defined roles, whose display name is the tenant's own
	// business.
	DescriptionKey string `gorm:"column:description_key;size:100;not null"`

	// CreatedAt is populated by gorm's autoCreateTime -- never by
	// application code, and never by a NOW() default in the migration,
	// which SQLite has no equivalent for.
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
}

// TableName pins Role to rbac_roles, so the table name does not depend on
// gorm's pluralization of the Go type name.
func (Role) TableName() string { return "rbac_roles" }

// RolePermission is one resource:action string granted to one role.
//
// Data domain: TENANT DATA, for the same reason Role is -- a grant belongs
// to the tenant whose role carries it. Reached only through
// RolePermissionRepository; isolation proven by tenancytest.AssertIsolated.
//
// RoleID references Role.ID and is stored as a plain id column with no
// foreign key. That is not laziness: cross-table constraints are avoided
// here for the same reason cross-module foreign keys are banned --
// independently released migrations and cascading deletes become
// unmanageable -- and within this module the rows are always written and
// removed together by the service that owns both, which is the invariant a
// constraint would otherwise be buying.
//
// The permission string itself is validated against the frozen permission
// catalog (catalog.go) at grant time, so a row can only ever name a
// permission some module actually declared.
type RolePermission struct {
	// ID is an application-generated UUID (see Role.ID).
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantModel promotes tenant_id and GetTenantID (see Role).
	dbkit.TenantModel

	// RoleID is the owning Role's id -- an id reference, no foreign key.
	RoleID string `gorm:"column:role_id;size:36;not null"`

	// Permission is the granted resource:action string, e.g. "notes:read".
	// Matching at evaluation time is exact: this module has no wildcard
	// grammar, deliberately (a wildcard grammar is a security surface that
	// needs a design decision, not an implementation guess).
	Permission string `gorm:"column:permission;size:100;not null"`

	// CreatedAt is populated by gorm's autoCreateTime (see Role.CreatedAt).
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
}

// TableName pins RolePermission to rbac_role_permissions.
func (RolePermission) TableName() string { return "rbac_role_permissions" }

// RoleBinding grants one role to one user, either across the whole tenant
// or over one subtree of the tenant's organization tree.
//
// Data domain: LINK DATA (the domain the memberships table is the
// archetype of): a binding
// joins a user, a tenant and a role. Link data IS tenant-scoped, so
// RoleBinding is dbkit.TenantScoped and RoleBindingRepository runs
// tenancytest.AssertIsolated exactly like the two tenant-data tables above.
//
// UserID references users.id in authn and NodeID references a node of the
// organization tree in org. Both are stored as bare ids: no foreign key, no
// import, no struct relation. It is what keeps rbac free of the authn
// dependency its whole design forbids.
//
// NodeID deliberately stores the node's ID, never its materialized path.
// A denormalized path column would be stale the moment the node moves, and
// a member moving in the tree must see its permissions follow
// immediately. The path is
// therefore resolved at evaluation time through the host-supplied
// SubtreeResolver (scope.go), and a binding whose node cannot be resolved
// denies rather than widening to the whole tenant.
//
// An empty NodeID means "the tenant root": the binding applies tenant-wide
// and needs no resolver at all, which is what lets a host with no
// organization module run this module unchanged. The column is NOT NULL
// with an empty-string sentinel rather than NULL, because NULLs are
// distinct in a PostgreSQL unique index -- two identical tenant-wide
// bindings for one user and role could coexist under NULL, while the empty
// string collapses them into the single row
// uq_rbac_role_bindings_tenant_user_role_node promises.
type RoleBinding struct {
	// ID is an application-generated UUID (see Role.ID).
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantModel promotes tenant_id and GetTenantID (see Role).
	dbkit.TenantModel

	// UserID is the granted user's id in authn -- an id reference only.
	UserID string `gorm:"column:user_id;size:64;not null"`

	// RoleID is the granted Role's id -- an id reference only.
	RoleID string `gorm:"column:role_id;size:36;not null"`

	// NodeID is the organization node this binding is scoped to, or the
	// empty string for a tenant-wide binding. See the type's doc comment
	// for why no path is stored beside it.
	NodeID string `gorm:"column:node_id;size:64;not null;default:''"`

	// CreatedAt is populated by gorm's autoCreateTime (see Role.CreatedAt).
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`

	// DeletedAt and DeletedBy are dbkit.SoftDeletable's required pair
	// (go/dbkit/soft_delete.go): implementing that interface below is what
	// flips this model's revoke write from a physical DELETE onto a
	// mark-delete UPDATE, and what gives the model a working restore. The
	// writes themselves go through this module's own origin-aware
	// RoleBindingRepository.Delete and RoleBindingRepository.Restore
	// (which shadow dbkit's promoted pair and extend the mark by the
	// revoke_origin column -- see that method's and RevokeOrigin's own
	// comments), never through dbkit's reflection-based field access and
	// never set by hand at a call site.
	//
	// RoleBinding is the only one of this module's three models that
	// implements dbkit.SoftDeletable: it is the only one with a
	// delete-shaped operation (RevokeRole's revoke write) --
	// rbac.Role and RolePermission have no delete path at all, so there is
	// nothing on them for mark-delete to change.
	//
	// uq_rbac_role_bindings_tenant_user_role_node is a partial index
	// scoped WHERE deleted_at IS NULL (migrations/{postgres,sqlite}/
	// 0002_add_soft_delete.sql adds these
	// two columns in the same migration),
	// so a revoked binding's (tenant, user, role, node) tuple frees up
	// immediately for a fresh AssignRole, instead of staying reserved by a
	// row nobody can see.
	DeletedAt *time.Time `gorm:"column:deleted_at"`
	DeletedBy string     `gorm:"column:deleted_by;not null;default:''"`

	// RevokeOrigin records WHICH writer soft-deleted this row. It is
	// meaningful only while the row is soft-deleted (deleted_at IS NOT
	// NULL): a live
	// row always carries the empty default, and every mark-delete -- and
	// every restore -- rewrites it, so no stale value ever survives onto a
	// row whose state it does not describe (migrations/{postgres,sqlite}/
	// 0003_add_revoke_origin.sql has the full value vocabulary and the
	// backfill policy).
	//
	// The three writers of revoked rows all route through the one
	// origin-aware revoke path RoleBindingRepository.Delete provides (which
	// shadows dbkit.Repository[RoleBinding].Delete -- see that method's doc
	// comment for why a mark-delete that cannot name its writer must be
	// unreachable): Service.RevokeRole writes the deliberate value (''),
	// the org.member.removed reap (reap.go's revokeReapedBindings) writes
	// 'member-removal', and the org.node.deleted reap writes
	// 'node-deletion'. The restore-side subscribers -- onMemberRestored and
	// onNodeRestored -- scope their re-instatement by the marker, so a
	// deliberate revocation is never undone by an org restore event, and a
	// member removed from a tenant while a node-reaped row of theirs still
	// sits revoked has that row re-attributed to the member-removal
	// (RoleBindingRepository.claimByUser) so a later node restore cannot
	// resurrect authorization for someone who is no longer a member.
	RevokeOrigin string `gorm:"column:revoke_origin;size:16;not null;default:''"`
}

// TableName pins RoleBinding to rbac_role_bindings.
func (RoleBinding) TableName() string { return "rbac_role_bindings" }

// IsTenantWide reports whether b applies across the whole tenant rather
// than to one subtree. It is the same question Scope.IsTenantWide asks of
// an incoming grant request, asked of a stored row.
func (b RoleBinding) IsTenantWide() bool { return b.NodeID == "" }

// GetDeletedAt returns RoleBinding's soft-delete marker, satisfying
// dbkit.SoftDeletable. Like GetTenantID, this is never called by dbkit's
// soft-delete auto-scope plugin or by Repository[RoleBinding] itself -- it
// is a pure marker used only for the capability check that routes a
// dbkit.Repository[RoleBinding] Delete onto the mark-delete path. The
// actual field writes go through this module's own origin-aware
// RoleBindingRepository.Delete/Restore methods (model.go's DeletedAt
// comment and repository.go's method comments carry the full story).
func (b RoleBinding) GetDeletedAt() *time.Time { return b.DeletedAt }

// Compile-time checks that all three models satisfy dbkit.TenantScoped, the
// constraint dbkit.Repository[T] requires.
var (
	_ dbkit.TenantScoped = Role{}
	_ dbkit.TenantScoped = RolePermission{}
	_ dbkit.TenantScoped = RoleBinding{}
)

// Compile-time check that RoleBinding satisfies dbkit.SoftDeletable -- see
// the DeletedAt/DeletedBy field comment above for why RoleBinding alone,
// among this module's three models, carries this capability.
var _ dbkit.SoftDeletable = RoleBinding{}

// newID returns the primary key a new row in this module gets.
//
// IDs are generated in the APPLICATION, never by the database:
// gen_random_uuid() and its relatives are PostgreSQL-only, and this
// module's tables must migrate and behave identically on SQLite. A single
// helper rather than a scattered uuid.NewString() keeps
// that decision in one place, next to the size:36 column definitions that
// depend on it.
func newID() string { return uuid.NewString() }
