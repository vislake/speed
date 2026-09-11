package app

// This file holds the host's half of the module set: constructModules is the
// WithModules callback that builds every module (and the one job queue they
// share), and registerEncryptedColumns is the WithPreDB callback that binds
// every encrypted column the modules own to its serializer and every
// blind-indexed column to its indexer, before the connection that parses
// those models exists.

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/vislake/speed/go/admin"
	aigateway "github.com/vislake/speed/go/ai-gateway"
	speedapp "github.com/vislake/speed/go/app"
	speedbridges "github.com/vislake/speed/go/app/bridges"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/compliance"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/integration"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/metering"
	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/notification/staticaddr"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pki"
	"github.com/vislake/speed/go/rbac"
	"github.com/vislake/speed/go/sharing"
	"github.com/vislake/speed/go/storage"
	"github.com/vislake/speed/go/tenancy"

	"github.com/vislake/speed/examples/reference-app/internal/app/demo"
	"github.com/vislake/speed/examples/reference-app/internal/attestation"
	"github.com/vislake/speed/examples/reference-app/internal/consult"
	demomodule "github.com/vislake/speed/examples/reference-app/internal/demo"
	"github.com/vislake/speed/examples/reference-app/internal/notes"
	"github.com/vislake/speed/examples/reference-app/internal/smilesim"
)

// OrgSubtreeResolverFor adapts org.Scope's Path method onto
// rbac.SubtreeResolver's NodePath -- the two no-import seams differ just
// enough (three return values vs. two; "not found" folded into an error vs.
// a plain boolean) that an adapter is needed, unlike the feature-gate
// seams' exact structural match (org.FeatureGateFunc over the config
// module's Handle). It returns the
// closure wrapped in rbac.SubtreeResolverFunc, so the wiring site passes
// the option a value it already accepts. org.Scope is safe to capture
// directly (unlike *config.Service in the readers below): orgModule.Scope()
// is available the moment org.NewModule returns, with no Attach-ordering
// constraint.
func OrgSubtreeResolverFor(scope org.Scope) rbac.SubtreeResolverFunc {
	return func(ctx context.Context, nodeID string) (string, bool, error) {
		path, err := scope.Path(ctx, nodeID)
		if err != nil {
			if apperr.HasCode(err, org.ErrNodeNotFound.Code) {
				// rbac's own contract: an unresolvable node DENIES the binding
				// that named it rather than erroring, so the caller's Can/
				// DataScope decision reports "no such node" as ok == false,
				// never widening to the tenant (SubtreeResolver's own doc
				// comment).
				return "", false, nil
			}
			return "", false, err
		}
		return path, true, nil
	}
}

// registerEncryptedColumns is the engine's WithPreDB callback: every
// encrypted column this app's modules own is bound through the module that
// owns it -- the module knows its GORM serializer name and the exact blind
// index column its lookups were designed against, so the host only supplies
// the keys. All registrations must happen before anything touches the
// models (GORM resolves a named serializer while it parses the schema),
// which is why this is a host step at all rather than something
// Module.Register could do (by then the database is open, and Register is
// forbidden from doing anything but declare).
//
// cipher is the platform cipher the engine built from the config.cipher_key
// material; it seals the four module-owned columns registered below
// (org's email, notification's contact address, ai-gateway's credential
// key, integration's webhook secret) and is the cipher the config module
// itself receives in constructModules. authn's PII columns and pki's local
// signer key each carry their own cipher, built here from the two key
// materials their modules declare separately -- an AES key must never
// double as another column's.
func (b *serverBuild) registerEncryptedColumns(_ context.Context, cipher *dbkit.Cipher) error {
	// authn's own PII columns (email, phone, TOTP secrets) have their own
	// cipher over the authn.pii_cipher_key material.
	piiCipher, err := dbkit.NewCipher(b.cfg.Authn.PII_Cipher_Key)
	if err != nil {
		return fmt.Errorf("reference-app: build authn's PII cipher: %w", err)
	}
	if regErr := authn.RegisterPIISerializer(piiCipher); regErr != nil {
		return fmt.Errorf("reference-app: register authn's PII serializer: %w", regErr)
	}

	// go/pki's LocalSigner private-key column, over the
	// pki.local_key_cipher_key material.
	pkiLocalKeyCipher, err := dbkit.NewCipher(b.cfg.PKI.Local_Key_Cipher_Key)
	if err != nil {
		return fmt.Errorf("reference-app: build pki's local-key cipher: %w", err)
	}
	if regErr := pki.RegisterLocalKeySerializer(pkiLocalKeyCipher); regErr != nil {
		return fmt.Errorf("reference-app: register pki's local-key serializer: %w", regErr)
	}

	if err := registerModuleSerializers(cipher); err != nil {
		return err
	}

	orgIndexer, contactEmailIndexer, contactPhoneIndexer, indexErr := buildModuleIndexers(b.cfg)
	if indexErr != nil {
		return indexErr
	}
	b.orgIndexer, b.contactEmailIndexer, b.contactPhoneIndexer = orgIndexer, contactEmailIndexer, contactPhoneIndexer
	return nil
}

