package aigateway

import "context"

// ChatRole names who spoke a ChatMessage.
type ChatRole string

const (
	// RoleSystem is a system-level instruction message.
	RoleSystem ChatRole = "system"
	// RoleUser is a message from the end user.
	RoleUser ChatRole = "user"
	// RoleAssistant is a message from the model itself -- the role every
	// ChatResponse.Message and every ChatChunk's accumulated content
	// carries.
	RoleAssistant ChatRole = "assistant"
)

// ChatMessage is one turn of a chat conversation.
type ChatMessage struct {
	// Role is who spoke this message.
	Role ChatRole
	// Content is the message text.
	Content string
}

// ChatRequest is Gateway.Chat and Gateway.ChatStream's input, and also what
// a ChatProvider implementation receives -- with one field meaning two
// different things depending which side of the Gateway is looking at it.
//
// At the Gateway.Chat/ChatStream boundary, Model is a caller-declared
// LOGICAL model key (for example "chat:default" or "chat:fast"), never a
// vendor-specific model id -- business code never sees or hardcodes one
// (docs/internal/08-ai-gateway.md's model-routing rule). The Gateway
// resolves that key to a ModelRoute and, before calling the provider,
// rewrites a copy of this same struct so that Model instead carries the
// route's concrete VendorModel (e.g. "gpt-4o-mini"). A ChatProvider
// implementation therefore always sees a concrete vendor model id in
// Model, never a logical key -- the translation has already happened by
// the time a provider's Chat or ChatStream method runs.
type ChatRequest struct {
	// Model is the logical model key at the Gateway boundary, and the
	// resolved vendor model id once a ChatProvider receives it. See the
	// type's own doc comment.
	Model string
	// Messages is the conversation so far, oldest first. At least one
	// message with non-empty Content is required.
	Messages []ChatMessage
	// Params carries vendor-specific arguments (temperature, top_p, and so
	// on) passed through to the provider's wire request verbatim, per the
	// design doc's explicit rule to avoid an over-rigid abstraction. A
	// provider merges Params into its wire request without letting any
	// entry override Model or Messages.
	Params map[string]any
}

// validate checks the fields every entry point (Gateway.Chat,
// Gateway.ChatStream) requires before any resolution or network call is
// attempted.
func (r ChatRequest) validate() error {
	if r.Model == "" {
		return ErrEmptyModel
	}
	if len(r.Messages) == 0 {
		return ErrEmptyMessages.WithParam("model", r.Model)
	}
	for _, m := range r.Messages {
		if m.Content == "" {
			return ErrEmptyMessages.WithParam("model", r.Model)
		}
	}
	return nil
}

// Usage is the token-count billing dimension the design doc names as the
// default AI metering case.
type Usage struct {
	// PromptTokens is how many tokens the request's messages consumed.
	PromptTokens int
	// CompletionTokens is how many tokens the model's reply consumed.
	CompletionTokens int
	// TotalTokens is PromptTokens plus CompletionTokens, as the vendor
	// reported it -- carried separately rather than computed, since a
	// vendor's own total occasionally includes dimensions (e.g. reasoning
	// tokens) neither of the other two fields capture.
	TotalTokens int
}

// ChatResponse is Gateway.Chat's (and a non-streaming ChatProvider.Chat's)
// result.
type ChatResponse struct {
	// Message is the assistant's reply.
	Message ChatMessage
	// Usage is the real token usage the vendor reported for this call.
	Usage Usage
	// FinishReason is the vendor's reason the reply stopped (for example
	// "stop", "length"), passed through verbatim. Empty when the vendor
	// did not report one.
	FinishReason string
}

// ChatChunk is one increment of a Gateway.ChatStream (and a streaming
// ChatProvider.ChatStream) response.
//
// # Channel contract
//
// A ChatStream channel delivers zero or more ordinary chunks (Delta
// non-empty, Usage nil, Err nil), and then exactly one of two possible
// terminal chunks, immediately followed by the channel being closed:
//
//   - a SUCCESS terminal chunk: Usage is non-nil and carries the stream's
//     real, final token usage (Err is nil). Delta and FinishReason may
//     also be set on this same chunk when the vendor's own final wire
//     event carries trailing content alongside the usage; a consumer
//     should append Delta (if any) before treating Usage as the answer.
//   - an ERROR terminal chunk: Err is non-nil (a sentinel chunk, never a
//     separate error return or error channel) and every other field is
//     the zero value. This covers both a transport failure that
//     interrupts an in-progress stream and a chunk the provider could not
//     parse.
//
// No chunk arrives after either terminal chunk; the channel is always
// closed by the goroutine that sent it. A consumer that only cares about
// the final answer can therefore simply range over the channel until it
// closes and treat the last-seen non-zero Usage as authoritative, checking
// Err on each chunk to detect the error path.
//
// This is a deliberate design choice among the alternatives (a second error
// return from ChatStream itself cannot report a failure that only manifests
// mid-stream, after the HTTP response has already started; a separate error
// channel forces every consumer to select over two channels for no benefit,
// since a chat stream is inherently sequential) -- see
// openai_compatible_test.go for the tests pinning both terminal shapes,
// including a malformed mid-stream chunk and a stream that ends in an
// HTTP-level error after already emitting content.
type ChatChunk struct {
	// Delta is the incremental content this chunk adds to the reply.
	// Empty on a chunk that carries nothing but a FinishReason, a Usage,
	// or an Err.
	Delta string
	// FinishReason is the vendor's reason the reply stopped, set on the
	// chunk that carries it (typically the last content chunk). Empty
	// otherwise.
	FinishReason string
	// Usage is non-nil only on the stream's terminal success chunk -- see
	// the type's own doc comment for the full channel contract.
	Usage *Usage
	// Err is non-nil only on the stream's terminal error chunk -- see the
	// type's own doc comment for the full channel contract.
	Err error
}

// ChatProvider is the unified chat abstraction every vendor integration
// implements. Business code never calls a ChatProvider directly -- it calls
// Gateway.Chat / Gateway.ChatStream, and the Gateway resolves and calls the
// provider on its behalf (see this package's doc comment for the full
// pipeline).
//
// This round's MVP does not build a self-hosted inference service, but
// ChatProvider is deliberately unaware of whether an implementation talks
// to a local or a remote model -- a future Ollama/vLLM integration is just
// one more implementation, per the design doc.
type ChatProvider interface {
	// Chat sends req and waits for the complete reply.
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)

	// ChatStream sends req and returns a channel of incremental chunks.
	// See ChatChunk's own doc comment for the full channel contract: the
	// channel is always closed by the provider, after either a terminal
	// success chunk (real Usage) or a terminal error chunk (Err set) --
	// never both, never neither.
	//
	// A non-nil error return means the request could not even be started
	// (for example a non-2xx HTTP status on the initial response, before
	// any streaming began); once ChatStream has returned a non-nil
	// channel, every failure from that point on surfaces as a chunk on
	// that channel, never as a second error return.
	ChatStream(ctx context.Context, req ChatRequest) (<-chan ChatChunk, error)
}
