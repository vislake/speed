package rbac

import (
	"time"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/dbkit"
)

// Role is one named bundle of permissions inside a single tenant.
//
// Data domain: TENANT DATA (docs/internal/04-data-and-tenancy.md). Roles
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
	// below and dbkit's own AGENTS.md on that choice).
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

	// Builtin marks the roles this module seeds itself. It exists so a
	// later admin surface can refuse to delete or rename them; nothing in
	// the evaluation path reads it, because a built-in role grants exactly
	// the way a tenant-defined one does.
	Builtin bool `gorm:"column:builtin;not null"`

	// DescriptionKey is an i18n message id, never localized text: the
	// backend never stores or returns user-facing prose (root CLAUDE.md's
	// internationalization rule; backend coding standard §12). It is empty
	// for tenant-defined roles, whose display name is the tenant's own
	// business.
	DescriptionKey string `gorm:"column:description_key;size:100;not null"`

	// CreatedAt is populated by gorm's autoCreateTime -- never by
	// application code, and never by a NOW() default in the migration,
	// which SQLite has no equivalent for (backend coding standard §5).
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
// here for the same reason root CLAUDE.md bans cross-module foreign keys --
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
	// needs a design decision, not an implementation guess -- see
	// AGENTS.md's deferrals).
	Permission string `gorm:"column:permission;size:100;not null"`

	// CreatedAt is populated by gorm's autoCreateTime (see Role.CreatedAt).
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
}

// TableName pins RolePermission to rbac_role_permissions.
func (RolePermission) TableName() string { return "rbac_role_permissions" }

// RoleBinding grants one role to one user, either across the whole tenant
// or over one subtree of the tenant's organization tree.
//
// Data domain: LINK DATA (docs/internal/04-data-and-tenancy.md's fourth
// domain, the one the memberships table is the archetype of): a binding
// joins a user, a tenant and a role. Link data IS tenant-scoped, so
// RoleBinding is dbkit.TenantScoped and RoleBindingRepository runs
// tenancytest.AssertIsolated exactly like the two tenant-data tables above.
//
// UserID references users.id in authn and NodeID references a node of the
// organization tree in org. Both are stored as bare ids: no foreign key, no
// import, no struct relation (root CLAUDE.md, "Do not import another
// business module's structs for database relations -- use ID references
// plus domain events"). It is what keeps rbac free of the authn dependency
// its whole design forbids.
//
// NodeID deliberately stores the node's ID, never its materialized path.
// A denormalized path column would be stale the moment the node moves, and
// docs/internal/16-verification.md requires the opposite: a member moving
// in the tree must see permissions follow immediately. The path is
// therefore resolved at evaluation time through the host-supplied
// SubtreeResolver (scope.go), and a binding whose node cannot be resolved
// denies rather than widening to the whole tenant.
//
// An empty NodeID means "the tenant root": the binding applies tenant-wide
// and needs no resolver at all, which is what lets a host with no
// organization module run this module unchanged. The column is NOT NULL
// with an empty-string sentinel rather than NULL, because NULLs are
// distinct in a PostgreSQL unique index -- two identical tenant-wide
// bindings for one user and role could coexist under NULL, while ”
// collapses them into the single row uq_rbac_role_bindings_tenant_user_role_node
// promises.
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
	// makes dbkit.Repository[RoleBinding].Delete -- promoted unchanged and
	// called by Service.RevokeRole -- a mark-delete instead of a physical
	// DELETE, and what makes dbkit.Repository[RoleBinding].Restore -- and
	// Service.RestoreRole, which wraps it -- meaningful for this model.
	// Neither field is ever set by application code directly: both writes
	// go through dbkit's own reflection-based field access, exactly as
	// TenantID does.
	//
	// RoleBinding is the only one of this module's three models that
	// adopted dbkit.SoftDeletable: it is the only one with a real
	// delete-shaped operation to retrofit (RevokeRole's
	// bindings.Delete(ctx, binding.ID) call) -- rbac.Role has no delete
	// path at all today, so there is nothing on Role or RolePermission for
	// mark-delete to change. See go/rbac/AGENTS.md's "Soft deletion"
	// section for the full round.
	//
	// uq_rbac_role_bindings_tenant_user_role_node became a partial index
	// scoped WHERE deleted_at IS NULL in the same migration that adds these
	// two columns (migrations/{postgres,sqlite}/0002_add_soft_delete.sql),
	// so a revoked binding's (tenant, user, role, node) tuple frees up
	// immediately for a fresh AssignRole, instead of staying reserved by a
	// row nobody can see.
	DeletedAt *time.Time `gorm:"column:deleted_at"`
	DeletedBy string     `gorm:"column:deleted_by;not null;default:''"`
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
// is a pure marker used only for the capability check that routes
// dbkit.Repository[RoleBinding].Delete onto the mark-delete path; the
// actual field writes go through reflection on fixed field names.
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
// IDs are generated in the APPLICATION, never by the database: gen_random_uuid()
// and its relatives are PostgreSQL-only, and this module's tables must
// migrate and behave identically on SQLite (root CLAUDE.md's dual-dialect
// rule). A single helper rather than a scattered uuid.NewString() keeps
// that decision in one place, next to the size:36 column definitions that
// depend on it.
func newID() string { return uuid.NewString() }
