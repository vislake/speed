package org

import (
	"context"
	"reflect"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// systemPurposeTenantsOfTest is the purpose TenantsOf's own tests grant
// themselves. It exists only here, mirroring how a host declares the
// purpose it takes a system context for: pkgcore.WithSystemContext refuses
// a purpose never declared through RegisterSystemPurpose, and
// RegisterSystemPurpose is idempotent, so a test-suite registration is
// exactly the shape a real host's Module.Register uses.
const systemPurposeTenantsOfTest = pkgcore.SystemPurpose("org.test.tenants_of")

func init() {
	pkgcore.RegisterSystemPurpose(systemPurposeTenantsOfTest)
}

// systemCtx returns a context carrying a system context granted for
// systemPurposeTenantsOfTest, for tests that exercise TenantsOf's
// system-context-gated read.
func systemCtx(ctx context.Context) (context.Context, error) {
	return pkgcore.WithSystemContext(ctx, pkgcore.SystemReason{
		Actor:   "org-test",
		Purpose: systemPurposeTenantsOfTest,
	})
}

// TestMemberService_TenantsOf_AnswersAcrossTenantsUnderSystemContext is
// the cross-tenant read's regression test: a user with memberships in
// several tenants is answered with all of them, by one query under a system
// context -- a host without the query can only scan its own bounded tenant
// lists and ask per tenant (the reference app's sign_in_memberships scan),
// which leaves a tenant created after boot invisible to the question.
func TestMemberService_TenantsOf_AnswersAcrossTenantsUnderSystemContext(t *testing.T) {
	m, _ := newTestModule(t)
	svc := m.Members()

	// Three tenants, each with a root the memberships can bind to.
	acmeCtx := tenantCtx("tenant-acme")
	globexCtx := tenantCtx("tenant-globex")
	otherCtx := tenantCtx("tenant-other")
	seed := func(ctx context.Context) string {
		t.Helper()
		tenant, err := pkgcore.MustTenantFromContext(ctx)
		if err != nil {
			t.Fatalf("tenant from context: %v", err)
		}
		root, err := m.Tree().CreateRoot(ctx, "Tenant Root", "group")
		if err != nil {
			t.Fatalf("CreateRoot(%q): %v", tenant, err)
		}
		return root.ID
	}
	acmeRoot := seed(acmeCtx)
	globexRoot := seed(globexCtx)
	_ = seed(otherCtx)

	sysCtx, sysErr := systemCtx(context.Background())
	if sysErr != nil {
		t.Fatalf("systemCtx: %v", sysErr)
	}

	// u-acme-globex holds active memberships in two of the three tenants.
	if _, err := svc.Add(acmeCtx, "u-acme-globex", acmeRoot); err != nil {
		t.Fatalf("Add (tenant-acme): %v", err)
	}
	if _, err := svc.Add(globexCtx, "u-acme-globex", globexRoot); err != nil {
		t.Fatalf("Add (tenant-globex): %v", err)
	}

	// A second user narrows the answer: TenantsOf is per user, never a
	// table-wide "who is everywhere" question.
	if _, err := svc.Add(globexCtx, "u-globex-only", globexRoot); err != nil {
		t.Fatalf("Add (tenant-globex, second user): %v", err)
	}

	got, err := svc.TenantsOf(sysCtx, "u-acme-globex")
	if err != nil {
		t.Fatalf("TenantsOf: %v", err)
	}
	want := []pkgcore.TenantID{"tenant-acme", "tenant-globex"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("TenantsOf(u-acme-globex) = %v, want %v (ordered by tenant id)", got, want)
	}

	got, err = svc.TenantsOf(sysCtx, "u-globex-only")
	if err != nil {
		t.Fatalf("TenantsOf(u-globex-only): %v", err)
	}
	if want := []pkgcore.TenantID{"tenant-globex"}; !reflect.DeepEqual(got, want) {
		t.Errorf("TenantsOf(u-globex-only) = %v, want %v", got, want)
	}

	// A user with no memberships anywhere answers an empty list, not an
	// error: "belongs to no organization" is the true answer for an
	// account that registered but nothing ever provisioned.
	got, err = svc.TenantsOf(sysCtx, "u-nowhere")
	if err != nil {
		t.Fatalf("TenantsOf(u-nowhere): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("TenantsOf(u-nowhere) = %v, want an empty list", got)
	}
}

// TestMemberService_TenantsOf_SystemContextGate pins the gate itself: the
// cross-tenant question is refused for a caller holding only an
// organization's own context, before any database work happens.
func TestMemberService_TenantsOf_SystemContextGate(t *testing.T) {
	m, _ := newTestModule(t)
	svc := m.Members()

	sysCtx, sysErr := systemCtx(context.Background())
	if sysErr != nil {
		t.Fatalf("systemCtx: %v", sysErr)
	}

	// A plain tenant-scoped context is refused, even though it names a
	// tenant the user genuinely belongs to: org.membership_not_found-style
	// per-tenant facts are Get's business, and the cross-tenant enumeration
	// needs the system grant.
	if _, err := svc.TenantsOf(tenantCtx("tenant-acme"), "u-any"); !hasCode(err, ErrSystemContextRequired.Code) {
		t.Errorf("TenantsOf under a tenant context error = %v, want org.system_context_required", err)
	}

	// A system context granted on top of a tenant context still answers:
	// the tenant is irrelevant to the question, and refusing the
	// combination would break the audited shape (tenancy.WithSystemContext
	// is routinely taken while already acting in one tenant).
	elevated, elevateErr := pkgcore.WithSystemContext(tenantCtx("tenant-acme"), pkgcore.SystemReason{
		Actor:   "org-test",
		Purpose: systemPurposeTenantsOfTest,
	})
	if elevateErr != nil {
		t.Fatalf("WithSystemContext over a tenant context: %v", elevateErr)
	}
	if _, err := svc.TenantsOf(elevated, "u-any"); err != nil {
		t.Errorf("TenantsOf under system context over a tenant context: %v, want a successful empty answer", err)
	}

	// An empty user id answers empty under the gate, and is refused
	// without it like any other question.
	if got, err := svc.TenantsOf(sysCtx, ""); err != nil || len(got) != 0 {
		t.Errorf("TenantsOf(system, %q) = %v, %v; want an empty list and no error", "", got, err)
	}
}

// TestMemberService_TenantsOf_LiveRowsOnly pins what "active membership"
// means for the answer: a suspended seat and a soft-deleted one are not
// memberships the account may act on, so neither may be answered as one.
func TestMemberService_TenantsOf_LiveRowsOnly(t *testing.T) {
	m, _ := newTestModule(t)
	svc := m.Members()

	acmeCtx := tenantCtx("tenant-acme")
	globexCtx := tenantCtx("tenant-globex")
	suspendedCtx := tenantCtx("tenant-suspended")

	sysCtx, sysErr := systemCtx(context.Background())
	if sysErr != nil {
		t.Fatalf("systemCtx: %v", sysErr)
	}

	acmeRoot, rootErr := m.Tree().CreateRoot(acmeCtx, "Acme Root", "group")
	if rootErr != nil {
		t.Fatalf("CreateRoot(acme): %v", rootErr)
	}
	globexRoot, rootErr := m.Tree().CreateRoot(globexCtx, "Globex Root", "group")
	if rootErr != nil {
		t.Fatalf("CreateRoot(globex): %v", rootErr)
	}
	if _, err := m.Tree().CreateRoot(suspendedCtx, "Suspended Root", "group"); err != nil {
		t.Fatalf("CreateRoot(suspended): %v", err)
	}

	// One live membership in tenant-acme; one in tenant-globex that Remove
	// mark-deletes mid-test; one suspended row seeded directly (no service
	// path writes MembershipStatusSuspended today, but the vocabulary
	// exists and a tenant that parks a member must see them vanish from
	// this answer exactly as from every other).
	if _, err := svc.Add(acmeCtx, "u-live", acmeRoot.ID); err != nil {
		t.Fatalf("Add(acme): %v", err)
	}
	if _, err := svc.Add(globexCtx, "u-live", globexRoot.ID); err != nil {
		t.Fatalf("Add(globex): %v", err)
	}
	repo := svc.Repository()
	seedMembership(t, repo, suspendedCtx, Membership{
		ID:     "20000000-0000-4000-8000-000000000001",
		UserID: "u-live",
		NodeID: "20000000-0000-4000-8000-000000000002",
		Status: MembershipStatusSuspended,
	})

	got, err := svc.TenantsOf(sysCtx, "u-live")
	if err != nil {
		t.Fatalf("TenantsOf: %v", err)
	}
	if want := []pkgcore.TenantID{"tenant-acme", "tenant-globex"}; !reflect.DeepEqual(got, want) {
		t.Errorf("TenantsOf before Remove = %v, want %v", got, want)
	}

	// Remove mark-deletes the globex membership (a second active member
	// exists there only if seeded -- none is, so seed a second seat in
	// tenant-globex first to keep Remove's last-member guard satisfied).
	seedMembership(t, repo, globexCtx, Membership{
		ID:     "20000000-0000-4000-8000-000000000003",
		UserID: "u-other",
		NodeID: globexRoot.ID,
		Status: MembershipStatusActive,
	})
	if removeErr := svc.Remove(globexCtx, "u-live"); removeErr != nil {
		t.Fatalf("Remove(globex): %v", removeErr)
	}

	got, err = svc.TenantsOf(sysCtx, "u-live")
	if err != nil {
		t.Fatalf("TenantsOf after Remove: %v", err)
	}
	// The suspended row is excluded, and the soft-deleted globex
	// membership is gone from the answer.
	if want := []pkgcore.TenantID{"tenant-acme"}; !reflect.DeepEqual(got, want) {
		t.Errorf("TenantsOf after Remove = %v, want %v", got, want)
	}
}
