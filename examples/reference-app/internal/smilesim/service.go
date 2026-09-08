// Package smilesim is the reference app's small, non-HTTP-generated
// business service that is go/ai-gateway's mandatory first consumer of
// Gateway.GenerateImage (a module API this app genuinely uses): given a
// patient photo the caller already uploaded through
// go/storage, it asks go/ai-gateway for an async AI smile simulation --
// an image-to-image transformation standing in for this
// dental SaaS's own real before/after preview feature.
//
// Like internal/consult (the chat consumer), it deliberately does not
// go through the OpenAPI machinery: ai-gateway itself ships no HTTP surface
// for a spec fragment to grow into. Its three routes (POST
// /api/v1/smile-simulation/simulate, GET
// /api/v1/smile-simulation/jobs/{id}, and GET
// /api/v1/smile-simulation/photos/{photoObjectID}/simulations, the
// per-photo enumeration that is the smile gallery's data source) are
// mounted by hand in cmd/server (cmd/server/smilesim.go), the same pattern
// consult.go and the notification module's own demo patient-message route
// establish in this app.
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
// notifications are event-driven): Simulate
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
// pipeline. It is also bounded, not a log of every job ever simulated:
// NotifyOnCompletion deletes both entries once a job's terminal outcome is
// fully processed, so the maps hold only outstanding (undelivered or
// still-pending) notifications -- see NotifyOnCompletion's own doc comment
// and Service's field comment on mu.
//
// # Credit accounting
//
// This is also the reference app's mandatory first consumer of
// go/billing's CreditService (a module API this app genuinely uses, the
// credit-pack-purchase, credit-reserve/refund and usage-display surface).
// Simulate reserves (PreDeduct) CreditsPerSimulation credits BEFORE ever
// calling Gateway.GenerateImage -- an insufficient balance refuses the
// request with billing.ErrInsufficientCredits and never reaches
// go/ai-gateway at all -- and settleCredit later settles that reservation
// once the job reaches a terminal status: Confirm on StatusSucceeded,
// Refund on StatusDeadLetter/StatusCancelled. The reservation opens only
// when this Service was built with a CreditService, the durable store AND
// the jobs.Queue all wired: the queue is the reconciliation sweep's only
// window onto each job's status, so a service that debits without one
// would open reservations no sweep could ever settle (see Simulate's own
// doc comment on the three-way pair). The gate go/ai-gateway runs before
// any enqueue -- the entitlement check, first among its pre-enqueue
// refusals -- is additionally pre-flighted by Simulate BEFORE the
// reservation opens, so a request the gate refuses is refused with the
// gateway's own coded answer while no PreDeduct/Refund pair has ever been
// written to the ledger for it (see Simulate's own doc comment). Every
// credit-pack-purchase leg (a real Stripe/Alipay/WeChat sandbox charge)
// is deliberately out of scope here -- cmd/server/server.go's
// seedDemoCredits is the Grant-based demo stand-in, since no live
// payment credentials exist in this environment.
//
// # Settlement reachability
//
// A reservation's settlement must not depend on a client happening to
// poll the job-status route to a terminal status: a caller may close its
// tab, lose its network, or simply stop polling once it has the answer it
// wanted, and the process itself may restart while a job is still
// running. Both are real paths to a reservation stuck Reserved forever if
// the only record of "which CreditTransaction belongs to which job" lived
// in an ordinary in-memory map, which is why this Service's credit
// bookkeeping -- unlike recipients/notified just below, an accepted
// in-memory-only limitation for this app's own demo notification glue --
// is NOT in-memory: store (reservation_store.go) durably persists the
// job-id-to-CreditTransaction-idempotency-key mapping the moment Simulate
// opens a reservation, and deletes it the moment settleCredit settles it,
// so the mapping survives a process restart exactly like the jobs.Queue
// row for the job it belongs to already does. On top of that,
// ReconcileOutstandingCredits (reconcile.go) periodically sweeps every
// still-present row, across every tenant, checking each job's current
// status through the same jobs.Queue a real client would poll and
// settling any that have already reached a terminal one --
// StartReconciler wraps that sweep in a ticker loop cmd/server starts once
// at boot, mirroring go/config's own anti-loss poller. NotifyOnCompletion's
// poll-driven call stays the fast path (a client that IS still polling
// observes settlement the instant the job finishes, with no
// sweep-interval delay); the sweep is the net underneath it, not a
// replacement for it.
//
// A Service built with a nil CreditService, a nil store or a nil
// jobs.Queue performs no credit accounting/reconciliation at all -- the
// same nil-is-legal default every other optional host seam in this
// codebase takes when unwired (mirroring bus's own nil-is-legal contract
// just above). The nil queue suppresses Simulate's reservation too, not
// just the sweep: a debit whose settlement would rest on a sweep that
// cannot run is never opened (see Simulate's own doc comment on the
// three-way pair).
//
// # Parameterized simulation options
//
// Simulate accepts an optional, validated option set -- variadic
// SimulateOption helpers (WithSmileStyle/WithToothShade/WithStrength) over
// DefaultSimulationOptions, so a call that names no options gets the
// documented defaults. The effective set is rendered into the vendor
// prompt by prompt.go's renderSimulationPrompt; each dimension's meaning
// and legal vocabulary live on its own type (SmileStyle, ToothShade,
// SimulationOptions.Strength in options.go). An option outside the
// vocabulary or range is refused with a coded smilesim.* error BEFORE any
// credit is reserved and before any job is enqueued -- never silently
// clamped -- with the reason recorded at options.go's Strength bound
// constants (a clamped value would make both the durable record and the
// rendered prompt lie about what the caller asked for on a billed
// operation). The prompt template keeps this product's core promise
// intact: every render ends with the preservation
// sentence verbatim ("Keep the rest of the face, lighting and background
// unchanged."), preceded by the strengthening identity sentence
// (identity, proportions, skin tone, lip color, pose) -- the
// preservation instruction is never weakened, only extended (prompt.go,
// pinned by prompt_test.go).
//
// Calling Simulate again with the SAME photo and the SAME options is an
// explicit NEW generation: a fresh credit charge and a fresh job, never an
// idempotent return of the earlier result. "Regenerate" in this product
// means "draw another variant from the same stochastic vendor call", which
// is exactly what a same-option repeat is for -- deduplicating it would
// remove the one thing the action does -- and each attempt bills honestly
// because each one is a real, new vendor call. Options differing in any
// dimension are of course always a new generation. The choice is pinned by
// TestService_Simulate_SamePhotoSameOptions_IsANewGeneration.
//
// # Per-photo result index
//
// Every Simulate whose enqueue succeeds also durably records, in this app's
// own SQLite (SimulationStore, simulation_store.go's smilesim_simulations
// table), which photo the generation was requested for, which EFFECTIVE
// option set produced it, and which go/ai-gateway job carries its outcome.
// An app-side record is necessary because no go/ module table may grow
// columns for one host's feature and go/jobs' own Queue offers no listing
// API -- the existing job row cannot answer "which simulations were
// generated from photo X, and with which options". The row survives a
// process restart exactly like the job row it points at, and is never
// deleted or updated: it is this package's append-only per-photo index.
// The outcome itself -- live status, and the generated image's output
// object id once the job succeeded -- is deliberately NOT duplicated into
// the record: Service.ListSimulationsByPhoto reads it per record from the
// job through the same jobs.Queue a client polls, so the job row stays the
// single source of truth for status/result and the index carries only what
// no existing record can. The job-status route answers
// Service.OptionsForJob, so a polled result also names the options that
// produced it.
//
// The deliberate no-snapshot choice has one recorded consequence, spelled
// out in ListSimulationsByPhoto's own "Recorded limitation" section: once
// a job's retention passes on the distributed queue, the enumeration can
// no longer read its outcome, and the row is omitted from listings -- the
// row itself is never deleted, but a terminal outcome this index chose
// not to duplicate cannot be presented after the queue that held the only
// copy of it is gone.
//
// # Provider capability assessment (honest record)
//
// The provider behind this service is whatever image provider this app's
// wiring routes smilesim.LogicalModel to -- today cmd/server/server.go
// routes the logical key to an OpenAI-compatible image provider serving the
// vendor model id "dall-e-3" over the provider's /images/edits multipart
// schema (an image-to-image call: the patient photo plus this package's
// rendered prompt, no mask). What is known about facial preservation on
// this path: the prompt is the ONLY preservation mechanism (no mask, no
// face-anchoring pipeline), the provider is reached through a generic
// OpenAI-compatible wire shape rather than a documented
// vendor-specific face-preserving capability, and everything this
// repository can prove offline is about the plumbing (the photo's bytes
// reach the vendor verbatim, the prompt renders correctly, the generated
// bytes come back as a new object) -- none of it about the vendor's actual
// image quality. Nothing is known, from this repository, about how well
// the routed model preserves a real patient's identity, proportions,
// lips, skin tone, lighting or pose under a smile edit, and claims in
// either direction would be fabrication. What a real acceptance run
// against a real provider would need to verify, at minimum: (1) the
// routed model/endpoint genuinely accepts and meaningfully answers an
// edit-shaped request (the "dall-e-3" id is a routing choice this app
// made, not a verified vendor capability on an edit endpoint); (2) on a
// panel of real patient photos, identity preservation -- measured by a
// face-similarity metric between input and output, not by eyeballing one
// image -- plus preservation of skin tone, lip color, lighting and pose,
// and absence of artifacts around the mouth region; and (3) per-option
// behavior, that higher SmileStyle/Strength options change the smile
// without degrading preservation. Whether that requires a dedicated
// face-preserving provider or a masking pipeline (mask the mouth region,
// inpaint the smile only -- a mask path go/ai-gateway's ImageRequest
// already supports via MaskObjectID) is an open product decision, to be
// made on the acceptance run's evidence, not before it.
//
// # Patient and case entities live in internal/cases
//
// This package's per-photo index above is keyed by go/storage photo
// object id only, the natural key because photos are first-class objects
// and a case layer sits ABOVE the photo. internal/cases's package doc
// comment describes the Case record that now groups a clinic-given
// patient (embedded in the case row) with the case's photos: a case
// detail's per-photo simulations come from THIS package's enumeration,
// fetched per photo -- the index above is unmodified and stays the single
// per-photo data source, with the case layer referencing its photos'
// object ids rather than any simulation record directly. The gallery UI
// itself is not built; this index and internal/cases together serve its
// queries.
package smilesim

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	aigateway "github.com/vislake/speed/go/ai-gateway"
	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/jobs"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// LogicalModel is the logical image model key this service asks
