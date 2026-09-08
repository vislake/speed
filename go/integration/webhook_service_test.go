package integration

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// alwaysAllowURL is a WithWebhookURLValidator override that accepts any URL
// -- this module's own tests must be able to create subscriptions pointing
// at an httptest.Server on loopback, which ValidateWebhookURL's real,
// production behavior always refuses (this file's own point: SSRF
// protection is tested directly against ValidateWebhookURL and isBlockedIP
// in ssrf_test.go, not re-exercised here).
func alwaysAllowURL(context.Context, string) error { return nil }

// testMapping is the fixed EventMapping every test in this file and
// webhook_delivery_test.go that needs one wires, unless it needs to prove
// something about a SPECIFIC mapping's own fields.
var testMapping = EventMapping{
	InternalType:  "test.thing.happened",
	PublicType:    "test.thing.happened",
	PublicVersion: "v1",
	Transform: func(_ context.Context, evt pkgcore.Event) (json.RawMessage, error) {
		return json.RawMessage(`{"seen":true}`), nil
	},
}

// newWebhookTestService builds a fully Attach-ed Module/Service pair over a
// fresh webhook-capable test database, with testMapping declared and SSRF
// validation bypassed (see alwaysAllowURL) so tests can freely target
// httptest servers. Extra options apply after those two defaults, so a test
// needing a real jobs.Queue or a specific http.Client passes them here.
func newWebhookTestService(t *testing.T, opts ...Option) (*Module, *Service) {
	t.Helper()
	base := []Option{
		WithEventMapping(testMapping),
		WithWebhookURLValidator(alwaysAllowURL),
	}
	m := NewModule(newWebhookTestDB(t), append(base, opts...)...)
	reg := newTestRegistry(t)
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	svc, err := m.Attach(reg)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	return m, svc
}

func TestService_CreateWebhookSubscription_ReturnsRawSecretOnce(t *testing.T) {
	_, svc := newWebhookTestService(t)

	created, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		URL:        "https://example.com/hook",
		EventTypes: []string{"test.thing.happened"},
		CreatedBy:  "user-1",
	})
	if err != nil {
		t.Fatalf("CreateWebhookSubscription: %v", err)
	}
	if created.Secret == "" {
		t.Error("Secret is empty, want the raw signing secret")
	}
	if created.ID == "" {
		t.Error("ID is empty")
	}

	list, err := svc.ListWebhookSubscriptions(ctxFor(testTenant))
	if err != nil {
		t.Fatalf("ListWebhookSubscriptions: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len(list) = %d, want 1", len(list))
	}
	// The list surface never re-exposes the secret -- WebhookSubscriptionSummary
	// has no Secret field at all, which is a compile-time guarantee this test
	// documents rather than needs to assert at runtime.
	if list[0].ID != created.ID || list[0].URL != created.URL {
		t.Errorf("list[0] = %+v, want it to match the created subscription", list[0])
	}
}

func TestService_CreateWebhookSubscription_EmptyCreatedBy_Refused(t *testing.T) {
	_, svc := newWebhookTestService(t)
	_, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		URL: "https://example.com/hook", EventTypes: []string{"test.thing.happened"},
	})
	if !apperrIs(err, ErrCreatedByRequired) {
		t.Errorf("error = %v, want ErrCreatedByRequired", err)
	}
}

func TestService_CreateWebhookSubscription_EmptyURL_Refused(t *testing.T) {
	_, svc := newWebhookTestService(t)
	_, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		CreatedBy: "user-1", EventTypes: []string{"test.thing.happened"},
	})
	if !apperrIs(err, ErrWebhookURLRequired) {
		t.Errorf("error = %v, want ErrWebhookURLRequired", err)
	}
}

func TestService_CreateWebhookSubscription_EmptyEventTypes_Refused(t *testing.T) {
	_, svc := newWebhookTestService(t)
	_, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		CreatedBy: "user-1", URL: "https://example.com/hook",
	})
	if !apperrIs(err, ErrEventTypesRequired) {
		t.Errorf("error = %v, want ErrEventTypesRequired", err)
	}
}

func TestService_CreateWebhookSubscription_UnknownEventType_Refused(t *testing.T) {
	_, svc := newWebhookTestService(t)
	_, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		CreatedBy: "user-1", URL: "https://example.com/hook", EventTypes: []string{"no.such.type"},
	})
	if !apperrIs(err, ErrWebhookEventTypeUnknown) {
		t.Errorf("error = %v, want ErrWebhookEventTypeUnknown", err)
	}
}

