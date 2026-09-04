package notification

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/i18n"
	"github.com/vislake/speed/go/tenancy/tenancytest"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/notification/locales"
)

// The consent ledger's three dev keys, all deliberately distinct: the
// cipher key that encrypts the address column at rest, the email index
// key, and the phone index key. F8's rule is that an index key must never
// double as the encryption key; the constants stand apart so no fixture
// can accidentally share bytes.
const (
	testCipherKey     = "0123456789abcdef0123456789abcdef"
	testEmailIndexKey = "abcdef0123456789abcdef0123456789"
	testPhoneIndexKey = "3456789abcdef0123456789abcdef015"
	testMailFrom      = "notifications@example.com"
)

// testPhone is the fixture E.164 number this file's SMS tests message.
const testPhone = "+8613800138000"

// registerContactSerializer installs the notification_address_enc gorm
// serializer once per test process, the same registration the host performs
// at bootstrap with a real key (see ContactAddressSerializerName). The
// serializer registry is process-global, so the Once keeps repeated
// registrations from churning it; NewCipher can only fail on key length,
// and the fixture key above is fixed 32 bytes, so the panic branch is
// unreachable by construction rather than a real failure path.
var registerContactSerializerOnce sync.Once

func registerContactSerializer() {
	registerContactSerializerOnce.Do(func() {
		cipher, err := dbkit.NewCipher([]byte(testCipherKey))
		if err != nil {
			panic(fmt.Sprintf("notification test: NewCipher on the fixed 32-byte fixture key: %v", err))
		}
		dbkit.RegisterEncryptedSerializer(ContactAddressSerializerName, cipher)
	})
}

// testEmailIndexer returns the email blind indexer the module's Register
// would receive through WithContactEmailIndexer, bound to the dev email
// index key and dbkit.NormalizeEmail.
func testEmailIndexer(t *testing.T) *dbkit.BlindIndexer {
	t.Helper()
	indexer, err := dbkit.NewBlindIndexer("address_index", []byte(testEmailIndexKey), dbkit.NormalizeEmail)
	if err != nil {
		t.Fatalf("NewBlindIndexer(email): %v", err)
	}
	return indexer
}

// testPhoneIndexer is the SMS twin of testEmailIndexer, over
// dbkit.NormalizePhoneE164.
func testPhoneIndexer(t *testing.T) *dbkit.BlindIndexer {
	t.Helper()
	indexer, err := dbkit.NewBlindIndexer("address_index", []byte(testPhoneIndexKey), dbkit.NormalizePhoneE164)
	if err != nil {
		t.Fatalf("NewBlindIndexer(phone): %v", err)
	}
	return indexer
}

// mustIndex blind-indexes an already-canonical value (the same normalized
// form the service indexes), failing the test on the indexer's error.
func mustIndex(t *testing.T, indexer *dbkit.BlindIndexer, normalized string) string {
	t.Helper()
	index, err := indexer.Index(normalized)
	if err != nil {
		t.Fatalf("Index(%q): %v", normalized, err)
	}
	return index
}

// recordingBus is an EventBus that delegates to the real in-memory bus and
// keeps a copy of everything published -- the org-events-test pattern --
// so a test can assert what the contact service announced without
// subscribing by hand. failWith makes Publish fail, for the emit-failure
// shape.
type recordingBus struct {
	pkgcore.EventBus

	mu        sync.Mutex
	published []pkgcore.Event
	failWith  error
}

func newRecordingBus() *recordingBus {
	return &recordingBus{EventBus: pkgcore.NewMemoryEventBus()}
}

func (b *recordingBus) Publish(ctx context.Context, evt pkgcore.Event) error {
	b.mu.Lock()
	b.published = append(b.published, evt)
	failure := b.failWith
	b.mu.Unlock()
	if failure != nil {
		return failure
	}
	return b.EventBus.Publish(ctx, evt)
}

func (b *recordingBus) events(eventType string) []pkgcore.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []pkgcore.Event
	for _, evt := range b.published {
		if evt.Type == eventType {
			out = append(out, evt)
		}
	}
	return out
}

// recordingMailer is a pkgcore.Mailer that keeps every message instead of
// sending it, and can be made to fail (the org-events-test pattern).
type recordingMailer struct {
	mu       sync.Mutex
	sent     []pkgcore.Mail
	failWith error
}

func (m *recordingMailer) Send(_ context.Context, mail pkgcore.Mail) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWith != nil {
		return m.failWith
	}
	m.sent = append(m.sent, mail)
	return nil
}

func (m *recordingMailer) messages() []pkgcore.Mail {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]pkgcore.Mail(nil), m.sent...)
}

// testHost is a stand-in for the host's *pkgcore.Registry: the four seams
// ContactService reads at call time, each backed by the standalone
// deployment mode's own implementation. It mirrors org's events test host
// one for one; the compile-time assertion pins that it satisfies the
// structural contactHost interface.
type testHost struct {
	kv      pkgcore.KVStore
	mailer  *recordingMailer
	catalog *i18n.Catalog
	bus     *recordingBus
}

func (h *testHost) KVStore() pkgcore.KVStore   { return h.kv }
func (h *testHost) Mailer() pkgcore.Mailer     { return h.mailer }
func (h *testHost) Locales() *i18n.Catalog     { return h.catalog }
func (h *testHost) EventBus() pkgcore.EventBus { return h.bus }

var _ contactHost = (*testHost)(nil)

// newTestHost builds a host whose catalog is notification's REAL locale
// bundle, merged through the same i18n.Builder the kernel uses: a message
// id the contact service looks up but never shipped fails here, in the
// test, rather than in production.
func newTestHost(t *testing.T) *testHost {
	t.Helper()
	builder := i18n.NewBuilder()
	if err := builder.AddModule(moduleName, locales.FS); err != nil {
		t.Fatalf("build the notification message catalog: %v", err)
	}
	return &testHost{
		kv:      pkgcore.NewMemoryKVStore(),
		mailer:  &recordingMailer{},
		catalog: builder.Build(),
		bus:     newRecordingBus(),
	}
}

// contactEnv is one fully seam-ful contact service: a fresh migrated
// database, the console SMS sender writing into smsBuf, a recording
// mailer, a recording bus carrying the audit registrar, and the two blind
// indexers -- the arrangement a host gets after Module.Register, built by
// direct field assignment (same package) instead of a module, so each test
// starts with an empty ledger and empty rate-limit budgets.
type contactEnv struct {
	svc    *ContactService
	host   *testHost
	smsBuf *bytes.Buffer
}

func newContactEnv(t *testing.T) *contactEnv {
	t.Helper()
	registerContactSerializer()
	env := &contactEnv{
		svc:    NewContactService(newTestDB(t)),
		host:   newTestHost(t),
		smsBuf: new(bytes.Buffer),
	}
	reg := pkgcore.NewRegistry(env.host.bus, env.host.kv, env.host.mailer)
	if err := reg.AuditActions.Add(contactAuditActionDecls...); err != nil {
		t.Fatalf("register the contact audit actions: %v", err)
	}
	env.svc.sms = NewConsoleSMSSender(env.smsBuf)
	env.svc.mailFrom = testMailFrom
	env.svc.emailIndexer = testEmailIndexer(t)
	env.svc.phoneIndexer = testPhoneIndexer(t)
	env.svc.host = env.host
	env.svc.audit = reg.AuditActions
	return env
}

