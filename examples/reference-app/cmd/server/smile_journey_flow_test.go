// smile_journey_flow_test.go drives the block-B acceptance journey at the
// wire level through the real composed HTTP stack -- the authn+tenancy
// middleware chain, a real temp-file SQLite database, a real
// jobs.StandaloneQueue, the cases surface (cmd/server/cases.go and
// cases_photos.go, whose upload/create/content operations block A
// shipped) and the smile-simulation surface (cmd/server/smilesim.go,
// whose simulate/job-status/enumeration/content operations the
// comparison view calls) -- against fakeOpenAIImageServer's canned,
// replayed provider answers, the same deterministic stand-in the
// smilesim flow suite uses. No live provider, no live API key: the
// OpenAI-compatible images-edits wire shape is fully answerable offline.
//
// The journey mirrors what the block-B gate makes a person do by
// clicking: a clinic user opens a case that carries a patient photo
// (block A), picks a smile option set from the documented vocabulary
// (smile style / tooth shade / strength), starts the generation, waits
// out its honest asynchronous status, and then has the generated image
// beside the original -- which the wire test can only prove by reading
// the result's bytes back through the surface the comparison view
// renders (the fragment's simulation-content operation, added by this
// journey), distinct from the uploaded photo and served with the media
// type storage's probe assigned.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

