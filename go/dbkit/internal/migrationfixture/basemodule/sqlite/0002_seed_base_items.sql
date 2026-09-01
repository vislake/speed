-- Only succeeds if 0001_create_base_items.sql already ran in this same
-- module. Its purpose is to make MigrationRegistry.Apply's within-module
-- filename ordering observable in a test: if this file were ever applied
-- before 0001 (or in a different transaction where 0001 had not yet run),
-- this INSERT fails with "no such table: base_items", which is exactly the
-- ordering bug this fixture exists to catch.
INSERT INTO base_items (id, tenant_id, label) VALUES ('seed-0002', 'tenant-seed', 'from-0002');