// TestService_CreateWebhookSubscription_BlockedURL_Refused proves the
// creation-time SSRF gate is actually wired into the service path (not
// only unit-tested in isolation against ValidateWebhookURL) -- this test
// uses the REAL validator, not alwaysAllowURL.
func TestService_CreateWebhookSubscription_BlockedURL_Refused(t *testing.T) {
	_, svc := newWebhookTestService(t, WithWebhookURLValidator(nil)) // nil restores the production ValidateWebhookURL
	_, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		CreatedBy: "user-1", URL: "http://127.0.0.1/hook", EventTypes: []string{"test.thing.happened"},
	})
	if !apperrIs(err, ErrWebhookURLBlocked) {
		t.Errorf("error = %v, want ErrWebhookURLBlocked", err)
	}
}

func TestService_UpdateWebhookSubscription_ChangesURLEventTypesAndActive(t *testing.T) {
	_, svc := newWebhookTestService(t)
	created, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		URL: "https://example.com/hook", EventTypes: []string{"test.thing.happened"}, CreatedBy: "user-1",
	})
	if err != nil {
		t.Fatalf("CreateWebhookSubscription: %v", err)
	}

	newURL := "https://example.com/hook-v2"
	inactive := false
	summary, err := svc.UpdateWebhookSubscription(ctxFor(testTenant), UpdateWebhookSubscriptionInput{
		ID:     created.ID,
		URL:    &newURL,
		Active: &inactive,
	})
	if err != nil {
		t.Fatalf("UpdateWebhookSubscription: %v", err)
	}
	if summary.URL != newURL {
		t.Errorf("URL = %q, want %q", summary.URL, newURL)
	}
	if summary.Active {
		t.Error("Active = true, want false")
	}
	if len(summary.EventTypes) != 1 || summary.EventTypes[0] != "test.thing.happened" {
		t.Errorf("EventTypes = %v, want unchanged [test.thing.happened]", summary.EventTypes)
	}
}

func TestService_UpdateWebhookSubscription_EmptyEventTypesSlice_Refused(t *testing.T) {
	_, svc := newWebhookTestService(t)
	created, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		URL: "https://example.com/hook", EventTypes: []string{"test.thing.happened"}, CreatedBy: "user-1",
	})
	if err != nil {
		t.Fatalf("CreateWebhookSubscription: %v", err)
	}
	_, err = svc.UpdateWebhookSubscription(ctxFor(testTenant), UpdateWebhookSubscriptionInput{
		ID: created.ID, EventTypes: []string{},
	})
	if !apperrIs(err, ErrEventTypesRequired) {
		t.Errorf("error = %v, want ErrEventTypesRequired", err)
	}
}

// TestService_UpdateWebhookSubscription_ConcurrentDelete_DeletionWins is the
// regression test for the resurrection race between
// UpdateWebhookSubscription and DeleteWebhookSubscription: an update whose
// FindByID read landed while the subscription was still live can find its
// own write landing after a concurrent delete's mark-delete committed, and
// that write must not resurrect the subscription. The two calls are raced
// against fresh subscriptions in a loop; whichever way each race resolves,
// the invariant is the same -- after both calls settle, the subscription is
// no longer readable as a live row. The update side writes through
// updateFields' targeted, deleted_at IS NULL guarded UPDATE: a whole-row
// save of the stale read, whose nil DeletedAt would overwrite the
// committed mark-delete and resurrect the row (Active value included), is
// exactly the interleaving this test refuses.
func TestService_UpdateWebhookSubscription_ConcurrentDelete_DeletionWins(t *testing.T) {
	_, svc := newWebhookTestService(t)

	for i := 0; i < 40; i++ {
		created, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
			URL: "https://example.com/hook", EventTypes: []string{"test.thing.happened"}, CreatedBy: "user-1",
		})
		if err != nil {
			t.Fatalf("iteration %d CreateWebhookSubscription: %v", i, err)
		}

		active := true
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = svc.UpdateWebhookSubscription(ctxFor(testTenant), UpdateWebhookSubscriptionInput{ID: created.ID, Active: &active})
		}()
		go func() {
			defer wg.Done()
			// A delete that loses the race (already-revoked-style no-op is
			// impossible here -- nothing deletes twice) may transiently hit
			// SQLite's writer lock; the update's write holds it only
			// momentarily, so a bounded retry settles the delete either way.
			for attempt := 0; ; attempt++ {
				if err := svc.DeleteWebhookSubscription(ctxFor(testTenant), created.ID); err == nil {
					return
				} else if attempt == 50 {
					t.Errorf("iteration %d DeleteWebhookSubscription never succeeded: %v", i, err)
					return
				}
			}
		}()
		wg.Wait()

		if _, err := svc.webhookRepo.FindByID(ctxFor(testTenant), created.ID); !apperrIs(err, dbkit.ErrRecordNotFound) {
			t.Fatalf("iteration %d: the subscription is readable as live after a concurrent update+delete (err = %v) -- the delete did not win", i, err)
		}
	}
}

