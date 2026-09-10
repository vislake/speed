// This file is the reference app's assembly core: ServerConfig's bootstrap
// resolution (ConfigFromEnv), the full module composition (BuildServer) and
// the host-side route guards and demo identity layer that wiring uses. It
// sits in internal/app so the composed server is importable: cmd/server's
// main.go boots it as the thin process shell, and the assembly-flow suites,
// which live beside the command in cmd/server, exercise the package through
// the command's tests. See doc.go for the package's design.

package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/admin"
	aigateway "github.com/vislake/speed/go/ai-gateway"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/compliance"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"

	// Blank-imported for its init side effect: registers dbkit.DialectSQLite
	// so dbkit.Open below has a driver to build from -- the reference app
	// runs its own database in standalone deployment mode's SQLite dialect
	// regardless of which deployment mode its other infrastructure seams
	// compose under.
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
	"github.com/vislake/speed/go/integration"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/metering"
	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/notification/staticaddr"

	// Blank-imported for its init() side effect: obs.Init's local
	// exporters wire a real /metrics scrape endpoint only when a local
	// metrics reader has been registered (go/observability's own doc
	// comment on Init and RegisterLocalMetricsReader) -- this is what
	// MetricsHandler below actually serves once main.go's run has called
	// obs.Init. Without this import, obs.Init still runs (traces and
	// metrics both go to stdout), but MetricsHandler answers 404.
	_ "github.com/vislake/speed/go/observability/exporter/prometheus"

	// Blank-imported for its init() side effect: registers the OTLP/gRPC
	// exporter factory obs.Init consults exactly when a caller supplies a
	// non-empty WithOTLPEndpoint (go/observability's ErrOTLPExporterNotRegistered
	// names this import as the fix). Without it, an APP_OTLP_ENDPOINT set
	// in ConfigFromEnv would fail run()'s Init with that error instead of
	// wiring the collector push the variable promises. Registration is
	// inert while the endpoint stays unset: Init stays on the local
	// exporters, byte-identical to this import never having existed.
	_ "github.com/vislake/speed/go/observability/exporter/otlp"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	eventbusredis "github.com/vislake/speed/go/pkgcore/eventbus/redis"
	kvredis "github.com/vislake/speed/go/pkgcore/kv/redis"
	objectstores3 "github.com/vislake/speed/go/pkgcore/objectstore/s3"
	"github.com/vislake/speed/go/pki"
	"github.com/vislake/speed/go/rbac"
	"github.com/vislake/speed/go/sharing"
	"github.com/vislake/speed/go/storage"
	"github.com/vislake/speed/go/tenancy"

	"github.com/vislake/speed/examples/reference-app/internal/attestation"
	"github.com/vislake/speed/examples/reference-app/internal/cases"
	"github.com/vislake/speed/examples/reference-app/internal/consult"
	"github.com/vislake/speed/examples/reference-app/internal/demo"
	"github.com/vislake/speed/examples/reference-app/internal/hostcore"
	"github.com/vislake/speed/examples/reference-app/internal/notes"
	"github.com/vislake/speed/examples/reference-app/internal/smilesim"
)

const (
	// DefaultPort is used when the PORT environment variable is unset.
	DefaultPort = "8080"

	// DefaultSQLitePath is used when APP_DB_PATH is unset. It is a
	// relative path so `go run ./cmd/server` works with zero setup -- the
	// example's own entry point must stay runnable with nothing
	// configured.
	DefaultSQLitePath = "reference-app.db"
)

// The host-neutral kernel this app shares with every generated project --
// the liveness endpoints and their pre-auth allowlist entries, the
// route-mounting rule, the mounted-route label seed, the path/timeout
// constants and the serve/graceful-shutdown lifecycle -- lives in
// internal/hostcore, byte-identical to the copy `saasctl new` embeds
// (tools/check_host_core_parity.py enforces the equality). What this file
// keeps is the host-specific half: ServerConfig's defaults above, this
// app's own composition (BuildServer), and its host-owned route guards
// and demo identity layer.

// DevConfigKey is the master key used when APP_CONFIG_KEY is unset. It is
// the ascending 0x00..0x1f byte sequence -- a recognizable constant, never
// a secret -- because zero-setup standalone development (`go run
// ./cmd/server`, `task dev`, this app's tests) must work with no
// environment at all, while config's Sensitive items demand a real
// 32-byte key the moment one is declared (Attach fails with
// ErrCipherRequired otherwise).
//
// This default is a documented trade-off, not a pattern to copy: it is a
// key committed to the repository, which real hosts must never do. A real
// deployment must set APP_CONFIG_KEY from a secret store (or refuse to
// start); the constant exists so the *demo* keeps working out of the box,
// and its name and doc comment are the guard rails that keep it honest.
var DevConfigKey = []byte{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
	0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
}

// DevOrgIndexKey is the HMAC key used when APP_ORG_INDEX_KEY is unset --
// the descending 0xff..0xe0 byte sequence, chosen precisely so it is
// visibly a DIFFERENT 32 bytes from DevConfigKey's ascending 0x00..0x1f
// (see the Org field's own doc comment (bootstrap.go) for why the two must
// never be the same secret). Like DevConfigKey, this is a recognizable
// constant for
// zero-setup standalone development, never a secret a real deployment
// should keep.
var DevOrgIndexKey = []byte{
	0xff, 0xfe, 0xfd, 0xfc, 0xfb, 0xfa, 0xf9, 0xf8,
	0xf7, 0xf6, 0xf5, 0xf4, 0xf3, 0xf2, 0xf1, 0xf0,
	0xef, 0xee, 0xed, 0xec, 0xeb, 0xea, 0xe9, 0xe8,
	0xe7, 0xe6, 0xe5, 0xe4, 0xe3, 0xe2, 0xe1, 0xe0,
}

// DevPKILocalKeyCipherKey, DevBlindIndexKey and DevPIICipherKey are authn's
// (and pki's) own committed-key placeholders, the same documented trade-off
// as DevConfigKey immediately above -- a real deployment must replace every
// one of them with real secret-manager material, never commit real keys the
// way this demo commits these. Each has a real override path
// (APP_PKI_LOCAL_KEY_CIPHER_KEY, APP_AUTHN_BLIND_INDEX_KEY and
// APP_AUTHN_PII_CIPHER_KEY, the Authn and Pki fields of the bootstrap target;
// or APP_ROOT_KEY deriving all three at once -- see loadHostConfig's own doc
// comment (bootstrap.go) for the root-key wiring and the three-tier
// precedence it applies); the loader consults those three variables while
// resolving the fields, with the Dev* struct defaults (hostConfigDefaults)
// standing as its lowest tier -- never a direct environment read from
// BuildServer.
//
// Each protects something different and each MUST stay stable across
// restarts for a different reason: DevPKILocalKeyCipherKey seals go/pki's
// LocalSigner private-key column (pki_local_keys, via
// pki.RegisterLocalKeySerializer) -- the signing key itself is generated
// once by pki.Service.EnsurePurpose (this file's authn.WithKeySource wiring
// below) and PERSISTS in cfg.SQLitePath across restarts; authn.WithBlindIndexKey's
// key must stay IDENTICAL across restarts or every already-stored
// email/phone blind index becomes unfindable; and DevPIICipherKey seals
// authn's encrypted PII columns (email, phone, TOTP secrets) via
// authn.RegisterPIISerializer, deliberately a DIFFERENT key from
// DevConfigKey -- dbkit's own key-separation rule (never let one key double
// as two different AEAD constructions) applies across modules, not only
// within one.
var (
	DevPKILocalKeyCipherKey = []byte{
		0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27,
		0x28, 0x29, 0x2a, 0x2b, 0x2c, 0x2d, 0x2e, 0x2f,
		0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37,
		0x38, 0x39, 0x3a, 0x3b, 0x3c, 0x3d, 0x3e, 0x3f,
	}
	DevBlindIndexKey = []byte{
		0x40, 0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47,
		0x48, 0x49, 0x4a, 0x4b, 0x4c, 0x4d, 0x4e, 0x4f,
		0x50, 0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x57,
		0x58, 0x59, 0x5a, 0x5b, 0x5c, 0x5d, 0x5e, 0x5f,
	}
	DevPIICipherKey = []byte{
		0x60, 0x61, 0x62, 0x63, 0x64, 0x65, 0x66, 0x67,
		0x68, 0x69, 0x6a, 0x6b, 0x6c, 0x6d, 0x6e, 0x6f,
		0x70, 0x71, 0x72, 0x73, 0x74, 0x75, 0x76, 0x77,
		0x78, 0x79, 0x7a, 0x7b, 0x7c, 0x7d, 0x7e, 0x7f,
	}
)

// DevNotificationIndexKey is the HMAC key used when
// APP_NOTIFICATION_INDEX_KEY is unset -- the ascending 0x80..0x9f byte
// sequence, the next free 32-byte region of this file's recognizable
// constants and chosen so it is visibly a DIFFERENT 32 bytes from every key
// above it (see the Notification field's own doc comment (bootstrap.go) for
// why the
// notification index key and the config cipher key must never be the same
// secret). Like its siblings, this is a recognizable constant for
// zero-setup standalone development, never a secret a real deployment
// should keep.
var DevNotificationIndexKey = []byte{
	0x80, 0x81, 0x82, 0x83, 0x84, 0x85, 0x86, 0x87,
	0x88, 0x89, 0x8a, 0x8b, 0x8c, 0x8d, 0x8e, 0x8f,
	0x90, 0x91, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97,
	0x98, 0x99, 0x9a, 0x9b, 0x9c, 0x9d, 0x9e, 0x9f,
}

// DemoHostTenants is a hard-coded, obviously-temporary Host -> TenantID
// lookup. It exists only so this reference app has *some* way to render a
// tenant-specific brand on the config module's pre-auth display endpoints
// (configModule's tenancy.NewDomainResolver wiring in BuildServer below)
// without a real custom-domain table.
//
// This is a placeholder, not a pattern to copy into a real deployment: a
// real Resolver must derive the tenant from a source the server itself
// controls -- never an unauthenticated, static Host map like this one,
// which anyone can trigger just by setting the Host header on an HTTP
// request. See go/tenancy/resolver.go's own Resolver doc comment for the
// same rule stated as a hard requirement on every implementation.
//
// Host does NOT select the tenant for anything else in this app. The notes
// API, and every other route this app protects, resolve their tenant from
// the caller's ACCESS TOKEN instead (authn.NewPrincipalResolver, wired
// below) -- an unauthenticated caller cannot choose a tenant just by
// setting Host, which a Host-keyed lookup like this one would otherwise
// allow. See BuildServer's middleware-chain doc comment for the full
// reasoning.
var DemoHostTenants = map[string]pkgcore.TenantID{
	"acme.demo.localhost":   "tenant-acme",
	"globex.demo.localhost": "tenant-globex",
}

// signInMemberships -- the authn.MembershipReader this app wires, whose
// customer-tenant answers read org's own memberships table live -- lives
// in sign_in_memberships.go with its full rationale. The short version of
// why host glue must exist here at all is structural: authn never imports
// org -- the two sit at the same dependency tier, peers, neither importing
// the other -- so whatever answers authn's two membership questions must
// be supplied by the assembling application, exactly like
// DemoNotesSubjectResolver and OrgSubtreeResolver below are. The app's
// membership store starts empty, and who fills it depends on the boot:
//
//   - Every boot seeds the fixed demo header actors' rbac grants
//     (seedDemoGrants) but NO memberships: those actors have no database
//     row, so nothing can sign in as them.
//   - A boot with APP_DEMO_USERS_PASSWORD set additionally registers the
//     three demo accounts of demo_users.go through the real register route
//     and places each into org's memberships table under every tenant its
//     actor model names -- which is what makes those real sign-ins succeed,
//     in this process and in any later one against the same database
//     (authn's resolveTenant refuses an account with no membership:
//     go/authn/service.go's nil-or-unseeded MembershipReader answer refuses
//     rather than allows, and the org rows are that answer now).
//   - Tests grant membership explicitly after registering an account
//     through the real HTTP surface (registerAndAuthenticate in
//     flowtests/server_test.go, flowtests/authn_e2e_test.go), keeping a reference to the same
//     store BuildServer itself wires.

// DemoOrgUserHeader is the header DemoOrgSubjectResolver reads to identify the
// HTTP caller: the stand-in for the verified access-token claims a real
// deployment's resolver would read, in exactly the spirit of
// DemoHostTenants' own disclaimer above. A caller sets it to whatever user
// id it wants to act as, with no verification whatsoever -- which is fine
// for this reference app's own demonstration purposes and would be a
// critical vulnerability in any real deployment.
const DemoOrgUserHeader = "X-Demo-User-Id"

// DemoOrgSubjectResolver supplies the caller identity to every module that
// declares the same structurally identical SubjectResolver seam: org's two
// caller-scoped endpoints (creating and accepting an invitation), every
// notification endpoint (which resolves its caller's inbox, contacts and
// preferences through it) and integration's creator reads. It exists only
// so this reference app has *some* way to demonstrate those endpoints end
// to end through a caller-chosen identity.
//
// In its default, header-enabled wiring, who a caller is is the
// X-Demo-User-Id header value, and nothing else: the resolver never falls
// back to the verified Principal authn.Middleware leaves in the request
// context, and an authenticated caller with no demo header gets the
// module's own per-operation 401 (notification.subject_unresolved,
// integration.subject_unresolved, org's sibling), never a fabricated user
// id. That header-only refusal is pinned behaviour of this app's rig
// (flowtests/notification_flow_test.go's subject-less leg; demoRouteGuards names the
// path routePublic for the same reason) -- for notification's and
// integration's surfaces it stays exactly that, because no browser-shaped
// flow reaches them.
//
// The principalFallback field is a deliberate exception for ORG's wiring
// alone, the same shape DemoNotesSubjectResolver's
// header-then-Principal fallback gives notes' and cases' creator
// seams: org's caller-scoped endpoints also serve browser-shaped callers --
// a signed-in clinic owner whose requests carry a bearer token and no demo
// header -- so the org module is wired with principalFallback set, and a
// header-less request with a verified Principal resolves as that
// Principal's user. The field's zero value (notification's, integration's
// and every test's instance) keeps the header-only resolver,
// preserving the pinned refusal where it belongs.
//
// The headerDisabled field is the other deliberate exception to the
// "never falls back to the Principal" rule: an operator who sets
// APP_DISABLE_DEMO_USER_HEADER has declared this deployment reads no demo
// header at all, so the only identity left to resolve a caller from is the
// verified Principal.
//
// This is a placeholder, not a pattern to copy into a real deployment: a
// real SubjectResolver must derive the caller from a source the server
// itself verified (a validated access token's subject claim), never an
// unauthenticated, client-supplied header like this one -- see
// org.SubjectResolver's own doc comment for the same rule stated as a hard
// requirement.
//
// headerDisabled carries the value of cfg.DisableDemoUserHeader
// (APP_DISABLE_DEMO_USER_HEADER) BuildServer wired this resolver with. The
// zero value keeps the header-only resolver; the disabled wiring is what
// an operator setting the kill switch gets, and it
// is the point of this field: it is what extends the kill switch -- which
// alone would only reach DemoUserHeader in the rbac gate -- to this
// resolver (and the org, notification and integration surfaces it serves),
// which would otherwise honor X-Demo-User-Id unconditionally. With
// headerDisabled set, Subject reads
// no header at all and resolves the caller from the verified authn
// Principal alone -- the identity authn.Middleware proved -- failing
// closed exactly like the header-only shape when no Principal exists. See
// the DisableDemoUserHeader field's own doc comment (bootstrap.go) for the
// full contract.
type DemoOrgSubjectResolver struct {
	// HeaderDisabled carries cfg.DisableDemoUserHeader
	// (APP_DISABLE_DEMO_USER_HEADER): when true, Subject reads no demo
	// header at all and resolves the verified authn Principal alone,
	// failing closed without one -- see the type's own doc comment for
	// the full contract. The zero value keeps the header-first resolver.
	HeaderDisabled bool

	// PrincipalFallback lets ORG's wiring (this file's org.NewModule
	// option) resolve a header-less request from the verified Principal
	// authn.Middleware left in the request context -- the browser-shaped
	// caller org's team surface needs. Zero value keeps the
	// header-only contract (see the type's own doc comment).
	PrincipalFallback bool
}

// Subject implements org.SubjectResolver, notification.SubjectResolver and
// integration.SubjectResolver -- the three modules that declare the
// identical seam, since all three share the identical
// (r *http.Request) (string, bool) shape with the identical fail-closed
// contract, and this app already has one instance to hand each of them. It
// fails closed: no header, no verified Principal in the wiring that reads
// one, reports ("", false), and the module's own per-operation refusal
// (notification.subject_unresolved,
// integration.subject_unresolved, org's sibling) is what a caller then
// sees.
//
// Resolution order in the header-enabled wiring: the X-Demo-User-Id header
// when present (the pre-auth flows' affordance); else, when
// principalFallback is set (org's wiring only), the verified Principal;
// else fail closed. In the headerDisabled wiring, the verified Principal
// alone.
func (r DemoOrgSubjectResolver) Subject(req *http.Request) (string, bool) {
	if !r.HeaderDisabled {
		if userID := req.Header.Get(DemoOrgUserHeader); userID != "" {
			return userID, true
		}
		if r.PrincipalFallback {
			if principal, ok := authn.PrincipalFromContext(req.Context()); ok && principal.UserID != "" {
				return principal.UserID, true
			}
		}
		return "", false
	}
	principal, ok := authn.PrincipalFromContext(req.Context())
	if !ok || principal.UserID == "" {
		return "", false
	}
	return principal.UserID, true
}

// compile-time checks that DemoOrgSubjectResolver satisfies the identical
// SubjectResolver seam the three modules it serves declare.
var (
	_ org.SubjectResolver          = DemoOrgSubjectResolver{}
	_ notification.SubjectResolver = DemoOrgSubjectResolver{}
	_ integration.SubjectResolver  = DemoOrgSubjectResolver{}
)

// DemoNotesSubjectResolver is what notes' create handler resolves the
// creating user from -- the notes.NewModule option BuildServer wires
// below. It is DemoOrgSubjectResolver's behavior plus one source: like
// its sibling it reads the X-Demo-User-Id header first, the attribution
// affordance every flow helper in this package sends and the namespace
// demo_notification.go's address table keys on; and only when no header
// is present does it fall back to the verified Principal authn.Middleware
// left in the request context. The fallback is what lets a browser-shaped
// caller with no demo header create notes: the accounts demo_users.go
// seeds (real users acting through their access tokens) are attributed
// through it, exactly as they pass the rbac gate through DemoSubjectResolver's
// own fallback, and the note-created events their creates publish name
// their real user ids -- which resolve to no notification addresses, an
// ordinary skip (see demo_notification.go's DemoUserAddresses).
//
// Notes' creator seam is not alone in having this second source:
// org's caller-scoped endpoints share it -- the org module's
// DemoOrgSubjectResolver instance is wired with the type's
// principalFallback field set, giving org the
// identical header-then-Principal shape (see DemoOrgSubjectResolver's own
// doc comment). The notification module's caller-scoped endpoints keep the
// header-only read, because its subject-less refusal is a pinned behaviour
// of this app's rig -- and a request that reaches those surfaces without
// the header stays refused rather than acting as the principal's user id.
//
// This is a placeholder, not a pattern to copy into a real deployment:
// the header is exactly as unverifiable here as in DemoOrgSubjectResolver,
// and a real deployment's resolver reads the creating user from the
// verified token the notes create handler already stands behind.
//
// headerDisabled carries the value of cfg.DisableDemoUserHeader
// (APP_DISABLE_DEMO_USER_HEADER) BuildServer wired this resolver with --
// the sibling of DemoOrgSubjectResolver's own field of the same name, and
// what extends the kill switch to this resolver: without it, a caller
// could still name any creator through X-Demo-User-Id on notes' and
// cases' surfaces while the rbac header alone was disabled. The
// zero value keeps the header-then-Principal resolver;
// with headerDisabled set, Subject skips the header read entirely
// and resolves the caller from the verified authn Principal alone, failing
// closed exactly like the default shape when no Principal exists.
type DemoNotesSubjectResolver struct {
	// HeaderDisabled carries cfg.DisableDemoUserHeader
	// (APP_DISABLE_DEMO_USER_HEADER): when true, Subject skips the
	// header read and resolves the verified authn Principal alone,
	// failing closed without one -- see the type's own doc comment for
	// the full contract. The zero value keeps the header-first resolver.
	HeaderDisabled bool
}

