package testkit

import (
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

func TestTenantCtx_CarriesTheTenant(t *testing.T) {
	ctx := TenantCtx("tenant-acme")
	got, ok := pkgcore.TenantFromContext(ctx)
	if !ok {
		t.Fatal("TenantFromContext reported no tenant on a TenantCtx context")
	}
	if got != pkgcore.TenantID("tenant-acme") {
		t.Fatalf("tenant = %q, want %q", got, "tenant-acme")
	}
}

func TestTenantCtx_CarriesNothingElse(t *testing.T) {
	ctx := TenantCtx("tenant-acme")
	if _, ok := pkgcore.ActorFromContext(ctx); ok {
		t.Fatal("TenantCtx carries an actor: it must be exactly a background context plus the tenant")
	}
}