// contactCodeRe matches any run of six digits -- the verification code's
// shape (see contact_code.go). Last-match extraction is deliberate: the
// console SMS line begins with the recipient's phone number, itself a long
// digit run, so the code is always the FINAL six-digit run on the line.
var contactCodeRe = regexp.MustCompile(`[0-9]{6}`)

// lastCode returns the final six-digit run in text (see contactCodeRe).
func lastCode(t *testing.T, text string) string {
	t.Helper()
	matches := contactCodeRe.FindAllString(text, -1)
	if len(matches) == 0 {
		t.Fatalf("no six-digit code in %q", text)
	}
	return matches[len(matches)-1]
}

// smsLines returns the console SMS sender's output line by line.
func smsLines(buf *bytes.Buffer) []string {
	trimmed := strings.TrimSpace(buf.String())
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// smsCodeAt returns the verification code of the i-th delivered SMS (0
// based, in delivery order).
func smsCodeAt(t *testing.T, buf *bytes.Buffer, i int) string {
	t.Helper()
	lines := smsLines(buf)
	if i >= len(lines) {
		t.Fatalf("want the %d-th SMS line, only %d delivered", i, len(lines))
	}
	return lastCode(t, lines[i])
}

// emailCodeAt returns the verification code of the i-th delivered mail (0
// based, in delivery order), extracted from the message body -- the
// recipient-visible text, which is where the code travels.
func emailCodeAt(t *testing.T, env *contactEnv, i int) string {
	t.Helper()
	mails := env.host.mailer.messages()
	if i >= len(mails) {
		t.Fatalf("want the %d-th mail, only %d delivered", i, len(mails))
	}
	return lastCode(t, mails[i].Text)
}

// wrongCode returns a code guaranteed different from code: every digit
// shifted up by one mod ten. Deterministic, so the "wrong code" paths never
// flake on a one-in-a-million collision with the real code.
func wrongCode(code string) string {
	out := make([]byte, len(code))
	for i := 0; i < len(code); i++ {
		out[i] = '0' + (code[i]-'0'+1)%10
	}
	return string(out)
}

// mustFindContact re-reads one contact row, the authoritative state after
// any service call (VerifyCode and CreateContact return snapshots whose
// status fields predate the transition they performed).
func mustFindContact(t *testing.T, env *contactEnv, ctx context.Context, id string) *VerifiedContact {
	t.Helper()
	got, err := env.svc.repo.FindByID(ctx, id)
	if err != nil {
		t.Fatalf("FindByID(%s): %v", id, err)
	}
	return got
}

// recordedAudit returns every recorded audit event under action, in order.
func recordedAudit(t *testing.T, env *contactEnv, action string) []audit.RecordedEvent {
	t.Helper()
	var out []audit.RecordedEvent
	for _, evt := range env.host.bus.events(audit.EventRecorded) {
		rec, ok := evt.Payload.(audit.RecordedEvent)
		if !ok {
			t.Fatalf("audit event payload is %T, want audit.RecordedEvent", evt.Payload)
		}
		if rec.Action == action {
			out = append(out, rec)
		}
	}
	return out
}

// assertRateLimited fails t unless err is the module's rate-limit error
// carrying the given dimension and a retry-after hint -- the fail-closed
// denial shape every limit shares.
func assertRateLimited(t *testing.T, err error, wantDimension string) {
	t.Helper()
	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("error %v is not an *apperr.Error, want the contact_rate_limited denial", err)
	}
	if appErr.Code != ErrContactRateLimited.Code {
		t.Fatalf("error code = %s, want %s", appErr.Code, ErrContactRateLimited.Code)
	}
	if appErr.Status != http.StatusTooManyRequests {
		t.Errorf("Status = %d, want %d", appErr.Status, http.StatusTooManyRequests)
	}
	if got := appErr.Params["dimension"]; got != wantDimension {
		t.Errorf("dimension param = %v, want %q", got, wantDimension)
	}
	if _, ok := appErr.Params["retry_after_seconds"]; !ok {
		t.Errorf("denial carries no retry_after_seconds param: %v", appErr.Params)
	}
}

// TestContact_AssertIsolated runs the tenant-data isolation suite every
// tenant-scoped repository must pass: the consent ledger of one tenant must
// be invisible to every other, and a row can never be created without a
// tenant context. The closure returns a row with a fresh id and a fresh
// address index on every call, as the suite requires -- the unique index is
// (tenant_id, channel, address_index), so no two rows the suite creates may
// collide. The TenantModel is left empty: the repository stamps the
// context's tenant.
func TestContact_AssertIsolated(t *testing.T) {
	env := newContactEnv(t)
	seq := 0
	tenancytest.AssertIsolated[VerifiedContact](t, env.svc.repo.Repository,
		func(_ pkgcore.TenantID) *VerifiedContact {
			seq++
			return &VerifiedContact{
				ID:           fmt.Sprintf("contact-%06d", seq),
				Channel:      ChannelEmail,
				Address:      fmt.Sprintf("patient-%06d@example.com", seq),
				AddressIndex: fmt.Sprintf("%064x", seq),
				Status:       ContactStatusPending,
				ConsentBy:    ContactConsentByDoubleOptIn,
			}
		})
}

// TestContact_ByChannelAndAddressIndex_TenantScoped pins the repository
// probe's cross-tenant contract: the same channel and address index under a
// different tenant is indistinguishable from an absent row (nil, nil) -- an
// address registered by tenant A must never be findable from tenant B, and
// the dedupe probe a create runs is exactly this method.
func TestContact_ByChannelAndAddressIndex_TenantScoped(t *testing.T) {
	env := newContactEnv(t)
	ctxA := tenantCtx("tenant-acme")
	ctxB := tenantCtx("tenant-other")
	const address = "scoped@example.com"
	index := mustIndex(t, env.svc.emailIndexer, address)

	contact := &VerifiedContact{
		ID:           "contact-scoped-1",
		Channel:      ChannelEmail,
		Address:      address,
		AddressIndex: index,
		Status:       ContactStatusPending,
		ConsentBy:    ContactConsentByDoubleOptIn,
	}
	if err := env.svc.repo.Create(ctxA, contact); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if got, err := env.svc.repo.ByChannelAndAddressIndex(ctxB, ChannelEmail, index); err != nil {
		t.Fatalf("ByChannelAndAddressIndex from another tenant: %v", err)
	} else if got != nil {
		t.Errorf("ByChannelAndAddressIndex from another tenant returned %+v, want nil", got)
	}

	got, err := env.svc.repo.ByChannelAndAddressIndex(ctxA, ChannelEmail, index)
	if err != nil {
		t.Fatalf("ByChannelAndAddressIndex from the owning tenant: %v", err)
	}
	if got == nil || got.ID != contact.ID {
		t.Errorf("ByChannelAndAddressIndex = %+v, want the row the owning tenant created", got)
	}
}

