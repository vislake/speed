package tenancytest

import (
	"strings"
	"testing"

	"github.com/vislake/speed/go/tenancy/tenancytest/internal/testutil"
)

// This file is a regression test for a bug independent testing found in
// isolationTenants (assert_isolated.go): it built every tenant id as
// "tenancytest-" + sanitizeForTenantID(t.Name()) + "-a"/"-b" with no length
// bound at all. This project's own testing convention explicitly asks for
// descriptive case/test names (backend-coding-standards §13), and a
// sufficiently descriptive t.Name() -- entirely plausible, not a contrived
// edge case -- produced a tenant id longer than the VARCHAR(64) tenant_id
// column every fixture in this package (and the backend coding standard's
// own Subscription example) uses. SQLite silently accepts a value of any
// length, so this was completely invisible there; PostgreSQL enforces
// VARCHAR(n) strictly and rejected it with "value too long for type
// character varying(64)" (SQLSTATE 22001) -- a failure that reads as
// unrelated to the isolation property actually under test, discovered only
// once such a test happened to run against Postgres.
//
// The fix bounds the derived name segment to isolationNameBudget, replacing
// an overflowing name's tail with a short deterministic hash of the FULL
// original name (see boundedTenantIDSegment) rather than truncating alone,
// so two long names sharing a common prefix still cannot collide once
// shortened.

// TestBoundedTenantIDSegment_ShortNamePassesThroughUnchanged proves the fix
// is fully backward compatible: any test name that already fits within
// budget derives the exact same segment sanitizeForTenantID alone would
// have produced, so no existing (short) test name's tenant id changes.
func TestBoundedTenantIDSegment_ShortNamePassesThroughUnchanged(t *testing.T) {
	const name = "TestAssertIsolated_Sprocket/sqlite"
	if len(name) > isolationNameBudget {
		t.Fatalf("test fixture error: name %q (len %d) does not actually fit isolationNameBudget (%d); pick a shorter one", name, len(name), isolationNameBudget)
	}

	got := boundedTenantIDSegment(name)
	want := sanitizeForTenantID(name)
	if got != want {
		t.Errorf("boundedTenantIDSegment(%q) = %q, want %q (unchanged from sanitizeForTenantID since it already fits)", name, got, want)
	}
}

// TestBoundedTenantIDSegment_LongNameIsBoundedAndDeterministic is the core
// regression case: a name whose sanitized form overflows isolationNameBudget
// must still come back no longer than isolationNameBudget, and must derive
// the identical segment on every call (isolationTenants relies on this to
// produce the same pair of tenant ids for a given *testing.T no matter how
// many times a caller might ask).
func TestBoundedTenantIDSegment_LongNameIsBoundedAndDeterministic(t *testing.T) {
	name := "TestAssertIsolated_" + strings.Repeat("VeryDescriptive", 6) + "/postgres"
	if len(name) <= isolationNameBudget {
		t.Fatalf("test fixture error: name %q (len %d) does not actually overflow isolationNameBudget (%d); make it longer", name, len(name), isolationNameBudget)
	}

	first := boundedTenantIDSegment(name)
	if len(first) > isolationNameBudget {
		t.Errorf("boundedTenantIDSegment(%q) = %q (len %d), want length <= isolationNameBudget (%d)", name, first, len(first), isolationNameBudget)
	}

	second := boundedTenantIDSegment(name)
	if second != first {
		t.Errorf("boundedTenantIDSegment(%q) is not deterministic: got %q then %q", name, first, second)
	}
}

// TestBoundedTenantIDSegment_LongNamesSharingAPrefixDoNotCollide proves the
// fix hashes the FULL original name rather than merely truncating it: two
// long names that agree on everything up to, and well past,
// isolationNameBudget characters -- so a naive truncate-only fix would have
// produced the identical, colliding segment for both -- still derive
// different segments, because the hash folds in the tail where they
// actually differ.
func TestBoundedTenantIDSegment_LongNamesSharingAPrefixDoNotCollide(t *testing.T) {
	commonPrefix := strings.Repeat("A", isolationNameBudget+10)
	name1 := commonPrefix + "-CaseOne"
	name2 := commonPrefix + "-CaseTwo"
	if len(commonPrefix) <= isolationNameBudget {
		t.Fatalf("test fixture error: commonPrefix (len %d) must alone already overflow isolationNameBudget (%d)", len(commonPrefix), isolationNameBudget)
	}

	seg1 := boundedTenantIDSegment(name1)
	seg2 := boundedTenantIDSegment(name2)
	if seg1 == seg2 {
		t.Errorf("boundedTenantIDSegment(%q) and boundedTenantIDSegment(%q) collided on %q; a truncate-only fix would wrongly produce the same segment for both, since they agree on the first %d characters (> isolationNameBudget)", name1, name2, seg1, isolationNameBudget)
	}
}

