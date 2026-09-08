-- Realigns metering_outbox_records' claim index with the claim query's
-- anti-starvation ordering of pending rows by attempts ASC, created_at
-- ASC (go/metering/repository.go's claimPendingOutboxRecords):
-- never-failed rows first, so a pile of permanently failing rows at the
-- head of the queue can never occupy batch after batch ahead of a
-- healthy row enqueued behind them -- and the index must carry the same
-- two leading columns or the ORDER BY would need a full sort of
-- everything the status filter matches. (Migration 0005 later replaces
-- the attempts ordering with the retry_after schedule, and its index
-- supersedes this one.)
--
-- 0002's idx_metering_outbox_records_status_created ordered by
-- (status, created_at) only -- right for the query's original
-- oldest-first ordering, dead weight once attempts became the primary
-- ordering key. The index is dropped and recreated here rather than
-- edited in place, because an applied migration is a permanent record;
-- a schema correction ships as a follow-up migration.
--
-- attempts is monotonically non-decreasing per row (markOutboxAttemptFailed
-- increments it on every failed delivery attempt, forever -- billing-grade
-- delivery retries indefinitely, it does not dead-letter), so the new
-- index's second column does churn on every recorded failure; that is the
-- price of the ordering it backs, on a platform table whose volume is
-- bounded by delivery throughput rather than retained history.
--
-- This is the PostgreSQL copy; the sqlite/ sibling carries the identical
-- schema on that dialect.
DROP INDEX idx_metering_outbox_records_status_created;

CREATE INDEX idx_metering_outbox_records_status_attempts_created
    ON metering_outbox_records (status, attempts, created_at);