// go/ai-gateway's Gateway to route -- never a vendor-specific model id, the
// image-side mirror of consult.LogicalModel's identical rule (see
// aigateway.ImageRequest.Model's own doc comment). The host wires
// aigateway.WithModelRoute for this exact key onto whatever image provider
// should actually answer it (cmd/server/server.go's buildServer).
const LogicalModel = "image:smile-simulation"

// CreditsPerSimulation is the flat credit cost Simulate reserves for one
// smile-simulation request -- a reference-app-level demo business policy,
// not a mechanism go/billing itself prescribes:
// a real deployment would size this per vendor cost, per resolution tier,
// or read it from a go/config item, none of which this app needs to
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
	// settleCredit settles -- see the package doc comment's "Credit
	// accounting" section. Nil is legal: Simulate then performs no credit
	// accounting at all (no PreDeduct call, never a refusal on balance),
	// and settleCredit never calls Confirm/Refund -- the same
	// optional-seam convention go/ai-gateway's own Entitlements option
	// follows for a host that leaves it unwired (it then enforces no
	// quota rather than panicking). cmd/server wires BOTH seams for
	// real -- a real *billing.CreditService here and the
	// WithEntitlements closure over billingModule.Entitlements() there
	// (server.go's own construction call at NewService, judged against
	// the demo subscriptions demo_entitlements.go seeds at boot) -- so
	// the nil states these field comments describe belong to other
	// hosts and to this package's standalone unit tests, never to this
	// app's booted wiring.
	credits *billing.CreditService

	// store durably persists the job-id-to-CreditTransaction-idempotency-key
	// mapping Simulate opens and settleCredit later settles -- see the
	// package doc comment's "Settlement reachability" section. Nil is
	// legal, with the same "no credit accounting at all" consequence as a
	// nil credits.
	store *ReservationStore

	// simulations durably records each generation request's photo, its
	// effective options and its job id -- see the package doc comment's
	// "Per-photo result index" section. Nil is legal: Simulate then
	// records nothing, and ListSimulationsByPhoto/OptionsForJob answer
	// "no record", the same optional-seam convention as store.
	simulations *SimulationStore

	// queue is the jobs.Queue ReconcileOutstandingCredits polls for each
	// outstanding reservation's current job status -- the SAME queue
	// go/ai-gateway's own job handler runs on, never a second queue of
	// this Service's own. Nil is legal: ReconcileOutstandingCredits and
	// StartReconciler are then no-ops, and Simulate performs no credit
	// reservation either -- a debit whose settlement would rest on a sweep
	// that cannot run is never opened (see Simulate's own doc comment on
	// the three-way credits/store/queue pair), mirroring credits and
	// store.
	queue jobs.Queue

	// bus is where NotifyOnCompletion publishes EventSimulationCompleted.
	// Nil is legal: NotifyOnCompletion is then simply a no-op -- the
	// same nil-legal convention the credits field above documents, where
	// an unwired optional dependency enforces nothing rather than
	// panicking (go/ai-gateway's own Entitlements option is the same
	// shape for a host that leaves it unwired). cmd/server's booted
	// Service passes a real one (reg.EventBus(), server.go's own
	// construction call at NewService), so a nil bus, like a nil
	// credits, describes other hosts and standalone unit tests, never
	// this app.
	bus pkgcore.EventBus

	// entitlements is the model-access gate Simulate runs BEFORE it opens
	// any credit reservation -- the same gate go/ai-gateway's own
	// GenerateImage runs inside its pipeline (checkEntitlement: feature
	// key "model:"+LogicalModel, requested 1, refusal answered with
	// aigateway.ErrEntitlementDenied). Nil is legal: Simulate then skips
	// the pre-flight and the gateway's own gate is the only judgment, the
	// same nil-is-unwired convention the gateway's own WithEntitlements
	// option follows. When wired, this MUST be the same seam the gateway
	// was built with (cmd/server passes one closure to both), so the
	// pre-flight and the gateway's own re-check answer the same question;
	// see Simulate's own doc comment on why the gate must run before the
	// reservation, not after it.
	entitlements aigateway.Entitlements

	// mu guards recipients and notified, both keyed by the image job's id
	// -- Simulate writes to recipients, NotifyOnCompletion reads both, and
	// both methods may be called concurrently (a real client polls the
	// job-status route from its own goroutine independent of any other
	// request this process is serving). The credit-settlement mapping
	// (creditKeys) lives durably in store instead of under this lock --
	// see the package doc comment's "Settlement reachability" section
	// for why.
	//
	// The two maps are bounded by outstanding notifications, never by the
	// count of jobs ever simulated: NotifyOnCompletion deletes both
	// entries once a job's terminal outcome has been fully processed -- an
	// accepted publish on the success path, and a terminal job observed
	// by a bus-less Service that can never deliver -- so an entry lives
	// only while its delivery is still pending or in flight (see
	// NotifyOnCompletion's own doc comment).
	mu         sync.Mutex
	recipients map[jobs.JobID]string
	notified   map[jobs.JobID]bool
}

