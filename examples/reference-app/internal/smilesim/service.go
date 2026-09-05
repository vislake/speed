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
//
// # Completion notification
//
// The generation job itself runs inside go/ai-gateway's own registered job
// handler (image_gateway.go), never inside this package, so this Service
// has no Handle of its own to hook a "the job just finished" moment onto --
// unlike, say, notes' handler.go, which publishes EventNoteCreated from
// the very request handler that just created the row. What this Service
// DOES own is EventSimulationCompleted (mirroring notes' identical "a
// business module publishes a fact, notification consumes it" shape --
// root CLAUDE.md's "Notifications are event-driven" rule): Simulate
// remembers the caller-supplied recipient against the job it started, and
// NotifyOnCompletion -- called by cmd/server's existing job-status poll
// route on every read, which a real client already does to learn when a
// generation is done -- publishes the event exactly once, the first time
// it observes that job at a terminal status. This adds no new job type
// and no change to go/jobs or go/ai-gateway: it reuses the polling this
// app's UI already needs to do anyway, rather than inventing a
// job-completion hook go/jobs' own Handler contract does not offer (only
// OnFailure exists, and this is not a failure path).
//
// The recipient/notified bookkeeping is a plain in-memory map, matching
// this app's other demo-only, single-process conveniences (cmd/server/
// demo_notification.go's own demoUserAddresses table): it does not survive
// a process restart, which is an acceptable limitation for a reference
// app's own demo feature, never for a real deployment's own notification
// pipeline.
package smilesim

