package tenancytest

import (
	"context"
	"errors"
	"hash/fnv"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// recordsPerTenant is how many records AssertIsolated creates for each of
// its two tenants. More than one matters: it is what makes the List
// assertions meaningful — a single-row list cannot distinguish "correctly
// filtered by tenant" from "returned everything in the table". It is a
// package-level constant, not a magic number inline, per the project's
// configuration standard.
const recordsPerTenant = 2

// idFieldName and tenantIDFieldName name the exported string fields every T
// usable with dbkit.Repository[T] is already required to declare — see
// dbkit's repository.go, which documents this convention and reaches T's id
// and tenant through the identical reflection, for the identical reason:
// dbkit.TenantScoped exposes only GetTenantID, so nothing else can name
// these generically.
//
// AssertIsolated reuses this exact convention instead of asking every
// caller's T to additionally satisfy some new interface (an "HasID"
// accessor, say) on top of what dbkit.TenantScoped and Repository[T] already
// demand: any T a caller can already build a *dbkit.Repository[T] for
// already has both fields, by construction, so requiring nothing more here
// is what keeps AssertIsolated[T dbkit.TenantScoped]'s constraint exactly as
// narrow as Repository[T]'s own.
const (
	idFieldName       = "ID"
	tenantIDFieldName = "TenantID"
)

// AssertIsolated verifies that a dbkit.Repository[T] correctly isolates rows
// by tenant. It creates two tenants' worth of homogeneous data via
// newRecord, then asserts cross-tenant reads/writes are denied exactly the
// way dbkit's own tenant_scope_test.go and repository_test.go already prove
// dbkit itself enforces — this function exists so every OTHER module's
// Repository[T] usage gets the same assurance without re-deriving these test
// cases from scratch.
//
// newRecord must return a distinct record — a distinct value in T's "ID"
// field, per the convention documented above idFieldName — on every call,
// even for the same tenant; AssertIsolated calls it repeatedly per tenant to
// build a realistic multi-row dataset, and fails the test immediately if it
// ever sees a repeated id, since every assertion keyed on that id would
// otherwise be ambiguous rather than a genuine isolation result.
//
// AssertIsolated assumes repo's underlying table starts with no rows
// belonging to either tenant it generates (their ids are derived from
// t.Name(), so two different tests, or two different calls within one test
// using distinct *testing.T values, never collide with each other) — this
// holds automatically for the common case of a freshly migrated
// dbtest.NewSQLite(t) or dbtest.NewPostgres(t) database, which is what every
// example in this package's own tests uses. The derived id is bounded in
// length (see isolationTenants and maxTenantIDLen below) so a long,
// descriptive t.Name() cannot silently overflow a realistic tenant_id
// column on PostgreSQL while SQLite accepts it unchecked.
//
// What AssertIsolated checks, in order: creating rows for two tenants;
// FindByID succeeding for the owning tenant and returning
// dbkit.ErrRecordNotFound for the other; List returning exactly the calling
// tenant's own rows, never the other tenant's; Update and Delete succeeding
// for the owning tenant; a cross-tenant Update or Delete attempt returning
// dbkit.ErrRecordNotFound while leaving the real row and both tenants'
// stored data untouched — including that it never falls back to creating a
// phantom row under the attacking tenant, the specific failure mode
// repository.go's own Update doc comment warns about; a forged TenantID set
// directly on the struct passed to Create being silently overwritten by the
// context's real tenant rather than trusted; and Create/List on a context
// carrying no tenant at all failing closed with pkgcore.ErrNoTenant.
//
// AssertIsolated calls t.Run for each of the checks above, so a failure's
// subtest name says which property broke; do not add t.Parallel() to those
// subtests if you are adjusting this function, since several deliberately
// run in a fixed order against state earlier subtests already created
// (Update/Delete-related checks reuse the very ids the initial create step
// produced).
func AssertIsolated[T dbkit.TenantScoped](t *testing.T, repo *dbkit.Repository[T], newRecord func(tenant pkgcore.TenantID) *T) {
	t.Helper()

	seenIDs := make(map[string]bool)
	mustNewRecord := func(tenant pkgcore.TenantID) (*T, string) {
		t.Helper()
		rec := newRecord(tenant)
		if rec == nil {
			t.Fatalf("newRecord(%q) returned nil", tenant)
		}
		id, ok := stringField(rec, idFieldName)
		if !ok || id == "" {
			t.Fatalf("newRecord(%q) produced a record with no usable %q field; T must declare an exported string field named %q, the same convention dbkit.Repository[T] itself requires (see dbkit's repository.go)", tenant, idFieldName, idFieldName)
		}
		if seenIDs[id] {
			t.Fatalf("newRecord(%q) returned id %q more than once; it must return a distinct record on every call", tenant, id)
		}
		seenIDs[id] = true
		return rec, id
	}

	tenantA, tenantB := isolationTenants(t)
	ctxA := pkgcore.WithTenant(context.Background(), tenantA)
	ctxB := pkgcore.WithTenant(context.Background(), tenantB)

	aIDs := make([]string, 0, recordsPerTenant)
	for i := 0; i < recordsPerTenant; i++ {
		rec, id := mustNewRecord(tenantA)
		if err := repo.Create(ctxA, rec); err != nil {
			t.Fatalf("Create(tenant %q, record %d) error = %v", tenantA, i, err)
		}
		aIDs = append(aIDs, id)
	}
	bIDs := make([]string, 0, recordsPerTenant)
	for i := 0; i < recordsPerTenant; i++ {
		rec, id := mustNewRecord(tenantB)
		if err := repo.Create(ctxB, rec); err != nil {
			t.Fatalf("Create(tenant %q, record %d) error = %v", tenantB, i, err)
		}
		bIDs = append(bIDs, id)
	}

	t.Run("list_scopes_to_calling_tenant", func(t *testing.T) {
		t.Helper()
		assertListScopedTo(t, repo, ctxA, tenantA, aIDs, tenantB, bIDs)
		assertListScopedTo(t, repo, ctxB, tenantB, bIDs, tenantA, aIDs)
	})

	t.Run("same_tenant_find_succeeds", func(t *testing.T) {
		t.Helper()
		for _, id := range aIDs {
			assertFindOwnedBy(t, repo, ctxA, tenantA, id)
		}
		for _, id := range bIDs {
			assertFindOwnedBy(t, repo, ctxB, tenantB, id)
		}
	})

	t.Run("cross_tenant_find_denied", func(t *testing.T) {
		t.Helper()
		for _, id := range aIDs {
			assertFindDenied(t, repo, ctxB, tenantB, id)
		}
		for _, id := range bIDs {
			assertFindDenied(t, repo, ctxA, tenantA, id)
		}
	})

	t.Run("same_tenant_update_succeeds", func(t *testing.T) {
		t.Helper()
		id := aIDs[0]
		rec, err := repo.FindByID(ctxA, id)
		if err != nil {
			t.Fatalf("FindByID(owning tenant %q, %q) error = %v", tenantA, id, err)
		}
		if err := repo.Update(ctxA, rec); err != nil {
			t.Errorf("Update(owning tenant %q, %q) error = %v, want success", tenantA, id, err)
		}
	})

	t.Run("cross_tenant_update_denied_without_corrupting_or_creating_a_phantom_row", func(t *testing.T) {
		t.Helper()
		id := aIDs[0]
		victim, err := repo.FindByID(ctxA, id)
		if err != nil {
			t.Fatalf("FindByID(owning tenant %q, %q) before the update attempt error = %v", tenantA, id, err)
		}

		if err := repo.Update(ctxB, victim); !isRecordNotFound(err) {
			t.Errorf("Update(other tenant %q, %q) error = %v, want dbkit.ErrRecordNotFound", tenantB, id, err)
		}

		assertFindOwnedBy(t, repo, ctxA, tenantA, id)
		assertFindDenied(t, repo, ctxB, tenantB, id)
	})

	t.Run("cross_tenant_delete_denied", func(t *testing.T) {
		t.Helper()
		id := aIDs[1]
		if err := repo.Delete(ctxB, id); !isRecordNotFound(err) {
			t.Errorf("Delete(other tenant %q, %q) error = %v, want dbkit.ErrRecordNotFound", tenantB, id, err)
		}
		assertFindOwnedBy(t, repo, ctxA, tenantA, id)
	})

	t.Run("same_tenant_delete_succeeds", func(t *testing.T) {
		t.Helper()
		id := aIDs[1]
		if err := repo.Delete(ctxA, id); err != nil {
			t.Fatalf("Delete(owning tenant %q, %q) error = %v, want success", tenantA, id, err)
		}
		assertFindDenied(t, repo, ctxA, tenantA, id)
	})

	t.Run("create_overwrites_a_forged_tenant_id_with_the_context_tenant", func(t *testing.T) {
		t.Helper()
		rec, id := mustNewRecord(tenantA)
		// Deliberately forge the struct's TenantID to a different tenant
		// before Create, using the same field-name convention documented
		// above idFieldName/tenantIDFieldName — proving Create backfills
		// TenantID from ctx regardless of what the caller's struct held,
		// the same property repository_test.go's own
		// TestRepository_Create_ForgedTenantIDOnModel_IsOverwrittenByContextTenant
		// proves for dbkit's own fixture.
		setStringField(t, rec, tenantIDFieldName, string(tenantB))

		if err := repo.Create(ctxA, rec); err != nil {
			t.Fatalf("Create(tenant %q, forged-TenantID record) error = %v", tenantA, err)
		}

		assertFindOwnedBy(t, repo, ctxA, tenantA, id)
		assertFindDenied(t, repo, ctxB, tenantB, id)
	})

	t.Run("no_tenant_in_context_fails_closed", func(t *testing.T) {
		t.Helper()
		noTenant := context.Background()

		rec, _ := mustNewRecord(tenantA)
		if err := repo.Create(noTenant, rec); !errors.Is(err, pkgcore.ErrNoTenant) {
			t.Errorf("Create(no tenant in context) error = %v, want errors.Is(err, pkgcore.ErrNoTenant)", err)
		}
		if _, err := repo.List(noTenant); !errors.Is(err, pkgcore.ErrNoTenant) {
			t.Errorf("List(no tenant in context) error = %v, want errors.Is(err, pkgcore.ErrNoTenant)", err)
		}
	})
}

// assertListScopedTo asserts that repo.List(ctx), called for owner (holding
// ownIDs), contains every id in ownIDs and none of otherTenant's otherIDs.
// It checks containment rather than the list's exact length or contents, so
// AssertIsolated never has to assume repo's table was empty before it ran.
func assertListScopedTo[T dbkit.TenantScoped](t *testing.T, repo *dbkit.Repository[T], ctx context.Context, owner pkgcore.TenantID, ownIDs []string, otherTenant pkgcore.TenantID, otherIDs []string) {
	t.Helper()

	rows, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List(tenant %q) error = %v", owner, err)
	}
	present := idSet(rows)

	for _, id := range ownIDs {
		if !present[id] {
			t.Errorf("List(tenant %q) is missing id %q, created under that same tenant", owner, id)
		}
	}
	for _, id := range otherIDs {
		if present[id] {
			t.Errorf("List(tenant %q) unexpectedly contains id %q, which belongs to tenant %q", owner, id, otherTenant)
		}
	}
}

