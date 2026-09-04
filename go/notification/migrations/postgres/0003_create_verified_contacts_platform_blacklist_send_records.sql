-- This migration creates the three tables of the consent-ledger and
-- delivery round: verified_contacts, platform_blacklist and send_records
-- (go/notification/{contact.go,blacklist.go}; the send_records model
-- arrives with the delivery job in the same round).
--
-- The three tables span two data domains
-- (docs/internal/04-data-and-tenancy.md's data-domain table):
--
--   verified_contacts   TENANT data: one tenant's consent ledger for
--                       external addresses. dbkit's isolation plugin
--                       scopes every access to the tenant in the context,
--                       and every index starts with tenant_id.
--   platform_blacklist  PLATFORM data: an address that must not be
--                       messaged, whatever tenant would send to it. No
--                       GetTenantID on the model, no tenant filter on any
--                       query; tenant_id is a real column recording the
--                       tenant whose send produced the record, defaulting
--                       to the empty-string sentinel for platform-level
--                       rows and never enforced (the audit-table
--                       convention). The UNIQUE (channel, address_index)
--                       index is the platform-wide dedupe.
--   send_records        PLATFORM data: the outbound-delivery log. Every
--                       column follows the same sentinel convention as
--                       platform_blacklist. An idempotency key rides on
--                       every record; uniqueness is scoped
--                       (tenant_id, idempotency_key).
--
-- verified_contacts.address holds CIPHERTEXT, never an address: the column
-- is written through the GORM serializer the host registers under
-- ContactAddressSerializerName (notification_address_enc), so the bytes on
-- disk are AES-256-GCM output and a database backup leaks no addresses.
-- The column type is the dialect's binary type -- this is the one place
-- the file differs from its sqlite/ sibling (BYTEA here, BLOB there);
-- every other line is byte-identical.
-- every other line is byte-identical. address_index is the HMAC-SHA256
-- blind index of the canonical address form (dbkit.NewBlindIndexer over
-- dbkit.NormalizeEmail / NormalizePhoneE164), the only form of the address
-- that is ever queryable, deduplicated on or rate-limited on. The
-- encryption key and the index keys are separate and never the same bytes
-- (AGENTS.md's "Separate index keys from the cipher key" adjudication).
--
-- verified_contacts' status column carries the consent state machine
-- (pending / verified / unsubscribed / bounced -- contact.go's
-- ContactStatus constants): a double_opt_in contact is created pending
-- without a code, the code is stamped on it before the synchronous send,
-- VerifyCode moves it to verified, Unsubscribe and the hard-failure leg
-- move it to the two terminal statuses. code_hash is therefore
-- DEFAULT '' (the empty string, the "no live code" value a freshly
-- created pending row holds until its first stamp) rather than NULL:
-- gorm's struct-based writes skip zero-valued fields, so a column with no
-- default would fail the create. consent_ref is likewise DEFAULT '' for
-- double_opt_in rows and carries the attesting business record's reference
-- on business_attested rows. consent_at, verified_at and code_expires_at
-- are true NULLs -- points in time that do not exist yet, which no
-- zero-value convention can express.
--
-- created_at / updated_at are written by gorm's autoCreateTime and
-- autoUpdateTime, never by a database default -- SQLite has no NOW().

CREATE TABLE verified_contacts (
    id               VARCHAR(36)  NOT NULL,
    tenant_id        VARCHAR(64)  NOT NULL,
    channel          VARCHAR(16)  NOT NULL,
    address          BYTEA        NOT NULL,
    address_index    VARCHAR(64)  NOT NULL,
    status           VARCHAR(16)  NOT NULL,
    consent_by       VARCHAR(32)  NOT NULL,
    consent_ref      VARCHAR(128) NOT NULL DEFAULT '',
    consent_at       TIMESTAMP,
    verified_at      TIMESTAMP,
    code_hash        VARCHAR(64)  NOT NULL DEFAULT '',
    code_expires_at  TIMESTAMP,
    created_at       TIMESTAMP    NOT NULL,
    updated_at       TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);

-- One consent record per (tenant, channel, address): re-registering the
-- same address on the same channel must resolve to the existing row, never
-- duplicate it. The unique index doubles as the dedupe probe's lookup
-- index (CreateContact's ByChannelAndAddressIndex filters on these three
-- columns), so it is the table's only index besides the primary key.
CREATE UNIQUE INDEX uq_verified_contacts_tenant_channel_address
    ON verified_contacts (tenant_id, channel, address_index);

CREATE TABLE platform_blacklist (
    id             VARCHAR(36)  NOT NULL,
    tenant_id      VARCHAR(64)  NOT NULL DEFAULT '',
    channel        VARCHAR(16)  NOT NULL,
    address_index  VARCHAR(64)  NOT NULL,
    reason         VARCHAR(16)  NOT NULL,
    created_at     TIMESTAMP    NOT NULL,
    updated_at     TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);

-- One blacklist record per (channel, address) platform-wide: a second
-- complaint or bounce adds nothing a first record did not say. The
-- platform-level uniqueness is the point of the table -- the same address
-- under a different tenant must still resolve to the same record.
CREATE UNIQUE INDEX uq_platform_blacklist_channel_address
    ON platform_blacklist (channel, address_index);

CREATE TABLE send_records (
    id                  VARCHAR(36)  NOT NULL,
    tenant_id           VARCHAR(64)  NOT NULL DEFAULT '',
    type_key            VARCHAR(128) NOT NULL,
    channel             VARCHAR(16)  NOT NULL,
    recipient_class     VARCHAR(16)  NOT NULL,
    recipient_user_id   VARCHAR(64)  NOT NULL DEFAULT '',
    contact_id          VARCHAR(36)  NOT NULL DEFAULT '',
    status              VARCHAR(16)  NOT NULL DEFAULT '',
    duration_ms         INTEGER      NOT NULL DEFAULT 0,
    error               VARCHAR(4000) NOT NULL DEFAULT '',
    provider_receipt_id VARCHAR(128) NOT NULL DEFAULT '',
    idempotency_key     VARCHAR(128) NOT NULL,
    created_at          TIMESTAMP    NOT NULL,
    updated_at          TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);

-- The index is the record-level half of the delivery job's best-effort
-- at-most-once convergence, scoped per tenant: the idempotency key is
-- derived from the business event plus the recipient and channel, so
-- re-enqueues and racing attempts of the same delivery never write a
-- second record (what the index governs is the record set, never the
-- transport sends themselves).
CREATE UNIQUE INDEX uq_send_records_tenant_idempotency
    ON send_records (tenant_id, idempotency_key);