// registerModuleSerializers binds the four module-owned encrypted columns
// through each module's own registrar -- the module knows the GORM
// serializer name its schema expects, so the name never crosses this
// boundary as a hand-typed string. Because the shared cipher is the only
// input, evaluating all four before reporting the first failure is
// equivalent to failing fast on it.
func registerModuleSerializers(cipher *dbkit.Cipher) error {
	for _, registration := range []struct {
		what string
		err  error
	}{
		{"org email serializer", org.RegisterEmailSerializer(cipher)},
		{"notification contact-address serializer", notification.RegisterContactAddressSerializer(cipher)},
		{"ai-gateway credential serializer", aigateway.RegisterCredentialAPIKeySerializer(cipher)},
		{"integration webhook-secret serializer", integration.RegisterWebhookSecretSerializer(cipher)},
	} {
		if registration.err != nil {
			return fmt.Errorf("reference-app: register the %s: %w", registration.what, registration.err)
		}
	}
	return nil
}

// buildModuleIndexers builds the blind indexers the org and notification
// modules query their encrypted columns through, each through the module's
// own constructor: the module owns its index column and canonical form, so
// neither crosses this boundary as a hand-typed string. org's invitation
// addresses and notification's verified contacts are made queryable by
// SEPARATE HMAC keys -- reusing cfg.Config.Cipher_Key for both would be
// exactly the AES-key-doubling-as-an-HMAC-key weakness dbkit warns against.
// One key serves notification's email and phone indexers alike (authn's
// single blind-index key precedent).
func buildModuleIndexers(cfg ServerConfig) (*dbkit.BlindIndexer, *dbkit.BlindIndexer, *dbkit.BlindIndexer, error) {
	orgIndexer, err := org.NewEmailIndexer(cfg.Org.Invitation_Email_Index_Key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("reference-app: build the org email indexer: %w", err)
	}
	contactEmailIndexer, err := notification.NewContactEmailIndexer(cfg.Notification.Contact_Index_Key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("reference-app: build the notification contact email indexer: %w", err)
	}
	contactPhoneIndexer, err := notification.NewContactPhoneIndexer(cfg.Notification.Contact_Index_Key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("reference-app: build the notification contact phone indexer: %w", err)
	}
	return orgIndexer, contactEmailIndexer, contactPhoneIndexer, nil
}

