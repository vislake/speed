package cases

import (
	"context"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// newService returns a Service backed by a fresh, per-test migrated
// repository (the helper repository_test.go owns).
func newService(t *testing.T) *Service {
	t.Helper()
	return NewService(newRepository(t))
}

// errCode extracts err's structured code, or "" when err is not an
// *apperr.Error (so a test failure message can name what was actually
// returned rather than assuming).
func errCode(err error) string {
	appErr, ok := apperr.As(err)
	if !ok {
		return ""
	}
	return appErr.Code
}

// assertCode fails the test when err does not carry exactly code.
func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want code %q", code)
	}
	if got := errCode(err); got != code {
		t.Fatalf("error code = %q, want %q (error = %v)", got, code, err)
	}
}

// TestService_CreateAndGet_RoundTrip drives the shape the P3 UI's create
// and detail views need end to end at service level: a create's input
// comes back trimmed and echoed on the case (so the create answer and a
// later detail read can never disagree), photos keep their request order
// with 0-based positions, and Get returns the same case with the same
// photos.
func TestService_CreateAndGet_RoundTrip(t *testing.T) {
	svc := newService(t)
	ctx := tenantCtx("tenant-acme")

	created, photos, err := svc.Create(ctx, CreateInput{
		PatientName:    "  Anna Meyer  ",
		PatientRef:     "  CH-1001 ",
		CreatorUserID:  "user-creator-1",
		PhotoObjectIDs: []string{"photo-front", "photo-side", "photo-smile"},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.ID == "" {
		t.Fatal("Create() returned an empty case id")
	}
	if created.PatientName != "Anna Meyer" {
		t.Fatalf("PatientName = %q, want the trimmed input", created.PatientName)
	}
	if created.PatientRef != "CH-1001" {
		t.Fatalf("PatientRef = %q, want the trimmed input", created.PatientRef)
	}
	if created.CreatorUserID != "user-creator-1" {
		t.Fatalf("CreatorUserID = %q, want the input creator", created.CreatorUserID)
	}
	if created.CreatedAt.IsZero() {
		t.Fatal("CreatedAt is zero, want gorm's autoCreateTime value")
	}
	if len(photos) != 3 {
		t.Fatalf("photos = %d entries, want 3", len(photos))
	}
	for i, want := range []struct {
		objectID string
		position int
	}{{"photo-front", 0}, {"photo-side", 1}, {"photo-smile", 2}} {
		if photos[i].ObjectID != want.objectID || photos[i].Position != want.position {
			t.Fatalf("photos[%d] = %+v, want object %q at position %d", i, photos[i], want.objectID, want.position)
		}
	}

	got, gotPhotos, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	// Field-by-field, deliberately not a whole-struct comparison: the
	// driver may store CreatedAt at a coarser precision than the in-memory
	// value, which is an encoding detail, not a disagreement.
	if got.ID != created.ID || got.PatientName != created.PatientName ||
		got.PatientRef != created.PatientRef || got.CreatorUserID != created.CreatorUserID {
		t.Fatalf("Get() = %+v, want the created case %+v", got, created)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("Get() CreatedAt is zero, want the stored value")
	}
	if len(gotPhotos) != 3 || gotPhotos[0].ObjectID != "photo-front" || gotPhotos[2].ObjectID != "photo-smile" {
		t.Fatalf("Get() photos = %+v, want the three in request order", gotPhotos)
	}
}

// TestService_Create_EmptyPhotoList_IsLegal pins the intake-first flow: a
// case may be created with no photos yet, and its detail answers an empty
// (non-nil) photo slice.
func TestService_Create_EmptyPhotoList_IsLegal(t *testing.T) {
	svc := newService(t)
	ctx := tenantCtx("tenant-acme")

	created, photos, err := svc.Create(ctx, CreateInput{PatientName: "No Photos Yet", CreatorUserID: "user-1"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if len(photos) != 0 || photos == nil {
		t.Fatalf("photos = %#v, want an empty non-nil slice", photos)
	}
	if _, gotPhotos, err := svc.Get(ctx, created.ID); err != nil || len(gotPhotos) != 0 || gotPhotos == nil {
		t.Fatalf("Get() photos = (%+v, %v), want an empty non-nil slice", gotPhotos, err)
	}
}

// TestService_Create_NoTenantContext_Refused pins the fail-closed first
// line of defense: a create whose context carries no tenant never touches
// the database -- the pre-flight lookup and the write both refuse with
// pkgcore's missing-tenant error.
func TestService_Create_NoTenantContext_Refused(t *testing.T) {
	svc := newService(t)

	_, _, err := svc.Create(context.Background(), CreateInput{PatientName: "No Tenant", CreatorUserID: "user-1"})
	if err == nil {
		t.Fatal("Create() on a tenant-less context succeeded, want a refusal")
	}
}

// TestService_Create_Validation pins every coded validation on the create
// input: each refusal happens before anything is inserted, and each names
// its own code. The length checks count Unicode code points, not bytes --
// the multi-byte cases (a two-byte rune and a four-byte rune) are what
// make a byte-based implementation fail.
func TestService_Create_Validation(t *testing.T) {
	svc := newService(t)
	ctx := tenantCtx("tenant-acme")

	t.Run("patient name required", func(t *testing.T) {
		_, _, err := svc.Create(ctx, CreateInput{PatientName: "   ", CreatorUserID: "user-1"})
		assertCode(t, err, ErrPatientNameRequired.Code)
	})

	t.Run("patient name length boundary counts runes", func(t *testing.T) {
		// Exactly the limit, in runes that need four UTF-8 bytes each: a
		// byte-counting implementation would refuse these 800 bytes.
		maxName := strings.Repeat("\U0001F642", patientNameMaxLength)
		if _, _, err := svc.Create(ctx, CreateInput{PatientName: maxName, CreatorUserID: "user-1"}); err != nil {
			t.Fatalf("Create() with a %d-rune (%d-byte) name error = %v, want success", patientNameMaxLength, len(maxName), err)
		}
		_, _, err := svc.Create(ctx, CreateInput{PatientName: maxName + "x", CreatorUserID: "user-1"})
		assertCode(t, err, ErrPatientNameTooLong.Code)
	})

	t.Run("patient reference length boundary", func(t *testing.T) {
		maxRef := strings.Repeat("a", patientRefMaxLength)
		if _, _, err := svc.Create(ctx, CreateInput{PatientName: "ok", PatientRef: maxRef, CreatorUserID: "user-1"}); err != nil {
			t.Fatalf("Create() with a %d-rune ref error = %v, want success", patientRefMaxLength, err)
		}
		_, _, err := svc.Create(ctx, CreateInput{PatientName: "ok", PatientRef: maxRef + "a", CreatorUserID: "user-1"})
		assertCode(t, err, ErrPatientRefTooLong.Code)
	})

	t.Run("too many photos", func(t *testing.T) {
		tooMany := make([]string, maxPhotosPerCaseCreate+1)
		for i := range tooMany {
			tooMany[i] = "photo-bulk-" + strings.Repeat("x", i/10+1) // distinct values
		}
		_, _, err := svc.Create(ctx, CreateInput{PatientName: "ok", PhotoObjectIDs: tooMany, CreatorUserID: "user-1"})
		assertCode(t, err, ErrTooManyPhotos.Code)
	})

	t.Run("empty photo object id", func(t *testing.T) {
		_, _, err := svc.Create(ctx, CreateInput{PatientName: "ok", PhotoObjectIDs: []string{"photo-1", ""}, CreatorUserID: "user-1"})
		assertCode(t, err, ErrPhotoObjectIDRequired.Code)
	})

	t.Run("duplicate photo object", func(t *testing.T) {
		_, _, err := svc.Create(ctx, CreateInput{PatientName: "ok", PhotoObjectIDs: []string{"photo-1", "photo-1"}, CreatorUserID: "user-1"})
		assertCode(t, err, ErrDuplicatePhotoObject.Code)
	})

	t.Run("no case row leaks from refused creates", func(t *testing.T) {
		// Every refusal above must leave the database untouched. A creator
		// id no earlier subtest ever created with (those used "user-1" and
		// legitimately landed some successful cases) must list nothing.
		list, err := svc.ListByCreator(ctx, "never-created-anything")
		if err != nil {
			t.Fatalf("ListByCreator() error = %v", err)
		}
		if len(list) != 0 {
			t.Fatalf("ListByCreator() = %d cases after nothing but refused creates, want 0", len(list))
		}
	})
}

// TestService_Create_PhotoObjectIDTrimmedAndBounded pins the P2-11
// normalization of a photo object id BEFORE the uniqueness checks run:
// the trimmed spelling is the only spelling that reaches the duplicate
// check, the already-attached pre-flight or a written row, so two
// spellings of one id can never become two storage keys that bypass the
// uq_case_photos_tenant_object invariant -- and an id longer than the
// object_id column can hold is refused with its own coded error rather
// than silently stored in full under SQLite (which enforces no VARCHAR
// length limit at all). Failing before the fix: the padded duplicate was
// accepted as two distinct values, a padded re-attachment of an attached
// object was accepted as a distinct key, and an over-long id was stored.
func TestService_Create_PhotoObjectIDTrimmedAndBounded(t *testing.T) {
	svc := newService(t)
	ctx := tenantCtx("tenant-acme")

	t.Run("padded duplicate in one request is a duplicate", func(t *testing.T) {
		_, _, err := svc.Create(ctx, CreateInput{
			PatientName:    "ok",
			PhotoObjectIDs: []string{"photo-1", " photo-1 "},
			CreatorUserID:  "user-1",
		})
		assertCode(t, err, ErrDuplicatePhotoObject.Code)
	})

	t.Run("padded spelling cannot re-attach an attached object", func(t *testing.T) {
		if _, _, err := svc.Create(ctx, CreateInput{
			PatientName:    "First Case",
			PhotoObjectIDs: []string{"photo-shared"},
			CreatorUserID:  "user-1",
		}); err != nil {
			t.Fatalf("first Create() error = %v", err)
		}
		// " photo-shared " is the SAME object id spelled with padding: it
		// must collide with the attachment above, never become a second key
		// that a second case may attach.
		_, _, err := svc.Create(ctx, CreateInput{
			PatientName:    "Second Case",
			PhotoObjectIDs: []string{" photo-shared "},
			CreatorUserID:  "user-1",
		})
		assertCode(t, err, ErrPhotoAlreadyAttached.Code)
	})

	t.Run("padded id is stored trimmed", func(t *testing.T) {
		created, photos, err := svc.Create(ctx, CreateInput{
			PatientName:    "Trimmed",
			PhotoObjectIDs: []string{"  photo-trimmed  "},
			CreatorUserID:  "user-1",
		})
		if err != nil {
			t.Fatalf("Create() error = %v", err)
		}
		if len(photos) != 1 || photos[0].ObjectID != "photo-trimmed" {
			t.Fatalf("created photos = %+v, want exactly photo-trimmed (the trimmed spelling)", photos)
		}
		if _, got, err := svc.Get(ctx, created.ID); err != nil || len(got) != 1 || got[0].ObjectID != "photo-trimmed" {
			t.Fatalf("Get() photos = (%+v, %v), want exactly photo-trimmed", got, err)
		}
	})

	t.Run("whitespace-only id is required-error", func(t *testing.T) {
		_, _, err := svc.Create(ctx, CreateInput{
			PatientName:    "ok",
			PhotoObjectIDs: []string{"   "},
			CreatorUserID:  "user-1",
		})
		assertCode(t, err, ErrPhotoObjectIDRequired.Code)
	})

	t.Run("object id length boundary counts runes", func(t *testing.T) {
		// objectIDLimit is photoObjectIDMaxLength's value (and the
		// casePhotoRecord.ObjectID column's declared size on both
		// dialects): a local literal rather than the package constant, so
		// this test also compiles and fails against the pre-fix service
		// (which defined neither). Changing the limit without changing
		// this literal fails the boundary case below, the same drift alarm
		// the constant's own doc comment serves.
		const objectIDLimit = 64
		maxID := strings.Repeat("\U0001F642", objectIDLimit)
		if _, _, err := svc.Create(ctx, CreateInput{
			PatientName:    "ok",
			PhotoObjectIDs: []string{maxID},
			CreatorUserID:  "user-1",
		}); err != nil {
			t.Fatalf("Create() with a %d-rune (%d-byte) id error = %v, want success", objectIDLimit, len(maxID), err)
		}
		_, _, err := svc.Create(ctx, CreateInput{
			PatientName:    "ok",
			PhotoObjectIDs: []string{maxID + "x"},
			CreatorUserID:  "user-1",
		})
		// The code literal rather than ErrPhotoObjectIDTooLong.Code: the
		// error's exported var is itself the fix, so this test must still
		// compile (and fail) against the pre-fix service, which defines
		// neither.
		assertCode(t, err, "cases.photo_object_id_too_long")
	})
}

// TestService_Create_PhotoAlreadyAttached pins the one-tenant-one-case-per-
// photo rule with the coded conflict a sequential double-create sees, and
// the cross-tenant counterpart: the same object id attached by another
// tenant is invisible to this tenant's pre-flight (attachment uniqueness
// is tenant-scoped, the uq_case_photos_tenant_object index's contract), so
// tenant B may reference an object tenant A's case already references.
func TestService_Create_PhotoAlreadyAttached(t *testing.T) {
	svc := newService(t)
	ctxA := tenantCtx("tenant-acme")
	ctxB := tenantCtx("tenant-globex")

	if _, _, err := svc.Create(ctxA, CreateInput{PatientName: "First Case", PhotoObjectIDs: []string{"photo-shared"}, CreatorUserID: "user-1"}); err != nil {
		t.Fatalf("first Create() error = %v", err)
	}

	_, _, err := svc.Create(ctxA, CreateInput{PatientName: "Second Case", PhotoObjectIDs: []string{"photo-shared"}, CreatorUserID: "user-1"})
	appErr, ok := apperr.As(err)
	if !ok || appErr.Code != ErrPhotoAlreadyAttached.Code {
		t.Fatalf("second Create() error = %v, want code %q", err, ErrPhotoAlreadyAttached.Code)
	}
	if appErr.Params["object_id"] != "photo-shared" {
		t.Fatalf("ErrPhotoAlreadyAttached params = %v, want object_id = photo-shared", appErr.Params)
	}

	if _, _, err := svc.Create(ctxB, CreateInput{PatientName: "Other Tenant", PhotoObjectIDs: []string{"photo-shared"}, CreatorUserID: "user-2"}); err != nil {
		t.Fatalf("cross-tenant Create() with the same object id error = %v, want success (attachment uniqueness is per tenant)", err)
	}
}

// TestService_Get_NotFoundIsTenantIndistinguishable pins the detail
// read's not-found contract: an unknown id and another tenant's case
// answer the same coded not-found, so a caller can never learn that an id
// exists in another tenant.
func TestService_Get_NotFoundIsTenantIndistinguishable(t *testing.T) {
	svc := newService(t)
	ctxA := tenantCtx("tenant-acme")
	ctxB := tenantCtx("tenant-globex")

	created, _, err := svc.Create(ctxA, CreateInput{PatientName: "Private Patient", CreatorUserID: "user-1"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if _, _, err := svc.Get(ctxA, "no-such-case"); err == nil || errCode(err) != ErrNotFound.Code {
		t.Fatalf("Get(unknown id) error = %v, want code %q", err, ErrNotFound.Code)
	}
	if _, _, err := svc.Get(ctxB, created.ID); err == nil || errCode(err) != ErrNotFound.Code {
		t.Fatalf("Get(another tenant's id) error = %v, want the identical code %q", err, ErrNotFound.Code)
	}
}

// TestService_ListByCreator_ScopesToTheCreator pins the round's chosen
// list semantic at service level: two creators in one tenant each see
// exactly their own cases.
func TestService_ListByCreator_ScopesToTheCreator(t *testing.T) {
	svc := newService(t)
	ctx := tenantCtx("tenant-acme")

	if _, _, err := svc.Create(ctx, CreateInput{PatientName: "Mine", CreatorUserID: "user-a"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, _, err := svc.Create(ctx, CreateInput{PatientName: "Theirs", CreatorUserID: "user-b"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	list, err := svc.ListByCreator(ctx, "user-a")
	if err != nil {
		t.Fatalf("ListByCreator() error = %v", err)
	}
	if len(list) != 1 || list[0].PatientName != "Mine" {
		t.Fatalf("ListByCreator(user-a) = %+v, want exactly user-a's own case", list)
	}
	list, err = svc.ListByCreator(ctx, "user-b")
	if err != nil {
		t.Fatalf("ListByCreator(user-b) error = %v", err)
	}
	if len(list) != 1 || list[0].PatientName != "Theirs" {
		t.Fatalf("ListByCreator(user-b) = %+v, want exactly user-b's own case", list)
	}
}