// TestContact_CreateDoubleOptIn_SMS_VerifiesAndBecomesDeliverable drives a
// full SMS double opt-in: create sends exactly one synchronous
// verification-code message (the sole sanctioned exception to event-driven
// messaging), the pending contact refuses delivery until the code is
// verified, VerifyCode moves the row to verified, and the deliverability
// gate then passes. This is the contact half of the round's second
// fail-before-pass requirement: a message to an unverified phone number is
// refused -- no transport call ever happens for it -- until consent
// verification completes. Every transport side effect is asserted as a
// count: exactly one SMS line total, however many gate checks run.
func TestContact_CreateDoubleOptIn_SMS_VerifiesAndBecomesDeliverable(t *testing.T) {
	env := newContactEnv(t)
	ctx := tenantCtx("tenant-acme")

	contact, err := env.svc.CreateContact(ctx, ContactCreateInput{Channel: ChannelSMS, Address: testPhone})
	if err != nil {
		t.Fatalf("CreateContact: %v", err)
	}
	if contact.Status != ContactStatusPending || contact.ConsentBy != ContactConsentByDoubleOptIn {
		t.Errorf("created contact = status %s / consent_by %s, want pending / double_opt_in",
			contact.Status, contact.ConsentBy)
	}

	lines := smsLines(env.smsBuf)
	if len(lines) != 1 {
		t.Fatalf("SMS lines = %d, want exactly 1", len(lines))
	}
	if !strings.HasPrefix(lines[0], "SMS to "+testPhone+": ") {
		t.Errorf("SMS line %q does not start with the fixture phone", lines[0])
	}
	code := lastCode(t, lines[0])

	// The code hash lands on the row in its own UPDATE (stampCode), so the
	// row -- not the create-time struct -- carries it.
	row := mustFindContact(t, env, ctx, contact.ID)
	if row.CodeHash != hashContactCode(code) {
		t.Errorf("row code_hash = %q, want the SHA-256 of the delivered code", row.CodeHash)
	}
	if row.CodeExpiresAt == nil || !row.CodeExpiresAt.After(time.Now()) {
		t.Errorf("row code_expires_at = %v, want a future expiry", row.CodeExpiresAt)
	}

	// The gate refuses before any transport is touched: the refusal is the
	// deliverability contract, and the SMS line count proves no message rode
	// along.
	if _, err = env.svc.EnsureDeliverable(ctx, contact.ID); err == nil {
		t.Fatal("EnsureDeliverable on a pending contact succeeded, want refusal")
	} else {
		assertCode(t, err, ErrContactNotVerified.Code)
	}
	if len(smsLines(env.smsBuf)) != 1 {
		t.Errorf("SMS lines = %d after the refused gate check, want still 1", len(smsLines(env.smsBuf)))
	}

	if _, err = env.svc.VerifyCode(ctx, VerifyCodeInput{ContactID: contact.ID, Code: code}); err != nil {
		t.Fatalf("VerifyCode: %v", err)
	}

	verified := mustFindContact(t, env, ctx, contact.ID)
	if verified.Status != ContactStatusVerified {
		t.Errorf("row status after VerifyCode = %s, want verified", verified.Status)
	}
	if verified.ConsentAt == nil || verified.VerifiedAt == nil {
		t.Errorf("row consent_at / verified_at not both set: %+v", verified)
	}

	deliverable, err := env.svc.EnsureDeliverable(ctx, contact.ID)
	if err != nil {
		t.Fatalf("EnsureDeliverable on the verified contact: %v", err)
	}
	if deliverable.ID != contact.ID {
		t.Errorf("EnsureDeliverable returned contact %q, want %q", deliverable.ID, contact.ID)
	}
	if len(smsLines(env.smsBuf)) != 1 {
		t.Errorf("SMS lines = %d at the end, want still 1 (verification sends nothing)", len(smsLines(env.smsBuf)))
	}

	verifiedEvents := recordedAudit(t, env, AuditActionContactVerified)
	if len(verifiedEvents) != 1 {
		t.Fatalf("verified audit events = %d, want exactly 1", len(verifiedEvents))
	}
	rec := verifiedEvents[0]
	if rec.TenantID != "tenant-acme" {
		t.Errorf("audit TenantID = %q, want tenant-acme", rec.TenantID)
	}
	if rec.Resource.Type != "contact" || rec.Resource.ID != contact.ID {
		t.Errorf("audit resource = %+v, want type contact and the row id", rec.Resource)
	}
	// DisplayName is channel plus blind index -- the plaintext phone number
	// must never reach the audit trail.
	wantDisplay := "sms:" + mustIndex(t, env.svc.phoneIndexer, testPhone)
	if rec.Resource.DisplayName != wantDisplay {
		t.Errorf("audit DisplayName = %q, want %q (blind index only, never the address)", rec.Resource.DisplayName, wantDisplay)
	}
	if strings.Contains(rec.Resource.DisplayName, testPhone) {
		t.Errorf("audit DisplayName %q leaks the plaintext phone number", rec.Resource.DisplayName)
	}
	if !rec.Result.Success {
		t.Errorf("audit Result = %+v, want Success", rec.Result)
	}
	if attested := recordedAudit(t, env, AuditActionContactAttested); len(attested) != 0 {
		t.Errorf("attested audit events = %d on a double-opt-in flow, want 0", len(attested))
	}
}

// TestContact_CreateDoubleOptIn_Email_RendersAndStoresHash covers the email
// leg of the same flow: the message travels through the registry's mailer
// with the module's From address and the normalized address, and the row's
// code hash matches the delivered code -- proving the hash the patient must
// type is the hash that was sent.
func TestContact_CreateDoubleOptIn_Email_RendersAndStoresHash(t *testing.T) {
	env := newContactEnv(t)
	ctx := tenantCtx("tenant-acme")

	// Deliberately mixed-case input: the stored and messaged address must be
	// the normalized lowercase form.
	contact, err := env.svc.CreateContact(ctx, ContactCreateInput{Channel: ChannelEmail, Address: "Ada@Example.COM"})
	if err != nil {
		t.Fatalf("CreateContact: %v", err)
	}

	mails := env.host.mailer.messages()
	if len(mails) != 1 {
		t.Fatalf("mails = %d, want exactly 1", len(mails))
	}
	if mails[0].From != testMailFrom {
		t.Errorf("mail From = %q, want %q", mails[0].From, testMailFrom)
	}
	if len(mails[0].To) != 1 || mails[0].To[0] != "ada@example.com" {
		t.Errorf("mail To = %v, want the normalized address", mails[0].To)
	}
	if mails[0].Subject == "" || mails[0].Text == "" {
		t.Errorf("mail subject/body rendered empty: %+v", mails[0])
	}
	code := lastCode(t, mails[0].Text)

	row := mustFindContact(t, env, ctx, contact.ID)
	if row.Address != "ada@example.com" {
		t.Errorf("stored address = %q, want the normalized form", row.Address)
	}
	if row.CodeHash != hashContactCode(code) {
		t.Errorf("row code_hash = %q, want the SHA-256 of the delivered code", row.CodeHash)
	}
}

