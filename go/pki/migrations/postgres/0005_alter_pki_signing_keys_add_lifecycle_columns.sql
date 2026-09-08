-- The signing-key lifecycle state machine needs two further columns on
-- pki_signing_keys:
--
--   * retiring_at: when a key was demoted from 'active' to 'retiring', the
--     reference point the retiring->retired transition is measured from.
--   * retiring_overlap: how long that retiring period lasts, copied onto
--     the row at creation time from the caller's own
--     Service.EnsurePurpose(..., maxCredentialLifetime) argument -- pki does
--     not know this number itself, so it is recorded rather than computed
--     (go/pki/model.go's SigningKey.RetiringOverlap doc comment has the
--     full argument for why it lives on the row rather than a separate
--     purpose-policy table).
--
-- This is the PostgreSQL copy; see the sqlite/ sibling for the identical
-- schema on that dialect.
ALTER TABLE pki_signing_keys ADD COLUMN retiring_at TIMESTAMP;
ALTER TABLE pki_signing_keys ADD COLUMN retiring_overlap BIGINT NOT NULL DEFAULT 0;
