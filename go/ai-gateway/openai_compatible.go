package aigateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// chatCompletionsPath is the OpenAI-compatible chat-completions endpoint
// path, appended to a provider's configured base URL.
const chatCompletionsPath = "/chat/completions"

// defaultHTTPTimeout bounds a non-streaming Chat call. It is not applied to
// ChatStream's underlying request -- a stream can legitimately run far
// longer than any fixed request timeout, so streaming calls are bounded
// only by ctx.
const defaultHTTPTimeout = 60 * time.Second

// maxErrorBodyBytes caps how much of a non-2xx response body
// errorFromResponse reads into the returned error's params, so a vendor
// that answers an error page with an unbounded body cannot make this
// package hold it all in memory.
const maxErrorBodyBytes = 4096

// streamScannerBufferBytes and streamScannerMaxBytes size the
// bufio.Scanner ChatStream reads Server-Sent-Events lines with: a
// realistic starting buffer plus a generous ceiling for one long "data: "
// line (bufio.Scanner's default 64KiB limit is comfortably enough for one
// chat chunk, but is raised here since a vendor's single SSE line has no
// contractual size bound).
const (
	streamScannerBufferBytes = 64 * 1024
	streamScannerMaxBytes    = 1024 * 1024
)

// sseDataPrefix is the Server-Sent-Events "data: " field prefix this
// provider parses; every other SSE field (event:, id:, retry:, comments
// starting with ":") is ignored, since the OpenAI-compatible schema only
// ever uses the data field.
const sseDataPrefix = "data:"

// sseDoneSentinel is the literal payload that terminates an OpenAI-
// compatible SSE stream.
const sseDoneSentinel = "[DONE]"

// OpenAICompatibleProvider implements ChatProvider against the
// chat-completions REST schema shared by OpenAI itself and every
// OpenAI-compatible host (Azure OpenAI, DeepSeek, many self-hosted/
// open-weight gateways): POST a JSON body carrying model/messages/stream to
// baseURL+"/chat/completions" with a Bearer Authorization header.
//
// It is implemented directly against the wire schema with stdlib net/http
// and encoding/json only -- no vendor SDK -- which is what keeps this
// module's default path at zero third-party dependencies, the same posture
// go/pki's LocalSigner and pkgcore's in-process EventBus keep for their own
// zero-dependency defaults.
//
// The zero value is not ready to use; construct one with
// NewOpenAICompatibleProvider.
type OpenAICompatibleProvider struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// OpenAICompatibleOption configures an OpenAICompatibleProvider at
// construction time.
type OpenAICompatibleOption func(*OpenAICompatibleProvider)

// WithHTTPClient overrides the *http.Client the provider issues requests
// with (default: a client with no timeout of its own, relying on ctx and
// defaultHTTPTimeout for Chat). Tests use this to point the provider at an
// httptest.Server.
func WithHTTPClient(client *http.Client) OpenAICompatibleOption {
	return func(p *OpenAICompatibleProvider) {
		if client != nil {
			p.httpClient = client
		}
	}
}

// NewOpenAICompatibleProvider returns a ChatProvider that calls baseURL
// with apiKey as a Bearer credential. baseURL must not include a trailing
// slash requirement of its own -- chatCompletionsPath is appended directly
// (for example "https://api.openai.com/v1" -> ".../v1/chat/completions").
func NewOpenAICompatibleProvider(baseURL, apiKey string, opts ...OpenAICompatibleOption) *OpenAICompatibleProvider {
	p := &OpenAICompatibleProvider{
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		apiKey:     apiKey,
		httpClient: &http.Client{},
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// compile-time check that *OpenAICompatibleProvider satisfies ChatProvider.
var _ ChatProvider = (*OpenAICompatibleProvider)(nil)

// buildRequestBody builds the JSON wire body for req -- whose Model is
// already the concrete vendor model id by the time a ChatProvider sees it,
// per ChatRequest's own doc comment -- merging req.Params in first so that
// model/messages/stream always win over a same-named Params entry: a
// caller can never override which model or which conversation is actually
// sent by smuggling a same-named key into Params.
func buildRequestBody(req ChatRequest, stream bool, streamUsage bool) ([]byte, error) {
	body := make(map[string]any, len(req.Params)+4)
	for k, v := range req.Params {
		body[k] = v
	}

	messages := make([]map[string]string, len(req.Messages))
	for i, m := range req.Messages {
		messages[i] = map[string]string{"role": string(m.Role), "content": m.Content}
	}

	body["model"] = req.Model
	body["messages"] = messages
	if stream {
		body["stream"] = true
		if streamUsage {
			// stream_options.include_usage is what makes an OpenAI-
			// compatible streaming response carry a final chunk with real
			// token usage -- without it, a streaming response never
			// reports usage at all. This is exactly the design doc's rule
			// that "流式响应在最后一个 chunk 处理真实用量" (streaming
			// responses handle real usage at the last chunk), made real by
			// asking for it explicitly.
			body["stream_options"] = map[string]any{"include_usage": true}
		}
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("aigateway: encode chat request: %w", err)
	}
	return encoded, nil
}

// newHTTPRequest builds the POST request every Chat/ChatStream call sends.
func (p *OpenAICompatibleProvider) newHTTPRequest(ctx context.Context, body []byte, accept string) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+chatCompletionsPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("aigateway: build chat request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	if accept != "" {
		httpReq.Header.Set("Accept", accept)
	}
	return httpReq, nil
}

// errorFromResponse builds ErrProviderRequestFailed from a non-2xx HTTP
// response, reading at most maxErrorBodyBytes of the body so an unbounded
// vendor error page cannot be held entirely in memory.
func errorFromResponse(resp *http.Response) error {
	limited := io.LimitReader(resp.Body, maxErrorBodyBytes)
	raw, _ := io.ReadAll(limited)
	return ErrProviderRequestFailed.
		WithParam("status", resp.StatusCode).
		WithParam("body", string(raw))
}

// openaiChatMessageWire is the message shape inside a non-streaming
// response's choices[].message.
type openaiChatMessageWire struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// openaiUsageWire is the usage shape shared by the non-streaming response
// and a streaming response's final chunk.
type openaiUsageWire struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// openaiChatResponseWire is the non-streaming chat-completions response
// body.
type openaiChatResponseWire struct {
	Choices []struct {
		Message      openaiChatMessageWire `json:"message"`
		FinishReason string                `json:"finish_reason"`
	} `json:"choices"`
	Usage openaiUsageWire `json:"usage"`
}

// Chat implements ChatProvider.
func (p *OpenAICompatibleProvider) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultHTTPTimeout)
	defer cancel()

	body, err := buildRequestBody(req, false, false)
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq, err := p.newHTTPRequest(ctx, body, "application/json")
	if err != nil {
		return ChatResponse{}, err
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return ChatResponse{}, ErrProviderRequestFailed.WithCause(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return ChatResponse{}, errorFromResponse(resp)
	}

	var wire openaiChatResponseWire
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return ChatResponse{}, ErrProviderResponseInvalid.WithCause(err)
	}
	if len(wire.Choices) == 0 {
		return ChatResponse{}, ErrProviderResponseInvalid.WithParam("reason", "no choices in response")
	}

	choice := wire.Choices[0]
	return ChatResponse{
		Message: ChatMessage{
			Role:    RoleAssistant,
			Content: choice.Message.Content,
		},
		Usage: Usage{
			PromptTokens:     wire.Usage.PromptTokens,
			CompletionTokens: wire.Usage.CompletionTokens,
			TotalTokens:      wire.Usage.TotalTokens,
		},
		FinishReason: choice.FinishReason,
	}, nil
}