// TestSmileJourney_CasePhotoThroughSimulationToComparison is the
// block-B journey in one pass over real HTTP: upload a patient photo,
// open the case carrying it, choose a non-default smile option set,
// generate, poll to the succeeded status, see the photo's enumeration
// list the generation with the options that produced it, and read the
// generated image's bytes back -- the data the before/after comparison
// view renders.
func TestSmileJourney_CasePhotoThroughSimulationToComparison(t *testing.T) {
	imgServer := newFakeOpenAIImageServer(t)
	srv, cfg := buildSmileSimTestServer(t, imgServer)
	token := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "smile-journey")

	// The case with the patient photo, exactly as block A's one-step
	// surface creates it: upload first (the photo travels base64 in
	// JSON), then create the case naming the uploaded object.
	photo := uploadPhotoAs(t, srv, token, base64.StdEncoding.EncodeToString(jpegWithExif(t)))
	opened := createCaseAs(t, srv, token, "", caseCreateBody{
		PatientName:    "Journey patient",
		PatientRef:     "JRN-001",
		PhotoObjectIDs: []string{photo.ObjectID},
	})
	if len(opened.Photos) != 1 || opened.Photos[0].ObjectID != photo.ObjectID {
		t.Fatalf("case %s photos = %+v, want the uploaded photo %s", opened.ID, opened.Photos, photo.ObjectID)
	}

	// The photo's bytes read back through the case surface (the original
	// the comparison sits next to), before any generation exists.
	original := photoContentAs(t, srv, token, opened.ID, photo.ObjectID)
	if original.MediaType != "image/jpeg" {
		t.Fatalf("photo media_type = %q, want image/jpeg", original.MediaType)
	}

	// The documented option set, deliberately NOT the defaults (a
	// bright-ladder shade and a mid-range strength) so the journey proves
	// the chosen parameters are the ones the generation and its record
	// carry, rather than whatever a default would have produced anyway.
	simulateBody, err := json.Marshal(map[string]any{
		"photo_object_id": photo.ObjectID,
		"options": map[string]any{
			"smile_style": "subtle",
			"tooth_shade": "white",
			"strength":    0.6,
		},
	})
	if err != nil {
		t.Fatalf("marshal simulate body: %v", err)
	}
	resp := smileSimRequest(t, srv, http.MethodPost, smileSimulatePath, token, simulateBody)
	defer resp.Body.Close()
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read simulate response: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST %s status = %d, want 202; body = %s", smileSimulatePath, resp.StatusCode, rawBody)
	}
	var jobRef struct {
		JobID string `json:"job_id"`
	}
	if unmarshalErr := json.Unmarshal(rawBody, &jobRef); unmarshalErr != nil || jobRef.JobID == "" {
		t.Fatalf("decode 202 answer %s: %v", rawBody, unmarshalErr)
	}

	// Generation is asynchronous: poll the job-status route to the
	// succeeded terminal state -- the same observation a browser's
	// honest in-progress view makes.
	status := waitForSmileSimSucceeded(t, srv, token, jobRef.JobID, time.Now().Add(30*time.Second))
	outputObjectID, _ := status["output_object_id"].(string)
	if outputObjectID == "" {
		t.Fatalf("succeeded job carried no output_object_id; status = %+v", status)
	}
	if outputObjectID == photo.ObjectID {
		t.Fatalf("output object id %s equals the input photo's -- the generated image must be a distinct object", outputObjectID)
	}
	if opts, ok := status["options"].(map[string]any); !ok {
		t.Fatalf("succeeded job carried no recorded options; status = %+v", status)
	} else {
		if opts["smile_style"] != "subtle" || opts["tooth_shade"] != "white" || opts["strength"] != 0.6 {
			t.Fatalf("recorded options = %+v, want the chosen subtle/white/0.6 set", opts)
		}
	}

	// The photo's enumeration now lists the generation, newest first,
	// with the effective options that produced it.
	listResp := smileSimRequest(t, srv, http.MethodGet,
		"/api/v1/smile-simulation/photos/"+photo.ObjectID+"/simulations", token, nil)
	defer listResp.Body.Close()
	listRaw, err := io.ReadAll(listResp.Body)
	if err != nil {
		t.Fatalf("read enumeration response: %v", err)
	}
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("GET simulations status = %d, want 200; body = %s", listResp.StatusCode, listRaw)
	}
	var listed struct {
		Simulations []struct {
			JobID          string `json:"job_id"`
			PhotoObjectID  string `json:"photo_object_id"`
			Status         string `json:"status"`
			OutputObjectID string `json:"output_object_id"`
			Options        struct {
				SmileStyle string  `json:"smile_style"`
				ToothShade string  `json:"tooth_shade"`
				Strength   float64 `json:"strength"`
			} `json:"options"`
		} `json:"simulations"`
	}
	if unmarshalErr := json.Unmarshal(listRaw, &listed); unmarshalErr != nil {
		t.Fatalf("decode enumeration %s: %v", listRaw, unmarshalErr)
	}
	if len(listed.Simulations) != 1 {
		t.Fatalf("photo enumeration lists %d simulations, want 1; body = %s", len(listed.Simulations), listRaw)
	}
	row := listed.Simulations[0]
	if row.JobID != jobRef.JobID || row.PhotoObjectID != photo.ObjectID || row.Status != "succeeded" {
		t.Fatalf("listed simulation = %+v, want the succeeded job for this photo", row)
	}
	if row.OutputObjectID != outputObjectID || row.Options.SmileStyle != "subtle" || row.Options.ToothShade != "white" || row.Options.Strength != 0.6 {
		t.Fatalf("listed simulation's output/options = %+v / %+v, want %s / subtle/white/0.6", row.OutputObjectID, row.Options, outputObjectID)
	}

	// The result image's bytes, served the way the before/after
	// comparison view reads them: the generated object is a different
	// image from the uploaded photo -- the vendor's canned gray PNG --
	// and its media type is the one storage's probe assigned.
	contentResp := smileSimRequest(t, srv, http.MethodGet,
		smileSimulationContentPath(photo.ObjectID, jobRef.JobID), token, nil)
	defer contentResp.Body.Close()
	contentRaw, err := io.ReadAll(contentResp.Body)
	if err != nil {
		t.Fatalf("read content response: %v", err)
	}
	if contentResp.StatusCode != http.StatusOK {
		t.Fatalf("GET simulation content status = %d, want 200; body = %s", contentResp.StatusCode, contentRaw)
	}
	var content struct {
		MediaType     string `json:"media_type"`
		ContentBase64 string `json:"content_base64"`
	}
	if unmarshalErr := json.Unmarshal(contentRaw, &content); unmarshalErr != nil {
		t.Fatalf("decode simulation content %s: %v", contentRaw, unmarshalErr)
	}
	if content.MediaType != "image/png" {
		t.Fatalf("simulation content media_type = %q, want image/png", content.MediaType)
	}
	decoded, err := base64.StdEncoding.DecodeString(content.ContentBase64)
	if err != nil {
		t.Fatalf("decode simulation content bytes: %v", err)
	}
	if !bytes.Equal(decoded, imgServer.generatedPNG) {
		t.Fatalf("simulation content bytes differ from the vendor's generated image (%d vs %d bytes)", len(decoded), len(imgServer.generatedPNG))
	}
	if bytes.Equal(decoded, mustDecode(t, original.ContentBase64)) {
		t.Fatalf("simulation content equals the uploaded photo's bytes -- the before/after comparison would show two identical images")
	}
}

