package pkgcore

// SeamPreset is one Preset entry: which registered implementation a seam
// builds when the host has not injected one directly, plus the Config that
// implementation's Registration.New receives.
type SeamPreset struct {
	// Implementation names a Registration on the seam's SeamRegistry -- the
	// name a kernel resolves through, for example "eventbus.redis" or
	// "mailer.smtp". An unknown name fails Bootstrap with
	// ErrUnknownImplementation naming it.
	Implementation string

	// Config carries the flat scalar settings New is called with (keyed by
	// whatever names the implementation documents). The zero value -- a nil
	// Config, what every entry of the built-in presets carries -- hands New
	// an empty Config, so the implementation builds from its own documented
	// defaults and fails with ErrMissingSeamConfig when a setting it cannot
	// default is absent. See Config's own doc comment for the boundary the
	// strings stay inside.
	Config Config
}

// Preset names, for each infrastructure seam, which registered implementation
// a Kernel builds when the host has not injected one directly for that seam,
// and with which Config. The map keys are the seam names below; a Kernel
// resolves an unwired seam by looking up its key here and building the named
// Registration on that seam's SeamRegistry (EventBusRegistry and so on),
// passing the entry's Config through.
//
// A Preset is one layer of the three-layer composition stack -- built-in
// preset < config-file override < code injection -- resolved inside
// Kernel/Bootstrap itself, which is the bootstrap layer pkgcore offers; a
// host's own configuration layer (koanf, environment) sits above it and
// feeds a resolved Preset in, exactly as it feeds Config.OTLPEndpoint in.
// pkgcore never reads a config file or the environment to build one itself.
// Preset.With derives an override from such a preset without mutating it.
type Preset map[string]SeamPreset

// With returns a copy of p with seam set to sp, leaving p untouched. It is
// the middle layer of the composition stack in its callable form: a host
// starts from a built-in preset and overrides the entries whose
// implementation or Config its own configuration names,
//
//	preset := pkgcore.PresetStandalone.With("mailer", pkgcore.SeamPreset{
//		Implementation: "mailer.smtp",
//		Config:         pkgcore.Config{"host": "smtp.example.com"},
//	})
//
// The copy matters because PresetStandalone and PresetDistributed are
// package-level maps shared by every Kernel in the process: assigning into a
// preset in place would rewrite the built-in default for every later
// bootstrap. An entry naming a seam no registry pre-registers is accepted
// here without validation; the typo surfaces at Bootstrap as
// ErrUnknownImplementation, naming the seam and the implementation.
func (p Preset) With(seam string, sp SeamPreset) Preset {
	next := make(Preset, len(p)+1)
	for k, v := range p {
		next[k] = v
	}
	next[seam] = sp
	return next
}

// Seam keys a Preset entry may set. These are the only four seams pkgcore
// pre-registers implementations for; Queue, distributed locks and the
// other seams are not here yet, since no built-in implementation exists
// for them in this package.
const (
	presetKeyEventBus    = "eventbus"
	presetKeyKVStore     = "kv"
	presetKeyMailer      = "mailer"
	presetKeyObjectStore = "objectstore"
)

// PresetStandalone is the Kernel zero value's default composition: every
// seam resolves to its in-process, zero-external-dependency implementation,
// so a bare NewKernel() is the zero-configuration standalone default and
// starts in seconds with nothing else running. No entry carries a Config:
// the in-process implementations have nothing to configure.
var PresetStandalone = Preset{
	presetKeyEventBus:    {Implementation: "eventbus.memory"},
	presetKeyKVStore:     {Implementation: "kv.memory"},
	presetKeyMailer:      {Implementation: "mailer.console"},
	presetKeyObjectStore: {Implementation: "objectstore.local"},
}

// PresetDistributed names pkgcore's own multi-replica-safe implementation of
// every seam: the composition WithPreset(PresetDistributed) selects for a
// host that wants pkgcore's built-in Redis/SMTP/S3 implementations rather
// than injecting its own. Three of the four names it points at --
// "eventbus.redis", "kv.redis" and "objectstore.s3" -- are registered by
// their own subpackages (eventbus/redis, kv/redis, objectstore/s3), not by
// this package, so a host that wants them must import the matching
// subpackage (a blank import is enough) before that seam resolves; an
// unimported one fails Bootstrap with ErrUnknownImplementation naming the
// seam and the implementation, the same "unknown driver" trade
// database/sql's own drivers make.
//
// No entry of this Preset carries a Config: it names implementations, not
// deployments, and pkgcore ships no credentials. The two Redis-backed seams
// therefore fall back to a bare-minimum default ("localhost:6379", no auth)
// that is only useful against a local Redis, and the SMTP and S3 seams --
// which have no safe default host, bucket or credential -- fail to resolve
// at all under this Preset with ErrMissingSeamConfig. A host that wants
// either carried through the preset layer overrides the entry with the
// config it needs -- PresetDistributed.With("mailer", SeamPreset{
// Implementation: "mailer.smtp", Config: Config{"host": ...}}) -- or injects
// the implementation directly with WithMailer or WithObjectStore, which this
// Preset does not stand in the way of: injection always wins over a
// Preset entry, per seam.
var PresetDistributed = Preset{
	presetKeyEventBus:    {Implementation: "eventbus.redis"},
	presetKeyKVStore:     {Implementation: "kv.redis"},
	presetKeyMailer:      {Implementation: "mailer.smtp"},
	presetKeyObjectStore: {Implementation: "objectstore.s3"},
}
