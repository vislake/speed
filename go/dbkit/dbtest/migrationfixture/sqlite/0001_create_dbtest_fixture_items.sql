-- dbtest_fixture_items backs dbtest's own tests of the migration-applying
-- constructors and Migrate. It is a test fixture only, used to exercise
-- those helpers; it is never part of a real deployment's schema.
--
-- Kept portable between PostgreSQL and SQLite on purpose (see the postgres/
-- copy of this file), following the same dual-dialect constraints every
-- real module's migrations must follow: no dialect-specific types, no
-- gen_random_uuid(), no NOW(). The primary key is (tenant_id, id) with
-- tenant_id leftmost, per the multi-tenant isolation standard.
--
-- CREATE TABLE IF NOT EXISTS, rather than a plain CREATE TABLE, is
-- deliberate: migrate_test.go applies this same tree under two module names
-- in one Migrate call -- to pin that every set lands in the one shared
-- schema_migrations ledger -- and the second set's files must still
-- succeed.
CREATE TABLE IF NOT EXISTS dbtest_fixture_items (
    id        VARCHAR(26) NOT NULL,
    tenant_id VARCHAR(26) NOT NULL,
    PRIMARY KEY (tenant_id, id)
);
