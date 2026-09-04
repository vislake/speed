package notes

import (
	"embed"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/examples/reference-app/internal/notes/locales"
	"github.com/vislake/speed/examples/reference-app/internal/notes/migrations"
)

//go:embed api/openapi.yaml
var openAPISpecYAML []byte

const (
	// moduleName is notes' pkgcore.Module.Name(). It is also the module
	// name dbkit.MigrationRegistry.Register keys its dependency graph on.
	moduleName = "notes"

	// apiPath is the path this module's single route is mounted at (see
	// Register below). It must agree with the path declared in this
	// module's own OpenAPI fragment (api/openapi.yaml), which is the
	// module-asset convention of docs/internal/21-api-contract.md: the
	// fragment's "paths:" keys are what oapi-codegen turns into the
	// method+path patterns of the generated registration helpers (see
	// api/notes-server.gen.go's HandlerWithOptions) and into the
	// api.ServerInterface method set Handler implements. A request can
	// only reach Handler through a route mounted at apiPath, and Handler's
	// generated inner router only serves the fragment's own paths, so
	// apiPath and the fragment must name the same path -- changing one
	// without the other leaves the endpoint dead, and only a test that
	// exercises the composed stack (cmd/server's end-to-end suite) sees
	// it.
	apiPath = "/api/v1/notes"

	// PermissionRead and PermissionWrite are notes' resource:action
	// permission strings (backend coding standard §2's Register example).
	//
	// They are really enforced. Declaring them here is what puts them in
	// the permission catalog go/rbac freezes after Bootstrap, and cmd/server
	// wraps this module's route in rbac's permission gate: a GET of
	// /api/v1/notes requires PermissionRead and anything that writes
	// requires PermissionWrite (see cmd/server/demo_subject.go, which
	// derives the resource half from these very constants rather than
	// retyping it).
	//
	// Nothing in THIS package checks them, and that is the design rather
	// than a gap: a business module declares its permission vocabulary and
	// the authorization engine enforces it at the edge, so notes needs no
	// dependency on rbac at all. What notes does not yet do is row-level
	// filtering by organization subtree (rbac.Service.DataScope), which
	// needs an organization tree this app has none of.
	PermissionRead  = "notes:read"
	PermissionWrite = "notes:write"

	// EventNoteCreated is the domain event type published whenever a note
	// is created, following the "<module>.<entity>.<action>" convention
	// (backend coding standard §8; go/pkgcore/registry.go's
	// EventDecl.Type doc comment). Handler.NotesCreateNote is the one
	// place that actually calls EventBus.Publish for it -- see handler.go.
	EventNoteCreated = "notes.note.created"

	// eventNoteCreatedPayloadType names NoteCreatedPayload for
	// EventDecl.PayloadType, so a subscriber (and the future event
	// catalog) knows what concrete type to expect in Event.Payload
	// without importing this package just to read a string.
	eventNoteCreatedPayloadType = "notes.NoteCreatedPayload"

	// AuditActionNoteCreate is notes' audit action string. It uses the
	// present-tense verb form docs/internal/10-compliance-and-audit.md's
	// own AuditEvent.Action examples use ("org.member.remove",
	// "billing.plan.change"), deliberately distinct from
	// EventNoteCreated's past-tense fact ("created"): the audit trail
	// records what operation was performed, the event records what
	// became true as a result -- two different questions that happen to
	// share a module and entity name.
	AuditActionNoteCreate = "notes.note.create"

	// Configuration keys, feature flags and their default values below.
	//
	// A word on ownership before the declarations: notes is this app's
	// only business module so far, yet the items it registers here are
	// platform-grade keys (the brand, the support address, the AI
	// feature toggles). That is a deliberate placeholder arrangement, not
	// the shape a finished app ships: root CLAUDE.md's "The reference app
	// is the mandatory first consumer of every module" rule means SOME
	// module must be the first consumer of the config module's schema
	// registration, and notes is the only candidate -- so it registers
	// the keys this app's own frontend and support flows need, and
	// documents the temporary custody in this comment. When the real
	// owner modules land (branding in a platform/tenant module, the
	// support address in notification, the AI toggles in ai-gateway),
	// these registrations move with their keys, and the notes module
	// shrinks back to its own CRUD schema.
	//
	// The values and validation shape follow pkgcore.ConfigItem's and
	// pkgcore.FeatureFlag's doc comments; config's Attach folds them into
	// its runtime schema (go/config/module.go's Attach doc comment) and
	// its unit and integration tiers pin the same semantics against
	// fixtures that mirror these declarations.
)

// ConfigKeyBrandSiteName is the public, tenant-overridable display name a
// tenant's own frontend shows (the "brand" of the white-label rule in
// docs/internal/11-cross-cutting.md's dynamic-config section). It is
// Public so the unauthenticated /api/config/public endpoint may serve it,
// and it defaults to the app's own name.
const ConfigKeyBrandSiteName = "brand.site_name"

