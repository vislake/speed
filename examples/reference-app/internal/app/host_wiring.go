package app

// host_wiring.go carries the components this host contributes besides the
// step components in component.go: the override components that construct a
// module with this app's own construction-time wiring, and the provider
// components whose products satisfy the seam tokens the assembled modules
// declare.
//
// # Overrides
//
// A module's own descriptor constructs the module from its configuration
// block, its declared key material and the optional seam tokens it reads
// from the by-type context. Two classes of module need more than that:
//
//   - Modules whose construction this app configures beyond what a
//     descriptor can carry -- a membership store, a social-provider set, a
//     webhook event mapping, a logical-model route, the write-capture scope
//     of its database connection. The module packages' own descriptors say
//     so at the point that matters (notification's descriptor: "the host
//     wiring is the path that performs it").
//   - Modules whose descriptor Init turn takes a snapshot of the OTHER
//     modules' declarations -- config's configuration schema, rbac's
//     permission catalog, admin's authorizer, each of which must cover every
//     module's declarations, not the ones made before the snapshotting
//     component's turn in plan order. The host takes those declarations and
//     performs the attach sequence itself, in the post-bootstrap step.
//
// For both classes the host selects its own override component: the
// descriptor is copied from the registry -- every declaration, capability,
// asset and lifecycle callback stays the module package's own, and only the
// Name (a component name is unique, and the module's own descriptor keeps
// its own), the New callback or the Init callback differ. A module whose
// seams are all reachable through the by-type context (sharing, compliance,
// notes, storage, ...) needs no override: the provider components below
// supply its tokens and its own descriptor constructs it.

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/admin"
	aigateway "github.com/vislake/speed/go/ai-gateway"
	speedbridges "github.com/vislake/speed/go/app/bridges"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/compliance"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/integration"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/metering"
	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/notification/staticaddr"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pki"
	"github.com/vislake/speed/go/rbac"
	"github.com/vislake/speed/go/sharing"
	"github.com/vislake/speed/go/storage"
	"github.com/vislake/speed/go/tenancy"

	"github.com/vislake/speed/examples/reference-app/internal/app/demo"
	"github.com/vislake/speed/examples/reference-app/internal/attestation"
	"github.com/vislake/speed/examples/reference-app/internal/consult"
	"github.com/vislake/speed/examples/reference-app/internal/notes"
	"github.com/vislake/speed/examples/reference-app/internal/smilesim"
)

// hostComponentPrefix names every component this file contributes. A host
// component that overrides a module's own descriptor carries the module's
// name as its suffix, so the override is readable as such.
const hostComponentPrefix = "reference-app."

// The declared bootstrap key paths this file's wiring reads material for.
// Each is the declaring module's own key path, spelled as its declaration
// carries it: the assembly's loader resolves the declaration, and this app
// reads the result by the same path.
const (
	// configCipherKeyPath is the path the platform cipher's material is
	// declared under (go/config's component).
	configCipherKeyPath = "config.cipher_key"
	// authnBlindIndexKeyPath is the path authn's blind-index HMAC key is
	// declared under (go/authn's component).
	authnBlindIndexKeyPath = "authn.blind_index_key"
	// pkiLocalKeyCipherKeyPath is the path the cipher sealing pki's
	// local-signer private-key column is declared under (go/pki's component).
	pkiLocalKeyCipherKeyPath = "pki.local_key_cipher_key"
	// orgInvitationIndexKeyPath is the path org's invitation-email blind
	// index's HMAC key is declared under (go/org's component).
	orgInvitationIndexKeyPath = "org.invitation_email_index_key"
	// notificationContactIndexKeyPath is the path notification's
	// contact-index HMAC key is declared under (go/notification's
	// component). One key serves both the email and the phone indexer.
	notificationContactIndexKeyPath = "notification.contact_index_key"
)

// declaredMaterial reads the []byte material a declared bootstrap key
// resolved to from the assembly's published material source, naming the
// declared path when the assembly carries no value for it.
func declaredMaterial(reg *pkgcore.ComponentRegistry, keyPath string) ([]byte, error) {
	material, err := pkgcore.BootstrapMaterialOf(reg)
	if err != nil {
		return nil, err
	}
	value, ok := material.Material(keyPath)
	if !ok {
		return nil, fmt.Errorf("reference-app: the assembly resolved no material for the declared bootstrap key %q", keyPath)
	}
	return value, nil
}

// capabilityComponent returns the registered descriptor named base under
// the host's own name, carrying one extra declaration: MultiReplicaSafe.
// It exists for the module whose descriptor deliberately declares no
// capability (signer.local: an in-process signer whose key material is
// per-replica by design), which a distributed composition of this app
// still selects -- the host's own placement decision, stated here rather
// than silently inherited. Every other module's descriptor declares its own
// capabilities, so no other component needs this copy.
func capabilityComponent(reg *pkgcore.ComponentRegistry, name string) (pkgcore.Component, error) {
	descriptor, ok := registeredComponent(reg, name)
	if !ok {
		return pkgcore.Component{}, fmt.Errorf("reference-app: component %q has no registered descriptor", name)
	}
	c := descriptor
	c.Name = hostComponentPrefix + name
	c.Capabilities |= pkgcore.MultiReplicaSafe
	return c, nil
}

