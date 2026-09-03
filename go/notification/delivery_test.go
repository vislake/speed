package notification

// delivery_test.go drives the outbound-delivery pipeline's job lifecycle
// end to end, through the same two-step shape a queue runs: Dispatch
// enqueues one job, and the worker's Handle runs one attempt over the
// enqueued job's own payload -- with the caller's state changes (an
// opt-out, an unsubscribe, an address edit) able to land between the two
// steps, exactly where the freshness semantics delivery.go's doc comments
// promise they are honoured.
//
// The tests are white-box (package notification) because the replay,
// freshness and failure semantics live in the service's private fields and
// constants -- the send-record probe (sendRecs/alreadyDelivered), the
// per-channel derived key (deriveDeliveryKey), the skip-reason vocabulary
// (skipReason*), the settle helpers' status writes -- and because the
// service is built by direct field assignment, the arrangement a host gets
// after Module.Register, without standing up a module. Each test starts
// with a fresh migrated database and empty transports, and asserts through
// the module's own repositories rather than through mocks: a send record is
// read with NewSendRecordRepository's ByTenantAndKey under the same
// white-box key, an inbox row through Repository.FindByDedupeKey.
//
// The env's catalog is the fixture clinic taxonomy's template bundle
// (render_test.go's testClinicCatalog) -- the shape a declaring business
// module ships -- and its preference service carries the same fixture
// taxonomy preference_service_test.go's attachedService attaches; the two
// agree on fixtureTypeAppointment, whose copy renders zh-CN when the
// dispatch's locale asks for it.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// deliveryTenant and deliveryUser are the fixed tenant and recipient every
// delivery test drives, mirroring module_test.go's tenantCtx("tenant-acme").
const (
	deliveryTenant = "tenant-acme"
	deliveryUser   = "user-7"
)

// deliveryAddresses is the address set the stub resolver holds for
// deliveryUser: both outbound channels covered, so a test that wants only
// one channel's path builds its own resolver map instead.
var deliveryAddresses = UserAddresses{
	Email: "patient@example.com",
	Phone: "+8613800138000",
}

// recordingSMSSender is an SMSSender test double that keeps every message
// instead of sending it, and can be made to fail -- the SMS twin of
// contact_test.go's recordingMailer, needed because the module's own
// console sender prints to a writer and therefore cannot inject a
// transport failure (the delivery pipeline's failAndRetry/failAndStop
// semantics are only reachable through a failing Send).
type recordingSMSSender struct {
	mu       sync.Mutex
	sent     []SMS
	failWith error
}

func (s *recordingSMSSender) Send(_ context.Context, sms SMS) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failWith != nil {
		return s.failWith
	}
	s.sent = append(s.sent, sms)
	return nil
}

func (s *recordingSMSSender) messages() []SMS {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SMS(nil), s.sent...)
}

// deliveryEnv is one fully seam-ful delivery service: a fresh migrated
// database shared by the preference service, the consent ledger and the
// delivery service (the arrangement Module.Register composes over one db),
// the clinic template catalog, a recording mailer, bus and SMS sender, and
// the stub queue and resolver the module's Register would receive through
// WithDeliveryQueue and WithUserAddressResolver. Every service is built by
// direct field assignment (same package), so each test starts with an
// empty ledger, empty inbox, empty send-record log and empty transports.
type deliveryEnv struct {
	db       *gorm.DB
	svc      *DeliveryService
	prefs    *PreferenceService
	contacts *ContactService
	host     *testHost
	queue    *stubQueue
	resolver *stubUserResolver
	sms      *recordingSMSSender
}

func newDeliveryEnv(t *testing.T) *deliveryEnv {
	t.Helper()
	registerContactSerializer()
	db := newTestDB(t)

	host := newTestHost(t)
	// The host's real notification bundle carries no clinic.* template ids;
	// the delivery renders a declaring module's copy, so the catalog is
	// swapped for the clinic fixture bundle. Consent-ledger messages never
	// render here (every contact in these tests is created business-attested,
	// which skips the verification-code send), so nothing the env needs
	// from the real bundle is lost.
	host.catalog = testClinicCatalog(t)

	queue := &stubQueue{}
	resolver := &stubUserResolver{byUser: make(map[string]UserAddresses)}
	sms := &recordingSMSSender{}

	prefs := NewPreferenceService(db)
	prefs.attachTypes(fixtureRegistrar{types: fixtureTypes})

	contacts := NewContactService(db)
	reg := pkgcore.NewRegistry(host.bus, host.kv, host.mailer)
	if err := reg.AuditActions.Add(contactAuditActionDecls...); err != nil {
		t.Fatalf("register the contact audit actions: %v", err)
	}
	contacts.sms = sms
	contacts.mailFrom = testMailFrom
	contacts.emailIndexer = testEmailIndexer(t)
	contacts.phoneIndexer = testPhoneIndexer(t)
	contacts.host = host
	contacts.audit = reg.AuditActions

	svc := newDeliveryService(db, prefs, contacts)
	svc.queue = queue
	svc.resolver = resolver
	svc.sms = sms
	svc.mailFrom = testMailFrom
	svc.host = host

	return &deliveryEnv{
		db:       db,
		svc:      svc,
		prefs:    prefs,
		contacts: contacts,
		host:     host,
		queue:    queue,
		resolver: resolver,
		sms:      sms,
	}
}

// deliveryDispatch returns the appointment-reminder dispatch every user
// delivery test drives: the fixture type whose template bundle renders all
// three channels, in the tenant's zh-CN locale, with the shared fixture
// params.
func deliveryDispatch() Dispatch {
	return Dispatch{
		TypeKey: fixtureTypeAppointment,
		Recipient: DispatchRecipient{
			Class:  RecipientClassUser,
			UserID: deliveryUser,
		},
		Locale: "zh-CN",
		Params: renderTestParams,
	}
}

// enqueue dispatches d through the service and returns the payload of the
// one task the queue recorded -- the bytes the worker's Handle will decode,
// exactly the Dispatch-to-job handoff a real queue carries. Tests that want
// a state change (an opt-out, an unsubscribe) to land between enqueue and
// attempt call enqueue, make the change, then attempt with the returned
// payload.
func (e *deliveryEnv) enqueue(t *testing.T, d Dispatch) []byte {
	t.Helper()
	before := len(e.queue.tasks)
	if _, err := e.svc.Dispatch(tenantCtx(deliveryTenant), d); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(e.queue.tasks) != before+1 {
		t.Fatalf("enqueued %d tasks, want exactly one more than the %d before", len(e.queue.tasks), before)
	}
	return e.queue.tasks[before].Payload
}