// Subject implements notes.SubjectResolver (and the cases package's
// identical copy of the seam -- see the compile-time check at the bottom
// of cases.go). It fails closed: no header (and, in the
// headerDisabled wiring, no verified Principal) reports ("", false), and
// the module's own per-operation refusal (notes.subject_unresolved,
// cases.subject_unresolved) is what a caller then sees.
func (r DemoNotesSubjectResolver) Subject(req *http.Request) (string, bool) {
	userID := ""
	if !r.HeaderDisabled {
		userID = req.Header.Get(DemoOrgUserHeader)
	}
	if userID == "" {
		principal, ok := authn.PrincipalFromContext(req.Context())
		if !ok || principal.UserID == "" {
			return "", false
		}
		userID = principal.UserID
	}
	return userID, true
}

// compile-time check that DemoNotesSubjectResolver satisfies notes' own
// copy of the seam.
var _ notes.SubjectResolver = DemoNotesSubjectResolver{}

// OrgFeatureGate adapts a *config.Service that is filled in AFTER this
// app's org.Module -- and its authn.Module -- are constructed into the
// modules' FeatureGate seams, read lazily -- the
// same "read a host seam at call time, never capture it at construction"
// idiom go/org's own hostSeams applies throughout the module (see
// go/org/events.go's doc comment on hostSeams for the identical reasoning).
//
// It exists because of a real ordering constraint in BuildServer: the
// config module's Service is only produced by configModule.Attach, which
// per its own contract runs strictly AFTER Kernel.Bootstrap returns -- and
// org.Module must already be part of that same Bootstrap call so its
// permissions, audit actions, events and routes are declared. Passing the
// *config.Service variable directly to org.WithFeatureGate before Attach
// has run would capture a non-nil FeatureGate interface wrapping a nil
// *config.Service pointer, which panics the moment anything calls
// IsEnabled on it. Holding a pointer to the variable instead, and
// dereferencing it only when IsEnabled is actually called (during a real
// HTTP request, long after BuildServer has finished wiring), sidesteps the
// ordering problem entirely.
type OrgFeatureGate struct{ Service **config.Service }

// IsEnabled implements org.FeatureGate -- and, identically,
// authn.FeatureGate, which is the same declaration under a different
// module: go/authn's copy of the seam (go/authn/module.go) mirrors org's
// shape exactly, and the two modules never import each other. One
// adapter therefore serves both gates, and BuildServer passes the same
// OrgFeatureGate value to org.WithFeatureGate and to authn.WithFeatureGate
// (see the authn wiring below).
func (g OrgFeatureGate) IsEnabled(ctx context.Context, key string) (bool, error) {
	svc := *g.Service
	if svc == nil {
		return false, fmt.Errorf("reference-app: the config service is not attached yet")
	}
	return svc.IsEnabled(ctx, key)
}

