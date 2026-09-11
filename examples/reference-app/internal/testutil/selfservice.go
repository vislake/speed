package testutil

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// FailOnceProvisioning fails exactly the first provisioning attempt it is
// asked about and succeeds afterwards -- the
// ServerConfig.FailSelfServiceProvision hook shape the failure-half
// regression below arms before building its server.
type FailOnceProvisioning struct {
	mu       sync.Mutex
	attempts int
}

// WaitForClinicMembership polls clinic's registrant membership row through a
// second connection to the SQLite file sqlitePath until it appears or the
// deadline passes. The membership row is the durable record of a completed
// provision, so its appearance means a provisioning attempt ran to
// completion -- after a failed synchronous attempt, the completing attempt
// can only be the retry job's. The poll applies the same filter org's own cross-tenant query
// applies -- status "active", never soft-deleted (go/org/membership.go's
// MembershipStatusActive and go/org/membership_tenants.go's
// membershipTenantRow). A read, so no authn rate-limit budget is spent
// waiting; a transient busy on the shared SQLite file while the retry's own
// writes land is a reason to poll again, never a failure.
func WaitForClinicMembership(t *testing.T, sqlitePath string, clinic pkgcore.TenantID, userID string) {
	t.Helper()

	ctx, cancelPoll := context.WithCancel(context.Background())
	defer cancelPoll()
	pollDB, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: sqlitePath})
	if err != nil {
		t.Fatalf("open the server's database for the membership poll: %v", err)
	}
	defer func() {
		if sqlDB, dbErr := pollDB.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
	}()
	deadline := time.Now().Add(15 * time.Second)
	var lastCountErr error
	for {
		var count int64
		countErr := pollDB.Table("memberships").
			Where("tenant_id = ? AND user_id = ? AND status = ? AND deleted_at IS NULL",
				string(clinic), userID, "active").
			Count(&count).Error
		if countErr != nil {
			// A transient busy on the shared SQLite file while the retry's
			// own writes land is a reason to poll again, not to fail.
			lastCountErr = countErr
		} else {
			lastCountErr = nil
			if count > 0 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the clinic %s never gained the registrant's membership: register answered 201, the synchronous attempt failed, and no retry converged it -- the account is stranded (last membership read error: %v)",
				clinic, lastCountErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Fail implements the FailSelfServiceProvision hook: the first attempt
// fails, later ones (the retry job's) succeed.
func (f *FailOnceProvisioning) Fail(userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.attempts == 1 {
		return errors.New("injected provisioning failure")
	}
	return nil
}

// Observed reports whether any provisioning attempt consumed the hook --
// the guarantee that the injected failure really fired (and therefore
// that the convergence the test then watches was the retry job's work,
// not a synchronous success).
func (f *FailOnceProvisioning) Observed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts >= 1
}
