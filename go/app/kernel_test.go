package app

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vislake/speed/go/config"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"
)

// failingResolver fails every resolution, the tenant-less state the
// pre-auth allowlist exists for.
type failingResolver struct{}

func (failingResolver) Resolve(*http.Request) (pkgcore.TenantID, error) {
	return "", errors.New("test: no tenant resolvable")
}

// TestPreAuthAllowlist_ExemptsBothMethodsOnEveryPlatformPath pins the
// returned option set directly: each platform pre-auth path passes the
// tenancy chain under GET and HEAD alike, and the same path under another
// method stays refused.
func TestPreAuthAllowlist_ExemptsBothMethodsOnEveryPlatformPath(t *testing.T) {
	protected := tenancy.Middleware(failingResolver{}, PreAuthAllowlist()...)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, path := range []string{obs.HealthzPath, obs.MetricsPath, config.PathPublic, config.PathSystemFeatures} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			rec := httptest.NewRecorder()
			protected.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
			if rec.Code != http.StatusNoContent {
				t.Errorf("%s %s: status %d, want %d; the path is on the pre-auth allowlist under both GET and HEAD", method, path, rec.Code, http.StatusNoContent)
			}
		}
		rec := httptest.NewRecorder()
		protected.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s: status %d, want %d; an unlisted method on an allowlisted path stays refused", path, rec.Code, http.StatusForbidden)
		}
	}
}
