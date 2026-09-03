-- configs is the config module's single table (go/config/model.go): one row
-- per configuration key per scope, keyed by (key, scope, tenant_id).
--
-- This is the SQLite copy of the migration; see the postgres/ sibling for
-- the full rationale of every column. The dialect differences stop at the
-- allowed SQL surface: no dialect-specific types, no gen_random_uuid(), no
-- NOW().
--
-- scope holds the module's Scope strings ("system" / "tenant"); tenant_id
-- holds the owning tenant on tenant-tier rows and the empty-string sentinel
-- on system-tier rows (never NULL, so the primary key stays a true unique
-- constraint); value holds the canonical string, or base64 ciphertext on
-- Sensitive items; updated_by/updated_at record the last Set.
CREATE TABLE configs (
    key        VARCHAR(100) NOT NULL,
    scope      VARCHAR(16)  NOT NULL,
    tenant_id  VARCHAR(64)  NOT NULL DEFAULT '',
    value      TEXT         NOT NULL,
    updated_by VARCHAR(100) NOT NULL DEFAULT '',
    updated_at TIMESTAMP    NOT NULL,
    PRIMARY KEY (key, scope, tenant_id)
);