// NewService returns a Service asking gateway for simulations, reserving
// and settling credits against credits (nil is legal -- see Service's own
// doc comment on the credits field), persisting the reservation-settlement
// mapping in store and checking job status for ReconcileOutstandingCredits
// through queue (nil is legal for either -- see Service's own doc comments
// on the store and queue fields), durably recording each generation
// request's photo/options/job mapping in simulations (nil is legal -- see
// Service's own doc comment on the simulations field), publishing
// simulation-completed events on bus (nil is legal -- see Service's own doc
// comment on the bus field), and pre-flighting the model-access gate
// through entitlements before any credit reservation opens (nil is legal --
// Simulate then skips the pre-flight; see Service's own doc comment on the
// entitlements field for why a wired one must be the very seam gateway was
// built with). Constructing one performs no I/O; call each store's own
// EnsureSchema once, separately, before first use (cmd/server's wiring does
// this).
func NewService(gateway *aigateway.Gateway, credits *billing.CreditService, bus pkgcore.EventBus, queue jobs.Queue, store *ReservationStore, simulations *SimulationStore, entitlements aigateway.Entitlements) *Service {
	return &Service{
		gateway:      gateway,
		credits:      credits,
		store:        store,
		simulations:  simulations,
		queue:        queue,
		bus:          bus,
		entitlements: entitlements,
		recipients:   make(map[jobs.JobID]string),
		notified:     make(map[jobs.JobID]bool),
	}
}

