package app

// This file runs the host's two post-Bootstrap assembly steps.
// runPostBootstrap executes after the kernel bootstrapped the module set: the
// typed Attach calls every module requires exactly once after Bootstrap, the
// host steps interleaved with them (the channel flags, the demo seeds, the
// rbac hand-off), and the platform credentials the gateway's write path
// needs. runPostAttach executes after that: the stores and services whose
// schemas must exist before the first request, the job queue's wiring onto
// the registry's declared handlers, and the modules' own background pipeline
// starts.

import (
	"context"
	"fmt"
	"io"
	"os"

	aigateway "github.com/vislake/speed/go/ai-gateway"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/examples/reference-app/internal/app/demo"
	"github.com/vislake/speed/examples/reference-app/internal/cases"
	"github.com/vislake/speed/examples/reference-app/internal/notes"
	"github.com/vislake/speed/examples/reference-app/internal/smilesim"
)

// SocialChannelFlagKey maps a configured social provider's Name() to the
// feature-flag key go/authn gates that channel under -- the host-side
// mirror of go/authn/identity.go's own unexported socialChannelFlag
// mapping, which this app cannot call. It returns "" for a provider authn
// does not gate: such a provider has no flag for an operator to have
// turned off, so there is nothing for this host to open, and
// openConfiguredAuthnChannels must skip it (writing an unknown key would
// be a config ErrUnknownKey, since the schema knows only declared flags).
func SocialChannelFlagKey(name string) string {
	switch name {
	case authn.ProviderGoogle:
		return authn.FeatureFlagSocialGoogle
	case authn.ProviderGitHub:
		return authn.FeatureFlagSocialGitHub
	case authn.ProviderWeChat:
		return authn.FeatureFlagSocialWeChat
	case authn.ProviderDingTalk:
		return authn.FeatureFlagSocialDingTalk
	case authn.ProviderFeishu:
		return authn.FeatureFlagSocialFeishu
	default:
		return ""
	}
}

// openConfiguredAuthnChannels opens, at the config system tier, every
// social sign-in channel this host assembled through cfg.SocialProviders.
//
// It exists because of a deliberate asymmetry in go/authn's declared flag
// defaults: authn.password_login and authn.sms_login default ON, while the
// five social flags default OFF -- go/authn/module.go: a channel with no
// credentials configured must not appear on the login page, so a flag that
// defaulted on would advertise a channel whose unconfigured deployment
// would fail it at the provider. A deployment that configures credentials
// (here: that assembles the provider through ServerConfig, its own
// out-of-band composition channel) is therefore expected to turn that
// channel's own flag on, and this step is how THIS host does that, for
// exactly the channels it actually wired. A system-tier row is the right
// tier: pre-auth reads -- authn's own channel gates and the login page's
// /api/v1/config/features answer -- carry no tenant and resolve from the
// system row down, so every request sees the same answer, while a
// tenant-tier row can still narrow a channel for one tenant.
//
// The step must run after Bootstrap: a system-tier write needs the audited
// system context under config.SystemPurposeSystemWrite, a purpose config's
// own Register declares during Bootstrap, and the schema the write
// validates against is frozen by configModule.Attach. Running here, before
// any route can serve, makes the page/API agreement hold from the very
// first request: without the gate wired into authn, config rows could hide
// a channel on the page while its endpoint kept issuing tokens; without
// this step, the wired gate would refuse a channel this host genuinely
// configured.
//
// No providers assembled means no rows are written at all, and the schema
// defaults alone keep password and SMS sign-in open. The writes are
// unconditional per boot (each Set upserts its row to true), matching the
// ai-gateway boot credential write and the seedDemo* steps: this app's
// composition declares its channels, and a restart re-affirms that
// declaration. Tenant-tier rows are never touched, so a tenant's own
// narrower choice survives every restart.
func openConfiguredAuthnChannels(ctx context.Context, cfgService *config.Service, providers []authn.SocialProvider) error {
	if len(providers) == 0 {
		return nil
	}
	// The bare pkgcore.WithSystemContext is deliberate here rather than
	// tenancy.WithSystemContext, the audited wrapper: this is a boot-time
	// declaration, run once per process start between Bootstrap and the
	// first served request, re-affirming host configuration under the fixed
	// "reference-app-boot" actor -- no operator session, request or ticket
	// exists yet to attribute an audit row to, and a row every restart
	// re-creates identically answers no question an audit reader asks. The
	// audited wrapper exists for request-time paths, where the bus is live
	// and the grant is an attributable action; code copying this
	// boot-time pattern must state the same precondition.
	sysCtx, err := pkgcore.WithSystemContext(ctx, pkgcore.SystemReason{
		Actor:   "reference-app-boot",
		Purpose: config.SystemPurposeSystemWrite,
	})
	if err != nil {
		return fmt.Errorf("reference-app: build the authn channel system context: %w", err)
	}
	for _, provider := range providers {
		flag := SocialChannelFlagKey(provider.Name())
		if flag == "" {
			continue
		}
		if setErr := cfgService.Set(sysCtx, config.ScopeSystem, flag, config.Value{Data: true}, "reference-app-boot"); setErr != nil {
			return fmt.Errorf("reference-app: open authn channel %q: %w", flag, setErr)
		}
	}
	return nil
}

