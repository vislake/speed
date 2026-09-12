package app

// This file runs the host's two post-assembly assembly steps.
// runPostBootstrap executes after the engine assembled the module set: the
// typed Attach calls every module requires exactly once after the assembly, the
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
// The step must run after the assembly: a system-tier write needs the
// system context under config.SystemPurposeSystemWrite, a purpose the
// config component declares in its descriptor -- registered at the
// assembly's Init entry, before this step's own Init callback runs -- and
// the schema the write validates against is frozen by configModule.Attach.
// Running here, before any route can serve, makes the page/API agreement
// hold from the very first request: without the gate wired into authn,
// config rows could hide a channel on the page while its endpoint kept
// issuing tokens; without this step, the wired gate would refuse a channel
// this host genuinely configured.
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
	// declaration, run once per process start between the assembly and the
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
// calls every module requires exactly once after its declaration turn, the
// host steps interleaved with them (the channel flags, the seeds, the rbac
// hand-off), and the platform credentials the gateway's write path needs.
// The host's post-bootstrap component calls it.
//
// It runs in the assembly's Init stage, after every module's own Init
// callback has declared: the components registering only takes the
// declaration; the three freeze points -- config's schema snapshot, rbac's
// permission catalog and admin's authorizer -- are taken here, so each
// covers every module's declarations, which is the order the module
// Registry always drove. This app's sequence is written out step by step;
// each step's own comment carries its ordering rationale.
func (b *serverBuild) runPostBootstrap(ctx context.Context, reg *pkgcore.ComponentRegistry, view assemblyView) error {
	// The org-backed half of the membership store binds here, only now that
	// every module has declared: the store's enumeration answer delegates to
	// org's own cross-tenant query, which org serves only an elevated
	// context -- the grant authn's resolveTenant takes under its own
	// SystemPurposeSignInTenantEnumeration before it calls TenantsOf, and
	// which authn's Register declared above. Nothing before this point can
	// serve a sign-in: authn answers membership questions only inside a
	// Login or Refresh call.
	b.memberships.attach(b.orgModule.Members())

	// config's schema snapshot: every module's configuration items and
	// feature flags are declared by now, so the schema the writes below (and
	// every later read) validate against is complete. The service is
	// published for the by-type context, where the app component's face
	// composition and the later steps read it.
	configService, err := b.configModule.Attach(reg)
	if err != nil {
		return fmt.Errorf("reference-app: attach the config module: %w", err)
	}
	reg.Put(configService)
	b.configService = configService

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
	if flagErr := openConfiguredAuthnChannels(ctx, configService, b.cfg.SocialProviders); flagErr != nil {
		return flagErr
	}
	if b.cfg.OnConfigReady != nil {
		b.cfg.OnConfigReady(configService)
	}

	// rbac's permission catalog snapshot comes after every module's
	// declaration turn, so it covers the whole permission catalog -- a
	// permission declared after the snapshot could never be granted at all.
	rbacService, err := b.rbacModule.Attach(reg)
	if err != nil {
		return fmt.Errorf("reference-app: attach the rbac module: %w", err)
	}
	reg.Put(rbacService)
	b.rbacService = rbacService

	if seedErr := demo.SeedDemoGrants(ctx, rbacService, b.cfg.HostTenants); seedErr != nil {
		return seedErr
	}
	if b.cfg.OnRBACReady != nil {
		b.cfg.OnRBACReady(rbacService)
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

	// notes' retention participant is registered here, after the assembly --
	// compliance's Register is what attaches the Retention registrar the
	// assembly's Retention seat resolves to, so Add before the assembly would
	// silently register onto a registrar nothing sweeps with. The
	// participant is built over the very dbkit.Open *gorm.DB the notes
	// Module already uses -- share the connection, never a second pool.
	if err := view.retention.Add(notes.NewRetentionParticipant(notes.NewRepository(b.db))); err != nil {
		return fmt.Errorf("reference-app: register notes retention participant: %w", err)
	}

	// admin's role-management surface needs the real *rbac.Service. The
	// admin component's own Start callback binds it (AttachRBAC), ordered
	// after rbac's Start by the descriptor's requires edge on rbac's
	// product -- the service is published and its catalog complete there,
	// so no host-side wiring call is needed.
	return nil
}

// runPostAttach runs the post-attach assembly step. It runs after the
// post-bootstrap step and before the HTTP face: the stores whose schemas
// must exist before the first request, the services built over them and the
// gateway's boot-time credentials. metering's background pipelines are its
// own component's Start, and the job queue's wiring onto the declared
// handlers is the queue component's own Start. The host's post-attach
// component calls it.
func (b *serverBuild) runPostAttach(ctx context.Context, view assemblyView) error {
	// The attestation layer's boot step runs here, after the assembly (the
	// attestation module's own migrations -- the attestations table -- and
	// the pki migrations the CA chain's pki_authorities rows live in were
	// applied there) and before any request can reach the surfaces that
	// attest or gate: EnsureAuthorityChain creates -- once per database,
	// idempotently, by the chain's fixed subject names -- the app's root
	// and issuing intermediate authorities. A failure stops the boot: an
	// app whose AI outputs cannot be attested must not start serving shares
	// of them.
	if err := b.attestationService.EnsureAuthorityChain(ctx); err != nil {
		return fmt.Errorf("reference-app: ensure the attestation CA chain: %w", err)
	}

	// The two smilesim stores share this app's own db connection; their
	// tables are the smilesim component's migrations, applied at the
	// assembly's Verify stage, so this step constructs the stores only:
	// the reservation store is the durable credit-reservation ledger and
	// the simulation store the per-photo result index.
	smileSimReservationStore := smilesim.NewReservationStore(b.db)
	smileSimulationStore := smilesim.NewSimulationStore(b.db)

	// The last argument is gatewayEntitlements -- the same adapter instance
	// aiGatewayModule's WithEntitlements gate runs -- so Simulate can
	// pre-flight the model-access gate before its credit reservation opens.
	b.smileSimService = smilesim.NewService(b.aiGatewayModule.Gateway(), b.billingModule.Credits(), view.bus, b.standaloneQueue, smileSimReservationStore, smileSimulationStore, b.gatewayEntitlements)

	// The case domain's repository shares this app's own db connection; its
	// two tables are the cases component's migrations, applied at the
	// assembly's Verify stage.
	b.caseRepository = cases.NewRepository(b.db)

	// The ai-gateway platform credentials: written only when the matching
	// API key is set (an empty default is the zero-setup posture).
	// WithSystemContext below accepts aigateway.SystemPurposeCredentialWrite
	// because the ai-gateway component declares it in its descriptor, and
	// the assembler registers every declared purpose at the Init stage's
	// entry -- before any Init callback, this step's own included, runs.
	// The bare pkgcore.WithSystemContext rather than the audited
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

	// The credit-reservation reconciler starts last: its sweep calls the
	// queue's Get and the database (through its own reservation store), so
	// it must start only once the queue's own tables exist -- which the
	// queue component's Start created before this assembly's Init-stage
	// steps ran. context.Background(), never ctx, per StartReconciler's
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