// compile-time checks that OrgFeatureGate satisfies org.FeatureGate and
// authn.FeatureGate, the two identical no-import declarations it serves.
var (
	_ org.FeatureGate   = OrgFeatureGate{}
	_ authn.FeatureGate = OrgFeatureGate{}
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
// ai-gateway boot credential write and the seedDemo* steps below: this
// app's composition declares its channels, and a restart re-affirms that
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

// OrgSubtreeResolver adapts org.Scope's Path method onto rbac.SubtreeResolver's
// NodePath -- the two no-import seams differ just enough (three return
// values vs. two; "not found" folded into an error vs. a plain boolean)
// that a one-line wrapper is needed, unlike OrgFeatureGate/FeatureGate's
// exact structural match above. org.Scope is safe to hold directly (unlike
// *config.Service above): orgModule.Scope() is available the moment
// org.NewModule returns, with no Attach-ordering constraint, so this
// adapter captures it at construction rather than reading it lazily.
type OrgSubtreeResolver struct{ Scope org.Scope }

// NodePath implements rbac.SubtreeResolver.
func (r OrgSubtreeResolver) NodePath(ctx context.Context, nodeID string) (string, bool, error) {
	path, err := r.Scope.Path(ctx, nodeID)
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

// compile-time check that OrgSubtreeResolver satisfies rbac.SubtreeResolver.
var _ rbac.SubtreeResolver = OrgSubtreeResolver{}

// readTenantDurationConfig is the shared core SharingConfigReader and
// ComplianceConfigReader below both call into: it resolves key's
// tenant-then-system-then-schema-default effective value through cfg
// (go/config's own three-tier resolution, Service.Get's own doc comment)
// and reports ok = false when it resolved at the schema default -- i.e.
// neither a tenant nor a system row exists at all -- exactly the
// "tenant has configured none" signal both go/sharing's TenantConfigReader
// and go/compliance's ExportDeliveryExpiryReader document. Using Get
// (rather than the type-erasing config.GetTyped) is what makes that
// distinction visible at all: GetTyped throws away Value.Scope, so it
// cannot tell a genuinely unset key from one that happens to resolve to
// its own declared Default.
func readTenantDurationConfig(ctx context.Context, cfg *config.Service, key string, tenant pkgcore.TenantID) (time.Duration, bool, error) {
	tenantCtx := pkgcore.WithTenant(ctx, tenant)
	v, err := cfg.Get(tenantCtx, key)
	if err != nil {
		return 0, false, err
	}
	if v.Scope == "" {
		// Resolved to the schema default: no explicit tenant or system row
		// exists, so report "unconfigured" and let the caller's own
		// fallback (identical to that same schema Default) apply, exactly
		// as if no reader were wired at all.
		return 0, false, nil
	}
	d, ok := v.Data.(time.Duration)
	if !ok {
		return 0, false, fmt.Errorf("reference-app: config key %q did not resolve to a duration value", key)
	}
	return d, true, nil
}

// SharingConfigReader adapts a *config.Service that is filled in AFTER
// this app's sharing.Module is constructed into sharing.TenantConfigReader,
// read lazily -- the identical ordering problem and the identical fix
// OrgFeatureGate's own doc comment explains at length: configModule.Attach
// (which produces the real *config.Service) runs strictly after
// Kernel.Bootstrap returns, and sharing.Module must already be part of
// that same Bootstrap call, so sharing.WithTenantConfigReader has to
// receive something today that becomes live only later. Holding a
// pointer to the configService variable, and dereferencing it only when
// ShareDefaultExpiry is actually called (during a real request, long
// after BuildServer has finished wiring), sidesteps the ordering problem
// exactly like OrgFeatureGate does for org.FeatureGate.
type SharingConfigReader struct{ Service **config.Service }

// ShareDefaultExpiry implements sharing.TenantConfigReader.
func (r SharingConfigReader) ShareDefaultExpiry(ctx context.Context, tenant pkgcore.TenantID) (time.Duration, bool, error) {
	svc := *r.Service
	if svc == nil {
		return 0, false, fmt.Errorf("reference-app: the config service is not attached yet")
	}
	return readTenantDurationConfig(ctx, svc, sharing.ConfigDefaultExpiry, tenant)
}

// compile-time check that SharingConfigReader satisfies
// sharing.TenantConfigReader.
var _ sharing.TenantConfigReader = SharingConfigReader{}

// ComplianceConfigReader is compliance's own copy of SharingConfigReader,
// adapting the same lazily-filled *config.Service into
// compliance.ExportDeliveryExpiryReader. It is a separate type, not a
// shared one, because the two seams are structurally different Go
// interfaces (ShareDefaultExpiry vs ExportDeliveryExpiry, per each
// module's own declared method name) even though their bodies both do
// nothing but call readTenantDurationConfig with a different config
// key -- forcing one type to implement both method names would be a
// coincidental unification neither module's own design asked for.
type ComplianceConfigReader struct{ Service **config.Service }

// ExportDeliveryExpiry implements compliance.ExportDeliveryExpiryReader.
func (r ComplianceConfigReader) ExportDeliveryExpiry(ctx context.Context, tenant pkgcore.TenantID) (time.Duration, bool, error) {
	svc := *r.Service
	if svc == nil {
		return 0, false, fmt.Errorf("reference-app: the config service is not attached yet")
	}
	return readTenantDurationConfig(ctx, svc, compliance.ConfigExportDeliveryExpiry, tenant)
}

// compile-time check that ComplianceConfigReader satisfies
// compliance.ExportDeliveryExpiryReader.
var _ compliance.ExportDeliveryExpiryReader = ComplianceConfigReader{}

// ServerConfig is main.go's own bootstrap wiring configuration -- the
// values a process must know before anything else can start (deployment
// mode, port, database path, the config master key, the optional Redis
// address, the demo host map). It is a plain struct resolved from the
// process environment by ConfigFromEnv's loader-driven bootstrap
// (bootstrap.go), NOT the dynamic configuration the config
// module serves: dynamic configuration lives in the configs table and can
// never hold the very key that encrypts it, so this bootstrap struct is
// the deliberate exception to "a plain struct, not pkgcore/config's
// dynamic configuration" -- it is main.go's own wiring, which never goes
// through Module.Register either.
type ServerConfig struct {
	DeploymentMode       pkgcore.DeploymentMode
	Port                 string
	SQLitePath           string
	ConfigKey            []byte
	OrgIndexKey          []byte
	NotificationIndexKey []byte

	// PKILocalKeyCipherKey, AuthnBlindIndexKey and AuthnPIICipherKey are
	// the three key materials whose environment overrides arrive through
	// APP_PKI_LOCAL_KEY_CIPHER_KEY, APP_AUTHN_BLIND_INDEX_KEY and
	// APP_AUTHN_PII_CIPHER_KEY -- see the Authn and Pki fields' own doc
	// comments (bootstrap.go) for what each
	// protects and why each is a separate secret.
	// ConfigFromEnv resolves all six key fields on this struct (these
	// three plus ConfigKey/OrgIndexKey/NotificationIndexKey above) through
	// the same three-tier precedence: an explicitly-set individual
	// environment variable wins over a APP_ROOT_KEY derivation, which
	// wins over the hardcoded development default.
	PKILocalKeyCipherKey []byte
	AuthnBlindIndexKey   []byte
	AuthnPIICipherKey    []byte

	RedisAddr   string
	HostTenants map[string]pkgcore.TenantID

	// OTLPEndpoint is the "host:port" target this deployment pushes its
	// traces and metrics to over OTLP/gRPC when non-empty (see the
	// OTLPEndpoint field's own doc comment (bootstrap.go) for what an empty
	// value means). ConfigFromEnv fills it from APP_OTLP_ENDPOINT; main.go's run
	// hands it to obs.Init through obs.WithOTLPEndpoint, exactly the shape
	// cfg.RedisAddr demonstrates for the Redis-backed seams.
	OTLPEndpoint string

	// PublicOrigin is this deployment's own public origin (scheme://host,
	// optional :port, no path), the fallback base URL org's invitation
	// accept links point at for a tenant that has no branded host in
	// HostTenants -- every self-registered clinic, whose tenant id
	// self_service.go derives from its registrant and which no configured
	// host can name (see the PublicOrigin field's own doc comment
	// (bootstrap.go) for the
	// full population split). ConfigFromEnv fills it from APP_PUBLIC_ORIGIN,
	// defaulting to "http://localhost:" + the resolved PORT so a
	// zero-setup local demo renders working links with no configuration;
	// the field's zero value keeps the fail-loud behavior for a
	// host that sets neither a branded host nor an origin, and the error
	// such a link build produces names this variable.
	PublicOrigin string

	// S3Endpoint, S3Bucket, S3AccessKey, S3SecretKey, S3Region and S3UseSSL
	// compose a real S3-compatible ObjectStore for the "objectstore" seam
	// (objectstore/s3.NewObjectStore) when S3Endpoint is non-empty --
	// the S3 fields' own doc comment (bootstrap.go) has the completeness
	// rule.
	// Empty S3Endpoint (the default) leaves "objectstore" on the Preset's
	// local-directory default.
	S3Endpoint  string
	S3Bucket    string
	S3AccessKey string
	S3SecretKey string
	S3Region    string
	S3UseSSL    bool

	// ObjectStoreRoot, when non-empty, replaces the "objectstore" seam's
	// Preset default with pkgcore.NewLocalObjectStore over this fixed
	// directory -- the local twin of the S3 fields above, and an
	// alternative to them (ConfigFromEnv refuses both a complete S3
	// composition and APP_OBJECT_STORE_ROOT, per the ObjectStoreRoot field's
	// own doc comment (bootstrap.go)). BuildServer injects it declaring the
	// SurvivesRestart
	// capability alone, never MultiReplicaSafe: the directory survives a
	// process restart, but nothing about a single-process local store is
	// replica-safe. The field exists because a host that needs its objects
	// to outlive one process must name the directory itself -- the Preset
	// default is a throwaway MkdirTemp -- which is exactly what the
	// two-boot expiry-sweep flow test (flowtests/periodic_scheduler_flow_test.go)
	// needs: boot 2's sweep must find the bytes boot 1 wrote.
	ObjectStoreRoot string

	// SMTPHost, SMTPPort, SMTPUsername and SMTPPassword compose a real SMTP
	// Mailer for the "mailer" seam (pkgcore.NewSMTPMailer) when SMTPHost is
	// non-empty -- the SMTP fields' own doc comment (bootstrap.go) has the
	// completeness rule.
	// Empty SMTPHost (the default) leaves "mailer" on the Preset's
	// console default, exactly like an unset Mailer field below.
	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string

	// SMSGatewayURL composes the real HTTP SMS transport
	// (pkgcore.NewHTTPSMSSender) for authn's "SMS sender" seam when
	// non-empty. See the SMSGatewayURL field's own doc comment (bootstrap.go)
	// for what an
	// empty value means under each deployment mode.
	SMSGatewayURL string

	// DisableQueueWorker, when true, makes BuildServer skip
	// standaloneQueue.Start -- see the DisableQueueWorker field's own doc
	// comment (bootstrap.go) for why this exists and what it changes.
	// ConfigFromEnv sets it
	// from APP_DISABLE_QUEUE_WORKER; false (the default) starts the queue
	// worker normally.
	DisableQueueWorker bool

	// FailSelfServiceProvision is the failure-injection hook for the
	// self-service provisioning chain (self_service.go): when non-nil,
	// every provisioning attempt the server makes consults it at the top
	// of provision and fails when it returns an error, so a failure can
	// be placed on the synchronous delivery of a registration's event and
	// the retry job watched converging the same clinic. It has two
	// writers: ConfigFromEnv arms it from APP_FAIL_SELF_SERVICE_PROVISION
	// (absent or "0" leaves it nil -- the production default -- and N
	// arms a hook failing the first N attempts of each account; the
	// FailSelfServiceProvision field's own doc comment (bootstrap.go) has the
	// contract),
	// and the reference-app suites arm it on their own ServerConfig
	// before BuildServer captures it into the provisioner it builds
	// (wireSelfService). The hook answers per user id: a test's closure
	// counts or keys on that argument however its scenario needs.
	FailSelfServiceProvision func(userID string) error

	// DisableDemoUserHeader, when true, makes BuildServer wire every demo
	// identity source away from the demo headers: every permission-gated
	// route's SubjectResolver through DemoSubjectResolverFor(true), and
	// every attribution seam through headerDisabled
	// DemoOrgSubjectResolver/DemoNotesSubjectResolver instances -- see
	// the DisableDemoUserHeader field's own doc comment (bootstrap.go) for
	// why this exists
	// and exactly what it changes. ConfigFromEnv sets it from
	// APP_DISABLE_DEMO_USER_HEADER; false (the default) keeps every demo
	// identity source on its header-enabled wiring, matching
	// DisableQueueWorker's own contract just above.
	DisableDemoUserHeader bool

	// TrustedProxies is the authn.WithTrustedProxies declaration: the IP
	// addresses and CIDR prefixes of the reverse proxies this deployment
	// receives requests through, so authn's session/login-history records
	// carry the real client address (recovered from the X-Forwarded-For
	// chain those proxies append) instead of the proxy's address.
	// ConfigFromEnv fills it from APP_TRUSTED_PROXIES, a comma-separated
	// list (see the TrustedProxies field's own doc comment (bootstrap.go));
	// the empty default -- the
	// zero-external-dependency `go run ./cmd/server` experience, and every
	// test's config -- keeps authn's fail-closed behavior of recording
	// every request's direct connection address. A value whose entries are
	// not IP addresses or CIDR prefixes refuses boot:
	// authn.WithTrustedProxies validates its input at module construction
	// (go/authn's newOptions).
	// A deployment behind a proxy declares the proxy here or its records
	// stay proxy-addressed; it must never declare an untrusted range, and
	// the declared proxy must overwrite or strip the X-Forwarded-For it
	// receives from its own clients, exactly as Fly.io's proxy does.
	TrustedProxies []string

	// ReadFlyClientIP is this deployment's declaration that its proxy is
	// Fly's, the per-header opt-in (APP_READ_FLY_CLIENT_IP)
	// that authorizes authn to read the single-hop Fly-Client-IP vendor
	// header for a request whose peer is within TrustedProxies (go/authn's
	// WithVendorClientIPHeaders / VendorClientIPHeaderFlyClientIP). The
	// header is the real client address on every request Fly's proxy
	// forwards, so this app's session/login-history records keep carrying
	// the real client behind the Fly proxy. False (the default) reads no
	// vendor header at all -- authn falls back to X-Forwarded-For and the
	// connection address -- which is the correct fail-closed shape for a
	// non-Fly deployment: reading Fly-Client-IP for ANY declared proxy
	// would let a client smuggle its own value through a generic reverse
	// proxy that forwards unknown headers verbatim (nginx, ALB, Envoy,
	// Cloudflare), minting its own recorded AND rate-limited address.
	// ConfigFromEnv refuses true with an empty TrustedProxies: that
	// combination could never read the header and would silently keep
	// recording the proxy itself.
	ReadFlyClientIP bool

	// WebDistDir names the directory holding this app's built frontend
	// (the dist/ examples/reference-app/web's `pnpm build` emits) when
	// this process should serve that frontend itself -- see frontend.go's
	// own package doc comment for this app's wiring and pkgcore/spa's for
	// the full serving contract. ConfigFromEnv
	// sets it from APP_WEB_DIST; the empty default (the zero-external-
	// dependency `go run ./cmd/server` experience, and every test's
	// config) leaves the composed handler serving no static files at all.
	WebDistDir string

	// PeriodicTaskInterval is the cadence of this host's periodic-task
	// scheduler (periodic_scheduler.go), the ticker that enqueues the
	// jobs-driven mechanisms this app wired -- storage's per-tenant expiry
	// sweep and pki's signing-key expiry scan. ConfigFromEnv never sets
	// it, so zero (the default) means the scheduler's own default,
	// defaultPeriodicTaskSchedulerInterval, and the flow tests inject a
	// sub-second interval to drive real ticks within test time -- the same
	// test-override shape Mailer and Memberships below use.
	PeriodicTaskInterval time.Duration

	// PKIPropagationWindow, PKIRenewalLeadTime and PKIExpiryScanWindow
	// override pki's rotation timing and its expiry-scan idempotency window
	// when non-zero: BuildServer applies pki.WithPropagationWindow,
	// pki.WithRenewalLeadTime and pki.WithExpiryScanWindow only for values
	// above zero, so the zero default (what ConfigFromEnv always leaves
	// them at) keeps the module's own DefaultPropagationWindow /
	// DefaultRenewalLeadTime / DefaultExpiryScanWindow in force. The pki
	// flow test
	// injects a renewal lead time past a signing key's validity so the very
	// next expiry scan stages its replacement within test time, and a scan
	// window below the scheduler's own tick cadence so every test tick
	// lands in a fresh window and each tick really runs a scan (the
	// production defaults would stall the test's stage-then-promote proof
	// across window boundaries no test time can wait out).
	PKIPropagationWindow time.Duration
	PKIRenewalLeadTime   time.Duration
	PKIExpiryScanWindow  time.Duration

	// Mailer overrides the console mailer the standalone Preset resolves
	// for the "mailer" seam when set. ConfigFromEnv sets it to a real
	// pkgcore.NewSMTPMailer composition when SMTPHost is configured (see
	// SMTPHost's doc comment above); it is otherwise nil in production, so
	// this field also exists for the org invitation-accept flows
	// (flowtests/org_flow_test.go, flowtests/org_clinic_invitation_test.go,
	// flowtests/org_invitation_signin_test.go), which need the rendered mail back
	// in-process to extract the invitation token rather than parsing it out
	// of console output.
	// BuildServer injects Mailer with MailerCapabilities below, defaulting
	// to pkgcore.Stateless when that field is left at its zero value --
	// the honest capability for a throwaway test double.
	Mailer pkgcore.Mailer

	// MailerCapabilities declares the capability bits BuildServer wires
	// Mailer with, when Mailer is non-empty. ConfigFromEnv sets it to
	// pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart -- the capabilities
	// the "mailer.smtp" builtin registration itself declares -- alongside
	// its real SMTP Mailer; every other caller (every test's in-process
	// double) leaves it at the zero value, which BuildServer treats as
	// pkgcore.Stateless.
	MailerCapabilities pkgcore.Capability

	// Memberships is the seam authn asks tenant-membership questions
	// through (authn.WithMembershipReader below): customer-tenant answers
	// read org's own memberships table, and this store carries the
	// rbac.SystemDomain grants plus any test shortcut. Nil defaults to a
	// fresh, empty signInMemberships in BuildServer; a test that needs to
	// seed membership after registering a demo user keeps its own reference
	// by setting this field before calling BuildServer, rather than
	// reaching into BuildServer's internals. See sign_in_memberships.go's
	// own doc comment for the full shape.
	Memberships *signInMemberships

	// DemoUsersPassword, when non-empty, makes BuildServer seed the three
	// demo accounts of demo_users.go at the end of its composition --
	// register each through the composed handler, then grant the
	// membership and role its actor model declares -- so a browser visitor
	// can sign in as demo-owner@example.com and friends with a real
	// account, real membership and real rbac grants, and no demo header.
	// ConfigFromEnv fills it from APP_DEMO_USERS_PASSWORD; the empty
	// default (the zero-external-dependency `go run ./cmd/server`
	// experience) skips the seed entirely.
	DemoUsersPassword string

	// DemoPlatformStaffPassword, when non-empty, makes BuildServer seed
	// the demo platform-staff account of demo_admin.go (seedDemoPlatformStaff)
	// at the end of its composition, INDEPENDENTLY of DemoUsersPassword: a
	// boot seeds each demo account set from its own variable, never one
	// from the other's. The platform administrator (BuiltinRoleOwner under
	// rbac.SystemDomain, every admin:* permission included) must never be
	// seeded from the ordinary demo users' password, so the two credential
	// sources stay apart by construction -- see the DemoPlatformStaffPassword
	// field's own doc comment (bootstrap.go) for why. ConfigFromEnv fills it from
	// APP_DEMO_PLATFORM_STAFF_PASSWORD; the empty default skips the seed.
	DemoPlatformStaffPassword string

	// SMSOutput is where the console SMS sender (the standalone deployment
	// mode's transport, pkgcore's sms.go) writes delivered messages. Nil
	// defaults to os.Stdout.
	SMSOutput io.Writer

	// SocialProviders, RedirectAllowlist and TrustedProviders wire authn's
	// social sign-in channels. All three default to empty/zero, the safe
	// nothing-enabled state this app ships with: no real OAuth app
	// credentials are configured for this example, and the per-provider
	// credential items authn registers are never read through dynamic
	// configuration (that read-through is unimplemented), so credentials
	// reach authn only through this field. flowtests/authn_e2e_test.go
	// supplies a channel pointed at a local httptest server here, to prove
	// the social sign-in flow end to end without a live provider.
	SocialProviders   []authn.SocialProvider
	RedirectAllowlist authn.RedirectAllowlist
	TrustedProviders  []string

	// AIGatewayBaseURL and AIGatewayAPIKey, when AIGatewayAPIKey is
	// non-empty, make BuildServer write a platform-wide ai-gateway
	// credential (aigateway.CredentialService.SetPlatformCredential) for
	// aigateway.ProviderOpenAICompatible at boot, so the consult module's
	// one route (consult.go) can actually reach a provider.
	// ConfigFromEnv never sets either -- there is no real OpenAI-compatible
	// key committed to this repository, the same posture SocialProviders'
	// own doc comment above describes -- so the zero-setup `go run
	// ./cmd/server` experience leaves the consult route permanently
	// answering aigateway.ErrCredentialNotFound until an operator wires a
	// real key. flowtests/consult_flow_test.go is what sets both: AIGatewayBaseURL to
	// an httptest.Server standing in for the OpenAI-compatible endpoint,
	// and AIGatewayAPIKey to a fixed test value, exactly the way cfg.Mailer
	// is a test-only override of a seam production leaves on its real
	// default.
	AIGatewayBaseURL string
	AIGatewayAPIKey  string

	// AIGatewayImageBaseURL and AIGatewayImageAPIKey are the image-side
	// mirror of AIGatewayBaseURL/AIGatewayAPIKey, above: when
	// AIGatewayImageAPIKey is non-empty, BuildServer writes a second
	// platform-wide ai-gateway credential for
	// aigateway.ProviderOpenAICompatibleImage at boot, so the smilesim
	// module's routes (smilesim.go) can actually reach an image
	// provider. The two credentials are deliberately independent rows of
	// the SAME ai_gateway_credentials table (keyed by provider name), so
	// chat and image credentials coexist with no schema change.
	// ConfigFromEnv fills
	// both from APP_AI_GATEWAY_IMAGE_BASE_URL/APP_AI_GATEWAY_IMAGE_API_KEY when the
	// API key variable is set (their doc comment carries the reasoning
	// and the non-secret shape of the e2e value); when both are unset, no
	// credential row is written, exactly like the chat pair above, and
	// flowtests/smilesim_flow_test.go is the other caller that sets both.
	AIGatewayImageBaseURL string
	AIGatewayImageAPIKey  string

	// WebhookURLValidator and WebhookHTTPClient override go/integration's
	// two SSRF enforcement points together (integration.
	// WithWebhookURLValidator's own doc comment explains why the two must
	// always move together) for ONE Module instance this BuildServer call
	// composes -- never a production weakening, since ConfigFromEnv never
	// sets either and BuildServer leaves both options unset (the module's
	// own strict default) whenever WebhookURLValidator is nil. This exists
	// for flowtests/webhook_flow_test.go alone: it is this app's only way to prove a
	// genuine signed HTTP delivery against a receiver it controls, since
	// neither an httptest.Server (loopback) nor a sibling Docker container
	// (RFC 1918 private space, exactly like every other Docker-backed
	// integration tier's own sibling containers in this repository) can
	// ever produce an address go/integration's production SSRF check is
	// willing to accept -- see integration.WithWebhookURLValidator's own
	// doc comment for the full argument. Mirrors AIGatewayBaseURL's and
	// cfg.Mailer's identical "test-only override of a seam production
	// leaves on its real default" shape above.
	WebhookURLValidator func(ctx context.Context, url string) error
	WebhookHTTPClient   *http.Client

	// OnRBACReady, when non-nil, receives the live *rbac.Service BuildServer
	// attaches, immediately after seedDemoGrants seeds every configured
	// tenant's built-in roles and demo grants. It exists purely for a test
	// that needs to grant a role scoped to an organization node CREATED
	// AFTER the server starts serving HTTP -- the subtree-scoped grant
	// test (flowtests/org_route_guards_test.go), which cannot
	// know a node's id at boot time, since org builds its tree through real
	// HTTP calls the test itself drives once the server is up. Nil in every
	// production boot and every other test is a complete no-op, mirroring
	// WebhookURLValidator's identical "test-only override of what BuildServer
	// already wires, never a production weakening" shape above.
	OnRBACReady func(*rbac.Service)

	// OnConfigReady, when non-nil, receives the live *config.Service
	// BuildServer attaches, immediately after openConfiguredAuthnChannels
	// opens the assembled social channels' system-tier flag rows. It exists
	// purely for a test that must write a configuration row through the
	// module's real Set path (flowtests/authn_e2e_test.go's
	// TestAuthnE2E_PasswordChannelDisabled...), which
	// disables authn.password_login through the same system-tier write an
	// operator's admin-console write would land (under
	// config.SystemPurposeSystemWrite) and proves the composed stack then
	// refuses the password endpoint while SMS and social stay open. Nil in
	// every production boot and every other test is a complete no-op,
	// mirroring OnRBACReady's identical "test-only override of what
	// BuildServer already wires, never a production weakening" shape above.
	OnConfigReady func(*config.Service)
}

// BuildServer wires the reference app's Kernel -- the authn, notes, org,
// config, rbac, storage, demo, notification, ai-gateway and audit Modules --
// their migrations, the job queue the storage and notification modules
// share, the demo notification glue (demo_notification.go), the
// consult glue (consult.go, go/ai-gateway's mandatory first
// consumer), and the authn+tenancy middleware chain into a single
// http.Handler. It is the one place that wiring logic lives -- main() and
// the end-to-end tests (flowtests/server_test.go, flowtests/authn_e2e_test.go,
// flowtests/org_flow_test.go, flowtests/storage_flow_test.go, flowtests/notification_flow_test.go and
// flowtests/consult_flow_test.go) all call it, so the two can never drift into
// testing a different wiring than the one that actually runs.
//
// It returns the composed handler, a cleanup function that closes
// everything BuildServer opened (the services, the injected Redis bus and
// its client when one was built, and the underlying database connection),
// and the wired *compliance.Module -- the flowtests/compliance_flow_test.go's one
// reach into the retention/erasure/export services this app bootstraps,
// since BuildServer exposes no module itself otherwise. The caller must
// call cleanup once done with the handler.
//
// The deployment mode itself refuses nothing here: the Kernel it
// bootstraps is what validates the assembled composition against
// cfg.DeploymentMode, failing startup with pkgcore's own capability error
// (ErrCapabilityUnsatisfied) when the resolved composition cannot run in
// the declared mode. Every stateful seam this app knows about -- eventbus,
// kv, mailer, objectstore, plus authn's own "SMS sender" seam -- can be
// pointed at a real, MultiReplicaSafe-capable implementation through the
// environment variables ConfigFromEnv resolves (APP_REDIS_ADDR, the
// APP_S3_* group, APP_OBJECT_STORE_ROOT, the APP_SMTP_* group,
// APP_SMS_GATEWAY_URL); every one of
// them defaults to the standalone Preset's in-process implementation when
// unset, so a plain `go run ./cmd/server` needs nothing else running. With
// none of them set, the distributed deployment mode always fails
// capability validation, naming the first unsatisfied seam in resolution
// order ("eventbus" first, per Kernel.Bootstrap's fixed order); with all of
// them set to a genuinely MultiReplicaSafe composition (real Redis, real
// S3-compatible storage, real SMTP, and authn's real HTTP SMS transport),
// the distributed deployment mode genuinely BOOTS, which is the
// property this file's own TestBuildServer_DistributedDeploymentMode_*
// tests pin the positive half of, and
// examples/reference-app/integration_test/distributed_mode_test.go proves
// end to end against real Docker-backed infrastructure.
func BuildServer(ctx context.Context, cfg ServerConfig) (http.Handler, func() error, *compliance.Module, error) {
	// dbkit.Options.AuditBus is wired here (and notes.Note does implement
	// dbkit.Auditable -- see its model.go), for org: org's OrgNode,
	// Membership and Invitation models opt into dbkit.Auditable, so their
	// writes are auto-captured by dbkit's GORM callbacks rather than
	// recorded by hand-written audit.Emit calls at each write path.
	// Pointing the capture plugin's publish target at a persister on this
	// SAME SQLite file is safe: the plugin buffers its captured events on
	// the write's own context and WithTenantSession publishes them only
	// once its own transaction has genuinely committed -- publishing
	// synchronously inside the still-open transaction would collide with
	// the write lock the transaction itself holds (SQLITE_BUSY). The
	// shape is proven against two real connections to one real SQLite
	// file in go/dbkit/audit_capture_test.go's
	// TestAuditCapturePlugin_WithTenantSession_SameFileSynchronousPersister_NoLongerDeadlocks.
	//
	// Org's models are captured, and notes' are not, through
	// Options.AuditModels -- the per-model capture scope dbkit's Options
	// carries: the scope is org's own
	// declaration, org.AuditableModels(), consumed verbatim, so this app
	// never keeps a hand-written list of org's Auditable models that could
	// drift from org's (see that function's doc comment for the marker-
	// added direction dbkit cannot check and org's own suite pins instead).
	// Note is outside the scope by construction, and deliberately: notes
	// records its own trail through the declarative audit.Emit call
	// notes/handler.go's NotesCreateNote makes after the note's
	// transaction has committed (a genuinely separate persister connection
	// remaining the simpler, clearer choice for notes, and its
	// "notes.note.create" rows the only record of a note write). Were the
	// capture scope to include Note, every note create would land twice --
	// once as the derived "note.create" row, once as the explicit
	// "notes.note.create" Emit row -- and note update/delete writes would
	// land under "note.update"/"note.delete" actions nothing declares,
	// which the persister's vocabulary gate refuses with a structured
	// alert. The scope list is this app's explicit answer to "which
	// Auditable models on this connection does the automatic mechanism
	// own": the ones org itself declares capturable, nothing else.
	// flowtests/org_audit_capture_test.go pins the composed outcome (member
	// removal and node delete during an impersonation session each leaving
	// a dual-identity row, and an invitation create leaving a row whose
	// diff carries no address-derived value).
	//
	// The bus itself is constructed right below, BEFORE dbkit.Open, and
	// the SAME instance is handed to Kernel.Bootstrap through
	// WithEventBus further down: reg.EventBus() must be the very bus the
	// capture plugin publishes on, or org's captured writes would vanish
	// into a bus audit.Module's subscriptions never see -- the one wiring
	// mistake this composition cannot fail loudly about, since each half
	// works on its own. With cfg.RedisAddr set the bus is the real
	// Redis-backed one (and doubles as the "kv" seam's store, as below);
	// unset, it is pkgcore's in-process memory bus, the identical
	// implementation (and identical zero capability declaration) the
	// standalone Preset would otherwise resolve on its own.
	//
	// authn's own PII columns (email, phone, TOTP secrets) must have their
	// serializer registered BEFORE dbkit.Open: GORM resolves a model's
	// serializer while it parses the schema, and this module's registry is
	// process-global (authn.RegisterPIISerializer's own doc comment).
	piiCipher, err := dbkit.NewCipher(cfg.AuthnPIICipherKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("reference-app: build authn's PII cipher: %w", err)
	}
	if regErr := authn.RegisterPIISerializer(piiCipher); regErr != nil {
		return nil, nil, nil, fmt.Errorf("reference-app: register authn's PII serializer: %w", regErr)
	}

	// go/pki's LocalSigner private-key column needs its own serializer
	// registered before dbkit.Open too, for the identical reason
	// authn.RegisterPIISerializer does -- GORM resolves a model's serializer
	// while it parses the schema (pki.RegisterLocalKeySerializer's own doc
	// comment).
	pkiLocalKeyCipher, err := dbkit.NewCipher(cfg.PKILocalKeyCipherKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("reference-app: build pki's local-key cipher: %w", err)
	}
	if regErr := pki.RegisterLocalKeySerializer(pkiLocalKeyCipher); regErr != nil {
		return nil, nil, nil, fmt.Errorf("reference-app: register pki's local-key serializer: %w", regErr)
	}

	// The audit-capture bus is constructed here, before dbkit.Open wires
	// it into Options.AuditBus below, and the identical instance is handed
	// to Kernel.Bootstrap's WithEventBus later -- see the comment on the
	// Open call for why the two must be one and the same bus. With
	// cfg.RedisAddr set, the bus is the real Redis-backed implementation
	// over a go-redis client this host constructs and owns (the client is
	// what cleanup closes); unset, it is pkgcore's in-process memory bus,
	// the identical implementation -- and identical zero capability
	// declaration -- the standalone Preset would otherwise resolve on its
	// own inside Bootstrap. busCapabilities declares what the chosen
	// implementation genuinely carries, so the WithEventBus call below can
	// declare it the way every other injection in this function does.
	var (
		bus             pkgcore.EventBus
		busCapabilities pkgcore.Capability
		redisBus        *eventbusredis.EventBus
		redisClient     *redis.Client
	)
	if cfg.RedisAddr != "" {
		redisClient = redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
		redisBus = eventbusredis.NewEventBus(redisClient)
		bus = redisBus
		busCapabilities = pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart
	} else {
		bus = pkgcore.NewMemoryEventBus()
		busCapabilities = 0
	}

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     cfg.SQLitePath,
		// Org's automatic write capture publishes on bus -- see the long
		// comment above this call for why the bus exists before Open, and
		// why the scope is org's own exported declaration and deliberately
		// nothing else (notes.Note stays out: its module records its own
		// trail through audit.Emit).
		AuditBus:    bus,
		AuditModels: org.AuditableModels(),
	})
	if err != nil {
		// The Redis client created above has no goroutine or connection
		// yet (go-redis dials lazily; the eventbus starts nothing until a
		// Subscribe), but it exists -- close it so no startup error path
		// leaks the handle. Every later error path runs the cleanup
		// closure, which closes redisClient itself.
		if redisClient != nil {
			_ = redisClient.Close()
		}
		return nil, nil, nil, fmt.Errorf("reference-app: open database: %w", err)
	}

	// configService and rbacService are filled by their Attach calls below
	// (nil until then), standaloneQueue by the pki module's wiring above
	// (nil until then), meteringModule by its construction beside the
	// billing/ai-gateway wiring below (nil until then), and
	// smileSimReconcilerStop and
	// periodicTaskSchedulerStop by their start calls below (both nil until
	// then); redisBus and redisClient were filled above, where the
	// audit-capture bus was constructed, when cfg.RedisAddr selects the
	// injected Redis-backed composition (nil otherwise).
	// cleanup closes the services and the job queue first -- stopping the
	// two ticker loops, config's anti-loss poller, rbac's cache janitor
	// and the queue's workers so none of them drains a job, enqueues a
	// task or runs a poll against a connection that is being torn down --
	// then the injected bus, stopping its readers so no remote event can
	// still be delivered to a handler writing the database, then the
	// client this host owns (eventbusredis.EventBus never closes it), and
	// the database last. Every close is attempted even when an earlier one
	// failed; the first error wins.
	var (
		configService             *config.Service
		rbacService               *rbac.Service
		standaloneQueue           *jobs.StandaloneQueue
		meteringModule            *metering.Module
		smileSimReconcilerStop    func()
		periodicTaskSchedulerStop func()
	)

	cleanup := func() error {
		var firstErr error
		// keepErr records err as the cleanup failure only when it is the
		// first one seen -- every close below is attempted regardless, so
		// neither an early nor a late failure can hide the other. It is a
		// helper rather than an inline "closeErr != nil && firstErr == nil"
		// guard because the very first site would make that guard a
		// tautology (firstErr is provably nil there).
		keepErr := func(err error) {
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if smileSimReconcilerStop != nil {
			// Stopped first, before standaloneQueue.Close and the database
			// close below: the reconciler sweep calls both
			// standaloneQueue.Get and the database (through its own
			// smilesim.ReservationStore), so it must not still be ticking
			// against either while they are being torn down.
			smileSimReconcilerStop()
		}
		if periodicTaskSchedulerStop != nil {
			// Stopped next, before standaloneQueue.Close below for the
			// same reason: the scheduler's tick body enqueues onto the
			// queue's task table, so it must not still be ticking while
			// the pool that drains it is being torn down (the blocking
			// stop also guarantees no enqueue is in flight when Close
			// runs -- see periodic_scheduler.go's own doc comment).
			periodicTaskSchedulerStop()
		}
		if configService != nil {
			keepErr(configService.Close())
		}
		if rbacService != nil {
			keepErr(rbacService.Close())
		}
		if standaloneQueue != nil {
			// StandaloneQueue.Close stops the dispatcher and waits for
			// in-flight jobs to finish, bounded by the same timeout that
			// bounds HTTP graceful shutdown. Close is idempotent, so an
			// error path that runs before Start is ever called is safe.
			queueCtx, cancel := context.WithTimeout(context.Background(), hostcore.ShutdownTimeout)
			keepErr(standaloneQueue.Close(queueCtx))
			cancel()
		}
		if meteringModule != nil {
			// meteringModule.Stop stops both of metering's background
			// pipelines -- the analytics recorder's flush loop and the
			// dispatcher's outbox poll -- and delivers whatever the
			// recorder still had buffered into the aggregator before
			// returning (go/metering/analytics.go's shutdown contract:
			// "an event is dropped, or delivered; it is never silently
			// lost"). Both loops write the shared database, so they stop
			// before sqlDB.Close below, the same "nothing drains against a
			// connection being torn down" ordering every other stop above
			// follows. Safe to call before Start, or more than once.
			meteringModule.Stop()
		}
		if redisBus != nil {
			redisBus.Close()
		}
		if redisClient != nil {
			keepErr(redisClient.Close())
		}
		sqlDB, dbErr := db.DB()
		keepErr(dbErr)
		if sqlDB != nil {
			keepErr(sqlDB.Close())
		}
		return firstErr
	}

	cipher, err := dbkit.NewCipher(cfg.ConfigKey)
	if err != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: build the config master cipher: %w", err)
	}

	// org's Invitation.Email column is encrypted at rest under this same
	// cipher (registered here, before anything touches the Invitation
	// model, since GORM resolves a named serializer at struct-parse time)
	// and made queryable by a SEPARATE HMAC key -- see OrgIndexKeyEnv's own
	// doc comment for why reusing cfg.ConfigKey for both would be exactly
	// the AES-key-doubling-as-an-HMAC-key weakness dbkit warns against.
	dbkit.RegisterEncryptedSerializer(org.EmailSerializerName, cipher)
	// The column argument below is org's exported EmailIndexColumn rather
	// than a hand-typed literal for the reason the notification block just
	// below documents: dbkit.NewBlindIndexer refuses an EMPTY column name
	// but has no guard for a non-empty wrong one -- a hand-typed literal
	// that drifted from the real column would fail only when someone
	// called Equal on the indexer. The
	// exported constant travels from the package that owns the schema,
	// pinned by org's own suite (go/org/email_index_column_drift_test.go).
	orgIndexer, err := dbkit.NewBlindIndexer(org.EmailIndexColumn, cfg.OrgIndexKey, dbkit.NormalizeEmail)
	if err != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: build the org email indexer: %w", err)
	}

	// notification's Contact.Address column is encrypted at rest under this
	// same config cipher (registered here, before anything touches the
	// Contact model, since GORM resolves a named serializer at struct-parse
	// time) and made queryable by a SEPARATE HMAC key -- see
	// the Notification field's own doc comment (bootstrap.go) for why reusing
	// cfg.ConfigKey for both would be exactly the AES-key-doubling-as-an-
	// HMAC-key weakness dbkit warns against. One key serves the email and
	// the phone indexers alike (authn's single blind-index key precedent);
	// both index the SAME column (verified_contacts.address_index), and the
	// column argument below is notification's exported AddressIndexColumn
	// rather than a hand-typed string because dbkit.NewBlindIndexer refuses
	// an EMPTY column name but has no guard for a non-empty wrong one -- a
	// hand-typed literal that drifted from the real column would fail only
	// when someone called Equal on the indexers. The
	// per-indexer error text below still names which of the two failed.
	dbkit.RegisterEncryptedSerializer(notification.ContactAddressSerializerName, cipher)
	contactEmailIndexer, err := dbkit.NewBlindIndexer(notification.AddressIndexColumn, cfg.NotificationIndexKey, dbkit.NormalizeEmail)
	if err != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: build the notification contact email indexer: %w", err)
	}
	contactPhoneIndexer, err := dbkit.NewBlindIndexer(notification.AddressIndexColumn, cfg.NotificationIndexKey, dbkit.NormalizePhoneE164)
	if err != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: build the notification contact phone indexer: %w", err)
	}

	// ai-gateway's ai_gateway_credentials.api_key column is encrypted at
	// rest under this same config cipher, registered here for the identical
	// "before anything touches the model" reason as org's and notification's
	// registrations immediately above. Unlike those two, no separate HMAC
	// key is needed: a credential is only ever looked up by (provider,
	// scope, tenant_id), never by its own value, so reusing cfg.ConfigKey's
	// cipher carries none of the AES-key-doubling-as-an-HMAC-key risk their
	// comments warn about -- there is no second, HMAC construction here to
	// double as.
	dbkit.RegisterEncryptedSerializer(aigateway.CredentialAPIKeySerializerName, cipher)

	// integration's WebhookSubscription.Secret column is encrypted at rest
	// under this same config cipher, registered here for the identical
	// "before anything touches the model" reason as every registration
	// above. A webhook secret must be READ BACK IN PLAINTEXT to sign
	// every delivery attempt, never merely compared, so no separate HMAC
	// blind index is needed here either -- the identical reasoning
	// aigateway.CredentialAPIKeySerializerName's own registration comment
	// gives.
	dbkit.RegisterEncryptedSerializer(integration.WebhookSecretSerializerName, cipher)

	// hostByTenant is cfg.HostTenants' reverse index: which configured
	// host belongs to a given tenant, the first source an invitation's
	// accept link draws its host from. The host is DISPLAY, never
	// acceptance: InviteService.Accept resolves the invitation's tenant
	// from the token itself, server-side, when the accepting request
	// carries no tenant claim (go/org/invite.go's own doc comment on
	// Accept), and this app's middleware chain lets the accept path
	// through tenant resolution (the allowlist entry below), so a link to
	// ANY host that serves this app's API is acceptable -- which is
	// exactly what makes the fallback below safe. Only the two demo
	// tenants have a configured host (DemoHostTenants); every other
	// tenant this app serves is a self-registered clinic whose tenant id
	// self_service.go derives from its registrant (ClinicTenantOf,
	// "tenant-" + user id, by construction never a cfg.HostTenants value
	// -- that derivation's own doc comment says so), so no configured
	// host can name it and its links draw their host from the second
	// source: this deployment's own public origin (cfg.PublicOrigin,
	// APP_PUBLIC_ORIGIN). A clinic link pointing at a configured demo
	// tenant's branded host would hand the invitee a link that brands
	// someone else's tenant; a host wiring with neither source fails the
	// link loudly, naming the missing knob, rather than guessing.
	hostByTenant := make(map[pkgcore.TenantID]string, len(cfg.HostTenants))
	for host, tenant := range cfg.HostTenants {
		hostByTenant[tenant] = host
	}

	// configService is also read here, lazily, by org's feature gate -- see
	// OrgFeatureGate's own doc comment for why it cannot be handed
	// configService directly at this point in the wiring.
	orgModule := org.NewModule(db,
		org.WithEmailIndexer(orgIndexer),
		org.WithFeatureGate(OrgFeatureGate{Service: &configService}),
		// principalFallback serves org's browser-shaped callers:
		// org's two caller-scoped endpoints also serve the team surface's
		// signed-in owner, whose requests carry a bearer
		// token and no X-Demo-User-Id header -- so the org module's resolver
		// instance falls back to the verified Principal when no demo header
		// is present, the same header-then-Principal shape
		// DemoNotesSubjectResolver gives notes' and cases' creator seams.
		// The notification and integration modules keep the flag unset (see
		// DemoOrgSubjectResolver's own doc comment for why their
		// header-only refusal stays pinned).
		org.WithSubjectResolver(DemoOrgSubjectResolver{
			HeaderDisabled:    cfg.DisableDemoUserHeader,
			PrincipalFallback: true,
		}),
		org.WithMailFrom("invitations@reference-app.example"),
		org.WithInvitationLinkBuilder(func(ctx context.Context, token string) (string, error) {
			tenant, tenantErr := pkgcore.MustTenantFromContext(ctx)
			if tenantErr != nil {
				return "", tenantErr
			}
			linkBase := ""
			if host, ok := hostByTenant[tenant]; ok {
				linkBase = "https://" + host
			} else if cfg.PublicOrigin != "" {
				linkBase = strings.TrimRight(cfg.PublicOrigin, "/")
			} else {
				return "", fmt.Errorf("reference-app: no host configured for tenant %q and no APP_PUBLIC_ORIGIN to fall back to", tenant)
			}
			// This app ships no frontend invitation-acceptance page: the
			// consumer shell's team surface (examples/reference-app/web's
			// team-view) is where an invitation is CREATED, while accepting
			// one stays the API-level flow a fresh invitee drives from the
			// email link. The link names org's real
			// POST /api/v1/org/invitations/accept endpoint and carries the
			// token as a query parameter purely so it is one recognizable
			// string a person (or, in flowtests/server_test.go's end-to-end suite, a
			// test) can extract the token back out of.
			return fmt.Sprintf("%s/api/v1/org/invitations/accept?token=%s", linkBase, url.QueryEscape(token)), nil
		}),
	)

	// standaloneQueue is this app's one job queue, shared by every module
	// whose asynchronous work runs on the jobs mechanism: a
	// jobs.StandaloneQueue over this app's own database connection, the
	// standalone mode's SQLite-backed worker pool whose task table
	// jobs.Wire creates below (no migration of this host's is involved).
	// It is constructed here, ahead of pkiModule just below -- the first
	// module whose NewModule options need the queue -- and ahead of the
	// storage, integration, notification, ai-gateway, compliance and
	// admin modules that take it as well. Its lifecycle is bound to this
	// host's: jobs.Wire below moves every registry-declared task handler
	// onto it and creates its tables, and Start launches the pool, both
	// after Bootstrap (only then do Register's declarations exist), and
	// cleanup's Close stops the pool before the shared database closes.
	standaloneQueue = jobs.NewStandaloneQueue(db)

	// pki owns authn's signing-key lifecycle: LocalSigner (its own
	// zero-external-dependency default) generates and stores the key in
	// cfg.SQLitePath, so it persists across restarts with no dev-seed
	// derivation required -- see DevPKILocalKeyCipherKey's own doc comment.
	// The reference app assembles authn directly rather than through
	// saasctl, so it wires pki here exactly as any authn-containing
	// generated project's own server.go does.
	//
	// The queue is part of that wiring too: WithQueue hands pki the
	// standaloneQueue constructed above, which is what makes Register
	// attach the queue to Service and CAService and declare the module's
	// two job handlers onto the registry -- the signing-key expiry-scan
	// task (pki.expiry_scan), which this host's periodic-task scheduler
	// enqueues on its tick, and the CRL-regenerate task, declared and
	// drained like every other registered handler but never scheduled:
	// the app's X.509 consumer (internal/attestation) verifies against
	// row state and chains and generates CRLs on demand, so a scheduled
	// refresh still has no reader (periodic_scheduler.go's doc comment
	// and go/pki/AGENTS.md record that honestly). The three
	// knobs below are applied only when a test injects them: a zero
	// PKIPropagationWindow, PKIRenewalLeadTime or PKIExpiryScanWindow
	// (what ConfigFromEnv always leaves them at) keeps pki's own
	// DefaultPropagationWindow / DefaultRenewalLeadTime /
	// DefaultExpiryScanWindow in force.
	pkiOpts := []pki.Option{pki.WithQueue(standaloneQueue)}
	if cfg.PKIPropagationWindow > 0 {
		pkiOpts = append(pkiOpts, pki.WithPropagationWindow(cfg.PKIPropagationWindow))
	}
	if cfg.PKIRenewalLeadTime > 0 {
		pkiOpts = append(pkiOpts, pki.WithRenewalLeadTime(cfg.PKIRenewalLeadTime))
	}
	if cfg.PKIExpiryScanWindow > 0 {
		pkiOpts = append(pkiOpts, pki.WithExpiryScanWindow(cfg.PKIExpiryScanWindow))
	}
	pkiModule := pki.NewModule(db, pkiOpts...)

	memberships := cfg.Memberships
	if memberships == nil {
		memberships = NewSignInMemberships()
	}
	// The org-backed half of the membership store is bound AFTER Bootstrap,
	// at the site just below the kernel call: TenantsOf's audited
	// system-context grant publishes on the bus Bootstrap finishes wiring,
	// and no sign-in can reach authn before then anyway (see the attach
	// call's own comment).
	smsOutput := cfg.SMSOutput
	if smsOutput == nil {
		smsOutput = os.Stdout
	}

	authnOpts := []authn.Option{
		authn.WithKeySource(pkiModule.Service()),
		authn.WithBlindIndexKey(cfg.AuthnBlindIndexKey),
		authn.WithMembershipReader(memberships),
		authn.WithDeploymentMode(cfg.DeploymentMode),
		authn.WithSocialProviders(cfg.SocialProviders...),
		authn.WithRedirectAllowlist(cfg.RedirectAllowlist),
		authn.WithTrustedProviders(cfg.TrustedProviders...),
		// Immediate revocation: every sign-out this app drives -- a user's
		// own logout, the self-service session revoke, and the
		// refresh-replay theft response -- must cut off an unexpired access
		// token AT ONCE rather than at its natural expiry. authn's
		// enforcement is default-wired, not a second option: the service
		// attaches its session manager to the verifier Service().Verifier()
		// hands out, and the authn.Middleware call below consults it with
		// no WithRevocationChecker of its own -- so this single selection
		// is the whole wiring, and the plain
		// authn.Middleware(authnModule.Service().Verifier()) every consumer
		// skeleton copies is exactly the enforced composition (with no
		// selection, immediate revocation would be a stored list nothing
		// consults). The cost is one key-value
		// read per authenticated request, the documented price of immediate
		// mode, against the in-process store in standalone boots and Redis
		// in the distributed composition; a deployment that prefers
		// natural-mode's zero per-request cost deletes this option.
		authn.WithRevocationMode(authn.RevocationModeImmediate),
		// The trusted-proxy declaration (APP_TRUSTED_PROXIES): the proxy
		// addresses whose requests may carry the forwarding headers authn
		// reads, so this app's session and login-history records carry the
		// real client address behind the proxy instead of the proxy's own.
		// Empty -- every local boot and every test -- is authn's fail-closed
		// default, and a declaration with an entry that is neither an IP
		// address nor a CIDR prefix refuses this NewModule call below.
		authn.WithTrustedProxies(cfg.TrustedProxies...),
		// The feature gate that makes authn's eight declared feature flags
		// (authn.password_login, authn.sms_login, the five authn.social.*
		// channels, authn.sso.oidc) effective at request time: the same
		// lazy *config.Service adapter org's own gate uses above --
		// authn.FeatureGate is org.FeatureGate's identical declaration, and
		// the config service is only produced by configModule.Attach, which
		// runs after Bootstrap returns (OrgFeatureGate's doc comment).
		// Without it this app's flags would be declarations with no
		// enforcement: a row disabling authn.password_login would hide the
		// login form while the password endpoint kept issuing tokens.
		// The channels this host assembles through
		// cfg.SocialProviders are opened at the system tier after Attach by
		// openConfiguredAuthnChannels, since their flags default OFF.
		authn.WithFeatureGate(OrgFeatureGate{Service: &configService}),
	}
	// The per-header vendor opt-in (APP_READ_FLY_CLIENT_IP), conditional on the
	// deployment declaration: cfg.ReadFlyClientIP's 'true' declares this
	// deployment's proxy is FLY's, the one proxy that genuinely overwrites
	// Fly-Client-IP on every request it forwards -- the host declaration
	// go/authn requires before it reads that single-hop vendor header at
	// all (WithVendorClientIPHeaders). Without it, authn never reads
	// Fly-Client-IP even from a declared proxy -- a generic reverse proxy
	// forwards a client-chosen value verbatim, so the header on its own
	// could never be trusted -- and this app's records still carry the
	// real client through the X-Forwarded-For chain Fly's proxy appends.
	// ConfigFromEnv refuses 'true' with an empty APP_TRUSTED_PROXIES, so
	// the two halves of the declaration cannot be assembled inconsistently.
	if cfg.ReadFlyClientIP {
		authnOpts = append(authnOpts, authn.WithVendorClientIPHeaders(authn.VendorClientIPHeaderFlyClientIP))
	}
	// The "SMS sender" seam, following the same conditional-injection shape
	// as every other seam this file wires: a configured gateway URL always
	// wins, under either deployment mode, and composes the real HTTP SMS
	// transport (pkgcore.NewHTTPSMSSender); absent that, the standalone
	// deployment mode falls back to the console transport, while the
	// distributed deployment mode is left deliberately UNWIRED --
	// authn.NewModule's own newOptions then
	// fails closed with authn.ErrMissingDistributedSMSSender rather than
	// this app silently keeping a console sender nobody in a distributed
	// replica pool is reading (see the SMSGatewayURL field's doc comment
	// (bootstrap.go), and
	// TestBuildServer_DistributedDeploymentMode_NoSMSGateway_FailsClosed
	// for the proof).
	switch {
	case cfg.SMSGatewayURL != "":
		authnOpts = append(authnOpts, authn.WithSMSSender(pkgcore.NewHTTPSMSSender(cfg.SMSGatewayURL)))
	case cfg.DeploymentMode != pkgcore.DeploymentModeDistributed:
		authnOpts = append(authnOpts, authn.WithSMSSender(pkgcore.NewConsoleSMSSender(smsOutput)))
	}
	authnModule, err := authn.NewModule(db, authnOpts...)
	if err != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: build the authn module: %w", err)
	}

	// notes' creator seam is DemoNotesSubjectResolver (declared above):
	// notes' create handler reads the creating user's id through it and
	// stamps it on the note and the event it publishes (internal/notes
	// module.go's NoteCreatedPayload), so an unwired resolver -- NewModule's
	// default -- would fail every create closed, with notes.subject_unresolved
	// (see internal/notes handler.go's ErrSubjectUnresolved). The resolver
	// reads the X-Demo-User-Id header first and falls back to the verified
	// Principal only when no header is present -- the fallback that lets the
	// seeded accounts of demo_users.go create notes through their access
	// tokens alone (see its own doc comment, and demo_subject.go's
	// DemoNotesCreatorUserID).
	notesModule := notes.NewModule(db, notes.WithSubjectResolver(DemoNotesSubjectResolver{HeaderDisabled: cfg.DisableDemoUserHeader}))

	// auditModule is go/dbkit/audit's persister. It shares notesModule's
	// own database connection -- no new infra dependency is needed for
	// this app to have a real, queryable audit trail -- and subscribes to
	// audit.EventRecorded, the event notes/handler.go's NotesCreateNote
	// publishes through audit.Emit after a note is successfully created
	// (see the doc comment on db's own construction above for why this
	// app uses that mechanism rather than dbkit's automatic
	// AuditBus-driven write capture).
	auditModule := audit.New(db)

	// The config module shares notes' database and is given everything it
	// needs to serve its two endpoints: the master cipher (notes declares a
	// Sensitive item, so Attach would refuse a cipher-less module), a
	// resolver, and the default anti-loss poller cadence.
	//
	// The resolver is tenancy.NewDomainResolver -- deliberately NOT the
	// authn-derived resolver that gates the notes API below -- and its
	// default tenant is deliberately empty. config's public endpoints are
	// pre-auth display decisions, the one case go/tenancy's DomainResolver
	// doc comment blesses with unmatched-host leniency; an empty default
	// tenant maps that leniency onto the endpoint's own "platform
	// defaults" tier (a host that resolves to no tenant reads system-scope
	// rows, never an error) -- a pre-auth display endpoint must render
	// before any sign-in, so an unmatched host can never error the page.
	// config's own internal resolver runs
	// entirely independently of the outer tenancy.Middleware wired at the
	// bottom of this function -- see this function's own middleware-chain
	// comment below for why both coexist.
	configModule := config.NewModule(db,
		config.WithCipher(cipher),
		config.WithResolver(tenancy.NewDomainResolver(
			func(host string) (pkgcore.TenantID, bool) {
				tid, ok := cfg.HostTenants[host]
				return tid, ok
			},
			"",
		)),
	)

	// rbac needs nothing from this host but a database: it declares its own
	// permissions during Register and reads EVERY module's declarations
	// once, in Attach, after Bootstrap. WithSubtreeResolver IS wired, onto
	// orgModule's own Scope (OrgSubtreeResolver below, an adapter over the
	// two seams' slightly different signatures -- see its own doc comment):
	// this app's organization tree is real (flowtests/org_flow_test.go's multi-level
	// DSO tree, and the subtree-scoped grant test),
	// so a node-scoped rbac binding must actually resolve against it rather
	// than deny for want of a resolver. Every demo grant seedDemoGrants
	// makes is still tenant-wide (rbac.Scope{}) -- the wiring below is what
	// makes a NARROWER grant (a role assigned with a real node id) mean
	// something, for a host that wants one, not what makes one mandatory.
	// WithQueue wires the host's standaloneQueue into rbac's org-event
	// reaping (rbac.Module.WithQueue's own doc comment has the full
	// shape): when this app removes a member or deletes an org node, the
	// org-event subscriber ENQUEUES the reap of that member's or node's
	// role bindings as a task on this same queue every other module's
	// asynchronous work runs on, instead of running the reap
	// synchronously inside the event delivery with no retry home. The
	// task handlers land on reg.Jobs in rbac's own Attach, like every
	// other
	// module's declarations, by the time the drain loop below moves them
	// onto the queue; the queue's retries are what converge a reap that
	// hit a transient database failure.
	rbacModule := rbac.NewModule(db,
		rbac.WithSubtreeResolver(OrgSubtreeResolver{Scope: orgModule.Scope()}),
		rbac.WithQueue(standaloneQueue))

	// storageModule is the reference app's first consumer of go/storage.
	// Its asynchronous work -- the thumbnail-derive task every completed
	// image object enqueues, and the expiry-sweep task this host's
	// periodic-task scheduler enqueues per tenant (periodic_scheduler.go)
	// -- runs on the host's standaloneQueue above
	// (storage.WithQueue), drained and claimed below like every other
	// registry-declared handler.
	storageModule := storage.NewModule(db, storage.WithQueue(standaloneQueue))

	// attestationService is the reference app's AI-output attestation layer
	// -- the real consumer of go/pki's X.509 layer that closes the "no real
	// consumer yet" exception docs/internal/22-pki.md recorded (see
	// internal/attestation's package doc comment and go/pki/AGENTS.md's
	// "Real consumer" section for the full account). It issues each
	// tenant's "simulation.attestation" certificate through pkiModule.CA(),
	// signs attested outputs with it, and gates their public shares on
	// chain verification. Constructed here -- after pkiModule and
	// storageModule, before sharingModule needs its gate -- with the pki CA
	// service, a pki certificate repository for the issue-vs-reuse decision,
	// the same ObjectService the resolver below serves bytes through (as
	// the package's ContentOpener seam, adapted in sharing_resolver.go) and
	// its own app table store over this app's db connection. Its two boot
	// steps (EnsureSchema, EnsureAuthorityChain) run later, after
	// Kernel.Bootstrap applied pki's migrations -- see that block's own
	// comment.
	attestationService := attestation.NewService(
		pkiModule.CA(),
		pki.NewCertificateRepository(db),
		storageContentOpener{svc: storageModule.ObjectService()},
		attestation.NewAttestationStore(db),
		db,
	)

	// sharingModule is the reference app's first real consumer of
	// go/sharing end to end (flowtests/sharing_flow_test.go): the host that wires a
	// real ResourceResolver behind the module's public access surface. Its
	// one public route
	// resolves a Share's ResourceRef through storageSharingResolver
	// (sharing_resolver.go), the structurally-typed adapter over
	// storageModule's own ObjectService -- sharing never imports go/storage
	// itself (resolver.go's own doc comment explains why), so this
	// composition is entirely this app's own, the same way every other
	// no-import-edge seam in this file (OrgFeatureGate,
	// DemoOrgSubjectResolver, ...) is wired. WithTenantConfigReader wires
	// SharingConfigReader (defined above, alongside OrgFeatureGate), so a
	// tenant's own sharing.default_expiry override -- once config.Service
	// exists, after Attach below -- actually governs Service.Create's
	// resolved expiry instead of always falling back to
	// defaultShareExpiry.
	sharingModule := sharing.NewModule(db,
		sharing.WithResourceResolver(&storageSharingResolver{svc: storageModule.ObjectService(), attest: attestationService}),
		sharing.WithTenantConfigReader(SharingConfigReader{Service: &configService}),
	)

	// integrationModule is the reference app's mandatory first consumer of
	// go/integration's outbound-webhook surface.
	// orgMemberJoinedWebhookMapping (webhooks.go) is this app's own
	// EventMapping -- a Module construction-time Option, per
	// integration.WithEventMapping's own doc comment, since the mapping's
	// Transform closes over org's own MemberJoined payload shape, which
	// go/integration may not import. WithWebhookQueue shares the same
	// standaloneQueue every other module's asynchronous work already runs
	// on. WebhookURLValidator/WebhookHTTPClient are test-only overrides
	// (see their own doc comments on ServerConfig above): nil in every
	// production boot, which leaves go/integration's SSRF protection
	// exactly as strict as its default. WithSubjectResolver wires the
	// identical DemoOrgSubjectResolver
	// instance org's and notification's own wiring already share -- it is
	// what lets the module's spec-generated HTTP surface (mounted below
	// through the generic mountModuleRoutes loop) resolve a creator for
	// the two create operations that record one
	// (integration_createAPIKey, integration_createWebhookSubscription),
	// the input those operations need and every other operation this
	// surface mounts does not (the Service-level APIs, exercised directly
	// by this module's own tests, never need one).
	integrationOpts := []integration.Option{
		integration.WithEventMapping(orgMemberJoinedWebhookMapping),
		integration.WithWebhookQueue(standaloneQueue),
		integration.WithSubjectResolver(DemoOrgSubjectResolver{HeaderDisabled: cfg.DisableDemoUserHeader}),
	}
	if cfg.WebhookURLValidator != nil {
		integrationOpts = append(integrationOpts, integration.WithWebhookURLValidator(cfg.WebhookURLValidator))
	}
	if cfg.WebhookHTTPClient != nil {
		integrationOpts = append(integrationOpts, integration.WithWebhookHTTPClient(cfg.WebhookHTTPClient))
	}
	integrationModule := integration.NewModule(db, integrationOpts...)

	// notificationModule is the reference app's first consumer of
	// go/notification, wired end to end as the module's mandatory
	// first-consumer proof (see demo_notification.go for the
	// host-side glue that
	// drives it -- the note-created subscription and the demo
	// patient-message route -- and flowtests/notification_flow_test.go for the
	// end-to-end legs). Its six required seams are all host-supplied here:
	// the console SMS sender writes to the same smsOutput the authn module's
	// sender writes to (the standalone deployment mode's transport,
	// go/notification/sms.go); the mail transport is whatever the "mailer"
	// seam resolves to -- the console mailer in production, cfg.Mailer in
	// tests; the two blind indexers built above make a contact's encrypted
	// email and phone address queryable by exact match; the delivery queue
	// is the same standaloneQueue storage's derive task runs on, so a
	// completed note-created or patient-reminder dispatch is drained by the
	// same worker pool (its Register call declares the delivery job handler
	// on the registry, and the drain loop below moves every handler onto
	// the queue); and the user-address resolver is notification/staticaddr
	// over the demo directory demo_notification.go owns -- the demo users
	// exist only as header values, so the operator-declared table is this
	// app's whole address store. WithSubjectResolver hands the HTTP surface
	// the same demo identity layer type org's handler uses -- its own
	// instance keeps principalFallback unset, so this surface's caller is
	// whoever the X-Demo-User-Id header says (see DemoOrgSubjectResolver's
	// own doc comment on why the two wirings differ) -- the module
	// resolves identity per operation and never reads it from the request
	// otherwise.
	notificationModule := notification.NewModule(db,
		notification.WithSMSSender(pkgcore.NewConsoleSMSSender(smsOutput)),
		notification.WithMailFrom("notifications@reference-app.example"),
		notification.WithContactEmailIndexer(contactEmailIndexer),
		notification.WithContactPhoneIndexer(contactPhoneIndexer),
		notification.WithDeliveryQueue(standaloneQueue),
		notification.WithUserAddressResolver(staticaddr.New(DemoUserAddresses)),
		notification.WithSubjectResolver(DemoOrgSubjectResolver{HeaderDisabled: cfg.DisableDemoUserHeader}),
	)

	// demoModule is the carrier of the app's demo notification type
	// (demo.patient_reminder) and nothing else. It must sit inside the
	// Bootstrap module set for the same reason every message-shipping
	// module must: Kernel.Bootstrap freezes the merged catalog from the
	// Locales() of the modules it is given, and the notification module
	// renders every dispatch from that frozen catalog -- a type whose copy
	// lives outside the set can never render (internal/demo module.go's
	// package comment says so at length).
	demoModule := demo.NewModule()

	// billingModule is the reference app's mandatory first consumer of
	// go/billing -- a module API this app genuinely uses, both judgment
	// halves of the module: the credits ledger
	// through Credits(), and
	// the subscription-derived entitlement path through Entitlements(),
	// which aiGatewayModule's own construction right below wires onto
	// go/ai-gateway's optional Entitlements seam. It shares this app's own
	// db connection, the same pattern every other module here uses.
	//
	// It is deliberately constructed BEFORE aiGatewayModule: Go evaluates
	// statements in order, and the WithEntitlements option below needs
	// billingModule's already-built EntitlementsService in scope. That is
	// safe even though both modules' Register calls (and the gateway's own
	// first Check) come later: billing.NewModule constructs its services
	// at construction time -- no I/O, nothing deferred to Bootstrap (see
	// go/billing/module.go's own NewModule doc comment) -- and
	// EntitlementsService.Check reads the subscription/plan rows fresh on
	// every call, so judging cannot start before the demo seed below has
	// run, no matter how early the service is built.
	//
	// The one deliberately absent wiring is a UsageReader (nil): quota-kind
	// grants need go/billing's real-time usage counter through that reader,
	// and this app still wires no usage reader into billing -- which is why
	// demo_entitlements.go's seed grants are Boolean, never Quota (see that
	// file's own doc comment). No WithQueue either: no payment-channel
	// gateway is wired, so PollingService's active-polling fallback has
	// nothing to poll -- an actual payment-gateway integration (a real
	// Stripe/Alipay/WeChat sandbox charge) stays out of this app's
	// scope.
	billingModule := billing.NewModule(db, nil)

	// meteringModule is go/metering's seat in this app: admin's D9 usage
	// dashboard reads its per-tenant summary rows (admin.WithMetering
	// below), and the ai-gateway module's construction right below records
	// every real consult/smilesim AI call's usage into it through the
	// gateway's UsageRecorder seam. Constructed here, immediately after
	// billingModule and before aiGatewayModule, because both later
	// constructions consume it -- the identical reason billingModule is
	// built before aiGatewayModule (Go evaluates statements in order, and
	// the WithMetering/WithUsageRecorder options below need the built
	// module in scope). NewModule performs no I/O (see go/metering/
	// module.go's own doc comment), so its Register-time declarations --
	// its two config items and its one published event -- and its
	// migration set register and apply with every other module's below.
	meteringModule = metering.NewModule(db)

	// aiGatewayModule is the reference app's mandatory first consumer of
	// go/ai-gateway: the
	// internal/consult service (wired below, after Bootstrap) calls its
	// Gateway.Chat under consult.LogicalModel ("chat:default"), which
	// WithModelRoute routes to the module's own zero-external-dependency
	// default provider, aigateway.ProviderOpenAICompatible. The vendor
	// model id ("gpt-4o-mini") is opaque to this app -- it is passed
	// through to whatever OpenAI-compatible endpoint the resolved
	// credential's base URL actually names (the real OpenAI API in a real
	// deployment, an httptest.Server in flowtests/consult_flow_test.go) -- and never
	// seen by consult's own code, per aigateway.ChatRequest.Model's own
	// doc comment on why business code never hardcodes a vendor model id.
	// aiGatewayModule additionally wires the image-generation
	// pipeline: the internal/smilesim service (wired below, after
	// Bootstrap) calls Gateway.GenerateImage under smilesim.LogicalModel
	// ("image:smile-simulation"), routed to the module's own
	// zero-external-dependency default image provider,
	// aigateway.ProviderOpenAICompatibleImage. WithImageGeneration shares
	// this app's own standaloneQueue -- the same pool storage's
	// thumbnail-derive task and notification's delivery task already run
	// on -- and storageModule's own ObjectService, so the job handler
	// go/ai-gateway registers on reg.Jobs (drained onto standaloneQueue
	// below, alongside every other module's job handlers) reads the
	// patient photo and writes the generated simulation back through the
	// very same storage this app's other consumers use.
	//
	// WithEntitlements wires
	// billingModule.Entitlements() -- the real billing.EntitlementsService
	// constructed above -- onto go/ai-gateway's optional, structurally-
	// typed Entitlements seam, so BOTH halves of this one Gateway instance
	// (Chat for consult, GenerateImage for smilesim; they share the very
	// same *aigateway.Gateway) gate every request on the calling tenant's
	// subscription before any provider is reached, exactly go/ai-gateway's
	// own checkEntitlement design: key "model:"+logicalModel, requested 1,
	// denial answered with ErrEntitlementDenied before the provider sees
	// the call. The adapter is an aigateway.EntitlementsFunc closure rather
	// than a direct assignment because the two modules' Decision types are
	// distinct named types (billing.Decision vs aigateway.Decision) -- the
	// exact no-adapter-to-write claim go/ai-gateway/seams.go's own doc
	// comment makes is a deliberate simplification that does not compile;
	// the closure shape below is that file's own documented example,
	// verbatim. An unwired seam (nil) would let every request through; a
	// wired one judged against an empty database would deny everything --
	// seedDemoEntitlements (boot, below) is what keeps the seeded demo
	// tenants on the allowed side from the very first request.
	// gatewayEntitlements is the ONE adapter instance this app's gateway
	// gate and smilesim's own pre-flight both run through. Binding it as a
	// value first lets the same closure be handed to two consumers:
	// aiGatewayModule's WithEntitlements below (the gateway's own gate,
	// checked inside Chat/ChatStream/GenerateImage before any provider is
	// reached) and smileSimService's NewService call further down (whose
	// Simulate pre-flights the gate BEFORE its credit reservation opens,
	// so a refused request never writes a PreDeduct/Refund pair -- see
	// internal/smilesim/service.go's Simulate doc comment). The two must
	// answer through the very same seam, or the pre-flight and the
	// gateway's re-check could disagree about the same state.
	gatewayEntitlements := aigateway.EntitlementsFunc(
		func(ctx context.Context, featureKey string, requested int64) (aigateway.Decision, error) {
			decision, checkErr := billingModule.Entitlements().Check(ctx, featureKey, requested)
			if checkErr != nil {
				return aigateway.Decision{}, checkErr
			}
			return aigateway.Decision{Allowed: decision.Allowed, Reason: string(decision.Reason)}, nil
		},
	)

	// WithUsageRecorder is the metering half of this round's D9 wiring: it
	// hands go/metering's analytics-grade Recorder to the SAME Gateway
	// instance both halves above run on, so every successful Chat/ChatStream
	// call records its token usage automatically -- docs/internal/
	// 08-ai-gateway.md's rule that AI metering is a built-in behavior
	// needing no manual reporting (image generation records the same way,
	// through the job handler go/ai-gateway registers). The adapter is an
	// aigateway.UsageRecorderFunc closure because the two modules' UsageEvent
	// types are distinct named types (ai-gateway deliberately never imports
	// go/metering -- see go/ai-gateway/seams.go's UsageEvent doc comment);
	// the closure below is that file's own documented example, verbatim.
	// meteringModule.Recorder() is the analytics-grade (fail-open) tier, the
	// only tier this seam's Record(ctx, event) shape can carry: the
	// billing-grade Enqueue path demands the caller's own transaction
	// handle, which a recorder callback has no room for (go/metering/
	// recorder.go's own Recorder doc comment). The events it buffers are
	// folded into real metering_usage_summaries rows by the recorder's
	// background flush loop once meteringModule.Start runs below, and
	// admin's D9 dashboard (adminModule's WithMetering wiring, below) reads
	// those rows back per tenant. The one feature this seam reports --
	// "ai.chat_tokens" -- is recorded through this tier alone, never also
	// through the billing-grade Enqueue path, per go/metering/recorder.go's
	// one-feature-one-tier rule.
	meteringUsageRecorder := meteringModule.Recorder()
	aiGatewayModule := aigateway.NewModule(db,
		aigateway.WithModelRoute(consult.LogicalModel, aigateway.ProviderOpenAICompatible, "gpt-4o-mini"),
		aigateway.WithModelRoute(smilesim.LogicalModel, aigateway.ProviderOpenAICompatibleImage, "dall-e-3"),
		aigateway.WithImageGeneration(standaloneQueue, storageModule.ObjectService()),
		aigateway.WithEntitlements(gatewayEntitlements),
		aigateway.WithUsageRecorder(aigateway.UsageRecorderFunc(
			func(ctx context.Context, event aigateway.UsageEvent) error {
				return meteringUsageRecorder.Record(ctx, metering.UsageEvent{
					TenantID:       event.TenantID,
					Feature:        event.Feature,
					Quantity:       event.Quantity,
					IdempotencyKey: event.IdempotencyKey,
					Metadata:       event.Metadata,
				})
			},
		)),
	)

	// complianceModule is the reference app's first consumer of
	// go/compliance: admin's audit-query HTTP shell reads through
	// complianceModule.AuditQuery(), which itself is a read-only wrapper
	// over the SAME audit.Repository (over this same database connection)
	// auditModule's own write-capture persister already writes into --
	// sharing one connection to one audit_events table, exactly as
	// auditModule shares notesModule's own connection above. WithQueue is
	// the same standaloneQueue every other module's asynchronous work
	// already shares; compliance.Module.Register refuses to proceed
	// without one (ErrQueueRequired) regardless of whether a caller
	// happens to use that half of the module. WithSharing wires the
	// already-constructed sharingModule's own Service() as
	// compliance.SharingCreator -- the seam an owning module opts into;
	// go/admin's audit-query export leg is that
	// module, so this is where compliance.ExportService.Export gains its
	// first genuine delivery path (a real, single-view go/sharing link)
	// rather than refusing every call with ErrSharingRequired.
	// WithExportConfigReader wires ComplianceConfigReader (defined above,
	// alongside OrgFeatureGate and SharingConfigReader), so a tenant's own
	// compliance.export_delivery_expiry override -- once config.Service
	// exists, after Attach below -- actually governs
	// ExportService.Export's minted delivery-link expiry instead of always
	// falling back to defaultExportDeliveryExpiry.
	complianceAuditRepo := audit.NewRepository(db)
	complianceModule := compliance.NewModule(complianceAuditRepo,
		compliance.WithQueue(standaloneQueue),
		compliance.WithSharing(sharingModule.Service()),
		compliance.WithExportConfigReader(ComplianceConfigReader{Service: &configService}),
	)

	// adminModule is the reference app's mandatory first consumer of
	// go/admin's operator-facing surface: the tenant ledger, the
	// impersonation pipeline, the cross-tenant user search, the
	// audit-query shell and export leg, role management and the
	// usage/billing dashboard. WithMetering hands it the meteringModule
	// constructed above (whose metering_usage_summaries rows the
	// ai-gateway UsageRecorder bridge, also above, feeds for real on
	// every consult/smilesim AI call), and WithBilling hands it the same
	// billingModule every other consumer in this file already uses (its
	// credit balances seeded by seedDemoCredits and subscriptions by
	// seedDemoEntitlements below). Both options are optional by
	// go/admin's own contract (a host wiring neither gets
	// ErrUsageModulesNotWired from UsageService.Summary, a host wiring
	// one gets that dimension present and the other absent); this app
	// wires both, so GET /api/v1/admin/usage-summary answers 200 with the
	// real per-tenant dashboard -- see flowtests/usage_summary_flow_test.go.
	// WithAuthn takes the *authn.Module itself, not its
	// Service() -- see admin.Module.DependsOn()'s own doc comment for the
	// resulting "authn" dependency Kernel.Bootstrap's sort honors below.
	// WithQueue is the same standaloneQueue every other module's
	// asynchronous work already shares -- the audit-query export leg
	// enqueues onto
	// it rather than running compliance.ExportService.Export synchronously
	// inside the request.
	adminModule := admin.NewModule(db,
		admin.WithAuthn(authnModule),
		admin.WithOrg(orgModule),
		admin.WithCompliance(complianceModule),
		admin.WithNotification(notificationModule),
		admin.WithMetering(meteringModule),
		admin.WithBilling(billingModule),
		admin.WithQueue(standaloneQueue),
	)

	migrationRegistry := dbkit.NewMigrationRegistry()
	if regErr := migrationRegistry.Register(pkiModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(authnModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(rbacModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(notesModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(orgModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(configModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(auditModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(storageModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(sharingModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(integrationModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	// demoModule is deliberately absent from this registry: it ships no
	// migrations (its Migrations() is an empty FS -- see internal/demo's
	// module doc), so there is nothing to register or apply for it.
	if regErr := migrationRegistry.Register(notificationModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(aiGatewayModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(billingModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(meteringModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	// complianceModule is deliberately absent from this registry too: it
	// ships no migrations of its own (Migrations() is an empty FS) --
	// every row it reads or writes lives in auditModule's own
	// audit_events table, already registered above.
	if regErr := migrationRegistry.Register(adminModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if applyErr := migrationRegistry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: apply migrations: %w", applyErr)
	}

	// Bootstrap registers the modules in argument order -- pki leads the
	// set (a position no other module's Register depends on, mirroring
	// its lead in the migration registry above: authn's construction
	// already consumed pkiModule.Service(), so this app's composition
	// order simply starts with pki), with authn immediately behind it, so
	// authn's Register-time declarations (its config items, its
	// permissions, its events) precede the modules that lean on them,
	// then notes and org before config, so the configuration items and
	// feature flags their Register calls declare (notes' own, and org's
	// read-only reliance on the org.invitations / org.invitation_email
	// flags it declares itself) are in the registry before the config
	// module's own Register runs, and then Attach freezes the schema
	// snapshot those declarations fold into, exactly the sequence config's
	// Attach doc comment prescribes ("after Kernel.Bootstrap has
	// returned"). rbac comes next so its own Attach -- which snapshots
	// every permission every module declared during Register -- runs
	// against a Registry that has already seen every module's
	// declarations. storage follows rbac simply because its own order is
	// not load-bearing: its DependsOn is nil, the queue its Register
	// validates is the host seam built above, and the permissions it
	// declares are folded into rbac's Attach snapshot no matter where it
	// sits. demo and notification follow storage for the same reason:
	// neither's position is load-bearing -- demo's Register only declares
	// its notification type, and notification's Register only validates
	// its host seams and registers its delivery job handler -- but both
	// must sit inside this set, and together: the merged catalog freezes
	// once after every module has registered, and notification renders
	// every dispatch (the demo patient-reminder copy included) from that
	// frozen catalog, so a demo whose templates lived outside the set
	// could never render (internal/demo module.go's package comment says
	// so at length). aiGatewayModule follows notification for the same
	// not-load-bearing reason: its own Register declares nothing but the
	// SystemPurposeCredentialWrite system purpose (go/ai-gateway/module.go's
	// Register doc comment), which the platform-credential write below
	// only needs registered before it runs, not before any other module's
	// Register. billingModule follows aiGatewayModule for the same
	// not-load-bearing reason: its own Register (module.go) declares only
	// its permissions, its five credit-ledger audit actions and its two
	// published events, none of which any other module's own Register
	// depends on -- its DependsOn is nil, matching go/metering's and
	// go/pki's own identical answer for the same reason. meteringModule
	// follows billingModule for the same not-load-bearing reason: its own
	// Register only attaches the registry's already-wired bus onto its
	// in-process Aggregator and declares its two config items and one
	// published event, none of which any other module's Register consumes
	// (the config items join the schema configModule.Attach freezes after
	// Bootstrap regardless of where this Register runs). complianceModule
	// follows for the same not-load-bearing
	// reason (it ships no migrations and validates only its own queue seam);
	// adminModule follows it and IS load-bearing in one respect --
	// admin.Module.DependsOn() names "authn", so Bootstrap's own dependency
	// sort (sortModulesByDependency) runs authn's Register before admin's
	// regardless of argument order, which is what makes
	// authnModule.Service() non-nil by the time admin's Register reads it.
	// audit last is not load-bearing order -- its Module.DependsOn
	// is nil, and its subscriptions are valid to install before or after
	// any publisher registers (see audit's Module.DependsOn doc comment)
	// -- it simply reads naturally as "the business-facing modules, then
	// the cross-cutting persister watching them."
	//
	// A single Registry -- and so a single EventBus, reg.EventBus() --
	// serves every module Bootstrap registers here, which is what lets
	// auditModule's subscriptions (installed inside its own Register)
	// actually receive the audit.EventRecorded event notesModule's
	// handler publishes through audit.Emit (see NewHandler's wiring
	// below), and what lets orgModule's own subscriptions receive
	// authn's UserCreated event on that same bus. The bus itself was
	// constructed above -- this app wires dbkit.Options.AuditBus for org's
	// automatic write capture, which is why the construction happens
	// before dbkit.Open, and why the SAME bus is injected here: see the
	// Open call's own comment for the full reasoning, and flowtests/org_audit_capture_test.go
	// for the composed proof.
	//
	// WithDeploymentMode(cfg.DeploymentMode) declares the topology the
	// composition is validated against; it never selects an implementation
	// (a deployment mode constrains which implementations may compose,
	// never selects one). Every
	// stateful seam below except the eventbus seam follows the same
	// conditional-injection shape: an unset env var leaves THAT seam on
	// the Preset's in-process default (so a plain `go run ./cmd/server`
	// needs nothing else running), and a configured one
	// injects a real implementation with the capability bits that
	// implementation genuinely carries. The eventbus seam is the one
	// deliberate exception -- it is injected in BOTH branches, memory or
	// Redis, because its construction happens before
	// dbkit.Open (see the Open call's own comment) and Kernel.Bootstrap
	// must resolve to that same pre-built bus.
	//
	// When APP_REDIS_ADDR is set, that pre-built bus is a REAL
	// Redis-backed EventBus -- eventbus/redis's NewEventBus over a go-redis
	// client this host constructs and owns -- declaring
	// MultiReplicaSafe|SurvivesRestart, the capabilities the Redis Streams
	// implementation genuinely carries. The SAME client also backs the
	// "kv" seam (kv/redis.NewKVStore takes the identical *redis.Client
	// type): one Redis instance backing both seams is this app's
	// deliberate minimal-footprint choice, mirroring go/notification's own
	// Redis integration leg, which shares one client across two bus
	// instances. This is not merely a convenience -- a distributed
	// deployment mode requires MultiReplicaSafe of every seam that carries
	// shared state, so wiring the "eventbus" seam alone can never let a
	// distributed composition succeed: the very next seam Kernel.Bootstrap
	// resolves and validates, "kv", would still fail on the Preset's
	// in-process default.
	//
	// When the APP_S3_* variables are set (the S3 fields' own doc comment
	// (bootstrap.go) has the completeness rule), WithObjectStore
	// injects a REAL S3-compatible ObjectStore (objectstore/s3.
	// NewObjectStore, reaching MinIO, Aliyun OSS or AWS S3 through the
	// minio-go client), declaring the same MultiReplicaSafe|SurvivesRestart
	// the "objectstore.s3" builtin registration itself declares. A
	// non-empty cfg.ObjectStoreRoot (from APP_OBJECT_STORE_ROOT, or
	// injected straight onto the struct by a test) is the local twin of
	// that composition and an alternative to it: WithObjectStore then
	// injects pkgcore.NewLocalObjectStore over the fixed directory,
	// declaring SurvivesRestart alone -- the directory genuinely survives
	// a process restart, and genuinely nothing about a single-process
	// local store is replica-safe, so MultiReplicaSafe is never claimed
	// for it.
	//
	// The Mailer override -- cfg.Mailer, non-nil either because
	// ConfigFromEnv composed a real pkgcore.NewSMTPMailer from the
	// APP_SMTP_* variables, or because a flowtests org or notification
	// suite injected an in-process capture double --
	// rides along as a fourth conditional option. Its capability
	// declaration is cfg.MailerCapabilities when set (MultiReplicaSafe|
	// SurvivesRestart for the real SMTP composition, matching the
	// "mailer.smtp" builtin's own declaration) and pkgcore.Stateless
	// otherwise -- the honest capability for a throwaway in-process test
	// double.
	//
	// Nothing about the rest of this wiring changes when any of these are
	// injected: audit.Emit still publishes on reg.EventBus() (the injected
	// bus), auditModule's subscriptions run synchronously on the
	// publishing side exactly as they do on the in-memory bus, and any
	// OTHER process consuming the same Redis streams, the same S3 bucket
	// or the same SMTP relay observes the same effects.
	//
	// go-redis is imported here -- and go.mod therefore requires it
	// directly -- because the app is the assembly host that eventbus/redis's
	// EventBus contract names as the client's owner: NewEventBus builds on a
	// client the host constructs and keeps owning ("the client the bus was
	// built on stays open, because the host owns it" -- Close's own doc
	// comment), which is exactly why cleanup below closes redisClient
	// itself. The no-concrete-infrastructure-implementation rule constrains
	// business modules, not the application that assembles them.
	kernelOptions := []pkgcore.KernelOption{pkgcore.WithDeploymentMode(cfg.DeploymentMode)}
	// The eventbus seam is injected in BOTH branches -- not left to the
	// Preset's default -- because the bus was already constructed above
	// (before dbkit.Open wired it as Options.AuditBus): reg.EventBus()
	// must resolve to that same bus, or org's captured writes would
	// publish onto a bus auditModule's subscriptions never see, each half
	// of the audit path working in isolation while no row ever lands.
	// busCapabilities carries the declaration of whichever implementation
	// the branch above chose (zero for the in-process memory bus, the
	// same declaration its "eventbus.memory" builtin registration
	// carries).
	kernelOptions = append(kernelOptions, pkgcore.WithEventBus(bus, busCapabilities))
	if cfg.RedisAddr != "" {
		kernelOptions = append(kernelOptions,
			pkgcore.WithKVStore(kvredis.NewKVStore(redisClient), pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart))
	}
	if cfg.S3Endpoint != "" {
		kernelOptions = append(kernelOptions,
			pkgcore.WithObjectStore(objectstores3.NewObjectStore(objectstores3.Config{
				Endpoint:  cfg.S3Endpoint,
				Bucket:    cfg.S3Bucket,
				AccessKey: cfg.S3AccessKey,
				SecretKey: cfg.S3SecretKey,
				Region:    cfg.S3Region,
				UseSSL:    cfg.S3UseSSL,
			}), pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart))
	}
	if cfg.ObjectStoreRoot != "" {
		kernelOptions = append(kernelOptions,
			pkgcore.WithObjectStore(pkgcore.NewLocalObjectStore(cfg.ObjectStoreRoot), pkgcore.SurvivesRestart))
	}
	if cfg.Mailer != nil {
		mailerCapabilities := cfg.MailerCapabilities
		if mailerCapabilities == 0 {
			mailerCapabilities = pkgcore.Stateless
		}
		kernelOptions = append(kernelOptions, pkgcore.WithMailer(cfg.Mailer, mailerCapabilities))
	}
	reg, err := pkgcore.NewKernel(kernelOptions...).Bootstrap(ctx, pkiModule, authnModule, notesModule, orgModule, configModule, rbacModule, storageModule, sharingModule, integrationModule, demoModule, notificationModule, aiGatewayModule, billingModule, meteringModule, complianceModule, adminModule, auditModule)
	if err != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: bootstrap kernel: %w", err)
	}
	// The loader target must bind the bootstrap surface this composition
	// declares: every key the modules above declared on the registry's
	// bootstrap seat, and every key this app owns (bootstrap.go's
	// hostBootstrapKeys). A module that adds or renames a declared key fails
	// this boot until the target grows the matching field, which is the point:
	// a declared key the host never resolves is a key whose contract silently
	// binds to nothing.
	if verifyErr := verifyBootstrapBinding(reg); verifyErr != nil {
		_ = cleanup()
		return nil, nil, nil, verifyErr
	}
	// Bind the org-backed half of the membership store here, only now that
	// Bootstrap has run: the store's enumeration answer
	// (signInMemberships.TenantsOf) takes an audited system-context grant
	// through tenancy.WithSystemContext, which publishes its audit event on
	// the bus Bootstrap finished wiring (reg.EventBus), and the purpose the
	// grant names must be declared before any sign-in can ask the question
	// -- the same once-per-boot, idempotent declaration a module's Register
	// makes for its own purposes. Nothing before this point can serve a
	// sign-in: authn answers membership questions only inside a Login or
	// Refresh call, and the first ones reach the store with the demo seeds
	// below.
	pkgcore.RegisterSystemPurpose(signInTenantEnumerationPurpose)
	memberships.attach(orgModule.Members(), reg.EventBus())
	// integrationModule's Attach must run after Bootstrap for the same
	// reason config's and rbac's do just below: its Service reads
	// reg.Events.Bus() and reg.AuditActions, which Bootstrap only finishes
	// wiring once every module's Register call has returned (module.go's
	// own Attach doc comment). Unlike config's and rbac's Attach calls, its
	// ordering relative to them is not load-bearing -- nothing here reads a
	// permission or configuration snapshot. Its return value is discarded:
	// every spec-generated surface the module mounts reads the Service at
	// call time through Handler's own Register-time forwarding wrapper, so
	// nothing here needs the *integration.Service itself.
	if _, attachErr := integrationModule.Attach(reg); attachErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: attach the integration module: %w", attachErr)
	}
	configService, err = configModule.Attach(reg)
	if err != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: attach the config module: %w", err)
	}
	// With the config service live, open the sign-in channels this host
	// actually assembled: authn's social flags default OFF (a channel with
	// no configured credentials must not appear on the login page), so a
	// provider this app wired through cfg.SocialProviders would otherwise
	// refuse with authn.channel_disabled the moment the gate wired into
	// authn above became real -- and the login page, which reads the same
	// system-tier rows through /api/v1/config/features, would agree that the
	// channel does not exist. See openConfiguredAuthnChannels' own doc
	// comment for the full reasoning. When cfg.SocialProviders is empty the
	// step writes nothing at all.
	if flagErr := openConfiguredAuthnChannels(ctx, configService, cfg.SocialProviders); flagErr != nil {
		_ = cleanup()
		return nil, nil, nil, flagErr
	}
	// OnConfigReady, when non-nil, receives the live *config.Service now
	// that the channel-flag rows above are in place -- the post-Attach
	// seam a test needs to write further rows through the real Set path
	// (ServerConfig's field doc).
	if cfg.OnConfigReady != nil {
		cfg.OnConfigReady(configService)
	}
	// rbac's Attach must also come after Bootstrap, and for a sharper
	// reason than config's: what it freezes is the snapshot of every
	// permission every module declared, so a snapshot taken any earlier
	// would be missing whatever registered after it -- and a permission
	// missing from that catalog cannot be granted at all.
	rbacService, err = rbacModule.Attach(reg)
	if err != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: attach the rbac module: %w", err)
	}
	if seedErr := seedDemoGrants(ctx, rbacService, cfg.HostTenants); seedErr != nil {
		_ = cleanup()
		return nil, nil, nil, seedErr
	}
	if cfg.OnRBACReady != nil {
		cfg.OnRBACReady(rbacService)
	}
	// seedDemoCredits is the demo, NOT-a-real-payment stand-in for a real
	// buy-a-credit-pack flow -- see that function's own doc comment for
	// exactly why a real Stripe/Alipay/WeChat sandbox charge stays out of
	// scope here. It runs
	// unconditionally, like seedDemoGrants just above,
	// regardless of cfg.DemoUsersPassword: internal/smilesim's own tests
	// (flowtests/smilesim_flow_test.go) need a real, non-zero starting balance on
	// tenant-acme to exercise the successful-generation leg, exactly the
	// way seedDemoGrants' own roles are needed by every test that gates a
	// route on a permission, demo password or not.
	if seedErr := seedDemoCredits(ctx, billingModule.Credits(), cfg.HostTenants); seedErr != nil {
		_ = cleanup()
		return nil, nil, nil, seedErr
	}

	// seedDemoEntitlements is the demo, NOT-a-real-purchase stand-in for
	// the "tenant buys a subscription, the payment channel confirms it"
	// leg of a real billing flow -- see
	// that function's own doc comment for exactly why a real
	// Stripe/Alipay/WeChat sandbox charge stays out of scope here. It runs
	// unconditionally, like seedDemoCredits just above: since the gateway
	// below is wired with WithEntitlements, every consult/smilesim request
	// is gated on the calling tenant holding an Active subscription to a
	// Plan granting that route's model key -- without this seed the demo
	// tenants would have none and every demo AI route would answer
	// aigateway.entitlement_denied. Running it here, before any route can
	// serve, also means the seam is live (never nil and never judging an
	// empty database) from the very first request.
	if seedErr := seedDemoEntitlements(ctx, billingModule.Plans(), billingModule.Subscriptions(), cfg.HostTenants); seedErr != nil {
		_ = cleanup()
		return nil, nil, nil, seedErr
	}

	// notes' retention participant is registered here, after Bootstrap --
	// compliance's Register is what attaches the Retention registrar the
	// kernel's reg.Retention seat resolves to, so Add before Bootstrap
	// would silently register onto a registrar nothing sweeps with (see
	// internal/notes/retention_participant.go's own doc comment). The
	// participant is built over the very dbkit.Open *gorm.DB the notes
	// Module already uses -- share the connection, never a second pool --
	// with notes.NewRepository(db) the same fresh-repository-over-the-
	// shared-db shape every earlier host-side seam wiring in this file
	// follows. Add returns ErrDuplicateRetentionParticipant on a repeated
	// Name, refused here like any other wiring failure.
	if err := reg.Retention.Add(notes.NewRetentionParticipant(notes.NewRepository(db))); err != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register notes retention participant: %w", err)
	}

	// admin's role-management surface needs the real *rbac.Service --
	// which, like config's and rbac's own Attach calls above, exists only
	// after Bootstrap has returned. This is why go/admin's RoleService is
	// wired through a distinct, post-Bootstrap Module.AttachRBAC call
	// rather than a WithXxx(*rbac.Module) construction-time Option the
	// way authn/org/compliance/notification are -- see AttachRBAC's own
	// doc comment for the full reasoning.
	adminModule.AttachRBAC(rbacService)

	// The ai-gateway platform credential: written only when cfg.AIGatewayAPIKey
	// is set (see its own doc comment on ServerConfig for why the default is
	// empty). This must run after Bootstrap, because aiGatewayModule.Register
	// is what calls pkgcore.RegisterSystemPurpose(aigateway.SystemPurposeCredentialWrite)
	// -- WithSystemContext below refuses an unregistered purpose -- and the
	// write itself needs the module's own CredentialService, which
	// aiGatewayModule.Credentials() exposes regardless of Bootstrap having
	// run (constructing a Module performs no I/O), so nothing here strictly
	// needs to wait for Bootstrap except the purpose registration. The bare
	// pkgcore.WithSystemContext rather than the audited
	// tenancy.WithSystemContext is deliberate, for this boot-time
	// precondition: the write is a config-driven declaration re-affirmed
	// identically on every restart under the fixed "reference-app-boot"
	// actor, with no operator session or ticket to attribute an audit row
	// to, so no audit reader has a question this grant's record would
	// answer -- the request-time platform-credential path in ai-gateway's
	// own handler.go, by contrast, must and does go through the audited
	// wrapper.
	if cfg.AIGatewayAPIKey != "" {
		sysCtx, sysErr := pkgcore.WithSystemContext(ctx, pkgcore.SystemReason{
			Actor:   "reference-app-boot",
			Purpose: aigateway.SystemPurposeCredentialWrite,
		})
		if sysErr != nil {
			_ = cleanup()
			return nil, nil, nil, fmt.Errorf("reference-app: build the ai-gateway credential system context: %w", sysErr)
		}
		if credErr := aiGatewayModule.Credentials().SetPlatformCredential(
			sysCtx, aigateway.ProviderOpenAICompatible, cfg.AIGatewayAPIKey, cfg.AIGatewayBaseURL,
		); credErr != nil {
			_ = cleanup()
			return nil, nil, nil, fmt.Errorf("reference-app: set the ai-gateway platform credential: %w", credErr)
		}
	}
	// The ai-gateway image-generation platform credential -- the same
	// system-context path as the chat credential above, written only when
	// cfg.AIGatewayImageAPIKey is set (see its own doc comment on
	// ServerConfig), including the same deliberate bare-primitive choice
	// and its boot-time precondition: a config-driven write re-affirmed on
	// every restart under the fixed "reference-app-boot" actor, with no
	// operator session or ticket to attribute an audit row to.
	if cfg.AIGatewayImageAPIKey != "" {
		sysCtx, sysErr := pkgcore.WithSystemContext(ctx, pkgcore.SystemReason{
			Actor:   "reference-app-boot",
			Purpose: aigateway.SystemPurposeCredentialWrite,
		})
		if sysErr != nil {
			_ = cleanup()
			return nil, nil, nil, fmt.Errorf("reference-app: build the ai-gateway image credential system context: %w", sysErr)
		}
		if credErr := aiGatewayModule.Credentials().SetPlatformCredential(
			sysCtx, aigateway.ProviderOpenAICompatibleImage, cfg.AIGatewayImageAPIKey, cfg.AIGatewayImageBaseURL,
		); credErr != nil {
			_ = cleanup()
			return nil, nil, nil, fmt.Errorf("reference-app: set the ai-gateway image platform credential: %w", credErr)
		}
	}

	// Wire the queue to the registry's declarations and start the pool.
	// Only now -- after Bootstrap -- can the wiring run: the modules'
	// Register calls declared the handlers on the registry (each handler's
	// backing service attached its seams in the same call), and jobs.Wire
	// drains that map onto standaloneQueue, refusing an entry that is not
	// a jobs.Handler rather than mis-typing it into a worker at job-claim
	// time, and creating the queue's own tables (no migration of this
	// host's is involved) so an Enqueue needs no Start first. Start is
	// non-blocking -- it launches the dispatcher and worker goroutines and
	// returns -- so the first enqueued job (a completed object's thumbnail
	// derivation) waits only as long as a poll of the queue's own task
	// table.
	if err := jobs.Wire(ctx, standaloneQueue, reg.Jobs); err != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: wire the job queue: %w", err)
	}
	// cfg.DisableQueueWorker skips Start entirely rather than merely
	// declining to enqueue: jobs.Wire above still runs (so the queue's
	// tables exist and a stray Enqueue call from another module's wiring is
	// never refused with jobs.ErrHandlerNotRegistered), but with no
	// dispatcher and no worker goroutines launched, this replica can never
	// claim or execute a Job of any type -- see the DisableQueueWorker
	// field's own doc comment (bootstrap.go) for why.
	if !cfg.DisableQueueWorker {
		if err := standaloneQueue.Start(ctx); err != nil {
			_ = cleanup()
			return nil, nil, nil, fmt.Errorf("reference-app: start the job queue: %w", err)
		}
		// Start the host's periodic-task scheduler on the same gate: a
		// task this replica can never execute is pointless to enqueue, so
		// the ticker that enqueues the wired mechanisms' tasks -- storage's
		// per-tenant expiry sweep and compliance's per-tenant retention
		// sweep over the scheduler's tenant universe, plus pki's
		// signing-key expiry scan -- only starts once the queue worker
		// did. The universe is the configured host tenants joined with
		// go/admin's tenant ledger (periodic_scheduler.go's
		// periodicTenantUniverse), so a self-registered clinic -- a tenant
		// this app's own registration flow provisions at runtime, never a
		// cfg.HostTenants value -- is swept from the tick after its org
		// root's ledger row lands. See periodic_scheduler.go.
		// context.Background(), never ctx, per
		// startPeriodicTaskScheduler's own doc comment: the enqueues must
		// keep running until cleanup's own periodicTaskSchedulerStop call,
		// not be cut short by whatever cancels BuildServer's own ctx.
		periodicTaskSchedulerStop = startPeriodicTaskScheduler(
			context.Background(),
			cfg.PeriodicTaskInterval,
			newPeriodicTenantUniverse(cfg.HostTenants, adminModule.Tenants()),
			storageModule.LifecycleService(),
			complianceModule.Retention(),
			pkiModule.Service(),
		)
	}
	// Start meteringModule's background pipelines now that Bootstrap has
	// returned (Register attached the registry's bus onto its Aggregator,
	// and Start itself must not run before that -- go/metering/module.go's
	// own New/Start split). This is what makes the ai-gateway UsageRecorder
	// wiring above real: without the recorder's flush loop running, a
	// recorded Chat call's event would sit in the analytics buffer forever
	// instead of folding into the metering_usage_summaries rows admin's D9
	// dashboard reads. Unlike the job-queue block above this is not gated
	// on cfg.DisableQueueWorker -- that flag governs the jobs.Queue worker
	// only, and metering's own two loops are independent of it. Start is
	// safe to call with ctx canceled or after a Stop (each loop no-ops a
	// second Start; see analytics.go and dispatcher.go), and cleanup's
	// meteringModule.Stop stops both loops and drains the recorder.
	meteringModule.Start(ctx)

	mux := http.NewServeMux()
	hostcore.MountLiveness(mux)
	orgGuardDeps := OrgRouteGuardDeps{scope: orgModule.Scope(), members: orgModule.Members()}
	adminHandler, authnHandler, mountErr := mountModuleRoutes(mux, reg, rbacService, orgGuardDeps, cfg.DisableDemoUserHeader)
	if mountErr != nil {
		_ = cleanup()
		return nil, nil, nil, mountErr
	}

	// hostcore.RegisterMountedRoutes hands every obs.Middleware this
	// process later constructs this app's REAL route table -- /healthz,
	// /metrics (which no module registered) and every route
	// mountModuleRoutes just mounted on mux -- so the route-label limiter
	// the middleware builds reserves a slot for each real route BEFORE any
	// request traffic arrives; see its own doc comment for the mechanism
	// and why the reservation matters. Registration is a snapshot consumed
	// at obs.Middleware CONSTRUCTION, so it must happen here, at assembly
	// time, before main.go's run builds the middleware that serves
	// traffic -- not after the server starts listening. Last registration
	// wins, and every BuildServer call registers the same table, so the
	// repeated calls this package's tests make are idempotent in effect.
	hostcore.RegisterMountedRoutes(reg)

	// wireDemoNotification adds the reference app's demo glue on top of the
	// mounted module routes: the subscription that turns notes' note-created
	// event into a notification dispatch for the note's creator, and the
	// hand-written demo patient-message route that dispatches the demo
	// module's patient-reminder type to a verified external contact. The
	// bus is reg.EventBus() -- the same bus Bootstrap gave every module, so
	// the note-created subscription hears exactly what notesModule's handler
	// publishes on that bus -- and the notificationModule services are the
	// module's own accessors, the same instances its Register validated and
	// its HTTP handler drives (see demo_notification.go for the seam
	// contracts, and flowtests/notification_flow_test.go for the end-to-end legs).
	// The call cannot fail: nothing it does returns an error.
	wireDemoNotification(mux, reg.EventBus(), notificationModule)

	// wireConsult mounts go/ai-gateway's mandatory-first-consumer route
	// (consult.go): consultService shares notesModule's own
	// database connection through a fresh notes.Repository, exactly the way
	// auditModule shares it above -- no new infrastructure dependency is
	// needed for this app to have a real consult surface -- and asks
	// aiGatewayModule's own Gateway, the same instance
	// aiGatewayModule.Register validated. The call cannot fail: nothing it
	// does returns an error.
	consultService := consult.NewService(notes.NewRepository(db), aiGatewayModule.Gateway())
	wireConsult(mux, consultService)

	// wireSmileSim mounts the image-generation half of go/ai-gateway's
	// mandatory-first-consumer routes (smilesim.go):
	// smileSimService asks
	// aiGatewayModule's own Gateway -- the same instance
	// aiGatewayModule.Register validated -- to run an async smile
	// simulation over a patient photo already uploaded through
	// storageModule's own HTTP surface, and the job-status route polls the
	// same standaloneQueue every other async task in this app shares.
	// billingModule.Credits() is the same *billing.CreditService instance
	// seedDemoCredits granted the demo tenants' starting balance against
	// above: smilesim.Service reserves smilesim.CreditsPerSimulation credits from it
	// before ever calling Gateway.GenerateImage, and settles that
	// reservation (Confirm/Refund) once the async job reaches a terminal
	// status -- see internal/smilesim/service.go's own "Credit accounting"
	// and "Settlement reachability" package doc sections.
	//
	// smileSimReservationStore shares this app's own db connection (like
	// every other module above) and gets its own tiny table created
	// imperatively via EnsureSchema, mirroring go/jobs.StandaloneQueue's
	// own "create the persistence schema if it does not already exist"
	// pattern rather than joining migrationRegistry -- see
	// reservation_store.go's own doc comment for why. standaloneQueue is
	// passed to NewService too, alongside the store: it is what
	// ReconcileOutstandingCredits polls for each outstanding reservation's
	// job status, and StartReconciler below wraps that sweep in a ticker
	// loop this app starts once at boot (smileSimReconcilerStop, stopped
	// first thing in cleanup above), so a reservation nobody ever polls
	// to completion -- or one whose settlement was in flight when this
	// process last restarted -- still gets settled rather than staying
	// Reserved forever.
	smileSimReservationStore := smilesim.NewReservationStore(db)
	if err := smileSimReservationStore.EnsureSchema(ctx); err != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: ensure smilesim credit reservation schema: %w", err)
	}
	// smileSimulationStore is the per-photo result index (see
	// internal/smilesim's package doc comment's "Per-photo result index"
	// section): each generation request's photo, effective options and job
	// id land in this app's own SQLite the moment its enqueue succeeds,
	// surviving a restart exactly like the job row they point at, so the
	// per-photo enumeration route (smilesim.go) has its data
	// source. Same
	// EnsureSchema-before-first-use shape as the reservation store above.
	smileSimulationStore := smilesim.NewSimulationStore(db)
	if err := smileSimulationStore.EnsureSchema(ctx); err != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: ensure smilesim simulation index schema: %w", err)
	}
	// The attestation layer's two boot steps run here, after Bootstrap (the
	// pki migrations the CA chain's pki_authorities rows live in were
	// applied there) and before any request can reach the surfaces that
	// attest or gate: EnsureSchema creates the app table, and
	// EnsureAuthorityChain creates -- once per database, idempotently, by
	// the chain's fixed subject names -- the app's root and issuing
	// intermediate authorities and records the intermediate as the issuing
	// authority every tenant's simulation-attestation certificate is
	// signed under. A failure stops the boot: an app whose AI outputs
	// cannot be attested must not start serving shares of them.
	if err := attestationService.EnsureSchema(ctx); err != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: ensure attestation schema: %w", err)
	}
	if err := attestationService.EnsureAuthorityChain(ctx); err != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: ensure the attestation CA chain: %w", err)
	}
	// The last argument is gatewayEntitlements -- the same adapter instance
	// aiGatewayModule's WithEntitlements gate runs -- so Simulate can
	// pre-flight the model-access gate before its credit reservation opens
	// (see that binding's own comment above and internal/smilesim's
	// Simulate doc comment).
	smileSimService := smilesim.NewService(aiGatewayModule.Gateway(), billingModule.Credits(), reg.EventBus(), standaloneQueue, smileSimReservationStore, smileSimulationStore, gatewayEntitlements)
	// context.Background(), never ctx, per StartReconciler's own doc
	// comment: the sweep must keep running until cleanup's own
	// smileSimReconcilerStop call, not be cut short by whatever cancels
	// BuildServer's own ctx.
	smileSimReconcilerStop = smileSimService.StartReconciler(context.Background(), 0)
	// memberships rides along as the recipient gate's membership answer --
	// the SAME store authn's MembershipReader reads, attached to org above
	// (see wireSmileSim's own doc comment and smilesim.go's
	// validateSimulateRecipient).
	// storageModule.ObjectService() is the fourth wireSmileSim argument:
	// the simulation-content route reads a generated image's stored bytes
	// through the same instance the cases photo routes drive (see
	// wireSmileSim's own doc comment).
	wireSmileSim(mux, smileSimService, standaloneQueue, memberships, storageModule.ObjectService(), attestationService)

	// WireClinicName mounts this host's own tenant-identity answer
	// (clinic_name.go): the org root name of the tenant the
	// caller's token is scoped to, the name the web renders for a clinic
	// that did not exist at boot (self-service registration provisions
	// it, naming the root after the registrant's display name) where the
	// demo roster's static copy has no entry. Mounted here among the
	// other hand-written app routes, behind the same chain every
	// authenticated route sits behind.
	WireClinicName(mux, orgModule.Tree())

	// wireTeamMembers mounts this host's own roster-with-identity answer
	// (team_members.go): the tenant's org membership roster,
	// each row enriched with the member's display identity from authn's
	// users table -- the composition the Team surface's "who works in
	// this clinic" promise needs, since org's member rows carry opaque
	// user ids only by its own module-boundary rule (team_members.go's
	// package doc has the full argument). The identity source is authn's
	// user row behind every member -- demo seeds and self-registered
	// clinics' invitees alike -- never the demo layer's own roster.
	// Mounted here among the other hand-written app routes, behind the
	// same chain every authenticated route sits behind, gated on the org
	// read permission through the same rbac gate the org module route
	// uses. The call cannot fail: nothing it does returns an error.
	wireTeamMembers(mux, teamMembersDeps{
		az:             rbacService,
		members:        orgModule.Members(),
		tree:           orgModule.Tree(),
		users:          authnModule.Service().Users(),
		headerDisabled: cfg.DisableDemoUserHeader,
	})

	// wireCasesRoutes mounts the case domain
	// (internal/cases, mounted in cases.go): the tenant-scoped
	// Case records a web UI renders from, each grouping a patient
	// (the clinic-given name/reference, embedded in the row) with the
	// photos of the case. caseRepository shares this app's own db
	// connection (like every store above) and gets its two tiny tables
	// created imperatively via EnsureSchema -- the same CREATE TABLE IF
	// NOT EXISTS pattern smileSimulationStore's own EnsureSchema call
	// right above uses (internal/cases's package doc comment's "House
	// discipline" section gives the same reasons for not joining
	// migrationRegistry). The service it backs is deliberately free of
	// the simulation layer: a case detail's per-photo simulations stay on
	// the enumeration route wireSmileSim just mounted (GET
	// /api/v1/smile-simulation/photos/{photoObjectID}/simulations), which
	// a per-photo view fetches rather than the case endpoint joining
	// them (see that package doc comment's "Shape decision" section). The
	// creator-attribution seam is DemoNotesSubjectResolver, the same host
	// type notes' module uses -- it satisfies the cases package's
	// identical copy of the SubjectResolver declaration,
	// compile-time-checked at the bottom of cases.go.
	caseRepository := cases.NewRepository(db)
	if err := caseRepository.EnsureSchema(ctx); err != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: ensure cases schema: %w", err)
	}
	// The photo-upload and photo-content routes (cases_photos.go) drive
	// storageModule's own ObjectService -- the same instance the storage
	// module's HTTP surface serves -- so the app's case surface and the
	// module agree on what an object is and which tenant's rows each read
	// (go/storage resolves the tenant from the request context itself).
	wireCasesRoutes(mux, cases.NewService(caseRepository), DemoNotesSubjectResolver{HeaderDisabled: cfg.DisableDemoUserHeader}, storageModule.ObjectService())

	// wireIntegrationAuthenticated mounts go/integration's
	// mandatory-first-consumer route (integration_authenticate.go):
	// a minimal "whoami" demo endpoint gated by the module's own
	// AuthMiddleware -- a key authenticates, a rotated-away key is refused,
	// a revoked key is refused -- through this app's own real, composed
	// HTTP stack, with the guard wired ahead of it through
	// integration.WithAuthenticationGuard, applied
	// inside wireIntegrationAuthenticated once reg.KVStore() exists, since
	// the guard's limiter is built over this same resolved KVStore seam --
	// so AuthMiddleware runs it BEFORE authentication and a forged-X-API-Key
	// flood pays the guard's budget instead of reaching
	// Service.Authenticate's lookups unbounded (flowtests/apikey_authenticate_flow_test.go
	// pins the order). The call cannot fail: nothing
	// it does returns an error.
	wireIntegrationAuthenticated(mux, integrationModule, reg.KVStore())

	// The middleware chain: authn.Middleware(verifier) FIRST, then
	// tenancy.Middleware(authn.NewPrincipalResolver()). Running authn
	// first verifies the token exactly once: a tenancy.Resolver's signature
	// (Resolve(*http.Request)
	// (pkgcore.TenantID, error)) cannot hand a verified JWT's claims to
	// anything downstream, so running tenancy first would force verifying
	// every token twice over two code paths free to drift. Running
	// authn.Middleware first verifies once; NewPrincipalResolver then just
	// reads the already-verified Principal out of the request context.
	//
	// Session revocation rides on the same call with no extra option: this
	// app selects immediate revocation in authnOpts above
	// (WithRevocationMode(RevocationModeImmediate)), the Service attaches
	// its SessionManager as the revocation source of the verifier
	// Service().Verifier() hands out, and authn.Middleware consults that
	// source on every request whose token verifies -- so the plain
	// Middleware(verifier) call below is exactly the enforced composition,
	// not a silently unenforced one: drop the option and the revocation
	// source is a stored list nothing consults.
	//
	// The consequence that matters here: authn.Middleware is OPTIONAL
	// auth (a missing token proceeds with no Principal; an invalid one
	// 401s immediately), so tenancy.Middleware's own fail-closed default
	// -- refuse a request whose (method, path) is not on the allowlist AND
	// whose resolver failed -- is what makes EVERY route this app mounts
	// require a valid Principal by default, with NO extra wrapping needed
	// per route: an unauthenticated request to the notes API gets 403
	// (tenant unresolved, because there is no Principal to read a tenant
	// from), failing closed like every other unresolvable request. The routes
	// listed in the allowlist below are the ONLY ones that work with no
	// Principal at all -- this chain never even sees authn's own subtree,
	// which topMux dispatches straight from authn.Middleware's output the
	// way it does admin's (hostcore.AuthnAPIPath's own doc comment has the
	// why): healthz and metrics (their constants' doc comments above),
	// config's two pre-auth display endpoints (still gated by their own
	// internal DomainResolver, see configModule's wiring above -- entirely
	// independent of this outer middleware), and the three routes that
	// resolve their own tenant server-side once this middleware lets them
	// through, sharing.PathAccess, IntegrationWhoamiPath and orgAcceptPath,
	// each with its own entry comment right below.
	//
	// Both GET and HEAD are allowlisted for healthz/metrics, not GET
	// alone: net/http's ServeMux automatically serves HEAD from a
	// registered "GET "+path pattern (Go's long-standing GET-implies-HEAD
	// convenience), but tenancy.Middleware does NOT extend WithAllowlist's
	// exemption the same way -- its own doc comment says so explicitly.
	// Allowlisting GET alone would leave HEAD one middleware change away
	// from a 403 the moment anything probes it with HEAD instead of GET.
	// admin.ImpersonationMiddleware sits between authn.Middleware and
	// tenancy.Middleware (see pipeline.go's own doc comment): it never
	// reorders this chain, it reads the real, already-verified
	// authn.Principal authn.Middleware just installed, and -- only when
	// the request carries a valid X-Admin-Impersonation grant id -- it
	// substitutes a Principal naming the impersonation target for
	// everything downstream, including tenancy.Middleware's own tenant
	// resolution. A request with no such header, or an invalid one, is
	// unaffected: this decorator is a no-op for every route notes/org/
	// storage/etc. serve unless an operator has actually started an
	// impersonation session.
	//
	// admin's OWN mounted route is deliberately excluded from that branch
	// entirely -- topMux below dispatches it straight from
	// authn.Middleware's own output, through guardAdminRoute's
	// adminSubjectResolver (demo_admin.go) and nothing else -- what
	// mountModuleRoutes' own doc comment explains: admin's five
	// permissions are evaluated in rbac.SystemDomain against the CALLER'S
	// OWN real, unsubstituted Principal, regardless of whichever tenant
	// their session happens to be currently scoped to and regardless of
	// any impersonation grant that may be active on the request -- neither
	// tenancy.Middleware's tenant resolution nor
	// admin.ImpersonationMiddleware's identity substitution has anything
	// to contribute to that decision, and letting either run first was a
	// real privilege-escalation gap found in review (an ordinary tenant's
	// own Owner role, or an impersonated identity, could otherwise reach
	// admin's console purely because rbac.BuiltinRoleOwner and the shared
	// global permission catalog carry no domain partitioning of their
	// own).
	//
	// authn's OWN mounted route is deliberately excluded from this branch
	// for its own structural reason: authn's whole HTTP surface must never
	// sit downstream of tenancy.Middleware (its routes resolve the tenant
	// from the Principal's own claim, per operation; go/authn/AGENTS.md's
	// "authn's own routes never sit downstream of tenancy.Middleware"
	// section), and its pre-auth half -- enterprise OIDC's dynamically
	// named "oidc:<tenant>" login-start path included -- cannot be
	// expressed by tenancy.WithAllowlist's exact (method, path) matching at
	// all (see hostcore.AuthnAPIPath's own doc comment). topMux therefore
	// dispatches hostcore.AuthnAPIPath straight from authn.Middleware's own
	// output, the same
	// shape adminRoutePath gets above -- with the deliberate difference
	// that authn's branch is UNGATED: it sits behind authn.Middleware's
	// optional verification and nothing else, because authn's Handler
	// itself is the per-operation authority on who may call what
	// (requirePrincipal), and any rbac gate ahead of it would refuse the
	// sign-in flow this app exists to demonstrate. Neither
	// tenancy.Middleware's tenant resolution nor
	// admin.ImpersonationMiddleware's identity substitution runs on this
	// branch: authn operations never read a middleware-injected tenant, and
	// an impersonation grant must never substitute an authn operation's
	// caller identity -- authn is the layer that MINTED the identities
	// impersonation substitutes between.
	restOfAppChain := admin.ImpersonationMiddleware(adminModule.Impersonation())(
		tenancy.Middleware(authn.NewPrincipalResolver(), append(hostcore.PreAuthAllowlist(),
			// tenancy.WithTenantStatusResolver is D4's enforcement seam
			// (docs/internal/23-admin.md, go/tenancy/tenant_status.go):
			// admin's own tenant ledger (*admin.TenantService, D3)
			// implements tenancy.TenantStatusResolver structurally --
			// admin is its one real implementer, but the interface itself
			// does not know admin exists, the identical no-import-in-
			// either-direction shape org.FeatureGate/rbac.SubtreeResolver
			// already use. This is what turns "an operator marked a
			// tenant suspended in admin's console" into every OTHER
			// route (notes, storage, org, ...) actually refusing that
			// tenant's requests on the very next one, rather than being
			// a ledger fact nothing downstream ever consults.
			tenancy.WithTenantStatusResolver(adminModule.Tenants()),
			// sharing.PathAccess is the one genuinely public, unauthenticated
			// route this app mounts: an anonymous visitor holding a bearer
			// share token carries no Principal and therefore no tenant claim
			// at all, by design (go/sharing's Handler doc comment) --
			// sharing.Service.AccessPublic resolves the tenant itself, from
			// the token alone, once this allowlist entry lets the request
			// reach it at all. GET only: the fragment defines no other
			// method on this path.
			tenancy.WithAllowlist(http.MethodGet, sharing.PathAccess),
			// IntegrationWhoamiPath is the identical shape, one layer
			// removed: its caller carries an API key, not a share token, and
			// like sharing.PathAccess it resolves ITS OWN tenant --
			// integration.AuthMiddleware, mounted ahead of this outer chain's
			// tenancy.Middleware reaching this route at all, via
			// Service.Authenticate (integration_authenticate.go).
			// Without this allowlist entry, tenancy.Middleware would refuse
			// every request here with tenancy.tenant_unresolved before
			// AuthMiddleware ever got the chance to resolve one from the
			// presented key.
			tenancy.WithAllowlist(http.MethodGet, IntegrationWhoamiPath),
			// orgAcceptPath (org_acceptInvitation) is the one org route this
			// app lets through tenant resolution, for the identical reason
			// sharing.PathAccess gets its entry: the caller an invitation
			// exists for -- a freshly invited person -- holds no membership
			// in, and typically no bearer token for, the inviting tenant, so
			// their request carries no tenant claim for tenancy.Middleware
			// to resolve. org's accept handler resolves the tenant itself,
			// server-side, from the invitation token (InviteService.Accept,
			// through go/org's narrow org_invitation_token_index -- the
			// sharing-model mechanism sharing's own public path uses), once
			// this allowlist entry lets the request reach it at all; without
			// the entry, tenancy.Middleware would refuse every accept with
			// tenancy.tenant_unresolved before org's handler ever saw the
			// token. POST only: the fragment defines no other method on this
			// path. Unlike sharing.PathAccess this is NOT an anonymous
			// surface: org's own per-operation SubjectResolver check still
			// refuses an unidentifiable acceptor with org.subject_unresolved,
			// and authn.Middleware still 401s a genuinely invalid bearer.
			tenancy.WithAllowlist(http.MethodPost, orgAcceptPath),
		)...)(mux),
	)

	topMux := http.NewServeMux()
	topMux.Handle(adminRoutePath, adminHandler)
	topMux.Handle(adminRoutePath+"/", adminHandler)
	topMux.Handle(hostcore.AuthnAPIPath, authnHandler)
	topMux.Handle(hostcore.AuthnAPIPath+"/", authnHandler)
	topMux.Handle("/", restOfAppChain)

	handler := authn.Middleware(authnModule.Service().Verifier())(topMux)
	// The demo-user seeds run last, once the composed handler exists: they
	// register the demo accounts through the same register route a browser
	// would use, which needs the whole chain above it. Each seed is opt-in
	// under its OWN variable (APP_DEMO_USERS_PASSWORD for the three
	// customer-tenant demo accounts of demo_users.go,
	// APP_DEMO_PLATFORM_STAFF_PASSWORD for the rbac.SystemDomain platform
	// administrator of demo_admin.go) and runs independently of the other:
	// the platform administrator must never be seeded from the ordinary
	// demo users' password variable -- see
	// the DemoPlatformStaffPassword field's own doc comment (bootstrap.go) for
	// why. An empty
	// variable skips the seed.
	if cfg.DemoUsersPassword != "" {
		if seedErr := seedDemoUsers(ctx, handler, authnModule.Service(), rbacService, orgModule, cfg.HostTenants, cfg.DemoUsersPassword); seedErr != nil {
			_ = cleanup()
			return nil, nil, nil, seedErr
		}
	}
	// seedDemoPlatformStaff is admin's own first-consumer demo account
	// (demo_admin.go): a real registered user whose ONLY membership is
	// rbac.SystemDomain, holding BuiltinRoleOwner there -- every
	// admin:* permission included, since owner carries every
	// permission any module declared.
	if cfg.DemoPlatformStaffPassword != "" {
		if _, seedErr := seedDemoPlatformStaff(ctx, handler, memberships, rbacService, authnModule.Service(), cfg.DemoPlatformStaffPassword); seedErr != nil {
			_ = cleanup()
			return nil, nil, nil, seedErr
		}
	}

	// The self-service signup chain (self_service.go) installs AFTER both
	// demo seeds, which is the ordering that keeps the demo path intact:
	// the seeds' registrations run above, before the provisioner's
	// subscription exists, so the demo accounts provision no clinic of
	// their own and keep exactly the memberships and grants the seeds
	// give them; every registration that reaches the composed handler
	// from here on -- a browser's, or a flow test's -- is a self-service
	// registration and gets its own clinic tenant, org root, membership,
	// owner grant, demo-plan subscription and credit seed before its 201
	// answer leaves (on the in-process bus, where the subscription's
	// provisioning runs synchronously inside the register request
	// itself). The clinic's org root is named after the registrant's own
	// authn display name (read through the authn service handed in here);
	// the three billing services ride along as the subscription and
	// credit half of the provisioned clinic -- the same PlanService,
	// SubscriptionService and CreditService the two demo seeds above just
	// used, so a clinic's subscription and starting balance are the demo
	// tenant's own (provision's own doc comment in self_service.go) --
	// and the standaloneQueue rides along for the failure half of the
	// guarantee: a synchronous provisioning attempt that fails enqueues
	// the retry job that converges the clinic (see provision's own doc
	// comment in self_service.go), and the queue's worker was started
	// above, so the retry runs on this same process's pool.
	// cfg.FailSelfServiceProvision rides along as the failure-injection
	// hook -- nil under the disabled default (APP_FAIL_SELF_SERVICE_PROVISION
	// absent or 0), armed either by ConfigFromEnv's own env-driven parse or
	// by a test's ServerConfig.
	if wireErr := wireSelfService(ctx, reg, orgModule, rbacService, authnModule.Service(), billingModule.Plans(), billingModule.Subscriptions(), billingModule.Credits(), standaloneQueue, cfg.FailSelfServiceProvision); wireErr != nil {
		_ = cleanup()
		return nil, nil, nil, wireErr
	}

	// Serve the built frontend when this boot is configured with one
	// (cfg.WebDistDir, set by ConfigFromEnv from APP_WEB_DIST -- the
	// Dockerfile ships the dist and sets the variable itself). The wrap
	// goes around the OUTSIDE of authn.Middleware's own output above, so
	// the requests the frontend answers -- "/" itself, /assets/* and
	// unknown non-API paths -- never fall into the authn/tenancy
	// middleware chain at all: GET / answers the app's index.html to a
	// caller with no tenant, which is exactly what a deployed sign-in page
	// needs, while every request the frontend does not answer (the whole
	// /api surface, /healthz, /metrics, and any non-GET/HEAD method)
	// reaches the composed chain unchanged -- see
	// frontend.go's package doc comment for this app's wiring and
	// pkgcore/spa's for the full interception rules.
	if cfg.WebDistDir != "" {
		handler = withFrontend(cfg.WebDistDir, handler)
	}
	return handler, cleanup, complianceModule, nil
}

// mountModuleRoutes copies every route reg's modules mounted onto mux,
// with TWO deliberate exceptions, each mounted by BuildServer on its own
// topMux branch directly behind authn.Middleware and nothing else -- see
// BuildServer's own composition comment -- and each returned here instead
// of mounted into mux:
//
//   - admin's own mounted route (adminRoutePath): admin's HTTP surface
//     must not sit behind ordinary tenancy.Middleware tenant resolution,
//     and must not sit behind admin.ImpersonationMiddleware's identity
//     substitution either -- go/admin/AGENTS.md's wiring-contract section
//     states both explicitly ("admin's OWN routes ... do not sit behind
//     ImpersonationMiddleware -- that decorator's effect is on the REST of
//     the application's routes only"; "does NOT go through ordinary
//     tenancy.Middleware tenant resolution").
//   - authn's own mounted route (hostcore.AuthnAPIPath): authn's HTTP
//     surface never
//     sits downstream of tenancy.Middleware, per the module's own
//     architecture (go/authn's routes resolve the tenant from the
//     Principal's own claim, per operation, never from a tenant a
//     middleware guessed). Composing it that way is also the only shape in
//     which the enterprise-OIDC login-start path can work at all: its
//     provider value is the dynamic "oidc:<tenant>" string a per-tenant
//     literal allowlist entry (tenancy.WithAllowlist's exact-match form)
//     cannot enumerate, and the path carries no Principal (it is the FIRST
//     step of a sign-in), so a tenancy.Middleware in front of it refused
//     every such request with tenancy.tenant_unresolved before authn's own
//     OIDC logic ever saw it -- the CONFIRMED GAP this composition closes
//     (see hostcore.AuthnAPIPath's own doc comment). authn.Middleware still
//     runs outside everything (it wraps topMux itself), so a genuinely
//     invalid bearer still 401s before this branch is reached; what is
//     gone is only the tenancy layer, whose fail-closed default had no
//     business deciding authn's own per-operation pre-auth question --
//     authn's Handler itself decides, operation by operation, whether a
//     Principal is required (go/authn/handler.go's requirePrincipal), the
//     same self-gating demoRouteGuards' routePublic entry for this path
//     already records. authn's allowlist entries are therefore gone from
//     the tenancy chain below, and its handlers receive no tenant context
//     from tenancy.Middleware -- they never read one (every authn
//     operation derives what it needs from the verified Principal's own
//     claims).
//
// net/http's ServeMux (since Go 1.22) distinguishes an exact-match pattern
// ("/api/v1/notes") from a subtree pattern ("/api/v1/notes/", matching
// everything below it): registering only the subtree pattern would make
// ServeMux redirect a bare request for the exact path with an HTTP
// redirect instead of serving it directly -- which would silently break a
// POST, since a redirect is not guaranteed to preserve the method or body
// across every client. pkgcore.MountedRoute's own doc comment says the
// Handler "serves every request below Path", meaning it must be reachable
// at Path itself AND at everything nested below it; the dual registration
// that satisfies that contract is the shared kernel's, so every plain
// route below mounts through hostcore.MountRoute (see its doc comment).
//
// Every route also passes through GuardModuleRoute on the way out, which
// is where rbac's permission gate is applied -- see demo_subject.go's
// demoRouteGuards. GuardModuleRoute still runs for the two excepted paths
// too (admin's dispatching to guardAdminRoute per its adminRouteSentinel
// entry, authn's resolving routePublic per its own entry), so the table's
// exhaustiveness check keeps covering both; only the DESTINATION of the
// resulting handler differs. A path the table does not name fails the
// build here rather than being served.
func mountModuleRoutes(mux *http.ServeMux, reg *pkgcore.Registry, az rbac.Authorizer, orgDeps OrgRouteGuardDeps, demoHeaderDisabled bool) (adminHandler http.Handler, authnHandler http.Handler, err error) {
	for _, route := range reg.Routes.Routes() {
		handler, guardErr := GuardModuleRoute(az, route.Path, route.Handler, orgDeps, demoHeaderDisabled)
		if guardErr != nil {
			return nil, nil, guardErr
		}
		if route.Path == adminRoutePath {
			adminHandler = handler
			continue
		}
		if route.Path == hostcore.AuthnAPIPath {
			authnHandler = handler
			continue
		}
		hostcore.MountRoute(mux, route.Path, handler)
	}
	if adminHandler == nil {
		return nil, nil, fmt.Errorf("reference-app: no module mounted %q; admin.Module.Register must run for this app to compose its dedicated middleware branch", adminRoutePath)
	}
	if authnHandler == nil {
		return nil, nil, fmt.Errorf("reference-app: no module mounted %q; authn.Module.Register must run for this app to compose its dedicated middleware branch", hostcore.AuthnAPIPath)
	}
	return adminHandler, authnHandler, nil
}