// runPostBootstrap runs the post-bootstrap assembly step: the typed Attach
// calls and the host steps interleaved with them. The transition's
// PostBootstrap hook and the host's post-bootstrap component both call it,
// so the option surface and the component assembly drive one body.
//
// The Attach calls follow pkgcore.Kernel.Bootstrap's post-bootstrap contract:
// each exactly once, after Bootstrap has returned, and each before the first
// step that consumes its Service -- the registry's declaration seats are only
// complete once every module registered. This app's sequence interleaves host
// steps between the attaches (the channel flags, the seeds, the rbac
// hand-off), which is why it is written out step by step; each step's own
// comment carries its ordering rationale.
//
// The Attach calls, and the binding check before them, need the module
// Registry the assembly bootstrapped (view.modules): they are the module
// world's own surface, and they are skipped on a view built from a component
// registry, where the same Services are component products.
func (b *serverBuild) runPostBootstrap(ctx context.Context, view assemblyView) error {
	if view.modules != nil {
		// The bootstrap binding check: the loader target must bind the keys
		// the composed modules declared, and every key this app owns
		// besides. A declared key the target never resolves is a key whose
		// contract silently binds to nothing.
		if err := verifyBootstrapBinding(view.modules); err != nil {
			return err
		}
	}

	// The org-backed half of the membership store binds here, only now that
	// Bootstrap has run: the store's enumeration answer delegates to org's
	// own cross-tenant query, which org serves only an elevated context --
	// the grant authn's resolveTenant takes under its own
	// SystemPurposeSignInTenantEnumeration before it calls TenantsOf, and
	// which authn's Register declared during the Bootstrap above. Nothing
	// before this point can serve a sign-in: authn answers membership
	// questions only inside a Login or Refresh call.
	b.memberships.attach(b.orgModule.Members())

	if view.modules != nil {
		// integration's Attach must run after Bootstrap for the same reason
		// config's and rbac's do: its Service reads reg.Events.Bus() and
		// reg.AuditActions, which Bootstrap only finishes wiring once every
		// module's Register call has returned. Its ordering relative to the
		// other attaches is not load-bearing. The return value is discarded:
		// every spec-generated surface the module mounts reads the Service at
		// call time through Handler's own Register-time forwarding wrapper.
		if _, attachErr := b.integrationModule.Attach(view.modules); attachErr != nil {
			return fmt.Errorf("reference-app: attach the integration module: %w", attachErr)
		}
		configService, err := b.configModule.Attach(view.modules)
		if err != nil {
			return fmt.Errorf("reference-app: attach the config module: %w", err)
		}
		b.configService = configService
	}

	// With the config service live, open the sign-in channels this host
	// actually assembled: authn's social flags default OFF, so a provider
	// this app wired through cfg.SocialProviders would otherwise refuse with
	// authn.channel_disabled the moment the gate wired into authn became
	// real -- and the login page, which reads the same system-tier rows
	// through /api/v1/config/features, would agree that the channel does not
	// exist (openConfiguredAuthnChannels' own doc comment has the full
	// reasoning). OnConfigReady, when non-nil, receives the live service now
	// that those rows are in place -- the post-Attach seam a test needs to
	// write further rows through the real Set path.
	if flagErr := openConfiguredAuthnChannels(ctx, b.configService, b.cfg.SocialProviders); flagErr != nil {
		return flagErr
	}
	if b.cfg.OnConfigReady != nil {
		b.cfg.OnConfigReady(b.configService)
	}

	// rbac's Attach must also come after Bootstrap, and for a sharper
	// reason: what it freezes is the snapshot of every permission every
	// module declared, so a snapshot taken any earlier would be missing
	// whatever registered after it -- and a permission missing from that
	// catalog cannot be granted at all.
	if view.modules != nil {
		rbacService, err := b.rbacModule.Attach(view.modules)
		if err != nil {
			return fmt.Errorf("reference-app: attach the rbac module: %w", err)
		}
		b.rbacService = rbacService
	}
	if seedErr := demo.SeedDemoGrants(ctx, b.rbacService, b.cfg.HostTenants); seedErr != nil {
		return seedErr
	}
	if b.cfg.OnRBACReady != nil {
		b.cfg.OnRBACReady(b.rbacService)
	}

	// SeedDemoCredits is the demo, NOT-a-real-payment stand-in for a real
	// buy-a-credit-pack flow. It runs unconditionally, regardless of
	// cfg.DemoUsersPassword: the smilesim suite needs a real, non-zero
	// starting balance on tenant-acme to exercise the successful-generation
	// leg, exactly the way SeedDemoGrants' roles are needed by every test
	// that gates a route on a permission, demo password or not.
	if seedErr := demo.SeedDemoCredits(ctx, b.billingModule.Credits(), b.cfg.HostTenants); seedErr != nil {
		return seedErr
	}

	// SeedDemoEntitlements is the demo, NOT-a-real-purchase stand-in for
	// the "tenant buys a subscription" leg of a real billing flow. It runs
	// unconditionally: the gateway is wired with WithEntitlements, so every
	// consult/smilesim request is gated on the calling tenant holding an
	// Active subscription to a Plan granting that route's model key --
	// without this seed the demo tenants would have none and every demo AI
	// route would answer aigateway.entitlement_denied. Running it here,
	// before any route can serve, also means the seam is live (never nil
	// and never judging an empty database) from the very first request.
	if seedErr := demo.SeedDemoEntitlements(ctx, b.billingModule.Plans(), b.billingModule.Subscriptions(), b.cfg.HostTenants); seedErr != nil {
		return seedErr
	}

	// notes' retention participant is registered here, after Bootstrap --
	// compliance's Register is what attaches the Retention registrar the
	// kernel's Retention seat resolves to, so Add before Bootstrap would
	// silently register onto a registrar nothing sweeps with. The
	// participant is built over the very dbkit.Open *gorm.DB the notes
	// Module already uses -- share the connection, never a second pool.
	if err := view.retention.Add(notes.NewRetentionParticipant(notes.NewRepository(b.db))); err != nil {
		return fmt.Errorf("reference-app: register notes retention participant: %w", err)
	}

	// admin's role-management surface needs the real *rbac.Service, which
	// exists only after Bootstrap has returned. This is why go/admin's
	// RoleService is wired through a distinct, post-Bootstrap
	// Module.AttachRBAC call rather than a construction-time Option -- see
	// AttachRBAC's own doc comment for the full reasoning.
	b.adminModule.AttachRBAC(b.rbacService)
	return nil
}

