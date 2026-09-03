-- memberships is the link table binding one person to one place in one
-- tenant's organization tree (go/org/membership.go).
--
-- Data domain: LINK data, and link data is tenant-scoped
-- (docs/internal/04-data-and-tenancy.md classifies it as isolated by
-- tenant_id and makes tenancytest.AssertIsolated mandatory for it). The
-- neighbouring users table is identity data and deliberately NOT
-- tenant-scoped, because one person belongs to several tenants -- which is
-- exactly why this bridging row must be: it is the per-tenant half of that
-- relationship, and a membership visible across tenants would expose one
-- tenant's roster to another.
--
-- user_id references authn's users.id and node_id references org_nodes.id,
-- both WITHOUT a foreign key. Cross-module foreign keys are forbidden
-- (docs/internal/04, rule 4) because they make independently released
-- migrations and cascading deletes unmanageable. The node reference carries
-- no in-module foreign key either, for the dual-dialect reason
-- go/org/repository.go documents: SQLite leaves foreign keys unenforced
-- unless the connection turns them on, so a constraint present on one engine
-- and absent on the other would make the two deployment modes diverge.
-- TreeService.Delete is what keeps the reference honest: it refuses to remove
-- a node with members bound inside it.
--
-- One membership per person per tenant, enforced by uq_memberships_tenant_user.
-- node_id says where in the tree they sit; their data scope is that node's
-- whole subtree.
--
-- status holds the closed set go/org/membership.go declares
-- ("active" / "invited" / "suspended"). It is a plain VARCHAR, not an enum
-- type: PostgreSQL has those and SQLite does not.
--
-- created_at / updated_at are written by gorm's autoCreateTime and
-- autoUpdateTime, never by a database default -- SQLite has no NOW().

CREATE TABLE memberships (
    id         VARCHAR(36) NOT NULL,
    tenant_id  VARCHAR(64) NOT NULL,
    user_id    VARCHAR(64) NOT NULL,
    node_id    VARCHAR(36) NOT NULL,
    status     VARCHAR(16) NOT NULL,
    created_at TIMESTAMP   NOT NULL,
    updated_at TIMESTAMP   NOT NULL,
    PRIMARY KEY (id)
);

-- One seat per person per tenant. This is also what makes the membership
-- creation path idempotent under a redelivered domain event: the loser of a
-- concurrent create sees a duplicate-key error and re-reads the winner.
CREATE UNIQUE INDEX uq_memberships_tenant_user ON memberships (tenant_id, user_id);

-- The subtree roster query: node ids are resolved from org_nodes first and
-- passed here as an IN list, so no join is needed and the isolation plugin
-- protects both statements. tenant_id is the leftmost column of every
-- composite index in this codebase.
CREATE INDEX idx_memberships_tenant_node ON memberships (tenant_id, node_id);

-- The "is anybody still active in this tenant?" check a member removal makes
-- before it would empty the tenant.
CREATE INDEX idx_memberships_tenant_status ON memberships (tenant_id, status);
