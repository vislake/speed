-- objects is the storage module's metadata table (go/storage/model.go): one
-- row per stored object, carrying the uploader's declared intent (size,
-- type, checksum), the pipeline's finalized reality (size, mime, digest,
-- dimensions) and the lifecycle state. The bytes themselves live in the
-- host's ObjectStore under the internal key this row names -- see
-- go/storage/key.go for the grammar and for why the key is never exposed
-- through an API.
--
-- This is the PostgreSQL copy of the migration; the SQLite sibling carries
-- the identical shape with INTEGER standing in for BIGINT (SQLite has no
-- BIGINT type). The dialect differences stop at the allowed SQL surface: no
-- dialect-specific types, no native arrays, no JSONB, no gen_random_uuid(),
-- no NOW(). id is an application-generated UUID, and created_at /
-- updated_at are written by gorm's autoCreateTime and autoUpdateTime,
-- never by a database default.
--
-- The timestamp columns are plain TIMESTAMP on both dialects: this
-- codebase's migrations deliberately use one timestamp type everywhere
-- (org's own 0001 does), and gorm serializes time.Time identically on both
-- engines, so no precision or zone semantics are lost by the choice.
--
-- The index that serves a keyset-cursor listing must sit on the table the
-- listing scans: object pages read objects, so idx_objects_tenant_created
-- is declared here rather than next to object_derivatives' unique index in
-- 0002. object_derivatives carries no cursor-order index of its own:
-- nothing lists derivatives this round, and the (tenant_id, object_id)
-- prefix of uq_object_derivatives_object_kind already serves the delete
-- cascade's per-object scan.
CREATE TABLE objects (
    id                VARCHAR(36)  NOT NULL,
    tenant_id         VARCHAR(64)  NOT NULL,
    key               VARCHAR(512) NOT NULL,
    state             VARCHAR(16)  NOT NULL,
    declared_size     BIGINT       NOT NULL,
    declared_type     VARCHAR(255) NOT NULL DEFAULT '',
    declared_checksum VARCHAR(64)  NOT NULL,
    size              BIGINT       NULL,
    mime              VARCHAR(255) NULL,
    checksum_sha256   VARCHAR(64)  NULL,
    width             INTEGER      NULL,
    height            INTEGER      NULL,
    upload_expires_at TIMESTAMP    NOT NULL,
    expires_at        TIMESTAMP    NULL,
    created_at        TIMESTAMP    NOT NULL,
    updated_at        TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);

-- The objects listing's index: every cursor page is
-- WHERE tenant_id = ? AND ((created_at < ?) OR (created_at = ? AND id < ?))
-- ORDER BY created_at DESC, id DESC, and tenant_id is the leftmost column
-- of every composite index in this codebase.
CREATE INDEX idx_objects_tenant_created ON objects (tenant_id, created_at);
