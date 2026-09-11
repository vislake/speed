package notification

// component.go registers the "notification" component with pkgcore's global
// component registration: the descriptor a composition configuration selects
// as the "notification" module's single implementation. The component builds
// the same *Module every other caller builds through NewModule, so the
// module's services, HTTP surface and registration behavior are one
// implementation reachable two ways.

import (
	"context"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/notification/locales"
	"github.com/vislake/speed/go/notification/migrations"
)

// notificationComponentConfig is the "notification" component's
// configuration schema: one field per key a composition block may carry.
// Decoding is strict, so a block naming any other key fails before anything
// is constructed.
//
//	mail_from   string   From address of every composed mail (Register requires it)
//	reply_to    string   Reply-To address, omitted on the wire when empty
type notificationComponentConfig struct {
	MailFrom string `json:"mail_from"`
	ReplyTo  string `json:"reply_to"`
}

// notificationComponent is the component descriptor for "notification". It
// declares MultiReplicaSafe: the module's state is its rows in the shared
// database, the inbox announcement fans out across replicas through the
// event bus, and the delivery pipeline is designed for several replicas
// draining one queue.
//
// Requires, per the module's own construction contract: the database, the
// SMS sender and delivery queue Register refuses to boot without, and the
// user-address resolver (ErrUserAddressResolverRequired). The mail transport
// and the key-value store are optional -- both read at call time through the
// registry, both failing closed when absent -- as are the subject and
// profile-locale resolvers, whose absence every endpoint documents as a
// legal, if reduced, wiring.
//
// BootstrapKeys declares the contact index key (notification.contact_index_key):
// the HMAC key the blind indexers over the module's encrypted contact
// addresses are built from.
//
// Prepare is deliberately not declared: the contact-address serializer and
// the two blind indexers it needs are registered today by the host's
// pre-database path (WithContactEmailIndexer/WithContactPhoneIndexer), from
// cipher material the host holds; this descriptor declares no Prepare step
// until key material is addressable through the registry. Init is
// deliberately not declared either: declaration (the module's Register call)
// is made today by the host's bootstrap path, not by this descriptor.
var notificationComponent = pkgcore.Component{
	Name:         "notification",
	Module:       "notification",
	Provides:     []any{(*Module)(nil)},
	Capabilities: pkgcore.MultiReplicaSafe,
	ConfigSchema: (*notificationComponentConfig)(nil),
	Requires: []pkgcore.Requirement{
		{Token: (*gorm.DB)(nil)},
		{Token: (*pkgcore.SMSSender)(nil)},
		{Token: (*jobs.Queue)(nil)},
		{Token: (*UserAddressResolver)(nil)},
		{Token: (*pkgcore.Mailer)(nil), Optional: true},
		{Token: (*pkgcore.KVStore)(nil), Optional: true},
		{Token: (*SubjectResolver)(nil), Optional: true},
		{Token: (*UserLocaleResolver)(nil), Optional: true},
	},
	BootstrapKeys: []pkgcore.BootstrapKey{bootstrapKeyDecl},
	Migrations:    migrations.FS,
	Locales:       locales.FS,
	OpenAPISpec:   openAPISpecYAML,
	New: func(_ context.Context, reg *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
		var c notificationComponentConfig
		if err := cfg.Decode(&c); err != nil {
			return nil, err
		}
		db, err := pkgcore.Get[*gorm.DB](reg)
		if err != nil {
			return nil, err
		}
		sms, err := pkgcore.Get[pkgcore.SMSSender](reg)
		if err != nil {
			return nil, err
		}
		queue, err := pkgcore.Get[jobs.Queue](reg)
		if err != nil {
			return nil, err
		}
		resolver, err := pkgcore.Get[UserAddressResolver](reg)
		if err != nil {
			return nil, err
		}
		opts := []Option{
			WithSMSSender(sms),
			WithMailFrom(c.MailFrom),
			WithReplyTo(c.ReplyTo),
			WithDeliveryQueue(queue),
			WithUserAddressResolver(resolver),
		}
		if subject, err := pkgcore.Get[SubjectResolver](reg); err == nil {
			opts = append(opts, WithSubjectResolver(subject))
		}
		if locale, err := pkgcore.Get[UserLocaleResolver](reg); err == nil {
			opts = append(opts, WithUserLocaleResolver(locale))
		}
		return NewModule(db, opts...), nil
	},
}

func init() { pkgcore.MustRegister(notificationComponent) }