// ConfigKeySupportReplyEmail is the address support mail is sent from
// (and shown as the contact address where support is offered). It is
// Sensitive: a mail configuration can embed account identity, it must lie
// encrypted at rest, and it must never reach the public endpoint.
const ConfigKeySupportReplyEmail = "support.reply_email"

// FeatureFlagSmilePreview gates the smile-preview feature; when a tenant
// enables it, the frontend offers previews before a full simulation. The
// second flag depends on it (see ai.premium_upsell's DependsOn below), so
// the two together exercise the flag-dependency chain of config's
// feature-flag runtime.
const FeatureFlagSmilePreview = "ai.smile_preview"

// FeatureFlagPremiumUpsell gates the premium upsell UI. It DependsOn
// ai.smile_preview: showing a premium offer for a feature the tenant has
// not even enabled is noise, so the flag only reads enabled while its
// dependency is on (config resolves DependsOn chains on every read).
const FeatureFlagPremiumUpsell = "ai.premium_upsell"

// NoteCreatedPayload is the concrete type carried in the
// pkgcore.Event.Payload of every EventNoteCreated event -- the payload
// shape eventNoteCreatedPayloadType names for EventDecl.PayloadType.
type NoteCreatedPayload struct {
	// NoteID is the created note's ID (Note.ID).
	NoteID string

	// TenantID is the owning tenant (pkgcore.TenantID), carried as a plain
	// string since the payload is a wire-shaped event type, not a
	// pkgcore-typed one.
	TenantID string

	// CreatorUserID is the creating user's id, resolved by the host's
	// SubjectResolver seam from the create request (see handler.go's
	// resolveSubject). Subscribers route on it: the notification module's
	// UserAddressResolver consults the same user id at dispatch time, so a
	// note-created fact with an empty CreatorUserID could never reach a
	// recipient -- which is exactly why NotesCreateNote refuses an
	// unresolvable request with ErrSubjectUnresolved before creating
	// anything, rather than publishing a creator-less fact.
	CreatorUserID string
}

// Module implements pkgcore.Module for notes, examples/reference-app's
// placeholder tenant-scoped business resource (see this package's doc.go).
// It is deliberately generic, non-dental business content, standing in for
// the real modules that land in later milestones.
type Module struct {
	repo    *Repository
	handler *Handler
	subject SubjectResolver
}

// options carries NewModule's optional wiring. notes keeps exactly one
// option so far -- the creator resolver -- and the struct exists so adding
// another never breaks existing call sites (the option pattern every
// module in this workspace uses).
type options struct {
	subject SubjectResolver
}

// Option configures a Module constructed by NewModule.
type Option func(*options)

// WithSubjectResolver wires the seam NotesCreateNote reads the creating
// user's id from (see handler.go's SubjectResolver). Unwired -- the
// default -- every create request fails closed with ErrSubjectUnresolved,
// never producing a creator-less note-created event.
func WithSubjectResolver(r SubjectResolver) Option {
	return func(o *options) { o.subject = r }
}

// noteCreatedNotificationType is EventNoteCreated's entry in the
// notification preference matrix (pkgcore.NotificationType, registered by
// Register below). Declaring it makes the type visible to the module that
// renders the recipient-facing preference UI -- the notification module --
// and marks it Unsubscribable: true, the "recipient may opt out of this
// kind of notification" choice (contrast demo.patient_reminder, the
// reference app's unsubscribable-false type; see cmd/server's wiring).
//
// The channel strings are the notification module's own channel vocabulary
// ("in_app", "email", "sms" -- notification.ChannelInApp and siblings):
// notes deliberately does not import notification -- business modules
// publish domain facts, notification subscribes, and the dependency never
// points the other way (backend coding standard §8) -- so the vocabulary
// is written out here against that module's contract, exactly as org
// writes out authn's event name rather than importing authn. notification
// ships the channel constants and pins them in its own tests; a rename
// there is caught when this registration stops matching the preference
// matrix it renders into.
var noteCreatedNotificationType = pkgcore.NotificationType{
	Key:             EventNoteCreated,
	Group:           "collaboration",
	DefaultChannels: []string{"in_app", "email", "sms"},
	Unsubscribable:  true,
}

// NewModule returns a Module whose repository is backed by db. db is
// expected to come from dbkit.Open; constructing a Module performs no I/O
// of its own -- opening and migrating db is the caller's responsibility,
// done once at startup before Bootstrap ever calls Register (see
// cmd/server's wiring for the exact sequence, and Register's own doc
// comment below for why Register itself still must not touch the
// database).
func NewModule(db *gorm.DB, opts ...Option) *Module {
	o := options{}
	for _, opt := range opts {
		opt(&o)
	}
	return &Module{repo: NewRepository(db), subject: o.subject}
}