import (
	"context"
	"encoding/json"
	"sync"

	aigateway "github.com/vislake/speed/go/ai-gateway"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
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

// EventSimulationCompleted is the domain event type Service publishes
// (via NotifyOnCompletion) once a smile-simulation job reaches a terminal
// status and a recipient was named for it -- the "notes.note.create"
// pattern's completion-side counterpart: a business fact published as a
// pkgcore.Event, for the reference app's own demo notification glue
// (cmd/server/demo_notification.go) to consume and dispatch through
// go/notification, exactly as it already does for notes.EventNoteCreated.
// Its Payload is a SimulationCompletedPayload.
const EventSimulationCompleted = "smilesim.simulation_completed"

// SimulationCompletedPayload is the concrete type carried in the
// pkgcore.Event.Payload of every EventSimulationCompleted event.
type SimulationCompletedPayload struct {
	// ImageJobID is the go/ai-gateway image-generation job's id (the value
	// Simulate returned).
	ImageJobID string

	// TenantID is the owning tenant, carried as a plain string since the
	// payload is a wire-shaped event type, not a pkgcore-typed one --
	// mirroring notes.NoteCreatedPayload's identical field.
	TenantID string

	// RecipientUserID is the user id Simulate was given when the
	// simulation was started -- who asked for it, and therefore who gets
	// notified. Never empty: NotifyOnCompletion never publishes for a job
	// Simulate was called for with an empty recipient (see that method's
	// own doc comment).
	RecipientUserID string

	// Succeeded reports whether the job actually completed successfully
	// (StatusSucceeded) as opposed to terminally failing
	// (StatusDeadLetter) or being cancelled (StatusCancelled).
	Succeeded bool

	// OutputObjectID is the generated image's go/storage object id --
	// aigateway.ImageJobResult.OutputObjectID, decoded from the job's own
	// Result -- set only when Succeeded.
	OutputObjectID string
}

// Service asks go/ai-gateway's Gateway to run an async before/after smile
// simulation over a patient photo the caller already uploaded through
// go/storage, and publishes EventSimulationCompleted once that simulation
// finishes for a caller who named a recipient (see the package doc
// comment's "Completion notification" section).
//
// The zero value is not ready to use; construct one with NewService.
type Service struct {
	gateway *aigateway.Gateway

	// bus is where NotifyOnCompletion publishes EventSimulationCompleted.
	// Nil is legal: NotifyOnCompletion is then simply a no-op, mirroring
	// how a Gateway with no wired Entitlements enforces no quota rather
	// than panicking.
	bus pkgcore.EventBus

	// mu guards recipients and notified, both keyed by the image job's
	// id -- Simulate writes to the first, NotifyOnCompletion reads both
	// and writes to the second, and both may be called concurrently
	// (a real client polls the job-status route from its own goroutine
	// independent of any other request this process is serving).
	mu         sync.Mutex
	recipients map[jobs.JobID]string
	notified   map[jobs.JobID]bool
}

// NewService returns a Service asking gateway for simulations and
// publishing simulation-completed events on bus (nil is legal -- see
// Service's own doc comment on the bus field). Constructing one performs
// no I/O.
func NewService(gateway *aigateway.Gateway, bus pkgcore.EventBus) *Service {
	return &Service{
		gateway:    gateway,
		bus:        bus,
		recipients: make(map[jobs.JobID]string),
		notified:   make(map[jobs.JobID]bool),
	}
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
//
// recipientUserID, when non-empty, is remembered against the returned job
// id: NotifyOnCompletion publishes EventSimulationCompleted naming this
// user once the job reaches a terminal status. An empty recipientUserID
// is legal and common (a caller with no one to notify, or a test) --
// NotifyOnCompletion simply never publishes for that job.
func (s *Service) Simulate(ctx context.Context, photoObjectID, recipientUserID string) (jobs.JobID, error) {
	jobID, err := s.gateway.GenerateImage(ctx, aigateway.ImageRequest{
		Model:         LogicalModel,
		Operation:     aigateway.ImageOperationImageToImage,
		Prompt:        simulationPrompt,
		InputObjectID: photoObjectID,
	})
	if err != nil {
		return "", err
	}
	if recipientUserID != "" {
		s.mu.Lock()
		s.recipients[jobID] = recipientUserID
		s.mu.Unlock()
	}
	return jobID, nil
}

// NotifyOnCompletion publishes EventSimulationCompleted for job, exactly
// once, the first time this is called after job has reached a terminal
// status (StatusSucceeded, StatusDeadLetter or StatusCancelled) -- a no-op
// for a job still pending/running/retrying, for a job Simulate was never
// given a recipient for, and for a job already notified (the map lookups
// below, both under one lock, make the "already notified" check and the
// mark atomic, so two concurrent callers -- two overlapping poll requests
// -- can never both publish for the same job).
//
// Callers that poll job status -- cmd/server's job-status route is this
// app's one caller -- call this after every read they make, terminal or
// not; the method itself decides whether there is anything to do. See the
// package doc comment's "Completion notification" section for why this
// poll-driven shape exists instead of a job-completion hook go/jobs does
// not offer.
func (s *Service) NotifyOnCompletion(ctx context.Context, job *jobs.Job) error {
	if job == nil {
		return nil
	}
	switch job.Status {
	case jobs.StatusSucceeded, jobs.StatusDeadLetter, jobs.StatusCancelled:
	default:
		return nil
	}

	s.mu.Lock()
	recipient, hasRecipient := s.recipients[job.ID]
	alreadyNotified := s.notified[job.ID]
	if hasRecipient && !alreadyNotified {
		s.notified[job.ID] = true
	}
	s.mu.Unlock()
	if !hasRecipient || alreadyNotified {
		return nil
	}

	payload := SimulationCompletedPayload{
		ImageJobID:      string(job.ID),
		TenantID:        string(job.TenantID),
		RecipientUserID: recipient,
		Succeeded:       job.Status == jobs.StatusSucceeded,
	}
	if payload.Succeeded && job.Result != nil {
		var result aigateway.ImageJobResult
		if err := json.Unmarshal(job.Result.Data, &result); err == nil {
			payload.OutputObjectID = result.OutputObjectID
		}
	}

	if s.bus == nil {
		return nil
	}
	return s.bus.Publish(ctx, pkgcore.Event{
		Type:     EventSimulationCompleted,
		TenantID: job.TenantID,
		Payload:  payload,
	})
}
