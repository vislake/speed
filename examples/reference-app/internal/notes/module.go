package notes

import (
	"embed"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/examples/reference-app/internal/notes/locales"
	"github.com/vislake/speed/examples/reference-app/internal/notes/migrations"
)

//go:embed openapi.yaml
var openAPISpecYAML []byte

const (
	// moduleName is notes' pkgcore.Module.Name(). It is also the module
	// name dbkit.MigrationRegistry.Register keys its dependency graph on.
	moduleName = "notes"

	// apiPath is the single literal path this module mounts (see
	// Register below) and the one Handler's internal router dispatches
	// methods on (see handler.go) -- both must agree on the exact same
	// absolute path.
	apiPath = "/api/v1/notes"

	// PermissionRead and PermissionWrite are notes' resource:action
	// permission strings (backend coding standard §2's Register example).
	// No rbac module exists yet to enforce them (root CLAUDE.md's M0
	// status), so declaring them here only exercises the registry surface
	// today; nothing in this package checks them yet.
	PermissionRead  = "notes:read"
	PermissionWrite = "notes:write"

	// EventNoteCreated is the domain event type published whenever a note
	// is created, following the "<module>.<entity>.<action>" convention
	// (backend coding standard §8; go/pkgcore/registry.go's
	// EventDecl.Type doc comment). Handler.create is the one place that
	// actually calls EventBus.Publish for it -- see handler.go.
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
)

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
}

// Module implements pkgcore.Module for notes, examples/reference-app's
// placeholder tenant-scoped business resource (see this package's doc.go).
// It is deliberately generic, non-dental business content, standing in for
// the real modules that land in later milestones.
type Module struct {
	repo    *Repository
	handler *Handler
}

// NewModule returns a Module whose repository is backed by db. db is
// expected to come from dbkit.Open; constructing a Module performs no I/O
// of its own -- opening and migrating db is the caller's responsibility,
// done once at startup before Bootstrap ever calls Register (see
// cmd/server's wiring for the exact sequence, and Register's own doc
// comment below for why Register itself still must not touch the
// database).
func NewModule(db *gorm.DB) *Module {
	return &Module{repo: NewRepository(db)}
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

// OpenAPISpec implements pkgcore.Module.
func (m *Module) OpenAPISpec() []byte { return openAPISpecYAML }

// Register implements pkgcore.Module. Per the interface's own doc comment
// ("It must not perform I/O; it only declares"), this method never touches
// the database or the network: constructing m.handler wires together
// already-built, in-memory values (m.repo, and the EventBus reference
// reg.EventBus() returns) -- obtaining that reference is not itself an I/O
// operation, it is just reading a field off reg. The bus is not actually
// called (no Publish) until a real HTTP request creates a note; see
// handler.go's create method.
func (m *Module) Register(reg *pkgcore.Registry) error {
	m.handler = NewHandler(m.repo, reg.EventBus())
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
	if err := reg.AuditActions.Add(AuditActionNoteCreate); err != nil {
		return err
	}
	return nil
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