// TestService_RestoreWebhookSubscription_ConcurrentDelete_DeletionWins is
// the regression test for the resurrection race in
// RestoreWebhookSubscription's own tail. The restore's un-mark and its
// forced pause land in ONE guarded write (restorePaused,
// webhook_repository.go): a restore that first un-marked the row and then
// saved it back through a whole-row write -- a save whose snapshot carries
// the fresh, post-restore row but whose unconditional scope writes every
// column of it, nil DeletedAt included -- would let a
// DeleteWebhookSubscription that committed its mark-delete between the
// read and that save find its deletion silently undone: the stale save
// resurrects the subscription the tenant had just deleted again, Active
// value rewritten along with it.
//
// The two calls are raced against fresh, delete-then-restore subscriptions
// in a loop; whichever way each race resolves, the invariant is the same
// -- once the delete has reported success, the subscription is no longer
// readable as a live row. The restore's flip only ever lands on a row
// that is still live at the moment of the write, so no interleaving can
// resurrect the row.
//
// One honesty note on this race's reachability: the restore's read-to-write
// window is far narrower than the update path's (the delete is always one
// statement behind the restore -- it can only find the row live after the
// restore's own un-mark has committed, while the restore's tail read
// follows that same commit immediately), so a real-concurrency run
// measures zero collisions even against a resurrection-shaped
// implementation. The deleted-between-read-and-save interleaving is
// therefore pinned separately, deterministically: in
// webhook_repository_test.go's direct repository calls
// (TestWebhookSubscriptionRepository_updateFields_PartialAndLiveOnly-style)
// and in this test's sibling
// TestWebhookSubscriptionRepository_RestoreTail_RestoreReadThenDeleteThenSave.
// This test's own role is the invariant under real scheduling pressure: no
// interleaving of the two service calls may resurrect, ever.
func TestService_RestoreWebhookSubscription_ConcurrentDelete_DeletionWins(t *testing.T) {
	_, svc := newWebhookTestService(t)

	for i := 0; i < 100; i++ {
		created, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
			URL: "https://example.com/hook", EventTypes: []string{"test.thing.happened"}, CreatedBy: "user-1",
		})
		if err != nil {
			t.Fatalf("iteration %d CreateWebhookSubscription: %v", i, err)
		}
		// The Restore race needs a currently mark-deleted row to un-mark.
		if err := svc.DeleteWebhookSubscription(ctxFor(testTenant), created.ID); err != nil {
			t.Fatalf("iteration %d DeleteWebhookSubscription (setup): %v", i, err)
		}

		var (
			wg              sync.WaitGroup
			start           = make(chan struct{})
			restoreErr      error
			deleteSucceeded bool
			deleteErr       error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			restoreErr = svc.RestoreWebhookSubscription(ctxFor(testTenant), created.ID)
		}()
		go func() {
			defer wg.Done()
			<-start
			// The delete can only run once the restore has un-marked the
			// row (its own FindByID refuses a deleted one), so it retries
			// -- mirroring TestService_UpdateWebhookSubscription_
			// ConcurrentDelete_DeletionWins's identical loop, including its
			// bounded tolerance of SQLite's writer lock.
			for attempt := 0; ; attempt++ {
				if err := svc.DeleteWebhookSubscription(ctxFor(testTenant), created.ID); err == nil {
					deleteSucceeded = true
					return
				} else if attempt == 200 {
					deleteErr = err
					return
				}
			}
		}()
		close(start)
		wg.Wait()

		if deleteErr != nil {
			t.Fatalf("iteration %d DeleteWebhookSubscription never succeeded: %v", i, deleteErr)
		}
		if !deleteSucceeded {
			t.Fatalf("iteration %d: the delete reported no success", i)
		}
		if restoreErr != nil && !apperrIs(restoreErr, ErrWebhookSubscriptionNotFound) {
			t.Fatalf("iteration %d RestoreWebhookSubscription: %v", i, restoreErr)
		}

		if _, err := svc.webhookRepo.FindByID(ctxFor(testTenant), created.ID); !apperrIs(err, dbkit.ErrRecordNotFound) {
			t.Fatalf("iteration %d: the subscription is readable as live after the restore's write raced a delete that reported success (err = %v) -- the restore resurrected the deleted row", i, err)
		}
	}
}

