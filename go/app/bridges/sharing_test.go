package bridges

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// TestShareExpiryReader_FailsClosedWithoutAnAttachedConfigService pins
// the adapter's pre-Attach direction: a read through a nil handle reports
// the config module's own not-attached refusal, which sharing surfaces as
// a genuine read failure -- never a fabricated duration and never a
// panic. (The resolved read after Attach is config's own tested
// TenantDuration contract; this adapter delegates to it unchanged.)
func TestShareExpiryReader_FailsClosedWithoutAnAttachedConfigService(t *testing.T) {
	reader := ShareExpiryReader{}
	expiry, ok, err := reader.ShareDefaultExpiry(context.Background(), "tenant-1")
	if err == nil {
		t.Fatalf("ShareDefaultExpiry on a nil handle answered (%v, %v), want a not-attached error", expiry, ok)
	}
	if !apperr.HasCode(err, config.ErrServiceNotAttached.Code) {
		t.Fatalf("ShareDefaultExpiry error %v does not carry config.ErrServiceNotAttached", err)
	}
}
