package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// catchUpBatchSize bounds how many outbox rows deliverPendingForType reads
// and delivers per query, mirroring eventbus/redis's eventReaderBatch: a
// large backlog is drained in bounded chunks rather than one unbounded
// query, and the cursor advances after each row so a crash mid-batch loses
// at most the rows already delivered in the current batch to redelivery on
// the next cycle -- see EventBus's own doc comment for the at-least-once
// framing this shares with eventbus/redis's cross-process delivery, and
// advanceCursorAtLeast's own doc comment for the retry that narrows (but,
// deliberately, does not claim to eliminate) the redelivery window.
const catchUpBatchSize = 200

// cursorAdvanceMaxAttempts bounds how many times advanceCursorAtLeast
// retries a transient failure (a connection dropped mid-query, most
// concretely) before giving up and letting the caller's own catch-up cycle
// retry from the unadvanced, persisted cursor. Because every attempt goes
// through pool -- not the deliverPendingForType caller's own dedicated
// LISTEN connection, which pgxpool never lends out -- a fresh attempt
// after the first one very likely lands on a different, healthy pooled
// connection, so this retry converts the ordinary case (a single backend
// killed or a brief network blip) from "redeliver the whole row next
// cycle" into "recover within this call", without pretending to survive an
// outage that outlasts cursorAdvanceMaxAttempts attempts.
const cursorAdvanceMaxAttempts = 5

// cursorAdvanceRetryDelay is the fixed delay advanceCursorAtLeast waits
// between attempts, mirroring reconnectDelay's role for the listener
// connection: short enough that a handful of retries still resolves well
// within one listenBlock cycle, long enough that a retry is not simply
// racing the same still-dying connection.
const cursorAdvanceRetryDelay = 100 * time.Millisecond

// outboxRow is one durable record read back from pkgcore_eventbus_outbox.
type outboxRow struct {
	id        int64
	eventType string
	tenantID  string
	payload   []byte
}

// insertOutboxAndNotify writes one outbox row and issues NOTIFY for it
// inside a single transaction, so the two either both happen or neither
// does: a session not currently listening (or disconnected) simply misses
// the NOTIFY, exactly as PostgreSQL documents, but the row it would have
// announced is already durably committed for the catch-up path to find.
// It returns the row's database-assigned id.
func insertOutboxAndNotify(ctx context.Context, pool *pgxpool.Pool, eventType, tenantID string, payload []byte) (int64, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin publish transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful Commit

	var id int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO pkgcore_eventbus_outbox (event_type, tenant_id, payload, created_at)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id`,
		eventType, tenantID, payload, time.Now().UTC(),
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("insert outbox row: %w", err)
	}

	// pg_notify, not a literal "NOTIFY <channel>" statement, because the
	// channel name then travels as a bind parameter like everything else
	// here rather than being spliced into SQL text (LISTEN, below, has no
	// such parameterized form and is a fixed, non-caller-controlled
	// constant instead).
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, pgEventChannel, eventType); err != nil {
		return 0, fmt.Errorf("notify %q: %w", pgEventChannel, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit publish transaction: %w", err)
	}
	return id, nil
}

// fetchOutboxSince returns up to catchUpBatchSize rows of eventType whose id
// is greater than afterID, ordered by id, for deliverPendingForType's
// catch-up scan.
func fetchOutboxSince(ctx context.Context, pool *pgxpool.Pool, eventType string, afterID int64) ([]outboxRow, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, event_type, tenant_id, payload
		   FROM pkgcore_eventbus_outbox
		  WHERE event_type = $1 AND id > $2
		  ORDER BY id
		  LIMIT $3`,
		eventType, afterID, catchUpBatchSize,
	)
	if err != nil {
		return nil, fmt.Errorf("query outbox rows: %w", err)
	}
	defer rows.Close()

	var out []outboxRow
	for rows.Next() {
		var row outboxRow
		if err := rows.Scan(&row.id, &row.eventType, &row.tenantID, &row.payload); err != nil {
			return nil, fmt.Errorf("scan outbox row: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate outbox rows: %w", err)
	}
	return out, nil
}

