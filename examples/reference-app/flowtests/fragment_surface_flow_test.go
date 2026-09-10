package flowtests

// fragment_surface_flow_test.go drives the case-domain fragment
// (internal/cases/api) and
// the smile-simulation fragment (internal/smilesim/api) -- through the
// real composed HTTP stack buildTestServer builds, in exactly the
// composition the case web UI will call: create a case naming an uploaded
// photo (cases fragment), run one async smile simulation over that photo
// (smilesim fragment's enqueue operation), observe it to success through
// the job-status operation, and read the finished simulation back from
// the per-photo enumeration operation -- the smile gallery's data
// source --
// with the case list and detail answered by the cases fragment along the
// way. Every leg goes through the handlers that implement the fragments'
// generated ServerInterfaces (internal/app/cases.go and
// internal/app/smilesim.go), so a request or response shape that drifted from the
// spec would fail here even where the compile-time assertions cannot
// see it (JSON-level drift is a runtime property).
//
// The tenant comes from the bearer token alone and the acting creator
// from the X-Demo-User-Id header, exactly as in cases_flow_test.go.

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/vislake/speed/examples/reference-app/internal/app/demo"
)

// TestFragmentSurface_CaseToSimulation_Journey is the end-to-end
// journey over the two fragments: one clinic
// staff member, one tenant, one photo, one case and one finished
// simulation, every step answered by a handler implementing a generated
// ServerInterface.
func TestFragmentSurface_CaseToSimulation_Journey(t *testing.T) {
	imgServer := newFakeOpenAIImageServer(t)
	srv, cfg := buildSmileSimTestServer(t, imgServer)

	token := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "fragment-surface")

	// A patient photo, uploaded and completed through storage's own real
	// HTTP surface -- the case's photo attachments reference such
	// already-uploaded objects, and the simulation job runs over this
	// same photo.
	photo := jpegWithExif(t)
	completedPhoto := uploadAndComplete(t, srv, token, photo, "")
	if completedPhoto.State != "completed" {
		t.Fatalf("photo state = %q, want completed", completedPhoto.State)
	}

	// Create a case naming that photo (cases fragment: cases_createCase),
	// read it back from the clinic-wide list (cases_listCases) and from
	// the detail route (cases_getCase) -- the three cases operations the
	// case web UI will call.
	created := createCaseAs(t, srv, token, demo.DemoOwnerUserID, caseCreateBody{
		PatientName:    "Fragment Journey Patient",
		PatientRef:     "JRN-001",
		PhotoObjectIDs: []string{completedPhoto.ID},
	})
	if created.ID == "" {
		t.Fatal("created case carries no id")
	}
	if len(created.Photos) != 1 || created.Photos[0].ObjectID != completedPhoto.ID {
		t.Fatalf("created case photos = %+v, want exactly the uploaded photo %q", created.Photos, completedPhoto.ID)
	}

	listResp := casesRequestAs(t, srv, http.MethodGet, casesPath, token, demo.DemoOwnerUserID, nil)
	listBody, err := io.ReadAll(listResp.Body)
	listResp.Body.Close()
	if err != nil {
		t.Fatalf("read cases list body: %v", err)
	}
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want %d; body = %s", casesPath, listResp.StatusCode, http.StatusOK, listBody)
	}
	var listOut struct {
		Cases []testCase `json:"cases"`
	}
	if err = json.Unmarshal(listBody, &listOut); err != nil {
		t.Fatalf("decode cases list: %v", err)
	}
	found := false
	for _, c := range listOut.Cases {
		if c.ID == created.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("created case %q missing from the caller's case list: %+v", created.ID, listOut.Cases)
	}

	detailResp := casesRequestAs(t, srv, http.MethodGet, caseDetailPathPrefix+created.ID, token, demo.DemoOwnerUserID, nil)
	defer detailResp.Body.Close()
	detail, detailBody := decodeCasesResponse(t, detailResp)
	if detailResp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want %d; body = %s", caseDetailPathPrefix+created.ID, detailResp.StatusCode, http.StatusOK, detailBody)
	}
	if len(detail.Photos) != 1 || detail.Photos[0].ObjectID != completedPhoto.ID {
		t.Fatalf("detail photos = %+v, want exactly the uploaded photo %q", detail.Photos, completedPhoto.ID)
	}

	// Enqueue one simulation over the case's photo with no explicit
	// options -- the web UI's first-run shape -- and poll the job-status
	// route until the job is terminal (smilesim fragment:
	// smilesim_simulate and smilesim_getJob).
	simulateBody, err := json.Marshal(map[string]string{"photo_object_id": completedPhoto.ID})
	if err != nil {
		t.Fatalf("marshal simulate request: %v", err)
	}
	final := smileSimulateBodyAndWait(t, srv, token, simulateBody, time.Now().Add(5*time.Second))
	if status, _ := final["status"].(string); status != "succeeded" {
		t.Fatalf("final job status = %v, want \"succeeded\"; body = %+v", final["status"], final)
	}
	outputObjectID, _ := final["output_object_id"].(string)
	if outputObjectID == "" {
		t.Fatalf("succeeded job carries no output_object_id: %+v", final)
	}
	// The job-status answer echoes the effective options the durable
	// per-photo record captured -- the documented defaults, since the
	// enqueue body named no options.
	assertOptionsIn(t, final, "natural", "natural", 1)

	// The per-photo enumeration -- the smile gallery's data source
	// (smilesim fragment: smilesim_listPhotoSimulations) -- lists exactly
	// the finished simulation under the case's photo.
	simulations := enumerateSimulations(t, srv, token, completedPhoto.ID)
	if len(simulations) != 1 {
		t.Fatalf("photo %q simulations = %d entries, want exactly the one enqueued above: %+v", completedPhoto.ID, len(simulations), simulations)
	}
	entry := simulations[0]
	if status, _ := entry["status"].(string); status != "succeeded" {
		t.Fatalf("enumeration entry status = %v, want \"succeeded\": %+v", entry["status"], entry)
	}
	if jobID, _ := entry["job_id"].(string); jobID == "" {
		t.Fatalf("enumeration entry carries no job_id: %+v", entry)
	}
	if got, _ := entry["photo_object_id"].(string); got != completedPhoto.ID {
		t.Fatalf("enumeration entry photo_object_id = %q, want %q: %+v", got, completedPhoto.ID, entry)
	}
	if got, _ := entry["output_object_id"].(string); got != outputObjectID {
		t.Fatalf("enumeration entry output_object_id = %q, want the job-status answer's %q: %+v", got, outputObjectID, entry)
	}
	assertOptionsIn(t, entry, "natural", "natural", 1)
	if createdAt, _ := entry["created_at"].(string); createdAt != "" {
		if _, err := time.Parse(time.RFC3339, createdAt); err != nil {
			t.Fatalf("enumeration entry created_at %q is not RFC3339: %v", createdAt, err)
		}
	} else {
		t.Fatalf("enumeration entry carries no created_at: %+v", entry)
	}

	// The fake vendor was genuinely reached: the routed vendor model id
	// proves the enqueue ran through the real gateway pipeline.
	if imgServer.lastModel != "dall-e-3" {
		t.Fatalf("fake server saw model = %q, want the routed vendor model %q", imgServer.lastModel, "dall-e-3")
	}
}
