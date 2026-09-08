-- This migration creates contact_type_unsubscribes, the consent ledger's
-- type-scoped opt-out table (go/notification/contact_type_unsubscribe.go):
-- the finer shape AGENTS.md's "Unsubscribe is permanent for the contact as
-- a whole" adjudication records as deliberately deferred until this round.
-- A whole-contact unsubscribe stays a status on the verified_contacts row
-- itself; this table carries the narrower facts -- "this verified contact
-- receives nothing of THIS notification type" -- one row per
-- (tenant, contact, type), terminal for as long as the contact row lives,
-- exactly as the whole-contact unsubscribe is terminal.
--
-- The table is TENANT data (docs/internal/04-data-and-tenancy.md's
-- data-domain table), like the verified_contacts rows it narrows:
-- dbkit's isolation plugin scopes every access to the tenant in the
-- context, and the unique index starts with tenant_id. contact_id names
-- the verified_contacts row the opt-out belongs to by the codebase's
-- no-foreign-key rule -- a plain, unenforced string column, the identical
-- treatment notification_preferences.recipient_user_id gives a reference
-- it cannot constrain.
--
-- type_key follows the <module>.<entity>.<action> convention of the
-- notification-type taxonomy (types.go): the opt-out's validity against
-- the live registrar is judged by ContactService.UnsubscribeType at write
-- time, never denormalized here. A row is written only for a VERIFIED
-- contact (the write path's status gate) and only for a type whose
-- declaration permits opting out (Unsubscribable true), so no row here can
-- ever be inert dead data under the ledger's own rules.
--
-- created_at / updated_at are written by gorm's autoCreateTime and
-- autoUpdateTime, never by a database default -- SQLite has no NOW().

CREATE TABLE contact_type_unsubscribes (
    id           VARCHAR(36)  NOT NULL,
    tenant_id    VARCHAR(64)  NOT NULL,
    contact_id   VARCHAR(36)  NOT NULL,
    type_key     VARCHAR(128) NOT NULL,
    created_at   TIMESTAMP    NOT NULL,
    updated_at   TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);

-- One opt-out per (tenant, contact, type): re-narrowing the same type is
-- an idempotent repeat, never a second row -- the same "the row is the
-- answer to one question" reasoning notification_preferences' own unique
-- index documents. The index doubles as the delivery gate's probe index
-- (ContactTypeUnsubscribeRepository.ByContactAndType filters on these
-- three columns), so it is the table's only index besides the primary key.
CREATE UNIQUE INDEX uq_contact_type_unsubscribes_tenant_contact_type
    ON contact_type_unsubscribes (tenant_id, contact_id, type_key);
