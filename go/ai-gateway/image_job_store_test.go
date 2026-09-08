package aigateway

// Tests for image_job_store.go's imageJobRow/imageJobRepository: the table
// it names, the tenancytest proof that ai_gateway_image_jobs is tenant
// data (mirroring go/storage's Object), and the claim/release/transition
// methods imageGenerateHandler.Handle's idempotency invariant is built on.

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

func TestImageJobRow_TableName(t *testing.T) {
	if want := "ai_gateway_image_jobs"; (imageJobRow{}).TableName() != want {
		t.Fatalf("imageJobRow.TableName() = %q, want %q", (imageJobRow{}).TableName(), want)
	}
}

// TestImageJobRow_IsTenantScoped_AssertIsolated proves ai_gateway_image_jobs
// is tenant data, exactly like go/storage's own objects table: an
// image-generation job's marker is meaningless outside the tenant it was
// enqueued under and must never be visible across one. This is the test the
// postgres and sqlite migration files' own doc comments cite.
func TestImageJobRow_IsTenantScoped_AssertIsolated(t *testing.T) {
	db := newTestDB(t)
	repo := dbkit.NewRepository[imageJobRow](db)

	var seq int
	newRecord := func(tenant pkgcore.TenantID) *imageJobRow {
		seq++
		return &imageJobRow{
			ID:          fmt.Sprintf("job-isolation-%d", seq),
			TenantModel: dbkit.TenantModel{TenantID: string(tenant)},
			Status:      imageJobStatusPending,
		}
	}

	tenancytest.AssertIsolated(t, repo, newRecord)
}

func TestImageJobRepository_Get_NoRow_ReturnsNilNil(t *testing.T) {
	repo := newImageJobRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")

	row, err := repo.get(ctx, "job-does-not-exist")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row != nil {
		t.Fatalf("get returned %+v for a job with no row, want nil", row)
	}
}

func TestImageJobRepository_ClaimPending_FirstCallClaims_SecondCallLosesRace(t *testing.T) {
	repo := newImageJobRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")

	claimed, err := repo.claimPending(ctx, "job-claim-1")
	if err != nil {
		t.Fatalf("first claimPending: %v", err)
	}
	if !claimed {
		t.Fatal("first claimPending returned claimed=false, want true")
	}

	row, err := repo.get(ctx, "job-claim-1")
	if err != nil {
		t.Fatalf("get after claim: %v", err)
	}
	if row == nil || row.Status != imageJobStatusPending {
		t.Fatalf("row after claim = %+v, want status %q", row, imageJobStatusPending)
	}

	// A second claim attempt for the SAME job id -- standing in for a
	// concurrent Handle call racing the first one -- must lose: the
	// primary-key INSERT is the compare-and-swap image_job_store.go's own
	// doc comment describes.
	claimedAgain, err := repo.claimPending(ctx, "job-claim-1")
	if err != nil {
		t.Fatalf("second claimPending: %v", err)
	}
	if claimedAgain {
		t.Fatal("second claimPending for the same job id returned claimed=true, want false -- only one caller may ever own a claim")
	}
}

func TestImageJobRepository_ClaimPending_DistinctJobIDs_BothClaim(t *testing.T) {
	repo := newImageJobRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")

	for _, jobID := range []string{"job-a", "job-b"} {
		claimed, err := repo.claimPending(ctx, jobID)
		if err != nil {
			t.Fatalf("claimPending(%q): %v", jobID, err)
		}
		if !claimed {
			t.Fatalf("claimPending(%q) returned claimed=false, want true for a distinct job id", jobID)
		}
	}
}