// assertFindOwnedBy asserts that repo.FindByID(ctx, id) succeeds and returns
// a row reporting tenant as its owner.
func assertFindOwnedBy[T dbkit.TenantScoped](t *testing.T, repo *dbkit.Repository[T], ctx context.Context, tenant pkgcore.TenantID, id string) {
	t.Helper()
	got, err := repo.FindByID(ctx, id)
	if err != nil {
		t.Errorf("FindByID(tenant %q, %q) error = %v, want success", tenant, id, err)
		return
	}
	if gotTenant := (*got).GetTenantID(); gotTenant != tenant {
		t.Errorf("FindByID(tenant %q, %q).GetTenantID() = %q, want %q", tenant, id, gotTenant, tenant)
	}
}

// assertFindDenied asserts that repo.FindByID(ctx, id), called under a
// tenant that does not own id, returns dbkit.ErrRecordNotFound — never the
// row, and never a different, more revealing error (see dbkit's own
// ErrRecordNotFound doc comment on why the two "not found" cases collapse
// together deliberately).
func assertFindDenied[T dbkit.TenantScoped](t *testing.T, repo *dbkit.Repository[T], ctx context.Context, tenant pkgcore.TenantID, id string) {
	t.Helper()
	got, err := repo.FindByID(ctx, id)
	if !isRecordNotFound(err) {
		t.Errorf("FindByID(tenant %q, %q) = (%v, %v), want (nil, dbkit.ErrRecordNotFound)", tenant, id, got, err)
	}
}

