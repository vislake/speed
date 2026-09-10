package flowtests

// notes_delete_restore_flow_test.go drives the notes module's HTTP
// delete/restore operations -- the pair the module's fragment, handler and
// generated interface carry together -- through the reference app's real
// composed stack (buildTestServer's output: authn, tenancy, the rbac route
// gate and a real SQLite database). The repository-level mark-delete
// lifecycle was already proven (internal/notes/repository_test.go); what
// these tests pin is the HTTP surface on top of it: the uniform 404
// refusal semantics the fragment documents (an unknown id, another
// tenant's id, an already-deleted note for delete, and a live note for
// restore all answer notes.note_not_found -- never a disclosure of which
// case it was), the delete/restore round trip through the list operation
// (a deleted note disappears from the list, a restored one returns with
// its text intact), and the rbac gate answering for the new verbs exactly
// as it answers for the old ones.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/app/demo"
)

// noteMutationRequestAs issues method against a per-note path under
// /api/v1/notes with the given bearer token and acting demo user --
// notesRequestAs's (server_test.go) per-item twin, kept local because that
// helper is hard-wired to the collection path.
func noteMutationRequestAs(t *testing.T, srv *httptest.Server, method, token, notePath, user string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(method, srv.URL+"/api/v1/notes"+notePath, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if user != "" {
		req.Header.Set(demo.DemoUserHeader, user)
		req.Header.Set(demo.DemoOrgUserHeader, demo.DemoNotesCreatorUserID)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s /api/v1/notes%s (user=%q): %v", method, notePath, user, err)
	}
	return resp
}

// assertNoteNotFound reads resp and requires the module's uniform 404:
// the notes.note_not_found code on the wire, whatever the underlying
// repository case was.
func assertNoteNotFound(t *testing.T, resp *http.Response, what string) {
	t.Helper()
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: read body: %v", what, err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("%s: status = %d, want %d; body = %s", what, resp.StatusCode, http.StatusNotFound, body)
	}
	var decoded struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("%s: decoding %s: %v", what, body, err)
	}
	if decoded.Code != "notes.note_not_found" {
		t.Fatalf("%s: error code = %q, want %q; body = %s", what, decoded.Code, "notes.note_not_found", body)
	}
}

// TestNotesDeleteRestore_HTTPLifecycle is the delete/restore pair's
// end-to-end shape: create, delete, list-without, restore, list-with,
// delete again -- each leg asserting the answer the fragment documents --
// plus the refusal legs: a second delete of the already-deleted note,
// a restore of a live note, an unknown id, and another tenant's id all
// answer the SAME uniform notes.note_not_found 404, so no caller can tell
// which case it hit.
func TestNotesDeleteRestore_HTTPLifecycle(t *testing.T) {
	srv, cfg, _ := buildTestServer(t)
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "delete-restore-acme")
	globexToken := registerAndAuthenticate(t, srv, cfg, "tenant-globex", "delete-restore-globex")

	const noteText = "a note that will be deleted and restored"
	noteID := createNoteAs(t, srv, acmeToken, noteText)

	// The not-yet-deleted refusal leg runs FIRST, while the note is live:
	// restoring it now must answer the uniform 404 (a live note is not in
	// the state restore needs), with no hint that the note exists.
	resp := noteMutationRequestAs(t, srv, http.MethodPost, acmeToken, "/"+noteID+"/restore", demo.DemoOwnerUserID)
	assertNoteNotFound(t, resp, "restore of a live note")

	// Delete: 204, and the note leaves the tenant's list.
	resp = noteMutationRequestAs(t, srv, http.MethodDelete, acmeToken, "/"+noteID, demo.DemoOwnerUserID)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	if got := listNotesAs(t, srv, acmeToken); len(got) != 0 {
		t.Fatalf("tenant-acme sees %d notes after the delete, want 0 (%+v)", len(got), got)
	}

	// Deleting the already-deleted note again answers the same 404 -- the
	// mark-delete refuses to clobber the original deleted_at/deleted_by
	// attribution with a second write, and the client cannot distinguish
	// this case from an unknown id.
	resp = noteMutationRequestAs(t, srv, http.MethodDelete, acmeToken, "/"+noteID, demo.DemoOwnerUserID)
	assertNoteNotFound(t, resp, "delete of the already-deleted note")

	// Restore: 204, and the note is visible again with its text intact.
	resp = noteMutationRequestAs(t, srv, http.MethodPost, acmeToken, "/"+noteID+"/restore", demo.DemoOwnerUserID)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("restore status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	notes := listNotesAs(t, srv, acmeToken)
	if len(notes) != 1 || notes[0].ID != noteID || notes[0].Text != noteText {
		t.Fatalf("tenant-acme sees %+v after the restore, want the restored note with its text intact", notes)
	}

	// The restored note is deletable again -- a fresh mark, and the note
	// leaves the list once more.
	resp = noteMutationRequestAs(t, srv, http.MethodDelete, acmeToken, "/"+noteID, demo.DemoOwnerUserID)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("second-lifecycle delete status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	if got := listNotesAs(t, srv, acmeToken); len(got) != 0 {
		t.Fatalf("tenant-acme sees %d notes after the second delete, want 0 (%+v)", len(got), got)
	}

	// Cross-tenant refusal: tenant-globex's owner -- holding notes:write in
	// ITS OWN tenant, so the rbac gate passes -- cannot delete tenant-acme's
	// note, and the 404 it gets is the same uniform one (never a hint that
	// the note exists, let alone whose it is).
	resp = noteMutationRequestAs(t, srv, http.MethodDelete, globexToken, "/"+noteID, demo.DemoOwnerUserID)
	assertNoteNotFound(t, resp, "another tenant's delete of acme's note")

	// Unknown id: the same uniform 404.
	resp = noteMutationRequestAs(t, srv, http.MethodDelete, acmeToken, "/00000000-0000-0000-0000-000000000000", demo.DemoOwnerUserID)
	assertNoteNotFound(t, resp, "delete of an unknown note id")

	// Restoring another tenant's deleted note: tenant-globex cannot restore
	// the note tenant-acme just deleted, uniform 404 again.
	resp = noteMutationRequestAs(t, srv, http.MethodPost, globexToken, "/"+noteID+"/restore", demo.DemoOwnerUserID)
	assertNoteNotFound(t, resp, "another tenant's restore of acme's deleted note")
}

// TestNotesDelete_RbacGate_AnswersForTheNewVerb pins that the route gate
// answers for the DELETE verb exactly as it does for the create POST: a
// caller holding notes:read alone is refused with rbac.permission_denied
// (403) before the handler runs -- DemoPermissionFor maps any non-read
// method to the write permission, so the item-level delete needs no
// gate-table change to be protected.
func TestNotesDelete_RbacGate_AnswersForTheNewVerb(t *testing.T) {
	srv, cfg, _ := buildTestServer(t)
	ownerToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "delete-gate-owner")
	readerToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "delete-gate-reader")

	noteID := createNoteAs(t, srv, ownerToken, "gate-probe note")
	resp := noteMutationRequestAs(t, srv, http.MethodDelete, readerToken, "/"+noteID, demo.DemoReaderUserID)
	assertPermissionDenied(t, resp, "demo-reader's delete of a note (notes:write missing)")

	// The same reader's restore is refused identically.
	resp = noteMutationRequestAs(t, srv, http.MethodPost, readerToken, "/"+noteID+"/restore", demo.DemoReaderUserID)
	assertPermissionDenied(t, resp, "demo-reader's restore of a note (notes:write missing)")
}