// attempt runs one worker attempt over payload -- the job the queue would
// hand to the delivery service's registered handler -- and returns the
// attempt's error (nil for a converged or deliberately stopped delivery).
func (e *deliveryEnv) attempt(t *testing.T, payload []byte) error {
	t.Helper()
	_, err := e.svc.Handle(tenantCtx(deliveryTenant), &jobs.Job{
		Type:    jobTypeDeliver,
		Payload: payload,
	}, nil)
	return err
}

// dispatchAndAttempt is the unbroken lifecycle: enqueue then run one
// attempt immediately, for tests whose state does not change in between.
func (e *deliveryEnv) dispatchAndAttempt(t *testing.T, d Dispatch) error {
	t.Helper()
	return e.attempt(t, e.enqueue(t, d))
}

// sendRecordByChannel reads the send record one delivery of d over channel
// settles under -- the white-box probe: the record is looked up under the
// derived key, as the delivery job itself would. A nil record means the
// attempt never settled one for that channel.
func (e *deliveryEnv) sendRecordByChannel(t *testing.T, ctx context.Context, d Dispatch, channel string) *SendRecord {
	t.Helper()
	key, err := deriveDeliveryKey(deliveryTenant, d, channel)
	if err != nil {
		t.Fatalf("deriveDeliveryKey(%s): %v", channel, err)
	}
	rec, err := e.svc.sendRecs.ByTenantAndKey(ctx, deliveryTenant, key)
	if err != nil {
		t.Fatalf("ByTenantAndKey(%s): %v", key, err)
	}
	return rec
}

// inboxRowByChannel reads the inbox row one delivery of d over the in-app
// channel produced, looked up under the same derived key; nil means the
// attempt never wrote one.
func (e *deliveryEnv) inboxRowByChannel(t *testing.T, ctx context.Context, d Dispatch) *InboxMessage {
	t.Helper()
	rec := e.sendRecordByChannel(t, ctx, d, ChannelInApp)
	if rec == nil {
		return nil
	}
	row, err := e.svc.inbox.FindByDedupeKey(ctx, rec.IdempotencyKey)
	if err != nil {
		t.Fatalf("FindByDedupeKey: %v", err)
	}
	return row
}

// TestDelivery_Dispatch_RefusesAMalformedShape pins Dispatch's validation
// gate, one offending field per case: each refusal names the field in its
// "field" parameter (the client's whole answer on what to fix), and no
// malformed dispatch ever reaches the queue.
func TestDelivery_Dispatch_RefusesAMalformedShape(t *testing.T) {
	env := newDeliveryEnv(t)

	base := Dispatch{
		TypeKey: fixtureTypeAppointment,
		Recipient: DispatchRecipient{
			Class:  RecipientClassUser,
			UserID: deliveryUser,
		},
		Locale: "zh-CN",
	}
	user := base.Recipient

	cases := []struct {
		name  string
		shape Dispatch
		field string
	}{
		{"an empty type key", Dispatch{}, "type_key"},
		{
			"a user recipient without a user id",
			Dispatch{TypeKey: base.TypeKey, Recipient: DispatchRecipient{Class: RecipientClassUser}, Locale: base.Locale},
			"recipient.user_id",
		},
		{"a missing locale", Dispatch{TypeKey: base.TypeKey, Recipient: user}, "locale"},
		{
			"an external recipient without a contact id",
			Dispatch{TypeKey: base.TypeKey, Recipient: DispatchRecipient{Class: RecipientClassExternal}, Locale: base.Locale},
			"recipient.contact_id",
		},
		{
			"an unknown recipient class",
			Dispatch{TypeKey: base.TypeKey, Recipient: DispatchRecipient{Class: "carrier-pigeon", UserID: deliveryUser}, Locale: base.Locale},
			"recipient.class",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.svc.Dispatch(tenantCtx(deliveryTenant), tc.shape)
			if err == nil {
				t.Fatal("Dispatch succeeded on a malformed dispatch, want ErrDispatchInvalid")
			}
			appErr, ok := apperr.As(err)
			if !ok {
				t.Fatalf("error %v is not an *apperr.Error, want code %s", err, ErrDispatchInvalid.Code)
			}
			if appErr.Code != ErrDispatchInvalid.Code {
				t.Fatalf("error code = %s, want %s", appErr.Code, ErrDispatchInvalid.Code)
			}
			if got := appErr.Params["field"]; got != tc.field {
				t.Errorf("field parameter = %v, want %q", got, tc.field)
			}
		})
	}

	if len(env.queue.tasks) != 0 {
		t.Errorf("queue holds %d tasks after refused dispatches, want none", len(env.queue.tasks))
	}
}

// TestDelivery_Dispatch_RequiresQueueAndTenant pins the two wiring-gate
// refusals of the enqueue path: without the delivery queue the dispatch
// cannot go anywhere (the boot-time seam validation module_test.go pins
// would already have refused this Module at Register), and without a tenant
// in ctx there is nothing to write the job's tenant under -- both refuse
// before any bytes are enqueued.
func TestDelivery_Dispatch_RequiresQueueAndTenant(t *testing.T) {
	t.Run("refused without a delivery queue", func(t *testing.T) {
		env := newDeliveryEnv(t)
		env.svc.queue = nil

		_, err := env.svc.Dispatch(tenantCtx(deliveryTenant), deliveryDispatch())
		assertCode(t, err, ErrDeliveryQueueRequired.Code)
		if len(env.queue.tasks) != 0 {
			t.Errorf("queue holds %d tasks, want none", len(env.queue.tasks))
		}
	})

	t.Run("refused without a tenant context", func(t *testing.T) {
		env := newDeliveryEnv(t)

		_, err := env.svc.Dispatch(context.Background(), deliveryDispatch())
		if !errors.Is(err, pkgcore.ErrNoTenant) {
			t.Fatalf("Dispatch without a tenant = %v, want pkgcore.ErrNoTenant", err)
		}
		if len(env.queue.tasks) != 0 {
			t.Errorf("queue holds %d tasks, want none", len(env.queue.tasks))
		}
	})
}

