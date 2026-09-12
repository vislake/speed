package notification

// component.go registers the "notification" component with pkgcore's global
// component registration: the descriptor a composition configuration selects
// as the "notification" module's single implementation. The component builds
// the same *Module every other caller builds through NewModule, so the
// module's services, HTTP surface and registration behavior are one
// implementation reachable two ways.

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/notification/locales"
	"github.com/vislake/speed/go/notification/migrations"
)

// ContactIndexKeyPath is the key path of the HMAC key notification's blind
// indexers are built from: the derive field "contact_index_key" under the
// component's own "notification" namespace, so the key resolves at the
// platform key path it has always carried (pkgcore.BootstrapKeyPurpose
// embeds the path, and a rename would silently rotate the key). It is also
// the material address a host's wiring reads the key from.
const ContactIndexKeyPath = "notification.contact_index_key"

// notificationComponentConfig is the "notification" component's
// configuration schema: one field per key a composition block may carry,
// plus the process-start key material as a derive field. The component's
// namespace is "notification", so the key resolves at ContactIndexKeyPath
// rather than at the field's bare local path. Decoding is
// strict, so a block naming any other key fails before anything is
// constructed.
//
//	contact_index_key  []byte  HMAC key the contact-address blind indexers are built from
//	mail_from          string  From address of every composed mail (Register requires it)
//	reply_to           string  Reply-To address, omitted on the wire when empty
type notificationComponentConfig struct {
	// ContactIndexKey indexes the encrypted contact addresses; one key
	// serves both the email and phone indexer. The derive option resolves
	// it through the five-source chain (an explicit flag/environment/file
	// value, the root-key derivation, the declared defaults table).
	ContactIndexKey []byte `json:"contact_index_key" config:"derive,sensitive,group=notification"`
	MailFrom        string `json:"mail_from"`
	ReplyTo         string `json:"reply_to"`
}

// ConfigDocs implements pkgcore.Documented: the operator-facing contract of
// the schema's sensitive key-material field.
func (*notificationComponentConfig) ConfigDocs() map[string]pkgcore.FieldDoc {
	return map[string]pkgcore.FieldDoc{
		"contact_index_key": {
			Description: "HMAC key the notification module's blind indexers index its encrypted contact addresses with; one key serves the email and phone indexers, whose canonical forms are disjoint, and it stays separate from every cipher key.",
			Default:     "documented non-secret development default",
		},
	}
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
// The schema's derive field declares the contact index key
// (ContactIndexKeyPath): the HMAC key the blind indexers over the module's
// encrypted contact addresses are built from.
//
// Prepare is deliberately not declared: the contact-address serializer and
// the two blind indexers it needs are registered today by the host's
// pre-database path (WithContactEmailIndexer/WithContactPhoneIndexer), from
// cipher material the host holds. Until this descriptor supplies those two
// indexers itself, a module built here fails Register's own
// ErrContactEmailIndexerRequired, so the assembly path reaches declaration
// only once the indexer step lands.
//
// Init runs the module's one declaration entry point, Register, inside the
// assembly's Init stage -- the one stage whose seats accept writes -- so the
// component world declares exactly what the module's Register declares.
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
		// The http component's route face (go/app/httpserve), where the
		// module's HTTP surface is mounted when the composition serves
		// HTTP. Optional: a pure-background composition carries no http
		// component and the module constructs without mounting.
		{Token: (*pkgcore.RouteRegistrar)(nil), Optional: true},
	},
	// The "notification" namespace keeps the schema's contact_index_key
	// field at the platform key path the key has always carried
	// (ContactIndexKeyPath) instead of the field's bare local path.
	ConfigNamespace: "notification",
	Migrations:      migrations.FS,
	Locales:         locales.FS,
	OpenAPISpec:     openAPISpecYAML,
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
		subject, ok, err := pkgcore.GetOptional[SubjectResolver](reg)
		if err != nil {
			return nil, err
		}
		if ok {
			opts = append(opts, WithSubjectResolver(subject))
		}
		locale, ok, err := pkgcore.GetOptional[UserLocaleResolver](reg)
		if err != nil {
			return nil, err
		}
		if ok {
			opts = append(opts, WithUserLocaleResolver(locale))
		}
		return NewModule(db, opts...), nil
	},
	Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, instance any) error {
		m, ok := instance.(*Module)
		if !ok {
			return fmt.Errorf("notification: component init got a %T instance, want *notification.Module", instance)
		}
		return m.Register(reg)
	},
}

func init() { pkgcore.MustRegister(notificationComponent) }
