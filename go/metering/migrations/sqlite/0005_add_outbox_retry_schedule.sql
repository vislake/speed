-- Adds metering_outbox_records.retry_after, the per-row re-claim schedule
-- that replaced Attempts as the claim query's ordering key
-- (go/metering/repository.go's claimPendingOutboxRecords and Dispatcher's
-- "Retry is scheduled, not priority-classed" doc comment).
--
-- The reviewer finding this migration closes (P1-metering-10): the claim
-- ordered pending rows by attempts ASC, created_at ASC -- never-failed rows
-- as a strict class ahead of every already-failed row -- so under a
-- sustained enqueue rate, where every batch filled with never-failed rows,
-- a row that had failed ONCE was never claimed again: permanent starvation
-- of exactly the rows retry exists to reach. Attempts is a count, not a
-- time: it could rank a row but could never say WHEN it may be claimed
-- again, so no ordering over it alone could both keep a pile of
-- permanently failing rows from occupying batch after batch (the property
-- 0004's ordering bought) and still reach a once-failed row under a flood
-- of new work (the property 0004's ordering sacrificed). The fix is the
-- same scheduled-eligibility discipline go/jobs' own scheduled_at gives
-- its retrying jobs -- the model this module's own doc comments already
-- cited -- with a fixed (not curving) delay this round: retry_after holds
-- the earliest moment the row may be claimed again. Enqueue sets it to the
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
-- The column is nullable: rows written before this migration carry NULL,
-- which the claim query treats as the row's CreatedAt (COALESCE in its
-- ORDER BY, an IS NULL escape in its eligibility predicate); Enqueue and
-- markOutboxAttemptFailed always write a concrete value, so NULL is a
-- legacy-only state, never a state the module itself produces.
--
-- The claim no longer orders by attempts, so 0004's
-- idx_metering_outbox_records_status_attempts_created -- recreated there
-- specifically to back the attempts-first ordering -- is dead weight and is
-- dropped, exactly as 0004 itself dropped 0002's created_at-first index
-- when the ordering changed the other way. Its replacement,
-- idx_metering_outbox_records_status_retry_after, serves the eligibility
-- scan (status filter plus the retry_after <= now half of the predicate)
-- on the ordering's leading column; the never-failed majority's rows carry
-- retry_after == created_at, so the schedule order and creation order
-- coincide there. attempts itself stays: it still records failure history
-- on the row, it just no longer orders claims.
--
-- This is the SQLite copy; the postgres/ sibling carries the full
-- rationale. The two are schema-identical.
ALTER TABLE metering_outbox_records ADD COLUMN retry_after TIMESTAMP;

UPDATE metering_outbox_records
   SET retry_after = created_at
 WHERE retry_after IS NULL;

DROP INDEX idx_metering_outbox_records_status_attempts_created;

CREATE INDEX idx_metering_outbox_records_status_retry_after
    ON metering_outbox_records (status, retry_after);
