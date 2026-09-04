package integration_test

// Runnable documentation for integration's public API, mirroring
// go/pki/example_test.go's convention: this example is compiled AND
// executed by `go test`, so a change to this module's public API that
// breaks the documented usage fails the build rather than only rotting in
// prose.
//
// It covers round 1's full lifecycle in one pass: issuing an API key (the
// raw value is available exactly once, right here), checking a request
// against the three-layer rate limiter, and revoking the key.

import (
	"context"
	"fmt"
	"strings"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/ratelimit"

	"github.com/vislake/speed/go/integration"
)

func Example() {
	ctx := context.Background()

	// A real host opens PostgreSQL in the distributed deployment mode
	// (dbkit.DialectPostgres). SQLite keeps this example self-contained
	// under `go test`, with no external service required -- which is
	// exactly what the standalone deployment mode does in production too.
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:integration_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	// integration itself needs no host-supplied permission lister for a
	// key requesting zero scopes; a real host wires one over its own
	// rbac.Service (see PermissionLister's own doc comment in seams.go).
	// This example wires a fixed one so it can demonstrate a non-empty
	// Scopes request too.
	permissions := integration.PermissionListerFunc(
		func(ctx context.Context, tenantID, userID string) ([]string, error) {
			return []string{"notes:read"}, nil
		},
	)
	m := integration.NewModule(db, integration.WithPermissionLister(permissions))

	// Migrations are versioned SQL, applied through dbkit's registry. There
	// is no AutoMigrate anywhere in this codebase.
	registry := dbkit.NewMigrationRegistry()
	if regErr := registry.Register(m); regErr != nil {
		fmt.Println("register migrations:", regErr)
		return
	}
	if applyErr := registry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations:", applyErr)
		return
	}

	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if regErr := m.Register(reg); regErr != nil {
		fmt.Println("register module:", regErr)
		return
	}
	svc, err := m.Attach(reg)
	if err != nil {
		fmt.Println("attach:", err)
		return
	}

	// APIKey is tenant data, so every call needs a tenant in ctx -- the
	// same rule every tenant-scoped repository in this codebase enforces.
	tenantCtx := pkgcore.WithTenant(ctx, pkgcore.TenantID("acme-dental"))

	// Create returns the raw key exactly once -- nothing else this module
	// ever returns reproduces it.
	created, err := svc.Create(tenantCtx, integration.CreateInput{
		CreatedBy: "user-1",
		Scopes:    []string{"notes:read"},
	})
	if err != nil {
		fmt.Println("create:", err)
		return
	}
	// created.Key is the raw plaintext credential -- Create's one and only
	// return of it. Its value is random and therefore not itself printable
	// as deterministic golden output, but its length (apiKeyLiteralPrefix
	// plus the base64url encoding of apiKeyTokenBytes of entropy, always the
	// same number of characters) and its literal prefix are fixed, so
	// printing them still visibly exercises "the raw key came back" rather
	// than only Prefix, the display-safe echo that List can also return.
	fmt.Printf("issued a key, prefix length: %d, key length: %d, key has literal prefix: %v\n",
		len(created.Prefix), len(created.Key), strings.HasPrefix(created.Key, "sk_"))

	// Check an inbound request against the three-layer limiter before
	// letting it through -- this module composes the three
	// go/ratelimit.Allow calls itself; go/ratelimit is never modified.
	limiter := integration.NewLayeredLimiter(
		ratelimit.New(pkgcore.NewMemoryKVStore()),
		integration.LayeredLimits{Key: ratelimit.Limit{Rate: 100, Per: 60_000_000_000}}, // 100 per minute
	)
	decision, err := limiter.Allow(tenantCtx, "global", "acme-dental", created.ID)
	if err != nil {
		fmt.Println("rate limit check:", err)
		return
	}
	fmt.Println("request allowed:", decision.Allowed)

	// Revoke the key once it is no longer needed.
	if err := svc.Revoke(tenantCtx, created.ID); err != nil {
		fmt.Println("revoke:", err)
		return
	}
	fmt.Println("key revoked")

	// Output:
	// issued a key, prefix length: 11, key length: 46, key has literal prefix: true
	// request allowed: true
	// key revoked
}