// pauseShapedWebhookUpdate reports whether stmt is an UPDATE whose SET
// clause names the Active column but NOT the DeletedAt unmark -- the
// statement shape a two-write restore would use for its pause (dbkit's
// Repository[WebhookSubscription].Restore unmark carries DeletedAt and no
// Active; a following pause write would carry Active and no DeletedAt).
// The two restore regression tests below use it to arm a gorm update-chain
// callback that fires deterministically at such a statement and only at
// it: against a two-write restore the callback would observe (or fail) the
// pause write precisely between the unmark's commit and the pause's,
// while the actual one-write restore -- one UPDATE carrying Active and
// DeletedAt together -- has no such statement at all, so the callback
// never fires.
func pauseShapedWebhookUpdate(stmt *gorm.Statement) bool {
	hasActive, hasDeletedAt := false, false
	for _, col := range stmt.Selects {
		switch col {
		case "Active":
			hasActive = true
		case "DeletedAt":
			hasDeletedAt = true
		}
	}
	return hasActive && !hasDeletedAt
}

// TestService_RestoreWebhookSubscription_NoDeliveryCanFireBetweenRestoreAndPause
// is the regression test for the restore-then-pause window.
// RestoreWebhookSubscription lands its restore and its forced pause in ONE
// write (restorePaused); a two-write shape -- dbkit's unmark
// (Repository[WebhookSubscription].Restore), then an updateFields call
// setting Active = false -- would leave the subscription LIVE with Active
// still true between the unmark's commit and the pause's commit, the exact
// row handleDomainEvent's fan-out matches (webhook_delivery.go's
// matchingSubscriptions, over ListActiveByTenant), so a matching domain
// event observed in that window would silently resume POSTing the
// tenant's live event data to an external, third-party URL nobody had
// looked at since the deletion, the very outcome the forced pause exists
// to prevent.
//
// The window is probed deterministically -- no sleeps, no scheduling luck,
// no goroutines: a gorm update-chain callback armed for the duration of
// the restore call fires immediately before any pause-shaped UPDATE (see
// pauseShapedWebhookUpdate). At that instant, against a two-write restore,
// the unmark would already have committed (it is an earlier, separate
// statement) and the pause would not, so the callback runs exactly the
// fan-out a matching domain event observed between the two writes would
// trigger. Against the actual one-write restore no pause-shaped statement
// ever exists, and the callback never fires: no instant of the call has a
// live-and-active row to fan out to.
func TestService_RestoreWebhookSubscription_NoDeliveryCanFireBetweenRestoreAndPause(t *testing.T) {
	fq := &fakeQueue{}
	m, svc := newWebhookTestService(t, WithWebhookQueue(fq))
	created, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		URL: "https://example.com/hook", EventTypes: []string{"test.thing.happened"}, CreatedBy: "user-1",
	})
	if err != nil {
		t.Fatalf("CreateWebhookSubscription: %v", err)
	}
	if !created.Active {
		t.Fatal("a freshly created subscription must start Active, this test's premise")
	}
	if delErr := svc.DeleteWebhookSubscription(ctxFor(testTenant), created.ID); delErr != nil {
		t.Fatalf("DeleteWebhookSubscription: %v", delErr)
	}

	probe := struct {
		fired    bool
		enqueued int
	}{}
	const hookName = "integration-test:restore-pause-window"
	if regErr := m.db.Callback().Update().Before("gorm:update").
		Register(hookName, func(db *gorm.DB) {
			if !pauseShapedWebhookUpdate(db.Statement) {
				return
			}
			// The would-be interleave point of a two-write restore: the
			// unmark committed, the pause has not. Run the real fan-out a
			// matching domain event observed at this instant would run.
			probe.fired = true
			_ = svc.handleDomainEvent(ctxFor(testTenant),
				pkgcore.Event{Type: testMapping.InternalType, TenantID: testTenant})
			probe.enqueued = len(fq.tasks)
		}); regErr != nil {
		t.Fatalf("registering the pause-window probe callback: %v", regErr)
	}
	defer m.db.Callback().Update().Remove(hookName)

	if restoreErr := svc.RestoreWebhookSubscription(ctxFor(testTenant), created.ID); restoreErr != nil {
		t.Fatalf("RestoreWebhookSubscription: %v", restoreErr)
	}

	if probe.fired {
		t.Fatalf("the restore performed a separate pause-shaped write after the unmark, and the fan-out run at that instant enqueued %d delivery task(s) -- a subscription must never exist in the live-and-active state between a restore and its forced pause; the two must be one atomic write", probe.enqueued)
	}
	if len(fq.tasks) != 0 {
		t.Fatalf("len(fq.tasks) = %d, want 0", len(fq.tasks))
	}
	list, err := svc.ListWebhookSubscriptions(ctxFor(testTenant))
	if err != nil {
		t.Fatalf("ListWebhookSubscriptions: %v", err)
	}
	if len(list) != 1 || list[0].Active {
		t.Fatalf("restored subscription = %+v, want exactly one, paused", list)
	}
}