// overrideComponent returns the registered descriptor named base carrying
// this host's own construction. Everything but the name and the New callback
// is the package's own declaration: the same Requires, Provides, assets
// (locale resources included -- the catalog merges them under the module
// name, assembly.go's hostCatalog), capabilities, system purposes and
// lifecycle callbacks the component ships, so an override declares exactly
// what the component declares and only constructs it differently.
// moduleName is the module the descriptor implements, and the suffix of the
// override's own name.
func overrideComponent(reg *pkgcore.ComponentRegistry, base, moduleName string, construct func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error), extra ...pkgcore.Requirement) (pkgcore.Component, error) {
	descriptor, ok := registeredComponent(reg, base)
	if !ok {
		return pkgcore.Component{}, fmt.Errorf("reference-app: component %q has no registered descriptor to override", base)
	}
	c := descriptor
	c.Name = hostComponentPrefix + moduleName
	c.New = construct
	c.Requires = append(append([]pkgcore.Requirement(nil), descriptor.Requires...), extra...)
	return c, nil
}

// registeredComponent returns the component registered under name.
func registeredComponent(reg *pkgcore.ComponentRegistry, name string) (pkgcore.Component, bool) {
	for _, c := range pkgcore.RegisteredComponents(reg) {
		if c.Name == name {
			return c, true
		}
	}
	return pkgcore.Component{}, false
}

// registeredModuleComponent returns the component a module package
// registered for itself: the descriptor carrying the module's name as both
// its component name and its module name.
func registeredModuleComponent(reg *pkgcore.ComponentRegistry, moduleName string) (pkgcore.Component, bool) {
	for _, c := range pkgcore.RegisteredComponents(reg) {
		if c.Name == moduleName && c.Module == moduleName {
			return c, true
		}
	}
	return pkgcore.Component{}, false
}

// dbComponent returns this host's database component: the SQLite connection
// the app opens, carrying the write-capture scope. The scope is what makes
// this a host component rather than the built-in "db.sqlite": the capture
// plugin is installed by the open call itself (dbkit.Open's AuditBus and
// AuditModels), so the audit bus and the model restriction are construction
// parameters no database component's configuration block can carry. Every
// auditable write org makes publishes a dbkit.write.captured event on the
// bus this component requires, which is the very bus the assembly resolved
// for every module -- the identity that keeps the capture visible to the
// audit persister subscribed on it.
//
// The declared capabilities describe this app's own database placement, and
// both bits are load-bearing for a distributed boot: the deployment's
// replicas point cfg.SQLitePath at ONE database (the shared-state shape the
// integration tier exercises, two processes reading each other's writes
// through the same file), and the file outlives any one process -- the
// properties MultiReplicaSafe and SurvivesRestart name. The built-in
// "db.sqlite" declares neither, because its own assumption is a per-replica
// file; a deployment that wants a client/server database selects the other
// dialect instead.
func (b *serverBuild) dbComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:         hostComponentPrefix + "db",
		Module:       "db",
		Provides:     []any{(*gorm.DB)(nil)},
		Capabilities: pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart,
		Requires:     []pkgcore.Requirement{{Token: (*pkgcore.EventBus)(nil)}},
		New: func(ctx context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
			bus, err := pkgcore.Get[pkgcore.EventBus](reg)
			if err != nil {
				return nil, fmt.Errorf("reference-app: read the assembled event bus for the write-capture scope: %w", err)
			}
			// Org's automatic write capture publishes on the bus the
			// assembly resolved; the capture scope is org's own exported
			// declaration and nothing else (notes.Note stays out: its
			// module records its own trail through audit.Emit).
			return dbkit.Open(ctx, dbkit.Options{
				Dialect:     dbkit.DialectSQLite,
				DSN:         b.cfg.SQLitePath,
				AuditBus:    bus,
				AuditModels: org.AuditableModels(),
			})
		},
		// Verify applies the assembled migration sets, the stage the
		// database component owns: every product is constructed by now, so
		// the selected components' assets are complete.
		Verify: func(ctx context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
			return dbkit.ApplyMigrations(ctx, reg)
		},
		Close: func(_ context.Context, _ *pkgcore.ComponentRegistry, instance any) error {
			if db, ok := instance.(*gorm.DB); ok {
				return dbkit.Close(db)
			}
			return nil
		},
	}
}

