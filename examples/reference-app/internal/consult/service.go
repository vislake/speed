// Package consult is the reference app's small, non-HTTP-generated
// business service that is go/ai-gateway's mandatory first consumer (root
// CLAUDE.md's "Reference App" section: "a module API that it does not
// actually use is not considered done"): given an existing note's id, it
// asks go/ai-gateway's Gateway for a short AI-generated
// consultation-suggestion summary of that note's text -- standing in for
// this dental SaaS's real consultation-assistant feature.
//
// It deliberately does not go through the OpenAPI machinery: ai-gateway
// itself ships no HTTP surface this round (see go/ai-gateway/AGENTS.md's
// "What this round ships" section), so there is no spec fragment for a
// route calling it to grow into. Its one route (POST
// /api/v1/consult/suggest) is mounted by hand in cmd/server
// (cmd/server/consult.go), the same pattern the notification module's own
// demo patient-message route already establishes in this app
// (cmd/server/demo_notification.go's package comment).
package consult

import (
	"context"

	aigateway "github.com/vislake/speed/go/ai-gateway"

	"github.com/vislake/speed/examples/reference-app/internal/notes"
)

// LogicalModel is the logical chat model key this service asks
// go/ai-gateway's Gateway to route -- never a vendor-specific model id (see
// aigateway.ChatRequest.Model's own doc comment for why business code never
// sees or hardcodes one). The host wires aigateway.WithModelRoute for this
// exact key onto whatever provider should actually answer it
// (cmd/server/server.go's buildServer).
const LogicalModel = "chat:default"

// systemPrompt primes every request this service sends: a short,
// deterministic instruction that keeps the model's reply on topic. It never
// varies by tenant or by note, so it carries no user-facing text of its own
// to localize -- it is sent to the vendor as part of the chat request, never
// rendered to a person.
const systemPrompt = "You are a dental consultation assistant. Given a " +
	"clinician's note, reply with one short, plain-language suggestion for " +
	"the patient's next step. Keep the reply to one or two sentences."

// Service asks go/ai-gateway's Gateway for a short AI-generated
// consultation-suggestion summary of one note's text.
//
// The zero value is not ready to use; construct one with NewService.
type Service struct {
	notes   *notes.Repository
	gateway *aigateway.Gateway
}

// NewService returns a Service reading notes through notesRepo -- expected
// to share the host's own notes.Repository/database connection, exactly the
// way go/dbkit/audit's persister shares it (cmd/server/server.go's own
// wiring comment on auditModule explains the identical reasoning) -- and
// asking gateway for suggestions. Constructing one performs no I/O.
func NewService(notesRepo *notes.Repository, gateway *aigateway.Gateway) *Service {
	return &Service{notes: notesRepo, gateway: gateway}
}

// Suggest reads noteID's text -- scoped to ctx's tenant through the same
// dbkit.Repository[Note] every other notes reader uses, so a note belonging
// to another tenant is invisible here exactly as it is to notes' own HTTP
// surface -- and asks the gateway for a short consultation-suggestion
// summary under LogicalModel.
//
// The gateway's own pipeline (credential resolution, model routing, the
// optional entitlement gate, the provider call, then automatic usage
// reporting) runs exactly as it would for any other caller; this service
// adds nothing to it beyond building the ChatRequest and reading the note
// it is about.
func (s *Service) Suggest(ctx context.Context, noteID string) (string, error) {
	note, err := s.notes.FindByID(ctx, noteID)
	if err != nil {
		return "", err
	}

	resp, err := s.gateway.Chat(ctx, aigateway.ChatRequest{
		Model: LogicalModel,
		Messages: []aigateway.ChatMessage{
			{Role: aigateway.RoleSystem, Content: systemPrompt},
			{Role: aigateway.RoleUser, Content: note.Text},
		},
	})
	if err != nil {
		return "", err
	}
	return resp.Message.Content, nil
}
