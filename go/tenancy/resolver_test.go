package tenancy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// newHostRequest returns a request whose Host is exactly host, which is all
// DomainResolver.Resolve reads from it.
func newHostRequest(host string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://placeholder.example/", nil)
	r.Host = host
	return r
}

func TestDomainResolver_Resolve(t *testing.T) {
	const (
		knownHost     = "tenant-a.example.com"
		emptyHitHost  = "misconfigured.example.com"
		unknownHost   = "unknown.example.com"
		knownTenant   = pkgcore.TenantID("tenant-a")
		defaultTenant = pkgcore.TenantID("default-tenant")
	)

	lookup := func(host string) (pkgcore.TenantID, bool) {
		switch host {
		case knownHost:
			return knownTenant, true
		case emptyHitHost:
			// A lookup that reports a match but hands back the zero value:
			// DomainResolver must not trust this as a real resolution.
			return pkgcore.TenantID(""), true
		default:
			return "", false
		}
	}

	tests := []struct {
		name       string
		resolver   *DomainResolver
		host       string
		wantTenant pkgcore.TenantID
	}{
		{
			name:       "known host resolves via lookup",
			resolver:   NewDomainResolver(lookup, defaultTenant),
			host:       knownHost,
			wantTenant: knownTenant,
		},
		{
			name:       "unknown host falls back to default",
			resolver:   NewDomainResolver(lookup, defaultTenant),
			host:       unknownHost,
			wantTenant: defaultTenant,
		},
		{
			name:       "lookup match with empty tenant falls back to default",
			resolver:   NewDomainResolver(lookup, defaultTenant),
			host:       emptyHitHost,
			wantTenant: defaultTenant,
		},
		{
			name:       "nil lookup always falls back to default",
			resolver:   NewDomainResolver(nil, defaultTenant),
			host:       knownHost,
			wantTenant: defaultTenant,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.resolver.Resolve(newHostRequest(tt.host))
			// DomainResolver must never fail: an unresolved host still has
			// to be able to render a login page.
			if err != nil {
				t.Fatalf("Resolve(%q) returned error %v, want nil", tt.host, err)
			}
			if got == "" {
				t.Fatalf("Resolve(%q) = empty TenantID, want it to always resolve to something", tt.host)
			}
			if got != tt.wantTenant {
				t.Errorf("Resolve(%q) = %q, want %q", tt.host, got, tt.wantTenant)
			}
		})
	}
}