// TestService_RestoreWebhookSubscription_PauseWriteFailure_LeavesNoRestoredActiveSubscription
// is the regression test for the failure leg of the one-write shape: a
// two-write restore whose separate pause write FAILED would report
// ErrInternal while the unmark had already committed -- leaving the
// subscription permanently restored-ACTIVE, delivering again to a URL
// nobody had looked at since the deletion, while every caller saw only an
// error (and a retry would answer the collapsed not-found, since the row
// was no longer mark-deleted for a second Restore to match). The single
// restorePaused write has no such leg: its failure leaves the row still
// mark-deleted, never restored at all.
//
// The failure is injected deterministically through the same gorm
// update-chain callback: armed for the duration of the restore call, it
// fails any pause-shaped UPDATE (see pauseShapedWebhookUpdate) before that
// statement executes. Against a two-write restore that is exactly the
// pause write, and the test observes the injected failure's aftermath:
// the unmark committed, the pause did not, and the row is live with
// Active true --
// matched by ListActiveByTenant, the fan-out's own query. Against the
// actual one-write restore no pause-shaped statement exists (the restore
// is one UPDATE carrying Active and DeletedAt together), the injection
// cannot fire, and the single write either lands the row paused or leaves
// it mark-deleted: either way, never restored-active.
func TestService_RestoreWebhookSubscription_PauseWriteFailure_LeavesNoRestoredActiveSubscription(t *testing.T) {
	m, svc := newWebhookTestService(t)
	created, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		URL: "https://example.com/hook", EventTypes: []string{"test.thing.happened"}, CreatedBy: "user-1",
	})
	if err != nil {
		t.Fatalf("CreateWebhookSubscription: %v", err)
	}
	if delErr := svc.DeleteWebhookSubscription(ctxFor(testTenant), created.ID); delErr != nil {
		t.Fatalf("DeleteWebhookSubscription: %v", delErr)
	}

	injected := errors.New("injected: the pause write failed")
	const hookName = "integration-test:restore-pause-failure"
	if regErr := m.db.Callback().Update().Before("gorm:update").
		Register(hookName, func(db *gorm.DB) {
			if pauseShapedWebhookUpdate(db.Statement) {
				db.AddError(injected)
			}
		}); regErr != nil {
		t.Fatalf("registering the pause-write failure callback: %v", regErr)
	}
	defer m.db.Callback().Update().Remove(hookName)

	restoreErr := svc.RestoreWebhookSubscription(ctxFor(testTenant), created.ID)

	// The invariant, however the restore call itself answered: the
	// subscription must not be deliverable. The injected failure must not
	// leave it restored-ACTIVE -- ListActiveByTenant, the exact query the
	// fan-out runs, matches it -- with restoreErr reporting only
	// ErrInternal.
	active, err := svc.webhookRepo.ListActiveByTenant(ctxFor(testTenant))
	if err != nil {
		t.Fatalf("ListActiveByTenant: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("a RestoreWebhookSubscription whose write path failed (restore error: %v) left a live, ACTIVE subscription behind: %+v -- a failed restore must never leave the row restored-active, whether the restore failed outright or its pause did", restoreErr, active[0])
	}
}

func TestService_UpdateWebhookSubscription_NotFound_Refused(t *testing.T) {
	_, svc := newWebhookTestService(t)
	_, err := svc.UpdateWebhookSubscription(ctxFor(testTenant), UpdateWebhookSubscriptionInput{ID: "no-such-id"})
	if !apperrIs(err, ErrWebhookSubscriptionNotFound) {
		t.Errorf("error = %v, want ErrWebhookSubscriptionNotFound", err)
	}
}

func TestService_UpdateWebhookSubscription_CrossTenant_NotFound(t *testing.T) {
	_, svc := newWebhookTestService(t)
	created, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		URL: "https://example.com/hook", EventTypes: []string{"test.thing.happened"}, CreatedBy: "user-1",
	})
	if err != nil {
		t.Fatalf("CreateWebhookSubscription: %v", err)
	}
	_, err = svc.UpdateWebhookSubscription(ctxFor("tenant-other"), UpdateWebhookSubscriptionInput{ID: created.ID})
	if !apperrIs(err, ErrWebhookSubscriptionNotFound) {
		t.Errorf("error = %v, want ErrWebhookSubscriptionNotFound (cross-tenant indistinguishable from absent)", err)
	}
}

