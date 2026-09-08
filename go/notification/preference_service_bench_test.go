package notification

// preference_service_bench_test.go benchmarks the delivery-decision gate a
// user delivery pays per (recipient, type): PreferenceService.
// ResolveForDelivery reads the live type taxonomy and then the recipient's
// stored preference row -- the check the module re-runs at send time for
// every delivery, by design (delivery.go's runDelivery). The number this
// reports is the per-delivery cost of that re-check on real rows, which is
// what a cache or a narrower lookup would have to beat. Part of the
// module-owned benchmark set docs/internal/20-quality-and-security.md's
// performance plan calls for; the database is a private temp-file SQLite
// migrated from the module's real migration files, no external services.
//
// Run: go test -bench=BenchmarkResolveForDelivery -benchmem .

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// preferenceBenchSink keeps the last resolution's channels reachable so the
// compiler cannot elide the resolution.
var preferenceBenchSink []string

const (
	// benchPrefTenant is the tenant every seeded preference row lives in.
	benchPrefTenant = "bench-tenant"
	// benchPrefUserCount is how many recipients carry a stored preference
	// for the rotating-recipient sub-benchmark -- the same order of
	// magnitude as one tenant's active user set in the reference app.
	benchPrefUserCount = 128
)

// benchPrefTypes is the type taxonomy the benchmarks resolve against: a
// representative spread across the domains a real host declares
// notification types for, one group per domain with a transactional
// (unsubscribable) member in each. It is a stand-in for the host's live
// registrar, exactly like the fixture taxonomy the module's own tests use.
var benchPrefTypes = []pkgcore.NotificationType{
	{Key: "clinic.appointment_reminder", Group: "appointments", DefaultChannels: []string{ChannelInApp, ChannelEmail, ChannelSMS}, Unsubscribable: true},
	{Key: "clinic.result_ready", Group: "results", DefaultChannels: []string{ChannelInApp, ChannelEmail}, Unsubscribable: true},
	{Key: "clinic.security_alert", Group: "security", DefaultChannels: []string{ChannelInApp, ChannelEmail, ChannelSMS}, Unsubscribable: false},
	{Key: "authn.verification_code", Group: "security", DefaultChannels: []string{ChannelEmail, ChannelSMS}, Unsubscribable: false},
	{Key: "authn.session_ended", Group: "security", DefaultChannels: []string{ChannelInApp, ChannelEmail}, Unsubscribable: true},
	{Key: "org.invitation_received", Group: "org", DefaultChannels: []string{ChannelEmail, ChannelInApp}, Unsubscribable: false},
	{Key: "org.member_joined", Group: "org", DefaultChannels: []string{ChannelInApp}, Unsubscribable: true},
	{Key: "note.shared", Group: "notes", DefaultChannels: []string{ChannelInApp, ChannelEmail}, Unsubscribable: true},
	{Key: "billing.invoice_paid", Group: "billing", DefaultChannels: []string{ChannelEmail, ChannelInApp}, Unsubscribable: false},
	{Key: "billing.credit_low", Group: "billing", DefaultChannels: []string{ChannelInApp, ChannelEmail}, Unsubscribable: true},
	{Key: "storage.export_ready", Group: "storage", DefaultChannels: []string{ChannelEmail, ChannelInApp}, Unsubscribable: true},
	{Key: "sharing.link_accessed", Group: "sharing", DefaultChannels: []string{ChannelInApp}, Unsubscribable: true},
	{Key: "integration.webhook_failed", Group: "integration", DefaultChannels: []string{ChannelEmail}, Unsubscribable: false},
}

// benchAppointmentType is the type every sub-benchmark resolves: the fixture
// appointment reminder, which a stored preference may narrow.
const benchAppointmentType = "clinic.appointment_reminder"

