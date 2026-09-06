-- See sqlite/0011_add_pending_mfa_factor_unique_index.sql for the full
-- design discussion; PostgreSQL's partial-index syntax is identical to
-- SQLite's here, so this file differs from the sqlite/ copy in comment
-- brevity only.
CREATE UNIQUE INDEX idx_user_mfa_factors_user_type_pending
    ON user_mfa_factors (user_id, type)
    WHERE status = 'pending';