// TestService_CreateWebhookSubscription_AuditFailureAfterCommit_ReturnsSecretWithError
// is the webhook twin of the API-key material-retention contract
// (service_test.go's TestService_Create_AuditFailureAfterCommit_ReturnsKeyWithError):
// the subscription row has committed, but recording its audit event failed
// (the bus is down). Service.CreateWebhookSubscription must return the
// created subscription ALONGSIDE the error -- its signing secret is shown
// exactly once and never reproduced, so a (nil, err) answer would orphan a
// live, already-delivering subscription whose secret nobody ever received
// and whose raw secret is lost forever.
func TestService_CreateWebhookSubscription_AuditFailureAfterCommit_ReturnsSecretWithError(t *testing.T) {
	_, svc := newWebhookTestService(t)
	svc.bus = errBus{}

	created, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		URL: "https://example.com/hook", EventTypes: []string{"test.thing.happened"}, CreatedBy: "user-1",
	})
	if err == nil {
		t.Fatal("CreateWebhookSubscription = nil error, want the audit failure reported")
	}
	if created == nil {
		t.Fatal("CreateWebhookSubscription returned nil on its post-commit audit failure: the secret was lost")
	}
	if created.Secret == "" {
		t.Fatal("CreateWebhookSubscription returned a created subscription with an empty Secret")
	}

	// The row committed and is live, and the secret this module stored is
	// exactly the one returned (read back through the encrypting
	// serializer).
	row, findErr := svc.webhookRepo.FindByID(ctxFor(testTenant), created.ID)
	if findErr != nil {
		t.Fatalf("FindByID after the partial create: %v", findErr)
	}
	if row.Secret != created.Secret {
		t.Error("the committed row's secret does not match the returned secret")
	}
	if !row.Active {
		t.Error("the committed subscription is inactive, want active")
	}
}

// TestService_ListRecentWebhookDeliveries_LimitClampedToMaximum pins the
// deliveries-listing bound: a caller-requested limit above
// maxRecentDeliveriesLimit must be clamped, never passed through to the
// repository as an unbounded query -- the fragment's own limit parameter
// declares the same 100 ceiling, and this Service-level clamp is its
// enforcement (the generated parameter binding performs no range
// validation). The clamp is observed by asking for a huge limit and
// checking that only as many rows as exist -- more than the default 50 but
// fewer than the huge request -- come back: a non-clamped (or default-
// falling) implementation would answer 50 or fewer.
func TestService_ListRecentWebhookDeliveries_LimitClampedToMaximum(t *testing.T) {
	_, svc := newWebhookTestService(t)
	subID, _ := createTestSubscription(t, svc, "https://example.com/hook")

	// 120 pending deliveries: above both the 50 default and the 100
	// maximum. Each handleDomainEvent must create a DISTINCT delivery (the
	// fan-out key carries an occurrence marker -- see
	// webhook_delivery_test.go), so the clock is pinned to advance one
	// nanosecond per call.
	at := fixedNow
	svc.now = func() time.Time { return at }
	prevQueue := svc.queue
	svc.queue = &fakeQueue{}
	defer func() { svc.queue = prevQueue }()
	for i := 0; i < 120; i++ {
		at = at.Add(time.Nanosecond)
		if err := svc.handleDomainEvent(ctxFor(testTenant), pkgcore.Event{Type: testMapping.InternalType, TenantID: testTenant}); err != nil {
			t.Fatalf("handleDomainEvent %d: %v", i, err)
		}
	}

	deliveries, err := svc.ListRecentWebhookDeliveries(ctxFor(testTenant), subID, 1<<30)
	if err != nil {
		t.Fatalf("ListRecentWebhookDeliveries: %v", err)
	}
	if len(deliveries) != maxRecentDeliveriesLimit {
		t.Fatalf("len(deliveries) = %d with 120 rows and a huge requested limit, want exactly %d (clamped to the maximum, not passed through unbounded)", len(deliveries), maxRecentDeliveriesLimit)
	}
}

func TestService_DeleteWebhookSubscription_RemovesIt(t *testing.T) {
	_, svc := newWebhookTestService(t)
	created, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		URL: "https://example.com/hook", EventTypes: []string{"test.thing.happened"}, CreatedBy: "user-1",
	})
	if err != nil {
		t.Fatalf("CreateWebhookSubscription: %v", err)
	}
	if deleteErr := svc.DeleteWebhookSubscription(ctxFor(testTenant), created.ID); deleteErr != nil {
		t.Fatalf("DeleteWebhookSubscription: %v", deleteErr)
	}
	list, err := svc.ListWebhookSubscriptions(ctxFor(testTenant))
	if err != nil {
		t.Fatalf("ListWebhookSubscriptions: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("len(list) = %d after delete, want 0", len(list))
	}
	if err := svc.DeleteWebhookSubscription(ctxFor(testTenant), created.ID); !apperrIs(err, ErrWebhookSubscriptionNotFound) {
		t.Errorf("second delete error = %v, want ErrWebhookSubscriptionNotFound", err)
	}
}

