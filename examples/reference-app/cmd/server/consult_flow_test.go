package main

// consult_flow_test.go drives go/ai-gateway end to end through the composed
// HTTP stack -- the authn+tenancy middleware chain, a real temp-file SQLite
// database (carrying both notes' and ai-gateway's real migrations), and the
// hand-written consult route (cmd/server/consult.go) -- against a fake
// OpenAI-compatible endpoint (fakeOpenAICompatibleServer below), standing in
// for the real vendor: no live API key is available or needed, since the
// OpenAI-compatible chat-completions schema is fully testable this way (see
// go/ai-gateway/AGENTS.md's own reference-app-consumer section).
//
// Three legs cover the round's acceptance shape:
//
//   - the happy path: a note created under one tenant's own token gets a
//     consultation suggestion back, and the fake server actually received
//     the OpenAI-compatible wire request this app's ai-gateway wiring was
//     supposed to send (the routed vendor model id, never the logical
//     "chat:default" key; the note's own text as the user message).
//   - an unknown note id is refused, never silently answered.
//   - a note belonging to ANOTHER tenant is refused exactly like an unknown
//     one -- proving the multi-tenant isolation notes' own Repository
//     already guarantees survives being read through this new, unrelated
//     module.
import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeOpenAICompatibleServer answers every POST /chat/completions with a
// fixed, non-streaming JSON reply carrying reply as the assistant's
// message -- the wire shape go/ai-gateway's OpenAICompatibleProvider parses
// (go/ai-gateway/openai_compatible.go's openaiChatResponseWire). It records
// the last request body it decoded, so a test can assert on exactly what
// this app's wiring sent.
type fakeOpenAICompatibleServer struct {
	*httptest.Server
	reply       string
	lastReqBody map[string]any
}

func newFakeOpenAICompatibleServer(t *testing.T, reply string) *fakeOpenAICompatibleServer {
	t.Helper()
	f := &fakeOpenAICompatibleServer{reply: reply}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "malformed request body", http.StatusBadRequest)
			return
		}
		f.lastReqBody = body

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message":       map[string]string{"role": "assistant", "content": f.reply},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]int{"prompt_tokens": 11, "completion_tokens": 9, "total_tokens": 20},
		})
	}))
	t.Cleanup(f.Close)
	return f
}

// buildConsultTestServer wires up buildServer's real output behind an
// httptest.Server, with the ai-gateway platform credential pointed at
// aiServer -- the same test-only-override pattern cfg.Mailer/cfg.SMSOutput
// already establish (server_test.go's buildTestServer, notification_flow_test.go's
// buildNotifTestServer).
func buildConsultTestServer(t *testing.T, aiServer *fakeOpenAICompatibleServer) (*httptest.Server, serverConfig) {
	t.Helper()

	cfg := testConfig(t)
	cfg.AIGatewayBaseURL = aiServer.URL
	cfg.AIGatewayAPIKey = "sk-test-consult-key"

	handler, cleanup, err := buildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, cfg
}

// consultSuggestRequest POSTs {"note_id": noteID} to srv's consult-suggest
// route, authenticated as token, and returns the raw *http.Response for the
// caller to assert on (status code and body both vary across this file's
// tests).
func consultSuggestRequest(t *testing.T, srv *httptest.Server, token, noteID string) *http.Response {
	t.Helper()

	body, err := json.Marshal(map[string]string{"note_id": noteID})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+consultSuggestPath, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", consultSuggestPath, err)
	}
	return resp
}