// ensureCursor returns replicaID's persisted watermark for eventType,
// initializing it -- once, the first time this replica ever sees this
// event type -- to the outbox's current maximum id for that type, so a
// first-time Subscribe starts at the live end exactly like a fresh Redis
// consumer group does (eventbus/redis's own "$" semantics), rather than
// replaying every event of that type ever published. Every call after
// that first one is a plain read (readCursor) of the row this call (or
// advanceCursorAtLeast) already created.
//
// deliverPendingForType calls this inside its per-batch deliverMu
// critical section on the first batch only (and re-reads via readCursor
// on every later batch), so this function's own writes are serialized
// against Publish's in-flight bookkeeping -- but NOT against
// advanceCursorAtLeast, which runs outside deliverMu entirely (see
// deliverMu's own doc comment on EventBus for why). A concurrent
// advance of the same (replicaID, eventType) row is therefore a real
// possibility, and the upsert is safe against it: if the advance's
// INSERT won the race, ON CONFLICT DO NOTHING skips this one and the
// SELECT below reads the row the advance created; if this INSERT wins
// (or the row already existed), the SELECT reads the existing row and
// advanceCursorAtLeast's own GREATEST upsert later never moves the
// watermark backward. The one race left is a SELECT that runs before a
// concurrent advance commits, which simply reads the older watermark --
// inside at-least-once delivery, where redelivering a row the local
// path just handled is always permitted.
func ensureCursor(ctx context.Context, pool *pgxpool.Pool, replicaID, eventType string) (int64, error) {
	now := time.Now().UTC()
	// eventType is passed twice ($2 and $4), once for the inserted column
	// value and once for the WHERE filter, rather than reusing one bind
	// parameter in both roles: PostgreSQL's extended query protocol infers
	// each parameter's type from every place it appears, and a bare value
	// in a SELECT list gives it nothing to infer from while the WHERE
	// comparison ties it to event_type's VARCHAR column -- reusing the
	// same parameter number in both roles left the two inferences
	// inconsistent and PostgreSQL refused the query outright (SQLSTATE
	// 42P08, "inconsistent types deduced for parameter"). Binding it twice
	// sidesteps the ambiguity instead of fighting pgx's parameter
	// inference with explicit casts.
	if _, err := pool.Exec(ctx,
		`INSERT INTO pkgcore_eventbus_cursor (replica_id, event_type, last_delivered_id, updated_at)
		 SELECT $1, $2, COALESCE(MAX(id), 0), $3 FROM pkgcore_eventbus_outbox WHERE event_type = $4
		 ON CONFLICT (replica_id, event_type) DO NOTHING`,
		replicaID, eventType, now, eventType,
	); err != nil {
		return 0, fmt.Errorf("initialize cursor for %q: %w", eventType, err)
	}

	return readCursor(ctx, pool, replicaID, eventType)
}

// readCursor returns replicaID's persisted watermark for eventType, which
// the caller is responsible for knowing exists: ensureCursor creates the
// row on first use, and both ensureCursor and advanceCursorAtLeast
// maintain it afterwards. deliverPendingForType re-reads it via this
// helper before every batch after the first rather than trusting a cursor
// value remembered from an earlier batch -- a concurrent local Publish
// may have advanced the persisted row since (see deliverPendingForType's
// own doc comment), and fetching from a stale position would redeliver
// the rows that Publish already handled.
func readCursor(ctx context.Context, pool *pgxpool.Pool, replicaID, eventType string) (int64, error) {
	var cursor int64
	if err := pool.QueryRow(ctx,
		`SELECT last_delivered_id FROM pkgcore_eventbus_cursor WHERE replica_id = $1 AND event_type = $2`,
		replicaID, eventType,
	).Scan(&cursor); err != nil {
		return 0, fmt.Errorf("read cursor for %q: %w", eventType, err)
	}
	return cursor, nil
}