// TestDelivery_OneAttemptDeliversEveryResolvedChannelAndWritesAgreeingRows
// pins the happy path's write set, the consistency property the send
// record, the inbox row and the announced event must share: one delivery
// fans out to every channel the preferences resolve (in_app, email and
// sms for the fixture appointment type under no stored preference), each
// transport is called exactly once with the rendered zh-CN copy, each
// channel settles its own succeeded send record under its own derived key,
// the in-app channel's inbox row carries the same key as its record's
// IdempotencyKey, and the announced event names the row that was written.
func TestDelivery_OneAttemptDeliversEveryResolvedChannelAndWritesAgreeingRows(t *testing.T) {
	env := newDeliveryEnv(t)
	env.resolver.byUser[deliveryUser] = deliveryAddresses
	ctx := tenantCtx(deliveryTenant)
	d := deliveryDispatch()

	if err := env.dispatchAndAttempt(t, d); err != nil {
		t.Fatalf("delivery attempt: %v", err)
	}

	mails := env.host.mailer.messages()
	if len(mails) != 1 {
		t.Fatalf("mailer sent %d messages, want the one email delivery", len(mails))
	}
	if mails[0].From != testMailFrom {
		t.Errorf("mail From = %q, want the module's fixed %q", mails[0].From, testMailFrom)
	}
	if len(mails[0].To) != 1 || mails[0].To[0] != deliveryAddresses.Email {
		t.Errorf("mail To = %v, want the resolved email address alone", mails[0].To)
	}
	if mails[0].Subject != "预约提醒" {
		t.Errorf("mail subject = %q, want the zh-CN template rendered", mails[0].Subject)
	}
	if mails[0].Text != "王芳 您好，您预约的 2026-09-10 09:30 快到了。详情请登录查看。" {
		t.Errorf("mail text = %q, want the zh-CN body rendered with params interpolated", mails[0].Text)
	}

	sms := env.sms.messages()
	if len(sms) != 1 {
		t.Fatalf("SMS sender sent %d messages, want the one SMS delivery", len(sms))
	}
	if sms[0].To != deliveryAddresses.Phone {
		t.Errorf("SMS To = %q, want the resolved phone number", sms[0].To)
	}
	if sms[0].Text != "王芳 您好，您预约的 2026-09-10 09:30 快到了。" {
		t.Errorf("SMS text = %q, want the zh-CN SMS copy rendered with params interpolated", sms[0].Text)
	}

	// Each channel settled its own succeeded record under its own derived
	// key -- the same key derivation the job's replay probe uses.
	for _, channel := range []string{ChannelInApp, ChannelEmail, ChannelSMS} {
		rec := env.sendRecordByChannel(t, ctx, d, channel)
		if rec == nil {
			t.Fatalf("no send record for channel %s after a successful delivery", channel)
		}
		if rec.Status != SendRecordStatusSucceeded {
			t.Errorf("channel %s record status = %s, want %s", channel, rec.Status, SendRecordStatusSucceeded)
		}
		if rec.TenantID != deliveryTenant {
			t.Errorf("channel %s record tenant = %q, want %q", channel, rec.TenantID, deliveryTenant)
		}
		if rec.TypeKey != d.TypeKey {
			t.Errorf("channel %s record type = %q, want %q", channel, rec.TypeKey, d.TypeKey)
		}
		if rec.RecipientClass != RecipientClassUser || rec.RecipientUserID != deliveryUser {
			t.Errorf("channel %s record recipient = (%s, %q), want (user, %q)", channel, rec.RecipientClass, rec.RecipientUserID, deliveryUser)
		}
	}

	// The inbox row and its send record agree on the key -- the row the
	// recipient reads is the row the record says was delivered -- and the
	// row carries the rendered copy and the announced identity.
	row := env.inboxRowByChannel(t, ctx, d)
	if row == nil {
		t.Fatal("no inbox row after the in-app delivery")
	}
	if row.GetTenantID() != deliveryTenant {
		t.Errorf("inbox row tenant = %q, want %q", row.GetTenantID(), deliveryTenant)
	}
	if row.RecipientUserID != deliveryUser || row.TypeKey != d.TypeKey || row.Group != "appointments" {
		t.Errorf("inbox row = (recipient %q, type %q, group %q), want (%q, %q, appointments)",
			row.RecipientUserID, row.TypeKey, row.Group, deliveryUser, d.TypeKey)
	}
	if row.Title != "预约提醒" {
		t.Errorf("inbox row title = %q, want the zh-CN title", row.Title)
	}
	if row.Body != "王芳 您好，您预约的 2026-09-10 09:30 快到了。" {
		t.Errorf("inbox row body = %q, want the zh-CN body rendered with params interpolated", row.Body)
	}

	// The announced event names the row that was written, once.
	announced := env.host.bus.events(EventInboxCreated)
	if len(announced) != 1 {
		t.Fatalf("bus carries %d inbox-created events, want the one announcement", len(announced))
	}
	payload, ok := announced[0].Payload.(InboxCreatedPayload)
	if !ok {
		t.Fatalf("announced payload is %T, want InboxCreatedPayload", announced[0].Payload)
	}
	if payload.MessageID != row.ID {
		t.Errorf("announced message_id = %q, want the written row's id %q", payload.MessageID, row.ID)
	}
	if payload.RecipientUserID != deliveryUser || payload.TypeKey != d.TypeKey || payload.TenantID != deliveryTenant {
		t.Errorf("announced payload = %+v, want the row's own recipient, type and tenant", payload)
	}
}

// TestDelivery_RetriedJobSendsNothingASecondTime pins the exactly-once
// property the whole pipeline rests on: a second worker attempt over the
// same job -- the queue's response to a crash after the record write, a
// duplicate enqueue, any replay -- finds each channel's succeeded record
// under the derived key and converges without a second transport call, a
// second inbox row or a second record.
func TestDelivery_RetriedJobSendsNothingASecondTime(t *testing.T) {
	env := newDeliveryEnv(t)
	env.resolver.byUser[deliveryUser] = deliveryAddresses
	ctx := tenantCtx(deliveryTenant)
	payload := env.enqueue(t, deliveryDispatch())

	if err := env.attempt(t, payload); err != nil {
		t.Fatalf("first attempt: %v", err)
	}
	inboxAfterFirst := env.inboxRowByChannel(t, ctx, deliveryDispatch())
	idsAfterFirst := make(map[string]string)
	for _, channel := range []string{ChannelInApp, ChannelEmail, ChannelSMS} {
		rec := env.sendRecordByChannel(t, ctx, deliveryDispatch(), channel)
		idsAfterFirst[channel] = rec.ID
	}

	if err := env.attempt(t, payload); err != nil {
		t.Fatalf("retried attempt: %v", err)
	}

	if got := len(env.host.mailer.messages()); got != 1 {
		t.Errorf("mailer sent %d messages after the retry, want the first attempt's one", got)
	}
	if got := len(env.sms.messages()); got != 1 {
		t.Errorf("SMS sender sent %d messages after the retry, want the first attempt's one", got)
	}
	if got := env.host.bus.events(EventInboxCreated); len(got) != 1 {
		t.Errorf("bus carries %d inbox-created events after the retry, want the first attempt's one", len(got))
	}
	for _, channel := range []string{ChannelInApp, ChannelEmail, ChannelSMS} {
		rec := env.sendRecordByChannel(t, ctx, deliveryDispatch(), channel)
		if rec == nil {
			t.Fatalf("channel %s lost its send record on the retry", channel)
		}
		if rec.Status != SendRecordStatusSucceeded {
			t.Errorf("channel %s record status = %s after the retry, want %s", channel, rec.Status, SendRecordStatusSucceeded)
		}
		if rec.ID != idsAfterFirst[channel] {
			t.Errorf("channel %s record id changed across attempts (%s -> %s): the retry must adopt the first record, one per delivery for life",
				channel, idsAfterFirst[channel], rec.ID)
		}
	}
	row := env.inboxRowByChannel(t, ctx, deliveryDispatch())
	if row == nil {
		t.Fatal("inbox row lost on the retry")
	}
	if row.ID != inboxAfterFirst.ID {
		t.Errorf("inbox row id changed across attempts (%s -> %s), want the first row", inboxAfterFirst.ID, row.ID)
	}
}