// Name implements pkgcore.Module.
func (m *Module) Name() string { return moduleName }

// DependsOn implements pkgcore.Module. notes depends on infrastructure
// (dbkit, tenancy) only, never on another business module -- and there is
// no other business module yet for it to depend on (root CLAUDE.md's M0
// status) -- so this is genuinely empty, not an aspirational placeholder.
func (m *Module) DependsOn() []string { return nil }

// Migrations implements pkgcore.Module.
func (m *Module) Migrations() embed.FS { return migrations.FS }

// Locales implements pkgcore.Module.
func (m *Module) Locales() embed.FS { return locales.FS }

// OpenAPISpec implements pkgcore.Module: it returns the module's own
// OpenAPI fragment, embedded from api/openapi.yaml. That fragment is the
// single source of this module's API surface -- the api package's
// generated types and ServerInterface (api/notes-server.gen.go, regenerated
// by task api:gen) derive from it, and Handler implements that interface
// (see handler.go) -- per docs/internal/21-api-contract.md's spec-first
// decision.
func (m *Module) OpenAPISpec() []byte { return openAPISpecYAML }

// Register implements pkgcore.Module. Per the interface's own doc comment
// ("It must not perform I/O; it only declares"), this method never touches
// the database or the network: constructing m.handler wires together
// already-built, in-memory values (m.repo, m.subject -- wired by NewModule's
// option, or nil for NotesCreateNote to refuse on, see
// ErrSubjectUnresolved -- and the EventBus reference reg.EventBus()
// returns); obtaining that reference is not itself an I/O operation, it is
// just reading a field off reg. The bus is not actually called (no
// Publish) until a real HTTP request creates a note; see handler.go's
// NotesCreateNote method.
func (m *Module) Register(reg *pkgcore.Registry) error {
	if err := reg.AuditActions.Add(AuditActionNoteCreate); err != nil {
		return err
	}

	// reg.AuditActions is handed to NewHandler so NotesCreateNote can call
	// audit.Emit against the exact AuditActionRegistrar AuditActionNoteCreate
	// was just declared on -- Emit itself validates the action string against
	// it before publishing (see audit.Emit's own doc comment), which is what
	// requires the declaration above to run before this line, not after it.
	m.handler = NewHandler(m.repo, reg.EventBus(), reg.AuditActions, m.subject)
	reg.Routes.Mount(apiPath, m.handler)

	if err := reg.Permissions.Add(PermissionRead, PermissionWrite); err != nil {
		return err
	}
	if err := reg.Events.Publishes(pkgcore.EventDecl{
		Type:        EventNoteCreated,
		PayloadType: eventNoteCreatedPayloadType,
		Description: "Published whenever a new note is created for a tenant.",
	}); err != nil {
		return err
	}
	// The notification-type declaration rides along with the event it
	// describes: publishing notes.note.created as a fact and declaring it as
	// a notification kind are two views of the same domain action, so they
	// live in the same Register call, keyed by the same constant
	// (EventNoteCreated). m.subject may be nil here -- a module without a
	// creator resolver still declares its type, and its create endpoint
	// then fails closed (see ErrSubjectUnresolved) rather than this
	// declaration being conditional on wiring.
	if err := reg.Notifications.Add(noteCreatedNotificationType); err != nil {
		return err
	}

	// The configuration schema and feature flags this module owns (see
	// the ownership note above the key constants). Registering them here
	// -- rather than letting config's own module invent defaults -- is
	// the point of the whole split: config provides the mechanism (the
	// table, the scope hierarchy, the validation, the endpoints), while
	// every value's meaning and default belongs to the module that owns
	// the key. pkgcore validates the declarations on Add (a malformed
	// item or a duplicated key fails registration), and config.Attach
	// freezes them into the runtime schema after Bootstrap.
	if err := reg.Config.Add(
		pkgcore.ConfigItem{
			Key:         ConfigKeyBrandSiteName,
			Type:        "string",
			Default:     "Smile Studio",
			Public:      true,
			Description: "The tenant's display name, shown on its own pages and emails.",
			Group:       "brand",
		},
		pkgcore.ConfigItem{
			Key:         ConfigKeySupportReplyEmail,
			Type:        "string",
			Sensitive:   true,
			Description: "The address this tenant's support mail is sent from.",
			Group:       "support",
		},
	); err != nil {
		return err
	}
	if err := reg.Features.Add(
		pkgcore.FeatureFlag{
			Key:         FeatureFlagSmilePreview,
			Default:     false,
			Description: "Lets a tenant's users try AI smile previews before a full simulation.",
		},
		pkgcore.FeatureFlag{
			Key:         FeatureFlagPremiumUpsell,
			Default:     true,
			Description: "Shows the premium upsell to a tenant's users.",
			DependsOn:   []string{FeatureFlagSmilePreview},
		},
	); err != nil {
		return err
	}
	return nil
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
