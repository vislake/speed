package org

import (
	"time"

	"github.com/vislake/speed/go/dbkit"
)

// tableOrgNodes is the org_nodes table name, shared by the model's
// TableName and by the migrations' own header comments.
const tableOrgNodes = "org_nodes"

// OrgNode is one node of a tenant's organization tree: the tenant root, or
// any group / region / store beneath it.
//
// # Data domain
//
// Tenant data (docs/internal/04-data-and-tenancy.md). A node is meaningless
// outside its tenant and must never be visible across one, so it implements
// dbkit.TenantScoped, is reached only through Repository (which embeds
// dbkit.Repository[OrgNode]), and its isolation is proven by
// tenancytest.AssertIsolated.
//
// It embeds dbkit.TenantModel for the tenant_id column and the promoted
// GetTenantID method, the pattern dbkit's AGENTS.md documents for a
// tenant-scoped model that does not need tenant_id inside its primary key:
// ID is an application-generated UUID, already globally unique on its own,
// so a plain tenant_id column with its own index is enough. Do NOT redeclare
// a same-named TenantID field here to add a primaryKey tag -- dbkit's
// tenant_scope.go doc comment documents exactly how that silently breaks
// GetTenantID and, with it, FindByID for the row's own legitimate owner.
//
// # Structure
//
// ParentID is the authoritative structural edge; Path is the derived query
// index. TreeService is the only thing that writes either, and it always
// writes both together -- a row whose Path does not agree with its ParentID
// chain is a corrupt row, not a supported state.
type OrgNode struct {
	// ID is an application-generated UUID (uuid.NewString), never a
	// database-generated one: the backend coding standard forbids
	// gen_random_uuid(), which SQLite has no equivalent for. Its lowercase
	// alphabet is load-bearing -- see path.go's dialect-identity proof.
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantModel promotes the tenant_id column and the GetTenantID method
	// that satisfies dbkit.TenantScoped.
	dbkit.TenantModel

	// ParentID is the id of this node's parent, or the empty string on the
	// tenant root.
	//
	// Empty rather than NULL, and the column is NOT NULL: the sibling-name
	// uniqueness index is UNIQUE(tenant_id, parent_id, name), and NULL is
	// distinct from itself in a unique index on both engines, so two roots
	// with the same name could coexist under NULL. The empty-string
	// sentinel collapses them into the single row the constraint promises.
	// go/config's configs table solved the identical problem the identical
	// way on its tenant_id column; the migrations cite it.
	ParentID string `gorm:"column:parent_id;size:36;not null;default:''"`

	// Path is the materialized path from the tenant root down to and
	// including this node, with leading and trailing separators. See
	// path.go for the grammar and for why the trailing separator matters.
	Path string `gorm:"column:path;size:1024;not null"`

	// Depth is the node's number of ancestors: rootDepth on the tenant
	// root. It is always depthOf(Path) and is stored so that ordering and
	// depth-bound checks need no string work in SQL.
	Depth int `gorm:"column:depth;not null"`

	// Name is the node's display name, unique among its siblings. It is
	// user-supplied text and is never used to build a path.
	Name string `gorm:"column:name;size:200;not null"`

	// Kind is the tenant's own business classification of the node --
	// "group", "region", "store" in the dental-SaaS reference app. org
	// deliberately does not enumerate the legal values: the whole point of
	// an arbitrary-depth tree is that the layer names are the tenant's
	// business vocabulary, not this module's.
	Kind string `gorm:"column:kind;size:64;not null;default:''"`

	// CreatedAt and UpdatedAt are populated by gorm's autoCreateTime /
	// autoUpdateTime, never written by application code and never NOW() in
	// a migration (SQLite has no NOW()).
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

// TableName names the org_nodes table.
func (OrgNode) TableName() string { return tableOrgNodes }

// IsRoot reports whether n is its tenant's root node.
func (n OrgNode) IsRoot() bool { return n.ParentID == "" }

// compile-time check that OrgNode satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = OrgNode{}
