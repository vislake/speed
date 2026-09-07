package cases

import (
	"context"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// Length bounds for the fields a create request carries. Each is enforced
// in the application because SQLite -- the standalone deployment mode's
// only backend -- does not enforce a VARCHAR length limit at all under its
// type-affinity system (the same reasoning notes' handler.go gives for its
// maxTextLength): the documented column size and a real PostgreSQL column
// under the distributed deployment mode would both reject what SQLite
// would silently store in full.
//
// patientNameMaxLength is a character count -- Unicode code points, not
// bytes -- matching the model's size:200 tag and the DDL's VARCHAR(200)
// (PostgreSQL's VARCHAR(n) is itself a character count), so the check uses
// utf8.RuneCountInString, deliberately not len (len counts UTF-8 bytes and
// would wrongly reject in-bounds multi-byte text; see service_test.go's
// four-byte-rune boundary case, which a byte-counting implementation
// fails).
const (
	patientNameMaxLength = 200
	patientRefMaxLength  = 64

	// photoObjectIDMaxLength is the character bound for one photo's object
	// id, matching casePhotoRecord.ObjectID's size:64 tag and its
	// VARCHAR(64) column on both dialects (PostgreSQL's VARCHAR(n) is a
	// character count, and SQLite -- which enforces no length limit at all
	// under its type-affinity system -- is why the check lives here, the
	// same reasoning patientNameMaxLength's own doc comment gives). 64 is
	// also far beyond any real go/storage object id (application-generated
	// UUIDs, 36 characters), so the bound refuses junk, never a legitimate
	// id.
	photoObjectIDMaxLength = 64

	// maxPhotosPerCaseCreate bounds one create request's photo list. Each
	// photo is its own row (never one unbounded value), so the cap exists
	// to keep a single request sane, not to bound storage: 50 attachments
	// is far beyond what a before/after case actually carries, and a
	// request beyond it is refused outright rather than half-applied.
	maxPhotosPerCaseCreate = 50
)

// The coded errors Service and the cmd/server surface share. Like every
// error this app's hand-mounted surfaces produce, each is a structured
// code plus optional params, never localized text (backend coding
// standard §6.2); the client resolves display text through its own i18n
// catalog. None of these carries a localized message in this package:
// this app's hand-mounted demo surfaces (smilesim, consult) ship codes
// only, and the locale bundles that map them are the consuming UI's.
var (
	// ErrPatientNameRequired is returned when a create request's patient
	// name is empty or all whitespace.
	ErrPatientNameRequired = apperr.Invalid("cases.patient_name_required")

	// ErrPatientNameTooLong is returned when a create request's patient
	// name exceeds patientNameMaxLength characters (counted as in that
	// constant's own doc comment).
	ErrPatientNameTooLong = apperr.Invalid("cases.patient_name_too_long")

	// ErrPatientRefTooLong is returned when a create request's optional
	// patient reference exceeds patientRefMaxLength characters.
	ErrPatientRefTooLong = apperr.Invalid("cases.patient_ref_too_long")

	// ErrPhotoObjectIDRequired is returned when a create request's photo
	// list contains an empty object id -- a malformed entry, refused
	// rather than silently skipped. The check runs on the trimmed value
	// (see Create's validation order), so a whitespace-only id -- which
	// trims to empty -- is refused here too, exactly as an explicitly
	// empty one is.
	ErrPhotoObjectIDRequired = apperr.Invalid("cases.photo_object_id_required")

	// ErrPhotoObjectIDTooLong is returned when a create request's photo
	// list contains an object id longer than photoObjectIDMaxLength
	// characters, with the limit and offending length in params.
	ErrPhotoObjectIDTooLong = apperr.Invalid("cases.photo_object_id_too_long")

	// ErrDuplicatePhotoObject is returned when a create request's photo
	// list names the same object id twice, with the duplicate's object id
	// in params -- a client bug, refused before anything is inserted.
	ErrDuplicatePhotoObject = apperr.Invalid("cases.duplicate_photo_object")

	// ErrTooManyPhotos is returned when a create request's photo list
	// exceeds maxPhotosPerCaseCreate entries, with the limit in params.
	ErrTooManyPhotos = apperr.Invalid("cases.too_many_photos")

	// ErrPhotoAlreadyAttached is returned when a create request names a
	// photo object that is already attached to another case of the same
	// tenant, with the object id in params. It is a Conflict, not an
	// Invalid: the request is well-formed, it just collides with an
	// existing fact (one tenant, one case per photo object -- see
	// casePhotoRecord's own doc comment).
	ErrPhotoAlreadyAttached = apperr.Conflict("cases.photo_already_attached")

	// ErrNotFound is returned by Get when no case with the given id exists
	// under the caller's tenant. Like dbkit's own record-not-found, it
	// deliberately does not distinguish "never existed" from "exists under
	// another tenant": either answer would leak another tenant's id space.
	ErrNotFound = apperr.NotFound("cases.not_found")

	// ErrSubjectUnresolved is returned when a create request cannot be
	// attributed to a user: no SubjectResolver is wired, or the wired
	// resolver could not name the request's creator. The create surface
	// needs the creator's user id for the case row's recorded
	// CreatorUserID, so an unattributable request is refused with a 401
	// before any case is created -- never served an empty or invented
	// creator -- mirroring the identical rule notes' SubjectResolver seam
	// documents. Only the create route resolves a subject: the list,
	// detail, upload and photo-content routes need no creator and read
	// no resolver. cmd/server's wireCasesRoutes is the only caller that
	// produces it.
	ErrSubjectUnresolved = apperr.Unauthorized("cases.subject_unresolved")
)

// SubjectResolver is the seam through which the HTTP surface attributes
// a create request to the user who made it -- the answer becomes the
// case row's CreatorUserID (recorded attribution; the clinic's case
// list is tenant-scoped and reads no creator). It is cases' own copy of the
// identical declaration notes' handler.go carries: this package imports no
// other app-internal package (see the package doc comment's "Shape
// decision" section -- the case layer is deliberately free of the
// simulation layer and of notes' module machinery), so each independent
// package declares the seam, and one host type (cmd/server's
// demoNotesSubjectResolver) satisfies both -- compile-time-checked at the
// bottom of cmd/server/cases.go. A resolver that returns ok=false -- or is
// not wired at all -- fails every create and list request closed with
// ErrSubjectUnresolved, never an invented or empty creator.
type SubjectResolver interface {
	Subject(r *http.Request) (string, bool)
}

// Case is a case row as Service methods return it -- the domain value a
// caller (the HTTP surface, tests) renders, deliberately not the row
// model: TenantID stays internal, and the fields below are exactly what
// the P3 UI's list and detail views render.
type Case struct {
	// ID is the case's application-generated id.
	ID string

	// PatientName is the clinic-entered display name (model.go's
	// caseRecord.PatientName).
	PatientName string

	// PatientRef is the optional clinic-given patient reference.
	PatientRef string

	// CreatorUserID is the user id of the staff member who created the
	// case, as the host's SubjectResolver seam attributed the create
	// request. It is the row's recorded attribution -- never a list key:
	// the clinic's case list is tenant-scoped, so every member of the
	// tenant sees every case of the tenant.
	CreatorUserID string

	// CreatedAt is when the case was created.
	CreatedAt time.Time
}

// Photo is one photo attachment of a case as Service methods return it.
type Photo struct {
	// ObjectID is the referenced go/storage photo object's id -- the value
	// that feeds smilesim's per-photo enumeration (see the package doc
	// comment's "Shape decision" section for the deliberate non-join).
	ObjectID string

	// Position is the 0-based attachment-order index within the case.
	Position int

	// CreatedAt is when the photo was attached.
	CreatedAt time.Time
}

// Service is the cases domain layer's facade: the validation and
// orchestration on top of Repository that create/list/detail requests go
// through. Construct one with NewService. The zero value is not ready to
// use.
type Service struct {
	repo *Repository
}

// NewService returns a Service backed by repo (whose EnsureSchema the
// caller must have run once -- cmd/server's wiring does this). Constructing
// one performs no I/O.
func NewService(repo *Repository) *Service {
	return &Service{repo: repo}
}

// CreateInput is Service.Create's argument: what one create request may
// say about a new case. Every field is plain request content -- the tenant
// and the creator never travel in here: the tenant comes from ctx (the
// repository resolves it, per every Repository write's
// overwrite-the-caller guarantee) and the creator comes from the host's
// SubjectResolver seam at the HTTP layer, never from a body the caller
// controls.
type CreateInput struct {
	// PatientName is the patient's display name, required.
	PatientName string

	// PatientRef is the optional clinic-given patient reference.
	PatientRef string

	// CreatorUserID is the creating user's id, as the SubjectResolver
	// seam attributed it. An empty value is legal at this layer (tests and
	// seed code create unattributed cases; the row's DEFAULT '' sentinel
	// exists for exactly that) -- the HTTP surface is what refuses an
	// unattributable request, with ErrSubjectUnresolved, before Service
	// is ever called.
	CreatorUserID string

	// PhotoObjectIDs is the case's initial photos, in attachment order:
	// references to already-uploaded go/storage objects of the caller's
	// tenant. Optional -- an empty list creates a case with no photos yet,
	// for an intake-first flow -- and each entry must be a non-empty object
	// id, unique within the list and not already attached to another case
	// of the same tenant (the checks below refuse each violation with its
	// own coded error before anything is inserted).
	PhotoObjectIDs []string
}

// Create validates input and durably creates the case with all of its
// photo rows in one transaction, returning the created case and its photos
// (positions 0..n-1, in input order) for the caller to echo back.
//
// ctx must carry a tenant: the repository resolves it for every row and
// fails the whole call closed when it is absent (never a case written
// under an empty tenant).
//
// Validation order, each failure refusing the request BEFORE the database
// is touched -- the "refuse before any side effect" posture smilesim's
// Simulate applies to its option validation:
//
//  1. patient name trimmed, required, and bounded at patientNameMaxLength
//     runes;
//  2. patient reference trimmed (an empty one stays the "" sentinel) and
//     bounded at patientRefMaxLength runes;
//  3. photo list bounded at maxPhotosPerCaseCreate entries, every entry
//     trimmed once (the trimmed value is the only spelling that reaches
//     any later check or any written row), non-empty after the trim
//     (ErrPhotoObjectIDRequired), bounded at photoObjectIDMaxLength runes
//     (ErrPhotoObjectIDTooLong), no entry repeated
//     (ErrDuplicatePhotoObject, naming the duplicate);
//  4. per photo, a tenant-scoped pre-flight that the object is not already
//     attached to another case of the same tenant (ErrPhotoAlreadyAttached,
//     naming the object). The pre-flight is the friendly answer to the
//     sequential double-create; the residual race -- two creates attaching
//     the same object in the same instant -- is caught one level down by
//     the uq_case_photos_tenant_object unique index and surfaces as a raw
//     create error rather than this coded conflict, recorded honestly in
//     the package doc comment's "Honest record" section rather than
//     papered over.
//
// The row ids (case and photos alike) are application-generated UUIDs,
// minted here, never by the database -- the backend coding standard §5
// rule caseRecord's own doc comment states.
func (s *Service) Create(ctx context.Context, in CreateInput) (Case, []Photo, error) {
	patientName := strings.TrimSpace(in.PatientName)
	if patientName == "" {
		return Case{}, nil, ErrPatientNameRequired
	}
	if length := utf8.RuneCountInString(patientName); length > patientNameMaxLength {
		return Case{}, nil, ErrPatientNameTooLong.WithParam("limit", patientNameMaxLength).WithParam("length", length)
	}

	patientRef := strings.TrimSpace(in.PatientRef)
	if length := utf8.RuneCountInString(patientRef); length > patientRefMaxLength {
		return Case{}, nil, ErrPatientRefTooLong.WithParam("limit", patientRefMaxLength).WithParam("length", length)
	}

	if len(in.PhotoObjectIDs) > maxPhotosPerCaseCreate {
		return Case{}, nil, ErrTooManyPhotos.WithParam("limit", maxPhotosPerCaseCreate).WithParam("length", len(in.PhotoObjectIDs))
	}
	// Each photo object id is trimmed once, up front, and the trimmed
	// value is what every later check and every written row sees: a
	// spelling difference is a client accident, never a distinct storage
	// key, so "abc" and " abc " must not become two object_id values that
	// defeat the uq_case_photos_tenant_object invariant (the same photo
	// attached to two cases under two spellings) or send smilesim's
	// per-photo enumeration chasing two keys for one object.
	photoObjectIDs := make([]string, len(in.PhotoObjectIDs))
	for i, objectID := range in.PhotoObjectIDs {
		photoObjectIDs[i] = strings.TrimSpace(objectID)
	}

	seen := make(map[string]struct{}, len(photoObjectIDs))
	for _, objectID := range photoObjectIDs {
		if objectID == "" {
			return Case{}, nil, ErrPhotoObjectIDRequired
		}
		if length := utf8.RuneCountInString(objectID); length > photoObjectIDMaxLength {
			return Case{}, nil, ErrPhotoObjectIDTooLong.
				WithParam("limit", photoObjectIDMaxLength).
				WithParam("length", length)
		}
		if _, dup := seen[objectID]; dup {
			return Case{}, nil, ErrDuplicatePhotoObject.WithParam("object_id", objectID)
		}
		seen[objectID] = struct{}{}
	}

	record := &caseRecord{
		ID:            uuid.NewString(),
		PatientName:   patientName,
		PatientRef:    patientRef,
		CreatorUserID: in.CreatorUserID,
	}
	photos := make([]casePhotoRecord, 0, len(photoObjectIDs))
	for position, objectID := range photoObjectIDs {
		// Pre-flight before any insert: see Create's own doc comment above
		// for what the sequential check catches and what the residual race
		// looks like. The lookup is keyed on the object_id column, tenant-
		// scoped -- never the photo row's own id (see repository.go's
		// photoObjectTaken doc comment).
		attached, err := s.repo.photoObjectTaken(ctx, objectID)
		if err != nil {
			return Case{}, nil, err
		}
		if attached {
			return Case{}, nil, ErrPhotoAlreadyAttached.WithParam("object_id", objectID)
		}
		photos = append(photos, casePhotoRecord{
			ID:       uuid.NewString(),
			CaseID:   record.ID,
			ObjectID: objectID,
			Position: position,
		})
	}

	if err := s.repo.createCaseWithPhotos(ctx, record, photos); err != nil {
		return Case{}, nil, err
	}
	return toCase(*record), toPhotos(photos), nil
}

// List returns every case of ctx's tenant, newest first -- the
// clinic-wide list the round's product decision names: a case one staff
// member created must be visible to every member of the same clinic
// (the colleague who sees the patient next), and only the tenant
// boundary hides a case from another practice. A tenant with no cases
// answers an empty list, never an error; ctx must carry a tenant (the
// underlying listByTenant fails closed without one, never listing
// across tenants).
func (s *Service) List(ctx context.Context) ([]Case, error) {
	records, err := s.repo.listByTenant(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Case, 0, len(records))
	for _, record := range records {
		out = append(out, toCase(record))
	}
	return out, nil
}

// Get returns ctx's tenant's case caseID with its photos in attachment
// order -- the case detail the P3 UI renders (its per-photo simulations
// are fetched from the P2a enumeration route; see the package doc
// comment's "Shape decision" section). A case that does not exist under
// ctx's tenant -- including one that exists under another tenant, which is
// deliberately indistinguishable -- answers ErrNotFound. A case with no
// photos answers an empty photo slice, never an error.
func (s *Service) Get(ctx context.Context, caseID string) (Case, []Photo, error) {
	record, err := s.repo.FindByID(ctx, caseID)
	if err != nil {
		if isRecordNotFound(err) {
			return Case{}, nil, ErrNotFound
		}
		return Case{}, nil, err
	}
	photoRecords, err := s.repo.listPhotosOf(ctx, caseID)
	if err != nil {
		return Case{}, nil, err
	}
	return toCase(*record), toPhotos(photoRecords), nil
}

// isRecordNotFound reports whether err is dbkit's not-found error, matched
// by Code rather than by identity (apperr.WithParam always derives a new
// *apperr.Error, so pointer identity is not stable across the decoration
// the underlying repository applies).
func isRecordNotFound(err error) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == dbkit.ErrRecordNotFound.Code
}

// toCase maps a caseRecord row onto the exported Case value.
func toCase(record caseRecord) Case {
	return Case{
		ID:            record.ID,
		PatientName:   record.PatientName,
		PatientRef:    record.PatientRef,
		CreatorUserID: record.CreatorUserID,
		CreatedAt:     record.CreatedAt,
	}
}

// toPhotos maps casePhotoRecord rows onto exported Photo values, always
// returning a non-nil empty slice for no rows (a caller rendering JSON
// wants "photos": [] never "photos": null).
func toPhotos(records []casePhotoRecord) []Photo {
	photos := make([]Photo, 0, len(records))
	for _, record := range records {
		photos = append(photos, Photo{
			ObjectID:  record.ObjectID,
			Position:  record.Position,
			CreatedAt: record.CreatedAt,
		})
	}
	return photos
}