// Simulate enqueues one async smile-simulation job over photoObjectID -- an
// existing, completed go/storage object of ctx's own tenant (typically
// uploaded through storage's own HTTP surface just before this call) -- and
// returns immediately with the job's id.
//
// options, when given, adjust the option set the simulation is generated
// with (see the package doc comment's "Parameterized simulation options"
// section); with none given, DefaultSimulationOptions applies. An invalid
// or out-of-range option set is refused with the coded smilesim.* error
// Validate returns (options.go) BEFORE any credit is reserved and before
// any job is enqueued -- see SimulateOption's own doc comment. The rendered
// prompt is prompt.go's renderSimulationPrompt over the effective set; the
// effective set itself (defaults applied) is what the durable per-photo
// record stores, so an outcome always names exactly which options produced
// it.
//
// Calling Simulate again with the same photo and the same options is an
// explicit NEW generation -- a fresh credit charge and a fresh job, per the
// package doc comment's "Parameterized simulation options" section -- so
// there is no deduplication to reason about here: every call is a new
// generation by design.
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
// When this Service was built with a non-nil CreditService, a non-nil
// store AND a non-nil jobs.Queue (see the package doc comment's "Credit
// accounting" section for why the three are a pair), Simulate reserves
// CreditsPerSimulation credits via CreditService.PreDeduct BEFORE calling
// Gateway.GenerateImage at all: an insufficient balance returns
// billing.ErrInsufficientCredits (a coded 409, never a silent fallback)
// and Gateway.GenerateImage -- and therefore go/ai-gateway, and any real
// vendor it might eventually reach -- is never called. A Service built
// with a CreditService but no store, or with both but no jobs.Queue,
// performs NO reservation either: the store's durable row is what both
// settlement paths act on, and the queue is the reconciliation sweep's
// only window onto each job's status -- so a reservation opened without
// that row, or on a service whose sweep can never run, could never be
// settled and would stay Reserved forever. Each missing piece suppresses
// PreDeduct outright rather than letting Simulate charge a tenant for
// credits no mechanism can ever release.
//
// The reservation also opens only AFTER the entitlement pre-flight below:
// the gate go/ai-gateway's GenerateImage runs before any enqueue must
// refuse BEFORE the reservation exists, or a deterministically refused
// request would write a PreDeduct/Refund pair into the ledger for a job
// that never ran. The pre-flight answers the gateway's own question
// (feature key "model:"+LogicalModel, requested 1) through the same
// Entitlements seam the gateway was built with, and a denial is answered
// with the gateway's own coded aigateway.ErrEntitlementDenied -- the
// gateway's own re-check inside GenerateImage then passes for the same
// state, except in a race where the state changed between the two calls,
// which lands in the same refund path any other post-reservation refusal
// takes.
//
// The reservation's idempotency key cannot be the eventual job id: PreDeduct
// must run before GenerateImage even executes, and GenerateImage (via its
// own jobs.Queue.Enqueue) is what mints the job id, so the key does not
// exist yet at the point this method must already have decided whether to
// refuse. Simulate instead mints one fresh, stable id per call
// (uuid.NewString(), prefixed for readability) and reuses that SAME id for
// every subsequent operation tied to this one generation request: it is
// PreDeduct's IdempotencyKey now, and -- once GenerateImage has returned a
// real job id -- store.save durably remembers it (see this package's doc
// comment's "Settlement reachability" section) so a later settleCredit
// call, from either NotifyOnCompletion's poll-driven path or
// ReconcileOutstandingCredits' sweep, settles this exact reservation,
// never a fresh one. This is "stable, tied to the specific generation
// request" in the sense CreditService's own idempotency contract requires
// (PreDeduct.go's doc comment): minted once and reused verbatim by every
// settlement call for this request, never regenerated per call the way
// that would defeat Confirm/Refund's own idempotent-retry contract.
//
// If GenerateImage itself fails AFTER the reservation succeeded (an
// unrouted model, a missing credential, an enqueue failure), the
// reservation is refunded immediately -- there will be no job, and
// therefore no later NotifyOnCompletion call, to settle it otherwise. The
// refund runs on a context.WithoutCancel of the request context, never on
// the request context itself: by the time this refund exists the caller's
// disconnect is already a live possibility (the request may be failing
// precisely because the client went away mid-call), and a client
// disconnect must not be able to kill the compensating refund and strand
// a Reserved reservation that no settlement path can reach. If the refund
// fails for a genuine reason anyway, the reservation is durably recorded
// as an orphaned reservation for ReconcileOutstandingCredits' sweep to
// refund on a later pass (see orphanRefundJobIDPrefix in
// reservation_store.go) -- never left Reserved with the only record of it
// a log line. Either way the failure is logged rather than masking
// GenerateImage's own, more actionable error, exactly the "log and
// swallow, never fail an otherwise-complete operation over a secondary
// side effect" stance recordImageUsage already takes in go/ai-gateway for
// its own usage-reporting side effect.
func (s *Service) Simulate(ctx context.Context, photoObjectID, recipientUserID string, options ...SimulateOption) (jobs.JobID, error) {
	effective := DefaultSimulationOptions()
	for _, apply := range options {
		apply(&effective)
	}
	if err := effective.Validate(); err != nil {
		return "", err
	}

	// The entitlement-gate pre-flight: go/ai-gateway's GenerateImage runs
	// its own checkEntitlement (key "model:"+LogicalModel, requested 1)
	// before it enqueues anything -- but that check happens INSIDE the
	// call, after this method's credit reservation would already have
	// opened. A deterministically refused request must be refused here,
	// before the reservation, with the gateway's own coded answer, so no
	// PreDeduct/Refund pair is ever written to the ledger for a job that
	// never ran. The gateway's own re-check inside GenerateImage answers
	// the same seam, so a request that passes here passes there unless the
	// state changed between the two calls -- a race that lands in the same
	// refund path as any other post-reservation refusal (see Simulate's
	// own doc comment on the reservation's ordering). A nil seam skips the
	// pre-flight entirely, exactly as an unwired gateway gate skips its
	// own check.
	if s.entitlements != nil {
		decision, checkErr := s.entitlements.Check(ctx, "model:"+LogicalModel, 1)
		if checkErr != nil {
			return "", checkErr
		}
		if !decision.Allowed {
			return "", aigateway.ErrEntitlementDenied.WithParam("model", LogicalModel).WithParam("reason", decision.Reason)
		}
	}

	var creditKey string
	// Reserved only when the CreditService, the store AND the queue are
	// ALL wired -- settlement (settleCredit, reached from
	// NotifyOnCompletion's poll and the reconciliation sweep alike) acts
	// only on the store's durable job-id-to-credit-key row, and the sweep
	// that heals a reservation no client ever polls to completion reads
	// each job's status through the queue: a debit on a service whose
	// sweep can never run would rest on clients polling forever. See
	// Simulate's own doc comment on this three-way pair.
	if s.credits != nil && s.store != nil && s.queue != nil {
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
		Prompt:        renderSimulationPrompt(effective),
		InputObjectID: photoObjectID,
	})

	// persistCtx is the request context stripped of its cancellation:
	// context.WithoutCancel keeps every value (the tenant included) and
	// drops only the Done channel. Everything this method still performs
	// after GenerateImage has returned -- the refund compensating a failed
	// enqueue, and the two durable records of the just-enqueued job below
	// -- exists to keep credit and index bookkeeping convergent with a job
	// that is already real (enqueued, or refused after a reservation), so
	// none of it may be killable by an ordinary client disconnect. Running
	// them on the raw request ctx would let a browser that closed, or a
	// network that dropped, fail the refund and strand a Reserved
	// reservation no settlement path could ever reach, or fail the
	// reservation/index rows and leave the enqueued job settleable against
	// no durable mapping.
	persistCtx := context.WithoutCancel(ctx)

	// tenant is always present whenever creditKey != "": PreDeduct above
	// has already succeeded in that case, and it never returns without
	// first resolving ctx's tenant itself (pkgcore.ErrNoTenant otherwise)
	// -- so the ignored ok is safe, never silently defaulting to an empty
	// tenant on a real path. GenerateImage's success path guarantees the
	// same for the saves below (ErrImageRequiresTenant otherwise).
	tenant, _ := pkgcore.TenantFromContext(persistCtx)

	if err != nil {
		if creditKey != "" {
			if _, refundErr := s.credits.Refund(persistCtx, creditKey); refundErr != nil {
				// The immediate refund could not run even on a cancel-free
				// context -- a genuine failure of the billing store itself,
				// not a client disconnect. The reservation must not be left
				// with its only record in this log line: persist it as an
				// orphaned reservation (a synthetic job id under
				// orphanRefundJobIDPrefix; see reservation_store.go) so
				// ReconcileOutstandingCredits' sweep refunds it, on its own
				// long-lived context, at the next pass -- idempotently,
				// since the sweep retries until the refund lands.
				if orphanErr := s.store.save(persistCtx, jobs.JobID(orphanRefundJobIDPrefix+creditKey), tenant, creditKey); orphanErr != nil {
					// Both the refund and the record that would let the
					// sweep retry it failed: nothing further this method can
					// do -- the billing store is down, and so is the store
					// that records what to heal. The reservation stays
					// Reserved until an operator reconciles it by hand from
					// this log line.
					obs.FromContext(ctx).Error("smilesim: refunding the credit reservation after a failed enqueue failed, and recording it for the reconciliation sweep failed too -- it must be reconciled by hand",
						"credit_idempotency_key", creditKey, "refund_error", refundErr, "error", orphanErr)
				} else {
					obs.FromContext(ctx).Warn("smilesim: refunding the credit reservation after a failed enqueue failed -- recorded as an orphaned reservation for the reconciliation sweep to refund",
						"credit_idempotency_key", creditKey, "error", refundErr)
				}
			}
		}
		return "", err
	}

	if creditKey != "" {
		if saveErr := s.store.save(persistCtx, jobID, tenant, creditKey); saveErr != nil {
			// The job is already enqueued and genuinely running at this
			// point -- refusing the call now would strand it with no way
			// for the caller to ever retrieve it either, which is worse
			// than the alternative logged here: this reservation cannot
			// be found by settleCredit later (neither NotifyOnCompletion's
			// poll nor ReconcileOutstandingCredits' sweep has a row to act
			// on), so it stays Reserved until an operator reconciles it
			// by hand from this log line. The write runs on persistCtx --
			// see that context's own comment above -- so a client
			// disconnect cannot be the failure behind this log line: it
			// records a genuine failure of the durable store itself, the
			// narrow, clearly logged failure mode this mechanism exists
			// to surface (see this package's doc comment's "Settlement
			// reachability" section).
			obs.FromContext(ctx).Error("smilesim: persisting the credit reservation for a just-enqueued job failed -- it cannot be automatically settled and must be reconciled by hand",
				"job_id", string(jobID), "credit_idempotency_key", creditKey, "error", saveErr)
		}
	}

	if s.simulations != nil {
		// save resolves the row's tenant from the ctx itself, through the
		// embedded dbkit.Repository (see simulation_store.go's save doc
		// comment) -- and the tenant is always present here for the
		// identical reason the reservation save just above gives:
		// GenerateImage has already succeeded and never returns a job id
		// without first resolving ctx's tenant. The write runs on
		// persistCtx, for the identical reason that context's own comment
		// above gives.
		if saveErr := s.simulations.save(persistCtx, jobID, photoObjectID, effective); saveErr != nil {
			// The same shape as the reservation-save failure logged just
			// above, and the same verdict: the job is already enqueued and
			// running, so refusing the call now would strand it with no
			// way for the caller to ever retrieve it, which is worse than
			// the alternative logged here. The consequence differs -- this
			// generation is missing from the per-photo index
			// (ListSimulationsByPhoto/OptionsForJob answer "no record" for
			// it, so the P3 gallery would not show it) rather than stuck
			// Reserved -- and, as above, the cancel-free context rules a
			// client disconnect out as the cause: the log line records a
			// genuine failure of the durable store itself (see this
			// package's doc comment's "Per-photo result index" section).
			obs.FromContext(ctx).Error("smilesim: persisting the photo/options index row for a just-enqueued job failed -- this generation will be missing from per-photo listings",
				"job_id", string(jobID), "photo_object_id", photoObjectID, "error", saveErr)
		}
	}

	s.mu.Lock()
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
// given a recipient for, and for a job already notified.
//
// "Already notified" is recorded as the latch below, and the latch records
// an ACCEPTED delivery, never an attempted one: it is set, under one
// lock, immediately before the publish is attempted -- so two concurrent
// callers (two overlapping poll requests) can never both publish for the
// same job -- and rolled back when the publish is refused (bus.Publish
// returns an error), so a refused delivery stays retryable by the next
// poll of this job instead of being skipped forever as "already
// notified". That retryability matters because this method's callers log
// and swallow its error: cmd/server's job-status route (this app's one
// caller) must not turn a status read into an error response over the
// notification side channel, so a publish failure is invisible to the
// caller -- and an attempt-only latch would make that invisible failure
// permanent. With the rollback, the very next poll after a transient
// publish failure re-attempts the delivery. (A concurrent caller that
// observed a claim during a failing publish and returned nil loses
// nothing: the claim is rolled back, so some later poll of this job still
// retries.)
//
// Everything this method does after the terminal-status check -- the
// credit settlement and the event publish -- is durable bookkeeping for a
// job that is already terminal, so it runs on a cancel-free derivation of
// ctx, the same context.WithoutCancel boundary Simulate draws around its
// own post-enqueue writes (persistCtx there, below): a client that closes
// its poll tab at this exact moment must not be able to roll back a
// credit settlement (Confirm/Refund are only healed by a later poll or
// the reconciliation sweep) or kill the event publish mid-delivery (a
// refused publish is retryable, but the one subscriber-side dispatch it
// would have driven is not -- see the publish call's own comment). The
// claim-latch and its rollback stay ordinary lock-guarded memory
// operations, deliberately not affected by ctx.
//
// The recipients and notified entries for job are deleted once its
// terminal outcome is fully processed -- on the success path, right after
// an accepted publish, and on the bus-less path, where no delivery can
// ever exist -- so the two maps stay bounded by outstanding
// (undelivered or still-pending) notifications instead of growing with
// every job ever simulated (see Service's field comment on mu).
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

	// persistCtx is ctx stripped of its cancellation, the identical
	// context.WithoutCancel boundary Simulate draws at its own
	// post-enqueue writes (see Simulate's own persistCtx comment): every
	// remaining call here is durable bookkeeping for a job that is already
	// terminal, and none of it may be killable by the client whose poll
	// observed that terminal state disconnecting mid-call.
	persistCtx := context.WithoutCancel(ctx)

	if err := s.settleCredit(persistCtx, job); err != nil {
		return err
	}

	if s.bus == nil {
		// No deliverer is wired (see Service's own doc comment on the bus
		// field): nothing is published and nothing is latched -- with no
		// bus there is no delivery for the latch to record, and the bus is
		// fixed at construction, so no later delivery could redeem a
		// latch set now. The recipient entry is equally dead weight: a
		// bus-less Service can never deliver, so a terminal job's entry is
		// removed rather than kept for a delivery that cannot exist.
		s.mu.Lock()
		delete(s.recipients, job.ID)
		s.mu.Unlock()
		return nil
	}

	s.mu.Lock()
	recipient, hasRecipient := s.recipients[job.ID]
	alreadyNotified := s.notified[job.ID]
	if hasRecipient && !alreadyNotified {
		// Claim the delivery under the lock -- at most one concurrent
		// caller sees !alreadyNotified, so at most one attempts the
		// publish. A refused publish rolls the claim back below.
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

	// The publish runs on persistCtx for the same reason the settlement
	// just above does: this job is terminal, and the publish is the one
	// dispatch attempt this observation drives. A publish refused for a
	// genuine reason stays retryable (the claim rolls back below), but a
	// publish killed by the poller's own disconnect would take the
	// subscriber-side dispatch -- the notification module's enqueue of the
	// delivery job -- down with it, and the subscription logs and swallows
	// that failure, so nothing would ever retry it.
	if err := s.bus.Publish(persistCtx, pkgcore.Event{
		Type:     EventSimulationCompleted,
		TenantID: job.TenantID,
		Payload:  payload,
	}); err != nil {
		// The delivery was not accepted, so the claim must not stand as
		// "already notified" -- see this method's own doc comment for why
		// the notification must stay retryable by a later poll.
		s.mu.Lock()
		delete(s.notified, job.ID)
		s.mu.Unlock()
		return err
	}

	// The delivery is accepted and out -- the success path. Neither entry
	// has a future now (a later poll of this job must not publish again,
	// and there is no recipient to remember for a job whose notification
	// is done), so both are deleted to keep the maps bounded by
	// outstanding notifications rather than by every job ever simulated
	// (see Service's field comment on mu).
	s.mu.Lock()
	delete(s.recipients, job.ID)
	delete(s.notified, job.ID)
	s.mu.Unlock()
	return nil
}