// TestConsultSuggest_ReturnsGatewaySuggestion is this round's mandatory
// end-to-end proof: a note's text really travels through
// internal/consult.Service, through go/ai-gateway's Gateway.Chat pipeline
// (credential resolution, model routing, the OpenAICompatibleProvider's
// real HTTP call), to a fake OpenAI-compatible server and back, with the
// server's own scripted reply landing verbatim in the HTTP response this
// app's consult route answers.
func TestConsultSuggest_ReturnsGatewaySuggestion(t *testing.T) {
	const wantSuggestion = "Schedule a follow-up appointment in two weeks."
	aiServer := newFakeOpenAICompatibleServer(t, wantSuggestion)
	srv, cfg := buildConsultTestServer(t, aiServer)

	token := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "consult-owner")
	const noteText = "Patient reports mild sensitivity after a whitening session."
	noteID := createNoteAs(t, srv, token, noteText)

	resp := consultSuggestRequest(t, srv, token, noteID)
	defer resp.Body.Close()

	var out struct {
		Suggestion string `json:"suggestion"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s status = %d, want %d; body = %+v", consultSuggestPath, resp.StatusCode, http.StatusOK, out)
	}
	if out.Suggestion != wantSuggestion {
		t.Fatalf("suggestion = %q, want the fake server's scripted reply %q", out.Suggestion, wantSuggestion)
	}

	// The fake server's own recorded request is what proves this app's
	// wiring (aigateway.WithModelRoute("chat:default", ...) in server.go)
	// actually ran, rather than the test somehow short-circuiting the
	// gateway: the vendor model id is the routed "gpt-4o-mini", never the
	// logical "chat:default" key business code asked for, and the note's
	// own text appears as a user-role message.
	if aiServer.lastReqBody == nil {
		t.Fatal("the fake OpenAI-compatible server never received a request")
	}
	if got, _ := aiServer.lastReqBody["model"].(string); got != "gpt-4o-mini" {
		t.Fatalf("fake server saw model = %q, want the routed vendor model \"gpt-4o-mini\", never the logical key", got)
	}
	messages, _ := aiServer.lastReqBody["messages"].([]any)
	if len(messages) == 0 {
		t.Fatal("fake server saw no messages")
	}
	last, _ := messages[len(messages)-1].(map[string]any)
	if got, _ := last["content"].(string); got != noteText {
		t.Fatalf("fake server's last message content = %q, want the note's own text %q", got, noteText)
	}
}

// TestConsultSuggest_UnknownNoteID_Refused proves an unknown note id is
// refused rather than silently answered (or, worse, passed through to the
// vendor as an empty prompt).
func TestConsultSuggest_UnknownNoteID_Refused(t *testing.T) {
	aiServer := newFakeOpenAICompatibleServer(t, "unused")
	srv, cfg := buildConsultTestServer(t, aiServer)

	token := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "consult-owner-2")

	resp := consultSuggestRequest(t, srv, token, "does-not-exist")
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Fatalf("POST %s with an unknown note id status = %d, want a refusal", consultSuggestPath, resp.StatusCode)
	}
	if aiServer.lastReqBody != nil {
		t.Fatal("the fake OpenAI-compatible server received a request for an unknown note id, want none")
	}
}

// TestConsultSuggest_NoteFromAnotherTenant_Refused proves a note created
// under one tenant cannot be summarized through a different tenant's token
// -- the same isolation notes' own HTTP surface (GET /api/v1/notes)
// already guarantees, now exercised through this unrelated module's own
// read path.
func TestConsultSuggest_NoteFromAnotherTenant_Refused(t *testing.T) {
	aiServer := newFakeOpenAICompatibleServer(t, "unused")
	srv, cfg := buildConsultTestServer(t, aiServer)

	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "consult-acme-owner")
	noteID := createNoteAs(t, srv, acmeToken, "Acme-only clinical note.")

	globexToken := registerAndAuthenticate(t, srv, cfg, "tenant-globex", "consult-globex-owner")
	resp := consultSuggestRequest(t, srv, globexToken, noteID)
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Fatalf("POST %s for another tenant's note status = %d, want a refusal", consultSuggestPath, resp.StatusCode)
	}
	if aiServer.lastReqBody != nil {
		t.Fatal("the fake OpenAI-compatible server received a request for another tenant's note, want none")
	}
}