// TestDelivery_OptOutBetweenEnqueueAndAttemptSendsNothing drives the
// freshness contract in its sharpest form: the dispatch is enqueued while
// the recipient's preference matrix still serves the type's defaults, and
// the recipient turns the whole type off BEFORE the worker attempt runs.
// The attempt resolves the channels at send time and finds none -- the
// opt-out is honoured, nothing is sent, and (the record half of the
// freshness promise) no channel even settles a send record, because a skip
// with no channel to skip on has nothing to record.
func TestDelivery_OptOutBetweenEnqueueAndAttemptSendsNothing(t *testing.T) {
	env := newDeliveryEnv(t)
	env.resolver.byUser[deliveryUser] = deliveryAddresses
	ctx := tenantCtx(deliveryTenant)
	payload := env.enqueue(t, deliveryDispatch())

	// The recipient's later decision, landing between enqueue and attempt:
	// opt out of the appointment type entirely (legal: it is
	// unsubscribable).
	if err := env.prefs.Set(ctx, deliveryUser, fixtureTypeAppointment, []string{}); err != nil {
		t.Fatalf("opt the recipient out: %v", err)
	}

	if err := env.attempt(t, payload); err != nil {
		t.Fatalf("delivery attempt after the opt-out: %v", err)
	}

	if got := len(env.host.mailer.messages()); got != 0 {
		t.Errorf("mailer sent %d messages to an opted-out recipient, want none", got)
	}
	if got := len(env.sms.messages()); got != 0 {
		t.Errorf("SMS sender sent %d messages to an opted-out recipient, want none", got)
	}
	if row := env.inboxRowByChannel(t, ctx, deliveryDispatch()); row != nil {
		t.Errorf("opted-out recipient received an inbox row (%q)", row.Title)
	}
	if rec := env.sendRecordByChannel(t, ctx, deliveryDispatch(), ChannelEmail); rec != nil {
		t.Errorf("an opted-out delivery settled a send record (%s: %s), want none", rec.Status, rec.Error)
	}
}

// TestDelivery_AddressRemovedBetweenEnqueueAndAttemptSkipsTheChannel drives
// the resolver half of the freshness contract the same way: the dispatch is
// enqueued while the host still has the user's email on file, and the
// address is gone by the time the worker attempts. The attempt resolves at
// send time, finds no email, and settles a skipped record carrying the
// no-email reason -- never a transport call to a stale address.
func TestDelivery_AddressRemovedBetweenEnqueueAndAttemptSkipsTheChannel(t *testing.T) {
	env := newDeliveryEnv(t)
	env.resolver.byUser[deliveryUser] = deliveryAddresses
	ctx := tenantCtx(deliveryTenant)
	payload := env.enqueue(t, deliveryDispatch())

	// The host's address book changes between enqueue and attempt: the
	// email is gone, the phone stays.
	env.resolver.byUser[deliveryUser] = UserAddresses{Phone: deliveryAddresses.Phone}

	if err := env.attempt(t, payload); err != nil {
		t.Fatalf("delivery attempt: %v", err)
	}

	if got := len(env.host.mailer.messages()); got != 0 {
		t.Errorf("mailer sent %d messages to an address that was gone at send time, want none", got)
	}
	rec := env.sendRecordByChannel(t, ctx, deliveryDispatch(), ChannelEmail)
	if rec == nil {
		t.Fatal("no send record for the email channel, want the skip recorded")
	}
	if rec.Status != SendRecordStatusSkipped || rec.Error != skipReasonNoEmail {
		t.Errorf("email record = (%s, %q), want (skipped, %q)", rec.Status, rec.Error, skipReasonNoEmail)
	}
}

// TestDelivery_UserWithNoAddressesSkipsEveryOutboundChannel pins the
// no-address skip for both transports at once -- the recipient the host has
// no email and no phone for. The in-app channel still delivers (it needs no
// address), the two transport channels settle skipped records carrying
// their reasons, and nothing reaches a transport.
func TestDelivery_UserWithNoAddressesSkipsEveryOutboundChannel(t *testing.T) {
	env := newDeliveryEnv(t)
	ctx := tenantCtx(deliveryTenant)

	if err := env.dispatchAndAttempt(t, deliveryDispatch()); err != nil {
		t.Fatalf("delivery attempt: %v", err)
	}

	if got := len(env.host.mailer.messages()); got != 0 {
		t.Errorf("mailer sent %d messages to a user with no email on file, want none", got)
	}
	if got := len(env.sms.messages()); got != 0 {
		t.Errorf("SMS sender sent %d messages to a user with no phone on file, want none", got)
	}
	if row := env.inboxRowByChannel(t, ctx, deliveryDispatch()); row == nil {
		t.Error("no inbox row: the in-app channel must deliver without any address")
	}

	email := env.sendRecordByChannel(t, ctx, deliveryDispatch(), ChannelEmail)
	if email == nil || email.Status != SendRecordStatusSkipped || email.Error != skipReasonNoEmail {
		t.Errorf("email record = %+v, want (skipped, %q)", email, skipReasonNoEmail)
	}
	sms := env.sendRecordByChannel(t, ctx, deliveryDispatch(), ChannelSMS)
	if sms == nil || sms.Status != SendRecordStatusSkipped || sms.Error != skipReasonNoPhone {
		t.Errorf("SMS record = %+v, want (skipped, %q)", sms, skipReasonNoPhone)
	}
}

