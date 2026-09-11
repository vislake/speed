package rbac

// authorizer_service_bench_test.go benchmarks the authorization-decision hot path --
// docs/internal/20-quality-and-security.md's "single-decision latency and
// cache hit rate" hotspot, owned by this module. Every request a host
// authorizes pays one Service.Can; the benchmarks report that cost in the
// two states the decision cache's design is about: the steady-state cached
// decision (the common case) and the decision right after an invalidation,
// which pays the full bindings-and-role-permissions reload. The service is
// attached exactly as a host attaches it (module Register then Attach over
// an in-memory bus), seeded with a realistic tenant: two roles and one
// hundred subjects on a private temp-file SQLite database migrated from the
// module's real migration files. No external services.
//
// Run: go test -bench=BenchmarkServiceCan -benchmem .

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// serviceBenchSink keeps the last decision reachable so the compiler cannot
// elide a Can call.
var serviceBenchSink bool

// benchRBACTenant is the tenant every benchmarked role, subject and grant
// lives in.
const benchRBACTenant = pkgcore.TenantID("bench-tenant")

const (
	// benchMemberRole holds the tenant's ordinary member permissions; the
	// majority of subjects hold it.
	benchMemberRole = "bench-member"
	// benchOpsRole holds a broader set; a minority of subjects hold it.
	benchOpsRole = "bench-ops"
	// benchSubjectCount is the size of the benchmarked tenant's user set.
	benchSubjectCount = 100
)

// newBenchService attaches a Service over a private temp-file SQLite
// database migrated from the module's real migration files, registering the
// host permissions the way a real host would -- the identical sequence the
// module's own test fixture (authorizer_service_test.go's attachTestService) runs, with
// *testing.T swapped for *testing.TB.
func newBenchService(b *testing.B) *Service {
	b.Helper()

	dsn := filepath.Join(b.TempDir(), "bench.sqlite")
	db, openErr := dbkit.Open(context.Background(), dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     dsn,
	})
	if openErr != nil {
		b.Fatalf("open temp-file sqlite database: %v", openErr)
	}
	b.Cleanup(func() {
		sqlDB, dbErr := db.DB()
		if dbErr != nil {
			return
		}
		_ = sqlDB.Close()
	})

	registry := dbkit.NewMigrationRegistry()
	if regErr := registry.Register(NewModule(db)); regErr != nil {
		b.Fatalf("registering the rbac migrations: %v", regErr)
	}
	if regErr := registry.Apply(context.Background(), db, dbkit.DialectSQLite); regErr != nil {
		b.Fatalf("applying the rbac migrations: %v", regErr)
	}

	reg := componenttest.NewRegistry()
	module := NewModule(db)
	var svc *Service
	if err := componenttest.DeclareAll(reg,
		func(r *pkgcore.ComponentRegistry) error { return r.Permissions.Add(testPermissions...) },
		module.Register,
		func(r *pkgcore.ComponentRegistry) error {
			attached, err := module.Attach(r)
			if err != nil {
				return err
			}
			svc = attached
			return nil
		},
	); err != nil {
		b.Fatalf("declare the host's permissions and attach the module: %v", err)
	}
	b.Cleanup(func() {
		if err := svc.Close(); err != nil {
			b.Errorf("Close: %v", err)
		}
	})
	return svc
}