// advanceCursorAtLeast moves replicaID's watermark for eventType forward to
// id, upserting the row if ensureCursor never ran for it yet (Publish's own
// local-delivery path can reach a type before the listener goroutine's
// deliverPendingForType has ever called ensureCursor for it), and never
// moving it backward -- GREATEST makes this call commute with any other
// advance of the same row regardless of arrival order. That commutativity
// is load-bearing, not defensive: since the re-entrancy fix no handler ever
// runs under deliverMu, both of this function's callers invoke it outside
// the lock (Publish's local-delivery path advances right after its own
// handlers, on the publisher's goroutine; deliverPendingForType advances
// after each row its listener-goroutine scan delivers), so concurrent
// advances of one row genuinely interleave -- a local Publish finishing a
// delivery while the catch-up scan is mid-batch on the same type, say --
// and the GREATEST upsert is what keeps the persisted watermark at the
// highest row either side delivered, exactly as the at-least-once contract
// needs. The concurrent-creation race against ensureCursor (which runs
// inside deliverMu's critical section) is likewise safe, for the reasons
// ensureCursor's own doc comment gives.
//
// # Why this call is retried, and what that does and does not guarantee
//
// Both of this function's callers (Publish's own synchronous local
// delivery, and deliverPendingForType's catch-up loop) invoke every
// subscribed handler for a row BEFORE calling this function to persist that
// the row was delivered -- handler-then-advance, deliberately, so a crash
// between the two loses at most the durability of "this row is done", never
// the row's actual delivery. That ordering means a failure of THIS call,
// after the handlers already ran, is an at-least-once hazard rather than an
// at-most-once one: the caller's watermark stays behind the row it just
// delivered, and the next catch-up cycle (the next NOTIFY, or the next
// listenBlock timeout) redelivers that same row to every handler a second
// time -- see EventBus's own doc comment for the honest, non-"exactly once"
// framing this is part of.
//
// Retrying here does not remove that hazard, but it does shrink the window
// it can occur in: pool.Exec acquires whichever pooled connection is free,
// which after a single connection loss (one killed backend, one dropped
// TCP session) is very likely to be a different, healthy one on the very
// next attempt, so most real transient failures now recover inside this
// call instead of surfacing as a duplicate at all. Only a failure that
// outlasts cursorAdvanceMaxAttempts attempts -- a sustained outage of every
// pooled connection, not a single blip -- still reaches the caller as an
// error and lets the row redeliver.
func advanceCursorAtLeast(ctx context.Context, pool *pgxpool.Pool, replicaID, eventType string, id int64) error {
	now := time.Now().UTC()
	var err error
	for attempt := 1; attempt <= cursorAdvanceMaxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("advance cursor for %q: %w", eventType, ctx.Err())
			case <-time.After(cursorAdvanceRetryDelay):
			}
		}
		_, err = pool.Exec(ctx,
			`INSERT INTO pkgcore_eventbus_cursor (replica_id, event_type, last_delivered_id, updated_at)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (replica_id, event_type) DO UPDATE
			   SET last_delivered_id = GREATEST(pkgcore_eventbus_cursor.last_delivered_id, EXCLUDED.last_delivered_id),
			       updated_at = EXCLUDED.updated_at`,
			replicaID, eventType, id, now,
		)
		if err == nil {
			return nil
		}
	}
	return fmt.Errorf("advance cursor for %q: %w", eventType, err)
}

// PurgeOutboxBefore deletes outbox rows older than olderThan, so a
// deployment does not grow this table without bound forever -- unlike
// eventbus/redis's stream, which trims itself automatically
// (eventStreamMaxLen), this table has no implicit retention, and applying
// one is this package's explicit, host-scheduled maintenance operation
// rather than an automatic background behavior.
//
// PurgeOutboxBefore does not consult any replica's cursor before deleting:
// a replica whose reader has been offline longer than olderThan loses the
// events that aged out of the window it was disconnected across, the same
// trade eventbus/redis's own trim-window loss (documented on EventBus)
// makes for a replica that stays disconnected past eventStreamMaxLen. A
// host that cannot tolerate that trade schedules this call at an interval
// generous enough that no replica is ever down that long, exactly as it
// would size Redis's stream trim window today.
func PurgeOutboxBefore(ctx context.Context, pool *pgxpool.Pool, olderThan time.Duration) (int64, error) {
	tag, err := pool.Exec(ctx,
		`DELETE FROM pkgcore_eventbus_outbox WHERE created_at < $1`,
		time.Now().UTC().Add(-olderThan),
	)
	if err != nil {
		return 0, fmt.Errorf("purge outbox rows: %w", err)
	}
	return tag.RowsAffected(), nil
}