// TestDelivery_ExternalContactReceivesTheRenderedEmail drives the external
// recipient path end to end: a business-attested email contact (created
// verified without a code, per contact.go's ContactCreateInput contract) is
// delivered to through the real consent gate, the copy renders in the
// platform default locale, and the settled record names the external
// recipient class and the contact id -- never a user id.
func TestDelivery_ExternalContactReceivesTheRenderedEmail(t *testing.T) {
	env := newDeliveryEnv(t)
	ctx := tenantCtx(deliveryTenant)

	contact, err := env.contacts.CreateContact(ctx, ContactCreateInput{
		Channel:    ChannelEmail,
		Address:    "wangfang@external.example.com",
		ConsentRef: "consent-ref-1",
	})
	if err != nil {
		t.Fatalf("create the verified contact: %v", err)
	}
	if contact.Status != ContactStatusVerified {
		t.Fatalf("business-attested contact status = %s, want verified", contact.Status)
	}

	d := Dispatch{
		TypeKey: fixtureTypeAppointment,
		Recipient: DispatchRecipient{
			Class:     RecipientClassExternal,
			ContactID: contact.ID,
		},
		Locale: "zh-CN",
		Params: renderTestParams,
	}
	if err := env.dispatchAndAttempt(t, d); err != nil {
		t.Fatalf("delivery attempt: %v", err)
	}

	mails := env.host.mailer.messages()
	if len(mails) != 1 {
		t.Fatalf("mailer sent %d messages, want the one contact email", len(mails))
	}
	if len(mails[0].To) != 1 || mails[0].To[0] != "wangfang@external.example.com" {
		t.Errorf("mail To = %v, want the verified contact's address", mails[0].To)
	}
	if mails[0].Subject != "预约提醒" {
		t.Errorf("mail subject = %q, want the zh-CN copy (the platform default locale)", mails[0].Subject)
	}
	if mails[0].Text != "王芳 您好，您预约的 2026-09-10 09:30 快到了。详情请登录查看。" {
		t.Errorf("mail text = %q, want the zh-CN body rendered with params interpolated", mails[0].Text)
	}

	rec := env.sendRecordByChannel(t, ctx, d, ChannelEmail)
	if rec == nil {
		t.Fatal("no send record for the contact delivery")
	}
	if rec.Status != SendRecordStatusSucceeded {
		t.Errorf("record status = %s, want %s", rec.Status, SendRecordStatusSucceeded)
	}
	if rec.RecipientClass != RecipientClassExternal || rec.ContactID != contact.ID {
		t.Errorf("record recipient = (class %s, contact %q), want (external, %q)", rec.RecipientClass, rec.ContactID, contact.ID)
	}
	if rec.RecipientUserID != "" {
		t.Errorf("record carries user id %q on an external delivery, want the empty sentinel", rec.RecipientUserID)
	}
	if row := env.inboxRowByChannel(t, ctx, d); row != nil {
		t.Error("a contact delivery wrote an inbox row: the in-app channel belongs to users")
	}
	if announced := env.host.bus.events(EventInboxCreated); len(announced) != 0 {
		t.Errorf("bus carries %d inbox-created events for a contact delivery, want none", len(announced))
	}
}

// TestDelivery_UnsubscribeBetweenEnqueueAndAttemptSkipsWithTheReasonRecorded
// drives the consent gate's freshness in its sharpest form: the delivery is
// enqueued while the contact is verified, and the tenant unsubscribes the
// contact BEFORE the worker attempt runs. The attempt's EnsureDeliverable
// gate finds the refusal, and the delivery settles a skipped record
// carrying the unsubscribed reason -- the operator's whole answer on why
// nothing was sent -- without ever calling the transport.
func TestDelivery_UnsubscribeBetweenEnqueueAndAttemptSkipsWithTheReasonRecorded(t *testing.T) {
	env := newDeliveryEnv(t)
	ctx := tenantCtx(deliveryTenant)

	contact, err := env.contacts.CreateContact(ctx, ContactCreateInput{
		Channel:    ChannelEmail,
		Address:    "wangfang@external.example.com",
		ConsentRef: "consent-ref-1",
	})
	if err != nil {
		t.Fatalf("create the verified contact: %v", err)
	}

	d := Dispatch{
		TypeKey: fixtureTypeAppointment,
		Recipient: DispatchRecipient{
			Class:     RecipientClassExternal,
			ContactID: contact.ID,
		},
		Locale: "zh-CN",
		Params: renderTestParams,
	}
	payload := env.enqueue(t, d)

	if _, err := env.contacts.Unsubscribe(ctx, UnsubscribeInput{ContactID: contact.ID}); err != nil {
		t.Fatalf("unsubscribe the contact: %v", err)
	}

	if err := env.attempt(t, payload); err != nil {
		t.Fatalf("delivery attempt after the unsubscribe: %v", err)
	}

	if got := len(env.host.mailer.messages()); got != 0 {
		t.Errorf("mailer sent %d messages to an unsubscribed contact, want none", got)
	}
	rec := env.sendRecordByChannel(t, ctx, d, ChannelEmail)
	if rec == nil {
		t.Fatal("no send record for the refused contact delivery, want the skip recorded")
	}
	if rec.Status != SendRecordStatusSkipped {
		t.Errorf("record status = %s, want %s", rec.Status, SendRecordStatusSkipped)
	}
	if rec.Error != skipReasonUnsubscribed {
		t.Errorf("record reason = %q, want %q", rec.Error, skipReasonUnsubscribed)
	}
	if rec.RecipientClass != RecipientClassExternal || rec.ContactID != contact.ID {
		t.Errorf("record recipient = (class %s, contact %q), want (external, %q)", rec.RecipientClass, rec.ContactID, contact.ID)
	}
}