// seedBenchTenant defines the two tenant roles and binds benchSubjectCount
// subjects to them: the first 80% of the user ids hold the member role, the
// rest the ops role, every binding tenant-wide -- the ordinary shape of a
// workspace whose members are visible everywhere. It returns the subjects,
// in the same order the roles were assigned, for the benchmarks to rotate
// over.
func seedBenchTenant(b *testing.B, svc *Service) []Subject {
	b.Helper()
	ctx := tenantCtx(benchRBACTenant)

	if _, err := svc.DefineRole(ctx, RoleDefinition{
		Key:            benchMemberRole,
		DescriptionKey: "rbac.role.member",
		Permissions:    []string{"notes:read", "notes:write"},
	}); err != nil {
		b.Fatalf("DefineRole(%q): %v", benchMemberRole, err)
	}
	if _, err := svc.DefineRole(ctx, RoleDefinition{
		Key:            benchOpsRole,
		DescriptionKey: "rbac.role.member",
		Permissions:    []string{"notes:read", "billing:manage"},
	}); err != nil {
		b.Fatalf("DefineRole(%q): %v", benchOpsRole, err)
	}

	subjects := make([]Subject, benchSubjectCount)
	for i := range subjects {
		sub := Subject{TenantID: benchRBACTenant, UserID: fmt.Sprintf("user-%04d", i)}
		role := benchMemberRole
		if i >= benchSubjectCount*4/5 {
			role = benchOpsRole
		}
		if err := svc.AssignRole(ctx, sub, role, Scope{}); err != nil {
			b.Fatalf("AssignRole(%q): %v", role, err)
		}
		subjects[i] = sub
	}
	return subjects
}

// warmBenchSubjects runs one Can per subject so every subject's grant set is
// cached before the timed loop starts.
func warmBenchSubjects(b *testing.B, svc *Service, subjects []Subject) {
	b.Helper()
	ctx := context.Background()
	for _, sub := range subjects {
		ok, err := svc.Can(ctx, sub, "read", "notes")
		if err != nil {
			b.Fatalf("Can() error = %v", err)
		}
		if !ok {
			b.Fatalf("Can(%s) = false for a subject holding notes:read", sub.UserID)
		}
	}
}

// BenchmarkServiceCan measures one authorization decision. The first two
// sub-benchmarks are the steady state every cached request pays -- a
// granted permission and a denied one, both answered from the subject's
// cached grant set. The third measures the decision immediately after an
// invalidation of the subject's cache entry (the state a revoke or binding
// change leaves behind), which pays the full reload: the subject's binding
// rows and their roles' permission rows from the database, then the cache
// write.
func BenchmarkServiceCan(b *testing.B) {
	ctx := context.Background()
	b.ReportAllocs()

	b.Run("cached-allow", func(b *testing.B) {
		svc := newBenchService(b)
		subjects := seedBenchTenant(b, svc)
		warmBenchSubjects(b, svc, subjects)
		b.ResetTimer()
		var last bool
		for i := 0; i < b.N; i++ {
			ok, err := svc.Can(ctx, subjects[i%len(subjects)], "read", "notes")
			if err != nil {
				b.Fatalf("Can() error = %v", err)
			}
			last = ok
		}
		serviceBenchSink = last
	})

	b.Run("cached-deny", func(b *testing.B) {
		svc := newBenchService(b)
		subjects := seedBenchTenant(b, svc)
		warmBenchSubjects(b, svc, subjects)
		b.ResetTimer()
		var last bool
		for i := 0; i < b.N; i++ {
			ok, err := svc.Can(ctx, subjects[i%len(subjects)], "delete", "notes")
			if err != nil {
				b.Fatalf("Can() error = %v", err)
			}
			last = ok
		}
		serviceBenchSink = last
	})

	b.Run("decision-after-invalidation", func(b *testing.B) {
		svc := newBenchService(b)
		subjects := seedBenchTenant(b, svc)
		warmBenchSubjects(b, svc, subjects)
		sub := subjects[0]
		b.ResetTimer()
		var last bool
		for i := 0; i < b.N; i++ {
			// The same invalidation a revoke of this subject publishes
			// (Service.onRoleBindingChanged); the next decision must reload
			// the grant set from the database.
			svc.cache.invalidate(grantKey{tenant: sub.TenantID, user: sub.UserID})
			ok, err := svc.Can(ctx, sub, "read", "notes")
			if err != nil {
				b.Fatalf("Can() error = %v", err)
			}
			last = ok
		}
		serviceBenchSink = last
	})
}