// authnComponent returns authn's descriptor with this app's construction:
// the membership store signing in resolves tenants through, the feature gate
// that makes authn's flags effective, the social providers this deployment
// assembled, the redirect allowlist and trusted-provider list their callbacks
// are validated against, and the per-header vendor opt-in.
func (b *serverBuild) authnComponent(reg *pkgcore.ComponentRegistry) (pkgcore.Component, error) {
	return overrideComponent(reg, "authn", "authn", func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
		db, err := pkgcore.Get[*gorm.DB](reg)
		if err != nil {
			return nil, err
		}
		// The signing-key lifecycle is taken from the pki module's own
		// construction product: the module's *Service satisfies authn's
		// KeySource contract structurally, and the module hands it out the
		// moment it is constructed.
		pkiModule, err := pkgcore.Get[*pki.Module](reg)
		if err != nil {
			return nil, err
		}
		keySource := pkiModule.Service()
		configModule, err := pkgcore.Get[*config.Module](reg)
		if err != nil {
			return nil, err
		}
		// The blind-index key is authn's second declared key material: the
		// assembly resolved it before anything was constructed, so this
		// override reads it from the published material source by its
		// declared path -- the same source authn's own descriptor reads.
		blindIndexKey, err := declaredMaterial(reg, authnBlindIndexKeyPath)
		if err != nil {
			return nil, err
		}
		opts := []authn.Option{
			authn.WithKeySource(keySource),
			authn.WithBlindIndexKey(blindIndexKey),
			authn.WithMembershipReader(b.memberships),
			authn.WithDeploymentMode(b.cfg.DeploymentMode),
			authn.WithSocialProviders(b.cfg.SocialProviders...),
			authn.WithRedirectAllowlist(b.cfg.RedirectAllowlist),
			authn.WithTrustedProviders(b.cfg.TrustedProviders...),
			// Immediate revocation: every sign-out this app drives -- a
			// user's own logout, the self-service session revoke, and the
			// refresh-replay theft response -- must cut off an unexpired
			// access token AT ONCE rather than at its natural expiry.
			authn.WithRevocationMode(authn.RevocationModeImmediate),
			authn.WithTrustedProxies(b.cfg.TrustedProxies...),
			// The feature gate that makes authn's declared feature flags
			// effective at request time: speedbridges.AuthnFeatureGate
			// adapts the config module's lazy handle. Without it this app's
			// flags would be declarations with no enforcement.
			authn.WithFeatureGate(speedbridges.AuthnFeatureGate(configModule.Handle())),
		}
		// The per-header vendor opt-in: a generic reverse proxy forwards a
		// client-chosen Fly-Client-IP verbatim, so the header is read only
		// for a deployment whose declared proxy is Fly's. ConfigFromEnv
		// refuses 'true' with an empty APP_TRUSTED_PROXIES, so the two
		// halves of the declaration cannot be assembled inconsistently.
		if b.cfg.ReadFlyClientIP {
			opts = append(opts, authn.WithVendorClientIPHeaders(authn.VendorClientIPHeaderFlyClientIP))
		}
		// The "SMS sender" seam: a configured gateway URL always wins, under
		// either deployment mode, and composes the real HTTP SMS transport;
		// absent that, the standalone deployment mode takes the seam the
		// composition selected (the console transport, or the host's own
		// capture sender), while the distributed deployment mode is left
		// deliberately UNWIRED -- authn's own construction then fails closed
		// with ErrMissingDistributedSMSSender rather than this app silently
		// keeping a transport nobody in a distributed replica pool is
		// reading.
		wireSender := b.cfg.SMSGatewayURL != "" || b.cfg.DeploymentMode != pkgcore.DeploymentModeDistributed
		sender, hasSender, err := pkgcore.GetOptional[pkgcore.SMSSender](reg)
		if err != nil {
			return nil, err
		}
		if wireSender && hasSender {
			opts = append(opts, authn.WithSMSSender(sender))
		}
		return authn.NewModule(db, opts...)
	},
		pkgcore.Requirement{Token: (*config.Module)(nil)},
		pkgcore.Requirement{Token: (*pki.Module)(nil)},
	)
}

// notificationComponent returns notification's descriptor with this app's
// construction: the SMS sender the composition selected, the two blind
// indexers over the module's encrypted contact addresses, the delivery
// queue, the demo directory's user-address resolver, the authn-backed
// profile-locale resolver, and the header-only subject resolver this app's
// notification surfaces pin.
func (b *serverBuild) notificationComponent(reg *pkgcore.ComponentRegistry) (pkgcore.Component, error) {
	return overrideComponent(reg, "notification", "notification", func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
		db, err := pkgcore.Get[*gorm.DB](reg)
		if err != nil {
			return nil, err
		}
		queue, err := pkgcore.Get[jobs.Queue](reg)
		if err != nil {
			return nil, err
		}
		sms, err := pkgcore.Get[pkgcore.SMSSender](reg)
		if err != nil {
			return nil, err
		}
		// The profile-locale resolver reads authn's user store through the
		// host adapter; b.authnUserLocales keeps the instance the composed
		// face's demo glue renders with.
		authnModule, err := pkgcore.Get[*authn.Module](reg)
		if err != nil {
			return nil, err
		}
		b.authnUserLocales = demo.AuthnUserLocales{Authn: authnModule}
		return notification.NewModule(db,
			notification.WithSMSSender(sms),
			notification.WithMailFrom("notifications@reference-app.example"),
			notification.WithReplyTo("support@reference-app.example"),
			notification.WithContactEmailIndexer(b.contactEmailIndexer),
			notification.WithContactPhoneIndexer(b.contactPhoneIndexer),
			notification.WithDeliveryQueue(queue),
			notification.WithUserAddressResolver(staticaddr.New(demo.DemoUserAddresses)),
			notification.WithUserLocaleResolver(b.authnUserLocales),
			notification.WithSubjectResolver(notification.SubjectResolverFunc(demo.DemoOrgSubjectResolverFor(b.cfg.DisableDemoUserHeader, false))),
		), nil
	}, pkgcore.Requirement{Token: (*authn.Module)(nil)})
}