// TestDelivery_UnknownContactIsSkippedSilently pins the not-found answer of
// the consent gate: a delivery naming a contact id no row of the tenant
// holds is refused without a record and without a transport call -- the
// refusal surfaces neither to the queue (the job converges) nor as a send
// record (there is no recipient state worth recording), exactly as
// deliverToContact's doc comment promises.
func TestDelivery_UnknownContactIsSkippedSilently(t *testing.T) {
	env := newDeliveryEnv(t)
	ctx := tenantCtx(deliveryTenant)

	d := Dispatch{
		TypeKey: fixtureTypeAppointment,
		Recipient: DispatchRecipient{
			Class:     RecipientClassExternal,
			ContactID: "no-such-contact",
		},
		Locale: "zh-CN",
		Params: renderTestParams,
	}
	if err := env.dispatchAndAttempt(t, d); err != nil {
		t.Fatalf("delivery attempt: %v", err)
	}

	if got := len(env.host.mailer.messages()); got != 0 {
		t.Errorf("mailer sent %d messages to an unknown contact, want none", got)
	}
	if rec := env.sendRecordByChannel(t, ctx, d, ChannelEmail); rec != nil {
		t.Errorf("an unknown-contact delivery settled a send record (%s: %s), want none", rec.Status, rec.Error)
	}
}

// TestDelivery_TransientTransportFailureIsRecordedAndRetried pins the
// retryable-failure half of the failure semantics: a transport that fails
// with an ordinary (non-permanent) error settles a failed record carrying
// the cause and returns the error -- the queue's retry is the response --
// and the retry, once the transport recovers, converges on the SAME record
// as succeeded. One delivery keeps one record for life, whatever its
// attempts did.
func TestDelivery_TransientTransportFailureIsRecordedAndRetried(t *testing.T) {
	env := newDeliveryEnv(t)
	env.resolver.byUser[deliveryUser] = deliveryAddresses
	ctx := tenantCtx(deliveryTenant)
	payload := env.enqueue(t, deliveryDispatch())

	env.host.mailer.failWith = errors.New("smtp: connection refused")
	if err := env.attempt(t, payload); err == nil {
		t.Fatal("attempt with a failing transport succeeded, want the retryable error back")
	}

	rec := env.sendRecordByChannel(t, ctx, deliveryDispatch(), ChannelEmail)
	if rec == nil {
		t.Fatal("no send record for the failed email delivery")
	}
	if rec.Status != SendRecordStatusFailed {
		t.Errorf("email record status = %s, want %s", rec.Status, SendRecordStatusFailed)
	}
	if !strings.Contains(rec.Error, "smtp: connection refused") {
		t.Errorf("email record error = %q, want the transport cause recorded", rec.Error)
	}
	if got := len(env.host.mailer.messages()); got != 0 {
		t.Errorf("mailer recorded %d sent messages for a refused send, want none", got)
	}
	failedID := rec.ID

	// The transport recovers; the queue retries the same job.
	env.host.mailer.failWith = nil
	if err := env.attempt(t, payload); err != nil {
		t.Fatalf("retried attempt: %v", err)
	}

	rec = env.sendRecordByChannel(t, ctx, deliveryDispatch(), ChannelEmail)
	if rec == nil || rec.Status != SendRecordStatusSucceeded {
		t.Fatalf("email record after the retry = %+v, want succeeded", rec)
	}
	if rec.ID != failedID {
		t.Errorf("record id changed across attempts (%s -> %s), want one record per delivery", failedID, rec.ID)
	}
	mails := env.host.mailer.messages()
	if len(mails) != 1 {
		t.Errorf("mailer sent %d messages, want the single retried send", len(mails))
	}
	if mails[0].To[0] != deliveryAddresses.Email {
		t.Errorf("retried mail To = %v, want the resolved address", mails[0].To)
	}
}

// TestDelivery_SMSTransientFailureIsRecordedAndRetried is the SMS twin of
// the email test: the same retryable-failure semantics through the module's
// own SMS seam, driven by a failing sender rather than a failing mailer.
func TestDelivery_SMSTransientFailureIsRecordedAndRetried(t *testing.T) {
	env := newDeliveryEnv(t)
	env.resolver.byUser[deliveryUser] = deliveryAddresses
	ctx := tenantCtx(deliveryTenant)
	payload := env.enqueue(t, deliveryDispatch())

	env.sms.failWith = errors.New("gateway: timeout")
	if err := env.attempt(t, payload); err == nil {
		t.Fatal("attempt with a failing SMS transport succeeded, want the retryable error back")
	}
	rec := env.sendRecordByChannel(t, ctx, deliveryDispatch(), ChannelSMS)
	if rec == nil || rec.Status != SendRecordStatusFailed {
		t.Fatalf("SMS record after the failure = %+v, want failed", rec)
	}
	if !strings.Contains(rec.Error, "gateway: timeout") {
		t.Errorf("SMS record error = %q, want the transport cause recorded", rec.Error)
	}

	env.sms.failWith = nil
	if err := env.attempt(t, payload); err != nil {
		t.Fatalf("retried attempt: %v", err)
	}
	rec = env.sendRecordByChannel(t, ctx, deliveryDispatch(), ChannelSMS)
	if rec == nil || rec.Status != SendRecordStatusSucceeded {
		t.Fatalf("SMS record after the retry = %+v, want succeeded", rec)
	}
	sent := env.sms.messages()
	if len(sent) != 1 || sent[0].To != deliveryAddresses.Phone {
		t.Errorf("SMS messages = %+v, want the single retried send to the resolved phone", sent)
	}
}

// TestDelivery_PermanentTransportFailureStopsTheChannelWithoutRetrying pins
// the terminal-failure half of the failure semantics: a transport error
// wrapping ErrTransportPermanent -- the address the gateway refuses, the
// message it will never accept -- settles a failed record and returns nil,
// so the queue does NOT retry a failure retrying cannot resolve. The
// per-channel independence shows in the same attempt: the email channel
// stops while the SMS channel (a healthy transport of its own) still
// delivers.
func TestDelivery_PermanentTransportFailureStopsTheChannelWithoutRetrying(t *testing.T) {
	env := newDeliveryEnv(t)
	env.resolver.byUser[deliveryUser] = deliveryAddresses
	ctx := tenantCtx(deliveryTenant)
	payload := env.enqueue(t, deliveryDispatch())

	env.host.mailer.failWith = fmt.Errorf("550 mailbox unavailable: %w", ErrTransportPermanent)
	if err := env.attempt(t, payload); err != nil {
		t.Fatalf("attempt with a permanent transport failure returned %v, want nil (stop, not retry)", err)
	}

	rec := env.sendRecordByChannel(t, ctx, deliveryDispatch(), ChannelEmail)
	if rec == nil || rec.Status != SendRecordStatusFailed {
		t.Fatalf("email record after the permanent failure = %+v, want failed", rec)
	}
	if !strings.Contains(rec.Error, "550 mailbox unavailable") {
		t.Errorf("email record error = %q, want the terminal cause recorded", rec.Error)
	}
	if got := len(env.host.mailer.messages()); got != 0 {
		t.Errorf("mailer recorded %d sent messages for a refused send, want none", got)
	}

	// The SMS channel of the same delivery was unaffected.
	smsRec := env.sendRecordByChannel(t, ctx, deliveryDispatch(), ChannelSMS)
	if smsRec == nil || smsRec.Status != SendRecordStatusSucceeded {
		t.Errorf("SMS record = %+v, want the independent channel to have succeeded", smsRec)
	}
	if got := len(env.sms.messages()); got != 1 {
		t.Errorf("SMS sender sent %d messages, want the one SMS delivery", got)
	}
}