// TestContact_VerifyCode_WrongCodeRefusedThenCorrectWorks pins the wrong /
// right code ladder: a wrong code fails with the code_invalid refusal, and
// the correct code still works afterwards (a wrong guess consumes nothing
// but the attempt's rate-limit budget).
func TestContact_VerifyCode_WrongCodeRefusedThenCorrectWorks(t *testing.T) {
	env := newContactEnv(t)
	ctx := tenantCtx("tenant-acme")

	contact, err := env.svc.CreateContact(ctx, ContactCreateInput{Channel: ChannelEmail, Address: "wrong-right@example.com"})
	if err != nil {
		t.Fatalf("CreateContact: %v", err)
	}
	code := emailCodeAt(t, env, 0)

	if _, err := env.svc.VerifyCode(ctx, VerifyCodeInput{ContactID: contact.ID, Code: wrongCode(code)}); err == nil {
		t.Fatal("VerifyCode with a wrong code succeeded, want refusal")
	} else {
		assertCode(t, err, ErrContactCodeInvalid.Code)
	}
	if row := mustFindContact(t, env, ctx, contact.ID); row.Status != ContactStatusPending {
		t.Errorf("row status after a wrong code = %s, want still pending", row.Status)
	}

	if _, err := env.svc.VerifyCode(ctx, VerifyCodeInput{ContactID: contact.ID, Code: code}); err != nil {
		t.Fatalf("VerifyCode with the correct code after a wrong attempt: %v", err)
	}
}

// TestContact_VerifyCode_ReplayRefused pins single-use: verifying the same
// code a second time is refused with the same invalid-code answer (never an
// "already used" oracle), and the audit trail records the transition once.
func TestContact_VerifyCode_ReplayRefused(t *testing.T) {
	env := newContactEnv(t)
	ctx := tenantCtx("tenant-acme")

	contact, err := env.svc.CreateContact(ctx, ContactCreateInput{Channel: ChannelEmail, Address: "replay@example.com"})
	if err != nil {
		t.Fatalf("CreateContact: %v", err)
	}
	code := emailCodeAt(t, env, 0)

	if _, err := env.svc.VerifyCode(ctx, VerifyCodeInput{ContactID: contact.ID, Code: code}); err != nil {
		t.Fatalf("first VerifyCode: %v", err)
	}
	if _, err := env.svc.VerifyCode(ctx, VerifyCodeInput{ContactID: contact.ID, Code: code}); err == nil {
		t.Fatal("replaying the consumed code succeeded, want refusal")
	} else {
		assertCode(t, err, ErrContactCodeInvalid.Code)
	}
	if got := recordedAudit(t, env, AuditActionContactVerified); len(got) != 1 {
		t.Errorf("verified audit events = %d after a replay, want exactly 1", len(got))
	}
}

// TestContact_VerifyCode_ExpiredCodeRefused pins expiry without ever naming
// a TTL constant: the row's own expiry is read back, the service clock is
// frozen one second past it, and the code -- correct in every other way --
// is refused with the same invalid-code answer an expiry must produce.
func TestContact_VerifyCode_ExpiredCodeRefused(t *testing.T) {
	env := newContactEnv(t)
	ctx := tenantCtx("tenant-acme")

	contact, err := env.svc.CreateContact(ctx, ContactCreateInput{Channel: ChannelEmail, Address: "expiry@example.com"})
	if err != nil {
		t.Fatalf("CreateContact: %v", err)
	}
	code := emailCodeAt(t, env, 0)
	row := mustFindContact(t, env, ctx, contact.ID)
	if row.CodeExpiresAt == nil {
		t.Fatal("row has no code expiry")
	}
	env.svc.now = func() time.Time { return row.CodeExpiresAt.Add(time.Second) }

	if _, err := env.svc.VerifyCode(ctx, VerifyCodeInput{ContactID: contact.ID, Code: code}); err == nil {
		t.Fatal("VerifyCode with an expired code succeeded, want refusal")
	} else {
		assertCode(t, err, ErrContactCodeInvalid.Code)
	}
}

// TestContact_ResendCode_Rotates pins resend semantics: a fresh code is
// stamped and sent (the old code's hash is replaced on the row), only the
// fresh code verifies, and once it does the old code is refused -- through
// the status gate, so the assertion holds deterministically even in the
// astronomically unlikely event that the two codes are equal.
func TestContact_ResendCode_Rotates(t *testing.T) {
	env := newContactEnv(t)
	ctx := tenantCtx("tenant-acme")

	contact, err := env.svc.CreateContact(ctx, ContactCreateInput{Channel: ChannelEmail, Address: "rotate@example.com"})
	if err != nil {
		t.Fatalf("CreateContact: %v", err)
	}
	first := emailCodeAt(t, env, 0)

	if err := env.svc.ResendCode(ctx, ResendCodeInput{ContactID: contact.ID}); err != nil {
		t.Fatalf("ResendCode: %v", err)
	}
	second := emailCodeAt(t, env, 1)

	row := mustFindContact(t, env, ctx, contact.ID)
	if row.CodeHash != hashContactCode(second) {
		t.Errorf("row code_hash after resend = %q, want the hash of the freshly sent code", row.CodeHash)
	}

	if _, err := env.svc.VerifyCode(ctx, VerifyCodeInput{ContactID: contact.ID, Code: second}); err != nil {
		t.Fatalf("VerifyCode with the resent code: %v", err)
	}
	// The old code cannot work now: the row is verified, and a verified row
	// answers any code with the invalid-code refusal.
	if _, err := env.svc.VerifyCode(ctx, VerifyCodeInput{ContactID: contact.ID, Code: first}); err == nil {
		t.Fatal("the pre-resend code verified after the resent code was consumed, want refusal")
	} else {
		assertCode(t, err, ErrContactCodeInvalid.Code)
	}
	if got := recordedAudit(t, env, AuditActionContactVerified); len(got) != 1 {
		t.Errorf("verified audit events = %d, want exactly 1", len(got))
	}
}

// TestContact_CreateContact_DedupeReturnsExistingRowNoResend pins the dedupe
// contract: registering an address that already has a consent record
// resolves by returning the existing row unchanged -- no second code, no
// second message, whatever case the caller types the address in (the
// dedupe keys on the normalized blind index).
func TestContact_CreateContact_DedupeReturnsExistingRowNoResend(t *testing.T) {
	env := newContactEnv(t)
	ctx := tenantCtx("tenant-acme")

	first, err := env.svc.CreateContact(ctx, ContactCreateInput{Channel: ChannelEmail, Address: "Case@Example.com"})
	if err != nil {
		t.Fatalf("first CreateContact: %v", err)
	}

	again, err := env.svc.CreateContact(ctx, ContactCreateInput{Channel: ChannelEmail, Address: "CASE@example.COM"})
	if err != nil {
		t.Fatalf("second CreateContact: %v", err)
	}
	if again.ID != first.ID {
		t.Errorf("dedupe returned a different row %q, want %q", again.ID, first.ID)
	}
	if mails := env.host.mailer.messages(); len(mails) != 1 {
		t.Errorf("mails = %d after a duplicate create, want still 1", len(mails))
	}
	if got := recordedAudit(t, env, AuditActionContactVerified); len(got) != 0 {
		t.Errorf("verified audit events = %d on a duplicate create, want 0", len(got))
	}
}

