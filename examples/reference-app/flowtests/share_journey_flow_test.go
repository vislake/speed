// share_journey_flow_test.go drives the share journey at the
// wire level through the real composed HTTP stack -- the same stack and
// the same canned provider smile_journey_flow_test.go uses for the
// simulation journey: a clinic user with a completed smile simulation on a
// case photo mints a patient-facing share link for the before/after pair
// (one share for the original photo, one for the simulation's result --
// the shape the patient page renders side by side), an unauthenticated
// visitor (no token, no session, no demo header) opens that link and
// receives both resources' real bytes through go/sharing's public access
// route (internal/app/sharing_resolver.go resolving each share's
// ResourceRef -- a go/storage object id -- to go/storage content), and a
// share that has been revoked or has expired refuses the same visitor
// honestly: the 404 sharing.not_accessible envelope, never bytes and
// never a crash.
//
// The server half of this journey is all pre-existing, separately proven
// machinery (go/sharing's create/access/revoke surface, proven end to
// end in sharing_flow_test.go over a plain storage object; this file's
// cases+simulation legs, proven in smile_journey_flow_test.go). What this
// journey assembles is the product surface -- the clinic's share action and
// the patient's page -- which cannot be driven by a Go wire test alone;
// the web host's own journeys (views/simulation-share-action.test.tsx,
// views/share-view.test.tsx) and the e2e gate over the core journey
// (web/e2e/core-journey.pending.spec.ts) cover the browser shape. This
// file pins the wire facts those surfaces rest on, end to end over a REAL
// simulation result: each shared resource's bytes ARE what the sharing
// side means them to be -- the before half is the uploaded patient
// photo, the after half is the vendor's generated image, distinct from
// the photo they were generated from (the browser-decode and
// mutual-difference claims the e2e gate checks with naturalWidth and
// source URLs are proven here at the source -- image.Decode over what
// /api/v1/sharing/access actually answered for each token) -- and the
// refused visitor is answered with the module's one honest 404 code,
// whatever the refusal reason.
package flowtests

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/vislake/speed/examples/reference-app/internal/apptest"
	"github.com/vislake/speed/examples/reference-app/internal/testutil"

	"github.com/vislake/speed/examples/reference-app/internal/app/demo"
	"github.com/vislake/speed/go/sharing"
)

