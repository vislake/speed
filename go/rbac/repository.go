package rbac

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// The three repositories below are this module's only data-access types.
// Each embeds dbkit.Repository[T] rather than holding a bare *gorm.DB
// (root CLAUDE.md's multi-tenant isolation rule; backend coding standard
// §3.2), so Create, FindByID, Update, Delete and List come from the
// generic base with the tenant check already fail-closed.
//
// All three tables are tenant-owned -- rbac_roles and rbac_role_permissions
// are tenant data, rbac_role_bindings is link data -- so all three run
// tenancytest.AssertIsolated, and this module runs
// tenancytest.AssertNotTenantScoped exactly zero times. That is not an
// omission: there is no identity-domain or platform-domain table in rbac
// for the reverse assertion to assert. The permission catalog, which IS
// platform-scoped, has no table at all -- it is the in-memory snapshot of
// what the modules declared (catalog.go).
//
// Each repository additionally carries the *gorm.DB it was built on, for
// the filtered reads Repository[T]'s deliberately minimal surface cannot
// express (it has List-all and nothing else). Those reads follow option 1
// of go/dbkit/AGENTS.md's "Known limitations": the query is built on the
// same *gorm.DB layer 1 already protects, still against a TenantScoped
// model, so the isolation plugin's own WHERE tenant_id = ? is appended for
// us -- no tenant filter is ever hand-written here -- and the whole call
// runs inside dbkit.WithTenantSession so isolation layer 3 (PostgreSQL
// row-level security) is engaged as well. findWithinTenant then re-checks
// every returned row's tenant in Go, the same defense-in-depth check
// Repository[T].FindByID performs on its single row.

// RoleRepository is the data-access type for rbac_roles.
type RoleRepository struct {
	*dbkit.Repository[Role]

	// db is the same connection the embedded Repository was built on, kept
	// for the filtered reads below. See the file comment above for why
	// that is the sanctioned path rather than a bypass.
	db *gorm.DB
}

// NewRoleRepository returns a RoleRepository backed by db. db is expected
// to come from dbkit.Open, already migrated with this module's own
// Migrations().
func NewRoleRepository(db *gorm.DB) *RoleRepository {
	return &RoleRepository{Repository: dbkit.NewRepository[Role](db), db: db}
}

// ByKey returns the role with the given key inside the tenant ctx carries.
//
// It returns ErrRoleNotFound both when no such role exists and when one
// exists under a different tenant -- deliberately indistinguishable, for
// the same reason dbkit.ErrRecordNotFound is: distinguishing them would
// let a caller learn that a role key exists in a tenant it cannot see.
func (r *RoleRepository) ByKey(ctx context.Context, key string) (*Role, error) {
	roles, err := findWithinTenant[Role](ctx, r.db, func(tx *gorm.DB) *gorm.DB {
		return tx.Where("key = ?", key)
	})
	if err != nil {
		return nil, err
	}
	if len(roles) == 0 {
		return nil, ErrRoleNotFound.WithParam("key", key)
	}
	// uq_rbac_roles_tenant_key makes more than one row impossible; taking
	// the first is not a silent "pick any", it is the only row there can be.
	role := roles[0]
	return &role, nil
}

// RolePermissionRepository is the data-access type for
// rbac_role_permissions.
type RolePermissionRepository struct {
	*dbkit.Repository[RolePermission]

	// db is the same connection the embedded Repository was built on.
	db *gorm.DB
}

// NewRolePermissionRepository returns a RolePermissionRepository backed by
// db (see NewRoleRepository for what db is expected to be).
func NewRolePermissionRepository(db *gorm.DB) *RolePermissionRepository {
	return &RolePermissionRepository{Repository: dbkit.NewRepository[RolePermission](db), db: db}
}

// ByRole returns every permission granted to roleID inside the tenant ctx
// carries. A role with no permissions, and a roleID belonging to another
// tenant, both yield an empty slice and no error: "grants nothing" is the
// correct, fail-closed answer to both.
func (r *RolePermissionRepository) ByRole(ctx context.Context, roleID string) ([]RolePermission, error) {
	return findWithinTenant[RolePermission](ctx, r.db, func(tx *gorm.DB) *gorm.DB {
		return tx.Where("role_id = ?", roleID)
	})
}

