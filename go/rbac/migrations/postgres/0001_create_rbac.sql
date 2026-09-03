-- rbac's three tables (go/rbac/model.go): roles, the permissions each role
-- grants, and the bindings that give a user a role over the whole tenant or
-- over one subtree of the organization tree.
--
-- Kept portable between PostgreSQL and SQLite on purpose (see the sqlite/
-- copy of this file, which must stay byte-comparable in substance),
-- following the dual-dialect constraints every module's migrations follow:
-- no dialect-specific types, no gen_random_uuid(), no NOW(), no native
-- arrays, no JSONB.
--
-- Every id column is VARCHAR(36) and holds an application-generated UUID.
-- tenant_id is the leftmost column of every index here, per the backend
-- coding standard's data-model rules; the ids themselves are globally
-- unique, so each table's primary key is its id alone and tenant_id is a
-- plain indexed column rather than part of a composite key.
--
-- There are deliberately no foreign keys, not even between these three
-- tables of one module: cross-table constraints make independently released
-- migrations and cascading deletes unmanageable, and the rows a constraint
-- would protect are always written and removed together by the service that
-- owns all three. user_id and node_id additionally reference data owned by
-- OTHER modules (authn's users, org's tree nodes), where a foreign key is
-- forbidden outright.

-- rbac_roles: one named permission bundle per tenant. key is unique within
-- a tenant and meaningless across tenants -- two tenants each having their
-- own "admin" role is the normal case, which is why the unique index is on
-- (tenant_id, key) and never on key alone. builtin marks the roles this
-- module seeds; description_key is an i18n message id, never localized
-- text, because the backend stores structured codes and lets the client
-- resolve the prose.
CREATE TABLE rbac_roles (
    id              VARCHAR(36)  NOT NULL,
    tenant_id       VARCHAR(64)  NOT NULL,
    key             VARCHAR(64)  NOT NULL,
    builtin         BOOLEAN      NOT NULL DEFAULT FALSE,
    description_key VARCHAR(100) NOT NULL DEFAULT '',
    created_at      TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);

CREATE UNIQUE INDEX uq_rbac_roles_tenant_key ON rbac_roles (tenant_id, key);

-- rbac_role_permissions: the resource:action strings a role grants. The
-- unique index makes a duplicate grant impossible at the storage layer,
-- so the service's own catalog validation is not the only thing standing
-- between a double-click and two identical rows. Its (tenant_id, role_id)
-- prefix is also the lookup path "which permissions does this role
-- grant", so no second index is needed for that read.
CREATE TABLE rbac_role_permissions (
    id         VARCHAR(36)  NOT NULL,
    tenant_id  VARCHAR(64)  NOT NULL,
    role_id    VARCHAR(36)  NOT NULL,
    permission VARCHAR(100) NOT NULL,
    created_at TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);

CREATE UNIQUE INDEX uq_rbac_role_permissions_tenant_role_permission
    ON rbac_role_permissions (tenant_id, role_id, permission);

-- rbac_role_bindings: user x tenant x role, optionally narrowed to one
-- organization subtree.
--
-- node_id is NOT NULL with an empty-string sentinel for "the tenant root",
-- never NULL: NULLs are distinct in a PostgreSQL unique index, so two
-- identical tenant-wide bindings for one user and role could coexist under
-- NULL, while '' collapses them into the single row the unique index
-- promises.
--
-- It stores the node's id and never its materialized path. A denormalized
-- path would go stale the moment the node moves in the tree, and
-- docs/internal/16-verification.md requires permissions to follow such a
-- move immediately; the path is resolved at evaluation time instead.
--
-- The (tenant_id, user_id) prefix of the unique index serves the hot read
-- "which bindings does this subject have". The second index serves the
-- reverse question, "which bindings reference this role", which role
-- deletion and permission changes need.
CREATE TABLE rbac_role_bindings (
    id         VARCHAR(36) NOT NULL,
    tenant_id  VARCHAR(64) NOT NULL,
    user_id    VARCHAR(64) NOT NULL,
    role_id    VARCHAR(36) NOT NULL,
    node_id    VARCHAR(64) NOT NULL DEFAULT '',
    created_at TIMESTAMP   NOT NULL,
    PRIMARY KEY (id)
);

CREATE UNIQUE INDEX uq_rbac_role_bindings_tenant_user_role_node
    ON rbac_role_bindings (tenant_id, user_id, role_id, node_id);

CREATE INDEX idx_rbac_role_bindings_tenant_role
    ON rbac_role_bindings (tenant_id, role_id);