// TestDelivery_ContactPermanentFailureMarksTheContactBounced pins the
// external-recipient terminal path: a permanent transport refusal on a
// contact delivery both settles the failed record (stop, not retry) and
// marks the contact bounced through the consent ledger -- the deliverability
// answer a later attempt to that address must find -- which the test proves
// by asking the consent gate itself.
func TestDelivery_ContactPermanentFailureMarksTheContactBounced(t *testing.T) {
	env := newDeliveryEnv(t)
	ctx := tenantCtx(deliveryTenant)

	contact, err := env.contacts.CreateContact(ctx, ContactCreateInput{
		Channel:    ChannelEmail,
		Address:    "wangfang@external.example.com",
		ConsentRef: "consent-ref-1",
	})
	if err != nil {
		t.Fatalf("create the verified contact: %v", err)
	}

	d := Dispatch{
		TypeKey: fixtureTypeAppointment,
		Recipient: DispatchRecipient{
			Class:     RecipientClassExternal,
			ContactID: contact.ID,
		},
		Locale: "zh-CN",
		Params: renderTestParams,
	}
	env.host.mailer.failWith = fmt.Errorf("550 mailbox unavailable: %w", ErrTransportPermanent)

	if attemptErr := env.dispatchAndAttempt(t, d); attemptErr != nil {
		t.Fatalf("attempt with a permanent contact failure returned %v, want nil (stop, not retry)", attemptErr)
	}

	rec := env.sendRecordByChannel(t, ctx, d, ChannelEmail)
	if rec == nil || rec.Status != SendRecordStatusFailed {
		t.Fatalf("record after the permanent contact failure = %+v, want failed", rec)
	}

	// The consent gate now refuses the address as bounced -- the mark
	// landed, and carries the contact's channel for the refusal record.
	_, err = env.contacts.EnsureDeliverable(ctx, contact.ID)
	assertCode(t, err, ErrContactBounced.Code)
	appErr, _ := apperr.As(err)
	if got := appErr.Params["channel"]; got != ChannelEmail {
		t.Errorf("bounce refusal channel = %v, want %q", got, ChannelEmail)
	}
}

// TestDelivery_MissingTemplateCopyStopsTheAttempt pins the render failure
// as a terminal, recorded stop: a delivery whose type has no copy for the
// resolved channel (here clinic.reminder_only, whose fixture bundle carries
// an in-app title alone) fails before any transport call and settles a
// failed record -- and returns nil, because retrying a missing template
// would only fail again.
func TestDelivery_MissingTemplateCopyStopsTheAttempt(t *testing.T) {
	env := newDeliveryEnv(t)
	env.resolver.byUser[deliveryUser] = deliveryAddresses
	// The fixture taxonomy is swapped for a single type whose copy exists
	// only on the in-app channel, while its declared channels ask for email.
	env.prefs.attachTypes(fixtureRegistrar{types: []pkgcore.NotificationType{
		{Key: "clinic.reminder_only", Group: "appointments", DefaultChannels: []string{ChannelEmail}, Unsubscribable: false},
	}})
	ctx := tenantCtx(deliveryTenant)

	d := Dispatch{
		TypeKey: "clinic.reminder_only",
		Recipient: DispatchRecipient{
			Class:  RecipientClassUser,
			UserID: deliveryUser,
		},
		Locale: "zh-CN",
	}
	if err := env.dispatchAndAttempt(t, d); err != nil {
		t.Fatalf("attempt with a missing template returned %v, want nil (stop, not retry)", err)
	}

	if got := len(env.host.mailer.messages()); got != 0 {
		t.Errorf("mailer sent %d messages rendered from nothing, want none", got)
	}
	rec := env.sendRecordByChannel(t, ctx, d, ChannelEmail)
	if rec == nil || rec.Status != SendRecordStatusFailed {
		t.Fatalf("record after the render failure = %+v, want failed", rec)
	}
	if rec.Error == "" {
		t.Error("record error is empty, want the render cause recorded")
	}
	if row := env.inboxRowByChannel(t, ctx, d); row != nil {
		t.Error("a render-failed delivery wrote an inbox row")
	}
}

