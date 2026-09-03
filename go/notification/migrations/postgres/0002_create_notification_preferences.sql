-- notification_preferences is one recipient's stored channel selection for
-- one notification type, inside one tenant (go/notification/preference.go).
--
-- Data domain: TENANT data (docs/internal/04-data-and-tenancy.md
-- classifies it as isolated by tenant_id and makes
-- tenancytest.AssertIsolated mandatory for it). The isolation plugin of
-- dbkit.Open scopes every read and write to the tenant in the context,
-- and this table's own unique index starts with tenant_id like every
-- tenant-owned index in this codebase.
--
-- Semantics, briefly (preference.go's doc comment carries the full story):
-- there is deliberately NO row for "the recipient has not chosen" -- absence
-- means the notification type's declared DefaultChannels apply, and the
-- defaults are never materialized into rows. A row exists only once the
-- recipient actively chose something, so at most one row per
-- (tenant, recipient_user_id, type_key) is the whole meaning of a
-- preference, and the unique index below is its only enforcement: a second
-- answer to the same question is an update of the row, never a second row.
--
-- recipient_user_id references authn's users.id WITHOUT a foreign key,
-- exactly like in_app_messages.recipient_user_id and org's
-- memberships.user_id: users is identity data, one person sits in several
-- tenants, and cross-module foreign keys are forbidden
-- (docs/internal/04, rule 4) because they make independently released
-- migrations and cascading deletes unmanageable.
--
-- channels holds the choice as a JSON array of channel names
-- ("in_app", "email", "sms" -- see go/notification/types.go) in the
-- platform's canonical vocabulary order, so the column is deterministic
-- regardless of the order the caller listed the channels in. It is a plain
-- TEXT column carrying JSON, never JSONB: nothing ever filters on a
-- channel, and the codebase rule forbids PostgreSQL-only types. It is NOT
-- NULL because "no row" and "chose nothing" are distinct values with
-- distinct meanings: no row means the type's defaults apply, while the
-- stored empty array "[]" is a deliberate opt-out, legal only on a type
-- whose declaration permits it (Unsubscribable). The application always
-- writes the column; no default exists because a database default could
-- never distinguish the two meanings.
--
-- created_at / updated_at are written by gorm's autoCreateTime and
-- autoUpdateTime, never by a database default -- SQLite has no NOW().
--
-- This file is byte-identical across sqlite/ and postgres/: nothing here
-- is dialect-specific.

CREATE TABLE notification_preferences (
    id                VARCHAR(36)  NOT NULL,
    tenant_id         VARCHAR(64)  NOT NULL,
    recipient_user_id VARCHAR(64)  NOT NULL,
    type_key          VARCHAR(128) NOT NULL,
    channels          TEXT         NOT NULL,
    created_at        TIMESTAMP    NOT NULL,
    updated_at        TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);

-- The uniqueness of a preference: one answer per (tenant, recipient, type).
-- The unique index doubles as the per-tenant, per-recipient listing index
-- (the listing query filters on the two leftmost columns), so it is the
-- table's only index -- every other access path goes through the primary
-- key.
CREATE UNIQUE INDEX uq_notification_preferences_tenant_recipient_type
    ON notification_preferences (tenant_id, recipient_user_id, type_key);