// runPostAttach runs the post-attach assembly step. It runs after the typed
// attaches and before the HTTP face: the stores whose schemas must exist
// before the first request, the services built over them, the gateway's
// boot-time credentials, the job queue's wiring onto the registry's declared
// handlers, and metering's background pipelines. The transition's PostAttach
// hook and the host's post-attach component both call it.
func (b *serverBuild) runPostAttach(ctx context.Context, view assemblyView) error {
	// The attestation layer's two boot steps run here, after Bootstrap (the
	// pki migrations the CA chain's pki_authorities rows live in were
	// applied there) and before any request can reach the surfaces that
	// attest or gate: EnsureSchema creates the app table, and
	// EnsureAuthorityChain creates -- once per database, idempotently, by
	// the chain's fixed subject names -- the app's root and issuing
	// intermediate authorities. A failure stops the boot: an app whose AI
	// outputs cannot be attested must not start serving shares of them.
	if err := b.attestationService.EnsureSchema(ctx); err != nil {
		return fmt.Errorf("reference-app: ensure attestation schema: %w", err)
	}
	if err := b.attestationService.EnsureAuthorityChain(ctx); err != nil {
		return fmt.Errorf("reference-app: ensure the attestation CA chain: %w", err)
	}

	// smileSimReservationStore shares this app's own db connection and gets
	// its own tiny table created imperatively via EnsureSchema, mirroring
	// go/jobs.StandaloneQueue's own "create the persistence schema if it
	// does not already exist" pattern rather than joining the migration
	// registry (reservation_store.go's own doc comment). The simulation
	// store is the per-photo result index, same EnsureSchema-before-first-
	// use shape.
	smileSimReservationStore := smilesim.NewReservationStore(b.db)
	if err := smileSimReservationStore.EnsureSchema(ctx); err != nil {
		return fmt.Errorf("reference-app: ensure smilesim credit reservation schema: %w", err)
	}
	smileSimulationStore := smilesim.NewSimulationStore(b.db)
	if err := smileSimulationStore.EnsureSchema(ctx); err != nil {
		return fmt.Errorf("reference-app: ensure smilesim simulation index schema: %w", err)
	}

	// The last argument is gatewayEntitlements -- the same adapter instance
	// aiGatewayModule's WithEntitlements gate runs -- so Simulate can
	// pre-flight the model-access gate before its credit reservation opens.
	b.smileSimService = smilesim.NewService(b.aiGatewayModule.Gateway(), b.billingModule.Credits(), view.bus, b.standaloneQueue, smileSimReservationStore, smileSimulationStore, b.gatewayEntitlements)

	// The case domain's repository shares this app's own db connection and
	// creates its two tiny tables imperatively via EnsureSchema -- the same
	// CREATE TABLE IF NOT EXISTS pattern the smilesim stores above use.
	b.caseRepository = cases.NewRepository(b.db)
	if err := b.caseRepository.EnsureSchema(ctx); err != nil {
		return fmt.Errorf("reference-app: ensure cases schema: %w", err)
	}

	// The ai-gateway platform credentials: written only when the matching
	// API key is set (an empty default is the zero-setup posture). Both
	// must run after Bootstrap, because aiGatewayModule.Register is what
	// calls pkgcore.RegisterSystemPurpose(aigateway.SystemPurposeCredentialWrite)
	// -- WithSystemContext below refuses an unregistered purpose. The bare
	// pkgcore.WithSystemContext rather than the audited
	// tenancy.WithSystemContext is deliberate, for this boot-time
	// precondition: the writes are config-driven declarations re-affirmed
	// identically on every restart under the fixed "reference-app-boot"
	// actor, with no operator session or ticket to attribute an audit row
	// to, so no audit reader has a question this grant's record would
	// answer -- the request-time platform-credential path in ai-gateway's
	// own handler.go, by contrast, must and does go through the audited
	// wrapper.
	if err := b.setPlatformCredentials(ctx); err != nil {
		return err
	}

	// Wire the queue to the registry's declarations. Only now -- after
	// Bootstrap -- can the wiring run: the modules' Register calls declared
	// the handlers on the registry, and jobs.Wire drains that map onto
	// standaloneQueue, refusing an entry that is not a jobs.Handler rather
	// than mis-typing it into a worker at job-claim time, and creating the
	// queue's own tables so an Enqueue needs no Start first. The pool
	// itself starts with the worker, in the engine's stage 8.
	if err := jobs.Wire(ctx, b.standaloneQueue, view.jobs); err != nil {
		return fmt.Errorf("reference-app: wire the job queue: %w", err)
	}

	// Start meteringModule's background pipelines now that Bootstrap has
	// returned (Register attached the registry's bus onto its Aggregator,
	// and Start itself must not run before that -- go/metering/module.go's
	// own New/Start split). This is what makes the ai-gateway UsageRecorder
	// wiring real: without the recorder's flush loop running, a recorded
	// Chat call's event would sit in the analytics buffer forever instead
	// of folding into the metering_usage_summaries rows admin's dashboard
	// reads. Unlike the queue, this is not gated on DisableQueueWorker --
	// that switch governs the jobs.Queue worker only, and metering's own
	// two loops are independent of it. Start is safe to call with ctx
	// canceled or after a Stop.
	b.meteringModule.Start(ctx)

	// The credit-reservation reconciler starts last: its sweep calls
	// standaloneQueue.Get and the database (through its own reservation
	// store), so it must start only once jobs.Wire has created the queue's
	// own tables. context.Background(), never ctx, per StartReconciler's
	// own doc comment: the sweep must keep running until the worker's stop
	// call, not be cut short by whatever cancels the assembly context.
	b.smileSimReconcilerStop = b.smileSimService.StartReconciler(context.Background(), 0)
	return nil
}

