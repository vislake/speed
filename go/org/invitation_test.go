package org

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/tenancy/tenancytest"

	"github.com/vislake/speed/go/org/migrations"
)

// testEncryptionKey is the AES key the tests' encrypted-email serializer is
// registered with.
//
// It is deliberately a DIFFERENT 32 bytes from testIndexerKey. dbkit takes
// the encryption key and the blind-index key through two separate
// constructors precisely because reusing one secret for both weakens both,
// and a test that shared them would quietly model the wrong deployment.
var testEncryptionKey = []byte("org-test-email-cipher-key-32byte")

const (
	// testMailFrom is the sender address the test module is wired with.
	testMailFrom = "invitations@example.test"
	// testLinkBase is the prefix testLinkBuilder builds accept URLs on.
	testLinkBase = "https://tenant-a.example.test/invitations/accept?token="
)

// testLinkBuilder stands in for a host's own link builder.
func testLinkBuilder(_ context.Context, token string) (string, error) {
	return testLinkBase + token, nil
}

// newInvitationTestDB returns a migrated SQLite database with the encrypted
// email serializer registered, which any test touching Invitation needs: the
// column is written through a named GORM serializer, and GORM fails to parse
// the model when nothing is registered under that name.
//
// Registering here rather than in a TestMain keeps the requirement visible at
// the call site, and GORM's registry is keyed by name, so repeating it is a
// no-op replacement rather than an error.
func newInvitationTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	cipher, err := dbkit.NewCipher(testEncryptionKey)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	dbkit.RegisterEncryptedSerializer(EmailSerializerName, cipher)
	return newTestDB(t)
}

// seedInvitation inserts one invitation directly through the repository.
func seedInvitation(t *testing.T, repo *InvitationRepository, ctx context.Context, inv Invitation) Invitation {
	t.Helper()
	if err := repo.Create(ctx, &inv); err != nil {
		t.Fatalf("seed invitation %q: %v", inv.ID, err)
	}
	return inv
}

// TestInvitationRepository_AssertIsolated runs the mandatory tenant-isolation
// suite against org_invitations, which is tenant data.
func TestInvitationRepository_AssertIsolated(t *testing.T) {
	repo := NewInvitationRepository(newInvitationTestDB(t))
	indexer := newTestEmailIndexer(t)

	n := 0
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *Invitation {
		n++
		email := fmt.Sprintf("invitee-%d@example.test", n)
		index, err := indexer.Index(email)
		if err != nil {
			t.Fatalf("Index: %v", err)
		}
		return &Invitation{
			ID:            fmt.Sprintf("30000000-0000-4000-8000-%012d", n),
			NodeID:        "node-1",
			Email:         email,
			EmailIndex:    index,
			InviterUserID: "u-inviter",
			Locale:        "en-US",
			TokenHash:     hashInvitationToken(fmt.Sprintf("token-%d", n)),
			Status:        InvitationStatusPending,
			ExpiresAt:     time.Now().Add(time.Hour),
		}
	})
}

// TestInvitation_EmailIsStoredEncrypted is the security assertion behind the
// serializer: the bytes on disk must not be the address. It reads the raw
// column through a second, serializer-free model so the check cannot be
// fooled by the decryption path it is meant to verify.
func TestInvitation_EmailIsStoredEncrypted(t *testing.T) {
	db := newInvitationTestDB(t)
	repo := NewInvitationRepository(db)
	ctx := tenantCtx("tenant-a")
	indexer := newTestEmailIndexer(t)

	const address = "Ada.Lovelace@Example.TEST"
	index, err := indexer.Index(address)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	seedInvitation(t, repo, ctx, Invitation{
		ID: "30000000-0000-4000-8000-000000000001", NodeID: "n-1",
		Email: address, EmailIndex: index, InviterUserID: "u-1", Locale: "en-US",
		TokenHash: hashInvitationToken("t"), Status: InvitationStatusPending,
		ExpiresAt: time.Now().Add(time.Hour),
	})

	var raw []byte
	row := db.WithContext(ctx).
		Raw("SELECT email FROM org_invitations WHERE id = ?", "30000000-0000-4000-8000-000000000001").
		Row()
	if scanErr := row.Scan(&raw); scanErr != nil {
		t.Fatalf("read the raw column: %v", scanErr)
	}
	if len(raw) == 0 {
		t.Fatal("the email column is empty")
	}
	if strings.Contains(string(raw), "Lovelace") || strings.Contains(strings.ToLower(string(raw)), "example.test") {
		t.Error("the stored email column contains the plaintext address")
	}

	// And it still round-trips through the serializer.
	got, err := repo.FindByID(ctx, "30000000-0000-4000-8000-000000000001")
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.Email != address {
		t.Errorf("decrypted email = %q, want %q", got.Email, address)
	}
}