// settleCredit is the credit half both NotifyOnCompletion's poll-driven
// path and ReconcileOutstandingCredits' sweep call: Confirm on a succeeded
// job, Refund on a dead-lettered or cancelled one, against the SAME
// reservation Simulate opened for job -- see the package doc comment's
// "Credit accounting" and "Settlement reachability" sections and
// Simulate's own doc comment on its idempotency-key shape. A no-op when
// this Service has no CreditService or store wired, or when store has no
// outstanding reservation on file for job (e.g. a Service built with
// credits == nil, a job created before this Service's credit wiring was
// built, or a job whose reservation some earlier settleCredit call
// already settled and deleted).
//
// Confirm/Refund are themselves idempotent under retry (CreditService's
// own compare-and-swap contract -- credit_service.go's Confirm doc
// comment), so calling this again for an already-settled job -- a later
// poll, or the reconciliation sweep racing a poll that got there first --
// is safe even in the narrow window between Confirm/Refund succeeding and
// this method's own store.delete call actually removing the row: the
// second caller resolves to the existing, already-settled
// CreditTransaction row rather than erroring or double-applying, exactly
// the "provably safe" property Simulate's stable, per-request idempotency
// key exists to guarantee.
func (s *Service) settleCredit(ctx context.Context, job *jobs.Job) error {
	if s.credits == nil || s.store == nil {
		return nil
	}
	row, hasCredit, err := s.store.get(ctx, job.ID)
	if err != nil {
		return fmt.Errorf("smilesim: look up credit reservation for job %q: %w", job.ID, err)
	}
	if !hasCredit {
		return nil
	}

	// Rebuilt explicitly from job.TenantID rather than trusted from ctx --
	// a worker context never carries tenant by inheritance, applied
	// defensively here even though every real caller's ctx already
	// carries this exact tenant (go/jobs' own Queue.Get refuses an id
	// outside ctx's tenant, so a caller could not have reached this job's
	// status at all under a different tenant to begin with).
	settleCtx := pkgcore.WithTenant(ctx, job.TenantID)

	if job.Status == jobs.StatusSucceeded {
		_, err = s.credits.Confirm(settleCtx, row.CreditKey)
	} else {
		_, err = s.credits.Refund(settleCtx, row.CreditKey)
	}
	if err != nil {
		return fmt.Errorf("smilesim: settle credit reservation for job %q: %w", job.ID, err)
	}

	// The reservation is settled at this point regardless of whether the
	// row can actually be removed below -- Confirm/Refund already
	// committed. A delete failure is logged and swallowed rather than
	// turned into an error this call would report as a settlement
	// failure: Confirm/Refund's own idempotent-retry contract (this
	// method's own doc comment) makes a leftover row harmless, merely
	// re-settled (safely) the next time it is observed.
	if delErr := s.store.delete(ctx, job.ID); delErr != nil {
		obs.FromContext(ctx).Warn("smilesim: credit reservation settled but removing its reservation row failed -- it will be re-settled (safely, idempotently) the next time it is observed",
			"job_id", string(job.ID), "error", delErr)
	}
	return nil
}