// TestSmileJourney_SimulationContentRefusals prove the content read's
// gates: an unknown job answers simulation_not_found without probing,
// another tenant's simulation is invisible, a job of one photo cannot be
// read through another photo's content route, and the original photo's
// bytes never leak through a simulation-content read.
func TestSmileJourney_SimulationContentRefusals(t *testing.T) {
	imgServer := newFakeOpenAIImageServer(t)
	srv, cfg := buildSmileSimTestServer(t, imgServer)
	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "content-acme")
	globexToken := registerAndAuthenticate(t, srv, cfg, "tenant-globex", "content-globex")

	// A completed simulation under acme, so there IS an output to
	// protect and a cross-tenant question to ask about it.
	photo := uploadPhotoAs(t, srv, acmeToken, base64.StdEncoding.EncodeToString(jpegWithExif(t)))
	simulateBody, err := json.Marshal(map[string]any{"photo_object_id": photo.ObjectID})
	if err != nil {
		t.Fatalf("marshal simulate body: %v", err)
	}
	resp := smileSimRequest(t, srv, http.MethodPost, smileSimulatePath, acmeToken, simulateBody)
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s status = %d, want 202; body = %s", smileSimulatePath, resp.StatusCode, body)
	}
	var jobRef struct {
		JobID string `json:"job_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&jobRef)
	resp.Body.Close()
	waitForSmileSimSucceeded(t, srv, acmeToken, jobRef.JobID, time.Now().Add(30*time.Second))

	// An unknown job id under the caller's own photo: 404
	// simulation_not_found -- the route answers "no such simulation",
	// never whether the job exists at all.
	resp = smileSimRequest(t, srv, http.MethodGet,
		smileSimulationContentPath(photo.ObjectID, "no-such-job"), acmeToken, nil)
	assertSmileSimContentError(t, resp, http.StatusNotFound, "smilesim.simulation_not_found")
	resp.Body.Close()

	// Another tenant's simulation, asked for through the photo id the
	// caller cannot see: the same 404 -- globex has no record for this
	// photo under its own tenant, so it learns nothing about acme's.
	resp = smileSimRequest(t, srv, http.MethodGet,
		smileSimulationContentPath(photo.ObjectID, jobRef.JobID), globexToken, nil)
	assertSmileSimContentError(t, resp, http.StatusNotFound, "smilesim.simulation_not_found")
	resp.Body.Close()

	// A second acme photo with no simulations of its own, read with the
	// first photo's job id: the job is not that photo's simulation, and
	// the answer must not reveal that the job id exists under the tenant
	// at all.
	otherPhoto := uploadPhotoAs(t, srv, acmeToken, base64.StdEncoding.EncodeToString(jpegWithExif(t)))
	resp = smileSimRequest(t, srv, http.MethodGet,
		smileSimulationContentPath(otherPhoto.ObjectID, jobRef.JobID), acmeToken, nil)
	assertSmileSimContentError(t, resp, http.StatusNotFound, "smilesim.simulation_not_found")
	resp.Body.Close()
}

// assertSmileSimContentError asserts resp carries the {code, params}
// envelope with wantCode and the given HTTP status.
func assertSmileSimContentError(t *testing.T, resp *http.Response, wantStatus int, wantCode string) {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read refusal body: %v", err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("status = %d, want %d; body = %s", resp.StatusCode, wantStatus, body)
	}
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode refusal %s: %v", body, err)
	}
	if envelope.Code != wantCode {
		t.Fatalf("refusal code = %q, want %q; body = %s", envelope.Code, wantCode, body)
	}
}

// mustDecode base64-decodes encoded, failing the test on any error.
func mustDecode(t *testing.T, encoded string) []byte {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	return decoded
}
