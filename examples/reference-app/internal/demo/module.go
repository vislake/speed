// Package demo hosts the reference app's demo notification types: the
// patient-facing appointment reminder ("demo.patient_reminder") that
// cmd/server's demo patient-message route dispatches to a verified external
// contact, and the smile-simulation-ready SMS
// (smilesim.EventSimulationCompleted) that cmd/server's smilesim completion
// glue dispatches to the user who started the simulation -- both wired end
// to end as the notification module's consumers (see
// cmd/server/demo_notification.go and the module's own notification_flow_test/
// smilesim_flow_test suites).
//
// The package exists because both types' template copy must live inside
// the app's Bootstrap module set: Kernel.Bootstrap assembles the merged
// message catalog from the Locales() of the modules it is given, freezing it
// once before the registry is returned, and the notification module renders
// every dispatch from that frozen catalog. A notification type whose copy
// sits outside the bootstrap set can never render. demo is the smallest
// module that qualifies: it declares no tables, no API fragment, no
// events of its own, no permissions and no configuration -- only two
// notification types and the two-language template set that renders them.
//
// Like notes it declares its type as a pkgcore.NotificationType rather than
// importing go/notification: business code publishes facts and notification
// consumes them, and the dependency never points the other way (backend
// coding standard §8; notes' noteCreatedNotificationType comment says the
// same of the shared channel vocabulary).
package demo

import (
	"embed"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/examples/reference-app/internal/demo/locales"
)

const (
	// moduleName is demo's pkgcore.Module.Name(). It appears in no
	// migration registry (the module ships no migrations) and in no CI
	// matrix (the module's only build leg is the reference app's own); its
	// one real purpose is the Locales() key the merged catalog prefixes
	// this module's message ids with.
	moduleName = "demo"

	// TypeKeyPatientReminder is the Key of the notification type this
	// module declares: a transactional, opt-out-resistant appointment
	// reminder for a verified external contact (an email or SMS address
	// whose owner consented, go/notification's verified_contacts ledger).
	// cmd/server's demo patient-message route dispatches it and reads this
	// constant rather than retyping the string.
	//
	// The type's Unsubscribable: false is the deliberate contrast notes
	// module.go's noteCreatedNotificationType comment points at: a patient
	// reminder is a transactional message tied to a consent the contact
	// already gave (the verification that created the contact row), not a
	// marketing stream a recipient must be able to silence -- so the
	// preference matrix refuses to switch its last channel off
	// (notification's ErrPreferenceOptoutNotAllowed), where notes' type,
	// unsubscribable true, may be silenced entirely.
	TypeKeyPatientReminder = "demo.patient_reminder"

	// TypeKeySimulationReady is the Key of the second notification type
	// this module declares: a transactional SMS telling a tenant member
	// their smile-simulation image has finished generating.
	// cmd/server's smilesim completion glue (demo_notification.go's
	// EventSimulationCompleted subscription) dispatches it and reads this
	// constant rather than retyping the string.
	//
	// Deliberately NOT the same string as smilesim.EventSimulationCompleted:
	// notes' own noteCreatedNotificationType can reuse its event's name
	// verbatim as its Key because notes declares BOTH the event and the
	// type -- but this type's template copy lives in THIS module's own
	// Locales() bundle, and pkgcore/i18n's per-module id-namespace rule
	// requires every id this module ships to start with "demo." (this
	// module's own Name()), never "smilesim." (smilesim declares no
	// pkgcore.Module at all -- see its own package comment). render.go
	// builds a template's message id directly from the Dispatch's TypeKey,
	// so the Key here MUST be the "demo."-prefixed string the copy below
	// actually lives under; the event type and the notification type key
	// are two different strings answering two different questions (a fact
	// that happened, versus a message vocabulary entry), exactly as they
	// are for every OTHER cross-module event-to-notification wiring in
	// this app (compare demoPatientMessagePath's trigger, which is not
	// itself an event name either).
	TypeKeySimulationReady = "demo.simulation_ready"
)

// patientReminderNotificationType is TypeKeyPatientReminder's entry in the
// notification preference matrix, registered by Register below. The channel
// strings are go/notification's own channel vocabulary, written out here
// without importing the module, exactly as notes writes out its three
// channels (see notes' own declaration for the reasoning).
var patientReminderNotificationType = pkgcore.NotificationType{
	Key:             TypeKeyPatientReminder,
	Group:           "clinical",
	DefaultChannels: []string{"email", "sms"},
	Unsubscribable:  false,
}

// simulationReadyNotificationType is TypeKeySimulationReady's entry in the
// notification preference matrix -- see that constant's own doc comment
// for why its Key is a "demo."-prefixed string of its own rather than
// smilesim.EventSimulationCompleted verbatim.
//
// SMS-only by design: a smile simulation is a short-lived, in-session
// result the recipient is typically still on the page for (or checking
// back on shortly after), the case an SMS ping serves well and an email
// digest does not. Unsubscribable: true, like notes' own type -- unlike
// the patient reminder above, this is not tied to a consent a contact
// separately gave; it is an ordinary user notification a recipient may
// silence.
var simulationReadyNotificationType = pkgcore.NotificationType{
	Key:             TypeKeySimulationReady,
	Group:           "clinical",
	DefaultChannels: []string{"sms"},
	Unsubscribable:  true,
}

// Module implements pkgcore.Module for demo: the reference app's notification
// type carrier. It is stateless -- no repository, no handler, no seams -- and
// its Register call is a single declaration.
type Module struct{}

// NewModule returns a Module. The construction is parameterless because a
// module that declares only copy and a type needs nothing to be wired into
// it.
func NewModule() *Module { return &Module{} }

// Name implements pkgcore.Module.
func (m *Module) Name() string { return moduleName }

// DependsOn implements pkgcore.Module. demo depends on nothing: its
// declaration is self-contained, and the notification module that renders it
// is not a dependency of the type's declaration but a consumer of the
// catalog both of them ride in.
func (m *Module) DependsOn() []string { return nil }

// Migrations implements pkgcore.Module. demo owns no tables, so it returns
// an empty FS rather than shipping an empty migrations directory; cmd/server
// therefore never registers demo on its dbkit.MigrationRegistry (see its
// wiring comment).
func (m *Module) Migrations() embed.FS { return embed.FS{} }

// Locales implements pkgcore.Module.
func (m *Module) Locales() embed.FS { return locales.FS }

// OpenAPISpec implements pkgcore.Module. demo mounts no HTTP surface of its
// own -- the demo patient-message route is a hand-written cmd/server route
// outside the OpenAPI machinery (see cmd/server/demo_notification.go) -- so
// there is no fragment to return.
func (m *Module) OpenAPISpec() []byte { return nil }

// Register implements pkgcore.Module: it declares the patient-reminder
// notification type, making it visible to the preference matrix of whatever
// notification module the host boots. No I/O happens here; declaring the type
// is all this module does.
func (m *Module) Register(reg *pkgcore.Registry) error {
	return reg.Notifications.Add(patientReminderNotificationType, simulationReadyNotificationType)
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