// TestContact_CreateContact_AttestedImmediatelyVerified covers the
// business_attested leg: an attested address is created already verified,
// no code is ever sent, the consent reference rides on the row and in the
// audit event, and the deliverability gate passes immediately.
func TestContact_CreateContact_AttestedImmediatelyVerified(t *testing.T) {
	env := newContactEnv(t)
	ctx := tenantCtx("tenant-acme")
	const ref = "intake/2026-09/0001"

	contact, err := env.svc.CreateContact(ctx, ContactCreateInput{
		Channel:    ChannelEmail,
		Address:    "attested@example.com",
		ConsentRef: ref,
	})
	if err != nil {
		t.Fatalf("CreateContact: %v", err)
	}
	if contact.Status != ContactStatusVerified {
		t.Errorf("attested contact status = %s, want verified", contact.Status)
	}
	if mails := env.host.mailer.messages(); len(mails) != 0 {
		t.Errorf("mails = %d on an attested create, want 0 (no code is ever sent)", len(mails))
	}

	row := mustFindContact(t, env, ctx, contact.ID)
	if row.ConsentBy != ContactConsentByBusinessAttested {
		t.Errorf("row consent_by = %q, want business_attested", row.ConsentBy)
	}
	if row.ConsentRef != ref {
		t.Errorf("row consent_ref = %q, want %q", row.ConsentRef, ref)
	}
	if row.ConsentAt == nil || row.VerifiedAt == nil {
		t.Errorf("row consent_at / verified_at not both set: %+v", row)
	}

	if deliverable, err := env.svc.EnsureDeliverable(ctx, contact.ID); err != nil || deliverable == nil {
		t.Errorf("EnsureDeliverable on the attested contact: err %v", err)
	}

	events := recordedAudit(t, env, AuditActionContactAttested)
	if len(events) != 1 {
		t.Fatalf("attested audit events = %d, want exactly 1", len(events))
	}
	if events[0].Resource.ID != contact.ID {
		t.Errorf("attested audit resource id = %q, want %q", events[0].Resource.ID, contact.ID)
	}
	wantDisplay := "email:" + mustIndex(t, env.svc.emailIndexer, "attested@example.com")
	if events[0].Resource.DisplayName != wantDisplay {
		t.Errorf("attested audit DisplayName = %q, want %q", events[0].Resource.DisplayName, wantDisplay)
	}
	if got := recordedAudit(t, env, AuditActionContactVerified); len(got) != 0 {
		t.Errorf("verified audit events = %d on an attested create, want 0", len(got))
	}
}

// TestContact_AttestationNeverOverwritesPendingRow pins the in-flight-flow
// rule: attesting an address that already exists as a pending double-opt-in
// row returns that pending row unchanged -- the attestation does not
// overwrite a verification the patient may already be completing, and no
// second message goes out.
func TestContact_AttestationNeverOverwritesPendingRow(t *testing.T) {
	env := newContactEnv(t)
	ctx := tenantCtx("tenant-acme")
	const address = "in-flight@example.com"

	pending, err := env.svc.CreateContact(ctx, ContactCreateInput{Channel: ChannelEmail, Address: address})
	if err != nil {
		t.Fatalf("CreateContact: %v", err)
	}
	mailCount := len(env.host.mailer.messages())

	got, err := env.svc.CreateContact(ctx, ContactCreateInput{
		Channel:    ChannelEmail,
		Address:    address,
		ConsentRef: "intake/2026-09/0002",
	})
	if err != nil {
		t.Fatalf("attested CreateContact over a pending row: %v", err)
	}
	if got.ID != pending.ID || got.Status != ContactStatusPending {
		t.Errorf("attestation over a pending row returned %+v, want the unchanged pending row", got)
	}
	row := mustFindContact(t, env, ctx, pending.ID)
	if row.Status != ContactStatusPending || row.ConsentRef != "" {
		t.Errorf("row after the attestation attempt = status %s consent_ref %q, want untouched pending", row.Status, row.ConsentRef)
	}
	if mails := env.host.mailer.messages(); len(mails) != mailCount {
		t.Errorf("mails = %d, want still %d", len(mails), mailCount)
	}
	if got := recordedAudit(t, env, AuditActionContactAttested); len(got) != 0 {
		t.Errorf("attested audit events = %d, want 0", len(got))
	}
}

// TestContact_Unsubscribe_PermanentAndIdempotent pins the unsubscribe state
// machine: any status moves to unsubscribed, the audit event fires exactly
// once per actual transition (an idempotent repeat emits nothing), the
// deliverability gate and every code path refuse the address afterwards,
// and re-registering the address resolves to the unsubscribed row -- an
// unsubscribe is permanent for the contact as a whole, never silently
// undone by a fresh create (the R8 rule).
func TestContact_Unsubscribe_PermanentAndIdempotent(t *testing.T) {
	env := newContactEnv(t)
	ctx := tenantCtx("tenant-acme")

	contact, err := env.svc.CreateContact(ctx, ContactCreateInput{Channel: ChannelEmail, Address: "opt-out@example.com"})
	if err != nil {
		t.Fatalf("CreateContact: %v", err)
	}
	code := emailCodeAt(t, env, 0)

	unsubscribed, err := env.svc.Unsubscribe(ctx, UnsubscribeInput{ContactID: contact.ID})
	if err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	if unsubscribed.Status != ContactStatusUnsubscribed {
		t.Errorf("unsubscribe returned status %s, want unsubscribed", unsubscribed.Status)
	}
	if got := recordedAudit(t, env, AuditActionContactUnsubscribed); len(got) != 1 {
		t.Fatalf("unsubscribed audit events = %d, want exactly 1", len(got))
	}

	// Idempotent repeat: succeeds, emits nothing new.
	again, err := env.svc.Unsubscribe(ctx, UnsubscribeInput{ContactID: contact.ID})
	if err != nil {
		t.Fatalf("second Unsubscribe: %v", err)
	}
	if again.Status != ContactStatusUnsubscribed {
		t.Errorf("idempotent unsubscribe returned status %s", again.Status)
	}
	if got := recordedAudit(t, env, AuditActionContactUnsubscribed); len(got) != 1 {
		t.Errorf("unsubscribed audit events = %d after an idempotent repeat, want still 1", len(got))
	}

	// Every message-touching path refuses the unsubscribed address.
	if _, err = env.svc.EnsureDeliverable(ctx, contact.ID); err == nil {
		t.Error("EnsureDeliverable on the unsubscribed contact succeeded, want refusal")
	} else {
		assertCode(t, err, ErrContactUnsubscribed.Code)
	}
	if _, err = env.svc.VerifyCode(ctx, VerifyCodeInput{ContactID: contact.ID, Code: code}); err == nil {
		t.Error("VerifyCode on the unsubscribed contact succeeded, want refusal")
	} else {
		assertCode(t, err, ErrContactUnsubscribed.Code)
	}
	if err = env.svc.ResendCode(ctx, ResendCodeInput{ContactID: contact.ID}); err == nil {
		t.Error("ResendCode on the unsubscribed contact succeeded, want refusal")
	} else {
		assertCode(t, err, ErrContactUnsubscribed.Code)
	}

	// Re-registration resolves to the existing unsubscribed row: no new
	// flow, no message, the unsubscribe stands.
	mailCount := len(env.host.mailer.messages())
	reRegistered, err := env.svc.CreateContact(ctx, ContactCreateInput{Channel: ChannelEmail, Address: "OPT-OUT@example.com"})
	if err != nil {
		t.Fatalf("re-registering the unsubscribed address: %v", err)
	}
	if reRegistered.ID != contact.ID || reRegistered.Status != ContactStatusUnsubscribed {
		t.Errorf("re-registration returned %+v, want the unsubscribed row", reRegistered)
	}
	if mails := env.host.mailer.messages(); len(mails) != mailCount {
		t.Errorf("mails = %d after re-registration, want still %d", len(mails), mailCount)
	}
}

