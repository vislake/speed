-- configs is the config module's single table (go/config/model.go): one row
-- per configuration key per scope, keyed by (key, scope, tenant_id).
--
-- Kept portable between PostgreSQL and SQLite on purpose (see the sqlite/
-- copy of this file), following the same dual-dialect constraints every
-- real module's migrations must follow: no dialect-specific types, no
-- gen_random_uuid(), no NOW().
--
-- scope is one of the module's Scope strings ("system" for platform-wide
-- rows, "tenant" for per-tenant overrides); the "user" tier the design
-- reserves is not stored yet, so the column only ever holds the two values
-- the application writes today.
--
-- tenant_id holds the owning tenant on tenant-tier rows and the empty
-- string on system-tier rows. It is NOT NULL with an empty-string sentinel
-- rather than NULL because NULLs are distinct in a PostgreSQL unique
-- index: two system rows for one key could coexist under NULL, while ''
-- collapses them into the single row the primary key promises. See
-- go/config/model.go's doc comment on row.
--
-- value holds the canonical string form of the configuration value
-- (go/config/values.go) -- or base64 ciphertext on Sensitive items, sealed
-- by the host's dbkit.Cipher; whether a row is encrypted is decided by the
-- schema, never by a row marker.
--
-- updated_by records the Actor of the last Set and updated_at its moment;
-- the poller reads rows newer than its watermark, so both are written on
-- every Set, never only on insert.
CREATE TABLE configs (
    key        VARCHAR(100) NOT NULL,
    scope      VARCHAR(16)  NOT NULL,
    tenant_id  VARCHAR(64)  NOT NULL DEFAULT '',
    value      TEXT         NOT NULL,
    updated_by VARCHAR(100) NOT NULL DEFAULT '',
    updated_at TIMESTAMP    NOT NULL,
    PRIMARY KEY (key, scope, tenant_id)
);
