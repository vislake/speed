//go:build integration

package clinic

import (
	"embed"

	"github.com/vislake/speed/go/pkgcore"
)

// AppointmentReminderKey is the notification type key the clinic fixture
// declares, delivered by the tier's Redis leg: a double opt-in consent flow
// ends at an in-app-only inbox row (the type's DefaultChannels carry just
// the in_app channel, so the delivery never needs a real address or
// transport), and its copy lives in this module's own locale files.
const AppointmentReminderKey = "clinic.appointment_reminder"

// Module is the fixture's pkgcore.Module: a stand-in business module of the
// host that declares the clinic's notification type and ships its message
// copy. It performs no I/O and owns no tables -- the tier only needs its
// declaration (the delivery pipeline's live-taxonomy lookup) and its bundle
// (the merged catalog's render).
type Module struct{}

// Name implements pkgcore.Module.
func (m *Module) Name() string { return "clinic" }

// DependsOn implements pkgcore.Module: nothing -- the fixture is a leaf.
func (m *Module) DependsOn() []string { return nil }

// Migrations implements pkgcore.Module: an empty FS -- the fixture owns no
// tables.
func (m *Module) Migrations() embed.FS { return embed.FS{} }

// Locales implements pkgcore.Module: the fixture's own embedded bundle, in
// both supported languages with identical id sets (see the .toml files'
// headers for why the copy is English in both).
func (m *Module) Locales() embed.FS { return FS }

// OpenAPISpec implements pkgcore.Module: nil -- the fixture mounts no HTTP.
func (m *Module) OpenAPISpec() []byte { return nil }

// Register implements pkgcore.Module: it declares the fixture's one
// notification type on the host's notification-type registrar, the way a
// real declaring module does in its own Register. Bootstrap assembles the
// merged catalog only after every module in the set has registered, and
// the preference service reads the registrar live (see notification's
// attachTypes), so this Add is safe however the module graph orders the
// two Register calls.
func (m *Module) Register(reg *pkgcore.Registry) error {
	return reg.Notifications.Add(pkgcore.NotificationType{
		Key:             AppointmentReminderKey,
		Group:           "appointments",
		DefaultChannels: []string{"in_app"},
		Unsubscribable:  true,
	})
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