// integrationComponent returns integration's descriptor with this app's
// construction: the org-member-joined event mapping this deployment's
// webhook surface publishes, the shared delivery queue, the header-only
// subject resolver its creator reads use, and the two SSRF overrides the
// webhook flow test arms (nil in every production boot, leaving the
// module's own strict default in force).
func (b *serverBuild) integrationComponent(reg *pkgcore.ComponentRegistry) (pkgcore.Component, error) {
	return overrideComponent(reg, "integration", "integration", func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
		db, err := pkgcore.Get[*gorm.DB](reg)
		if err != nil {
			return nil, err
		}
		opts := []integration.Option{
			integration.WithEventMapping(orgMemberJoinedWebhookMapping),
			integration.WithSubjectResolver(integration.SubjectResolverFunc(demo.DemoOrgSubjectResolverFor(b.cfg.DisableDemoUserHeader, false))),
		}
		queue, hasQueue, err := pkgcore.GetOptional[jobs.Queue](reg)
		if err != nil {
			return nil, err
		}
		if hasQueue {
			opts = append(opts, integration.WithWebhookQueue(queue))
		}
		if b.cfg.WebhookURLValidator != nil {
			opts = append(opts, integration.WithWebhookURLValidator(b.cfg.WebhookURLValidator))
		}
		if b.cfg.WebhookHTTPClient != nil {
			opts = append(opts, integration.WithWebhookHTTPClient(b.cfg.WebhookHTTPClient))
		}
		return integration.NewModule(db, opts...), nil
	})
}

// aiGatewayComponent returns ai-gateway's descriptor with this app's
// construction: the logical-model routes consult and smilesim ask the
// gateway for, image generation over the shared queue and storage service,
// the billing-derived entitlements gate, and metering's usage recorder.
func (b *serverBuild) aiGatewayComponent(reg *pkgcore.ComponentRegistry) (pkgcore.Component, error) {
	return overrideComponent(reg, "ai-gateway", "ai-gateway", func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
		db, err := pkgcore.Get[*gorm.DB](reg)
		if err != nil {
			return nil, err
		}
		opts := []aigateway.GatewayOption{
			aigateway.WithModelRoute(consult.LogicalModel, aigateway.ProviderOpenAICompatible, "gpt-4o-mini"),
			aigateway.WithModelRoute(smilesim.LogicalModel, aigateway.ProviderOpenAICompatibleImage, "dall-e-3"),
		}
		// Image generation is wired exactly when both products it runs on
		// are selected: the queue its job handler drains and the storage
		// service whose bytes it reads and writes.
		queue, hasQueue, err := pkgcore.GetOptional[jobs.Queue](reg)
		if err != nil {
			return nil, err
		}
		storageModule, hasStorage, err := pkgcore.GetOptional[*storage.Module](reg)
		if err != nil {
			return nil, err
		}
		if hasQueue && hasStorage {
			opts = append(opts, aigateway.WithImageGeneration(queue, storageModule.ObjectService()))
		}
		entitlements, hasEntitlements, err := pkgcore.GetOptional[aigateway.Entitlements](reg)
		if err != nil {
			return nil, err
		}
		if hasEntitlements {
			opts = append(opts, aigateway.WithEntitlements(entitlements))
		}
		recorder, hasRecorder, err := pkgcore.GetOptional[aigateway.UsageRecorder](reg)
		if err != nil {
			return nil, err
		}
		if hasRecorder {
			opts = append(opts, aigateway.WithUsageRecorder(recorder))
		}
		return aigateway.NewModule(db, opts...), nil
	})
}

// cryptoComponent returns the component that builds and registers every
// serializer and blind indexer this app's modules read their encrypted
// columns through, before the connection that parses those models exists --
// which is why its work runs in the Prepare stage, and why the cipher it
// builds is provided as a product: the config module's Sensitive items are
// sealed with it. authn's own PII column needs no registration here: its
// descriptor performs it in its own Prepare callback.
func (b *serverBuild) cryptoComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:         hostComponentPrefix + "crypto",
		Capabilities: pkgcore.MultiReplicaSafe,
		Provides:     []any{(*dbkit.Cipher)(nil)},
		Prepare: func(_ context.Context, reg *pkgcore.ComponentRegistry) error {
			cipherKey, err := declaredMaterial(reg, configCipherKeyPath)
			if err != nil {
				return err
			}
			cipher, err := dbkit.NewCipher(cipherKey)
			if err != nil {
				return fmt.Errorf("reference-app: build the platform cipher: %w", err)
			}

			// go/pki's LocalSigner private-key column, over the
			// pki.local_key_cipher_key material.
			pkiLocalKeyCipherKey, err := declaredMaterial(reg, pkiLocalKeyCipherKeyPath)
			if err != nil {
				return err
			}
			pkiLocalKeyCipher, err := dbkit.NewCipher(pkiLocalKeyCipherKey)
			if err != nil {
				return fmt.Errorf("reference-app: build pki's local-key cipher: %w", err)
			}
			if regErr := pki.RegisterLocalKeySerializer(pkiLocalKeyCipher); regErr != nil {
				return fmt.Errorf("reference-app: register pki's local-key serializer: %w", regErr)
			}
			if err := registerModuleSerializers(cipher); err != nil {
				return err
			}

			orgIndexer, contactEmailIndexer, contactPhoneIndexer, indexErr := buildModuleIndexers(reg)
			if indexErr != nil {
				return indexErr
			}
			b.orgIndexer, b.contactEmailIndexer, b.contactPhoneIndexer = orgIndexer, contactEmailIndexer, contactPhoneIndexer
			b.platformCipher = cipher
			return nil
		},
		New: func(_ context.Context, _ *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
			if b.platformCipher == nil {
				return nil, fmt.Errorf("reference-app: the platform cipher is missing; its Prepare callback builds it before construction")
			}
			return b.platformCipher, nil
		},
	}
}

