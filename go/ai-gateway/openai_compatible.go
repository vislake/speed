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

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
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
// errorFromResponse reads while looking for the vendor's JSON error
// envelope, so a vendor that answers an error page with an unbounded body
// cannot make this package scan it all.
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

// ProviderOpenAICompatible is this provider's name: the component name the
// "chat" directory carries it under (provider_components.go), the string a
// route's Provider field holds, and the key a credential row is stored
// under -- one identity across all three.
const ProviderOpenAICompatible = "chat.openai-compatible"

// openaiCompatibleFromConfig adapts a flat pkgcore.Config onto
// NewOpenAICompatibleProvider: the chatProviderComponent descriptor's New
// callback bridges its structured configuration block onto this map, and
// Gateway.buildChat feeds the credential it just resolved for the current
// call (base_url, api_key) through the per-call construction override, so
// this constructor performs no I/O at all; it only validates and assigns
// fields.
//
// The refusal of a config without base_url (or api_key) is this
// constructor's own declaration that OpenAICompatibleProvider has no
// default endpoint of its own: it comes back as the coded, Invalid-
// classified ErrProviderConfigInvalid, so a caller resolving a route onto
// a credential stored without a base URL sees a distinguishable
// configuration error rather than an uncoded one. pkgcore.ErrMissingSeamConfig
// stays attached as the cause, so errors.Is-based callers keep
// recognizing the refusal unchanged.
func openaiCompatibleFromConfig(cfg pkgcore.Config) (ChatProvider, error) {
	baseURL := cfg["base_url"]
	apiKey := cfg["api_key"]
	if baseURL == "" || apiKey == "" {
		return nil, ErrProviderConfigInvalid.
			WithParam("provider", ProviderOpenAICompatible).
			WithParam("reason", "credential carries no base_url or api_key")
	}
	return NewOpenAICompatibleProvider(baseURL, apiKey), nil
}

// setHTTPClient implements httpClientSettable (provider_guard.go): Gateway.resolve
// swaps a freshly built provider's client for guardedProviderHTTPClient
// when the credential that built it resolved at the tenant tier. Not part
// of the public construction API -- hosts configure a client through
// WithHTTPClient -- and nil is ignored, mirroring that option's own nil
// guard.
func (p *OpenAICompatibleProvider) setHTTPClient(c *http.Client) {
	if c != nil {
		p.httpClient = c
	}
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
			// reports usage at all. Streaming usage lands on the last
			// chunk only when it is asked for explicitly.
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
// response. The returned error's params carry ONLY the status code: the
// answer the dialed endpoint gave stays out of what is handed back to the
// caller who steered the dial -- the response-reflux half of this
// module's SSRF posture, the body twin of the no-IP-echo rule
// ErrBaseURLBlocked pins for refusal answers (provider_guard.go). And unlike the
// refusal rule it is not confined to blocked destinations: this echo
// channel is the common error path of every provider call, so it would
// survive the SSRF guards for any ALLOWED endpoint that answers with an
// error body. Which is why the raw body is not logged server-side either:
// provider error text is unbounded free text that routinely echoes the
// request that failed -- content-moderation-class refusals quote the
// input they refused -- and observability's redaction layer masks
// credential shapes, never arbitrary echoed content (its own coverage
// table records plaintext PII and full prompts as caller-declared). What
// reaches the log instead is the vendor's own error contract, parsed from
// the JSON body when it is one: the envelope's enumeration fields
// (error.type / error.code) as structured attributes. A body that is not
// the vendor's JSON error envelope -- an HTML error page, a reverse-proxy
// banner, malformed JSON -- contributes no attributes, and the log line
// carries the status code alone.
func errorFromResponse(ctx context.Context, resp *http.Response) error {
	var wire openaiErrorWire
	// The envelope parse is best-effort and bounded at maxErrorBodyBytes:
	// a non-JSON body, or an envelope longer than the bound, simply
	// contributes no structured fields.
	_ = json.NewDecoder(io.LimitReader(resp.Body, maxErrorBodyBytes)).Decode(&wire)
	attrs := []any{"status_code", resp.StatusCode}
	if wire.Error.Type != "" {
		attrs = append(attrs, "error_type", wire.Error.Type)
	}
	if wire.Error.Code != "" {
		attrs = append(attrs, "error_code", wire.Error.Code)
	}
	obs.FromContext(ctx).Warn("aigateway: provider answered a non-2xx status", attrs...)
	return ErrProviderRequestFailed.WithParam("status", resp.StatusCode)
}

// openaiErrorWire is the error envelope a non-2xx response body may carry
// (the shape OpenAI-compatible hosts answer refusals with), parsed down
// to its two enumeration fields only. The envelope's message field is
// deliberately not read: it is free text, the field content-moderation-
// class refusals echo the refused request input into, so it must never
// reach the log. See errorFromResponse's doc comment.
type openaiErrorWire struct {
	Error struct {
		Type string `json:"type"`
		Code string `json:"code"`
	} `json:"error"`
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
		return ChatResponse{}, errorFromResponse(ctx, resp)
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
		return nil, errorFromResponse(ctx, resp)
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

	// sawUsage tracks whether a usage-bearing chunk was ever sent on out.
	// It is the sole input to warnIfNoUsage below -- see that function's
	// doc comment for why a vendor that never reports usage gets a log
	// line rather than a fabricated Usage or a terminal error chunk.
	sawUsage := false

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
			warnIfNoUsage(ctx, sawUsage)
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
			sawUsage = true
		}
		if hasContent {
			if !send(chunk) {
				return
			}
		}
	}

	if err := scanner.Err(); err != nil {
		send(ChatChunk{Err: ErrProviderRequestFailed.WithCause(err)})
		return
	}
	// A scanner that stops with no error and no [DONE] line (the vendor
	// closed the connection cleanly without sending the sentinel) is
	// treated as an ordinary end of stream, not an error: some
	// OpenAI-compatible hosts omit the literal sentinel line. This
	// matches real-world OpenAI-compatible server behavior better than
	// failing a stream that otherwise delivered every chunk correctly.
	warnIfNoUsage(ctx, sawUsage)
}

// warnIfNoUsage logs a warning when a ChatStream call is about to end
// cleanly (no transport error, no malformed chunk) without ever having sent
// a usage-bearing chunk -- reached from both clean-exit paths in
// streamChunks, the [DONE] sentinel and a clean EOF alike.
//
// buildRequestBody always requests stream_options.include_usage, but some
// self-hosted/open-weight OpenAI-compatible hosts (many llama.cpp/vLLM
// front ends among them) silently drop unknown request fields and never
// report usage for a streaming response at all. The content already sent
// on out is real and successfully delivered, so this is deliberately not
// turned into a terminal error chunk (that would make a consumer discard or
// distrust content that in fact arrived correctly) and Usage is
// deliberately never fabricated as a zero value (ChatChunk's own doc
// comment requires a success terminal chunk's Usage to be the stream's
// REAL final token usage, and a fabricated zero would misreport actual
// usage to a wired UsageRecorder as if the call had truly cost nothing).
// The one thing this function does is make the gap visible in the logs --
// without it, Gateway.recordUsage (gateway.go) simply never fires for this
// stream, with nothing anywhere indicating why.
func warnIfNoUsage(ctx context.Context, sawUsage bool) {
	if sawUsage {
		return
	}
	obs.FromContext(ctx).Warn("aigateway: chat stream ended without vendor usage data")
}
