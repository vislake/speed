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
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/notification/locales"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/i18n"
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

// ghostContactType is the fixture type the delivery env's taxonomy never
// declares (it is absent from fixtureTypes) but whose email copy the
// widened catalog below carries -- the exact harmful precondition for a
// delivery that must be refused: a module that shipped its locale template
// resources under the <type_key>.<channel>.<part> convention without ever
// calling reg.Notifications.Add. The key lives under its own module
// prefix, exactly as a second business module's ids would (the i18n
// builder keeps every module in its own "<module>." id space).
const ghostContactType = "clinic-billing.bill_ready"

// billReadyFixtureFS is that undeclared module's bilingual template
// bundle: the email copy for ghostContactType in both languages, with
// identical id sets -- the shape every declaring module's locale files
// have (render_test.go's clinicFixtureFS documents the convention), minus
// the declaration itself.
var billReadyFixtureFS = fstest.MapFS{
	"zh-CN.toml": &fstest.MapFile{Data: []byte(`
"clinic-billing.bill_ready.email.subject" = "您的账单已就绪"
"clinic-billing.bill_ready.email.body_text" = "{{.patient_name}} 您好，您的账单已生成，详情请登录查看。"
`)},
	"en-US.toml": &fstest.MapFile{Data: []byte(`
"clinic-billing.bill_ready.email.subject" = "Your bill is ready"
"clinic-billing.bill_ready.email.body_text" = "Hi {{.patient_name}}, your bill is ready. Sign in for details."
`)},
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

	svc := newDeliveryService(NewRepository(db), prefs, contacts)
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

// TestDelivery_UnknownContactRefusalSurfacesToTheQueue pins the not-found
// answer of the consent gate: a delivery naming a contact id no row of the
// tenant holds returns the gate's refusal -- the queue's bounded
// retry-and-dead-letter horizon is the response, so a delivery that can
// never deliver converges to an operator-visible dead-lettered job instead
// of a silent success -- and the refusal settles nothing: no transport
// call, no send record, no channel to record under.
func TestDelivery_UnknownContactRefusalSurfacesToTheQueue(t *testing.T) {
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
	if err := env.dispatchAndAttempt(t, d); err == nil {
		t.Fatal("delivery to an unknown contact succeeded, want the not-found refusal back")
	} else {
		assertCode(t, err, ErrContactNotFound.Code)
	}

	if got := len(env.host.mailer.messages()); got != 0 {
		t.Errorf("mailer sent %d messages to an unknown contact, want none", got)
	}
	if rec := env.sendRecordByChannel(t, ctx, d, ChannelEmail); rec != nil {
		t.Errorf("an unknown-contact delivery settled a send record (%s: %s), want none", rec.Status, rec.Error)
	}
}

// TestDelivery_PendingContactRefusalIsDeferredUntilVerification pins the
// pending answer of the consent gate: a delivery to a contact whose double
// opt-in code was never verified returns the gate's refusal (the job
// retries within the queue's bounded horizon instead of silently
// converging), and once the code IS verified the very same job payload
// delivers -- the deferral's payoff. A verification landing inside the
// retry horizon lets the job deliver itself; the refusal is the job's
// signal to retry, where a silent skip would have dropped the message
// unless someone noticed and re-dispatched. The attempt's refusal carries
// no record because there is no channel to record under -- the gate
// refused before the contact's channel ever resolved.
func TestDelivery_PendingContactRefusalIsDeferredUntilVerification(t *testing.T) {
	env := newDeliveryEnv(t)
	ctx := tenantCtx(deliveryTenant)

	// A double_opt_in create renders the module's verification-code copy,
	// which the env's clinic fixture catalog does not carry, so the
	// catalog is widened to the merged real bundle plus the clinic
	// fixture (the shape Kernel.Bootstrap assembles for a host that ships
	// both modules).
	builder := i18n.NewBuilder()
	if err := builder.AddModule(moduleName, locales.FS); err != nil {
		t.Fatalf("AddModule(notification): %v", err)
	}
	if err := builder.AddModule("clinic", clinicFixtureFS); err != nil {
		t.Fatalf("AddModule(clinic): %v", err)
	}
	env.host.catalog = builder.Build()

	// A double_opt_in create: the row is pending and the verification code
	// goes out by email, the one sanctioned pre-consent message.
	contact, err := env.contacts.CreateContact(ctx, ContactCreateInput{
		Channel: ChannelEmail,
		Address: "wangfang@external.example.com",
	})
	if err != nil {
		t.Fatalf("create the pending contact: %v", err)
	}
	mails := env.host.mailer.messages()
	if len(mails) != 1 {
		t.Fatalf("create sent %d mails, want the one verification code", len(mails))
	}
	code := lastCode(t, mails[0].Text)

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

	if attemptErr := env.attempt(t, payload); attemptErr == nil {
		t.Fatal("delivery to the pending contact succeeded, want the not-verified refusal back")
	} else {
		assertCode(t, attemptErr, ErrContactNotVerified.Code)
	}
	if got := len(env.host.mailer.messages()); got != 1 {
		t.Errorf("mailer sent %d messages to the pending contact, want only the verification code", got)
	}
	if rec := env.sendRecordByChannel(t, ctx, d, ChannelEmail); rec != nil {
		t.Errorf("the refused attempt settled a send record (%s: %s), want none", rec.Status, rec.Error)
	}

	// The recipient proves consent; the same job payload now delivers.
	verified, err := env.contacts.VerifyCode(ctx, VerifyCodeInput{ContactID: contact.ID, Code: code})
	if err != nil {
		t.Fatalf("VerifyCode: %v", err)
	}
	if verified.Status != ContactStatusVerified {
		t.Fatalf("verified contact status = %s, want %s", verified.Status, ContactStatusVerified)
	}

	if attemptErr := env.attempt(t, payload); attemptErr != nil {
		t.Fatalf("delivery after the verification: %v", attemptErr)
	}
	rec := env.sendRecordByChannel(t, ctx, d, ChannelEmail)
	if rec == nil || rec.Status != SendRecordStatusSucceeded {
		t.Fatalf("record after the deferred delivery = %+v, want succeeded", rec)
	}
	if rec.RecipientClass != RecipientClassExternal || rec.ContactID != contact.ID {
		t.Errorf("record recipient = (class %s, contact %q), want (external, %q)", rec.RecipientClass, rec.ContactID, contact.ID)
	}
	mails = env.host.mailer.messages()
	if len(mails) != 2 {
		t.Fatalf("mailer sent %d messages, want the verification code plus the one delivery", len(mails))
	}
	if mails[1].To[0] != "wangfang@external.example.com" {
		t.Errorf("delivered mail To = %v, want the contact's address", mails[1].To)
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
	if rec.Error != failureReasonTransportFailed {
		t.Errorf("email record error = %q, want the bounded transient-transport classification %q", rec.Error, failureReasonTransportFailed)
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
	if rec.Error != failureReasonTransportFailed {
		t.Errorf("SMS record error = %q, want the bounded transient-transport classification %q", rec.Error, failureReasonTransportFailed)
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
	if rec.Error != failureReasonTransportRefused {
		t.Errorf("email record error = %q, want the bounded permanent-transport classification %q", rec.Error, failureReasonTransportRefused)
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

// TestDelivery_TerminalRefusalAfterASettledKeyDoesNotDowngradeTheSucceededRecord
// pins the never-downgrade-succeeded invariant (delivery.go's settle doc) in
// its sharpest form: the contact's delivery succeeds -- a succeeded send
// record is settled -- and the contact then reaches a terminal state
// (unsubscribed, or bounced). Running the SAME dispatch again afterwards --
// a replay, a duplicate enqueue, or a job the queue re-delivered -- is
// refused at the consent gate and settles a skipped record under the same
// derived key. That later refusal settle must not overwrite the earlier
// succeeded row, or the log would lose the historical fact that the key
// already delivered -- the operator's whole audit answer. The stored record
// must stay succeeded with every field intact and updated_at unmoved (the
// guard drops the write; it does not rewrite).
func TestDelivery_TerminalRefusalAfterASettledKeyDoesNotDowngradeTheSucceededRecord(t *testing.T) {
	flips := []struct {
		name string
		flip func(t *testing.T, env *deliveryEnv, ctx context.Context, contactID string)
	}{
		{
			name: "unsubscribed",
			flip: func(t *testing.T, env *deliveryEnv, ctx context.Context, contactID string) {
				t.Helper()
				if _, err := env.contacts.Unsubscribe(ctx, UnsubscribeInput{ContactID: contactID}); err != nil {
					t.Fatalf("unsubscribe the contact: %v", err)
				}
			},
		},
		{
			name: "bounced",
			flip: func(t *testing.T, env *deliveryEnv, ctx context.Context, contactID string) {
				t.Helper()
				if err := env.contacts.MarkBounced(ctx, contactID); err != nil {
					t.Fatalf("mark the contact bounced: %v", err)
				}
			},
		},
	}

	for _, flip := range flips {
		t.Run(flip.name, func(t *testing.T) {
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

			if err := env.dispatchAndAttempt(t, d); err != nil {
				t.Fatalf("first delivery attempt: %v", err)
			}
			if got := len(env.host.mailer.messages()); got != 1 {
				t.Fatalf("first delivery sent %d mails, want 1", got)
			}
			before := env.sendRecordByChannel(t, ctx, d, ChannelEmail)
			if before == nil || before.Status != SendRecordStatusSucceeded {
				t.Fatalf("record after the first delivery = %+v, want succeeded", before)
			}

			flip.flip(t, env, ctx, contact.ID)

			if err := env.dispatchAndAttempt(t, d); err != nil {
				t.Fatalf("replay attempt after the %s flip: %v", flip.name, err)
			}
			if got := len(env.host.mailer.messages()); got != 1 {
				t.Errorf("refused replay sent %d mails, want only the first delivery's one", got)
			}

			after := env.sendRecordByChannel(t, ctx, d, ChannelEmail)
			if after == nil {
				t.Fatal("replay refused the delivery but settled no record, want the succeeded row kept")
			}
			if after.Status != SendRecordStatusSucceeded {
				t.Errorf("replay after the %s flip downgraded the record to %s (%q); the key already delivered and the succeeded row must survive the later settle", flip.name, after.Status, after.Error)
			}
			if after.ID != before.ID || after.Error != before.Error ||
				after.DurationMs != before.DurationMs || !after.UpdatedAt.Equal(before.UpdatedAt) {
				t.Errorf("the refused replay's settle rewrote the succeeded row: before = %+v, after = %+v", before, after)
			}
		})
	}
}

// TestDelivery_ErrorSettleAfterASettledKeyDoesNotDowngradeTheSucceededRecord
// pins the same never-downgrade-succeeded invariant on the error settle
// paths: after a user's email delivery succeeded, a LATER attempt at the
// same key that fails for its own reason settles through the same
// failAndRetry the transient-failure legs call -- the acknowledged
// double-send window, where a retry that never saw the first attempt's
// record sends again and fails (delivery.go's deliverUserChannel doc spells
// the window out). That failed settle must not overwrite the succeeded row
// either: the record stays succeeded with every field intact, the attempt's
// cause still returns unchanged (the job's retry answer is untouched by the
// guard), and the job's own next retry converges on the alreadyDelivered
// probe.
func TestDelivery_ErrorSettleAfterASettledKeyDoesNotDowngradeTheSucceededRecord(t *testing.T) {
	env := newDeliveryEnv(t)
	env.resolver.byUser[deliveryUser] = deliveryAddresses
	ctx := tenantCtx(deliveryTenant)

	d := deliveryDispatch()
	if err := env.dispatchAndAttempt(t, d); err != nil {
		t.Fatalf("first delivery attempt: %v", err)
	}
	before := env.sendRecordByChannel(t, ctx, d, ChannelEmail)
	if before == nil || before.Status != SendRecordStatusSucceeded {
		t.Fatalf("record after the first delivery = %+v, want succeeded", before)
	}

	// A later attempt at the same key builds its record exactly as the
	// delivery path does -- fresh, with an empty ID that settle adopts or
	// invents -- and fails; failAndRetry is the transient-failure leg's own
	// settle call.
	rec := env.svc.sendRecordFor(deliveryTenant, d, ChannelEmail)
	key, err := deriveDeliveryKey(deliveryTenant, d, ChannelEmail)
	if err != nil {
		t.Fatalf("deriveDeliveryKey: %v", err)
	}
	rec.IdempotencyKey = key
	cause := errors.New("smtp 550 relay denied")

	got := env.svc.failAndRetry(ctx, deliveryTenant, rec, cause)
	if !errors.Is(got, cause) {
		t.Fatalf("failAndRetry returned %v, want the cause itself -- the guard must not change the retry answer", got)
	}

	after := env.sendRecordByChannel(t, ctx, d, ChannelEmail)
	if after == nil {
		t.Fatal("the failed settle removed the record, want the succeeded row kept")
	}
	if after.Status != SendRecordStatusSucceeded {
		t.Errorf("a failed settle after a succeeded key downgraded the record to %s (%q)", after.Status, after.Error)
	}
	if after.ID != before.ID || after.Error != before.Error ||
		after.DurationMs != before.DurationMs || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("the failed settle rewrote the succeeded row: before = %+v, after = %+v", before, after)
	}
}

// TestDelivery_StaleAdoptedSettleDoesNotDowngradeTheSucceededRecord pins the
// never-downgrade-succeeded invariant across the interleaving the two
// sequential-replay tests above cannot produce (delivery.go's settle doc):
// the acknowledged double-send window, where a later failed attempt at an
// already-succeeded key settles after its adopt probe ran while the row
// still said failed -- before the winner's succeeded settle committed. A
// guard checked only at adopt time lets that settle through, and the plain
// in-place Save it lands on the adopted id then erases the succeeded row
// the winner committed in between: the real delivery vanishes from the log,
// and the failed job's own retry probes a failed row and sends a third
// time. The guard must therefore be decided by the write itself -- one
// compare-and-set statement that checks and writes together -- not by a
// probe that ran earlier.
//
// The test reconstructs that window deterministically: the failed attempt's
// record carries the succeeded row's id pre-set -- exactly the state a
// settle whose adopt probe ran before the winner's commit reaches when it
// finally writes -- and failAndRetry settles it after the succeeded row is
// on disk. A probe-time guard never runs here (the id is already adopted),
// so only a guarded write can refuse the downgrade.
func TestDelivery_StaleAdoptedSettleDoesNotDowngradeTheSucceededRecord(t *testing.T) {
	env := newDeliveryEnv(t)
	env.resolver.byUser[deliveryUser] = deliveryAddresses
	ctx := tenantCtx(deliveryTenant)

	d := deliveryDispatch()
	if err := env.dispatchAndAttempt(t, d); err != nil {
		t.Fatalf("first delivery attempt: %v", err)
	}
	before := env.sendRecordByChannel(t, ctx, d, ChannelEmail)
	if before == nil || before.Status != SendRecordStatusSucceeded {
		t.Fatalf("record after the first delivery = %+v, want succeeded", before)
	}

	// The double-send window's loser: a later attempt at the same key whose
	// settle adopted the succeeded row's id while it still said failed, and
	// settles its own failure only now, after the winner's succeeded settle
	// committed. failAndRetry is the transient-failure leg's own settle
	// call; the pre-set ID is the stale adoption.
	rec := env.svc.sendRecordFor(deliveryTenant, d, ChannelEmail)
	key, err := deriveDeliveryKey(deliveryTenant, d, ChannelEmail)
	if err != nil {
		t.Fatalf("deriveDeliveryKey: %v", err)
	}
	rec.IdempotencyKey = key
	rec.ID = before.ID
	cause := errors.New("smtp 550 relay denied")

	got := env.svc.failAndRetry(ctx, deliveryTenant, rec, cause)
	if !errors.Is(got, cause) {
		t.Fatalf("failAndRetry returned %v, want the cause itself -- the guard must not change the retry answer", got)
	}

	after := env.sendRecordByChannel(t, ctx, d, ChannelEmail)
	if after == nil {
		t.Fatal("the failed settle removed the record, want the succeeded row kept")
	}
	if after.Status != SendRecordStatusSucceeded {
		t.Errorf("a stale-adopted failed settle after the winner committed downgraded the record to %s (%q): the guard must be decided by the write itself, never by a probe that ran before the winner's commit", after.Status, after.Error)
	}
	if after.ID != before.ID || after.Error != before.Error ||
		after.DurationMs != before.DurationMs || !after.CreatedAt.Equal(before.CreatedAt) ||
		!after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("the refused settle rewrote the succeeded row: before = %+v, after = %+v", before, after)
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
	if rec.Error != failureReasonRenderFailed {
		t.Errorf("record error = %q, want the bounded render-failure classification %q", rec.Error, failureReasonRenderFailed)
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
		if rec.Error != failureReasonResolutionFailed {
			t.Errorf("channel %s record error = %q, want the bounded resolution-failure classification %q", channel, rec.Error, failureReasonResolutionFailed)
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

// setupTestMeterProvider installs, as OTel's global MeterProvider for the
// duration of the test, a real SDK MeterProvider backed by a ManualReader --
// never a Prometheus/OTLP exporter, since this file only needs to read back
// exactly what was recorded -- mirroring
// go/jobs/standalone_queue_test.go's own helper of the same name. Must be
// called BEFORE newDeliveryEnv(t) so registerDeliveryMetrics (run from
// newDeliveryService) registers its instruments against this provider
// rather than the process-global one another test may have already
// installed; see that test file's own collectMetric doc comment for why a
// late otel.SetMeterProvider still works; this file simply avoids relying
// on it.
func setupTestMeterProvider(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	otel.SetMeterProvider(mp)
	return reader
}

// collectMetric runs a fresh Collect and returns the single metric named
// name, failing the test if it is missing -- name is always one of
// deliveryCountMetricName/deliveryDurationMetricName.
func collectMetric(t *testing.T, reader *sdkmetric.ManualReader, name string) metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() error = %v, want nil", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	var got []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			got = append(got, m.Name)
		}
	}
	t.Fatalf("metric %q not found; metrics present: %v", name, got)
	return metricdata.Metrics{}
}

// deliveryAttrString reads key out of attrs as a plain string, for comparing
// against a metric data point's own Attributes.
func deliveryAttrString(attrs attribute.Set, key string) string {
	v, _ := attrs.Value(attribute.Key(key))
	return v.AsString()
}

// deliveryCounterValue returns the int64 Sum value of m's data point labeled
// exactly by typeKey/channel/status, failing the test if m is not a
// Sum[int64] or no matching data point exists.
func deliveryCounterValue(t *testing.T, m metricdata.Metrics, typeKey, channel, status string) int64 {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %q Data = %T, want metricdata.Sum[int64]", m.Name, m.Data)
	}
	for _, dp := range sum.DataPoints {
		if deliveryAttrString(dp.Attributes, "type_key") != typeKey {
			continue
		}
		if deliveryAttrString(dp.Attributes, "channel") != channel {
			continue
		}
		if deliveryAttrString(dp.Attributes, "status") != status {
			continue
		}
		return dp.Value
	}
	t.Fatalf("metric %q has no data point for type_key=%q channel=%q status=%q", m.Name, typeKey, channel, status)
	return 0
}

// deliveryHistogramCount returns the observation Count of m's data point
// labeled exactly by typeKey/channel/status, failing the test if m is not a
// Histogram[float64] or no matching data point exists.
func deliveryHistogramCount(t *testing.T, m metricdata.Metrics, typeKey, channel, status string) uint64 {
	t.Helper()
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("metric %q Data = %T, want metricdata.Histogram[float64]", m.Name, m.Data)
	}
	for _, dp := range hist.DataPoints {
		if deliveryAttrString(dp.Attributes, "type_key") == typeKey &&
			deliveryAttrString(dp.Attributes, "channel") == channel &&
			deliveryAttrString(dp.Attributes, "status") == status {
			return dp.Count
		}
	}
	t.Fatalf("metric %q has no data point for type_key=%q channel=%q status=%q; data points: %+v", m.Name, typeKey, channel, status, hist.DataPoints)
	return 0
}

// TestDelivery_MetricsRecordCountAndDurationByChannelAndStatus proves
// DeliveryService emits the notification domain's metrics -- per-channel
// delivery success rate, latency and (derived from the same counter's
// status attribute) bounce rate -- rather than only writing send_records.
// Without registerDeliveryMetrics, both collectMetric calls below would
// fail with "metric ... not found", which is the negative control this
// test relies on.
//
// One delivery whose email channel fails permanently while its SMS and
// in-app channels succeed exercises both outcomes settle can reach in a
// single attempt, across all three channel labels.
func TestDelivery_MetricsRecordCountAndDurationByChannelAndStatus(t *testing.T) {
	reader := setupTestMeterProvider(t)
	env := newDeliveryEnv(t)
	env.resolver.byUser[deliveryUser] = deliveryAddresses
	d := deliveryDispatch()
	payload := env.enqueue(t, d)

	env.host.mailer.failWith = fmt.Errorf("550 mailbox unavailable: %w", ErrTransportPermanent)
	if err := env.attempt(t, payload); err != nil {
		t.Fatalf("attempt with a permanent transport failure returned %v, want nil (stop, not retry)", err)
	}

	count := collectMetric(t, reader, deliveryCountMetricName)
	if got := deliveryCounterValue(t, count, d.TypeKey, ChannelEmail, SendRecordStatusFailed); got != 1 {
		t.Errorf("%s{type_key=%s,channel=email,status=failed} = %d, want 1", deliveryCountMetricName, d.TypeKey, got)
	}
	if got := deliveryCounterValue(t, count, d.TypeKey, ChannelSMS, SendRecordStatusSucceeded); got != 1 {
		t.Errorf("%s{type_key=%s,channel=sms,status=succeeded} = %d, want 1", deliveryCountMetricName, d.TypeKey, got)
	}
	if got := deliveryCounterValue(t, count, d.TypeKey, ChannelInApp, SendRecordStatusSucceeded); got != 1 {
		t.Errorf("%s{type_key=%s,channel=in_app,status=succeeded} = %d, want 1", deliveryCountMetricName, d.TypeKey, got)
	}

	duration := collectMetric(t, reader, deliveryDurationMetricName)
	if got := deliveryHistogramCount(t, duration, d.TypeKey, ChannelEmail, SendRecordStatusFailed); got != 1 {
		t.Errorf("%s{type_key=%s,channel=email,status=failed} count = %d, want 1", deliveryDurationMetricName, d.TypeKey, got)
	}
	if got := deliveryHistogramCount(t, duration, d.TypeKey, ChannelSMS, SendRecordStatusSucceeded); got != 1 {
		t.Errorf("%s{type_key=%s,channel=sms,status=succeeded} count = %d, want 1", deliveryDurationMetricName, d.TypeKey, got)
	}
}

// TestRegisterDeliveryMetrics_Smoke is registerDeliveryMetrics's own
// equivalent of go/jobs/standalone_queue_test.go's
// TestRegisterJobMetrics_Smoke: registration alone (no delivery ever
// attempted) must not error or panic.
func TestRegisterDeliveryMetrics_Smoke(t *testing.T) {
	count, duration := registerDeliveryMetrics()
	if count == nil || duration == nil {
		t.Fatalf("registerDeliveryMetrics() = (%v, %v), want two non-nil instruments", count, duration)
	}
}

// TestDelivery_RetriedAttemptKeepsCreatedAtAndTimeBoundedAuditVisibility pins
// the audit-side consequence of a re-settled delivery at full settle depth:
// a real failed-then-retried email delivery under one idempotency key -- the
// exact shape the queue's retry runs when a transport recovers. The retry's
// guarded upgrade writes the record in place, and that rewrite must leave
// created_at at the FIRST attempt's instant (delivery.go's settle adopts
// only the existing row's id onto a freshly built, zero-CreatedAt record, so
// the guarded UPDATE would otherwise write year 1 into created_at -- see the
// SendRecordRepository.SaveGuarded regression test). The instant matters to
// the operator: the record must still answer ListByFilter's time-bounded
// From/To search around when the delivery was first attempted.
func TestDelivery_RetriedAttemptKeepsCreatedAtAndTimeBoundedAuditVisibility(t *testing.T) {
	env := newDeliveryEnv(t)
	env.resolver.byUser[deliveryUser] = deliveryAddresses
	ctx := tenantCtx(deliveryTenant)
	d := deliveryDispatch()
	payload := env.enqueue(t, d)

	env.host.mailer.failWith = errors.New("smtp: connection refused")
	if err := env.attempt(t, payload); err == nil {
		t.Fatal("attempt with a failing transport succeeded, want the retryable error back")
	}

	first := env.sendRecordByChannel(t, ctx, d, ChannelEmail)
	if first == nil || first.Status != SendRecordStatusFailed {
		t.Fatalf("email record after the failed attempt = %+v, want failed", first)
	}
	createdAt := first.CreatedAt
	if createdAt.IsZero() {
		t.Fatal("the failed attempt's settle left created_at at the zero value, test premise broken")
	}

	// The transport recovers; the queue retries the same job under the same
	// derived key.
	env.host.mailer.failWith = nil
	if err := env.attempt(t, payload); err != nil {
		t.Fatalf("retried attempt: %v", err)
	}

	got := env.sendRecordByChannel(t, ctx, d, ChannelEmail)
	if got == nil || got.Status != SendRecordStatusSucceeded {
		t.Fatalf("email record after the retry = %+v, want succeeded", got)
	}
	if !got.CreatedAt.Equal(createdAt) {
		t.Errorf("created_at after the retried settle = %v (zero = %v), want the first attempt's %v unchanged", got.CreatedAt, got.CreatedAt.IsZero(), createdAt)
	}

	page, err := env.svc.sendRecs.ListByFilter(ctx, SendRecordFilter{
		TenantID: deliveryTenant,
		Channel:  ChannelEmail,
		From:     createdAt.Add(-time.Second),
		To:       createdAt.Add(time.Second),
		Limit:    50,
	})
	if err != nil {
		t.Fatalf("ListByFilter(time-bounded): %v", err)
	}
	if len(page) != 1 || page[0].ID != got.ID {
		t.Errorf("time-bounded ListByFilter after the retry = %+v, want the retried delivery's one record (a zeroed created_at is invisible to From/To)", page)
	}
}

// TestDelivery_RepeatedDispatchOfOneDelivery_SettlesOneRecord pins the
// dedupe contract the resend regressions below rest on: dispatching the
// SAME dispatch twice is ONE delivery -- the queue's own retry of a job
// re-runs an identical payload, and the derived key must keep that retry
// from double-sending. A deliberate resend is therefore a DIFFERENT
// dispatch, and the module must give the caller a way to say so (see
// TestDelivery_ResendAfterALocaleChange_DeliversInTheNewLocale and the
// per-occurrence marker test).
func TestDelivery_RepeatedDispatchOfOneDelivery_SettlesOneRecord(t *testing.T) {
	env := newDeliveryEnv(t)
	env.resolver.byUser[deliveryUser] = deliveryAddresses
	ctx := tenantCtx(deliveryTenant)
	d := deliveryDispatch()

	if err := env.dispatchAndAttempt(t, d); err != nil {
		t.Fatalf("first delivery attempt: %v", err)
	}
	if err := env.dispatchAndAttempt(t, d); err != nil {
		t.Fatalf("second delivery attempt: %v", err)
	}

	if mails := env.host.mailer.messages(); len(mails) != 1 {
		t.Errorf("mailer sent %d messages for two dispatches of one delivery, want the single send", len(mails))
	}
	rec := env.sendRecordByChannel(t, ctx, d, ChannelEmail)
	if rec == nil || rec.Status != SendRecordStatusSucceeded {
		t.Fatalf("email record = %+v, want the one succeeded record", rec)
	}
}

// TestDelivery_ResendWithAFreshOccurrenceID_DeliversAgain pins the
// occurrence half of the delivery key's contract: a DELIBERATE resend --
// identical type, recipient, channel, locale and parameters, re-dispatched
// under a fresh OccurrenceID -- is a new delivery, not the old one's
// replay. The derived key hashes the occurrence marker, so each resend
// settles its own send record and sends again; without the marker a resend
// is indistinguishable from the queue's own retry of the old job and is
// correctly swallowed (TestDelivery_RepeatedDispatchOfOneDelivery_SettlesOneRecord
// pins that side).
func TestDelivery_ResendWithAFreshOccurrenceID_DeliversAgain(t *testing.T) {
	env := newDeliveryEnv(t)
	env.resolver.byUser[deliveryUser] = deliveryAddresses
	ctx := tenantCtx(deliveryTenant)

	first := deliveryDispatch()
	first.OccurrenceID = "occurrence-1"
	if err := env.dispatchAndAttempt(t, first); err != nil {
		t.Fatalf("first delivery attempt: %v", err)
	}

	// The identical content, deliberately re-dispatched: the demo-shape
	// "the patient asked again" occurrence. The rendered copy is identical;
	// only the occurrence is new.
	resend := deliveryDispatch()
	resend.OccurrenceID = "occurrence-2"
	if err := env.dispatchAndAttempt(t, resend); err != nil {
		t.Fatalf("resend attempt: %v", err)
	}

	mails := env.host.mailer.messages()
	if len(mails) != 2 {
		t.Fatalf("mailer sent %d messages, want 2: the original delivery and the deliberate resend", len(mails))
	}
	if mails[0].To[0] != deliveryAddresses.Email || mails[1].To[0] != deliveryAddresses.Email {
		t.Errorf("mail recipients = %v then %v, want the resolved address for both deliveries", mails[0].To, mails[1].To)
	}

	firstRec := env.sendRecordByChannel(t, ctx, first, ChannelEmail)
	resendRec := env.sendRecordByChannel(t, ctx, resend, ChannelEmail)
	if firstRec == nil || firstRec.Status != SendRecordStatusSucceeded {
		t.Fatalf("first delivery record = %+v, want succeeded", firstRec)
	}
	if resendRec == nil || resendRec.Status != SendRecordStatusSucceeded {
		t.Fatalf("resend record = %+v, want its own succeeded record", resendRec)
	}
	if firstRec.ID == resendRec.ID {
		t.Errorf("the resend adopted the original delivery's record id %s, want one record per occurrence", firstRec.ID)
	}
	if firstRec.IdempotencyKey == resendRec.IdempotencyKey {
		t.Errorf("the resend derived the original delivery's key %s, want a distinct key per occurrence", firstRec.IdempotencyKey)
	}
}

// TestDelivery_ResendAfterALocaleChange_DeliversInTheNewLocale pins the
// locale half of the delivery key's contract: the copy is rendered in the
// recipient's locale, so a resend whose locale changed is NOT the delivery
// the record already logs -- it must deliver again, in the new locale,
// rather than be swallowed by the dedupe probe as the old delivery's
// replay. A delivery key that leaves the locale out of its derivation
// cannot tell the two dispatches apart.
func TestDelivery_ResendAfterALocaleChange_DeliversInTheNewLocale(t *testing.T) {
	env := newDeliveryEnv(t)
	env.resolver.byUser[deliveryUser] = deliveryAddresses

	zh := deliveryDispatch() // the fixture's zh-CN copy
	if err := env.dispatchAndAttempt(t, zh); err != nil {
		t.Fatalf("zh-CN delivery attempt: %v", err)
	}
	en := deliveryDispatch()
	en.Locale = "en-US"
	if err := env.dispatchAndAttempt(t, en); err != nil {
		t.Fatalf("en-US delivery attempt: %v", err)
	}

	mails := env.host.mailer.messages()
	if len(mails) != 2 {
		t.Fatalf("mailer sent %d messages, want 2: the zh-CN delivery and the en-US resend", len(mails))
	}
	if mails[0].Subject != "预约提醒" || mails[1].Subject != "Appointment reminder" {
		t.Errorf("mail subjects = %q then %q, want the zh-CN copy then the en-US copy", mails[0].Subject, mails[1].Subject)
	}
	rec := env.sendRecordByChannel(t, tenantCtx(deliveryTenant), en, ChannelEmail)
	if rec == nil || rec.Status != SendRecordStatusSucceeded {
		t.Fatalf("en-US resend record = %+v, want its own succeeded record under its own key", rec)
	}
}

// TestDelivery_GatewayEchoingTheRecipientAddress_NeverReachesTheStoredRecord
// pins the send-record PII rule in its ordinary shape: a transport error
// that quotes the recipient's address (an SMTP 5xx names the mailbox it
// rejected) is what a delivery failure returns, and the send_records.error
// column -- the text the operator send-record search reads back -- must never
// store it. The module stores no transport text at all: the failed attempt
// carries the bounded classification, and not even the exact form of the
// address the module handed the transport appears anywhere (the
// non-normalized echo -- the form no caller-side guard could predict -- is
// the regression test further below).
func TestDelivery_GatewayEchoingTheRecipientAddress_NeverReachesTheStoredRecord(t *testing.T) {
	env := newDeliveryEnv(t)
	env.resolver.byUser[deliveryUser] = deliveryAddresses
	ctx := tenantCtx(deliveryTenant)
	d := deliveryDispatch()

	env.host.mailer.failWith = fmt.Errorf("smtp: 550 %s: recipient address rejected", deliveryAddresses.Email)
	if err := env.dispatchAndAttempt(t, d); err == nil {
		t.Fatal("attempt with the refusing gateway succeeded, want the retryable error back")
	}

	rec := env.sendRecordByChannel(t, ctx, d, ChannelEmail)
	if rec == nil || rec.Status != SendRecordStatusFailed {
		t.Fatalf("email record after the refusal = %+v, want failed", rec)
	}
	if rec.Error != failureReasonTransportFailed {
		t.Errorf("record error = %q, want the bounded transient-transport classification %q (no transport text stored)", rec.Error, failureReasonTransportFailed)
	}
	if strings.Contains(rec.Error, deliveryAddresses.Email) {
		t.Errorf("record error carries the plaintext address: %q", rec.Error)
	}
}

// TestDelivery_ContactBounceCarryingTheAddress_StoredRecordAndReadbackNeverCarryIt
// is the external-contact half of the PII rule: a permanent gateway refusal
// quoting the contact's address settles a failed record (and marks the
// contact bounced), and neither the record's own error text nor the rows
// the operator search (ListByFilter) returns may contain the address
// -- the record carries only the bounded permanent-transport
// classification, whatever form the gateway's echo took.
func TestDelivery_ContactBounceCarryingTheAddress_StoredRecordAndReadbackNeverCarryIt(t *testing.T) {
	env := newDeliveryEnv(t)
	ctx := tenantCtx(deliveryTenant)

	const address = "wangfang@external.example.com"
	contact, err := env.contacts.CreateContact(ctx, ContactCreateInput{
		Channel:    ChannelEmail,
		Address:    address,
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
	env.host.mailer.failWith = fmt.Errorf("smtp: 550 <%s>: mailbox unavailable: %w", address, ErrTransportPermanent)
	if attemptErr := env.dispatchAndAttempt(t, d); attemptErr != nil {
		t.Fatalf("delivery attempt: %v", attemptErr)
	}

	rec := env.sendRecordByChannel(t, ctx, d, ChannelEmail)
	if rec == nil || rec.Status != SendRecordStatusFailed {
		t.Fatalf("email record after the permanent refusal = %+v, want failed", rec)
	}
	if rec.Error != failureReasonTransportRefused {
		t.Errorf("record error = %q, want the bounded permanent-transport classification %q (no transport text stored)", rec.Error, failureReasonTransportRefused)
	}
	if strings.Contains(rec.Error, address) {
		t.Errorf("record error carries the plaintext contact address: %q", rec.Error)
	}

	rows, err := env.svc.sendRecs.ListByFilter(ctx, SendRecordFilter{TenantID: deliveryTenant, Limit: 50})
	if err != nil {
		t.Fatalf("ListByFilter: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListByFilter returned %d rows, want the one failed record", len(rows))
	}
	if rows[0].Error != failureReasonTransportRefused {
		t.Errorf("the operator readback = %q, want the bounded permanent-transport classification %q", rows[0].Error, failureReasonTransportRefused)
	}
	if strings.Contains(rows[0].Error, address) {
		t.Errorf("the operator readback carries the plaintext contact address: %q", rows[0].Error)
	}
	row, err := env.contacts.repo.FindByID(ctx, contact.ID)
	if err != nil {
		t.Fatalf("FindByID after the refusal: %v", err)
	}
	if row.Status != ContactStatusBounced {
		t.Errorf("contact status after the permanent refusal = %s, want bounced", row.Status)
	}
}

// TestDelivery_TransportEchoingTheAddressInANonNormalizedForm_NeverReachesTheStoredRecord
// pins the bounded-classification shape's reason for existing. The module
// hands a transport the normalized form of the recipient's address (a
// lowercased email, an E.164 phone), while the transport echoes the
// mailbox it rejected in whatever form IT chose -- an uppercase rendering
// of the email, a plus-less MSISDN. No transport text is stored at all:
// the failed record carries the bounded classification, and a raw echo
// cannot reach the column in ANY form, normalized or not -- an echo in the
// column would plant plaintext PII in a platform table with no deletion
// path.
//
// The classification is asserted as its literal text ("transport refused",
// the value of the module's unexported failureReasonTransportRefused
// constant), not through the constant itself, so the assertion does not
// share its data with the implementation it checks.
func TestDelivery_TransportEchoingTheAddressInANonNormalizedForm_NeverReachesTheStoredRecord(t *testing.T) {
	const refused = "transport refused"

	t.Run("uppercase email echo", func(t *testing.T) {
		env := newDeliveryEnv(t)
		env.resolver.byUser[deliveryUser] = deliveryAddresses
		ctx := tenantCtx(deliveryTenant)
		d := deliveryDispatch()

		echo := strings.ToUpper(deliveryAddresses.Email)
		env.host.mailer.failWith = fmt.Errorf("smtp: 550 <%s>: mailbox unavailable: %w", echo, ErrTransportPermanent)
		if attemptErr := env.dispatchAndAttempt(t, d); attemptErr != nil {
			t.Fatalf("permanent refusal returned %v, want the terminal stop", attemptErr)
		}

		rec := env.sendRecordByChannel(t, ctx, d, ChannelEmail)
		if rec == nil || rec.Status != SendRecordStatusFailed {
			t.Fatalf("email record after the refusal = %+v, want failed", rec)
		}
		if strings.Contains(rec.Error, deliveryAddresses.Email) || strings.Contains(rec.Error, echo) {
			t.Errorf("record error carries the plaintext address: %q", rec.Error)
		}
		if rec.Error != refused {
			t.Errorf("record error = %q, want the bounded classification %q (no transport text stored)", rec.Error, refused)
		}

		rows, err := env.svc.sendRecs.ListByFilter(ctx, SendRecordFilter{TenantID: deliveryTenant, Status: SendRecordStatusFailed, Limit: 50})
		if err != nil {
			t.Fatalf("ListByFilter: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("ListByFilter returned %d failed rows, want the one failed email record", len(rows))
		}
		if rows[0].Error != refused || strings.Contains(rows[0].Error, echo) {
			t.Errorf("the operator readback = %q, want the bounded classification alone", rows[0].Error)
		}
	})

	t.Run("plus-less phone echo", func(t *testing.T) {
		env := newDeliveryEnv(t)
		env.resolver.byUser[deliveryUser] = deliveryAddresses
		ctx := tenantCtx(deliveryTenant)
		d := deliveryDispatch()

		echo := strings.TrimPrefix(deliveryAddresses.Phone, "+")
		env.sms.failWith = fmt.Errorf("gateway: 550 %s: invalid number: %w", echo, ErrTransportPermanent)
		if attemptErr := env.dispatchAndAttempt(t, d); attemptErr != nil {
			t.Fatalf("permanent refusal returned %v, want the terminal stop", attemptErr)
		}

		rec := env.sendRecordByChannel(t, ctx, d, ChannelSMS)
		if rec == nil || rec.Status != SendRecordStatusFailed {
			t.Fatalf("SMS record after the refusal = %+v, want failed", rec)
		}
		if strings.Contains(rec.Error, deliveryAddresses.Phone) || strings.Contains(rec.Error, echo) {
			t.Errorf("record error carries the plaintext address: %q", rec.Error)
		}
		if rec.Error != refused {
			t.Errorf("record error = %q, want the bounded classification %q (no transport text stored)", rec.Error, refused)
		}
	})
}

// TestDelivery_ContactVerifiedOnAnUnknownChannel_RecordsAndStops pins the
// corrupt-row path of the contact delivery: a verified contact whose
// channel no transport can serve (impossible through CreateContact's
// validation, reachable only by a row written around it) must settle as a
// recorded, terminal failure -- the record the operator reads, the job
// NOT walked through the retry-and-dead-letter horizon -- never a bare
// error that retries pointlessly and logs nothing.
func TestDelivery_ContactVerifiedOnAnUnknownChannel_RecordsAndStops(t *testing.T) {
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
	// Corrupt the row the way only a writer around the module's own
	// validation could: a channel outside the transport vocabulary.
	row, err := env.contacts.repo.FindByID(ctx, contact.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	row.Channel = "carrier-pigeon"
	if updateErr := env.contacts.repo.Update(ctx, row); updateErr != nil {
		t.Fatalf("corrupt the contact's channel: %v", updateErr)
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
	if attemptErr := env.dispatchAndAttempt(t, d); attemptErr != nil {
		t.Fatalf("delivery attempt on the corrupt channel returned %v, want the terminal stop (no queue retry)", attemptErr)
	}
	if mails := env.host.mailer.messages(); len(mails) != 0 {
		t.Errorf("mailer sent %d messages on an unknown channel, want none", len(mails))
	}

	key, err := deriveDeliveryKey(deliveryTenant, d, "carrier-pigeon")
	if err != nil {
		t.Fatalf("deriveDeliveryKey: %v", err)
	}
	rec, err := env.svc.sendRecs.ByTenantAndKey(ctx, deliveryTenant, key)
	if err != nil {
		t.Fatalf("ByTenantAndKey: %v", err)
	}
	if rec == nil {
		t.Fatal("no send record for the unknown-channel delivery, want the terminal refusal recorded")
	}
	if rec.Status != SendRecordStatusFailed {
		t.Errorf("record status = %s, want %s", rec.Status, SendRecordStatusFailed)
	}
}

// TestDelivery_ContactDeliveryOfAnUndeclaredType_NeverReachesTheTransport
// pins the type-registry gate on the external-contact delivery path: an
// undeclared type must be refused before anything renders or sends, even
// when the merged catalog carries the type's copy -- the harmful half of a
// module that ships locale template resources but forgets
// reg.Notifications.Add -- because a message that went out would bypass
// the preference matrix, the unsubscribe decision and the declared default
// channel strategy the declaration owns. The refusal is terminal and
// recorded under the contact's own channel; declaring the type restores
// ordinary delivery.
func TestDelivery_ContactDeliveryOfAnUndeclaredType_NeverReachesTheTransport(t *testing.T) {
	env := newDeliveryEnv(t)
	ctx := tenantCtx(deliveryTenant)

	// Widen the env's catalog with the undeclared module's own bundle; the
	// env's taxonomy (fixtureTypes, attached in newDeliveryEnv) never
	// declares ghostContactType.
	builder := i18n.NewBuilder()
	if err := builder.AddModule("clinic", clinicFixtureFS); err != nil {
		t.Fatalf("AddModule(clinic): %v", err)
	}
	if err := builder.AddModule("clinic-billing", billReadyFixtureFS); err != nil {
		t.Fatalf("AddModule(clinic-billing): %v", err)
	}
	env.host.catalog = builder.Build()

	contact, err := env.contacts.CreateContact(ctx, ContactCreateInput{
		Channel:    ChannelEmail,
		Address:    "wangfang@external.example.com",
		ConsentRef: "consent-ref-1",
	})
	if err != nil {
		t.Fatalf("create the verified contact: %v", err)
	}

	d := Dispatch{
		TypeKey: ghostContactType,
		Recipient: DispatchRecipient{
			Class:     RecipientClassExternal,
			ContactID: contact.ID,
		},
		Locale: "zh-CN",
		Params: renderTestParams,
	}

	// Leg one: the type is undeclared. The attempt must stop terminally
	// (no error for the queue to retry), record the refusal as a failed
	// send, and never touch the transport.
	if attemptErr := env.dispatchAndAttempt(t, d); attemptErr != nil {
		t.Fatalf("attempt for the undeclared type returned %v, want the terminal recorded stop (no queue retry)", attemptErr)
	}
	if mails := env.host.mailer.messages(); len(mails) != 0 {
		t.Errorf("the undeclared type's delivery reached the mailer (%d messages), want the registry refusal before any transport", len(mails))
	}
	rec := env.sendRecordByChannel(t, ctx, d, ChannelEmail)
	if rec == nil {
		t.Fatal("no send record for the undeclared type's attempt, want the terminal refusal recorded")
	}
	if rec.Status != SendRecordStatusFailed {
		t.Errorf("record status = %s, want %s", rec.Status, SendRecordStatusFailed)
	}
	if rec.Error != failureReasonTypeUndeclared {
		t.Errorf("record error = %q, want the bounded undeclared-type classification %q", rec.Error, failureReasonTypeUndeclared)
	}

	// Leg two: the module declares its type; the same delivery renders and
	// sends exactly as any declared type's does. The dispatch now carries
	// only the parameter the declared copy actually references (the bill
	// email renders {{.patient_name}} alone) -- appointment_time is
	// copy-inert for this copy, and a declared type's dispatch carrying a
	// parameter no template references is refused at the enqueue boundary
	// under the copy gate, exactly as the bill's own writer would learn.
	ghost := pkgcore.NotificationType{
		Key:             ghostContactType,
		Group:           "billing",
		DefaultChannels: []string{ChannelEmail},
		Unsubscribable:  true,
	}
	env.prefs.attachTypes(fixtureRegistrar{types: append(slices.Clone(fixtureTypes), ghost)})
	declared := d
	declared.Params = maps.Clone(d.Params)
	delete(declared.Params, "appointment_time")
	if attemptErr := env.dispatchAndAttempt(t, declared); attemptErr != nil {
		t.Fatalf("attempt for the now-declared type: %v", attemptErr)
	}
	if mails := env.host.mailer.messages(); len(mails) != 1 {
		t.Errorf("mailer delivered %d messages, want exactly the declared type's 1", len(mails))
	}
	rec = env.sendRecordByChannel(t, ctx, declared, ChannelEmail)
	if rec == nil || rec.Status != SendRecordStatusSucceeded {
		t.Fatalf("record after the declared delivery = %+v, want succeeded", rec)
	}
}

// annotatedAppointmentType is the fixture appointment declaration carrying
// the recipient-visible-params annotation: it declares exactly the two
// names its templates interpolate (render_test.go's renderTestParams), the
// way a declaring business module states which parameters may reach its
// recipients (pkgcore.NotificationType.RecipientVisibleParams -- the
// recipient-visible gate's contract, whose owner is the type's own
// declaration).
var annotatedAppointmentType = pkgcore.NotificationType{
	Key:                    fixtureTypeAppointment,
	Group:                  "appointments",
	DefaultChannels:        []string{ChannelInApp, ChannelEmail, ChannelSMS},
	Unsubscribable:         true,
	RecipientVisibleParams: []string{"patient_name", "appointment_time"},
}

// TestDelivery_Dispatch_RefusesParamsOutsideRecipientVisibleDeclaration is
// the allowlist-governance test at the enqueue boundary: once a type's
// declaration states which parameters may reach its recipient, a dispatch
// carrying anything else is refused with ErrDispatchParamsNotAllowed --
// naming the type and the sorted offending keys -- before anything is
// enqueued. The refusal is structural rather than a matter of caller
// memory: admin's impersonation notice declares the EMPTY list (go/admin
// module.go), so internal context (an operator's free-text reason, an
// administrator's user id) cannot even be dispatched for it, and every
// type's params surface is decided by declaration, never by whatever a
// dispatch happens to carry.
func TestDelivery_Dispatch_RefusesParamsOutsideRecipientVisibleDeclaration(t *testing.T) {
	env := newDeliveryEnv(t)
	env.prefs.attachTypes(fixtureRegistrar{types: []pkgcore.NotificationType{annotatedAppointmentType}})
	ctx := tenantCtx(deliveryTenant)

	d := deliveryDispatch()
	d.Params = maps.Clone(renderTestParams)
	d.Params["reason"] = "investigating suspected fraud on this account"
	d.Params["admin_user_id"] = "admin-1"
	_, err := env.svc.Dispatch(ctx, d)
	appErr, ok := apperr.As(err)
	if !ok || appErr.Code != ErrDispatchParamsNotAllowed.Code {
		t.Fatalf("Dispatch() error = %v, want %s", err, ErrDispatchParamsNotAllowed.Code)
	}
	if got := appErr.Params["type_key"]; got != fixtureTypeAppointment {
		t.Errorf("refusal type_key = %v, want %q", got, fixtureTypeAppointment)
	}
	if got := appErr.Params["params"]; got != "admin_user_id,reason" {
		t.Errorf("refusal params = %v, want the sorted offending keys %q", got, "admin_user_id,reason")
	}
	if len(env.queue.tasks) != 0 {
		t.Errorf("queue holds %d tasks after the refused dispatch, want none", len(env.queue.tasks))
	}

	// The declared parameters themselves still dispatch, enqueue and
	// deliver untouched: the allowlist governs both directions.
	if err := env.dispatchAndAttempt(t, deliveryDispatch()); err != nil {
		t.Fatalf("dispatch of the declared parameters: %v", err)
	}
}

// TestDelivery_StalePayloadParams_NarrowedBeforeRowAndKey is the
// allowlist-governance test at the persistence boundary: a delivery job
// whose payload carries a parameter outside the type's declaration -- a
// payload whose enqueue path bypassed the boundary check, which Dispatch's
// own refusal never saw -- must still deliver (the notice is not lost), but
// nothing beyond the declaration may derive into the delivery key or
// persist into the inbox row. The row is the recipient-visible surface (the
// inbox API serves it back as it stands), so the narrowing -- delivery.go's
// recipientVisibleOnly, applied to every payload that reaches the delivery
// path -- must hold for every payload the path sees, not only for
// dispatches validated at the enqueue boundary.
func TestDelivery_StalePayloadParams_NarrowedBeforeRowAndKey(t *testing.T) {
	env := newDeliveryEnv(t)
	env.prefs.attachTypes(fixtureRegistrar{types: []pkgcore.NotificationType{annotatedAppointmentType}})
	env.resolver.byUser[deliveryUser] = deliveryAddresses
	ctx := tenantCtx(deliveryTenant)

	stale := deliveryDispatch()
	stale.Params = maps.Clone(renderTestParams)
	stale.Params["reason"] = "investigating suspected fraud on this account"
	stale.Params["admin_user_id"] = "admin-1"
	// The payload shape that bypasses the enqueue gate: marshaled straight
	// into the queue, so Dispatch's refusal never saw it.
	payload, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("marshal stale payload: %v", err)
	}
	if err := env.attempt(t, payload); err != nil {
		t.Fatalf("attempt over the stale payload returned %v, want nil (the delivery converges; only the undeclared params are narrowed)", err)
	}

	// The delivery ran under the NARROWED payload: its derived key, send
	// record, inbox row and rendered copy all derive from the declared
	// parameters alone, so the row is looked up under the narrowed key.
	clean := env.svc.recipientVisibleOnly(stale)
	row := env.inboxRowByChannel(t, ctx, clean)
	if row == nil {
		t.Fatal("no inbox row for the narrowed stale payload, want the in-app delivery to succeed")
	}
	if len(row.Params) == 0 {
		t.Fatal("narrowed payload inbox row carries no params, want the declared ones persisted")
	}
	var got map[string]any
	if err := json.Unmarshal(row.Params, &got); err != nil {
		t.Fatalf("unmarshal row params: %v", err)
	}
	for _, forbidden := range []string{"reason", "admin_user_id"} {
		if _, ok := got[forbidden]; ok {
			t.Errorf("inbox row params carry %q, want it narrowed out of the recipient-visible row (the leak channel)", forbidden)
		}
	}
	for _, declared := range []string{"patient_name", "appointment_time"} {
		if _, ok := got[declared]; !ok {
			t.Errorf("inbox row params lack the declared parameter %q, want it preserved", declared)
		}
	}

	// Every outbound surface is clean: the email renders the declared
	// parameters and nothing of the internal context.
	mails := env.host.mailer.messages()
	if len(mails) != 1 {
		t.Fatalf("mailer delivered %d messages for the stale payload, want the one email", len(mails))
	}
	if !strings.Contains(mails[0].Text, "王芳") {
		t.Errorf("mail text = %q, want the declared parameter rendered into the copy", mails[0].Text)
	}
	if strings.Contains(mails[0].Text, "fraud") {
		t.Errorf("mail text %q embeds the internal reason, want it absent from every recipient-visible surface", mails[0].Text)
	}
}

// overDeclaredAppointmentType is a fixture appointment declaration whose
// recipient-visible annotation is WIDER than its own copy: it declares an
// extra recipient-visible name -- internal_marker -- that no template of
// the type references. Such a declaration is the general hole in the
// restricted-type gate: on a restricted type the declared list is the whole
// gate, so a value no copy anywhere uses would be allowed to ride verbatim
// into the inbox row and API. Copy governance must refuse it regardless of
// what the declaration says.
var overDeclaredAppointmentType = pkgcore.NotificationType{
	Key:                    fixtureTypeAppointment,
	Group:                  "appointments",
	DefaultChannels:        []string{ChannelInApp, ChannelEmail, ChannelSMS},
	Unsubscribable:         true,
	RecipientVisibleParams: []string{"patient_name", "appointment_time", "internal_marker"},
}

// TestDelivery_Dispatch_RefusesParamsNoTemplateReferences is the
// copy-governance test at the enqueue boundary: a dispatch whose Params
// carry a parameter name no copy template of the type references is
// refused with ErrDispatchParamsUnreferenced -- naming the type and the
// offending keys -- before anything is enqueued. The copy gate applies
// whether or not the type's declaration restricts its recipient-visible
// list: leg 1 drives the fixture's unrestricted declaration (nil
// RecipientVisibleParams, the shape on which the declaration gate has
// nothing to refuse, so the copy gate is the whole protection against a
// copy-inert parameter riding through), and leg 2 drives a restricted
// declaration that itself lists the unreferenced parameter -- the
// annotation's word is not the copy's, and a value no template uses must
// not reach the row even when a declaration says it may.
func TestDelivery_Dispatch_RefusesParamsNoTemplateReferences(t *testing.T) {
	// Leg 1: the unrestricted fixture type. The recipient-visible gate has
	// nothing to refuse (nil list = unrestricted), so a copy-inert
	// parameter must be refused by the copy gate alone.
	env := newDeliveryEnv(t)
	ctx := tenantCtx(deliveryTenant)

	d := deliveryDispatch()
	d.Params = maps.Clone(renderTestParams)
	d.Params["internal_marker"] = "occurrence-42"
	_, err := env.svc.Dispatch(ctx, d)
	appErr, ok := apperr.As(err)
	if !ok || appErr.Code != ErrDispatchParamsUnreferenced.Code {
		t.Fatalf("Dispatch() error = %v, want %s", err, ErrDispatchParamsUnreferenced.Code)
	}
	if got := appErr.Params["type_key"]; got != fixtureTypeAppointment {
		t.Errorf("refusal type_key = %v, want %q", got, fixtureTypeAppointment)
	}
	if got := appErr.Params["params"]; got != "internal_marker" {
		t.Errorf("refusal params = %v, want the offending key %q", got, "internal_marker")
	}
	if len(env.queue.tasks) != 0 {
		t.Errorf("queue holds %d tasks after the refused dispatch, want none", len(env.queue.tasks))
	}

	// The parameters the copy DOES reference still dispatch, enqueue and
	// deliver untouched: the copy gate governs both directions.
	if err := env.dispatchAndAttempt(t, deliveryDispatch()); err != nil {
		t.Fatalf("dispatch of the copy's own parameters: %v", err)
	}

	// Leg 2: a restricted declaration that itself admits the unreferenced
	// parameter. The recipient-visible gate passes it (the declaration
	// lists it), so only the copy gate stands between it and the row.
	declared := newDeliveryEnv(t)
	declared.prefs.attachTypes(fixtureRegistrar{types: []pkgcore.NotificationType{overDeclaredAppointmentType}})
	dd := deliveryDispatch()
	dd.Params = maps.Clone(renderTestParams)
	dd.Params["internal_marker"] = "occurrence-42"
	_, derr := declared.svc.Dispatch(ctx, dd)
	dappErr, dok := apperr.As(derr)
	if !dok || dappErr.Code != ErrDispatchParamsUnreferenced.Code {
		t.Fatalf("Dispatch() over a declaration that lists the unreferenced parameter = %v, want %s -- the copy gate must not defer to the declaration", derr, ErrDispatchParamsUnreferenced.Code)
	}
	if got := dappErr.Params["params"]; got != "internal_marker" {
		t.Errorf("refusal params = %v, want the offending key %q", got, "internal_marker")
	}
	if len(declared.queue.tasks) != 0 {
		t.Errorf("queue holds %d tasks after the refused dispatch, want none", len(declared.queue.tasks))
	}
}

// TestDelivery_StalePayloadParams_UnreferencedParam_DroppedBeforeRowAndKey
// is the copy-governance test at the persistence boundary: a delivery job
// whose payload carries a parameter no template of the type references --
// a payload whose enqueue path bypassed the copy gate, which Dispatch's
// own refusal never saw -- must still deliver (the message is not lost),
// but
// the copy-inert parameter must not derive into the delivery key and must
// not persist into the inbox row: the row stores exactly the parameters
// its own copy was rendered from, which is what the inbox API serves back.
func TestDelivery_StalePayloadParams_UnreferencedParam_DroppedBeforeRowAndKey(t *testing.T) {
	env := newDeliveryEnv(t)
	env.resolver.byUser[deliveryUser] = deliveryAddresses
	ctx := tenantCtx(deliveryTenant)

	stale := deliveryDispatch()
	stale.Params = maps.Clone(renderTestParams)
	stale.Params["internal_marker"] = "occurrence-42"
	// The payload shape that bypasses the copy gate: marshaled straight into
	// the queue, so Dispatch's refusal never saw it.
	payload, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("marshal stale payload: %v", err)
	}
	if err := env.attempt(t, payload); err != nil {
		t.Fatalf("attempt over the stale payload returned %v, want nil (the delivery converges; only the copy-inert parameter is dropped)", err)
	}

	// No delivery may exist under the marker-bearing key: a parameter no
	// copy renders must not distinguish the delivery (OccurrenceID is the
	// first-class field for that). Without the narrowing the delivery would
	// derive under exactly this key and the row under it would carry the
	// marker verbatim.
	full := deliveryDispatch()
	full.Params = maps.Clone(stale.Params)
	if row := env.inboxRowByChannel(t, ctx, full); row != nil {
		t.Fatalf("inbox row exists under the marker-bearing delivery key with params %s, want the copy-inert parameter kept out of the key derivation and the row", row.Params)
	}

	// The delivery itself landed under the copy's own parameters, and the
	// row persists exactly them -- nothing more.
	clean := deliveryDispatch()
	clean.Params = maps.Clone(renderTestParams)
	row := env.inboxRowByChannel(t, ctx, clean)
	if row == nil {
		t.Fatal("no inbox row under the copy-parameter delivery key, want the in-app delivery to succeed there")
	}
	if len(row.Params) == 0 {
		t.Fatal("inbox row carries no params, want the copy's own parameters persisted")
	}
	var got map[string]any
	if err := json.Unmarshal(row.Params, &got); err != nil {
		t.Fatalf("unmarshal row params: %v", err)
	}
	if !maps.Equal(got, renderTestParams) {
		t.Errorf("inbox row params = %v, want exactly the copy's own parameters %v -- a parameter no template references must not ride through into the recipient-visible row", got, renderTestParams)
	}

	// The outbound copy still renders the referenced parameters: the
	// dropped parameter changed nothing a recipient sees.
	mails := env.host.mailer.messages()
	if len(mails) != 1 {
		t.Fatalf("mailer delivered %d messages for the stale payload, want the one email", len(mails))
	}
	if !strings.Contains(mails[0].Text, "王芳") {
		t.Errorf("mail text = %q, want the referenced parameter rendered into the copy", mails[0].Text)
	}
}

// TestDelivery_TypeOptOutBetweenEnqueueAndAttempt_SkipsThatTypeOnlyAndKeepsOthers
// drives the type-scoped opt-out's freshness in the delivery's sharpest
// form: two dispatches are enqueued while the contact is verified -- one
// for fixtureTypeAppointment, one for fixtureTypeResult -- and the contact
// narrows itself out of fixtureTypeResult BEFORE the worker attempts run.
// The attempt of the opted-out type must settle a skipped record under the
// type-scoped reason without ever calling the transport, while the other
// type's attempt still delivers: the whole point of the shape is that one
// type's opt-out leaves the contact reachable for the rest.
func TestDelivery_TypeOptOutBetweenEnqueueAndAttempt_SkipsThatTypeOnlyAndKeepsOthers(t *testing.T) {
	env := newDeliveryEnv(t)
	ctx := tenantCtx(deliveryTenant)
	env.contacts.types = fixtureRegistrar{types: fixtureTypes}

	contact, err := env.contacts.CreateContact(ctx, ContactCreateInput{
		Channel:    ChannelEmail,
		Address:    "wangfang@external.example.com",
		ConsentRef: "consent-ref-1",
	})
	if err != nil {
		t.Fatalf("create the verified contact: %v", err)
	}

	optedOut := Dispatch{
		TypeKey: fixtureTypeResult,
		Recipient: DispatchRecipient{
			Class:     RecipientClassExternal,
			ContactID: contact.ID,
		},
		Locale: "zh-CN",
		Params: renderTestParams,
	}
	stillOn := Dispatch{
		TypeKey: fixtureTypeAppointment,
		Recipient: DispatchRecipient{
			Class:     RecipientClassExternal,
			ContactID: contact.ID,
		},
		Locale: "zh-CN",
		Params: renderTestParams,
	}
	optedOutPayload := env.enqueue(t, optedOut)
	stillOnPayload := env.enqueue(t, stillOn)

	if err := env.contacts.UnsubscribeType(ctx, UnsubscribeTypeInput{
		ContactID: contact.ID,
		TypeKey:   fixtureTypeResult,
	}); err != nil {
		t.Fatalf("UnsubscribeType: %v", err)
	}

	if err := env.attempt(t, optedOutPayload); err != nil {
		t.Fatalf("delivery attempt of the opted-out type: %v", err)
	}
	if err := env.attempt(t, stillOnPayload); err != nil {
		t.Fatalf("delivery attempt of the still-on type: %v", err)
	}

	mails := env.host.mailer.messages()
	if len(mails) != 1 {
		t.Fatalf("mailer sent %d messages, want exactly the still-on type's one email", len(mails))
	}
	if mails[0].Subject != "预约提醒" {
		t.Errorf("mail subject = %q, want the still-on type's appointment copy", mails[0].Subject)
	}

	skip := env.sendRecordByChannel(t, ctx, optedOut, ChannelEmail)
	if skip == nil {
		t.Fatal("no send record for the type-opted-out delivery, want the skip recorded")
	}
	if skip.Status != SendRecordStatusSkipped {
		t.Errorf("opted-out record status = %s, want %s", skip.Status, SendRecordStatusSkipped)
	}
	if skip.Error != skipReasonTypeUnsubscribed {
		t.Errorf("opted-out record reason = %q, want %q", skip.Error, skipReasonTypeUnsubscribed)
	}
	if skip.RecipientClass != RecipientClassExternal || skip.ContactID != contact.ID {
		t.Errorf("opted-out record recipient = (class %s, contact %q), want (external, %q)", skip.RecipientClass, skip.ContactID, contact.ID)
	}

	delivered := env.sendRecordByChannel(t, ctx, stillOn, ChannelEmail)
	if delivered == nil {
		t.Fatal("no send record for the still-on delivery, want the succeeded record")
	}
	if delivered.Status != SendRecordStatusSucceeded {
		t.Errorf("still-on record status = %s, want %s", delivered.Status, SendRecordStatusSucceeded)
	}
}

// TestDelivery_TypeOptedOutContact_WholeUnsubscribeStillSkipsWithTheWholeReason
// pins the precedence between the two opt-out shapes at the delivery: a
// contact that opts out of one type and THEN whole-unsubscribes is refused
// by the whole-contact status answer (the broader, later withdrawal), never
// by the narrower type-scoped one -- the whole unsubscribe is the contact's
// durable fact, and its skip record carries the whole-contact reason.
func TestDelivery_TypeOptedOutContact_WholeUnsubscribeStillSkipsWithTheWholeReason(t *testing.T) {
	env := newDeliveryEnv(t)
	ctx := tenantCtx(deliveryTenant)
	env.contacts.types = fixtureRegistrar{types: fixtureTypes}

	contact, err := env.contacts.CreateContact(ctx, ContactCreateInput{
		Channel:    ChannelEmail,
		Address:    "wangfang@external.example.com",
		ConsentRef: "consent-ref-1",
	})
	if err != nil {
		t.Fatalf("create the verified contact: %v", err)
	}
	if err := env.contacts.UnsubscribeType(ctx, UnsubscribeTypeInput{
		ContactID: contact.ID,
		TypeKey:   fixtureTypeAppointment,
	}); err != nil {
		t.Fatalf("UnsubscribeType: %v", err)
	}
	if _, err := env.contacts.Unsubscribe(ctx, UnsubscribeInput{ContactID: contact.ID}); err != nil {
		t.Fatalf("whole unsubscribe: %v", err)
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

	if got := len(env.host.mailer.messages()); got != 0 {
		t.Errorf("mailer sent %d messages to a whole-unsubscribed contact, want none", got)
	}
	rec := env.sendRecordByChannel(t, ctx, d, ChannelEmail)
	if rec == nil {
		t.Fatal("no send record for the refused delivery, want the skip recorded")
	}
	if rec.Status != SendRecordStatusSkipped {
		t.Errorf("record status = %s, want %s", rec.Status, SendRecordStatusSkipped)
	}
	if rec.Error != skipReasonUnsubscribed {
		t.Errorf("record reason = %q, want the whole-contact reason %q", rec.Error, skipReasonUnsubscribed)
	}
}