// TestService_DeleteWebhookSubscription_MarksInsteadOfPhysicallyRemoving
// proves DeleteWebhookSubscription performs a mark-delete: the row
// survives in the table (findable through the unscoped repository) with
// deleted_at set, rather than vanishing -- the property a physical DELETE
// could never offer.
func TestService_DeleteWebhookSubscription_MarksInsteadOfPhysicallyRemoving(t *testing.T) {
	m, svc := newWebhookTestService(t)
	created, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		URL: "https://example.com/hook", EventTypes: []string{"test.thing.happened"}, CreatedBy: "user-1",
	})
	if err != nil {
		t.Fatalf("CreateWebhookSubscription: %v", err)
	}
	if err := svc.DeleteWebhookSubscription(ctxFor(testTenant), created.ID); err != nil {
		t.Fatalf("DeleteWebhookSubscription: %v", err)
	}

	var count int64
	if err := dbkit.WithTenantSession(ctxFor(testTenant), m.db, func(tx *gorm.DB) error {
		return tx.Unscoped().Model(&WebhookSubscription{}).
			Where("id = ? AND deleted_at IS NOT NULL", created.ID).
			Count(&count).Error
	}); err != nil {
		t.Fatalf("counting the mark-deleted row: %v", err)
	}
	if count != 1 {
		t.Fatalf("mark-deleted row count = %d, want exactly 1 -- Delete must mark, never physically remove", count)
	}
}

// TestService_RestoreWebhookSubscription_UndoesTheDelete proves the full
// cycle: delete, verify invisible everywhere a normal read looks, restore,
// verify visible again with URL/EventTypes/CreatedBy intact and Active
// forced false regardless of its value before deletion (see
// RestoreWebhookSubscription's own doc comment for why).
func TestService_RestoreWebhookSubscription_UndoesTheDelete(t *testing.T) {
	_, svc := newWebhookTestService(t)
	created, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		URL: "https://example.com/hook", EventTypes: []string{"test.thing.happened"}, CreatedBy: "user-1",
	})
	if err != nil {
		t.Fatalf("CreateWebhookSubscription: %v", err)
	}
	if deleteErr := svc.DeleteWebhookSubscription(ctxFor(testTenant), created.ID); deleteErr != nil {
		t.Fatalf("DeleteWebhookSubscription: %v", deleteErr)
	}
	list, err := svc.ListWebhookSubscriptions(ctxFor(testTenant))
	if err != nil {
		t.Fatalf("ListWebhookSubscriptions (after delete): %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("len(list) = %d after delete, want 0", len(list))
	}

	if restoreErr := svc.RestoreWebhookSubscription(ctxFor(testTenant), created.ID); restoreErr != nil {
		t.Fatalf("RestoreWebhookSubscription: %v", restoreErr)
	}

	list, err = svc.ListWebhookSubscriptions(ctxFor(testTenant))
	if err != nil {
		t.Fatalf("ListWebhookSubscriptions (after restore): %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len(list) = %d after restore, want exactly 1", len(list))
	}
	restored := list[0]
	if restored.ID != created.ID || restored.URL != created.URL {
		t.Errorf("restored = %+v, want it to match the created subscription's id and URL", restored)
	}
	if len(restored.EventTypes) != 1 || restored.EventTypes[0] != "test.thing.happened" {
		t.Errorf("EventTypes = %v, want the original selection intact", restored.EventTypes)
	}
	if restored.CreatedBy != created.CreatedBy {
		t.Errorf("CreatedBy = %q, want %q", restored.CreatedBy, created.CreatedBy)
	}
	if restored.Active {
		t.Error("Active = true after Restore, want false -- Restore must always land a subscription paused")
	}
}

// TestService_RestoreWebhookSubscription_ForcesActiveFalse_EvenIfActiveAtDelete
// pins the exact scenario RestoreWebhookSubscription's own doc comment
// argues about: a subscription that was ACTIVE at the moment of deletion
// must not resume fan-out just because it was restored.
func TestService_RestoreWebhookSubscription_ForcesActiveFalse_EvenIfActiveAtDelete(t *testing.T) {
	fq := &fakeQueue{}
	_, svc := newWebhookTestService(t, WithWebhookQueue(fq))
	created, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		URL: "https://example.com/hook", EventTypes: []string{"test.thing.happened"}, CreatedBy: "user-1",
	})
	if err != nil {
		t.Fatalf("CreateWebhookSubscription: %v", err)
	}
	if !created.Active {
		t.Fatal("a freshly created subscription must start Active, this test's premise")
	}
	if err := svc.DeleteWebhookSubscription(ctxFor(testTenant), created.ID); err != nil {
		t.Fatalf("DeleteWebhookSubscription: %v", err)
	}
	if err := svc.RestoreWebhookSubscription(ctxFor(testTenant), created.ID); err != nil {
		t.Fatalf("RestoreWebhookSubscription: %v", err)
	}

	// A domain event matching the restored subscription's own EventTypes
	// must NOT be fanned out to it: it is live again, but paused.
	if err := svc.handleDomainEvent(ctxFor(testTenant), pkgcore.Event{Type: testMapping.InternalType, TenantID: testTenant}); err != nil {
		t.Fatalf("handleDomainEvent: %v", err)
	}
	if len(fq.tasks) != 0 {
		t.Fatalf("len(fq.tasks) = %d, want 0 -- a restored-but-not-reactivated subscription must never be fanned out to", len(fq.tasks))
	}
}