func TestValidateInviteEmail(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"a plain address", "ada@example.test", "ada@example.test"},
		{"surrounding whitespace is trimmed", "  ada@example.test  ", "ada@example.test"},
		{"case is preserved here and normalized by the indexer", "Ada@Example.TEST", "Ada@Example.TEST"},
		{"a subdomain", "ada@mail.example.test", "ada@mail.example.test"},
		{"a plus tag", "ada+dental@example.test", "ada+dental@example.test"},
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
		{"no at sign", "not-an-address", ""},
		{"no local part", "@example.test", ""},
		{"no domain", "ada@", ""},
		{"two at signs", "ada@@example.test", ""},
		{"a domain with no dot", "ada@localhost", ""},
		{"a domain starting with a dot", "ada@.example.test", ""},
		{"a domain ending with a dot", "ada@example.test.", ""},
		{"an inner space", "ada lovelace@example.test", ""},
		{"a smuggled header break", "ada@example.test\nBcc: victim@example.test", ""},
		{"a tab", "ada@example\t.test", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateInviteEmail(tc.input)
			if tc.want == "" {
				if !hasCode(err, ErrInvalidEmail.Code) {
					t.Fatalf("validateInviteEmail(%q) error = %v, want org.invalid_email", tc.input, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateInviteEmail(%q): %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("validateInviteEmail(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}

	t.Run("longer than the RFC 5321 forward-path limit", func(t *testing.T) {
		long := strings.Repeat("a", maxEmailLen) + "@example.test"
		if _, err := validateInviteEmail(long); !hasCode(err, ErrInvalidEmail.Code) {
			t.Errorf("validateInviteEmail(overlong) error = %v, want org.invalid_email", err)
		}
	})

	t.Run("an invalid address never reaches the error payload", func(t *testing.T) {
		// An error's parameters are rendered, logged and traced, so an
		// address must not travel in one.
		_, err := validateInviteEmail("ada lovelace@example.test")
		appErr, ok := apperr.As(err)
		if !ok {
			t.Fatalf("error %v is not an *apperr.Error", err)
		}
		for key, value := range appErr.Params {
			if s, isString := value.(string); isString && strings.Contains(s, "@") {
				t.Errorf("parameter %q carries what looks like an address: %q", key, s)
			}
		}
	})
}

func TestInvitation_IsPending(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		status string
		expiry time.Time
		want   bool
	}{
		{"pending and fresh", InvitationStatusPending, now.Add(time.Hour), true},
		{"pending but expired", InvitationStatusPending, now.Add(-time.Second), false},
		{"pending, expiring exactly now", InvitationStatusPending, now, false},
		{"accepted", InvitationStatusAccepted, now.Add(time.Hour), false},
		{"revoked", InvitationStatusRevoked, now.Add(time.Hour), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inv := Invitation{Status: tc.status, ExpiresAt: tc.expiry}
			if got := inv.IsPending(now); got != tc.want {
				t.Errorf("IsPending() = %t, want %t", got, tc.want)
			}
		})
	}
}

// TestNewInvitationToken pins the two properties the token design rests on:
// it is URL-safe, and the value stored is the hash rather than the token.
func TestNewInvitationToken(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		token, hash, err := newInvitationToken()
		if err != nil {
			t.Fatalf("newInvitationToken: %v", err)
		}
		if seen[token] {
			t.Fatalf("newInvitationToken returned the duplicate %q", token)
		}
		seen[token] = true

		if strings.ContainsAny(token, "+/=&?#") {
			t.Errorf("token %q carries a character that needs URL escaping", token)
		}
		if len(hash) != 64 {
			t.Errorf("hash %q is %d characters, want the 64 of a hex SHA-256", hash, len(hash))
		}
		if strings.Contains(hash, token) {
			t.Error("the stored hash contains the token itself")
		}
		if got := hashInvitationToken(token); got != hash {
			t.Errorf("hashInvitationToken(token) = %q, want %q", got, hash)
		}
	}
}

func TestInvitationRepository_byTokenHash(t *testing.T) {
	repo := NewInvitationRepository(newInvitationTestDB(t))
	ctxA := tenantCtx("tenant-a")
	ctxB := tenantCtx("tenant-b")
	indexer := newTestEmailIndexer(t)

	index, err := indexer.Index("ada@example.test")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	seedInvitation(t, repo, ctxA, Invitation{
		ID: "30000000-0000-4000-8000-000000000001", NodeID: "n-1",
		Email: "ada@example.test", EmailIndex: index, InviterUserID: "u-1", Locale: "en-US",
		TokenHash: hashInvitationToken("secret-token"), Status: InvitationStatusPending,
		ExpiresAt: time.Now().Add(time.Hour),
	})

	got, err := repo.byTokenHash(ctxA, hashInvitationToken("secret-token"))
	if err != nil {
		t.Fatalf("byTokenHash: %v", err)
	}
	if got.ID != "30000000-0000-4000-8000-000000000001" {
		t.Errorf("byTokenHash returned %q", got.ID)
	}

	if _, err := repo.byTokenHash(ctxA, hashInvitationToken("wrong")); !hasCode(err, ErrInvitationNotFound.Code) {
		t.Errorf("byTokenHash(wrong token) error = %v, want org.invitation_not_found", err)
	}

	// The correct token, from the wrong tenant: this is the property that
	// makes it safe for the token never to name its tenant.
	if _, err := repo.byTokenHash(ctxB, hashInvitationToken("secret-token")); !hasCode(err, ErrInvitationNotFound.Code) {
		t.Errorf("byTokenHash(other tenant) error = %v, want org.invitation_not_found", err)
	}
}

func TestInvitationRepository_pendingByEmail(t *testing.T) {
	repo := NewInvitationRepository(newInvitationTestDB(t))
	ctx := tenantCtx("tenant-a")
	indexer := newTestEmailIndexer(t)

	index, err := indexer.Index("ada@example.test")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	otherIndex, err := indexer.Index("grace@example.test")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	seedInvitation(t, repo, ctx, Invitation{
		ID: "30000000-0000-4000-8000-000000000001", NodeID: "n-1",
		Email: "ada@example.test", EmailIndex: index, InviterUserID: "u-1", Locale: "en-US",
		TokenHash: hashInvitationToken("t1"), Status: InvitationStatusPending,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	seedInvitation(t, repo, ctx, Invitation{
		ID: "30000000-0000-4000-8000-000000000002", NodeID: "n-1",
		Email: "ada@example.test", EmailIndex: index, InviterUserID: "u-1", Locale: "en-US",
		TokenHash: hashInvitationToken("t2"), Status: InvitationStatusRevoked,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	seedInvitation(t, repo, ctx, Invitation{
		ID: "30000000-0000-4000-8000-000000000003", NodeID: "n-1",
		Email: "grace@example.test", EmailIndex: otherIndex, InviterUserID: "u-1", Locale: "en-US",
		TokenHash: hashInvitationToken("t3"), Status: InvitationStatusPending,
		ExpiresAt: time.Now().Add(time.Hour),
	})

	// The lookup normalizes exactly as the write path did: a differently
	// cased, space-padded spelling of the same address still matches, which
	// is the whole point of normalizing before indexing.
	got, err := repo.pendingByEmail(ctx, indexer, "  Ada@EXAMPLE.test ")
	if err != nil {
		t.Fatalf("pendingByEmail: %v", err)
	}
	if len(got) != 1 || got[0].ID != "30000000-0000-4000-8000-000000000001" {
		t.Fatalf("pendingByEmail returned %d rows (%+v), want only the pending one for that address", len(got), got)
	}

	// An input with no canonical form -- blank, in the normalizer's terms --
	// is refused before any SQL runs.
	if _, err := repo.pendingByEmail(ctx, indexer, "   "); !hasCode(err, ErrInvalidEmail.Code) {
		t.Errorf("pendingByEmail(blank address) error = %v, want org.invalid_email", err)
	}
}

func TestInvitationRepository_byStatus(t *testing.T) {
	repo := NewInvitationRepository(newInvitationTestDB(t))
	ctx := tenantCtx("tenant-a")
	indexer := newTestEmailIndexer(t)
	index, err := indexer.Index("ada@example.test")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}

	for i, status := range []string{InvitationStatusPending, InvitationStatusAccepted, InvitationStatusPending} {
		seedInvitation(t, repo, ctx, Invitation{
			ID: fmt.Sprintf("30000000-0000-4000-8000-%012d", i+1), NodeID: "n-1",
			Email: "ada@example.test", EmailIndex: index, InviterUserID: "u-1", Locale: "en-US",
			TokenHash: hashInvitationToken(fmt.Sprintf("t%d", i)), Status: status,
			ExpiresAt: time.Now().Add(time.Hour),
		})
	}

	got, err := repo.byStatus(ctx, InvitationStatusPending)
	if err != nil {
		t.Fatalf("byStatus: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("byStatus(pending) returned %d rows, want 2", len(got))
	}
	for _, inv := range got {
		if inv.Status != InvitationStatusPending {
			t.Errorf("byStatus(pending) returned a %q row", inv.Status)
		}
	}
}

// TestInvitationMigrations_ShipBothDialects pins that the two new tables
// exist in both dialect directories under the same file names, which is what
// dbkit.MigrationRegistry.Apply requires.
func TestInvitationMigrations_ShipBothDialects(t *testing.T) {
	for _, name := range []string{"0002_create_memberships.sql", "0003_create_org_invitations.sql"} {
		for _, dialect := range []string{"postgres", "sqlite"} {
			if _, err := migrations.FS.ReadFile(dialect + "/" + name); err != nil {
				t.Errorf("read %s/%s: %v", dialect, name, err)
			}
		}
	}
}
