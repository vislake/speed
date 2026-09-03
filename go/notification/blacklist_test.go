package notification

import (
	"fmt"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// TestPlatformBlacklist_AssertNotTenantScoped is the mandatory isolation
// assertion for a platform-domain table (docs/internal/04-data-and-tenancy.md's
// data-domain table): an address that complained or hard-bounced is bad for
// every tenant of the platform, so platform_blacklist must be visible to any
// query whatever tenant -- or no tenant -- is in the context. The suite's
// createFn returns a distinct id and address index on every call, as it
// requires: the platform-wide UNIQUE (channel, address_index) index would
// otherwise turn the suite's second create into a duplicate-key failure
// before the visibility question was ever asked.
func TestPlatformBlacklist_AssertNotTenantScoped(t *testing.T) {
	db := newTestDB(t)

	seq := 0
	tenancytest.AssertNotTenantScoped(t, db, PlatformBlacklist{},
		func(db *gorm.DB) error {
			seq++
			return db.Create(&PlatformBlacklist{
				ID:           fmt.Sprintf("blk-%03d", seq),
				Channel:      ChannelEmail,
				AddressIndex: fmt.Sprintf("addr-%03d", seq),
				Reason:       BlacklistReasonComplaint,
			}).Error
		},
		func(db *gorm.DB) (int64, error) {
			var n int64
			err := db.Model(&PlatformBlacklist{}).Count(&n).Error
			return n, err
		},
	)
}

// insertBlacklistFixture writes one platform_blacklist row straight through
// the plain *gorm.DB -- the sanctioned data path for platform data, exactly
// as blacklist.go's own repository uses it. The row is deliberately created
// with no tenant in the context: this table must never need one.
func insertBlacklistFixture(t *testing.T, db *gorm.DB, id, channel, addressIndex, reason string) {
	t.Helper()
	if err := db.Create(&PlatformBlacklist{
		ID:           id,
		Channel:      channel,
		AddressIndex: addressIndex,
		Reason:       reason,
	}).Error; err != nil {
		t.Fatalf("insert blacklist fixture %s: %v", id, err)
	}
}

// TestPlatformBlacklist_IsBlacklisted_EmptyTable_ReportsFalse pins the absent
// case of the deliverability probe: an address nobody has ever complained
// about reads as (false, nil) -- never an error the delivery job would have
// to treat as a failure.
func TestPlatformBlacklist_IsBlacklisted_EmptyTable_ReportsFalse(t *testing.T) {
	db := newTestDB(t)
	repo := NewPlatformBlacklistRepository(db)

	got, err := repo.IsBlacklisted(tenantCtx("tenant-acme"), ChannelSMS, "some-index")
	if err != nil {
		t.Fatalf("IsBlacklisted on an empty table: %v", err)
	}
	if got {
		t.Error("IsBlacklisted on an empty table = true, want false")
	}
}

// TestPlatformBlacklist_IsBlacklisted_MatchingRow_ReportsTrue pins the
// present case together with its two near-misses: the probe matches on the
// full (channel, address_index) pair, so the same index under the other
// channel and a neighbour index under the same channel both read false. A
// blacklist that leaked one channel's verdict onto the other would stop SMS
// delivery over one tenant's email complaint.
func TestPlatformBlacklist_IsBlacklisted_MatchingRow_ReportsTrue(t *testing.T) {
	db := newTestDB(t)
	repo := NewPlatformBlacklistRepository(db)
	ctx := tenantCtx("tenant-acme")

	insertBlacklistFixture(t, db, "blk-001", ChannelEmail, "index-email-42", BlacklistReasonComplaint)

	got, err := repo.IsBlacklisted(ctx, ChannelEmail, "index-email-42")
	if err != nil {
		t.Fatalf("IsBlacklisted(matching pair): %v", err)
	}
	if !got {
		t.Error("IsBlacklisted(matching pair) = false, want true")
	}

	for _, miss := range []struct {
		channel, index string
	}{
		{ChannelSMS, "index-email-42"},
		{ChannelEmail, "index-email-43"},
	} {
		got, err := repo.IsBlacklisted(ctx, miss.channel, miss.index)
		if err != nil {
			t.Fatalf("IsBlacklisted(%s, %s): %v", miss.channel, miss.index, err)
		}
		if got {
			t.Errorf("IsBlacklisted(%s, %s) = true, want false", miss.channel, miss.index)
		}
	}

	// The fixture's reason and recording tenant survive the round trip; the
	// repository deliberately exposes no way to read them back, so the probe
	// goes through the same plain *gorm.DB the fixture was written with.
	var row PlatformBlacklist
	if err := db.Where("id = ?", "blk-001").First(&row).Error; err != nil {
		t.Fatalf("read back the fixture row: %v", err)
	}
	if row.Reason != BlacklistReasonComplaint {
		t.Errorf("fixture reason = %q, want %q", row.Reason, BlacklistReasonComplaint)
	}
	if row.Channel != ChannelEmail || row.AddressIndex != "index-email-42" {
		t.Errorf("fixture pair = (%q, %q), want (%q, %q)", row.Channel, row.AddressIndex, ChannelEmail, "index-email-42")
	}
}

// TestPlatformBlacklist_IsBlacklisted_VisibleAcrossTenants pins the
// cross-tenant property that justifies the platform domain in the first
// place: a record written under one tenant's send -- or under no tenant at
// all -- answers true to every other tenant's deliverability probe. A
// blacklist scoped to the tenant that reported it would let the platform
// keep messaging an address it already knows is bad on behalf of everyone
// else.
func TestPlatformBlacklist_IsBlacklisted_VisibleAcrossTenants(t *testing.T) {
	db := newTestDB(t)
	repo := NewPlatformBlacklistRepository(db)

	// Written with no tenant in the context -- the platform-level shape.
	insertBlacklistFixture(t, db, "blk-001", ChannelSMS, "index-sms-7", BlacklistReasonHardBounce)
	// Written with a tenant attached, imitating the future writer recording
	// whose send produced the record.
	insertBlacklistFixture(t, db, "blk-002", ChannelEmail, "index-email-7", BlacklistReasonComplaint)

	for _, probe := range []struct {
		channel, index string
		want           bool
	}{
		{ChannelSMS, "index-sms-7", true},
		{ChannelEmail, "index-email-7", true},
		{ChannelSMS, "index-email-7", false},
	} {
		for _, tenant := range []string{"tenant-acme", "tenant-bright"} {
			got, err := repo.IsBlacklisted(tenantCtx(tenant), probe.channel, probe.index)
			if err != nil {
				t.Fatalf("IsBlacklisted(tenant %s, %s, %s): %v", tenant, probe.channel, probe.index, err)
			}
			if got != probe.want {
				t.Errorf("IsBlacklisted(tenant %s, %s, %s) = %v, want %v", tenant, probe.channel, probe.index, got, probe.want)
			}
		}
	}
}