// tenancyResolverComponent returns the request-to-tenant resolver the config
// module's two public endpoints pick whose configuration to serve with: this
// deployment's configured host map, with an empty default tenant so an
// unmatched host reads the platform-defaults tier -- the documented display
// decision for the unauthenticated case.
func (b *serverBuild) tenancyResolverComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:         hostComponentPrefix + "tenancy-resolver",
		Capabilities: pkgcore.MultiReplicaSafe,
		Provides:     []any{(*tenancy.Resolver)(nil)},
		New: func(_ context.Context, _ *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
			return tenancy.NewDomainResolver(
				func(host string) (pkgcore.TenantID, bool) {
					tid, ok := b.cfg.HostTenants[host]
					return tid, ok
				},
				"",
			), nil
		},
	}
}

// orgComponent returns org's descriptor with this app's construction: the
// feature gate that makes org's declared flags effective at request time
// (the config module's lazy handle adapted through org's own gate seam --
// wired here rather than supplied as a token, because the config module's
// own service satisfies the gate's method set structurally and two values
// assignable to one seam are ambiguous), the email indexer over the
// invitation-address column, the caller resolver org's caller-scoped
// endpoints read, the mail identities its invitations render with, and the
// invitation-link builder.
func (b *serverBuild) orgComponent(reg *pkgcore.ComponentRegistry) (pkgcore.Component, error) {
	return overrideComponent(reg, "org", "org", func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
		db, err := pkgcore.Get[*gorm.DB](reg)
		if err != nil {
			return nil, err
		}
		configModule, err := pkgcore.Get[*config.Module](reg)
		if err != nil {
			return nil, err
		}
		opts := []org.Option{
			org.WithEmailIndexer(b.orgIndexer),
			org.WithFeatureGate(speedbridges.OrgFeatureGate(configModule.Handle())),
			// principalFallback serves org's browser-shaped callers: org's
			// two caller-scoped endpoints also serve the team surface's
			// signed-in owner, whose requests carry a bearer token and no
			// X-Demo-User-Id header -- so org's resolver instance falls
			// back to the verified Principal when no demo header is
			// present, the same header-then-Principal shape notes' creator
			// seam gives. The notification and integration modules keep the
			// flag false (their own overrides wire that variant).
			org.WithSubjectResolver(org.SubjectResolverFunc(demo.DemoOrgSubjectResolverFor(b.cfg.DisableDemoUserHeader, true))),
			org.WithMailFrom("invitations@reference-app.example"),
			// Replies to an invitation land in the support inbox rather than
			// on the no-reply-ish sender above. Module-level configuration by
			// design: the inviter's own address is not reachable here.
			org.WithReplyTo("support@reference-app.example"),
			org.WithInvitationLinkBuilder(b.invitationLinkBuilder()),
		}
		return org.NewModule(db, opts...), nil
	}, pkgcore.Requirement{Token: (*config.Module)(nil)})
}

// registerOnlyComponent returns the module's registered descriptor with its
// Init callback reduced to the module's one declaration entry point. It is
// the rbac override's shape: this host attaches rbac's permission-catalog
// snapshot itself, in the post-bootstrap step, after every module's
// declaration turn has run -- its Init-stage consumers (the demo seeds, the
// self-service provisioning chain) need the complete catalog then, and the
// descriptor's own Start callback completes the snapshot as the backstop
// either way. Everything else -- Start included -- is the module's own.
func registerOnlyComponent(reg *pkgcore.ComponentRegistry, moduleName string, declare func(any, *pkgcore.ComponentRegistry) error) (pkgcore.Component, error) {
	base, ok := registeredComponent(reg, moduleName)
	if !ok {
		return pkgcore.Component{}, fmt.Errorf("reference-app: component %q has no registered descriptor", moduleName)
	}
	c := base
	c.Name = hostComponentPrefix + moduleName
	c.Init = func(_ context.Context, reg *pkgcore.ComponentRegistry, instance any) error {
		return declare(instance, reg)
	}
	return c, nil
}

// rbacComponent returns rbac's descriptor declaring only: its permission
// catalog snapshot is taken in the post-bootstrap step (rbacModule.Attach),
// after every module has declared its permissions, because this host's
// Init-stage consumers need the complete catalog while they run. The
// descriptor's own Start callback completes the snapshot as the backstop
// that makes completeness structural rather than a function of plan order.
func (b *serverBuild) rbacComponent(reg *pkgcore.ComponentRegistry) (pkgcore.Component, error) {
	return registerOnlyComponent(reg, "rbac", func(instance any, reg *pkgcore.ComponentRegistry) error {
		m, ok := instance.(*rbac.Module)
		if !ok {
			return fmt.Errorf("reference-app: the rbac component was handed a %T instance, want *rbac.Module", instance)
		}
		return m.Register(reg)
	})
}

// invitationLinkBuilder returns the invitation-link builder org renders into

// invitationLinkBuilder returns the invitation-link builder org renders into
// its invitation mail: the tenant's configured host when it has one, this
// deployment's public origin otherwise (hostByTenant's own doc comment has
// the population split). The link names org's real accept endpoint and
// carries the token as a query parameter, so it is one recognizable string a
// person or a flow test can extract the token from.
func (b *serverBuild) invitationLinkBuilder() org.InvitationLinkBuilder {
	return func(ctx context.Context, token string) (string, error) {
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
		return fmt.Sprintf("%s/api/v1/org/invitations/accept?token=%s", linkBase, url.QueryEscape(token)), nil
	}
}

