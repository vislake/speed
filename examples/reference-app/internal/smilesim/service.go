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
//
// # Credit accounting
//
// This is also the reference app's mandatory first consumer of
// go/billing's CreditService (docs/internal/15-roadmap.md's M2 exit
// condition's credit-pack-purchase, credit-reserve/refund and
// usage/billing-display leg), following the identical "a module API that
// it does not actually use is not considered done" rule root CLAUDE.md's
// Reference App section states.
// Simulate reserves (PreDeduct) CreditsPerSimulation credits BEFORE ever
// calling Gateway.GenerateImage -- an insufficient balance refuses the
// request with billing.ErrInsufficientCredits and never reaches
// go/ai-gateway at all -- and NotifyOnCompletion settles that reservation
// once the job reaches a terminal status: Confirm on StatusSucceeded,
// Refund on StatusDeadLetter/StatusCancelled. Every credit-pack-purchase
// leg (a real Stripe/Alipay/WeChat sandbox charge) is deliberately out of
// scope here -- see cmd/server/server.go's seedDemoCredits for the
// Grant-based demo stand-in this round ships instead, and
// go/billing/gateway/AGENTS.md for why no live payment credentials exist
// in this environment.
//
// creditKeys (below) is the same "in-memory, keyed by job id" shape
// recipients/notified already establish, storing the CreditTransaction
// idempotency key Simulate's PreDeduct used so NotifyOnCompletion's later
// Confirm/Refund settles the SAME reservation rather than minting a new
// one. A Service built with a nil CreditService (NewService's credits
// parameter) performs no credit accounting at all -- the same nil-is-legal
// default every other optional host seam in this codebase takes when
// unwired (mirroring bus's own nil-is-legal contract just above).
package smilesim

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/uuid"

	aigateway "github.com/vislake/speed/go/ai-gateway"
	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/jobs"
	obs "github.com/vislake/speed/go/observability"
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

// CreditsPerSimulation is the flat credit cost Simulate reserves for one
// smile-simulation request -- a reference-app-level demo business policy
// (this round's own scope), not a mechanism go/billing itself prescribes:
// a real deployment would size this per vendor cost, per resolution tier,
// or read it from a go/config item, none of which this round needs to
// prove the reserve/confirm/refund mechanism end to end.
const CreditsPerSimulation int64 = 10

// creditReasonSimulate is the free-text Reason every credit reservation
// this Service opens carries (billing.PreDeductInput.Reason) -- a short,
// human-readable note on the resulting ledger entry, never parsed back by
// code.
//
// #nosec G101 -- this is a ledger annotation string, not a credential:
// gosec's hardcoded-credential heuristic matches on the "cred" substring
// inside "creditReasonSimulate" alone, the same class of false positive
// cmd/server/demo_users.go's own demoUsersPasswordEnv constant already
// documents (there, the "Password" substring) for a differently-shaped
// identifier.
const creditReasonSimulate = "smilesim:simulate"

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

	// credits is the billing.CreditService Simulate reserves against and
	// NotifyOnCompletion settles -- see the package doc comment's "Credit
	// accounting" section. Nil is legal: Simulate then performs no credit
	// accounting at all (no PreDeduct call, never a refusal on balance),
	// and NotifyOnCompletion never calls Confirm/Refund -- mirroring how a
	// Gateway with no wired Entitlements enforces no quota rather than
	// panicking.
	credits *billing.CreditService

	// bus is where NotifyOnCompletion publishes EventSimulationCompleted.
	// Nil is legal: NotifyOnCompletion is then simply a no-op, mirroring
	// how a Gateway with no wired Entitlements enforces no quota rather
	// than panicking.
	bus pkgcore.EventBus

	// mu guards recipients, notified and creditKeys, all keyed by the
	// image job's id -- Simulate writes to recipients and creditKeys,
	// NotifyOnCompletion reads all three, and both methods may be called
	// concurrently (a real client polls the job-status route from its own
	// goroutine independent of any other request this process is
	// serving).
	mu         sync.Mutex
	recipients map[jobs.JobID]string
	notified   map[jobs.JobID]bool

	// creditKeys remembers, for each job Simulate reserved credits for,
	// the CreditTransaction idempotency key PreDeduct used -- so
	// NotifyOnCompletion's later Confirm/Refund settles the SAME
	// reservation Simulate opened rather than minting a new one. A job
	// Simulate ran with credits == nil (or a Service with no CreditService
	// wired at all) has no entry here, which is exactly what makes
	// NotifyOnCompletion's credit-settlement step a no-op for it.
	creditKeys map[jobs.JobID]string
}

