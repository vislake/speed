package pkgcore

import "github.com/vislake/speed/go/pkgcore/i18n"

// Registrar is the declaration face a module's or a component's declaration
// body addresses: the ten declaration seats it writes its declarations
// into, plus the resolved infrastructure values it reads while declaring.
//
// It is a pure view over one declaration face, not an abstraction layer. The
// two registry shapes answer every accessor with the very object they hold:
// the module Registry returns the registrar stored in its own field, and the
// component assembly's ComponentRegistry returns the seat it built, whose
// write gate (writes only during the Init stage) runs inside that seat and
// nowhere else. A body written against Registrar therefore declares into
// exactly the seats, through exactly the gate, of whichever registry it was
// handed -- the two shapes answer the same declarations, and a declaration
// made through an accessor is indistinguishable from one made through the
// registry's own field.
//
// The accessors are suffixed "Seat" where a name would collide with a
// registrar type's purpose: RoutesSeat, ConfigSeat and so on, one per
// declaration seat. The five value accessors carry the names the module
// Registry has always answered them under, and both shapes share their
// contract: EventBus derives from the Events registrar, so substituting that
// registrar moves publishers and subscribers together; KVStore, Mailer,
// ObjectStore and Locales return the value the registry was wired with, and
// a registry with none answers nil.
type Registrar interface {
	// RoutesSeat returns the seat modules mount HTTP handlers on.
	RoutesSeat() RouteRegistrar
	// ConfigSeat returns the seat modules declare runtime configuration
	// schema on.
	ConfigSeat() ConfigSchemaRegistrar
	// FeaturesSeat returns the seat modules declare feature flags on.
	FeaturesSeat() FeatureRegistrar
	// PermissionsSeat returns the seat modules define resource:action
	// permissions on.
	PermissionsSeat() PermissionRegistrar
	// JobsSeat returns the seat modules register asynchronous job handlers
	// on.
	JobsSeat() JobHandlerRegistrar
	// NotificationsSeat returns the seat modules declare notification types
	// on.
	NotificationsSeat() NotificationRegistrar
	// EventsSeat returns the seat modules declare published events and
	// install subscriptions on.
	EventsSeat() EventRegistrar
	// AuditActionsSeat returns the seat modules define audit actions on.
	AuditActionsSeat() AuditActionRegistrar
	// RetentionSeat returns the seat modules register retention-sweep,
	// right-to-erasure and data-export participants on.
	RetentionSeat() RetentionRegistrar
	// SchedulesSeat returns the seat modules declare periodic tasks on.
	SchedulesSeat() PeriodicTaskRegistrar

	// EventBus returns the bus the Events seat installs subscriptions on,
	// so a publisher and a subscriber resolve the same bus.
	EventBus() EventBus
	// KVStore returns the key-value store the registry was wired with.
	KVStore() KVStore
	// Mailer returns the mail transport the registry was wired with.
	Mailer() Mailer
	// ObjectStore returns the object store the registry was wired with.
	ObjectStore() ObjectStore
	// Locales returns the merged message catalog the registry carries.
	Locales() *i18n.Catalog
}

// The two registry shapes both answer the declaration face: the module
// Registry through the fields and methods it has always carried, and the
// component assembly's ComponentRegistry through its seats and by-type
// context. A declaration body can therefore be handed either one.
var (
	_ Registrar = (*Registry)(nil)
	_ Registrar = (*ComponentRegistry)(nil)
)