// subjectResolverComponent returns the caller-identity closure notes' and
// cases' creator seams resolve their callers through -- the X-Demo-User-Id
// header first, then the verified Principal. org, notification and
// integration declare structurally identical seam interfaces but each wires
// its own variant in its own override component (org with the Principal
// fallback its browser-shaped callers need, the other two header-only,
// preserving the pinned subject-less refusal on their surfaces); this
// component exists for the modules that stay on their own descriptors.
func (b *serverBuild) subjectResolverComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:         hostComponentPrefix + "subject-resolvers",
		Capabilities: pkgcore.MultiReplicaSafe,
		Provides: []any{
			(*org.SubjectResolver)(nil),
			(*notification.SubjectResolver)(nil),
			(*integration.SubjectResolver)(nil),
			(*notes.SubjectResolver)(nil),
		},
		New: func(_ context.Context, _ *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
			return demo.DemoNotesSubjectResolver{HeaderDisabled: b.cfg.DisableDemoUserHeader}, nil
		},
	}
}

// rbacSubtreeComponent returns the subtree resolver rbac materializes
// node-scoped bindings with: org's own Scope adapted onto rbac's seam, so a
// binding granted on an organization node resolves against this app's real
// tree rather than denying for want of a resolver.
func (b *serverBuild) rbacSubtreeComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:         hostComponentPrefix + "rbac-subtree",
		Capabilities: pkgcore.MultiReplicaSafe,
		Provides:     []any{(*rbac.SubtreeResolver)(nil)},
		Requires:     []pkgcore.Requirement{{Token: (*org.Module)(nil)}},
		New: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
			orgModule, err := pkgcore.Get[*org.Module](reg)
			if err != nil {
				return nil, err
			}
			return OrgSubtreeResolverFor(orgModule.Scope()), nil
		},
	}
}

// notificationAddressComponent returns notification's user-address resolver:
// the demo directory the reference app ships, the same static table the demo
// notification glue renders addresses from.
func (b *serverBuild) notificationAddressComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:         hostComponentPrefix + "notification-addresses",
		Capabilities: pkgcore.MultiReplicaSafe,
		Provides:     []any{(*notification.UserAddressResolver)(nil)},
		New: func(_ context.Context, _ *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
			return staticaddr.New(demo.DemoUserAddresses), nil
		},
	}
}

// notificationLocaleComponent returns notification's profile-locale
// resolver: authn's user store adapted onto the module's locale seam, read
// lazily so the wiring order is not a hazard.
func (b *serverBuild) notificationLocaleComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:         hostComponentPrefix + "notification-locales",
		Capabilities: pkgcore.MultiReplicaSafe,
		Provides:     []any{(*notification.UserLocaleResolver)(nil)},
		Requires:     []pkgcore.Requirement{{Token: (*authn.Module)(nil)}},
		New: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
			authnModule, err := pkgcore.Get[*authn.Module](reg)
			if err != nil {
				return nil, err
			}
			b.authnUserLocales = demo.AuthnUserLocales{Authn: authnModule}
			return b.authnUserLocales, nil
		},
	}
}

// sharingResourceComponent returns sharing's resource resolver: the
// structurally-typed adapter over storage's ObjectService and the app's
// attestation service (sharing_resolver.go), so a public share's
// ResourceRef resolves through this app's own storage and attestation
// composition -- sharing never imports either.
func (b *serverBuild) sharingResourceComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:         hostComponentPrefix + "sharing-resources",
		Capabilities: pkgcore.MultiReplicaSafe,
		Provides:     []any{(*sharing.ResourceResolver)(nil)},
		Requires: []pkgcore.Requirement{
			{Token: (*storage.Module)(nil)},
			{Token: (*attestation.Service)(nil)},
		},
		New: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
			storageModule, err := pkgcore.Get[*storage.Module](reg)
			if err != nil {
				return nil, err
			}
			attestationService, err := pkgcore.Get[*attestation.Service](reg)
			if err != nil {
				return nil, err
			}
			return &storageSharingResolver{svc: storageModule.ObjectService(), attest: attestationService}, nil
		},
	}
}

// sharingExpiryComponent returns sharing's tenant-config reader: the
// config module's handle adapted onto the module's expiry seam, so a
// tenant's own sharing.default_expiry override governs Service.Create's
// resolved expiry instead of always falling back to the default.
func (b *serverBuild) sharingExpiryComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:         hostComponentPrefix + "sharing-expiry",
		Capabilities: pkgcore.MultiReplicaSafe,
		Provides:     []any{(*sharing.TenantConfigReader)(nil)},
		Requires:     []pkgcore.Requirement{{Token: (*config.Module)(nil)}},
		New: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
			configModule, err := pkgcore.Get[*config.Module](reg)
			if err != nil {
				return nil, err
			}
			return speedbridges.ShareExpiryReader{Handle: configModule.Handle()}, nil
		},
	}
}

// integrationPermissionComponent returns integration's permission lister:
// the seam a scoped API key is created against, reading rbac's runtime
// service lazily at call time. The module's own construction declares the
// lister mandatory, and this app's integration wiring (the override
// component) passes no lister to NewModule, so the module keeps refusing a
// scoped key exactly as it always has; the provided value is there for
// whichever consumer asks.
//
// The component deliberately declares no requirement on rbac: a
// requirement edge would order this provider before rbac's Init turn, and
// rbac's Init turn is where its permission catalog is frozen out of the
// declarations made before it -- pulling rbac forward would freeze the
// catalog before the modules behind it declared their permissions.
func (b *serverBuild) integrationPermissionComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:         hostComponentPrefix + "integration-permissions",
		Capabilities: pkgcore.MultiReplicaSafe,
		Provides:     []any{(*integration.PermissionLister)(nil)},
		New: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
			return integration.PermissionListerFunc(func(ctx context.Context, tenantID, userID string) ([]string, error) {
				rbacService, err := pkgcore.Get[*rbac.Service](reg)
				if err != nil {
					return nil, err
				}
				return rbacService.ListPermissions(ctx, rbac.Subject{
					TenantID: pkgcore.TenantID(tenantID),
					UserID:   userID,
				})
			}), nil
		},
	}
}