// newBenchPreferenceService returns a PreferenceService over a private
// temp-file SQLite database migrated from the module's real migration files,
// with the benchPrefTypes taxonomy attached -- the state every real call
// runs in after Module.Register. The migration step mirrors the module's
// internal/testutil.NewSQLite, whose *testing.T signature a benchmark cannot
// satisfy.
func newBenchPreferenceService(b *testing.B) *PreferenceService {
	b.Helper()
	dsn := filepath.Join(b.TempDir(), "bench.sqlite")
	db, err := dbkit.Open(context.Background(), dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     dsn,
	})
	if err != nil {
		b.Fatalf("open temp-file sqlite database: %v", err)
	}
	b.Cleanup(func() {
		sqlDB, dbErr := db.DB()
		if dbErr != nil {
			return
		}
		_ = sqlDB.Close()
	})

	registry := dbkit.NewMigrationRegistry()
	if err := registry.Register(NewModule(db)); err != nil {
		b.Fatalf("register the notification migrations: %v", err)
	}
	if err := registry.Apply(context.Background(), db, dbkit.DialectSQLite); err != nil {
		b.Fatalf("apply the notification migrations: %v", err)
	}

	svc := NewPreferenceService(db)
	svc.attachTypes(fixtureRegistrar{types: benchPrefTypes})
	return svc
}

// seedBenchPreferenceRows stores one preference row per recipient in users:
// each has switched the appointment reminder down to the in-app and email
// channels, so the seeded rows are real decisions a delivery re-checks.
func seedBenchPreferenceRows(b *testing.B, svc *PreferenceService, ctx context.Context, users []string) {
	b.Helper()
	for _, u := range users {
		if err := svc.Set(ctx, u, benchAppointmentType, []string{ChannelInApp, ChannelEmail}); err != nil {
			b.Fatalf("Set(%q): %v", u, err)
		}
	}
}

// BenchmarkResolveForDelivery measures one channel-resolution for one
// delivery: the taxonomy lookup plus the recipient's preference row read,
// under three realistic row states. "no-stored-preference" is the recipient
// who never chose, whose answer is the type's declared defaults;
// "stored-preference" is the same recipient re-checked on every delivery;
// "stored-preference-rotating-recipients" walks a tenant-sized set of
// recipients who all chose, the shape a busy tenant's delivery load takes.
func BenchmarkResolveForDelivery(b *testing.B) {
	svc := newBenchPreferenceService(b)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID(benchPrefTenant))

	// One recipient with a stored preference, one without.
	stored := "user-1042"
	if err := svc.Set(ctx, stored, benchAppointmentType, []string{ChannelInApp, ChannelSMS}); err != nil {
		b.Fatalf("Set(%q): %v", stored, err)
	}
	absent := "user-9999"

	users := make([]string, benchPrefUserCount)
	for i := range users {
		users[i] = fmt.Sprintf("user-%04d", i)
	}
	seedBenchPreferenceRows(b, svc, ctx, users)

	b.ReportAllocs()
	b.Run("no-stored-preference", func(b *testing.B) {
		b.ResetTimer()
		var last []string
		for i := 0; i < b.N; i++ {
			_, channels, err := svc.ResolveForDelivery(ctx, absent, benchAppointmentType)
			if err != nil {
				b.Fatalf("ResolveForDelivery() error = %v", err)
			}
			last = channels
		}
		preferenceBenchSink = last
	})
	b.Run("stored-preference", func(b *testing.B) {
		b.ResetTimer()
		var last []string
		for i := 0; i < b.N; i++ {
			_, channels, err := svc.ResolveForDelivery(ctx, stored, benchAppointmentType)
			if err != nil {
				b.Fatalf("ResolveForDelivery() error = %v", err)
			}
			last = channels
		}
		preferenceBenchSink = last
	})
	b.Run("stored-preference-rotating-recipients", func(b *testing.B) {
		b.ResetTimer()
		var last []string
		for i := 0; i < b.N; i++ {
			_, channels, err := svc.ResolveForDelivery(ctx, users[i%len(users)], benchAppointmentType)
			if err != nil {
				b.Fatalf("ResolveForDelivery() error = %v", err)
			}
			last = channels
		}
		preferenceBenchSink = last
	})
}
