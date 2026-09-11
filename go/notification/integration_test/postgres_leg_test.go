//go:build integration

// Package notification_test holds go/notification's integration tier: the
// module's proof surface re-run against real servers -- PostgreSQL for the
// five tables' schema and isolation semantics (migrations zero to head,
// tenancytest.AssertIsolated against a real server, and the consent
// ledger's single-winner verify) and Redis for the cross-replica inbox
// delivery whose wire format and at-least-once fan-out an in-process bus
// cannot stand in for. The two files of this package are
// postgres_leg_test.go (which also carries the test fixtures the two legs
// share) and redis_leg_test.go. The tier is physically separate from
// go/notification's unit tests (all of which live in package notification
// itself, one file per source file) and carries the "integration" build
// tag: a plain "go test ./..." never compiles or runs anything in this
// directory; the tier is invoked explicitly with "go test -tags=integration
// ./integration_test/..." from the module directory -- the tag-and-directory
// convention every Docker-backed tier in this workspace follows.
//
// Every test here spins up its own disposable container(s) and requires a
// working Docker (or Docker-API-compatible) daemon; there is no fallback
// or skip-on-missing-Docker path, matching the other modules' tiers.
//
// Why PostgreSQL earns a leg of its own: the module's migrations must run
// from zero on BOTH dialects, and the unit tier can only run the SQLite
// half. The PostgreSQL half is not a formality --
// the consent ledger's verify transition (contact.go's consumePendingCode)
// is a compare-and-swap UPDATE whose single-winner property SQLite cannot
// even put at risk: SQLite serializes whole-database writes, so two
// concurrent verifies of one code can never race there. PostgreSQL runs
// them under real row-lock concurrency, which is exactly where a broken
// CAS -- a read-then-write instead of a conditional UPDATE -- would hand
// the code to both verifiers. go/authn's tier re-proves its own
// refresh-rotation single-winner on real PostgreSQL for the same reason
// (its header records it); this leg is the consent ledger's twin proof.
// The isolation suite likewise earns its second run: AssertIsolated
// passing on SQLite proves nothing about the plugin's behaviour under
// PostgreSQL's different transaction semantics.
package notification_test

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	// Blank-imported for its init side effect: registers
	// dbkit.DialectPostgres so the dbkit.Open(DialectPostgres) call below
	// has a driver to build from.
	_ "github.com/vislake/speed/go/dbkit/dialect/postgres"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/componenttest"
	"github.com/vislake/speed/go/pkgcore/i18n"
	"github.com/vislake/speed/go/pkgcore/testkit"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// ---------------------------------------------------------------------------
// Test fixtures shared by both legs
//
// The unit tier's doubles -- its stubQueue, stubUserResolver and fixture
// key constants -- live in package notification's own test files
// (module_test.go, contact_test.go) and cannot be imported from this
// external test package. These copies exist so the integration tier stands
// alone; they are kept behaviour-identical to their unit-tier twins, and
// each carries a comment where the twin's doc comment explains the shape.
// ---------------------------------------------------------------------------

// The fixture keys mirror the unit tier's byte for byte (contact_test.go's
// constants): a host's address-encryption key and its two blind-index keys
// must never share bytes (spelled out in contact.go's doc comment), and
// the constants stand apart so no fixture can accidentally conflate them.
// This binary is a separate process from the
// unit tier's, so the values being identical to the unit tier's matters
// only for the reader's familiarity.
const (
	testCipherKey     = "0123456789abcdef0123456789abcdef"
	testEmailIndexKey = "abcdef0123456789abcdef0123456789"
	testPhoneIndexKey = "3456789abcdef0123456789abcdef015"
	testMailFrom      = "notifications@example.com"
)

// registerContactSerializer installs the notification_address_enc gorm
// serializer once per test process, the same registration a host performs
// at bootstrap with a real key (see notification.ContactAddressSerializer
// Name). The serializer registry is process-global, so the Once keeps
// repeated registrations from churning it; NewCipher can only fail on key
// length, and the fixture key above is fixed 32 bytes, so the panic branch
// is unreachable by construction rather than a real failure path. The
// unit-tier twin of this helper is contact_test.go's.
var registerContactSerializerOnce sync.Once

