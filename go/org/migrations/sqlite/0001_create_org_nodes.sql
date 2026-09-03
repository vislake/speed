-- org_nodes is the org module's organization tree (go/org/model.go): one row
-- per node, with the tenant root carrying an empty parent_id.
--
-- This is the SQLite copy of the migration; see the postgres/ sibling for
-- the full rationale of every column. The dialect differences stop at the
-- allowed SQL surface: no dialect-specific types, no native arrays, no
-- JSONB, no gen_random_uuid(), no NOW(). Nothing here is a recursive CTE
-- either: subtree queries are indexed LIKE prefix scans over the path
-- column, so no engine has to be argued about (go/org/path.go).
--
-- id is an application-generated UUID, drawn from the lowercase-hex-and-
-- hyphen alphabet go/org/path.go pins. That alphabet is load-bearing, not
-- cosmetic: SQLite's LIKE is ASCII-case-insensitive by default while
-- PostgreSQL's is case-sensitive, and an all-lowercase id space is what
-- makes both engines select the identical rows for one prefix scan.
--
-- parent_id is NOT NULL with an empty-string sentinel on the tenant root,
-- never NULL. NULL is distinct from itself in a unique index on both
-- engines, so two roots with the same name could coexist under NULL while
-- '' collapses them into the single row uq_org_nodes_sibling_name promises.
-- go/config/migrations/postgres/0001_create_configs.sql solved the identical
-- problem the identical way on its tenant_id column.
--
-- path holds the materialized path, "/root-id/child-id/" with leading AND
-- trailing separators; depth is its number of ancestors, stored so depth
-- bounds and ordering need no string work in SQL. VARCHAR(1024) leaves ample
-- headroom: the application bounds depth at 8, i.e. 9 levels of 36-character
-- UUIDs, 334 characters. The bound is enforced in Go because SQLite does not
-- enforce VARCHAR(n) under type affinity.
--
-- created_at / updated_at are written by gorm's autoCreateTime and
-- autoUpdateTime, never by a database default.
CREATE TABLE org_nodes (
    id         VARCHAR(36)   NOT NULL,
    tenant_id  VARCHAR(64)   NOT NULL,
    parent_id  VARCHAR(36)   NOT NULL DEFAULT '',
    path       VARCHAR(1024) NOT NULL,
    depth      INTEGER       NOT NULL,
    name       VARCHAR(200)  NOT NULL,
    kind       VARCHAR(64)   NOT NULL DEFAULT '',
    created_at TIMESTAMP     NOT NULL,
    updated_at TIMESTAMP     NOT NULL,
    PRIMARY KEY (id)
);

-- The subtree scan's index: every "self and descendants" query is
-- WHERE tenant_id = ? AND path LIKE '<prefix>%', and tenant_id is the
-- leftmost column of every composite index in this codebase.
CREATE INDEX idx_org_nodes_tenant_path ON org_nodes (tenant_id, path);

-- The children scan's index, used by the direct-children query and by the
-- has-children check a non-cascading delete makes.
CREATE INDEX idx_org_nodes_tenant_parent ON org_nodes (tenant_id, parent_id);

-- Sibling names are unique within one parent. This is the backstop behind
-- the application's own pre-check: a lost race between two concurrent
-- creates is rejected here, and org maps the resulting duplicate-key error
-- back to org.duplicate_sibling_name.
CREATE UNIQUE INDEX uq_org_nodes_sibling_name ON org_nodes (tenant_id, parent_id, name);