// TestContact_MarkBounced_TerminalAndIdempotent pins the delivery-side hard
// failure marking: a bounced contact refuses delivery and verification from
// then on, and re-marking is a no-op. MarkBounced deliberately records no
// audit event (it is a transport observation the delivery job owns, not a
// consent transition).
func TestContact_MarkBounced_TerminalAndIdempotent(t *testing.T) {
	env := newContactEnv(t)
	ctx := tenantCtx("tenant-acme")

	contact, err := env.svc.CreateContact(ctx, ContactCreateInput{Channel: ChannelSMS, Address: testPhone})
	if err != nil {
		t.Fatalf("CreateContact: %v", err)
	}
	code := smsCodeAt(t, env.smsBuf, 0)
	if _, err := env.svc.VerifyCode(ctx, VerifyCodeInput{ContactID: contact.ID, Code: code}); err != nil {
		t.Fatalf("VerifyCode: %v", err)
	}

	if err := env.svc.MarkBounced(ctx, contact.ID); err != nil {
		t.Fatalf("MarkBounced: %v", err)
	}
	if err := env.svc.MarkBounced(ctx, contact.ID); err != nil {
		t.Fatalf("idempotent MarkBounced: %v", err)
	}
	row := mustFindContact(t, env, ctx, contact.ID)
	if row.Status != ContactStatusBounced {
		t.Errorf("row status = %s, want bounced", row.Status)
	}

	if _, err := env.svc.EnsureDeliverable(ctx, contact.ID); err == nil {
		t.Error("EnsureDeliverable on the bounced contact succeeded, want refusal")
	} else {
		assertCode(t, err, ErrContactBounced.Code)
	}
	if _, err := env.svc.VerifyCode(ctx, VerifyCodeInput{ContactID: contact.ID, Code: code}); err == nil {
		t.Error("VerifyCode on the bounced contact succeeded, want refusal")
	} else {
		assertCode(t, err, ErrContactBounced.Code)
	}
	if got := recordedAudit(t, env, AuditActionContactUnsubscribed); len(got) != 0 {
		t.Errorf("unsubscribed audit events = %d on a bounce, want 0", len(got))
	}
	if got := recordedAudit(t, env, AuditActionContactVerified); len(got) != 1 {
		t.Errorf("verified audit events = %d, want exactly the one from VerifyCode", len(got))
	}
}

// TestContact_UnknownIDReturnsNotFound covers every id-addressed method's
// absent-row answer: one refusal shape (contact_not_found) whichever
// operation names a row that does not exist, so callers can classify the
// five entry points uniformly.
func TestContact_UnknownIDReturnsNotFound(t *testing.T) {
	env := newContactEnv(t)
	ctx := tenantCtx("tenant-acme")
	const missing = "contact-does-not-exist"

	if _, err := env.svc.EnsureDeliverable(ctx, missing); err == nil {
		t.Error("EnsureDeliverable on an unknown id succeeded, want contact_not_found")
	} else {
		assertCode(t, err, ErrContactNotFound.Code)
	}
	if _, err := env.svc.VerifyCode(ctx, VerifyCodeInput{ContactID: missing, Code: "123456"}); err == nil {
		t.Error("VerifyCode on an unknown id succeeded, want contact_not_found")
	} else {
		assertCode(t, err, ErrContactNotFound.Code)
	}
	if err := env.svc.ResendCode(ctx, ResendCodeInput{ContactID: missing}); err == nil {
		t.Error("ResendCode on an unknown id succeeded, want contact_not_found")
	} else {
		assertCode(t, err, ErrContactNotFound.Code)
	}
	if _, err := env.svc.Unsubscribe(ctx, UnsubscribeInput{ContactID: missing}); err == nil {
		t.Error("Unsubscribe on an unknown id succeeded, want contact_not_found")
	} else {
		assertCode(t, err, ErrContactNotFound.Code)
	}
	if err := env.svc.MarkBounced(ctx, missing); err == nil {
		t.Error("MarkBounced on an unknown id succeeded, want contact_not_found")
	} else {
		assertCode(t, err, ErrContactNotFound.Code)
	}
	if mails := env.host.mailer.messages(); len(mails) != 0 {
		t.Errorf("mails = %d across the not-found suite, want 0", len(mails))
	}
}

// TestContact_Validation_InvalidChannelAndAddress pins the input gate: an
// unknown channel, a phone number that is not E.164 and a blank email are
// all refused before anything is written or sent.
func TestContact_Validation_InvalidChannelAndAddress(t *testing.T) {
	env := newContactEnv(t)
	ctx := tenantCtx("tenant-acme")

	if _, err := env.svc.CreateContact(ctx, ContactCreateInput{Channel: "push", Address: testPhone}); err == nil {
		t.Error("CreateContact on channel push succeeded, want refusal")
	} else {
		assertCode(t, err, ErrContactInvalidChannel.Code)
	}
	if _, err := env.svc.CreateContact(ctx, ContactCreateInput{Channel: ChannelSMS, Address: "not-a-phone"}); err == nil {
		t.Error("CreateContact on a non-E.164 phone succeeded, want refusal")
	} else {
		assertCode(t, err, ErrContactInvalidAddress.Code)
	}
	if _, err := env.svc.CreateContact(ctx, ContactCreateInput{Channel: ChannelEmail, Address: "   "}); err == nil {
		t.Error("CreateContact on a blank email succeeded, want refusal")
	} else {
		assertCode(t, err, ErrContactInvalidAddress.Code)
	}

	// Nothing was written and nothing was sent. The probe runs under the
	// same tenant session the repository's own calls use -- a bare
	// connection with no tenant in its context is not a query this database
	// is obliged to answer.
	var count int64
	if err := dbkit.WithTenantSession(ctx, env.svc.repo.db, func(tx *gorm.DB) error {
		return tx.Model(&VerifiedContact{}).Count(&count).Error
	}); err != nil {
		t.Fatalf("count the ledger: %v", err)
	}
	if count != 0 {
		t.Errorf("ledger rows = %d after refused creates, want 0", count)
	}
	if mails := env.host.mailer.messages(); len(mails) != 0 {
		t.Errorf("mails = %d after refused creates, want 0", len(mails))
	}
}