func registerContactSerializer() {
	registerContactSerializerOnce.Do(func() {
		cipher, err := dbkit.NewCipher([]byte(testCipherKey))
		if err != nil {
			panic(fmt.Sprintf("notification integration test: NewCipher on the fixed 32-byte fixture key: %v", err))
		}
		dbkit.RegisterEncryptedSerializer(notification.ContactAddressSerializerName, cipher)
	})
}

// emailIndexer returns the email blind indexer the module's Register would
// receive through WithContactEmailIndexer, bound to the dev email index
// key and dbkit.NormalizeEmail (the unit tier's twin: contact_test.go's
// testEmailIndexer).
func emailIndexer(t *testing.T) *dbkit.BlindIndexer {
	t.Helper()
	indexer, err := dbkit.NewBlindIndexer("address_index", []byte(testEmailIndexKey), dbkit.NormalizeEmail)
	if err != nil {
		t.Fatalf("email blind indexer: %v", err)
	}
	return indexer
}

// phoneIndexer is the SMS twin of emailIndexer, over dbkit.NormalizePhone
// E164.
func phoneIndexer(t *testing.T) *dbkit.BlindIndexer {
	t.Helper()
	indexer, err := dbkit.NewBlindIndexer("address_index", []byte(testPhoneIndexKey), dbkit.NormalizePhoneE164)
	if err != nil {
		t.Fatalf("phone blind indexer: %v", err)
	}
	return indexer
}

// mustIndex is the shared one-liner the tier's factories and assertions
// use to turn a normalized raw address into its blind index.
func mustIndex(t *testing.T, indexer *dbkit.BlindIndexer, normalized string) string {
	t.Helper()
	index, err := indexer.Index(normalized)
	if err != nil {
		t.Fatalf("indexing %q: %v", normalized, err)
	}
	return index
}

// stubQueue is a jobs.Queue test double that records every task enqueued
// on it, so a Dispatch can be driven into the delivery pipeline without
// starting a worker pool. Get and Cancel are never reached by the delivery
// pipeline and panic rather than pretend. The Redis leg reads the recorded
// tasks to harvest the delivery job and run its handler; the twin of this
// type is module_test.go's stubQueue.
type stubQueue struct {
	tasks []jobs.Task
}

func (q *stubQueue) Enqueue(_ context.Context, task jobs.Task, _ ...jobs.EnqueueOption) (jobs.JobID, error) {
	q.tasks = append(q.tasks, task)
	return "stub-job", nil
}

func (q *stubQueue) Get(context.Context, jobs.JobID) (*jobs.Job, error) {
	panic("stubQueue.Get: not implemented")
}

func (q *stubQueue) Cancel(context.Context, jobs.JobID) error {
	panic("stubQueue.Cancel: not implemented")
}

// stubUserResolver is a notification.UserAddressResolver test double
// answering from a per-user table, with an injectable error for the seam's
// failure tests. The twin of this type is module_test.go's
// stubUserResolver.
type stubUserResolver struct {
	byUser map[string]notification.UserAddresses
	err    error
}

func (r *stubUserResolver) Resolve(_ context.Context, userID string) (notification.UserAddresses, error) {
	if r.err != nil {
		return notification.UserAddresses{}, r.err
	}
	return r.byUser[userID], nil
}

// codeOf returns the apperr code of err, failing the test when err is not
// an apperr carrying one -- a nil error or a non-apperr error is a
// different bug than the code the assertion expected.
func codeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error, got nil")
	}
	ae, ok := apperr.As(err)
	if !ok {
		t.Fatalf("expected an apperr, got %T: %v", err, err)
	}
	return ae.Code
}

// contactCodeRe matches any run of six digits -- the verification code's
// shape (see notification's contact_code.go). Last-match extraction is
// deliberate: the console SMS line begins with the recipient's phone
// number, itself a long digit run, so the code is always the FINAL
// six-digit run on the line.
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

// smsCodeAt returns the verification code of the i-th delivered SMS (0
// based, in delivery order), read back from the console sender's buffer.
func smsCodeAt(t *testing.T, buf *bytes.Buffer, i int) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if i >= len(lines) {
		t.Fatalf("want the %d-th SMS line, only %d delivered", i, len(lines))
	}
	return lastCode(t, lines[i])
}