// gatewayEntitlementsComponent returns the entitlement gate both halves of
// this app's gateway judging run through: billing's subscription-derived
// EntitlementsService adapted onto the gateway's own seam. The instance is
// also kept on the build state, because smilesim's own pre-flight reads the
// same seam before its credit reservation opens.
func (b *serverBuild) gatewayEntitlementsComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:         hostComponentPrefix + "gateway-entitlements",
		Capabilities: pkgcore.MultiReplicaSafe,
		Provides:     []any{(*aigateway.Entitlements)(nil)},
		Requires:     []pkgcore.Requirement{{Token: (*billing.Module)(nil)}},
		New: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
			billingModule, err := pkgcore.Get[*billing.Module](reg)
			if err != nil {
				return nil, err
			}
			b.gatewayEntitlements = speedbridges.Entitlements(billingModule.Entitlements())
			return b.gatewayEntitlements, nil
		},
	}
}

// gatewayUsageComponent returns the usage recorder every successful gateway
// call records its token usage through: metering's analytics-grade recorder
// adapted onto the gateway's own seam. The events it buffers fold into
// metering_usage_summaries rows once metering's own pipelines run.
func (b *serverBuild) gatewayUsageComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:         hostComponentPrefix + "gateway-usage",
		Capabilities: pkgcore.MultiReplicaSafe,
		Provides:     []any{(*aigateway.UsageRecorder)(nil)},
		Requires:     []pkgcore.Requirement{{Token: (*metering.Module)(nil)}},
		New: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
			meteringModule, err := pkgcore.Get[*metering.Module](reg)
			if err != nil {
				return nil, err
			}
			return speedbridges.UsageRecorder(meteringModule.Recorder()), nil
		},
	}
}

// complianceSharingComponent returns compliance's share creator: sharing's
// own runtime service, the seam that gives the audit-query export leg a real
// delivery path (a single-view share link) instead of refusing every call.
func (b *serverBuild) complianceSharingComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:         hostComponentPrefix + "compliance-sharing",
		Capabilities: pkgcore.MultiReplicaSafe,
		Provides:     []any{(*compliance.SharingCreator)(nil)},
		Requires:     []pkgcore.Requirement{{Token: (*sharing.Module)(nil)}},
		New: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
			sharingModule, err := pkgcore.Get[*sharing.Module](reg)
			if err != nil {
				return nil, err
			}
			return sharingModule.Service(), nil
		},
	}
}

// attestationComponent returns this app's AI-output attestation layer: the
// real consumer of go/pki's X.509 layer, issuing each tenant's certificate
// through the pki module and gating public shares on chain verification. Its
// two boot steps (EnsureSchema, EnsureAuthorityChain) run in the
// post-attach step.
func (b *serverBuild) attestationComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:         hostComponentPrefix + "attestation",
		Capabilities: pkgcore.MultiReplicaSafe,
		Provides:     []any{(*attestation.Service)(nil)},
		Requires: []pkgcore.Requirement{
			{Token: (*pki.Module)(nil)},
			{Token: (*storage.Module)(nil)},
			{Token: (*gorm.DB)(nil)},
		},
		New: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
			pkiModule, err := pkgcore.Get[*pki.Module](reg)
			if err != nil {
				return nil, err
			}
			storageModule, err := pkgcore.Get[*storage.Module](reg)
			if err != nil {
				return nil, err
			}
			db, err := pkgcore.Get[*gorm.DB](reg)
			if err != nil {
				return nil, err
			}
			return attestation.NewService(
				pkiModule.CA(),
				pki.NewCertificateRepository(db),
				storageContentOpener{svc: storageModule.ObjectService()},
				attestation.NewAttestationStore(db),
				db,
			), nil
		},
	}
}

// tenantListerComponent returns the tenant universe the queue's per-tenant
// periodic declarations expand through: the configured host tenants joined
// with go/admin's tenant ledger, so a clinic provisioned at run time is
// swept too.
//
// The admin module is read lazily, at call time, rather than required as a
// construction dependency: admin's own construction requires the queue this
// lister feeds, so a requirement edge here would close the cycle
// queue -> lister -> admin -> queue. The read runs from the queue's
// scheduler, long after every product exists.
func (b *serverBuild) tenantListerComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:         hostComponentPrefix + "tenant-lister",
		Capabilities: pkgcore.MultiReplicaSafe,
		Provides:     []any{(*jobs.TenantLister)(nil)},
		New: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
			return lazyAdminTenantUniverse{configured: b.cfg.HostTenants, reg: reg}, nil
		},
	}
}

// lazyAdminTenantUniverse is the periodic tenant universe with the ledger
// read deferred to call time (see tenantListerComponent's own doc comment
// for why the read cannot be a construction dependency).
type lazyAdminTenantUniverse struct {
	configured map[string]pkgcore.TenantID
	reg        *pkgcore.ComponentRegistry
}

