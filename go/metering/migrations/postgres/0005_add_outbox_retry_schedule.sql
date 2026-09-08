-- Adds metering_outbox_records.retry_after, the per-row re-claim schedule
-- that is the claim query's ordering key
-- (go/metering/repository.go's claimPendingOutboxRecords and Dispatcher's
-- "Retry is scheduled, not priority-classed" doc comment).
--
-- Why a schedule, not a failure count: ordering pending rows by attempts
-- ASC, created_at ASC -- never-failed rows as a strict class ahead of every
-- already-failed row -- starves a row that failed once under a sustained
-- enqueue rate, where every batch fills with never-failed rows: the failed
-- row is never claimed again, permanent starvation of exactly the rows
-- retry exists to reach. Attempts is a count, not a time: it can rank a
-- row but cannot say WHEN it may be claimed again, so no ordering over it
-- alone can both keep a pile of permanently failing rows from occupying
-- batch after batch and still reach a once-failed row under a flood of
-- new work. The schedule is the same scheduled-eligibility discipline
-- go/jobs' own scheduled_at gives its retrying jobs, with a fixed (not
-- curving) delay: retry_after holds the earliest moment the row may be
-- claimed again. Enqueue sets it to the
-- row's own CreatedAt (a never-failed row is claimable from birth);
-- markOutboxAttemptFailed moves it to the failure time plus the
-- dispatcher's retry delay. The claim query then takes pending rows whose
-- retry_after has arrived, oldest-scheduled first: a failed row re-enters
-- the queue at a moment in the future rather than re-joining the head, so
-- rows enqueued during its re-claim window go first (a pile of failing
-- rows cannot occupy batch after batch) while the failed row itself is
-- reached the moment its window opens, whatever the arrival rate of new
-- work (no permanent starvation).
--
-- The column is nullable while this schema step is in force: rows written
-- before this migration carry NULL, and the claim query treats them as
-- their CreatedAt (a COALESCE in its ORDER BY, an IS NULL escape in its
-- eligibility predicate); Enqueue and markOutboxAttemptFailed always
-- write a concrete value, so NULL is never a state the module's own
-- writers produce. Migration 0007 makes the column NOT NULL and removes
-- both accommodations.
--
-- The claim query no longer orders by attempts, so 0004's
-- idx_metering_outbox_records_status_attempts_created -- built to back
-- the attempts-first ordering -- is dead weight and is dropped. Its
-- replacement, idx_metering_outbox_records_status_retry_after, serves
-- the eligibility scan (status filter plus the retry_after <= now half of
-- the predicate) on the ordering's leading column; the never-failed
-- majority's rows carry retry_after == created_at, so the schedule order
-- and creation order coincide there. attempts itself stays on the row as
-- recorded failure history; it orders nothing.
--
-- This is the PostgreSQL copy; the sqlite/ sibling carries the identical
-- schema on that dialect. Kept portable on purpose: no dialect-specific
-- types, no NOW() (the eligibility cutoff is always a Go-side parameter).
ALTER TABLE metering_outbox_records ADD COLUMN retry_after TIMESTAMP;

UPDATE metering_outbox_records
   SET retry_after = created_at
 WHERE retry_after IS NULL;

DROP INDEX idx_metering_outbox_records_status_attempts_created;

CREATE INDEX idx_metering_outbox_records_status_retry_after
    ON metering_outbox_records (status, retry_after);