// SimulationOutcome is one entry of Service.ListSimulationsByPhoto's answer:
// a simulation generated from one photo, with the effective options that
// produced it and its outcome read live from the job at query time.
type SimulationOutcome struct {
	// JobID is the go/ai-gateway image-generation job's id.
	JobID jobs.JobID

	// PhotoObjectID is the go/storage object id of the photo the
	// simulation was generated from -- the enumeration's grouping key,
	// echoed per entry for a caller rendering a row on its own.
	PhotoObjectID string

	// Options is the effective option set that produced this simulation,
	// durably recorded at request time (see the package doc comment's
	// "Per-photo result index" section).
	Options SimulationOptions

	// Status is the job's LIVE status, read from the queue at call time --
	// never a snapshot stored by this package, so an enumeration always
	// reflects what a job-status poll would answer right now.
	Status jobs.Status

	// OutputObjectID is the generated image's go/storage object id, set
	// only when Status is StatusSucceeded and the job's own result names
	// one.
	OutputObjectID string

	// Error is the job's recorded failure message, set only when the job
	// carries one (dead-lettered, cancelled after a failed attempt, or
	// retrying).
	Error string

	// CreatedAt is when the generation was requested.
	CreatedAt time.Time
}

// OptionsForJob returns the effective option set durably recorded for job,
// and whether a record exists at all -- false with a nil error means this
// Service never recorded one for a job under ctx's own tenant (the job
// was created before the per-photo index existed, Simulate was called on
// a Service built with a nil simulations store, or the row belongs to
// another tenant). The lookup is tenant-scoped: it answers only for
// ctx's own tenant, mirroring jobs.Queue.Get's own scoping so a caller
// that could not poll the job could not learn its options either.
//
// It is how the job-status route attaches the producing options to a
// polled result -- see the package doc comment's "Per-photo result index"
// section.
func (s *Service) OptionsForJob(ctx context.Context, jobID jobs.JobID) (SimulationOptions, bool, error) {
	if s.simulations == nil {
		return SimulationOptions{}, false, nil
	}
	// get resolves the tenant from the ctx itself (its tenant-scoped
	// lookup filters by the ctx tenant, injected by dbkit's plugin -- see
	// simulation_store.go's get doc comment); the guard below keeps this
	// method's own contract of answering "no record" for a ctx without
	// one rather than surfacing an error.
	if _, ok := pkgcore.TenantFromContext(ctx); !ok {
		return SimulationOptions{}, false, nil
	}
	row, has, err := s.simulations.get(ctx, jobID)
	if err != nil {
		return SimulationOptions{}, false, err
	}
	if !has {
		return SimulationOptions{}, false, nil
	}
	opts, err := row.options()
	if err != nil {
		return SimulationOptions{}, false, fmt.Errorf("smilesim: decode recorded options for job %q: %w", jobID, err)
	}
	return opts, true, nil
}

