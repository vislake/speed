-- Realigns metering_outbox_records' claim index with the claim query's
-- anti-starvation ordering (go/metering/repository.go's
-- claimPendingOutboxRecords): the query now orders pending rows by
-- attempts ASC, created_at ASC -- never-failed rows first, so a pile of
-- permanently failing rows at the head of the queue can never occupy
-- batch after batch ahead of a healthy row enqueued behind them -- and
-- the index it scans must carry the same two leading columns or the
-- ORDER BY would need a sort of everything the status filter matches.
--
-- 0002's idx_metering_outbox_records_status_created ordered by
-- (status, created_at) only: correct while the query was oldest-first,
-- dead weight once attempts became the primary ordering key. Dropped and
-- recreated here rather than edited in place, since an applied migration
-- is a permanent record -- the same follow-up-migration shape
-- go/org's 0004/0005 files use.
--
-- attempts is monotonically non-decreasing per row (markOutboxAttemptFailed
-- increments it on every failed delivery attempt, forever -- billing-grade
-- delivery retries indefinitely, it does not dead-letter), so the new
-- index's second column does churn on every recorded failure; that is the
-- price of the ordering it backs, on a platform table whose volume is
-- bounded by delivery throughput rather than retained history.
--
-- This is the SQLite copy; the postgres/ sibling carries the full
-- rationale. The two are schema-identical.
DROP INDEX idx_metering_outbox_records_status_created;

CREATE INDEX idx_metering_outbox_records_status_attempts_created
    ON metering_outbox_records (status, attempts, created_at);