// TestService_RestoreWebhookSubscription_UnknownID_ReturnsNotFound mirrors
// go/rbac's TestService_RestoreRole_NothingToRestore_IsReported and
// go/org's TestMemberService_Restore_UnknownID_ReturnsMembershipNotFound.
func TestService_RestoreWebhookSubscription_UnknownID_ReturnsNotFound(t *testing.T) {
	_, svc := newWebhookTestService(t)
	if err := svc.RestoreWebhookSubscription(ctxFor(testTenant), "no-such-id"); !apperrIs(err, ErrWebhookSubscriptionNotFound) {
		t.Errorf("error = %v, want ErrWebhookSubscriptionNotFound", err)
	}
}

// TestService_RestoreWebhookSubscription_LiveSubscription_ReturnsNotFound
// mirrors go/org's TestMemberService_Restore_LiveMembership_ReturnsMembershipNotFound:
// restoring a subscription that was never deleted reports the identical
// collapsed not-found signal as an id that never existed at all.
func TestService_RestoreWebhookSubscription_LiveSubscription_ReturnsNotFound(t *testing.T) {
	_, svc := newWebhookTestService(t)
	created, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		URL: "https://example.com/hook", EventTypes: []string{"test.thing.happened"}, CreatedBy: "user-1",
	})
	if err != nil {
		t.Fatalf("CreateWebhookSubscription: %v", err)
	}
	if err := svc.RestoreWebhookSubscription(ctxFor(testTenant), created.ID); !apperrIs(err, ErrWebhookSubscriptionNotFound) {
		t.Errorf("error = %v, want ErrWebhookSubscriptionNotFound", err)
	}
}

// TestService_RestoreWebhookSubscription_Twice_SecondCallReturnsNotFound
// mirrors go/org's TestMemberService_Restore_Twice_SecondCallReturnsMembershipNotFound.
func TestService_RestoreWebhookSubscription_Twice_SecondCallReturnsNotFound(t *testing.T) {
	_, svc := newWebhookTestService(t)
	created, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		URL: "https://example.com/hook", EventTypes: []string{"test.thing.happened"}, CreatedBy: "user-1",
	})
	if err != nil {
		t.Fatalf("CreateWebhookSubscription: %v", err)
	}
	if err := svc.DeleteWebhookSubscription(ctxFor(testTenant), created.ID); err != nil {
		t.Fatalf("DeleteWebhookSubscription: %v", err)
	}
	if err := svc.RestoreWebhookSubscription(ctxFor(testTenant), created.ID); err != nil {
		t.Fatalf("first RestoreWebhookSubscription: %v", err)
	}
	if err := svc.RestoreWebhookSubscription(ctxFor(testTenant), created.ID); !apperrIs(err, ErrWebhookSubscriptionNotFound) {
		t.Errorf("second RestoreWebhookSubscription error = %v, want ErrWebhookSubscriptionNotFound", err)
	}
}

// TestService_RestoreWebhookSubscription_CrossTenant_NotFound mirrors this
// file's own TestService_UpdateWebhookSubscription_CrossTenant_NotFound:
// restoring another tenant's mark-deleted row must be indistinguishable
// from restoring nothing at all.
func TestService_RestoreWebhookSubscription_CrossTenant_NotFound(t *testing.T) {
	_, svc := newWebhookTestService(t)
	created, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		URL: "https://example.com/hook", EventTypes: []string{"test.thing.happened"}, CreatedBy: "user-1",
	})
	if err != nil {
		t.Fatalf("CreateWebhookSubscription: %v", err)
	}
	if err := svc.DeleteWebhookSubscription(ctxFor(testTenant), created.ID); err != nil {
		t.Fatalf("DeleteWebhookSubscription: %v", err)
	}
	if err := svc.RestoreWebhookSubscription(ctxFor("tenant-other"), created.ID); !apperrIs(err, ErrWebhookSubscriptionNotFound) {
		t.Errorf("error = %v, want ErrWebhookSubscriptionNotFound (cross-tenant indistinguishable from absent)", err)
	}
}

func TestService_ListRecentWebhookDeliveries_EmptyForUnknownSubscription(t *testing.T) {
	_, svc := newWebhookTestService(t)
	got, err := svc.ListRecentWebhookDeliveries(ctxFor(testTenant), "no-such-subscription", 10)
	if err != nil {
		t.Fatalf("ListRecentWebhookDeliveries: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
}