// setPlatformCredentials writes the platform-wide ai-gateway credentials the
// configured API keys name -- the chat credential for
// aigateway.ProviderOpenAICompatible and the image credential for
// aigateway.ProviderOpenAICompatibleImage, each when its own key is set.
// The two credentials are deliberately independent rows of the SAME
// ai_gateway_credentials table (keyed by provider name), so chat and image
// credentials coexist with no schema change.
func (b *serverBuild) setPlatformCredentials(ctx context.Context) error {
	for _, credential := range []struct {
		provider string
		apiKey   string
		baseURL  string
	}{
		{aigateway.ProviderOpenAICompatible, b.cfg.AIGatewayAPIKey, b.cfg.AIGatewayBaseURL},
		{aigateway.ProviderOpenAICompatibleImage, b.cfg.AIGatewayImageAPIKey, b.cfg.AIGatewayImageBaseURL},
	} {
		if credential.apiKey == "" {
			continue
		}
		if credential.baseURL == "" {
			return fmt.Errorf("reference-app: the %s platform credential has an API key but no base URL", credential.provider)
		}
		sysCtx, sysErr := pkgcore.WithSystemContext(ctx, pkgcore.SystemReason{
			Actor:   "reference-app-boot",
			Purpose: aigateway.SystemPurposeCredentialWrite,
		})
		if sysErr != nil {
			return fmt.Errorf("reference-app: build the %s credential system context: %w", credential.provider, sysErr)
		}
		if credErr := b.aiGatewayModule.Credentials().SetPlatformCredential(
			sysCtx, credential.provider, credential.apiKey, credential.baseURL,
		); credErr != nil {
			return fmt.Errorf("reference-app: set the %s platform credential: %w", credential.provider, credErr)
		}
	}
	return nil
}

// osStderr is the console SMS fallback's sink resolution helper: an unset
// ServerConfig.SMSOutput writes to stdout, the same zero-setup default the
// authn and notification senders share.
func smsOutputFor(cfg ServerConfig) io.Writer {
	if cfg.SMSOutput != nil {
		return cfg.SMSOutput
	}
	return os.Stdout
}