func TestImageJobRepository_ReleaseClaim_RemovesPendingRow(t *testing.T) {
	repo := newImageJobRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")

	if _, err := repo.claimPending(ctx, "job-release-1"); err != nil {
		t.Fatalf("claimPending: %v", err)
	}
	if err := repo.releaseClaim(ctx, "job-release-1"); err != nil {
		t.Fatalf("releaseClaim: %v", err)
	}

	row, err := repo.get(ctx, "job-release-1")
	if err != nil {
		t.Fatalf("get after release: %v", err)
	}
	if row != nil {
		t.Fatalf("get after releaseClaim = %+v, want nil -- a fresh retry must start from a clean slate", row)
	}

	// A retry after release can claim again -- the whole point of
	// releasing rather than leaving the row stuck at "pending".
	claimed, err := repo.claimPending(ctx, "job-release-1")
	if err != nil {
		t.Fatalf("claimPending after release: %v", err)
	}
	if !claimed {
		t.Fatal("claimPending after releaseClaim returned claimed=false, want true")
	}
}

func TestImageJobRepository_ReleaseClaim_LeavesGeneratedRowUntouched(t *testing.T) {
	repo := newImageJobRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")

	if _, err := repo.claimPending(ctx, "job-release-2"); err != nil {
		t.Fatalf("claimPending: %v", err)
	}
	if err := repo.markGenerated(ctx, "job-release-2", "image.fake", ImageBytes{Content: []byte("x"), MIME: "image/png"}, ImageUsage{ImageCount: 1}); err != nil {
		t.Fatalf("markGenerated: %v", err)
	}

	// releaseClaim's own status guard must never remove a row that has
	// already advanced past "pending" -- see its own doc comment.
	if err := repo.releaseClaim(ctx, "job-release-2"); err != nil {
		t.Fatalf("releaseClaim: %v", err)
	}

	row, err := repo.get(ctx, "job-release-2")
	if err != nil {
		t.Fatalf("get after releaseClaim: %v", err)
	}
	if row == nil || row.Status != imageJobStatusGenerated {
		t.Fatalf("row after releaseClaim on a generated row = %+v, want it untouched at status %q", row, imageJobStatusGenerated)
	}
}

func TestImageJobRepository_MarkGenerated_TransitionsPendingToGenerated(t *testing.T) {
	repo := newImageJobRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")

	if _, err := repo.claimPending(ctx, "job-generated-1"); err != nil {
		t.Fatalf("claimPending: %v", err)
	}
	img := ImageBytes{Content: []byte("fake-bytes"), MIME: "image/png"}
	usage := ImageUsage{ImageCount: 1, Steps: 30, ResolutionTier: "512x512"}
	if err := repo.markGenerated(ctx, "job-generated-1", "image.fake", img, usage); err != nil {
		t.Fatalf("markGenerated: %v", err)
	}

	row, err := repo.get(ctx, "job-generated-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row == nil {
		t.Fatal("get returned nil after markGenerated")
	}
	if row.Status != imageJobStatusGenerated {
		t.Fatalf("row.Status = %q, want %q", row.Status, imageJobStatusGenerated)
	}
	if row.Provider != "image.fake" {
		t.Fatalf("row.Provider = %q, want %q", row.Provider, "image.fake")
	}
	if got := row.image(); got.MIME != img.MIME || !bytes.Equal(got.Content, img.Content) {
		t.Fatalf("row.image() = %+v, want %+v", got, img)
	}
	if row.usage() != usage {
		t.Fatalf("row.usage() = %+v, want %+v", row.usage(), usage)
	}
}

// TestImageJobRepository_MarkGenerated_NotPending_ReturnsError proves the
// guarded transition refuses to run against a row that is not currently
// "pending" -- the compare-and-swap that would otherwise let two callers
// both believe they recorded the vendor's answer.
func TestImageJobRepository_MarkGenerated_NotPending_ReturnsError(t *testing.T) {
	repo := newImageJobRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")

	if _, err := repo.claimPending(ctx, "job-generated-2"); err != nil {
		t.Fatalf("claimPending: %v", err)
	}
	img := ImageBytes{Content: []byte("x"), MIME: "image/png"}
	if err := repo.markGenerated(ctx, "job-generated-2", "image.fake", img, ImageUsage{}); err != nil {
		t.Fatalf("first markGenerated: %v", err)
	}

	// The row is now "generated", not "pending" -- a second call must fail
	// rather than silently overwrite the first answer.
	if err := repo.markGenerated(ctx, "job-generated-2", "image.other", img, ImageUsage{}); err == nil {
		t.Fatal("second markGenerated on an already-generated row succeeded, want an error")
	}
}