// channelsJSON marshals a channel list into the datatypes.JSON the
// notification_preferences.channels column stores -- the column type is
// dbkit's portable JSON (never a PostgreSQL-native array), so a preference
// factory in this tier writes the same bytes the unit tier's
// channelsJSON helper (preference.go) does.
func channelsJSON(channels []string) datatypes.JSON {
	b, err := json.Marshal(channels)
	if err != nil {
		panic(fmt.Sprintf("channelsJSON: %v", err))
	}
	return datatypes.JSON(b)
}

// ---------------------------------------------------------------------------
// PostgreSQL helpers
// ---------------------------------------------------------------------------

// startPostgresContainer starts a disposable PostgreSQL 16 container for
// one test, already registered for termination via t.Cleanup -- the
// disposable-container pattern every Docker-backed tier in this workspace
// shares.
func startPostgresContainer(t *testing.T, ctx context.Context) *postgres.PostgresContainer {
	t.Helper()

	pgContainer, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("notification"),
		postgres.WithUsername("notification"),
		postgres.WithPassword("notification"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres testcontainer: %v", err)
	}
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(pgContainer); terminateErr != nil {
			t.Errorf("terminate postgres testcontainer: %v", terminateErr)
		}
	})
	return pgContainer
}

// openNotificationPostgres opens the container's database through
// dbkit.Open -- the only sanctioned way to obtain a *gorm.DB -- and
// applies notification's own migrations to it with the dialect they ship
// for, exactly the way a host applies them at startup. Nothing here calls
// AutoMigrate; the versioned SQL under migrations/postgres is what creates
// every table, which is what makes this the zero-to-head proof on the
// second dialect. The address serializer is
// registered too: every row the tier writes through a model carrying an
// encrypted address column needs the process-global cipher registration a
// host performs at bootstrap.
func openNotificationPostgres(t *testing.T, ctx context.Context, pgContainer *postgres.PostgresContainer) *gorm.DB {
	t.Helper()

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres testcontainer connection string: %v", err)
	}
	db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectPostgres, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open(DialectPostgres): %v", err)
	}
	registerContactSerializer()
	migrations := dbkit.NewMigrationRegistry()
	if err = migrations.Register(notification.NewModule(db)); err != nil {
		t.Fatalf("registering the notification migrations: %v", err)
	}
	if err = migrations.Apply(ctx, db, dbkit.DialectPostgres); err != nil {
		t.Fatalf("applying the notification migrations on PostgreSQL: %v", err)
	}
	return db
}

// localeCarrier is the declaration face's locale half: the module name --
// the id prefix its bundle's message ids merge under -- and the embedded
// bundle itself. Every pkgcore.Module carried by these two legs satisfies
// it, notification's own module and the clinic fixture alike.
type localeCarrier interface {
	Name() string
	Locales() embed.FS
}

// hostCatalog merges every booted module's locale bundle into the message
// catalog, module by module under the module's own name -- the same merge a
// real host performs over its selected components' assets and publishes
// into the by-type context before anything can render (a notification
// template, a verification-code message). A bundle whose ids do not carry
// its module's prefix fails the merge here, naming the module, exactly as
// the host's own catalog build would refuse it.
func hostCatalog(t *testing.T, modules ...localeCarrier) *i18n.Catalog {
	t.Helper()
	builder := i18n.NewBuilder()
	for _, m := range modules {
		if err := builder.AddModule(m.Name(), m.Locales()); err != nil {
			t.Fatalf("merge the %s message catalog: %v", m.Name(), err)
		}
	}
	return builder.Build()
}

// bootModule returns a fully wired notification.Module over db -- every
// required seam filled with the tier's fixtures (the SMS sender writing to
// smsBuf, the recording stubQueue), declared through the module's Register
// the way a host's assembly declares it, with the merged catalog (the
// module's own bundle) published into the registry in the same Init window
// as the host's own catalog step. The returned registry is the host's.
func bootModule(t *testing.T, ctx context.Context, db *gorm.DB) (*notification.Module, *pkgcore.ComponentRegistry, *bytes.Buffer) {
	t.Helper()

	smsBuf := new(bytes.Buffer)
	module := notification.NewModule(db,
		notification.WithSMSSender(pkgcore.NewConsoleSMSSender(smsBuf)),
		notification.WithMailFrom(testMailFrom),
		notification.WithContactEmailIndexer(emailIndexer(t)),
		notification.WithContactPhoneIndexer(phoneIndexer(t)),
		notification.WithDeliveryQueue(&stubQueue{}),
		notification.WithUserAddressResolver(&stubUserResolver{byUser: map[string]notification.UserAddresses{}}),
	)
	reg := componenttest.NewRegistry()
	if err := componenttest.DeclareAll(reg,
		module.Register,
		func(r *pkgcore.ComponentRegistry) error {
			r.Put(hostCatalog(t, module))
			return nil
		},
	); err != nil {
		t.Fatalf("declare the module and publish the merged catalog: %v", err)
	}
	return module, reg, smsBuf
}