// isRecordNotFound reports whether err is dbkit.ErrRecordNotFound, matched
// by Code rather than identity: apperr's WithParam/WithCause always derive a
// new *apperr.Error rather than mutate the receiver (see apperr's own doc
// comment, and dbkit/AGENTS.md's Rules section), so the pointer a Repository
// method returns is never the same pointer as the package-level sentinel.
func isRecordNotFound(err error) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == dbkit.ErrRecordNotFound.Code
}

// idSet reads records' "ID" field (see idFieldName) into a set, for
// order-independent, count-independent containment checks.
func idSet[T any](records []T) map[string]bool {
	set := make(map[string]bool, len(records))
	for i := range records {
		if id, ok := stringField(&records[i], idFieldName); ok {
			set[id] = true
		}
	}
	return set
}

// maxTenantIDLen bounds every tenant id isolationTenants derives from a
// test name. It matches the tenant_id column width every fixture in this
// package declares (size:64) — the same width the backend coding
// standard's own Subscription example uses. AssertIsolated has no generic,
// dialect-independent way to discover the actual width of a caller's own
// tenant_id column, so a caller whose model declares a narrower one
// (dbkit's own internal Widget fixture, for instance, uses a ULID-width
// 26) still needs a short enough t.Name() that the derived id fits that
// narrower column too; only PostgreSQL enforces VARCHAR(n) length
// strictly; SQLite alone will not catch it.
const maxTenantIDLen = 64

