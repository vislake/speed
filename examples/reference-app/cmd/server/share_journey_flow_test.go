// share_journey_flow_test.go drives the block-C acceptance journey at the
// wire level through the real composed HTTP stack -- the same stack and
// the same canned provider smile_journey_flow_test.go uses for the
// block-B journey: a clinic user with a completed smile simulation on a
// case photo mints a patient-facing share link for the simulation's
// result, an unauthenticated visitor (no token, no session, no demo
// header) opens that link and receives the generated image's real bytes
// through go/sharing's public access route (cmd/server/sharing_resolver.go
// resolving the share's ResourceRef -- the simulation output object -- to
// go/storage content), and a share that has been revoked or has expired
// refuses the same visitor honestly: the 404 sharing.not_accessible
// envelope, never bytes and never a crash.
//
// The server half of this journey is all pre-existing, separately proven
// machinery (go/sharing's create/access/revoke surface, proven end to
// end in sharing_flow_test.go over a plain storage object; this file's
// cases+simulation legs, proven in smile_journey_flow_test.go). What this
// round assembles is the product surface -- the clinic's share action and
// the patient's page -- which cannot be driven by a Go wire test alone;
// the web host's own journeys (photo-simulation-panel.test.tsx's share
// leg, views/share-view.test.tsx) and the block-C e2e gate
// (web/e2e/core-journey.pending.spec.ts) cover the browser shape. This
// file pins the wire facts those surfaces rest on, end to end over a REAL
// simulation result: the shared bytes ARE the generated image (the
// browser-decode claim the e2e gate checks with naturalWidth is proven
// here at the source -- image.Decode over what /api/v1/sharing/access
// actually answered), distinct from the uploaded before-photo, and the
// refused visitor is answered with the module's one honest 404 code,
// whatever the refusal reason.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/vislake/speed/go/sharing"
)

// TestShareJourney_CompletedSimulationSharedToAnonymousVisitor is block
// C's wire journey in one pass: photo upload, case creation, simulation
// to its succeeded status, the share minted by the practice (demo-owner,
// the same actor sharing_flow_test.go proves holds sharing:create), and
// the unauthenticated visitor's read of the shared result -- genuine
// image bytes that decode, that equal the vendor's generated output, and
// that differ from the patient photo they were generated from. The same
// share then answers the revoked visitor with the honest 404 envelope.
func TestShareJourney_CompletedSimulationSharedToAnonymousVisitor(t *testing.T) {
	imgServer := newFakeOpenAIImageServer(t)
	srv, cfg := buildSmileSimTestServer(t, imgServer)
	token := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "share-journey")

	// The case with the patient photo and the completed simulation,
	// exactly as the block-B journey makes them: upload the before
	// photo, open the case, generate, poll to the succeeded status and
	// read the output object id the comparison view renders.
	jpeg := jpegWithExif(t)
	photo := uploadPhotoAs(t, srv, token, base64.StdEncoding.EncodeToString(jpeg))
	opened := createCaseAs(t, srv, token, "", caseCreateBody{
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

	// The share the practice mints for the completed simulation: the
	// ResourceRef is the simulation's OUTPUT object -- the "after" the
	// patient is meant to see -- created through the real owner-facing
	// route as demo-owner (the sharing:create grant sharing_flow_test.go
	// pins), with no explicit expiry so the tenant's forced default
	// applies.
	createBody, err := json.Marshal(map[string]any{"resourceRef": outputObjectID})
	if err != nil {
		t.Fatalf("marshal create body: %v", err)
	}
	createResp := storageRequest(t, srv, http.MethodPost, sharing.PathShares,
		token, demoOwnerUserID, "application/json", bytes.NewReader(createBody))
	var created testCreateShareResponse
	decodeSharingBody(t, createResp, http.StatusCreated, "create simulation share", &created)
	if created.Token == "" || created.Share.ID == "" {
		t.Fatalf("create response = %+v, want a token and a share id", created)
	}
	if created.Share.ResourceRef != outputObjectID {
		t.Fatalf("created share resource_ref = %q, want the simulation output object %q",
			created.Share.ResourceRef, outputObjectID)
	}

	// The patient's side: a genuinely unauthenticated GET of the share
	// link -- no Authorization header, no demo header, no session -- and
	// the bytes that come back are the simulation itself: decodable, the
	// vendor's generated image, and not the before photo.
	access := sharingAccessRequest(t, srv, created.Token, "")
	raw, err := io.ReadAll(access.Body)
	access.Body.Close()
	if err != nil {
		t.Fatalf("read access response: %v", err)
	}
	if access.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200; body = %s", sharing.PathAccess, access.StatusCode, raw)
	}
	if !bytes.Equal(raw, imgServer.generatedPNG) {
		t.Fatalf("shared bytes differ from the vendor's generated image (%d vs %d bytes)", len(raw), len(imgServer.generatedPNG))
	}
	if bytes.Equal(raw, jpeg) {
		t.Fatalf("shared bytes equal the before photo's -- the patient link would show the original, not the simulation")
	}
	if _, _, decodeErr := image.Decode(bytes.NewReader(raw)); decodeErr != nil {
		t.Fatalf("shared content is not a decodable image: %v", decodeErr)
	}
	if got := access.Header.Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png (the media type the browser decodes by)", got)
	}
	if got := access.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on the access answer", got)
	}

	// The practice revokes the share, and the very same visitor is
	// refused honestly on the next open: 404 with the module's one
	// outward refusal code -- never bytes, never a crash, and never a
	// hint of which refusal reason applied.
	revokeResp := storageRequest(t, srv, http.MethodPost,
		sharing.PathShares+"/"+created.Share.ID+"/revoke", token, demoOwnerUserID, "", nil)
	decodeSharingBody(t, revokeResp, http.StatusOK, "revoke simulation share", &testSharingShare{})
	refused := sharingAccessRequest(t, srv, created.Token, "")
	refusedBody, err := io.ReadAll(refused.Body)
	refused.Body.Close()
	if err != nil {
		t.Fatalf("read refused response: %v", err)
	}
	if refused.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s after revoke: status = %d, want 404; body = %s", sharing.PathAccess, refused.StatusCode, refusedBody)
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

// TestShareJourney_ExpiredShareRefusesHonestly proves the expiry half of
// block C regression (b): a share whose lifetime genuinely ran out -- a
// real row whose expires_at passed, not a revoked one -- refuses an
// anonymous visitor with the same honest 404 sharing.not_accessible
// envelope, which is exactly why the patient page renders one human
// message for a link that no longer works whatever the reason.
func TestShareJourney_ExpiredShareRefusesHonestly(t *testing.T) {
	srv, cfg, _ := buildTestServer(t)
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "expiry-journey")

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
		acmeToken, demoOwnerUserID, "application/json", bytes.NewReader(createBody))
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
