-- in_app_messages is one tenant's in-app notification inbox: one row per
-- message delivered to one recipient (go/notification/model.go).
--
-- Data domain: TENANT data (docs/internal/04-data-and-tenancy.md
-- classifies it as isolated by tenant_id and makes
-- tenancytest.AssertIsolated mandatory for it). The isolation plugin of
-- dbkit.Open scopes every read and write to the tenant in the context,
-- and this table's own composite index starts with tenant_id like every
-- tenant-owned index in this codebase.
--
-- recipient_user_id references authn's users.id WITHOUT a foreign key,
-- exactly like org's memberships.user_id: users is identity data and one
-- person sits in several tenants, so the per-tenant half of that
-- relationship lives here -- one row set per (tenant, person). The row is
-- isolated by tenant_id alone, which is correct for an inbox: a person
-- who belongs to several tenants has one inbox in each, and the tenant
-- scoping keeps tenant A's messages invisible to tenant B. Cross-module
-- foreign keys are forbidden (docs/internal/04, rule 4) because they
-- make independently released migrations and cascading deletes
-- unmanageable.
--
-- The column is named "group" after the inbox grouping it serves, even
-- though GROUP is a reserved word on PostgreSQL: it is quoted in the DDL
-- here, gorm quotes every identifier it generates itself, and any
-- hand-written SQL in later blocks must quote it too ("group"). Do not
-- rename it to dodge the quoting -- the name carries meaning for the
-- inbox grouping feature, and SQLite accepts the quoted form as well.
--
-- title and body are what the recipient sees, rendered by the producer
-- before the row is written; the row is a snapshot of the delivery, not a
-- template reference. link is the deep link the message points at, and
-- params holds the template parameters that title/body were rendered
-- from, for the UI to re-open the details view. params is a plain TEXT
-- column carrying JSON -- datatypes.JSON on the Go side, never JSONB:
-- the codebase rule forbids PostgreSQL-only types, and nothing ever
-- filters on parameter contents, so a JSONB column would buy query power
-- nothing uses.
--
-- dedupe_key is the idempotency key of the delivery path: a producer
-- that redelivers (a retried job, a replayed event) recomputes the same
-- key and the unique index turns the second insert into a duplicate-key
-- error the subscriber re-reads as "already delivered". It is globally
-- unique, not per tenant, because the derivation (shipped with the
-- producer of a later block) folds the tenant in: a key can then never
-- collide across tenants, and a future derivation bug that forgets the
-- tenant fails loudly with a false duplicate instead of silently
-- deduplicating two tenants' messages. Rows without a key are never
-- deduplicated: dedupe_key is nullable, and NULLs are distinct under a
-- unique index on both SQLite and PostgreSQL.
--
-- expiry_at is when the message stops being shown in the inbox and read_at
-- is when the recipient opened it; both are written by the application,
-- never by a database default, and both stay NULL until then.
--
-- created_at / updated_at are written by gorm's autoCreateTime and
-- autoUpdateTime, never by a database default -- SQLite has no NOW().
--
-- This file is byte-identical across sqlite/ and postgres/: nothing here
-- is dialect-specific.

CREATE TABLE in_app_messages (
    id                VARCHAR(36)  NOT NULL,
    tenant_id         VARCHAR(64)  NOT NULL,
    recipient_user_id VARCHAR(64)  NOT NULL,
    type_key          VARCHAR(128) NOT NULL,
    "group"           VARCHAR(64)  NOT NULL DEFAULT '',
    title             VARCHAR(255) NOT NULL,
    body              VARCHAR(4000) NOT NULL,
    params            TEXT,
    link              VARCHAR(2000) NOT NULL DEFAULT '',
    dedupe_key        VARCHAR(128),
    expiry_at         TIMESTAMP,
    read_at           TIMESTAMP,
    created_at        TIMESTAMP    NOT NULL,
    updated_at        TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);

-- The delivery path's idempotency guard: at most one row per dedupe key,
-- tenant included in the derivation as the header above explains. NULL
-- keys stay unlimited because NULLs are distinct on both engines.
CREATE UNIQUE INDEX uq_in_app_messages_dedupe_key ON in_app_messages (dedupe_key);

-- The one per-recipient inbox query (unread first, oldest at the end of a
-- page): tenant_id is the leftmost column of every composite index in
-- this codebase, and the index doubles as the tenant's own index for
-- scans that walk the whole inbox.
CREATE INDEX idx_in_app_messages_tenant_recipient_created
    ON in_app_messages (tenant_id, recipient_user_id, created_at);