// isolationTenantPrefix and isolationTenantSuffixLen are the fixed,
// non-negotiable parts of every id isolationTenants builds — "tenancytest-"
// plus either "-a" or "-b" — that isolationNameBudget below must leave
// room for.
const (
	isolationTenantPrefix    = "tenancytest-"
	isolationTenantSuffixLen = 2 // len("-a") == len("-b")
)

// isolationNameBudget is how many characters of a (sanitized) test name
// isolationTenants can embed verbatim and still keep the assembled id
// within maxTenantIDLen.
const isolationNameBudget = maxTenantIDLen - len(isolationTenantPrefix) - isolationTenantSuffixLen

// isolationTenants returns two distinct tenant ids derived from t's own
// name, so two different tests — or two calls within the same test using
// distinct *testing.T subtests — never collide on tenant id, and any
// failure message's tenant id already names the test that produced it.
//
// The name segment is bounded to isolationNameBudget (see maxTenantIDLen);
// see boundedTenantIDSegment for what happens once a descriptive t.Name()
// — which this project's own testing convention explicitly asks for, see
// backend-coding-standards §13 — would not otherwise fit.
func isolationTenants(t *testing.T) (a, b pkgcore.TenantID) {
	t.Helper()
	name := boundedTenantIDSegment(t.Name())
	return pkgcore.TenantID(isolationTenantPrefix + name + "-a"), pkgcore.TenantID(isolationTenantPrefix + name + "-b")
}

// boundedTenantIDSegment sanitizes name exactly as sanitizeForTenantID
// always has, then, only once the result would overflow
// isolationNameBudget, replaces the overflowing tail with a short
// deterministic hash of the full, original name — so two long names that
// happen to share a long common prefix still never collide once
// shortened, and a given t.Name() always derives the same segment across
// runs. A name that already fits is returned exactly as
// sanitizeForTenantID would have produced it on its own, so every
// existing, already-short test name keeps deriving the exact id it always
// has.
//
// Without this bound, a sufficiently descriptive t.Name() silently
// produces a tenant id SQLite accepts unchecked but that PostgreSQL
// rejects outright with "value too long for type character varying(64)"
// (SQLSTATE 22001) — a failure that reads as unrelated to the isolation
// property AssertIsolated actually checks.
func boundedTenantIDSegment(name string) string {
	sanitized := sanitizeForTenantID(name)
	if len(sanitized) <= isolationNameBudget {
		return sanitized
	}

	sum := fnv.New64a()
	_, _ = sum.Write([]byte(name)) // hash.Hash64's Write never returns an error
	hash := strconv.FormatUint(sum.Sum64(), 36)

	keep := isolationNameBudget - len(hash) - 1 // -1 for the separating '-'
	if keep < 0 {
		keep = 0
	}
	return sanitized[:keep] + "-" + hash
}

// sanitizeForTenantID replaces every character of name outside
// [A-Za-z0-9] with '-', so a subtest's slash-separated, space-containing
// t.Name() (for example "TestAssertIsolated_Sprocket/sqlite") turns into a
// value safe to embed in a tenant id and, downstream, a SQL column value.
func sanitizeForTenantID(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, name)
}

// stringField reads m's exported field named name through reflection,
// mirroring the exact convention dbkit.Repository[T] documents and relies
// on internally for the same "ID"/"TenantID" fields (see dbkit's
// repository.go) — dbkit.TenantScoped exposes no accessor for either, so
// this is the only way to read them generically from outside dbkit.
func stringField[T any](m *T, name string) (value string, ok bool) {
	f := reflect.ValueOf(m).Elem().FieldByName(name)
	if !f.IsValid() || f.Kind() != reflect.String {
		return "", false
	}
	return f.String(), true
}

// setStringField writes value into m's exported string field named name
// through reflection. It exists solely for the forged-tenant-id check
// above: proving Create overwrites a caller-forged TenantID rather than
// trusting it requires forging one first.
func setStringField[T any](t *testing.T, m *T, name, value string) {
	t.Helper()
	f := reflect.ValueOf(m).Elem().FieldByName(name)
	if !f.IsValid() || !f.CanSet() || f.Kind() != reflect.String {
		t.Fatalf("record type %T has no settable exported string field %q", *m, name)
	}
	f.SetString(value)
}