// TestContact_AddressEncryptedAtRest proves the privacy claim on the bytes
// themselves: the address column of a committed row holds ciphertext -- not
// the plaintext, byte for byte -- and a repository read decrypts it back.
// The raw-column probe reads through the same connection the repository was
// built on (test code reaching past the Repository is exactly what this
// assertion is for).
func TestContact_AddressEncryptedAtRest(t *testing.T) {
	env := newContactEnv(t)
	ctx := tenantCtx("tenant-acme")
	const plaintext = "encrypt-me@example.com"

	contact, err := env.svc.CreateContact(ctx, ContactCreateInput{Channel: ChannelEmail, Address: plaintext})
	if err != nil {
		t.Fatalf("CreateContact: %v", err)
	}

	var raws [][]byte
	if err := dbkit.WithTenantSession(ctx, env.svc.repo.db, func(tx *gorm.DB) error {
		return tx.Model(&VerifiedContact{}).
			Where("id = ?", contact.ID).Pluck("address", &raws).Error
	}); err != nil {
		t.Fatalf("read the raw address column: %v", err)
	}
	if len(raws) != 1 {
		t.Fatalf("raw address rows = %d, want 1", len(raws))
	}
	if bytes.Equal(raws[0], []byte(plaintext)) {
		t.Error("the address column holds the plaintext address -- nothing is encrypted at rest")
	}
	if len(raws[0]) == 0 {
		t.Error("the address column is empty")
	}

	row := mustFindContact(t, env, ctx, contact.ID)
	if row.Address != plaintext {
		t.Errorf("repository read returned %q, want the plaintext %q", row.Address, plaintext)
	}
}

// TestContact_SendRateLimit_PerAddress pins one send dimension: one address
// may receive at most contactCodeSendDailyPerAddress verification-code
// messages a day. Create is the first send, four resends exhaust the
// budget, and the fifth resend is denied with the per-address dimension --
// before any message goes out.
func TestContact_SendRateLimit_PerAddress(t *testing.T) {
	env := newContactEnv(t)
	ctx := tenantCtx("tenant-acme")
	const address = "send-limit@example.com"

	contact, err := env.svc.CreateContact(ctx, ContactCreateInput{Channel: ChannelEmail, Address: address})
	if err != nil {
		t.Fatalf("CreateContact: %v", err)
	}

	for i := 0; i < 4; i++ {
		if err := env.svc.ResendCode(ctx, ResendCodeInput{ContactID: contact.ID}); err != nil {
			t.Fatalf("resend %d: %v", i+1, err)
		}
	}
	index := mustIndex(t, env.svc.emailIndexer, address)
	if err := env.svc.ResendCode(ctx, ResendCodeInput{ContactID: contact.ID}); err == nil {
		t.Fatal("the sixth send of the day succeeded, want the rate-limit denial")
	} else {
		assertRateLimited(t, err, "send.address."+index)
	}
	if mails := env.host.mailer.messages(); len(mails) != contactCodeSendDailyPerAddress {
		t.Errorf("mails = %d, want the %d that fit inside the budget", len(mails), contactCodeSendDailyPerAddress)
	}
}

// TestContact_SendRateLimit_PerTenant pins the tenant-wide backstop: across
// twenty distinct addresses the tenant may send twenty codes a day, and the
// twenty-first create -- a fresh address that passed the dedupe probe -- is
// denied before its pending row is ever written, so the ledger cannot be
// flooded by a hammering tenant.
func TestContact_SendRateLimit_PerTenant(t *testing.T) {
	env := newContactEnv(t)
	ctx := tenantCtx("tenant-acme")

	for i := 0; i < contactCodeSendDailyPerTenant; i++ {
		if _, err := env.svc.CreateContact(ctx, ContactCreateInput{
			Channel: ChannelEmail,
			Address: fmt.Sprintf("bulk-%02d@example.com", i),
		}); err != nil {
			t.Fatalf("create %d: %v", i+1, err)
		}
	}

	const overflow = "bulk-overflow@example.com"
	if _, err := env.svc.CreateContact(ctx, ContactCreateInput{Channel: ChannelEmail, Address: overflow}); err == nil {
		t.Fatal("the twenty-first create succeeded, want the rate-limit denial")
	} else {
		assertRateLimited(t, err, "send.tenant.tenant-acme")
	}
	index := mustIndex(t, env.svc.emailIndexer, overflow)
	if got, err := env.svc.repo.ByChannelAndAddressIndex(ctx, ChannelEmail, index); err != nil {
		t.Fatalf("ByChannelAndAddressIndex after the denied create: %v", err)
	} else if got != nil {
		t.Errorf("the denied create left a row behind: %+v", got)
	}
}

// TestContact_VerifyRateLimit_PerAddress pins the brute-force bound: ten
// verify attempts per code lifetime at one-in-a-million per attempt, then
// the eleventh attempt -- even carrying the correct code -- is denied. The
// denial answering the correct code proves the limit fails closed rather
// than silently allowing the guess that broke the budget.
func TestContact_VerifyRateLimit_PerAddress(t *testing.T) {
	env := newContactEnv(t)
	ctx := tenantCtx("tenant-acme")
	const address = "verify-limit@example.com"

	contact, err := env.svc.CreateContact(ctx, ContactCreateInput{Channel: ChannelEmail, Address: address})
	if err != nil {
		t.Fatalf("CreateContact: %v", err)
	}
	code := emailCodeAt(t, env, 0)

	for i := 0; i < contactCodeVerifyPerAddress; i++ {
		if _, err := env.svc.VerifyCode(ctx, VerifyCodeInput{ContactID: contact.ID, Code: wrongCode(code)}); err == nil {
			t.Fatalf("wrong attempt %d succeeded, want the invalid-code refusal", i+1)
		} else {
			assertCode(t, err, ErrContactCodeInvalid.Code)
		}
	}
	index := mustIndex(t, env.svc.emailIndexer, address)
	if _, err := env.svc.VerifyCode(ctx, VerifyCodeInput{ContactID: contact.ID, Code: code}); err == nil {
		t.Fatal("the eleventh attempt succeeded, want the rate-limit denial")
	} else {
		assertRateLimited(t, err, "verify.address."+index)
	}
	if row := mustFindContact(t, env, ctx, contact.ID); row.Status != ContactStatusPending {
		t.Errorf("row status = %s after the denied eleventh attempt, want still pending", row.Status)
	}
}