// NewService returns a Service asking gateway for simulations, reserving
// and settling credits against credits (nil is legal -- see Service's own
// doc comment on the credits field), and publishing simulation-completed
// events on bus (nil is legal -- see Service's own doc comment on the bus
// field). Constructing one performs no I/O.
func NewService(gateway *aigateway.Gateway, credits *billing.CreditService, bus pkgcore.EventBus) *Service {
	return &Service{
		gateway:    gateway,
		credits:    credits,
		bus:        bus,
		recipients: make(map[jobs.JobID]string),
		notified:   make(map[jobs.JobID]bool),
		creditKeys: make(map[jobs.JobID]string),
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
//
// # Credit reservation
//
// When this Service was built with a non-nil CreditService (see the
// package doc comment's "Credit accounting" section), Simulate reserves
// CreditsPerSimulation credits via CreditService.PreDeduct BEFORE calling
// Gateway.GenerateImage at all: an insufficient balance returns
// billing.ErrInsufficientCredits (a coded 409, never a silent fallback)
// and Gateway.GenerateImage -- and therefore go/ai-gateway, and any real
// vendor it might eventually reach -- is never called.
//
// The reservation's idempotency key cannot be the eventual job id: PreDeduct
// must run before GenerateImage even executes, and GenerateImage (via its
// own jobs.Queue.Enqueue) is what mints the job id, so the key does not
// exist yet at the point this method must already have decided whether to
// refuse. Simulate instead mints one fresh, stable id per call
// (uuid.NewString(), prefixed for readability) and reuses that SAME id for
// every subsequent operation tied to this one generation request: it is
// PreDeduct's IdempotencyKey now, and -- once GenerateImage has returned a
// real job id -- creditKeys[jobID] remembers it so NotifyOnCompletion's
// later Confirm/Refund settles this exact reservation, never a fresh one.
// This is "stable, tied to the specific generation request" in the sense
// CreditService's own idempotency contract requires (PreDeduct.go's doc
// comment): minted once and reused verbatim by every settlement call for
// this request, never regenerated per call the way that would defeat
// Confirm/Refund's own idempotent-retry contract.
//
// If GenerateImage itself fails AFTER the reservation succeeded (an
// unrouted model, a missing credential, an enqueue failure), the
// reservation is refunded immediately -- there will be no job, and
// therefore no later NotifyOnCompletion call, to settle it otherwise. A
// refund failure at that point is logged and swallowed rather than masking
// GenerateImage's own, more actionable error: the reservation is left
// Reserved rather than lost, a bookkeeping loose end root CLAUDE.md's
// audit/reconciliation tooling -- not this method -- is the right place to
// resolve, exactly the "log and swallow, never fail an otherwise-complete
// operation over a secondary side effect" stance recordImageUsage already
// takes in go/ai-gateway for its own usage-reporting side effect.
func (s *Service) Simulate(ctx context.Context, photoObjectID, recipientUserID string) (jobs.JobID, error) {
	var creditKey string
	if s.credits != nil {
		creditKey = "smilesim:" + uuid.NewString()
		if _, err := s.credits.PreDeduct(ctx, billing.PreDeductInput{
			Amount:         CreditsPerSimulation,
			IdempotencyKey: creditKey,
			Reason:         creditReasonSimulate,
		}); err != nil {
			return "", err
		}
	}

	jobID, err := s.gateway.GenerateImage(ctx, aigateway.ImageRequest{
		Model:         LogicalModel,
		Operation:     aigateway.ImageOperationImageToImage,
		Prompt:        simulationPrompt,
		InputObjectID: photoObjectID,
	})
	if err != nil {
		if creditKey != "" {
			if _, refundErr := s.credits.Refund(ctx, creditKey); refundErr != nil {
				obs.FromContext(ctx).Warn("smilesim: refund credit reservation after a failed enqueue failed",
					"credit_idempotency_key", creditKey, "error", refundErr)
			}
		}
		return "", err
	}

	s.mu.Lock()
	if creditKey != "" {
		s.creditKeys[jobID] = creditKey
	}
	if recipientUserID != "" {
		s.recipients[jobID] = recipientUserID
	}
	s.mu.Unlock()
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

	if err := s.settleCredit(ctx, job); err != nil {
		return err
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

// settleCredit is NotifyOnCompletion's credit half: Confirm on a succeeded
// job, Refund on a dead-lettered or cancelled one, against the SAME
// reservation Simulate opened for job -- see the package doc comment's
// "Credit accounting" section and Simulate's own doc comment on its
// idempotency-key shape. A no-op when this Service has no CreditService
// wired, or when Simulate never reserved credits for job (job.ID has no
// entry in creditKeys -- e.g. a Service built with credits == nil, or a
// job that predates this round's own credit wiring).
//
// Confirm/Refund are themselves idempotent under retry (CreditService's
// own compare-and-swap contract -- credit_service.go's Confirm doc
// comment), so calling this again for an already-settled job on a later
// poll is safe: it resolves to the existing, already-settled
// CreditTransaction row rather than erroring or double-applying, exactly
// the "provably safe" property Simulate's stable, per-request idempotency
// key exists to guarantee.
func (s *Service) settleCredit(ctx context.Context, job *jobs.Job) error {
	if s.credits == nil {
		return nil
	}
	s.mu.Lock()
	creditKey, hasCredit := s.creditKeys[job.ID]
	s.mu.Unlock()
	if !hasCredit {
		return nil
	}

	// Rebuilt explicitly from job.TenantID rather than trusted from ctx --
	// root CLAUDE.md's "workers do not inherit tenant context" trap,
	// applied defensively here even though every real caller's ctx already
	// carries this exact tenant (go/jobs' own Queue.Get refuses an id
	// outside ctx's tenant, so a caller could not have reached this job's
	// status at all under a different tenant to begin with).
	settleCtx := pkgcore.WithTenant(ctx, job.TenantID)

	var err error
	if job.Status == jobs.StatusSucceeded {
		_, err = s.credits.Confirm(settleCtx, creditKey)
	} else {
		_, err = s.credits.Refund(settleCtx, creditKey)
	}
	if err != nil {
		return fmt.Errorf("smilesim: settle credit reservation for job %q: %w", job.ID, err)
	}
	return nil
}
