-- See sqlite/0010_scope_mfa_factor_unique_index_to_active.sql for the full
-- design discussion; PostgreSQL's partial-index syntax is identical to
-- SQLite's here, so this file differs from the sqlite/ copy in comment
-- brevity only.
DROP INDEX idx_user_mfa_factors_user_type;

CREATE UNIQUE INDEX idx_user_mfa_factors_user_type
    ON user_mfa_factors (user_id, type)
    WHERE status = 'active';