func TestImageJobRepository_MarkGenerated_NoRowAtAll_ReturnsError(t *testing.T) {
	repo := newImageJobRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")

	err := repo.markGenerated(ctx, "job-never-claimed", "image.fake", ImageBytes{}, ImageUsage{})
	if err == nil {
		t.Fatal("markGenerated with no prior claim succeeded, want an error -- markGenerated must never create a row on its own")
	}
}

func TestImageJobRepository_MarkCompleted_TransitionsGeneratedToCompleted_ClearsContent(t *testing.T) {
	repo := newImageJobRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")

	if _, err := repo.claimPending(ctx, "job-completed-1"); err != nil {
		t.Fatalf("claimPending: %v", err)
	}
	img := ImageBytes{Content: []byte("fake-bytes"), MIME: "image/png"}
	if err := repo.markGenerated(ctx, "job-completed-1", "image.fake", img, ImageUsage{ImageCount: 1}); err != nil {
		t.Fatalf("markGenerated: %v", err)
	}

	completed, err := repo.markCompleted(ctx, "job-completed-1", "object-xyz")
	if err != nil {
		t.Fatalf("markCompleted: %v", err)
	}
	if !completed {
		t.Fatal("markCompleted returned completed=false, want true for a genuinely generated row")
	}

	row, err := repo.get(ctx, "job-completed-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row == nil || row.Status != imageJobStatusCompleted {
		t.Fatalf("row after markCompleted = %+v, want status %q", row, imageJobStatusCompleted)
	}
	if row.OutputObjectID != "object-xyz" {
		t.Fatalf("row.OutputObjectID = %q, want %q", row.OutputObjectID, "object-xyz")
	}
	if len(row.Content) != 0 {
		t.Fatalf("row.Content = %q after markCompleted, want cleared", row.Content)
	}
}

// TestImageJobRepository_MarkCompleted_NotGenerated_ReturnsFalse proves the
// guard that makes recordImageUsage fire at most once: a second
// markCompleted call for an already-completed (or still-pending) row must
// report completed=false, never re-transition.
func TestImageJobRepository_MarkCompleted_NotGenerated_ReturnsFalse(t *testing.T) {
	repo := newImageJobRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")

	if _, err := repo.claimPending(ctx, "job-completed-2"); err != nil {
		t.Fatalf("claimPending: %v", err)
	}
	if err := repo.markGenerated(ctx, "job-completed-2", "image.fake", ImageBytes{Content: []byte("x"), MIME: "image/png"}, ImageUsage{}); err != nil {
		t.Fatalf("markGenerated: %v", err)
	}
	first, err := repo.markCompleted(ctx, "job-completed-2", "object-1")
	if err != nil {
		t.Fatalf("first markCompleted: %v", err)
	}
	if !first {
		t.Fatal("first markCompleted returned false, want true")
	}

	second, err := repo.markCompleted(ctx, "job-completed-2", "object-2")
	if err != nil {
		t.Fatalf("second markCompleted: %v", err)
	}
	if second {
		t.Fatal("second markCompleted on an already-completed row returned true, want false")
	}

	// The first completion's own output id must survive untouched.
	row, err := repo.get(ctx, "job-completed-2")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.OutputObjectID != "object-1" {
		t.Fatalf("row.OutputObjectID after a no-op second markCompleted = %q, want the first call's %q", row.OutputObjectID, "object-1")
	}
}
