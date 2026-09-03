package pkgcore

// Preset names, for each infrastructure seam, which registered implementation
// a Kernel builds when the host has not injected one directly for that seam.
// The map keys are the seam names below; a Kernel resolves an unwired seam by
// looking up its key here and building the named Registration on that seam's
// SeamRegistry (EventBusRegistry and so on).
//
// A Preset is the middle layer of the three docs/internal/03-deployment-modes.md
// describes -- built-in preset < config-file override < code injection --
// resolved inside Kernel/Bootstrap itself, which is the bootstrap layer
// pkgcore offers; a host's own configuration layer (koanf, environment) sits
// above it and feeds a resolved Preset in, exactly as it feeds
// Config.OTLPEndpoint in today. pkgcore never reads a config file or the
// environment to build one itself.
type Preset map[string]string

// Seam keys a Preset entry may set. These are the only four seams pkgcore
// pre-registers implementations for; see the deferred-scope note in this
// round's plan for why Queue, distributed locks and the other seams
// docs/internal/03-deployment-modes.md's table names are not here yet.
const (
	presetKeyEventBus    = "eventbus"
	presetKeyKVStore     = "kv"
	presetKeyMailer      = "mailer"
	presetKeyObjectStore = "objectstore"
)

// PresetStandalone is the Kernel zero value's default composition: every
// seam resolves to its in-process, zero-external-dependency implementation,
// so a bare NewKernel() behaves exactly like today's zero-configuration
// standalone default and starts in seconds with nothing else running.
var PresetStandalone = Preset{
	presetKeyEventBus:    "eventbus.memory",
	presetKeyKVStore:     "kv.memory",
	presetKeyMailer:      "mailer.console",
	presetKeyObjectStore: "objectstore.local",
}

// PresetDistributed names pkgcore's own multi-replica-safe implementation of
// every seam: the composition WithPreset(PresetDistributed) selects for a
// host that wants pkgcore's built-in Redis/SMTP/S3 implementations rather
// than injecting its own. Because the preset layer carries no
// per-implementation configuration yet (see Config's doc comment, and the
// deferred-scope note on real credential sourcing in this round's plan), the
// two Redis-backed seams fall back to a bare-minimum default
// ("localhost:6379", no auth) that is only useful against a local Redis, and
// the SMTP and S3 seams -- which have no safe default host, bucket or
// credential -- fail to resolve at all under this Preset with
// ErrMissingSeamConfig. A host that wants real credentials injects the
// implementation directly with WithMailer or WithObjectStore instead, which
// this Preset does not stand in the way of: injection always wins over a
// Preset entry, per seam.
var PresetDistributed = Preset{
	presetKeyEventBus:    "eventbus.redis",
	presetKeyKVStore:     "kv.redis",
	presetKeyMailer:      "mailer.smtp",
	presetKeyObjectStore: "objectstore.s3",
}
