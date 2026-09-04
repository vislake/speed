package aigateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	obs "github.com/vislake/speed/go/observability"
)

// --- Chat (non-streaming) --------------------------------------------------

func TestOpenAICompatibleProvider_Chat_Success(t *testing.T) {
	var gotAuth, gotModel string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		gotModel, _ = gotBody["model"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message":       map[string]string{"role": "assistant", "content": "hello there"},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7},
		})
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProvider(srv.URL, "sk-test")
	resp, err := p.Chat(context.Background(), ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}},
		Params:   map[string]any{"temperature": 0.2},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Message.Role != RoleAssistant || resp.Message.Content != "hello there" {
		t.Fatalf("Chat message = %+v", resp.Message)
	}
	if resp.Usage != (Usage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7}) {
		t.Fatalf("Chat usage = %+v", resp.Usage)
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("Chat finish reason = %q, want %q", resp.FinishReason, "stop")
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, "Bearer sk-test")
	}
	if gotModel != "gpt-4o-mini" {
		t.Fatalf("wire model = %q, want %q", gotModel, "gpt-4o-mini")
	}
	if got, _ := gotBody["temperature"].(float64); got != 0.2 {
		t.Fatalf("Params passthrough temperature = %v, want 0.2", gotBody["temperature"])
	}
}

func TestOpenAICompatibleProvider_Chat_ParamsCannotOverrideModelOrMessages(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "ok"}}},
			"usage":   map[string]int{},
		})
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProvider(srv.URL, "sk-test")
	_, err := p.Chat(context.Background(), ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}},
		Params:   map[string]any{"model": "smuggled-model"},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got, _ := gotBody["model"].(string); got != "gpt-4o-mini" {
		t.Fatalf("wire model = %q, want the request's own model to win over Params", got)
	}
}

func TestOpenAICompatibleProvider_Chat_NonOKStatus_ReturnsProviderRequestFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProvider(srv.URL, "sk-bad")
	_, err := p.Chat(context.Background(), ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}},
	})
	if got, ok := apperrCode(err); !ok || got != ErrProviderRequestFailed.Code {
		t.Fatalf("Chat err = %v, want ErrProviderRequestFailed", err)
	}
}

func TestOpenAICompatibleProvider_Chat_MalformedJSON_ReturnsProviderResponseInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{not valid json`))
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProvider(srv.URL, "sk-test")
	_, err := p.Chat(context.Background(), ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}},
	})
	if got, ok := apperrCode(err); !ok || got != ErrProviderResponseInvalid.Code {
		t.Fatalf("Chat err = %v, want ErrProviderResponseInvalid", err)
	}
}

func TestOpenAICompatibleProvider_Chat_NoChoices_ReturnsProviderResponseInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{}, "usage": map[string]int{}})
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProvider(srv.URL, "sk-test")
	_, err := p.Chat(context.Background(), ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}},
	})
	if got, ok := apperrCode(err); !ok || got != ErrProviderResponseInvalid.Code {
		t.Fatalf("Chat err = %v, want ErrProviderResponseInvalid", err)
	}
}

// --- ChatStream -------------------------------------------------------------

// sseHandler writes each of lines as its own SSE frame, flushing after each
// one so a streaming client observes them incrementally rather than all at
// once.
func sseHandler(lines ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, line := range lines {
			fmt.Fprintf(w, "data: %s\n\n", line)
			flusher.Flush()
		}
	}
}

func drainStream(t *testing.T, ch <-chan ChatChunk) []ChatChunk {
	t.Helper()
	var chunks []ChatChunk
	timeout := time.After(5 * time.Second)
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				return chunks
			}
			chunks = append(chunks, chunk)
		case <-timeout:
			t.Fatal("timed out draining the stream")
		}
	}
}

func TestOpenAICompatibleProvider_ChatStream_Success_ContentThenUsageThenCloses(t *testing.T) {
	srv := httptest.NewServer(sseHandler(
		`{"choices":[{"delta":{"content":"Hel"}}]}`,
		`{"choices":[{"delta":{"content":"lo"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		`[DONE]`,
	))
	defer srv.Close()

	p := NewOpenAICompatibleProvider(srv.URL, "sk-test")
	ch, err := p.ChatStream(context.Background(), ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	chunks := drainStream(t, ch)
	if len(chunks) != 4 {
		t.Fatalf("got %d chunks, want 4: %+v", len(chunks), chunks)
	}
	if chunks[0].Delta != "Hel" || chunks[1].Delta != "lo" {
		t.Fatalf("content chunks = %+v", chunks[:2])
	}
	if chunks[2].FinishReason != "stop" {
		t.Fatalf("finish-reason chunk = %+v", chunks[2])
	}
	last := chunks[3]
	if last.Err != nil {
		t.Fatalf("terminal chunk carries an error: %v", last.Err)
	}
	if last.Usage == nil || *last.Usage != (Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}) {
		t.Fatalf("terminal chunk usage = %+v, want the real usage", last.Usage)
	}
}

func TestOpenAICompatibleProvider_ChatStream_NonOKStatus_ReturnsErrorDirectly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProvider(srv.URL, "sk-test")
	_, err := p.ChatStream(context.Background(), ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}},
	})
	if got, ok := apperrCode(err); !ok || got != ErrProviderRequestFailed.Code {
		t.Fatalf("ChatStream err = %v, want ErrProviderRequestFailed returned directly (before any streaming began)", err)
	}
}

