package rbac

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

func TestSubject_Valid(t *testing.T) {
	cases := []struct {
		name string
		sub  Subject
		want bool
	}{
		{name: "tenant_and_user", sub: Subject{TenantID: "t1", UserID: "u1"}, want: true},
		{name: "system_domain_is_an_ordinary_tenant", sub: Subject{TenantID: SystemDomain, UserID: "ops-1"}, want: true},
		{name: "missing_user", sub: Subject{TenantID: "t1"}, want: false},
		{name: "missing_tenant", sub: Subject{UserID: "u1"}, want: false},
		{name: "zero_value", sub: Subject{}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.sub.Valid(); got != tc.want {
				t.Fatalf("Subject%+v.Valid() = %t, want %t", tc.sub, got, tc.want)
			}
		})
	}
}

func TestSystemDomain_IsAnOrdinaryTenantID(t *testing.T) {
	// The pseudo-tenant must be a plain tenant id, because every layer
	// below (the isolation plugin, Repository[T], row-level security)
	// treats it as one. A test pins the literal so a later change cannot
	// quietly turn it into a marker that means something else.
	if SystemDomain != pkgcore.TenantID("system") {
		t.Fatalf("SystemDomain = %q, want %q", SystemDomain, "system")
	}
}

func TestScope_IsTenantWide(t *testing.T) {
	if !(Scope{}).IsTenantWide() {
		t.Fatal("the zero Scope must mean the tenant root")
	}
	if (Scope{NodeID: "/g1"}).IsTenantWide() {
		t.Fatal("a Scope naming a node must not report itself as tenant-wide")
	}
}

func TestWithSubject_RoundTrips(t *testing.T) {
	want := Subject{TenantID: "t1", UserID: "u1"}
	got, ok := SubjectFromContext(WithSubject(context.Background(), want))
	if !ok {
		t.Fatal("SubjectFromContext reported no subject on a context that carries one")
	}
	if got != want {
		t.Fatalf("SubjectFromContext = %+v, want %+v", got, want)
	}
}

func TestSubjectFromContext_NoSubject_FailsClosed(t *testing.T) {
	got, ok := SubjectFromContext(context.Background())
	if ok {
		t.Fatalf("SubjectFromContext on a bare context reported a subject %+v", got)
	}
	if got != (Subject{}) {
		t.Fatalf("SubjectFromContext returned %+v alongside ok=false, want the zero Subject", got)
	}
}

func TestSubjectFromContext_IncompleteSubject_ReportsNoSubject(t *testing.T) {
	// An incomplete Subject must be reported as absent, so every caller's
	// "no subject, deny" branch covers it and no caller has to remember to
	// re-check Valid. The half-filled value must not leak out either.
	for _, sub := range []Subject{{TenantID: "t1"}, {UserID: "u1"}, {}} {
		ctx := WithSubject(context.Background(), sub)
		got, ok := SubjectFromContext(ctx)
		if ok {
			t.Fatalf("SubjectFromContext reported the incomplete subject %+v as usable", sub)
		}
		if got != (Subject{}) {
			t.Fatalf("SubjectFromContext leaked %+v for the incomplete subject %+v", got, sub)
		}
	}
}

func TestSubjectFromContext_ForeignValueUnderAStringKey_IsInvisible(t *testing.T) {
	// The context key is an unexported struct type, so nothing outside this
	// package can plant a Subject. A same-named string key must not be
	// mistaken for one.
	//nolint:staticcheck // deliberately using a bare string key: the point is that it must NOT be found.
	ctx := context.WithValue(context.Background(), "subjectContextKey{}", Subject{TenantID: "t1", UserID: "attacker"})
	if _, ok := SubjectFromContext(ctx); ok {
		t.Fatal("a Subject planted under a foreign key was accepted")
	}
}