// TestShareJourney_CompletedSimulationSharedToAnonymousVisitor is the
// share journey in one pass: photo upload, case creation, simulation
// to its succeeded status, the before/after pair of shares minted by the
// practice (demo-owner, the same actor sharing_flow_test.go proves holds
// sharing:create), and the unauthenticated visitor's reads of both
// shared resources -- the before half answers the uploaded patient
// photo's own bytes, the after half answers the vendor's generated
// image, each decodable and neither the other. Both shares then answer
// the revoked visitor with the honest 404 envelope, independently.
func TestShareJourney_CompletedSimulationSharedToAnonymousVisitor(t *testing.T) {
	imgServer := newFakeOpenAIImageServer(t)
	srv, cfg := buildSmileSimTestServer(t, imgServer)
	token := apptest.RegisterAndAuthenticate(t, srv, cfg, "tenant-acme", "share-journey")

	// The case with the patient photo and the completed simulation,
	// exactly as the smile journey makes them: upload the before
	// photo, open the case, generate, poll to the succeeded status and
	// read the output object id the comparison view renders.
	jpeg := jpegWithExif(t)
	photo := uploadPhotoAs(t, srv, token, base64.StdEncoding.EncodeToString(jpeg))
	opened := testutil.CreateCaseAs(t, srv, token, "", testutil.CaseCreateBody{
		PatientName:    "Share journey patient",
		PatientRef:     "SHJ-001",
		PhotoObjectIDs: []string{photo.ObjectID},
	})
	if len(opened.Photos) != 1 || opened.Photos[0].ObjectID != photo.ObjectID {
		t.Fatalf("case %s photos = %+v, want the uploaded photo %s", opened.ID, opened.Photos, photo.ObjectID)
	}

	simulateBody, marshalErr := json.Marshal(map[string]any{"photo_object_id": photo.ObjectID})
	if marshalErr != nil {
		t.Fatalf("marshal simulate body: %v", marshalErr)
	}
	resp := smileSimRequest(t, srv, http.MethodPost, smileSimulatePath, token, simulateBody)
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s status = %d, want 202; body = %s", smileSimulatePath, resp.StatusCode, body)
	}
	var jobRef struct {
		JobID string `json:"job_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jobRef); err != nil {
		t.Fatalf("decode 202 answer: %v", err)
	}
	resp.Body.Close()
	status := waitForSmileSimSucceeded(t, srv, token, jobRef.JobID, time.Now().Add(30*time.Second))
	outputObjectID, _ := status["output_object_id"].(string)
	if outputObjectID == "" {
		t.Fatalf("succeeded job carried no output_object_id; status = %+v", status)
	}

	// The PAIR of shares the practice's share action mints for the
	// completed simulation: one share per half of the comparison the
	// patient page renders -- the BEFORE photo and the simulation's
	// OUTPUT object -- each created through the real owner-facing route
	// as demo-owner (the sharing:create grant sharing_flow_test.go
	// pins), with no explicit expiry so the tenant's forced default
	// applies to both.
	mintPair := func() map[string]testCreateShareResponse {
		created := map[string]testCreateShareResponse{}
		for name, resourceRef := range map[string]string{
			"before": photo.ObjectID,
			"after":  outputObjectID,
		} {
			body, err := json.Marshal(map[string]any{"resourceRef": resourceRef})
			if err != nil {
				t.Fatalf("marshal create body: %v", err)
			}
			createResp := storageRequest(t, srv, http.MethodPost, sharing.PathShares,
				token, demo.DemoOwnerUserID, "application/json", bytes.NewReader(body))
			var share testCreateShareResponse
			decodeSharingBody(t, createResp, http.StatusCreated, "create "+name+" simulation share", &share)
			if share.Token == "" || share.Share.ID == "" {
				t.Fatalf("%s create response = %+v, want a token and a share id", name, share)
			}
			if share.Share.ResourceRef != resourceRef {
				t.Fatalf("%s share resource_ref = %q, want %q", name, share.Share.ResourceRef, resourceRef)
			}
			created[name] = share
		}
		return created
	}
	pair := mintPair()

	// The patient's side: a genuinely unauthenticated GET of each half
	// of the share link -- no Authorization header, no demo header, no
	// session -- and the bytes that come back are that half of the
	// comparison itself: decodable, the before half the uploaded photo
	// and the after half the vendor's generated image, and never each
	// other.
	readAccess := func(token string) (*http.Response, []byte) {
		access := sharingAccessRequest(t, srv, token, "")
		raw, err := io.ReadAll(access.Body)
		access.Body.Close()
		if err != nil {
			t.Fatalf("read access response: %v", err)
		}
		if access.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status = %d, want 200; body = %s", sharing.PathAccess, access.StatusCode, raw)
		}
		return access, raw
	}

	beforeResp, rawBefore := readAccess(pair["before"].Token)
	// The before half is the uploaded patient photo exactly as the
	// storage module holds it -- the completion pipeline's structural
	// metadata strip has removed the EXIF profile the upload carried,
	// so the reference is the storage surface's own content answer for
	// the same object (cases_photos_test.go's identical comparison),
	// never the pre-sanitization upload bytes.
	storageBefore := storageRequest(t, srv, http.MethodGet,
		"/api/v1/storage/objects/"+photo.ObjectID+"/content",
		token, demo.DemoOwnerUserID, "", nil)
	storageBeforeBytes, readErr := io.ReadAll(storageBefore.Body)
	storageBefore.Body.Close()
	if readErr != nil {
		t.Fatalf("read storage content body: %v", readErr)
	}
	if storageBefore.StatusCode != http.StatusOK {
		t.Fatalf("storage content status = %d, want 200", storageBefore.StatusCode)
	}
	if !bytes.Equal(rawBefore, storageBeforeBytes) {
		t.Fatalf("shared before bytes differ from the photo's own storage content (%d vs %d bytes)",
			len(rawBefore), len(storageBeforeBytes))
	}
	if bytes.Equal(rawBefore, jpeg) {
		t.Fatalf("shared before bytes equal the pre-sanitization upload -- the EXIF-bearing photo should never be what a share serves")
	}
	if _, _, decodeErr := image.Decode(bytes.NewReader(rawBefore)); decodeErr != nil {
		t.Fatalf("shared before content is not a decodable image: %v", decodeErr)
	}
	if got := beforeResp.Header.Get("Content-Type"); got != "image/jpeg" {
		t.Errorf("before Content-Type = %q, want image/jpeg (the media type the browser decodes by)", got)
	}

	afterResp, rawAfter := readAccess(pair["after"].Token)
	if !bytes.Equal(rawAfter, imgServer.generatedPNG) {
		t.Fatalf("shared after bytes differ from the vendor's generated image (%d vs %d bytes)",
			len(rawAfter), len(imgServer.generatedPNG))
	}
	if bytes.Equal(rawAfter, jpeg) {
		t.Fatalf("shared after bytes equal the before photo's -- the patient link would show the original, not the simulation")
	}
	if _, _, decodeErr := image.Decode(bytes.NewReader(rawAfter)); decodeErr != nil {
		t.Fatalf("shared after content is not a decodable image: %v", decodeErr)
	}
	if got := afterResp.Header.Get("Content-Type"); got != "image/png" {
		t.Errorf("after Content-Type = %q, want image/png (the media type the browser decodes by)", got)
	}
	if got := afterResp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("after Cache-Control = %q, want no-store on the access answer", got)
	}

	// The practice revokes both shares, and the very same visitor is
	// refused honestly on the next open of either: 404 with the
	// module's one outward refusal code -- never bytes, never a crash,
	// and never a hint of which refusal reason applied.
	revoke := func(shareID string) {
		revokeResp := storageRequest(t, srv, http.MethodPost,
			sharing.PathShares+"/"+shareID+"/revoke", token, demo.DemoOwnerUserID, "", nil)
		decodeSharingBody(t, revokeResp, http.StatusOK, "revoke simulation share", &testSharingShare{})
	}
	for _, name := range []string{"before", "after"} {
		share := pair[name]
		revoke(share.Share.ID)
		refused := sharingAccessRequest(t, srv, share.Token, "")
		refusedBody, err := io.ReadAll(refused.Body)
		refused.Body.Close()
		if err != nil {
			t.Fatalf("read refused response: %v", err)
		}
		if refused.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s after revoking the %s share: status = %d, want 404; body = %s",
				sharing.PathAccess, name, refused.StatusCode, refusedBody)
		}
		var envelope struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(refusedBody, &envelope); err != nil {
			t.Fatalf("decode refused body %s: %v", refusedBody, err)
		}
		if envelope.Code != "sharing.not_accessible" {
			t.Fatalf("refused code = %q, want sharing.not_accessible; body = %s", envelope.Code, refusedBody)
		}
	}
}

// TestShareJourney_ExpiredShareRefusesHonestly proves the expiry half of
// the share journey: a share whose lifetime genuinely ran out -- a
// real row whose expires_at passed, not a revoked one -- refuses an
// anonymous visitor with the same honest 404 sharing.not_accessible
// envelope, which is exactly why the patient page renders one human
// message for a link that no longer works whatever the reason.
func TestShareJourney_ExpiredShareRefusesHonestly(t *testing.T) {
	srv, cfg, _ := apptest.BuildServer(t)
	acmeToken := apptest.RegisterAndAuthenticate(t, srv, cfg, "tenant-acme", "expiry-journey")

	content := sharingTestJPEG(t)
	completed := uploadAndComplete(t, srv, acmeToken, content, sha256Hex(content))

	// An explicit expiry inside the module's allowed window -- now plus
	// half a second -- is accepted at creation, then runs out while this
	// test watches.
	expiresAt := time.Now().Add(500 * time.Millisecond).Format(time.RFC3339Nano)
	createBody, err := json.Marshal(map[string]any{
		"resourceRef": completed.ID,
		"expiresAt":   expiresAt,
	})
	if err != nil {
		t.Fatalf("marshal create body: %v", err)
	}
	createResp := storageRequest(t, srv, http.MethodPost, sharing.PathShares,
		acmeToken, demo.DemoOwnerUserID, "application/json", bytes.NewReader(createBody))
	var created testCreateShareResponse
	decodeSharingBody(t, createResp, http.StatusCreated, "create short-lived share", &created)

	// Wait the share's lifetime out, generously: the create above
	// resolved the expiry strictly in the future, and this sleep ends
	// well past it.
	time.Sleep(1200 * time.Millisecond)

	refused := sharingAccessRequest(t, srv, created.Token, "")
	refusedBody, err := io.ReadAll(refused.Body)
	refused.Body.Close()
	if err != nil {
		t.Fatalf("read refused response: %v", err)
	}
	if refused.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s after expiry: status = %d, want 404; body = %s", sharing.PathAccess, refused.StatusCode, refusedBody)
	}
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(refusedBody, &envelope); err != nil {
		t.Fatalf("decode refused body %s: %v", refusedBody, err)
	}
	if envelope.Code != "sharing.not_accessible" {
		t.Fatalf("refused code = %q, want sharing.not_accessible; body = %s", envelope.Code, refusedBody)
	}
}