// ListTenants implements jobs.TenantLister: the same universe
// periodicTenantUniverse resolves, over the ledger service read from the
// registry at call time.
func (u lazyAdminTenantUniverse) ListTenants(ctx context.Context) ([]pkgcore.TenantID, error) {
	adminModule, err := pkgcore.Get[*admin.Module](u.reg)
	if err != nil {
		return nil, fmt.Errorf("reference-app: read the admin module for the periodic tenant universe: %w", err)
	}
	return newPeriodicTenantUniverse(u.configured, adminModule.Tenants()).ListTenants(ctx)
}

var _ jobs.TenantLister = lazyAdminTenantUniverse{}

// mailerComponent returns this host's mail transport for a boot that
// supplies its own: the in-process capture double the org invitation suites
// inject, declared Stateless -- the honest capability for a throwaway
// in-process test double. A boot without one selects a registered mailer
// implementation (mailer.smtp or mailer.console) instead.
func (b *serverBuild) mailerComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:         hostComponentPrefix + "mailer",
		Module:       "mailer",
		Provides:     []any{(*pkgcore.Mailer)(nil)},
		Capabilities: pkgcore.Stateless | pkgcore.MultiReplicaSafe,
		New: func(_ context.Context, _ *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
			return b.cfg.Mailer, nil
		},
	}
}

// smsComponent returns this host's SMS transport for a boot that captures
// the delivered messages: the console sender writing to the host's declared
// output, declared Stateless like the registered console sender it stands
// in for. A boot without a captured output selects the registered console
// sender (standard output) or the gateway-backed one.
func (b *serverBuild) smsComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:         hostComponentPrefix + "sms",
		Module:       "sms",
		Provides:     []any{(*pkgcore.SMSSender)(nil)},
		Capabilities: pkgcore.Stateless | pkgcore.MultiReplicaSafe,
		New: func(_ context.Context, _ *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
			return pkgcore.NewConsoleSMSSender(smsOutputFor(b.cfg)), nil
		},
	}
}

// workerComponent returns the host's worker step: the job queue's own
// component owns the dispatcher, the worker pool and the periodic-task
// scheduler, so what is left host-side is the credit-reservation reconciler
// the post-attach step starts -- stopped here so its sweep never outlives
// the assembly -- and the two runtime services whose own background loops
// the registry does not close, because a service published with Put is not a
// constructed component (config's anti-loss poller and rbac's cache
// janitor).
func (b *serverBuild) workerComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:         hostComponentPrefix + "worker",
		Capabilities: pkgcore.MultiReplicaSafe,
		New:          newHostStep,
		Close: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
			var firstErr error
			keepErr := func(err error) {
				if err != nil && firstErr == nil {
					firstErr = err
				}
			}
			if b.smileSimReconcilerStop != nil {
				b.smileSimReconcilerStop()
			}
			configService, hasConfig, err := pkgcore.GetOptional[*config.Service](reg)
			if err != nil {
				keepErr(err)
			}
			if hasConfig {
				keepErr(configService.Close())
			}
			rbacService, hasRBAC, err := pkgcore.GetOptional[*rbac.Service](reg)
			if err != nil {
				keepErr(err)
			}
			if hasRBAC {
				keepErr(rbacService.Close())
			}
			return firstErr
		},
	}
}

// hostWiringComponents returns the host's provider and override components,
// in an order whose independent components plan in registration order. The
// registry is the assembly instance the override components copy their
// modules' descriptors from.
//
// Every module descriptor this host selects declares its own capabilities
// -- the modules own that declaration -- so the only capability this file
// states itself is the host's own: each provider below adapts a
// shared-database service or hands each unit of work straight out, holding
// nothing a second replica would silently split, and says so on its own
// descriptor.
func (b *serverBuild) hostWiringComponents(reg *pkgcore.ComponentRegistry) ([]pkgcore.Component, error) {
	components := []pkgcore.Component{b.dbComponent(), b.cryptoComponent()}

	for _, build := range []func(*pkgcore.ComponentRegistry) (pkgcore.Component, error){
		b.authnComponent,
		b.orgComponent,
		b.rbacComponent,
		b.notificationComponent,
		b.integrationComponent,
		b.aiGatewayComponent,
	} {
		c, err := build(reg)
		if err != nil {
			return nil, err
		}
		components = append(components, c)
	}

	// signer.local is the one module descriptor whose capability stays
	// deliberately undeclared (an in-process signer); this host selects its
	// own copy carrying the placement decision.
	signerLocal, err := capabilityComponent(reg, "signer.local")
	if err != nil {
		return nil, err
	}
	components = append(components, signerLocal)

	components = append(components,
		b.tenancyResolverComponent(),
		b.subjectResolverComponent(),
		b.rbacSubtreeComponent(),
		b.notificationAddressComponent(),
		b.notificationLocaleComponent(),
		b.sharingResourceComponent(),
		b.sharingExpiryComponent(),
		b.integrationPermissionComponent(),
		b.gatewayEntitlementsComponent(),
		b.gatewayUsageComponent(),
		b.complianceSharingComponent(),
		b.attestationComponent(),
		b.tenantListerComponent(),
	)
	if b.cfg.Mailer != nil {
		components = append(components, b.mailerComponent())
	}
	if b.cfg.SMSOutput != nil {
		components = append(components, b.smsComponent())
	}
	return components, nil
}