// TestContact_CreateCodeSendFailure_DeletesRow pins the create-time
// rollback: when the code message cannot be delivered, the pending row is
// revoked -- a pending contact whose code the patient could never receive
// must not exist. The mail count is the distinguishing signal: had the row
// survived, the retried create would have deduped onto it and sent nothing.
func TestContact_CreateCodeSendFailure_DeletesRow(t *testing.T) {
	env := newContactEnv(t)
	ctx := tenantCtx("tenant-acme")
	const address = "rollback@example.com"

	env.host.mailer.failWith = errors.New("smtp down")
	if _, err := env.svc.CreateContact(ctx, ContactCreateInput{Channel: ChannelEmail, Address: address}); err == nil {
		t.Fatal("CreateContact succeeded while the mailer was failing, want the delivery-failed error")
	} else {
		assertCode(t, err, ErrContactCodeDeliveryFailed.Code)
	}
	env.host.mailer.failWith = nil

	index := mustIndex(t, env.svc.emailIndexer, address)
	if got, err := env.svc.repo.ByChannelAndAddressIndex(ctx, ChannelEmail, index); err != nil {
		t.Fatalf("ByChannelAndAddressIndex after the failed create: %v", err)
	} else if got != nil {
		t.Errorf("the failed create left a pending row behind: %+v", got)
	}

	if _, err := env.svc.CreateContact(ctx, ContactCreateInput{Channel: ChannelEmail, Address: address}); err != nil {
		t.Fatalf("retried CreateContact: %v", err)
	}
	if mails := env.host.mailer.messages(); len(mails) != 1 {
		t.Errorf("mails = %d, want exactly the retry's 1 (a leftover row would have deduped and sent 0)", len(mails))
	}
}

// contactRowAt returns a directly constructed verified_contacts row with its
// CreatedAt pinned to at, so ordering tests do not depend on the clock's
// resolution (gorm's autoCreateTime fills CreatedAt only when it is zero).
// Rows are built by hand because the list surface under test is read-only:
// seeding through the full create flow would smuggle consent machinery into
// a listing test.
func contactRowAt(t *testing.T, env *contactEnv, id, address string, at time.Time) *VerifiedContact {
	return &VerifiedContact{
		ID:           id,
		Channel:      ChannelEmail,
		Address:      address,
		AddressIndex: mustIndex(t, env.svc.emailIndexer, address),
		Status:       ContactStatusVerified,
		ConsentBy:    ContactConsentByBusinessAttested,
		ConsentRef:   "consent-doc-1",
		CreatedAt:    at,
	}
}

// TestContact_ListForTenant_NewestFirst_TenantScoped pins the roster
// listing's contract: the tenant of ctx owns the answer (another tenant's
// newer contact never appears), and within the tenant the order is newest
// first -- created_at DESC, the order the contacts API's list operation
// serves without pagination.
func TestContact_ListForTenant_NewestFirst_TenantScoped(t *testing.T) {
	env := newContactEnv(t)
	ctx := tenantCtx("tenant-acme")

	t1 := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC)
	rows := []*VerifiedContact{
		contactRowAt(t, env, "contact-a1", "first@example.com", t1),
		contactRowAt(t, env, "contact-a2", "second@example.com", t2),
		contactRowAt(t, env, "contact-a3", "third@example.com", t3),
	}
	for _, row := range rows {
		if err := env.svc.repo.Create(ctx, row); err != nil {
			t.Fatalf("Create(%s): %v", row.ID, err)
		}
	}
	other := contactRowAt(t, env, "contact-b1", "other-tenant@example.com", t3.Add(time.Hour))
	if err := env.svc.repo.Create(tenantCtx("tenant-bright"), other); err != nil {
		t.Fatalf("Create(other tenant): %v", err)
	}

	got, err := env.svc.repo.ListForTenant(ctx)
	if err != nil {
		t.Fatalf("ListForTenant: %v", err)
	}
	want := []string{"contact-a3", "contact-a2", "contact-a1"}
	if len(got) != len(want) {
		t.Fatalf("ListForTenant returned %d rows %v, want %d (%v)", len(got), contactIDsOf(got), len(want), want)
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("row %d = %q, want %q (newest first)", i, got[i].ID, id)
		}
	}
}

// contactIDsOf is the string form of a contact slice's ids, for failure
// messages.
func contactIDsOf(rows []VerifiedContact) []string {
	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}
	return ids
}

// TestContact_ListForTenant_SameCreatedAt_IdDescTiebreak pins the tiebreak
// half of the roster ordering: two contacts created in the same instant
// list deterministically by id DESC, so the unpaged roster never presents
// same-instant rows in an order that can change between reads.
func TestContact_ListForTenant_SameCreatedAt_IdDescTiebreak(t *testing.T) {
	env := newContactEnv(t)
	ctx := tenantCtx("tenant-acme")

	same := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ id, address string }{
		{"contact-1", "one@example.com"},
		{"contact-2", "two@example.com"},
	} {
		if err := env.svc.repo.Create(ctx, contactRowAt(t, env, tc.id, tc.address, same)); err != nil {
			t.Fatalf("Create(%s): %v", tc.id, err)
		}
	}

	got, err := env.svc.repo.ListForTenant(ctx)
	if err != nil {
		t.Fatalf("ListForTenant: %v", err)
	}
	if len(got) != 2 || got[0].ID != "contact-2" || got[1].ID != "contact-1" {
		t.Errorf("ListForTenant = %v, want [contact-2 contact-1]: same created_at must break ties by id DESC", contactIDsOf(got))
	}
}

// TestContactService_List_ReturnsTheRosterNewestFirst is the service face of
// the same contract: List answers the tenant's whole roster, newest first,
// and returns the model rows -- whose Address field carries the decrypted
// plaintext (the serializer decrypts on read). Stripping the address is the
// response layer's job; this test pins that the service itself is where the
// roster ends and the plaintext begins.
func TestContactService_List_ReturnsTheRosterNewestFirst(t *testing.T) {
	env := newContactEnv(t)
	ctx := tenantCtx("tenant-acme")

	t1 := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	const older, newer = "older@example.com", "newer@example.com"
	for _, row := range []*VerifiedContact{
		contactRowAt(t, env, "contact-old", older, t1),
		contactRowAt(t, env, "contact-new", newer, t2),
	} {
		if err := env.svc.repo.Create(ctx, row); err != nil {
			t.Fatalf("Create(%s): %v", row.ID, err)
		}
	}
	if err := env.svc.repo.Create(tenantCtx("tenant-bright"), contactRowAt(t, env, "contact-other", "other@example.com", t2.Add(time.Hour))); err != nil {
		t.Fatalf("Create(other tenant): %v", err)
	}

	got, err := env.svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List returned %d rows %v, want the owning tenant's 2", len(got), contactIDsOf(got))
	}
	if got[0].ID != "contact-new" || got[1].ID != "contact-old" {
		t.Errorf("List = %v, want [contact-new contact-old]", contactIDsOf(got))
	}
	if got[0].Address != newer || got[1].Address != older {
		t.Errorf("List returned addresses %q and %q, want the decrypted plaintext %q and %q",
			got[0].Address, got[1].Address, newer, older)
	}
}
