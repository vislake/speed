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

// Module carries the fixture's module contract: a stand-in business module
// of the host that declares the clinic's notification type and ships its
// message copy. It performs no I/O and owns no tables -- the tier only needs
// its declaration (the delivery pipeline's live-taxonomy lookup) and its
// bundle (the merged catalog's render).
type Module struct{}

// Name implements the module contract.
func (m *Module) Name() string { return "clinic" }

// DependsOn implements the module contract: nothing -- the fixture is a leaf.
func (m *Module) DependsOn() []string { return nil }

// Migrations implements the module contract: an empty FS -- the fixture owns
// no tables.
func (m *Module) Migrations() embed.FS { return embed.FS{} }

// Locales implements the module contract: the fixture's own embedded bundle,
// in both supported languages with identical id sets (see the .toml files'
// headers for why the copy is English in both).
func (m *Module) Locales() embed.FS { return FS }

// OpenAPISpec implements the module contract: nil -- the fixture mounts no
// HTTP.
func (m *Module) OpenAPISpec() []byte { return nil }

// Register implements the module contract: it declares the fixture's one
// notification type on the host's notification-type registrar, the way a
// real declaring module does in its own Register. The assembly builds the
// merged catalog only after every module in the set has registered, and
// the preference service reads the registrar live (see notification's
// attachTypes), so this Add is safe however the module graph orders the
// two Register calls.
func (m *Module) Register(reg *pkgcore.ComponentRegistry) error {
	return reg.NotificationsSeat().Add(pkgcore.NotificationType{
		Key:             AppointmentReminderKey,
		Group:           "appointments",
		DefaultChannels: []string{"in_app"},
		Unsubscribable:  true,
	})
}
