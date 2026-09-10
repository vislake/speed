-- PostgreSQL copy of the sqlite/ sibling's 0001_create_dbtest_fixture_items.sql:
-- identical DDL, because the fixture table is deliberately portable (see
-- that file's own comment for what it backs and why IF NOT EXISTS matters
-- here). Kept as a separate file because a real module's migration set
-- carries one file per dialect, which is the layout these tests exercise.
CREATE TABLE IF NOT EXISTS dbtest_fixture_items (
    id        VARCHAR(26) NOT NULL,
    tenant_id VARCHAR(26) NOT NULL,
    PRIMARY KEY (tenant_id, id)
);