// TestDelivery_AnnounceFailureRetriesAndConverges drives the pipeline's
// most delicate recovery: the inbox row is committed, then the bus refuses
// the announcement. The attempt settles a failed in-app record and returns
// the error -- a row whose announcement never went out must be retried --
// while the other channels of the same delivery succeed independently. The
// retry finds the committed row through the dedupe probe, announces it (no
// second row, no second record), and converges the in-app record to
// succeeded without touching the other channels' already-succeeded records
// or their transports.
func TestDelivery_AnnounceFailureRetriesAndConverges(t *testing.T) {
	env := newDeliveryEnv(t)
	env.resolver.byUser[deliveryUser] = deliveryAddresses
	ctx := tenantCtx(deliveryTenant)
	payload := env.enqueue(t, deliveryDispatch())

	env.host.bus.failWith = errors.New("bus: stream unavailable")
	if err := env.attempt(t, payload); err == nil {
		t.Fatal("attempt with a failing bus succeeded, want the retryable error back")
	}

	// The row is already committed -- the announcement is latency in front
	// of a durable row, never the row's own delivery -- and its record
	// carries the failure.
	row := env.inboxRowByChannel(t, ctx, deliveryDispatch())
	if row == nil {
		t.Fatal("no inbox row: the row must be committed before the announcement goes out")
	}
	rec := env.sendRecordByChannel(t, ctx, deliveryDispatch(), ChannelInApp)
	if rec == nil || rec.Status != SendRecordStatusFailed {
		t.Fatalf("in-app record after the announce failure = %+v, want failed", rec)
	}
	failedID := rec.ID
	// The other channels of the same delivery converged independently.
	if got := len(env.host.mailer.messages()); got != 1 {
		t.Errorf("mailer sent %d messages, want the email delivery", got)
	}
	if emailRec := env.sendRecordByChannel(t, ctx, deliveryDispatch(), ChannelEmail); emailRec == nil || emailRec.Status != SendRecordStatusSucceeded {
		t.Errorf("email record = %+v, want succeeded", emailRec)
	}

	// The bus recovers; the queue retries the same job.
	env.host.bus.failWith = nil
	if err := env.attempt(t, payload); err != nil {
		t.Fatalf("retried attempt: %v", err)
	}

	if got := len(env.host.mailer.messages()); got != 1 {
		t.Errorf("mailer sent %d messages after the retry, want the first attempt's one", got)
	}
	if got := len(env.sms.messages()); got != 1 {
		t.Errorf("SMS sender sent %d messages after the retry, want the first attempt's one", got)
	}
	rowAfter := env.inboxRowByChannel(t, ctx, deliveryDispatch())
	if rowAfter == nil || rowAfter.ID != row.ID {
		t.Errorf("inbox row after the retry = %+v, want the first attempt's row %q untouched", rowAfter, row.ID)
	}
	rec = env.sendRecordByChannel(t, ctx, deliveryDispatch(), ChannelInApp)
	if rec == nil || rec.Status != SendRecordStatusSucceeded {
		t.Fatalf("in-app record after the retry = %+v, want succeeded", rec)
	}
	if rec.ID != failedID {
		t.Errorf("record id changed across attempts (%s -> %s), want one record per delivery", failedID, rec.ID)
	}

	// Two announcements of the same row -- the failed publish and the
	// retry's -- both naming the one committed row; never a second row and
	// never a row without an announcement.
	announced := env.host.bus.events(EventInboxCreated)
	if len(announced) != 2 {
		t.Fatalf("bus carries %d inbox-created events, want the failed publish and the retry's", len(announced))
	}
	for i, evt := range announced {
		payload, ok := evt.Payload.(InboxCreatedPayload)
		if !ok {
			t.Fatalf("announcement %d payload is %T, want InboxCreatedPayload", i, evt.Payload)
		}
		if payload.MessageID != row.ID {
			t.Errorf("announcement %d message_id = %q, want the committed row's id %q", i, payload.MessageID, row.ID)
		}
	}
}

// TestDelivery_ResolverFailureIsRecordedAndRetried pins the resolver
// seam's retryable failures: an address lookup that fails settles failed
// records on the channels that needed it and returns the error for the
// queue to retry -- the user may have an address by the next attempt --
// while the in-app channel (which needs no address) delivers regardless.
func TestDelivery_ResolverFailureIsRecordedAndRetried(t *testing.T) {
	env := newDeliveryEnv(t)
	env.resolver.byUser[deliveryUser] = deliveryAddresses
	env.resolver.err = errors.New("directory: unavailable")
	ctx := tenantCtx(deliveryTenant)
	payload := env.enqueue(t, deliveryDispatch())

	if err := env.attempt(t, payload); err == nil {
		t.Fatal("attempt with a failing resolver succeeded, want the retryable error back")
	}

	if got := len(env.host.mailer.messages()); got != 0 {
		t.Errorf("mailer sent %d messages despite the resolver failure, want none", got)
	}
	for _, channel := range []string{ChannelEmail, ChannelSMS} {
		rec := env.sendRecordByChannel(t, ctx, deliveryDispatch(), channel)
		if rec == nil || rec.Status != SendRecordStatusFailed {
			t.Fatalf("channel %s record after the resolver failure = %+v, want failed", channel, rec)
		}
		if !strings.Contains(rec.Error, "resolve addresses") {
			t.Errorf("channel %s record error = %q, want the resolver cause recorded", channel, rec.Error)
		}
	}
	// The in-app channel never consulted the resolver.
	if rec := env.sendRecordByChannel(t, ctx, deliveryDispatch(), ChannelInApp); rec == nil || rec.Status != SendRecordStatusSucceeded {
		t.Errorf("in-app record = %+v, want the independent channel to have succeeded", rec)
	}

	// The directory recovers; the queue retries the same job.
	env.resolver.err = nil
	if err := env.attempt(t, payload); err != nil {
		t.Fatalf("retried attempt: %v", err)
	}
	mails := env.host.mailer.messages()
	if len(mails) != 1 || mails[0].To[0] != deliveryAddresses.Email {
		t.Errorf("mail messages = %+v, want the single retried send to the resolved email", mails)
	}
	for _, channel := range []string{ChannelEmail, ChannelSMS} {
		if rec := env.sendRecordByChannel(t, ctx, deliveryDispatch(), channel); rec == nil || rec.Status != SendRecordStatusSucceeded {
			t.Errorf("channel %s record after the retry = %+v, want succeeded", channel, rec)
		}
	}
}

// TestDelivery_WorkerRefusesContextsWithoutATenant pins the worker-side
// tenant gate: Handle reads the tenant from its context (jobs rebuilds it
// from the job record before calling), and a context without one is refused
// with pkgcore.ErrNoTenant before any repository is touched -- never
// guessed, never inherited. The job handed over is a genuinely enqueued
// one's payload, so the refusal is the context's doing, not the payload's.
func TestDelivery_WorkerRefusesContextsWithoutATenant(t *testing.T) {
	env := newDeliveryEnv(t)
	payload := env.enqueue(t, deliveryDispatch())

	_, err := env.svc.Handle(context.Background(), &jobs.Job{
		Type:    jobTypeDeliver,
		Payload: payload,
	}, nil)
	if !errors.Is(err, pkgcore.ErrNoTenant) {
		t.Fatalf("Handle without a tenant = %v, want pkgcore.ErrNoTenant", err)
	}
}

// TestDelivery_RunDeliveryRefusesAnUnknownRecipientClass reaches the
// unreachable default branch of the class switch directly: Dispatch's
// validation refuses an unknown class before anything is enqueued, but a
// worker can in principle be handed a payload no validating caller saw, and
// the refusal must name the class field rather than half-deliver.
func TestDelivery_RunDeliveryRefusesAnUnknownRecipientClass(t *testing.T) {
	env := newDeliveryEnv(t)

	d := Dispatch{
		TypeKey: fixtureTypeAppointment,
		Recipient: DispatchRecipient{
			Class:  "carrier-pigeon",
			UserID: deliveryUser,
		},
		Locale: "zh-CN",
	}
	err := env.svc.runDelivery(tenantCtx(deliveryTenant), d)
	assertCode(t, err, ErrDispatchInvalid.Code)
	appErr, _ := apperr.As(err)
	if got := appErr.Params["field"]; got != "recipient.class" {
		t.Errorf("field parameter = %v, want %q", got, "recipient.class")
	}
}