// ByRoles returns every permission granted to any of roleIDs inside the
// tenant ctx carries -- the evaluation path's one permission read.
//
// It exists so that flattening a subject's bindings into a grant set costs
// ONE query rather than one per role. The alternative, calling ByRole in a
// loop, is the N+1 pattern on the hottest read in the product; the IN
// clause is expressed through the same tenant-filtered builder as every
// other read here, so it buys that without giving up any isolation layer.
//
// An empty roleIDs yields an empty slice without touching the database: an
// "IN ()" predicate is a dialect-specific edge (and a syntax error on some
// of them) with no useful answer.
func (r *RolePermissionRepository) ByRoles(ctx context.Context, roleIDs []string) ([]RolePermission, error) {
	if len(roleIDs) == 0 {
		return nil, nil
	}
	return findWithinTenant[RolePermission](ctx, r.db, func(tx *gorm.DB) *gorm.DB {
		return tx.Where("role_id IN ?", roleIDs)
	})
}

// RoleBindingRepository is the data-access type for rbac_role_bindings.
type RoleBindingRepository struct {
	*dbkit.Repository[RoleBinding]

	// db is the same connection the embedded Repository was built on.
	db *gorm.DB
}

// NewRoleBindingRepository returns a RoleBindingRepository backed by db
// (see NewRoleRepository for what db is expected to be).
func NewRoleBindingRepository(db *gorm.DB) *RoleBindingRepository {
	return &RoleBindingRepository{Repository: dbkit.NewRepository[RoleBinding](db), db: db}
}

// ByUser returns every binding held by userID inside the tenant ctx
// carries. It is the hot read of the evaluation path: a subject's whole
// grant set starts here.
//
// A userID with no bindings, and a userID whose bindings all live in
// another tenant, both yield an empty slice and no error -- the same
// fail-closed "grants nothing" both cases deserve.
func (r *RoleBindingRepository) ByUser(ctx context.Context, userID string) ([]RoleBinding, error) {
	return findWithinTenant[RoleBinding](ctx, r.db, func(tx *gorm.DB) *gorm.DB {
		return tx.Where("user_id = ?", userID)
	})
}

// ByRole returns every binding that references roleID inside the tenant
// ctx carries -- the reverse question ByUser answers, which changing or
// deleting a role needs.
func (r *RoleBindingRepository) ByRole(ctx context.Context, roleID string) ([]RoleBinding, error) {
	return findWithinTenant[RoleBinding](ctx, r.db, func(tx *gorm.DB) *gorm.DB {
		return tx.Where("role_id = ?", roleID)
	})
}

// Find returns the one binding that grants userID the role roleID at
// exactly nodeID (empty nodeID meaning tenant-wide), inside the tenant ctx
// carries. It reports ErrBindingNotFound when there is none.
//
// The scope is matched EXACTLY rather than "at or above": assigning and
// revoking address one row, and treating a revoke of a tenant-wide grant as
// covering a node-scoped one would delete a grant the caller did not name.
// uq_rbac_role_bindings makes the four columns unique, so at most one row
// can match.
func (r *RoleBindingRepository) Find(ctx context.Context, userID, roleID, nodeID string) (*RoleBinding, error) {
	rows, err := findWithinTenant[RoleBinding](ctx, r.db, func(tx *gorm.DB) *gorm.DB {
		return tx.
			Where("user_id = ?", userID).
			Where("role_id = ?", roleID).
			Where("node_id = ?", nodeID)
	})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrBindingNotFound.
			WithParam("role_id", roleID).
			WithParam("node_id", nodeID)
	}
	binding := rows[0]
	return &binding, nil
}

