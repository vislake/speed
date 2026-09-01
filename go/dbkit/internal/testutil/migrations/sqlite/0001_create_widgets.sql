-- Widget is dbkit's minimal tenant-scoped fixture table. It backs the
-- Widget GORM model in internal/testutil and exists only to exercise
-- dbkit's tenant-isolation plugin, Repository[T], and migration
-- aggregation in tests; it is never part of a real deployment's schema.
--
-- Kept portable between PostgreSQL and SQLite on purpose (see the
-- postgres/ copy of this file): no dialect-specific types, no
-- gen_random_uuid() (IDs are ULIDs generated in the application), no
-- NOW() (timestamps use GORM's autoCreateTime/autoUpdateTime when a
-- model needs them; Widget does not).
--
-- The primary key is (tenant_id, id) with tenant_id leftmost, per the
-- multi-tenant isolation standard every tenant-scoped table follows.
CREATE TABLE widgets (
    id        VARCHAR(26) NOT NULL,
    tenant_id VARCHAR(26) NOT NULL,
    name      VARCHAR(255) NOT NULL,
    value     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, id)
);