func TestOpenAICompatibleProvider_ChatStream_MalformedChunk_TerminalErrorChunk(t *testing.T) {
	srv := httptest.NewServer(sseHandler(
		`{"choices":[{"delta":{"content":"Hel"}}]}`,
		`{not valid json at all`,
	))
	defer srv.Close()

	p := NewOpenAICompatibleProvider(srv.URL, "sk-test")
	ch, err := p.ChatStream(context.Background(), ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	chunks := drainStream(t, ch)
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2 (content then terminal error): %+v", len(chunks), chunks)
	}
	if chunks[0].Delta != "Hel" {
		t.Fatalf("first chunk = %+v", chunks[0])
	}
	last := chunks[1]
	if last.Err == nil {
		t.Fatal("terminal chunk carries no error, want one for the malformed line")
	}
	if got, ok := apperrCode(last.Err); !ok || got != ErrProviderResponseInvalid.Code {
		t.Fatalf("terminal chunk err = %v, want ErrProviderResponseInvalid", last.Err)
	}
	if last.Usage != nil {
		t.Fatalf("terminal error chunk carries Usage %+v, want nil -- a failed stream must never be billed", last.Usage)
	}
}

func TestOpenAICompatibleProvider_ChatStream_MidStreamConnectionDrop_TerminalErrorChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter does not support Flush")
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
		flusher.Flush()

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("ResponseWriter does not support Hijack")
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("Hijack: %v", err)
			return
		}
		// Closing the raw connection mid-response, without the chunked
		// transfer's terminating "0\r\n\r\n", makes the client observe an
		// I/O error reading the body instead of a clean end of stream --
		// simulating a real mid-stream network failure.
		_ = conn.Close()
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProvider(srv.URL, "sk-test")
	ch, err := p.ChatStream(context.Background(), ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	chunks := drainStream(t, ch)
	if len(chunks) == 0 {
		t.Fatal("got no chunks at all, want at least the content chunk sent before the drop")
	}
	last := chunks[len(chunks)-1]
	if last.Err == nil {
		t.Fatalf("last chunk = %+v, want a terminal error chunk for the dropped connection", last)
	}
	if got, ok := apperrCode(last.Err); !ok || got != ErrProviderRequestFailed.Code {
		t.Fatalf("terminal chunk err = %v, want ErrProviderRequestFailed", last.Err)
	}
}

