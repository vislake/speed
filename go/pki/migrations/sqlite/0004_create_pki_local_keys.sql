-- pki_local_keys is the LocalSigner implementation's own private-key store
-- (go/pki/model.go). Only LocalSigner ever reads or writes this table.
-- Platform data -- no tenant_id column; isolation proven by
-- tenancytest.AssertNotTenantScoped.
--
-- This is the SQLite copy; see the postgres/ sibling for the full rationale
-- of every column, including why not_after exists unpopulated this round.
CREATE TABLE pki_local_keys (
    key_ref                VARCHAR(64) NOT NULL,
    algorithm              VARCHAR(32) NOT NULL,
    encrypted_private_key  BLOB        NOT NULL,
    not_after              TIMESTAMP,
    created_at             TIMESTAMP   NOT NULL,
    updated_at             TIMESTAMP   NOT NULL,
    PRIMARY KEY (key_ref)
);

-- Round 2's expiry-scan index -- schema now, scan later, per this file's
-- own doc comment above.
CREATE INDEX idx_pki_local_keys_not_after ON pki_local_keys (not_after);