// openaiStreamChunkWire is one Server-Sent-Events "data: " payload of a
// streaming chat-completions response. Choices is empty on the final chunk
// carrying Usage (when stream_options.include_usage was requested); Usage
// is nil on every ordinary content chunk.
type openaiStreamChunkWire struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openaiUsageWire `json:"usage"`
}

// ChatStream implements ChatProvider. See ChatChunk's own doc comment for
// the exact channel contract this method's returned channel honors.
func (p *OpenAICompatibleProvider) ChatStream(ctx context.Context, req ChatRequest) (<-chan ChatChunk, error) {
	body, err := buildRequestBody(req, true, true)
	if err != nil {
		return nil, err
	}
	httpReq, err := p.newHTTPRequest(ctx, body, "text/event-stream")
	if err != nil {
		return nil, err
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, ErrProviderRequestFailed.WithCause(err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, errorFromResponse(resp)
	}

	out := make(chan ChatChunk)
	go streamChunks(ctx, resp.Body, out)
	return out, nil
}

// streamChunks reads Server-Sent-Events lines from body, decodes each
// "data: " payload, and sends ChatChunk values on out until either the
// sseDoneSentinel line arrives (clean end of stream) or a failure occurs --
// a malformed chunk, or an I/O error reading body. It closes out and body
// exactly once, on every exit path, and never sends after closing.
func streamChunks(ctx context.Context, body io.ReadCloser, out chan<- ChatChunk) {
	defer close(out)
	defer func() { _ = body.Close() }()

	send := func(chunk ChatChunk) bool {
		select {
		case out <- chunk:
			return true
		case <-ctx.Done():
			return false
		}
	}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, streamScannerBufferBytes), streamScannerMaxBytes)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, sseDataPrefix) {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, sseDataPrefix))
		if data == "" {
			continue
		}
		if data == sseDoneSentinel {
			return
		}

		var wire openaiStreamChunkWire
		if err := json.Unmarshal([]byte(data), &wire); err != nil {
			send(ChatChunk{Err: ErrProviderResponseInvalid.WithCause(err)})
			return
		}

		chunk := ChatChunk{}
		hasContent := false
		if len(wire.Choices) > 0 {
			choice := wire.Choices[0]
			chunk.Delta = choice.Delta.Content
			if choice.FinishReason != nil {
				chunk.FinishReason = *choice.FinishReason
			}
			hasContent = chunk.Delta != "" || chunk.FinishReason != ""
		}
		if wire.Usage != nil {
			usage := Usage{
				PromptTokens:     wire.Usage.PromptTokens,
				CompletionTokens: wire.Usage.CompletionTokens,
				TotalTokens:      wire.Usage.TotalTokens,
			}
			chunk.Usage = &usage
			hasContent = true
		}
		if hasContent {
			if !send(chunk) {
				return
			}
		}
	}

	if err := scanner.Err(); err != nil {
		send(ChatChunk{Err: ErrProviderRequestFailed.WithCause(err)})
	}
	// A scanner that stops with no error and no [DONE] line (the vendor
	// closed the connection cleanly without sending the sentinel) is
	// treated as an ordinary end of stream, not an error: some
	// OpenAI-compatible hosts omit the literal sentinel line. This
	// matches real-world OpenAI-compatible server behavior better than
	// failing a stream that otherwise delivered every chunk correctly.
}
