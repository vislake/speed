package pki

import (
	"context"
	"fmt"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// SignerRegistry is the package-level pkgcore.SeamRegistry[Signer] every
// host resolves a named Signer implementation through, mirroring the
// database/sql driver-registration pattern pkgcore's own built-in seam
// components already follow (self-registered from go/pkgcore's
// eventbus_memory.go, kv_memory.go, mailer_console.go, mailer_smtp.go and
// objectstore_local.go) -- and the exact
// mechanism the KMS-backed providers follow too: go/pki/signer/vault and
// go/pki/signer/kmsaws each register themselves under a name
// ("signer.vault", "signer.aws-kms") from their own init(), the same shape
// go/pkgcore's own eventbus/redis, kv/redis and objectstore/s3
// registrations establish. A host that never imports a provider subpackage
// never resolves that name, and resolving an unimported one fails with
// pkgcore.ErrUnknownImplementation naming it -- the
// "unknown driver" cost database/sql's own drivers accept, spelled out for
// the built-in seams' own registries.
//
// "signer.local" is registered below, in this package's own init(), rather
// than through a subpackage: LocalSigner already lives in go/pki's root
// package (it is the module's zero-external-dependency default, not an
// optional add-on -- see signer_local.go), so splitting it into a
// subpackage would buy no dependency isolation the way the redis/s3 splits
// did. See localSignerFromConfig's own doc comment for what building it
// from a flat pkgcore.Config, rather than the *gorm.DB Module already
// holds, actually costs.
//
// # SignerRegistry vs. Module.WithSigner
//
// Both exist, deliberately, for two different callers -- the identical
// shape pkgcore itself keeps WithEventBus/WithKVStore/WithMailer/
// WithObjectStore (direct injection) alongside the name-based seam
// registries:
//
//   - WithSigner is for a caller that already holds a concrete Signer
//     value -- typically a *LocalSigner built over the same *gorm.DB the
//     rest of the Module's tables share (NewModule's own default), or any
//     other Signer constructed however the caller likes. No name lookup,
//     no Config adaptation: the caller already did the wiring.
//   - SignerRegistry is for composing a Signer by name plus a flat
//     pkgcore.Config -- the shape a config file or an
//     environment-driven bootstrap naturally produces, without the caller
//     writing any Go construction code. A host wiring go/pki through such
//     a mechanism calls SignerRegistry.Build(name, cfg) and passes the
//     result to WithSigner -- the two are composed together, not
//     alternatives to each other.
var SignerRegistry = pkgcore.NewSeamRegistry[Signer]()

func init() {
	mustRegisterSigner(pkgcore.Registration[Signer]{
		Name: "signer.local",
		// LocalSigner does NOT declare pkgcore.KeyNeverLeavesBoundary: Sign
		// decrypts the private key into this process's memory for the
		// duration of the crypto/ed25519 call -- see signer_local.go's own
		// doc comment.
		Capabilities: 0,
		New:          localSignerFromConfig,
	})
}

// mustRegisterSigner adds r to SignerRegistry and panics if that fails. It
// is only ever called here, against the one name this file controls, so a
// failure -- a duplicate name -- is a programming error in this file, not a
// condition a caller could hit or would want to recover from. This mirrors
// pkgcore's own unexported mustRegister helper and the identical copy each
// of eventbus/redis, kv/redis and objectstore/s3 carries; it cannot call
// pkgcore's because that one is unexported to pkgcore's own root package.
func mustRegisterSigner(r pkgcore.Registration[Signer]) {
	if err := SignerRegistry.Register(r); err != nil {
		panic(fmt.Sprintf("pki: builtin implementation registration failed: %v", err))
	}
}

// localSignerFromConfig adapts a flat pkgcore.Config onto NewLocalSigner,
// which needs a *gorm.DB -- something a map[string]string cannot carry
// directly. This constructor resolves that by opening its OWN database
// connection via dbkit.Open, using cfg's "dialect" and "dsn": the same
// flat-Config-carries-rich-configuration trade the redis and S3
// registrations already make for a client address and credentials.
//
// # This is deliberately NOT the same connection a Module shares
//
// A *pki.Module built by NewModule already has an open, migrated *gorm.DB
// and uses it directly for its default LocalSigner -- that path (still the
// recommended one for wiring LocalSigner as part of a Module) goes through
// WithSigner, never through this registry entry. Building a Signer through
// SignerRegistry.Build("signer.local", cfg) instead opens a SEPARATE
// connection to whatever database cfg's dsn names. That database must
// already carry this module's migrations (pki_local_keys in particular) --
// nothing here applies them -- and the caller's process must have already
// blank-imported the matching dbkit dialect driver
// (dbkit/dialect/sqlite or dbkit/dialect/postgres), exactly as any other
// dbkit.Open caller must; an unregistered dialect fails with dbkit's own
// clear error naming the missing blank import, not a panic here.
//
// # The GORM serializer registry is process-global -- this constructor does
// # NOT call RegisterLocalKeySerializer
//
// LocalKey.EncryptedPrivateKey is only ever correctly readable when
// LocalKeySerializerName is registered against the SAME cipher every
// database holding a pki_local_keys table uses -- see
// RegisterLocalKeySerializer's own doc comment for why that registration
// must happen once, before any *gorm.DB using the schema is opened.
// Calling RegisterLocalKeySerializer again from inside this function, with
// whatever cipher a Config happened to carry, would silently overwrite that
// process-wide registration for every OTHER already-open connection to a
// pki_local_keys table -- including a Module's own. So this constructor
// requires the host to have already called RegisterLocalKeySerializer
// itself (the same requirement NewModule's own doc comment already places
// on every caller), and cfg carries no cipher material at all.
//
// # No caller-supplied context
//
// pkgcore.Registration[T].New takes no context.Context, but dbkit.Open
// performs a real connection and pings it. The other built-in seam
// registrations never hit this because their client construction does not
// dial (go-redis and the S3/SMTP clients all connect lazily, on first
// use); "signer.local" genuinely must dial at Build time, so this
// function uses context.Background() and a Build call against an
// unreachable dsn can block until the underlying driver's own dial/ping
// timeout elapses (SQLite's is effectively instantaneous; a network
// PostgreSQL server that is down is not). A host that needs a bounded
// timeout constructs the *gorm.DB itself, under its own context, and wires
// it with WithSigner instead of going through this registry entry.
func localSignerFromConfig(cfg pkgcore.Config) (Signer, error) {
	dialect := cfg["dialect"]
	dsn := cfg["dsn"]
	if dialect == "" || dsn == "" {
		return nil, fmt.Errorf("pki: builtin signer.local seam: %w: requires \"dialect\" and \"dsn\"", pkgcore.ErrMissingSeamConfig)
	}

	db, err := dbkit.Open(context.Background(), dbkit.Options{
		Dialect: dbkit.Dialect(dialect),
		DSN:     dsn,
	})
	if err != nil {
		return nil, fmt.Errorf("pki: builtin signer.local seam: %w", err)
	}
	return NewLocalSigner(db), nil
}

// BuildSignerRequiring resolves the Signer registered under name exactly as
// SignerRegistry.Build does, and refuses the resolution when the
// registration's declared Capability cannot satisfy required -- the
// pki-local half of the capability comparison the assembly performs for
// its selected components (go/pkgcore/component_assembly.go's
// validateComponentCapabilities), which has no knowledge of this registry
// or of pki.Signer (go/pkgcore/capability.go's KeyNeverLeavesBoundary doc
// comment records that boundary). The comparison is truthful only here, at the registry
// resolution: a registration's Capability is fixed at Register time and
// returned alongside the constructed value, while a Signer injected
// directly through Module.WithSigner carries no capability declaration at
// all for this function -- or anything else -- to compare. A host that
// intends to require pkgcore.KeyNeverLeavesBoundary of the signer it wires
// (the vault/kmsaws direct-sign names declare it; the envelope names and
// "signer.local" do not) declares that intent by calling this function
// instead of SignerRegistry.Build, and a wrong name -- "signer.vault"
// where "signer.vault-direct" was meant -- fails here with an error naming
// the signer and the missing capability instead of silently composing a
// signer that decrypts key material into this process's memory.
//
// The resolution is constructed before the capability is compared, exactly
// like the assembly's own construct-then-validate machinery
// (go/pkgcore/component_assembly.go's validateComponentCapabilities): a
// registration's New
// is this registry's only outward channel for its declared Capability, so
// the comparison cannot precede construction through the public API. The
// constructed value is discarded on refusal -- never returned, never
// wired, never signed with.
//
// The returned error wraps pkgcore.ErrCapabilityUnsatisfied, the identical
// sentinel the assembly's own capability refusal wraps, so a caller
// that already treats that sentinel as "the assembly cannot run as
// declared" needs no new error vocabulary for the pki tier.
func BuildSignerRequiring(name string, cfg pkgcore.Config, required pkgcore.Capability) (Signer, error) {
	signer, caps, err := SignerRegistry.Build(name, cfg)
	if err != nil {
		return nil, err
	}
	if caps.Has(required) {
		return signer, nil
	}
	missing := required &^ caps
	return nil, fmt.Errorf("%w: pki signer %q declares %s, which lacks %s required by the caller",
		pkgcore.ErrCapabilityUnsatisfied, name, caps, missing)
}