// TestPostgres_MigrationsFromZero_CreateEveryTableFromZero proves the
// second dialect's migration set actually runs and produces the five
// tables the models are mapped onto. openNotificationPostgres has already
// applied it from an empty database; this asserts the result rather than
// trusting the absence of an error. The query goes through the raw
// database/sql handle -- the information_schema probe is not a GORM query
// against a model, and the module's own repositories are the sanctioned
// data paths for the module's tables, not for the catalog of tables
// itself.
func TestPostgres_MigrationsFromZero_CreateEveryTableFromZero(t *testing.T) {
	ctx := context.Background()
	db := openNotificationPostgres(t, ctx, startPostgresContainer(t, ctx))

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB(): %v", err)
	}
	for _, table := range []string{
		"in_app_messages",
		"notification_preferences",
		"verified_contacts",
		"platform_blacklist",
		"send_records",
	} {
		var exists bool
		if err = sqlDB.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1)`,
			table).Scan(&exists); err != nil {
			t.Fatalf("querying information_schema for %q: %v", table, err)
		}
		if !exists {
			t.Errorf("table %q was not created by the PostgreSQL migration set", table)
		}
	}
}

// TestPostgres_Repositories_AreTenantIsolated runs the mandatory suite
// against a real server rather than SQLite. The unit tier runs the same
// three assertions; repeating them here is what proves the isolation does
// not depend on a SQLite-only behaviour of the GORM plugin. The factories
// mirror their unit-tier twins (repository_test.go, preference_repository
// _test.go, contact_test.go) with one deliberate adaptation: every id is
// short and seq-derived, because a real server enforces the models'
// declared id sizes (varchar(36)) that SQLite ignores -- the unit twin of
// the preference factory below prefixes its id with the tenant id and
// would be refused by PostgreSQL. Each closure returns a fresh row on
// every call, distinct in every column the suite's rows could collide on,
// with the TenantModel left empty for the repository to stamp.
func TestPostgres_Repositories_AreTenantIsolated(t *testing.T) {
	ctx := context.Background()
	db := openNotificationPostgres(t, ctx, startPostgresContainer(t, ctx))

	t.Run("inbox messages", func(t *testing.T) {
		repo := notification.NewRepository(db)
		seq := 0
		tenancytest.AssertIsolated[notification.InboxMessage](t, repo.Repository, func(_ pkgcore.TenantID) *notification.InboxMessage {
			seq++
			return &notification.InboxMessage{
				ID:              fmt.Sprintf("inbox-%06d", seq),
				RecipientUserID: "user-7",
				TypeKey:         "note.shared",
				Title:           "Note 42 was shared with you",
				Body:            "Lin opened Note 42 and shared it with the whole clinic.",
			}
		})
	})

	t.Run("notification preferences", func(t *testing.T) {
		repo := notification.NewPreferenceRepository(db)
		seq := 0
		tenancytest.AssertIsolated[notification.NotificationPreference](t, repo.Repository, func(_ pkgcore.TenantID) *notification.NotificationPreference {
			seq++
			return &notification.NotificationPreference{
				ID:              fmt.Sprintf("pref-%06d", seq),
				RecipientUserID: fmt.Sprintf("user-%06d", seq),
				TypeKey:         "clinic.appointment_reminder",
				Channels:        channelsJSON([]string{notification.ChannelInApp, notification.ChannelEmail}),
			}
		})
	})

	t.Run("verified contacts", func(t *testing.T) {
		repo := notification.NewVerifiedContactRepository(db)
		seq := 0
		tenancytest.AssertIsolated[notification.VerifiedContact](t, repo.Repository, func(_ pkgcore.TenantID) *notification.VerifiedContact {
			seq++
			return &notification.VerifiedContact{
				ID:           fmt.Sprintf("contact-%06d", seq),
				Channel:      notification.ChannelEmail,
				Address:      fmt.Sprintf("patient-%06d@example.com", seq),
				AddressIndex: fmt.Sprintf("%064x", seq),
				Status:       notification.ContactStatusPending,
				ConsentBy:    notification.ContactConsentByDoubleOptIn,
			}
		})
	})
}

// TestPostgres_ConsentFlow_SingleWinnerVerifyAndDeliverabilityGate is the
// reason this leg exists at all, in two senses.
//
// The single-winner half is the compare-and-swap proof: one verification
// code, two concurrent VerifyCode calls, exactly one winner. The unit tier
// runs the same race on SQLite, whose writer lock serializes the two
// transactions -- the test passes there even if the conditional UPDATE
// were a read-then-write, because the second transaction simply cannot
// begin until the first committed. Real PostgreSQL runs the two
// transactions under genuine row-lock concurrency, which is the only
// setting in which a broken CAS would surface as two successes; go/authn's
// tier re-proves its refresh rotation on the same grounds. The replay half
// pins that the loser's code -- and any later replay -- answers with the
// deliberately indistinct ErrContactCodeInvalid.
//
// The deliverability half drives the consent ledger's state machine over
// the real database: a pending contact is refused before verification, the
// verified row passes the gate, an unsubscribe is permanent, and a bounced
// contact is refused with its own code. Each contact the flow creates uses
// a distinct phone number: verified_contacts' unique index is
// (tenant_id, channel, address_index), and the module's duplicate probe
// resolves an existing address to its existing row rather than starting a
// fresh flow.
func TestPostgres_ConsentFlow_SingleWinnerVerifyAndDeliverabilityGate(t *testing.T) {
	ctx := context.Background()
	db := openNotificationPostgres(t, ctx, startPostgresContainer(t, ctx))
	module, _, smsBuf := bootModule(t, ctx, db)
	svc := module.Contacts()
	const tenant = "tenant-acme"
	cctx := testkit.TenantCtx(tenant)

	// Contact A: an SMS double opt-in, still pending. The deliverability
	// gate must refuse it before verification.
	contactA, err := svc.CreateContact(cctx, notification.ContactCreateInput{
		Channel: notification.ChannelSMS,
		Address: "+8613800138000",
	})
	if err != nil {
		t.Fatalf("CreateContact A: %v", err)
	}
	if contactA.Status != notification.ContactStatusPending {
		t.Fatalf("fresh SMS contact status = %q, want pending", contactA.Status)
	}
	if got := codeOf(t, func() error {
		_, err := svc.EnsureDeliverable(cctx, contactA.ID)
		return err
	}()); got != notification.ErrContactNotVerified.Code {
		t.Errorf("EnsureDeliverable on a pending contact = code %q, want %q", got, notification.ErrContactNotVerified.Code)
	}

	// Two goroutines race one verification code through the real database.
	// Exactly one must win; the loser must answer ErrContactCodeInvalid,
	// the same answer a wrong code or a replay gets.
	code := smsCodeAt(t, smsBuf, 0)
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			_, err := svc.VerifyCode(cctx, notification.VerifyCodeInput{ContactID: contactA.ID, Code: code})
			results <- err
		}()
	}
	close(start)
	var winners, losers int
	for i := 0; i < 2; i++ {
		switch err := <-results; {
		case err == nil:
			winners++
		case codeOf(t, err) == notification.ErrContactCodeInvalid.Code:
			losers++
		default:
			t.Fatalf("VerifyCode raced with code %q: %v", code, err)
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("concurrent VerifyCode: %d winners, %d losers, want exactly one of each", winners, losers)
	}

	// The code is spent: a replay of the same code must be refused with
	// the indistinct invalid-code answer, never a second verification.
	if _, err := svc.VerifyCode(cctx, notification.VerifyCodeInput{ContactID: contactA.ID, Code: code}); err == nil {
		t.Fatalf("replaying a spent verification code succeeded")
	} else if got := codeOf(t, err); got != notification.ErrContactCodeInvalid.Code {
		t.Errorf("replayed code = code %q, want %q", got, notification.ErrContactCodeInvalid.Code)
	}

	// The verified row now passes the deliverability gate.
	verified, err := svc.EnsureDeliverable(cctx, contactA.ID)
	if err != nil {
		t.Fatalf("EnsureDeliverable on the verified contact: %v", err)
	}
	if verified.Status != notification.ContactStatusVerified {
		t.Fatalf("EnsureDeliverable returned status %q, want verified", verified.Status)
	}

	// Unsubscribing is permanent: the gate refuses the row with the
	// unsubscribed code from here on.
	if _, err := svc.Unsubscribe(cctx, notification.UnsubscribeInput{ContactID: contactA.ID}); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	if got := codeOf(t, func() error {
		_, err := svc.EnsureDeliverable(cctx, contactA.ID)
		return err
	}()); got != notification.ErrContactUnsubscribed.Code {
		t.Errorf("EnsureDeliverable on the unsubscribed contact = code %q, want %q", got, notification.ErrContactUnsubscribed.Code)
	}

	// Contact B: a business-attested SMS contact, verified from birth --
	// the attestation path, which sends no code. A delivery that bounces
	// marks it, and the gate then refuses it with the bounced code.
	contactB, err := svc.CreateContact(cctx, notification.ContactCreateInput{
		Channel:    notification.ChannelSMS,
		Address:    "+8613800138002",
		ConsentRef: "consent-contract-2026-09",
	})
	if err != nil {
		t.Fatalf("CreateContact B: %v", err)
	}
	if contactB.Status != notification.ContactStatusVerified {
		t.Fatalf("business-attested contact status = %q, want verified", contactB.Status)
	}
	if err := svc.MarkBounced(cctx, contactB.ID); err != nil {
		t.Fatalf("MarkBounced: %v", err)
	}
	if got := codeOf(t, func() error {
		_, err := svc.EnsureDeliverable(cctx, contactB.ID)
		return err
	}()); got != notification.ErrContactBounced.Code {
		t.Errorf("EnsureDeliverable on the bounced contact = code %q, want %q", got, notification.ErrContactBounced.Code)
	}

	// Contact C: a second SMS double opt-in left pending; its create
	// proves the ledger took a second distinct address (and that the
	// console buffer now holds two SMS lines -- the code extraction above
	// pinned the first). The pending gate was already proven on A; C only
	// needs to exist with the right status.
	contactC, err := svc.CreateContact(cctx, notification.ContactCreateInput{
		Channel: notification.ChannelSMS,
		Address: "+8613800138001",
	})
	if err != nil {
		t.Fatalf("CreateContact C: %v", err)
	}
	if contactC.Status != notification.ContactStatusPending {
		t.Fatalf("fresh SMS contact C status = %q, want pending", contactC.Status)
	}
	if smsLines := strings.Count(strings.TrimSpace(smsBuf.String()), "\n") + 1; smsLines != 2 {
		t.Errorf("console SMS delivered %d lines, want 2 (one per double opt-in send)", smsLines)
	}
}

// TestPostgres_SendRecordSaveGuarded_GuardStatementRunsAgainstARealServer
// executes the delivery log's guarded rewrite (notification's SaveGuarded)
// against real PostgreSQL -- the second-dialect execution the unit tier
// cannot provide, since it runs the statement only on SQLite: settle's
// guarded write is the statement whose dialect shape only the second
// dialect exercises. Its value-vs-value comparison of this write's status
// against the succeeded sentinel binds the sentinel as a SQL literal -- an
// expression with both sides as parameters resolves only through
// PostgreSQL's implicit text-typing of unknown parameters (42P18 the day
// either side stops being plain text). This test pins that every guarded
// outcome the statement promises -- the refused downgrade, the allowed
// rewrite of a non-succeeded row, and the allowed re-settle of a succeeded
// one -- lands through the real server's own semantics, with the guard's
// refusal a (false, nil) answer rather than an error.
func TestPostgres_SendRecordSaveGuarded_GuardStatementRunsAgainstARealServer(t *testing.T) {
	ctx := context.Background()
	db := openNotificationPostgres(t, ctx, startPostgresContainer(t, ctx))
	repo := notification.NewSendRecordRepository(db)

	seed := func(id, key, status string) *notification.SendRecord {
		t.Helper()
		rec := &notification.SendRecord{
			ID:              id,
			TenantID:        "tenant-acme",
			TypeKey:         "clinic.appointment_reminder",
			Channel:         notification.ChannelEmail,
			RecipientClass:  notification.RecipientClassUser,
			RecipientUserID: "user-7",
			Status:          status,
			IdempotencyKey:  key,
		}
		if err := repo.Create(ctx, rec); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
		return rec
	}
	readBack := func(key string) *notification.SendRecord {
		t.Helper()
		rec, err := repo.ByTenantAndKey(ctx, "tenant-acme", key)
		if err != nil {
			t.Fatalf("ByTenantAndKey(%s): %v", key, err)
		}
		if rec == nil {
			t.Fatalf("ByTenantAndKey(%s) = nil, want the seeded row", key)
		}
		return rec
	}

	// A settle that would downgrade a succeeded row is refused: (false,
	// nil), no error, and the row untouched -- the never-downgrade guard
	// decided by the UPDATE statement's own WHERE against a real server.
	succeeded := seed("sr-pg-0001", "delivery-key-pg-1", notification.SendRecordStatusSucceeded)
	loser := *succeeded
	loser.Status = notification.SendRecordStatusFailed
	loser.Error = "a later attempt failed"
	landed, err := repo.SaveGuarded(ctx, &loser)
	if err != nil {
		t.Fatalf("SaveGuarded downgrade on real PostgreSQL: %v", err)
	}
	if landed {
		t.Error("the downgrading guarded write landed, want the guard's (false, nil) refusal")
	}
	before := readBack("delivery-key-pg-1")
	if before.Status != notification.SendRecordStatusSucceeded || before.Error != "" {
		t.Errorf("row after the refused downgrade = status %q error %q, want the seeded succeeded row untouched", before.Status, before.Error)
	}

	// A re-settle of the same winner is allowed: the row is rewritten in
	// place, created_at preserved (the guard passes for a succeeded writer).
	// created_at is compared before-vs-after through the same read path: the
	// module's postgres migrations declare naive TIMESTAMP columns (the
	// SQLite-parity choice, no NOW() on either dialect), so a struct-side
	// time in the local zone never equals a driver-side readback in UTC --
	// the row-level property the guard promises is that the guarded UPDATE
	// left the column's stored value untouched, which two identical reads
	// pin directly.
	resettle := *succeeded
	resettle.Error = "re-settled by a later replay"
	landed, err = repo.SaveGuarded(ctx, &resettle)
	if err != nil {
		t.Fatalf("SaveGuarded re-settle on real PostgreSQL: %v", err)
	}
	if !landed {
		t.Error("the succeeded re-settle did not land, want the guard's (true, nil) allowance")
	}
	got := readBack("delivery-key-pg-1")
	if got.Status != notification.SendRecordStatusSucceeded || got.Error != "re-settled by a later replay" {
		t.Errorf("row after the re-settle = status %q error %q, want the rewrite landed", got.Status, got.Error)
	}
	if !got.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("re-settled row created_at = %v, want the pre-write read %v (a guarded write never touches created_at)", got.CreatedAt, before.CreatedAt)
	}

	// A write over a non-succeeded row is allowed: the guard exists only
	// to protect a succeeded row.
	failed := seed("sr-pg-0002", "delivery-key-pg-2", notification.SendRecordStatusFailed)
	failed.Error = "the first attempt failed"
	if err := repo.Save(ctx, failed); err != nil {
		t.Fatalf("Save the failed row's first error text: %v", err)
	}
	retry := *failed
	retry.Error = "the second attempt failed"
	landed, err = repo.SaveGuarded(ctx, &retry)
	if err != nil {
		t.Fatalf("SaveGuarded over the failed row on real PostgreSQL: %v", err)
	}
	if !landed {
		t.Error("the guarded write over the failed row did not land, want (true, nil)")
	}
	if got := readBack("delivery-key-pg-2"); got.Status != notification.SendRecordStatusFailed || got.Error != "the second attempt failed" {
		t.Errorf("row after the allowed rewrite = status %q error %q, want the second attempt's text", got.Status, got.Error)
	}

	// An id no row carries is a refusal, never a create and never an error.
	ghost := *succeeded
	ghost.ID = "sr-pg-9999"
	ghost.IdempotencyKey = "delivery-key-pg-ghost"
	ghost.Error = "a write for an id nobody seeded"
	landed, err = repo.SaveGuarded(ctx, &ghost)
	if err != nil {
		t.Fatalf("SaveGuarded for an absent id on real PostgreSQL: %v", err)
	}
	if landed {
		t.Error("the guarded write for an absent id landed, want (false, nil)")
	}
}
