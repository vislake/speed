-- base_items backs migrations_test.go's "base" fixture module. It is a test
-- fixture only, used to exercise MigrationRegistry.Apply; it is never part
-- of a real deployment's schema.
--
-- Kept portable between PostgreSQL and SQLite on purpose (see the sqlite/
-- copy of this file), following the same dual-dialect constraints every
-- real module's migrations must follow: no dialect-specific types, no
-- gen_random_uuid(), no NOW(). The primary key is (tenant_id, id) with
-- tenant_id leftmost, per the multi-tenant isolation standard.
CREATE TABLE base_items (
    id        VARCHAR(26) NOT NULL,
    tenant_id VARCHAR(26) NOT NULL,
    label     VARCHAR(255) NOT NULL,
    PRIMARY KEY (tenant_id, id)
);