// constructModules is the engine's WithModules callback: it constructs the
// app's whole module set over the opened database and the platform cipher,
// in the order the modules' own construction requires, and returns them in
// the order the kernel registers them.
//
// The returned order is Bootstrap's: pki leads the set (authn's
// construction already consumed pkiModule.Service()), authn immediately
// behind it so its Register-time declarations precede the modules that
// lean on them, then notes and org before config so the configuration items
// and feature flags their Register calls declare are in the registry before
// the config module's own Register runs, then rbac so its own Attach --
// which snapshots every permission every module declared -- runs against a
// Registry that has already seen every module's declarations. The later
// modules' positions are not load-bearing (each's Register only declares
// its own seats), with two exceptions stated at their own wiring below:
// billing and metering are CONSTRUCTED before ai-gateway because its
// options need their built services, and admin's DependsOn("authn") makes
// Bootstrap's dependency sort run authn's Register before admin's
// regardless of argument order. audit last reads naturally as "the
// business-facing modules, then the cross-cutting persister watching them."
func (b *serverBuild) constructModules(ctx context.Context, deps speedapp.ModuleDeps) ([]pkgcore.Module, error) {
	b.db = deps.DB

	// hostByTenant is cfg.HostTenants' reverse index: which configured host
	// belongs to a given tenant, the first source an invitation's accept
	// link draws its host from. The host is DISPLAY, never acceptance:
	// org's InviteService.Accept resolves the invitation's tenant from the
	// token itself, server-side, when the accepting request carries no
	// tenant claim, and this app's middleware chain lets the accept path
	// through tenant resolution (the allowlist entry in routes.go), so a
	// link to ANY host that serves this app's API is acceptable. Only the
	// two demo tenants have a configured host (DemoHostTenants); every
	// other tenant this app serves is a self-registered clinic whose tenant
	// id self_service.go derives from its registrant (ClinicTenantOf,
	// "tenant-" + user id, by construction never a cfg.HostTenants value),
	// so no configured host can name it and its links draw their host from
	// the second source: this deployment's own public origin
	// (cfg.PublicOrigin, APP_PUBLIC_ORIGIN).
	b.hostByTenant = make(map[pkgcore.TenantID]string, len(b.cfg.HostTenants))
	for host, tenant := range b.cfg.HostTenants {
		b.hostByTenant[tenant] = host
	}

	// The config module shares notes' database and is given everything it
	// needs to serve its two endpoints: the platform cipher (notes declares
	// a Sensitive item, so Attach would refuse a cipher-less module), a
	// resolver, and the default anti-loss poller cadence. It is constructed
	// ahead of org and authn because both wire reads into it below, before
	// Attach exists: they take the module's lazy Handle (configModule.Handle,
	// live from Attach on) and adapt it through each module's own
	// FeatureGateFunc.
	//
	// The resolver is tenancy.NewDomainResolver -- deliberately NOT the
	// authn-derived resolver that gates the notes API below -- and its
	// default tenant is deliberately empty. config's public endpoints are
	// pre-auth display decisions, the one case go/tenancy's DomainResolver
	// doc comment blesses with unmatched-host leniency; an empty default
	// tenant maps that leniency onto the endpoint's own "platform defaults"
	// tier (a host that resolves to no tenant reads system-scope rows,
	// never an error). config's own internal resolver runs entirely
	// independently of the outer tenancy.Middleware the chain wires -- see
	// routes.go.
	b.configModule = config.NewModule(deps.DB,
		config.WithCipher(deps.Cipher),
		config.WithResolver(tenancy.NewDomainResolver(
			func(host string) (pkgcore.TenantID, bool) {
				tid, ok := b.cfg.HostTenants[host]
				return tid, ok
			},
			"",
		)),
	)
	configHandle := b.configModule.Handle()

	b.orgModule = org.NewModule(deps.DB,
		org.WithEmailIndexer(b.orgIndexer),
		org.WithFeatureGate(speedbridges.OrgFeatureGate(configHandle)),
		// principalFallback serves org's browser-shaped callers: org's two
		// caller-scoped endpoints also serve the team surface's signed-in
		// owner, whose requests carry a bearer token and no X-Demo-User-Id
		// header -- so org's resolver instance falls back to the verified
		// Principal when no demo header is present, the same
		// header-then-Principal shape DemoNotesSubjectResolver gives
		// notes' creator seam. The notification and integration modules
		// keep the flag false (see DemoOrgSubjectResolverFor's own doc
		// comment for why their header-only refusal stays pinned).
		org.WithSubjectResolver(org.SubjectResolverFunc(demo.DemoOrgSubjectResolverFor(b.cfg.DisableDemoUserHeader, true))),
		org.WithMailFrom("invitations@reference-app.example"),
		// Replies to an invitation land in the support inbox rather than on
		// the no-reply-ish sender above. Module-level configuration by
		// design: the inviter's own address is not reachable here (see
		// org.WithReplyTo's own doc comment).
		org.WithReplyTo("support@reference-app.example"),
		org.WithInvitationLinkBuilder(func(ctx context.Context, token string) (string, error) {
			tenant, tenantErr := pkgcore.MustTenantFromContext(ctx)
			if tenantErr != nil {
				return "", tenantErr
			}
			linkBase := ""
			if host, ok := b.hostByTenant[tenant]; ok {
				linkBase = "https://" + host
			} else if b.cfg.PublicOrigin != "" {
				linkBase = strings.TrimRight(b.cfg.PublicOrigin, "/")
			} else {
				return "", fmt.Errorf("reference-app: no host configured for tenant %q and no APP_PUBLIC_ORIGIN to fall back to", tenant)
			}
			// This app ships no frontend invitation-acceptance page: the
			// team surface is where an invitation is CREATED, while
			// accepting one stays the API-level flow a fresh invitee drives
			// from the email link. The link names org's real
			// POST /api/v1/org/invitations/accept endpoint and carries the
			// token as a query parameter so it is one recognizable string a
			// person (or a flow test) can extract the token back out of.
			return fmt.Sprintf("%s/api/v1/org/invitations/accept?token=%s", linkBase, url.QueryEscape(token)), nil
		}),
	)

	// standaloneQueue is this app's one job queue, shared by every module
	// whose asynchronous work runs on the jobs mechanism: a
	// jobs.StandaloneQueue over this app's own database connection, the
	// standalone mode's SQLite-backed worker pool whose task table
	// jobs.Wire creates after Bootstrap and whose pool the background
	// worker starts. jobs.WithEventBus hands the queue the same bus
	// reg.EventBus() resolves to, which is what makes the queue publish the
	// jobs.job.terminal signal: internal/smilesim's subscription
	// (composed in routes.go) settles the job's credit reservation and
	// publishes its completion notification the moment that signal lands.
	b.standaloneQueue = jobs.NewStandaloneQueue(deps.DB, jobs.WithEventBus(b.bus))

	// pki owns authn's signing-key lifecycle: LocalSigner (its own
	// zero-external-dependency default) generates and stores the key in
	// cfg.SQLitePath, so it persists across restarts with no dev-seed
	// derivation required. The reference app assembles authn directly
	// rather than through saasctl, so it wires pki here exactly as any
	// authn-containing generated project's own server does. WithQueue makes
	// Register attach the queue to Service and CAService and declare the
	// module's two job handlers onto the registry -- the signing-key
	// expiry-scan task (pki.expiry_scan), which this host's periodic-task
	// scheduler enqueues on its tick, and the CRL-regenerate task. The
	// three knobs below are applied only when a test injects them: a zero
	// PKIPropagationWindow, PKIRenewalLeadTime or PKIExpiryScanWindow
	// (what ConfigFromEnv always leaves them at) keeps pki's own defaults
	// in force.
	pkiOpts := []pki.Option{pki.WithQueue(b.standaloneQueue)}
	if b.cfg.PKIPropagationWindow > 0 {
		pkiOpts = append(pkiOpts, pki.WithPropagationWindow(b.cfg.PKIPropagationWindow))
	}
	if b.cfg.PKIRenewalLeadTime > 0 {
		pkiOpts = append(pkiOpts, pki.WithRenewalLeadTime(b.cfg.PKIRenewalLeadTime))
	}
	if b.cfg.PKIExpiryScanWindow > 0 {
		pkiOpts = append(pkiOpts, pki.WithExpiryScanWindow(b.cfg.PKIExpiryScanWindow))
	}
	b.pkiModule = pki.NewModule(deps.DB, pkiOpts...)

	b.memberships = b.cfg.Memberships
	if b.memberships == nil {
		b.memberships = NewSignInMemberships()
	}

	authnOpts := []authn.Option{
		authn.WithKeySource(b.pkiModule.Service()),
		authn.WithBlindIndexKey(b.cfg.Authn.Blind_Index_Key),
		authn.WithMembershipReader(b.memberships),
		authn.WithDeploymentMode(b.cfg.DeploymentMode),
		authn.WithSocialProviders(b.cfg.SocialProviders...),
		authn.WithRedirectAllowlist(b.cfg.RedirectAllowlist),
		authn.WithTrustedProviders(b.cfg.TrustedProviders...),
		// Immediate revocation: every sign-out this app drives -- a user's
		// own logout, the self-service session revoke, and the
		// refresh-replay theft response -- must cut off an unexpired access
		// token AT ONCE rather than at its natural expiry. authn's
		// enforcement is default-wired, not a second option: the service
		// attaches its session manager to the verifier Service().Verifier()
		// hands out, and the chain consults it with no
		// WithRevocationChecker of its own -- so this single selection is
		// the whole wiring. The cost is one key-value read per authenticated
		// request, the documented price of immediate mode, against the
		// in-process store in standalone boots and Redis in the distributed
		// composition.
		authn.WithRevocationMode(authn.RevocationModeImmediate),
		// The trusted-proxy declaration (APP_TRUSTED_PROXIES): the proxy
		// addresses whose requests may carry the forwarding headers authn
		// reads, so this app's session and login-history records carry the
		// real client address behind the proxy instead of the proxy's own.
		// Empty -- every local boot and every test -- is authn's fail-closed
		// default, and a declaration with an entry that is neither an IP
		// address nor a CIDR prefix refuses this NewModule call.
		authn.WithTrustedProxies(b.cfg.TrustedProxies...),
		// The feature gate that makes authn's eight declared feature flags
		// (authn.password_login, authn.sms_login, the five authn.social.*
		// channels, authn.sso.oidc) effective at request time:
		// speedbridges.AuthnFeatureGate adapts the same config-module handle
		// org's own gate above is adapted from. Without it this app's flags
		// would be declarations with no enforcement: a row disabling
		// authn.password_login would hide the login form while the password
		// endpoint kept issuing tokens. The channels this host assembles
		// through cfg.SocialProviders are opened at the system tier after
		// Attach by openConfiguredAuthnChannels, since their flags default
		// OFF.
		authn.WithFeatureGate(speedbridges.AuthnFeatureGate(configHandle)),
	}
	// The per-header vendor opt-in (APP_READ_FLY_CLIENT_IP): a generic
	// reverse proxy forwards a client-chosen Fly-Client-IP verbatim, so the
	// header is read only for a deployment whose declared proxy is Fly's.
	// ConfigFromEnv refuses 'true' with an empty APP_TRUSTED_PROXIES, so the
	// two halves of the declaration cannot be assembled inconsistently.
	if b.cfg.ReadFlyClientIP {
		authnOpts = append(authnOpts, authn.WithVendorClientIPHeaders(authn.VendorClientIPHeaderFlyClientIP))
	}
	// The "SMS sender" seam: a configured gateway URL always wins, under
	// either deployment mode, and composes the real HTTP SMS transport;
	// absent that, the standalone deployment mode falls back to the console
	// transport, while the distributed deployment mode is left deliberately
	// UNWIRED -- authn.NewModule's own newOptions then fails closed with
	// authn.ErrMissingDistributedSMSSender rather than this app silently
	// keeping a console sender nobody in a distributed replica pool is
	// reading.
	switch {
	case b.cfg.SMSGatewayURL != "":
		authnOpts = append(authnOpts, authn.WithSMSSender(pkgcore.NewHTTPSMSSender(b.cfg.SMSGatewayURL)))
	case b.cfg.DeploymentMode != pkgcore.DeploymentModeDistributed:
		authnOpts = append(authnOpts, authn.WithSMSSender(pkgcore.NewConsoleSMSSender(smsOutputFor(b.cfg))))
	}
	authnModule, err := authn.NewModule(deps.DB, authnOpts...)
	if err != nil {
		return nil, fmt.Errorf("reference-app: build the authn module: %w", err)
	}
	b.authnModule = authnModule

	// notes' creator seam is DemoNotesSubjectResolver: notes' create handler
	// reads the creating user's id through it and stamps it on the note and
	// the event it publishes, so an unwired resolver would fail every create
	// closed with notes.subject_unresolved. The resolver reads the
	// X-Demo-User-Id header first and falls back to the verified Principal
	// only when no header is present -- the fallback that lets the seeded
	// demo accounts create notes through their access tokens alone.
	b.notesModule = notes.NewModule(deps.DB, notes.WithSubjectResolver(demo.DemoNotesSubjectResolver{HeaderDisabled: b.cfg.DisableDemoUserHeader}))

	// auditModule is go/dbkit/audit's persister. It shares notesModule's
	// own database connection -- no new infra dependency is needed for this
	// app to have a real, queryable audit trail -- and subscribes to
	// audit.EventRecorded, the event notes' create handler publishes
	// through audit.Emit after a note is successfully created.
	b.auditModule = audit.New(deps.DB)

	// rbac needs nothing from this host but a database and the two seams
	// below: it declares its own permissions during Register and reads
	// EVERY module's declarations once, in Attach, after Bootstrap.
	// WithSubtreeResolver wires orgModule's own Scope
	// (OrgSubtreeResolverFor, an adapter over the two seams' slightly
	// different signatures): this app's organization tree is real, so a
	// node-scoped rbac binding must actually resolve against it rather than
	// deny for want of a resolver. WithQueue wires the host's
	// standaloneQueue into rbac's org-event reaping: when this app removes
	// a member or deletes an org node, the org-event subscriber ENQUEUES
	// the reap of that member's or node's role bindings as a task on this
	// same queue, instead of running the reap synchronously inside the
	// event delivery with no retry home.
	b.rbacModule = rbac.NewModule(deps.DB,
		rbac.WithSubtreeResolver(OrgSubtreeResolverFor(b.orgModule.Scope())),
		rbac.WithQueue(b.standaloneQueue))

	// storageModule is the reference app's first consumer of go/storage.
	// Its asynchronous work -- the thumbnail-derive task every completed
	// image object enqueues, and the expiry-sweep task this host's
	// periodic-task scheduler enqueues per tenant -- runs on the host's
	// standaloneQueue above.
	b.storageModule = storage.NewModule(deps.DB, storage.WithQueue(b.standaloneQueue))

	// attestationService is the reference app's AI-output attestation layer
	// -- the real consumer of go/pki's X.509 layer. It issues each tenant's
	// "simulation.attestation" certificate through pkiModule.CA(), signs
	// attested outputs with it, and gates their public shares on chain
	// verification. Constructed here -- after pkiModule and storageModule,
	// before sharingModule needs its gate -- with the pki CA service, a pki
	// certificate repository for the issue-vs-reuse decision, the same
	// ObjectService the resolver serves bytes through (adapted in
	// sharing_resolver.go) and its own app table store over this app's db
	// connection. Its two boot steps run in postAttach.
	b.attestationService = attestation.NewService(
		b.pkiModule.CA(),
		pki.NewCertificateRepository(deps.DB),
		storageContentOpener{svc: b.storageModule.ObjectService()},
		attestation.NewAttestationStore(deps.DB),
		deps.DB,
	)

	// sharingModule wires a real ResourceResolver behind the module's
	// public access surface: its one public route resolves a Share's
	// ResourceRef through storageSharingResolver (sharing_resolver.go), the
	// structurally-typed adapter over storageModule's own ObjectService --
	// sharing never imports go/storage itself, so this composition is
	// entirely this app's own. WithTenantConfigReader wires
	// speedbridges.ShareExpiryReader over the config module's lazy Handle
	// (configHandle, captured above), so a tenant's own
	// sharing.default_expiry override actually governs Service.Create's
	// resolved expiry instead of always falling back to the default.
	b.sharingModule = sharing.NewModule(deps.DB,
		sharing.WithResourceResolver(&storageSharingResolver{svc: b.storageModule.ObjectService(), attest: b.attestationService}),
		sharing.WithTenantConfigReader(speedbridges.ShareExpiryReader{Handle: configHandle}),
	)

	// integrationModule wires go/integration's outbound-webhook surface.
	// orgMemberJoinedWebhookMapping (webhooks.go) is this app's own
	// EventMapping -- a construction-time Option, since the mapping's
	// Transform closes over org's own MemberJoined payload shape, which
	// go/integration may not import. WithWebhookQueue shares the same
	// standaloneQueue every other module's asynchronous work already runs
	// on. WebhookURLValidator/WebhookHTTPClient are test-only overrides:
	// nil in every production boot, which leaves go/integration's SSRF
	// protection exactly as strict as its default.
	integrationOpts := []integration.Option{
		integration.WithEventMapping(orgMemberJoinedWebhookMapping),
		integration.WithWebhookQueue(b.standaloneQueue),
		integration.WithSubjectResolver(integration.SubjectResolverFunc(demo.DemoOrgSubjectResolverFor(b.cfg.DisableDemoUserHeader, false))),
	}
	if b.cfg.WebhookURLValidator != nil {
		integrationOpts = append(integrationOpts, integration.WithWebhookURLValidator(b.cfg.WebhookURLValidator))
	}
	if b.cfg.WebhookHTTPClient != nil {
		integrationOpts = append(integrationOpts, integration.WithWebhookHTTPClient(b.cfg.WebhookHTTPClient))
	}
	b.integrationModule = integration.NewModule(deps.DB, integrationOpts...)

	// notificationModule's six required seams are all host-supplied here:
	// the console SMS sender writes to the same smsOutput the authn
	// module's sender writes to; the mail transport is whatever the
	// "mailer" seam resolves to (the console default, "mailer.smtp" once
	// the APP_SMTP_* group is configured, cfg.Mailer in tests); the two
	// blind indexers built above make a contact's encrypted email and phone
	// address queryable by exact match; the delivery queue is the same
	// standaloneQueue storage's derive task runs on; and the user-address
	// resolver is notification/staticaddr over the demo directory. The
	// subject resolver keeps principalFallback unset, so this surface's
	// caller is whoever the X-Demo-User-Id header says (see
	// DemoOrgSubjectResolverFor's own doc comment on why the two wirings
	// differ). authnUserLocales adapts authn's user store to
	// notification's UserLocaleResolver seam, reading the authn service
	// lazily so the wiring order is not a hazard.
	b.authnUserLocales = demo.AuthnUserLocales{Authn: b.authnModule}
	b.notificationModule = notification.NewModule(deps.DB,
		notification.WithSMSSender(pkgcore.NewConsoleSMSSender(smsOutputFor(b.cfg))),
		notification.WithMailFrom("notifications@reference-app.example"),
		notification.WithReplyTo("support@reference-app.example"),
		notification.WithContactEmailIndexer(b.contactEmailIndexer),
		notification.WithContactPhoneIndexer(b.contactPhoneIndexer),
		notification.WithDeliveryQueue(b.standaloneQueue),
		notification.WithUserAddressResolver(staticaddr.New(demo.DemoUserAddresses)),
		notification.WithUserLocaleResolver(b.authnUserLocales),
		notification.WithSubjectResolver(notification.SubjectResolverFunc(demo.DemoOrgSubjectResolverFor(b.cfg.DisableDemoUserHeader, false))),
	)

	// demoModule is the carrier of the app's demo notification type
	// (demo.patient_reminder) and nothing else. It must sit inside the
	// Bootstrap module set because Kernel.Bootstrap freezes the merged
	// catalog from the Locales() of the modules it is given, and the
	// notification module renders every dispatch from that frozen catalog
	// -- a type whose copy lives outside the set can never render.
	b.demoModule = demomodule.NewModule()

	// billingModule is the reference app's mandatory first consumer of
	// go/billing, both judgment halves of the module: the credits ledger
	// through Credits(), and the subscription-derived entitlement path
	// through Entitlements(), which aiGatewayModule below wires onto
	// go/ai-gateway's optional Entitlements seam. It is deliberately
	// constructed BEFORE aiGatewayModule: Go evaluates statements in order,
	// and the WithEntitlements option below needs billingModule's
	// already-built EntitlementsService in scope. That is safe even though
	// billing's Register comes later: billing.NewModule constructs its
	// services at construction time -- no I/O -- and EntitlementsService.Check
	// reads the subscription/plan rows fresh on every call, so judging
	// cannot start before the demo seed has run. The one deliberately
	// absent wiring is a UsageReader (nil): quota-kind grants need
	// go/billing's real-time usage counter through that reader, and this
	// app wires none into billing -- which is why the demo seed's grants
	// are Boolean, never Quota. No WithQueue either: no payment-channel
	// gateway is wired, so PollingService's active-polling fallback has
	// nothing to poll.
	b.billingModule = billing.NewModule(deps.DB, nil)

	// meteringModule is go/metering's seat in this app: admin's usage
	// dashboard reads its per-tenant summary rows (admin.WithMetering
	// below), and the ai-gateway module's construction below records every
	// real consult/smilesim AI call's usage into it through the gateway's
	// UsageRecorder seam. Constructed here, immediately after billingModule
	// and before aiGatewayModule, because both later constructions consume
	// it. NewModule performs no I/O, so its Register-time declarations
	// (its two config items and its one published event) and its migration
	// set register and apply with every other module's.
	b.meteringModule = metering.NewModule(deps.DB)

	// aiGatewayModule is the reference app's mandatory first consumer of
	// go/ai-gateway: the internal/consult service calls its Gateway.Chat
	// under consult.LogicalModel ("chat:default"), which WithModelRoute
	// routes to the module's own zero-external-dependency default provider
	// (aigateway.ProviderOpenAICompatible), and the internal/smilesim
	// service calls Gateway.GenerateImage under smilesim.LogicalModel
	// ("image:smile-simulation"), routed to
	// aigateway.ProviderOpenAICompatibleImage. The vendor model ids are
	// opaque to this app -- they are passed through to whatever
	// OpenAI-compatible endpoint the resolved credential's base URL
	// actually names. WithImageGeneration shares this app's own
	// standaloneQueue and storageModule's own ObjectService, so the job
	// handler go/ai-gateway registers reads the patient photo and writes
	// the generated simulation back through the very same storage this
	// app's other consumers use. WithEntitlements wires the real
	// billing.EntitlementsService through speedbridges.Entitlements, so
	// BOTH halves of this one Gateway instance gate every request on the
	// calling tenant's subscription before any provider is reached; an
	// unwired seam (nil) would let every request through, and a wired one
	// judged against an empty database would deny everything --
	// SeedDemoEntitlements (postBootstrap) is what keeps the seeded demo
	// tenants on the allowed side from the very first request.
	// gatewayEntitlements is the ONE adapter instance this app's gateway
	// gate and smilesim's own pre-flight both run through: binding it as a
	// value first lets the same closure be handed to two consumers --
	// aiGatewayModule's WithEntitlements below and smileSimService's
	// NewService call (postAttach), whose Simulate pre-flights the gate
	// BEFORE its credit reservation opens, so a refused request never
	// writes a PreDeduct/Refund pair. The two must answer through the very
	// same seam, or the pre-flight and the gateway's re-check could
	// disagree about the same state.
	b.gatewayEntitlements = speedbridges.Entitlements(b.billingModule.Entitlements())

	// WithUsageRecorder hands go/metering's analytics-grade Recorder to the
	// SAME Gateway instance both halves above run on through
	// speedbridges.UsageRecorder, so every successful Chat/ChatStream call
	// records its token usage automatically (image generation records the
	// same way, through the job handler go/ai-gateway registers).
	// meteringModule.Recorder() is the analytics-grade (fail-open) tier,
	// the only tier this seam's Record(ctx, event) shape can carry: the
	// billing-grade Enqueue path demands the caller's own transaction
	// handle, which a recorder callback has no room for. The events it
	// buffers are folded into real metering_usage_summaries rows by the
	// recorder's flush loop once meteringModule.Start runs (postAttach),
	// and admin's dashboard reads those rows back per tenant. The one
	// feature this seam reports -- "ai.chat_tokens" -- is recorded through
	// this tier alone, never also through the billing-grade Enqueue path.
	meteringUsageRecorder := b.meteringModule.Recorder()
	b.aiGatewayModule = aigateway.NewModule(deps.DB,
		aigateway.WithModelRoute(consult.LogicalModel, aigateway.ProviderOpenAICompatible, "gpt-4o-mini"),
		aigateway.WithModelRoute(smilesim.LogicalModel, aigateway.ProviderOpenAICompatibleImage, "dall-e-3"),
		aigateway.WithImageGeneration(b.standaloneQueue, b.storageModule.ObjectService()),
		aigateway.WithEntitlements(b.gatewayEntitlements),
		aigateway.WithUsageRecorder(speedbridges.UsageRecorder(meteringUsageRecorder)),
	)

	// complianceModule is the reference app's first consumer of
	// go/compliance: admin's audit-query HTTP shell reads through
	// complianceModule.AuditQuery(), which itself is a read-only wrapper
	// over the SAME audit.Repository (over this same database connection)
	// auditModule's own write-capture persister already writes into.
	// WithQueue is the same standaloneQueue every other module's
	// asynchronous work already shares; compliance.Module.Register refuses
	// to proceed without one. WithSharing wires the already-constructed
	// sharingModule's own Service() as compliance.SharingCreator -- the
	// seam an owning module opts into; go/admin's audit-query export leg is
	// that module, so this is where compliance.ExportService.Export gains
	// its first genuine delivery path (a real, single-view go/sharing link)
	// rather than refusing every call with ErrSharingRequired.
	// WithExportConfigReader wires compliance.NewConfigReader over the
	// config module's handle, so a tenant's own
	// compliance.export_delivery_expiry override actually governs
	// ExportService.Export's minted delivery-link expiry.
	complianceAuditRepo := audit.NewRepository(deps.DB)
	b.complianceModule = compliance.NewModule(complianceAuditRepo,
		compliance.WithQueue(b.standaloneQueue),
		compliance.WithSharing(b.sharingModule.Service()),
		compliance.WithExportConfigReader(compliance.NewConfigReader(configHandle)),
	)

	// adminModule is the reference app's mandatory first consumer of
	// go/admin's operator-facing surface: the tenant ledger, the
	// impersonation pipeline, the cross-tenant user search, the audit-query
	// shell and export leg, role management and the usage/billing
	// dashboard. WithMetering hands it the meteringModule constructed above
	// and WithBilling the same billingModule every other consumer in this
	// file already uses; both options are optional by go/admin's own
	// contract, and this app wires both. WithAuthn takes the *authn.Module
	// itself, not its Service() -- see admin.Module.DependsOn()'s own doc
	// comment for the resulting "authn" dependency Kernel.Bootstrap's sort
	// honors. WithQueue is the same standaloneQueue every other module's
	// asynchronous work already shares -- the audit-query export leg
	// enqueues onto it rather than running
	// compliance.ExportService.Export synchronously inside the request.
	b.adminModule = admin.NewModule(deps.DB,
		admin.WithAuthn(b.authnModule),
		admin.WithOrg(b.orgModule),
		admin.WithCompliance(b.complianceModule),
		admin.WithNotification(b.notificationModule),
		admin.WithMetering(b.meteringModule),
		admin.WithBilling(b.billingModule),
		admin.WithQueue(b.standaloneQueue),
	)

	_ = ctx
	return []pkgcore.Module{
		b.pkiModule,
		b.authnModule,
		b.notesModule,
		b.orgModule,
		b.configModule,
		b.rbacModule,
		b.storageModule,
		b.sharingModule,
		b.integrationModule,
		b.demoModule,
		b.notificationModule,
		b.aiGatewayModule,
		b.billingModule,
		b.meteringModule,
		b.complianceModule,
		b.adminModule,
		b.auditModule,
	}, nil
}