// ListSimulationsByPhoto returns every simulation generated from
// photoObjectID under ctx's own tenant -- the P3 gallery's data source --
// newest first. Each outcome's Status/OutputObjectID/Error are read LIVE
// from the job through the same jobs.Queue a client polls (per row, under
// the job's own rebuilt tenant context, since worker contexts never carry
// tenant by inheritance), so the job row stays the single source of truth
// for the outcome
// and this index carries only what no existing record can: the
// photo-to-generation mapping and the options (see the package doc
// comment's "Per-photo result index" section).
//
// # Recorded limitation: the queue's own retention
//
// A row whose job the queue no longer has on file is OMITTED from the
// enumeration rather than failing it -- one aged-out simulation must not
// take its whole photo's album down with it (a 404 on the listing route).
// The distributed queue deletes a completed task once its retention
// window passes (go/jobs/queue/asynq's DefaultCompletedRetention, 24
// hours), after which the job row -- and with it the only copy of the
// outcome this package does not duplicate -- is gone: an aged-out job is
// by definition one that already reached a terminal status, and this
// index deliberately stores no terminal-outcome snapshot (see the package
// doc comment's "Per-photo result index" section for why status and
// result stay in the job row), so nothing truthful remains to list for
// it. The row itself is never deleted -- the durable record of "this
// generation was requested" survives -- but the live enumeration cannot
// present an outcome it can no longer read. The standalone queue this
// app boots on retains completed jobs indefinitely, so the omission only
// ever manifests under a distributed composition. A job read that fails
// for any OTHER reason -- the queue itself being down, say -- still
// fails the whole enumeration: an outage is transient where retention
// is permanent, and an empty album must not be the answer to a queue
// that is merely unreachable.
//
// ctx must carry a tenant; one without refuses with pkgcore.ErrNoTenant
// rather than listing across tenants. A Service built with a nil
// simulations store or a nil queue -- the optional-seam convention this
// package's other methods follow -- answers an empty list, mirroring
// ReconcileOutstandingCredits' own nil-wiring no-op.
func (s *Service) ListSimulationsByPhoto(ctx context.Context, photoObjectID string) ([]SimulationOutcome, error) {
	if s.simulations == nil || s.queue == nil {
		return nil, nil
	}
	if _, ok := pkgcore.TenantFromContext(ctx); !ok {
		return nil, pkgcore.ErrNoTenant
	}

	rows, err := s.simulations.listByPhoto(ctx, photoObjectID)
	if err != nil {
		return nil, err
	}

	outcomes := make([]SimulationOutcome, 0, len(rows))
	for _, row := range rows {
		opts, optErr := row.options()
		if optErr != nil {
			return nil, fmt.Errorf("smilesim: decode recorded options for job %q: %w", row.JobID, optErr)
		}

		// Rebuilt from the row's own stored tenant, never trusted from ctx
		// -- a worker context never carries tenant by inheritance, applied
		// the way ReconcileOutstandingCredits already does. (The
		// row's tenant is ctx's own by construction -- listByPhoto only
		// ever returns ctx's own tenant's rows, filtered by dbkit's
		// tenant-scoping plugin -- so the rebuild is belt-and-braces, not
		// a behavior difference.)
		job, getErr := s.queue.Get(pkgcore.WithTenant(ctx, pkgcore.TenantID(row.TenantID)), jobs.JobID(row.JobID))
		if getErr != nil {
			// A job the queue no longer has -- its retention passed -- is
			// omitted per this method's own "Recorded limitation" section;
			// any other read failure still fails the whole enumeration.
			if !isJobNotFound(getErr) {
				return nil, fmt.Errorf("smilesim: fetch status for recorded simulation job %q: %w", row.JobID, getErr)
			}
			continue
		}
		if job == nil {
			// A queue answer of (nil, nil) for a recorded job id is the
			// same "no longer on file" state as a not-found error (the
			// conformance contract answers ErrJobNotFound, but the omission
			// must not depend on which spelling a queue implementation
			// chose).
			continue
		}

		outcome := SimulationOutcome{
			JobID:         job.ID,
			PhotoObjectID: row.PhotoObjectID,
			Options:       opts,
			Status:        job.Status,
			Error:         job.Error,
			CreatedAt:     row.CreatedAt,
		}
		if job.Status == jobs.StatusSucceeded && job.Result != nil {
			var result aigateway.ImageJobResult
			if decErr := json.Unmarshal(job.Result.Data, &result); decErr != nil {
				return nil, fmt.Errorf("smilesim: decode succeeded job %q's image result: %w", row.JobID, decErr)
			}
			outcome.OutputObjectID = result.OutputObjectID
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}

// isJobNotFound reports whether err is, or wraps, a jobs.ErrJobNotFound
// answer -- matched on the error's code rather than errors.Is, because
// apperr-derived answers (WithParam/WithCause) are new *apperr.Error
// values whose chain does not carry the original sentinel (the same
// matching rule go/jobs' own tests apply).
func isJobNotFound(err error) bool {
	appErr, ok := apperr.As(err)
	if !ok {
		return false
	}
	return appErr.Code == jobs.ErrJobNotFound.Code
}
