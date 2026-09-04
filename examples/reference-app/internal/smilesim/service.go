// Package smilesim is the reference app's small, non-HTTP-generated
// business service that is go/ai-gateway round 2's mandatory first
// consumer of Gateway.GenerateImage (root CLAUDE.md's "Reference App"
// section: "a module API that it does not actually use is not considered
// done"): given a patient photo the caller already uploaded through
// go/storage, it asks go/ai-gateway for an async AI smile simulation --
// an image-to-image transformation standing in for this
// dental SaaS's own real before/after preview feature, exactly the
// use case root CLAUDE.md's own premise for this reference app names.
//
// Like internal/consult (round 1's chat consumer), it deliberately does not
// go through the OpenAPI machinery: ai-gateway itself ships no HTTP surface
// for either round's spec fragment to grow into. Its two routes (POST
// /api/v1/smile-simulation/simulate, GET
// /api/v1/smile-simulation/jobs/{id}) are mounted by hand in cmd/server
// (cmd/server/smilesim.go), the same pattern consult.go and the
// notification module's own demo patient-message route already establish
// in this app.
package smilesim

import (
	"context"

	aigateway "github.com/vislake/speed/go/ai-gateway"
	"github.com/vislake/speed/go/jobs"
)

// LogicalModel is the logical image model key this service asks
// go/ai-gateway's Gateway to route -- never a vendor-specific model id, the
// image-side mirror of consult.LogicalModel's identical rule (see
// aigateway.ImageRequest.Model's own doc comment). The host wires
// aigateway.WithModelRoute for this exact key onto whatever image provider
// should actually answer it (cmd/server/server.go's buildServer).
const LogicalModel = "image:smile-simulation"

// simulationPrompt primes every request this service sends: a short,
// deterministic instruction, never varying by tenant or by photo, so it
// carries no user-facing text of its own to localize -- it is sent to the
// vendor as part of the image request, never rendered to a person, exactly
// like consult.systemPrompt's identical role for chat.
const simulationPrompt = "Simulate a bright, straight, natural-looking " +
	"smile for this dental patient photo. Keep the rest of the face, " +
	"lighting and background unchanged."

// Service asks go/ai-gateway's Gateway to run an async before/after smile
// simulation over a patient photo the caller already uploaded through
// go/storage.
//
// The zero value is not ready to use; construct one with NewService.
type Service struct {
	gateway *aigateway.Gateway
}

// NewService returns a Service asking gateway for simulations. Constructing
// one performs no I/O.
func NewService(gateway *aigateway.Gateway) *Service {
	return &Service{gateway: gateway}
}

// Simulate enqueues one async smile-simulation job over photoObjectID -- an
// existing, completed go/storage object of ctx's own tenant (typically
// uploaded through storage's own HTTP surface just before this call) -- and
// returns immediately with the job's id.
//
// This never touches storage or a vendor itself: Gateway.GenerateImage's
// own pipeline (the entitlement gate, route/credential resolution, and the
// job enqueue) runs exactly as it would for any other caller, and the real
// work -- reading photoObjectID's bytes, calling the resolved
// ImageProvider, and writing the generated image back as a brand new
// go/storage object -- happens later, inside the job go/ai-gateway's own
// Module.Register registered. A caller retrieves the result once the job
// completes by polling the same jobs.Queue this app's wiring shares with
// go/ai-gateway (cmd/server/smilesim.go's job-status route) -- see
// aigateway's own image_gateway.go doc comment for the full mechanism.
func (s *Service) Simulate(ctx context.Context, photoObjectID string) (jobs.JobID, error) {
	return s.gateway.GenerateImage(ctx, aigateway.ImageRequest{
		Model:         LogicalModel,
		Operation:     aigateway.ImageOperationImageToImage,
		Prompt:        simulationPrompt,
		InputObjectID: photoObjectID,
	})
}