func TestOpenAICompatibleProvider_ChatStream_NoDoneSentinel_EndsCleanly(t *testing.T) {
	// Some OpenAI-compatible hosts close the connection cleanly without
	// ever sending the literal "data: [DONE]" line. That must still be
	// treated as an ordinary end of stream, not an error.
	srv := httptest.NewServer(sseHandler(
		`{"choices":[{"delta":{"content":"Hi"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
	))
	defer srv.Close()

	p := NewOpenAICompatibleProvider(srv.URL, "sk-test")
	ch, err := p.ChatStream(context.Background(), ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	chunks := drainStream(t, ch)
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2: %+v", len(chunks), chunks)
	}
	if chunks[1].Err != nil {
		t.Fatalf("last chunk = %+v, want a clean terminal success chunk with no error", chunks[1])
	}
}

// TestOpenAICompatibleProvider_ChatStream_NoUsageEverSent_WarnsAndEndsCleanly
// reproduces the bug: a vendor that ignores
// stream_options.include_usage (real self-hosted/open-weight
// OpenAI-compatible gateways do this) and closes the connection cleanly
// after content, with no usage chunk and no [DONE] sentinel either, used to
// leave the channel with no terminal chunk at all and nothing in the logs
// to explain the metering gap. It must now: (1) still deliver the content
// chunk that genuinely arrived, (2) never fabricate a Usage or turn the
// already-successful content into a terminal error, and (3) log a warning
// so the omission is not silent.
func TestOpenAICompatibleProvider_ChatStream_NoUsageEverSent_WarnsAndEndsCleanly(t *testing.T) {
	srv := httptest.NewServer(sseHandler(
		`{"choices":[{"delta":{"content":"Hi"}}]}`,
	))
	defer srv.Close()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	ctx := obs.WithLogger(context.Background(), logger)

	p := NewOpenAICompatibleProvider(srv.URL, "sk-test")
	ch, err := p.ChatStream(ctx, ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	chunks := drainStream(t, ch)
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1 (just the content that genuinely arrived): %+v", len(chunks), chunks)
	}
	if chunks[0].Delta != "Hi" {
		t.Fatalf("content chunk = %+v", chunks[0])
	}
	if chunks[0].Err != nil {
		t.Fatalf("content chunk carries an error %v, want the already-delivered content left untouched", chunks[0].Err)
	}
	if chunks[0].Usage != nil {
		t.Fatalf("content chunk carries fabricated Usage %+v, want none", chunks[0].Usage)
	}
	if !strings.Contains(logBuf.String(), "aigateway: chat stream ended without vendor usage data") {
		t.Fatalf("log output = %q, want a warning naming the missing vendor usage data", logBuf.String())
	}
}

// TestOpenAICompatibleProvider_ChatStream_DoneWithNoUsage_WarnsAndEndsCleanly
// is the same gap reached through the other clean-exit path: a vendor that
// terminates with the literal "[DONE]" sentinel but never sent a
// usage-bearing chunk before it.
func TestOpenAICompatibleProvider_ChatStream_DoneWithNoUsage_WarnsAndEndsCleanly(t *testing.T) {
	srv := httptest.NewServer(sseHandler(
		`{"choices":[{"delta":{"content":"Hi"}}]}`,
		`[DONE]`,
	))
	defer srv.Close()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	ctx := obs.WithLogger(context.Background(), logger)

	p := NewOpenAICompatibleProvider(srv.URL, "sk-test")
	ch, err := p.ChatStream(ctx, ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	chunks := drainStream(t, ch)
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1: %+v", len(chunks), chunks)
	}
	if !strings.Contains(logBuf.String(), "aigateway: chat stream ended without vendor usage data") {
		t.Fatalf("log output = %q, want a warning naming the missing vendor usage data", logBuf.String())
	}
}

// TestOpenAICompatibleProvider_ChatStream_UsageSent_NoWarning is the
// negative case pinning that the warning added above never fires on the
// ordinary, correctly-behaving path.
func TestOpenAICompatibleProvider_ChatStream_UsageSent_NoWarning(t *testing.T) {
	srv := httptest.NewServer(sseHandler(
		`{"choices":[{"delta":{"content":"Hi"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		`[DONE]`,
	))
	defer srv.Close()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	ctx := obs.WithLogger(context.Background(), logger)

	p := NewOpenAICompatibleProvider(srv.URL, "sk-test")
	ch, err := p.ChatStream(ctx, ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	drainStream(t, ch)
	if strings.Contains(logBuf.String(), "chat stream ended without vendor usage data") {
		t.Fatalf("log output = %q, want no missing-usage warning when the vendor did report usage", logBuf.String())
	}
}