// CreateWithPermissions inserts a role and the permission rows it grants
// in ONE tenant-scoped transaction, so a role never becomes visible
// holding a partial permission set.
//
// Atomicity matters more here than the two-statement cost suggests: a role
// that materialized with three of its five permissions would be a silently
// under-powered grant that no error told anyone about, and the retry that
// followed would hit the role key's unique index rather than completing the
// set. dbkit.WithTenantSession already opens the transaction (it must, to
// scope the PostgreSQL session GUC), so the write rides inside the same
// boundary every read here uses, with the isolation plugin's create
// callback forcing tenant_id on every row -- including the batch insert,
// which it covers row by row.
func (r *RoleRepository) CreateWithPermissions(ctx context.Context, role *Role, permissions []RolePermission) error {
	if _, err := pkgcore.MustTenantFromContext(ctx); err != nil {
		return err
	}
	if err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		if err := tx.Create(role).Error; err != nil {
			return err
		}
		if len(permissions) == 0 {
			return nil
		}
		return tx.Create(&permissions).Error
	}); err != nil {
		if errors.Is(err, pkgcore.ErrNoTenant) {
			return err
		}
		return ErrStorage.WithCause(err)
	}
	return nil
}

// ReplaceForRole makes roleID's permission rows exactly permissions,
// deleting what is no longer granted and inserting what is new, in one
// tenant-scoped transaction.
//
// It is a replace rather than a merge because the caller (built-in role
// reconciliation) knows the whole desired set, and a merge would leave a
// permission that was removed from the definition still granted -- the
// failure mode where a role quietly keeps authority someone deliberately
// took away from it.
func (r *RolePermissionRepository) ReplaceForRole(ctx context.Context, roleID string, permissions []RolePermission) error {
	if _, err := pkgcore.MustTenantFromContext(ctx); err != nil {
		return err
	}
	if err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		if err := tx.Where("role_id = ?", roleID).Delete(&RolePermission{}).Error; err != nil {
			return err
		}
		if len(permissions) == 0 {
			return nil
		}
		return tx.Create(&permissions).Error
	}); err != nil {
		if errors.Is(err, pkgcore.ErrNoTenant) {
			return err
		}
		return ErrStorage.WithCause(err)
	}
	return nil
}

// findWithinTenant runs a filtered read for one tenant-scoped model.
//
// It is a package-level generic function rather than a method because Go
// has no generic methods, and one function rather than four copies because
// every one of the filtered reads above needs the identical three-part
// protection and the identical error mapping:
//
//  1. Resolve the tenant from ctx first and fail closed before touching the
//     database, exactly as every dbkit.Repository[T] method does.
//  2. Run the query inside dbkit.WithTenantSession, so isolation layer 3
//     (the PostgreSQL app.current_tenant GUC a row-level security policy
//     reads) is engaged for this statement, and let the isolation plugin
//     append the tenant filter itself -- apply only ever adds the module's
//     own business condition, never a tenant one.
//  3. Re-verify every returned row's tenant in Go afterwards, the same
//     defense-in-depth check Repository[T].FindByID makes on its single
//     row, so isolation still holds if layer 1 were ever absent (a
//     *gorm.DB not built through dbkit.Open) rather than silently leaking.
//
// A row that fails step 3 is dropped rather than turned into an error, for
// the same reason FindByID reports a cross-tenant hit as "not found": the
// caller learns nothing about another tenant's data either way, and an
// error here would make a plugin-less connection fail loudly in a place
// that has nothing to do with the caller's request.
func findWithinTenant[T dbkit.TenantScoped](
	ctx context.Context,
	db *gorm.DB,
	apply func(tx *gorm.DB) *gorm.DB,
) ([]T, error) {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return nil, err
	}

	var rows []T
	if err := dbkit.WithTenantSession(ctx, db, func(tx *gorm.DB) error {
		return apply(tx).Find(&rows).Error
	}); err != nil {
		// A caller that classifies "no tenant" must keep seeing pkgcore's
		// own sentinel rather than a storage error wrapping it; everything
		// else is genuinely a storage failure.
		if errors.Is(err, pkgcore.ErrNoTenant) {
			return nil, err
		}
		return nil, ErrStorage.WithCause(err)
	}

	owned := make([]T, 0, len(rows))
	for _, row := range rows {
		if row.GetTenantID() != tenant {
			continue
		}
		owned = append(owned, row)
	}
	return owned, nil
}