// TestIsolationTenants_LongDescriptiveSubtestName_StaysWithinMaxTenantIDLen
// reproduces the exact originally reported failure shape end to end through
// the actual exported entry point isolationTenants feeds: a realistic,
// descriptive subtest name -- the kind backend-coding-standards §13 asks
// authors to write -- long enough that the OLD, unbounded formula
// ("tenancytest-" + sanitizeForTenantID(t.Name()) + "-a"/"-b", with no
// bound at all) would have exceeded the 64-character tenant_id column every
// fixture in this package declares. Before the fix, the two assertions
// below on len(a)/len(b) fail for exactly this t.Name(); after the fix,
// they pass, because the overflowing tail is now replaced with a short
// hash instead of being embedded verbatim.
func TestIsolationTenants_LongDescriptiveSubtestName_StaysWithinMaxTenantIDLen(t *testing.T) {
	// Deliberately mirrors a real, plausible descriptive subtest name, not
	// a synthetic worst case: this is close to the exact subtest name whose
	// derived id independent testing measured at 84 characters against the
	// 64-character column every fixture in this package uses.
	t.Run("RepositoryBuiltFromSystemContextElevatedDB_WithoutAnyAdditionalScoping/postgres", func(t *testing.T) {
		rawLen := len(isolationTenantPrefix) + len(sanitizeForTenantID(t.Name())) + isolationTenantSuffixLen
		if rawLen <= maxTenantIDLen {
			t.Fatalf("test fixture error: t.Name() %q (unbounded derived length %d) does not actually overflow maxTenantIDLen (%d); the old, unbounded formula must overflow here for this test to mean anything", t.Name(), rawLen, maxTenantIDLen)
		}

		a, b := isolationTenants(t)
		if len(a) > maxTenantIDLen {
			t.Errorf("isolationTenants(%q) tenant A id = %q (len %d), want length <= maxTenantIDLen (%d); the unbounded formula would have produced length %d here", t.Name(), a, len(a), maxTenantIDLen, rawLen)
		}
		if len(b) > maxTenantIDLen {
			t.Errorf("isolationTenants(%q) tenant B id = %q (len %d), want length <= maxTenantIDLen (%d); the unbounded formula would have produced length %d here", t.Name(), b, len(b), maxTenantIDLen, rawLen)
		}
		if a == b {
			t.Errorf("isolationTenants(%q) returned identical ids for both tenants: %q", t.Name(), a)
		}
	})
}

// TestAssertIsolated_LongDescriptiveSubtestName_WorksAcrossDialects is the
// full end-to-end confirmation: AssertIsolated itself, run under a long,
// descriptive subtest name against a real dbkit.Repository[sprocket], must
// succeed.
//
// SQLite only here -- see TestAssertIsolated_Sprocket's doc comment
// (assert_isolated_test.go) for why a plain _test.go file must not reach
// testutil.Dialects()'s postgres entry. PostgreSQL is if anything the more
// important dialect for this particular regression -- it is the one that
// actually enforces VARCHAR(64) and is what surfaced this bug in the first
// place -- so its leg is not skipped, merely relocated: it runs from
// tenancytest/integration_test/postgres_assert_isolated_tenant_id_length_test.go,
// behind //go:build integration. (The name kept saying "WorksAcrossDialects"
// across that split only because renaming it added unrelated churn; the two
// dialects' coverage together is exactly what it always was.)
func TestAssertIsolated_LongDescriptiveSubtestName_WorksAcrossDialects(t *testing.T) {
	t.Run("ADeliberatelyLongAndDescriptiveSubtestNameLikeTheProjectConventionAsksFor", func(t *testing.T) {
		repo := newSprocketRepo(t, testutil.Dialects()[0].NewDB(t)) // sqlite: see doc comment above
		AssertIsolated(t, repo, newSprocket)
	})
}
