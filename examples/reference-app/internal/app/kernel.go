package app

// This file composes the kernel options the engine bootstraps with: the
// deployment mode, the two seams this host injects (the audit bus and,
// when Redis composes it, the key-value store), the preset overrides the
// configured SMTP and S3 groups name, and the two single-value injections
// a test may arm.

import (
	"strconv"

	"github.com/vislake/speed/go/pkgcore"
	kvredis "github.com/vislake/speed/go/pkgcore/kv/redis"
)

// kernelOptions assembles the kernel options for this boot.
//
// WithDeploymentMode declares the topology the composition is validated
// against; it never selects an implementation (a deployment mode constrains
// which implementations may compose, never selects one). Every seam follows
// the same "an unset env var leaves THAT seam on the Preset's in-process
// default, so a plain `go run ./cmd/server` needs nothing else running"
// shape, but a configured seam travels one of two paths: the SMTP and S3
// compositions -- pure strings, no shared resource -- override the Preset
// entry through its config channel, while the eventbus and the Redis-composed
// kv seam are injected, because the pre-built bus must be the very instance
// dbkit's write-capture plugin publishes on (options above) and the one
// *redis.Client backing both is a typed resource this host owns and closes.
//
// The eventbus is injected in BOTH branches -- not left to the Preset's
// default -- for exactly that identity reason: reg.EventBus() must resolve
// to the bus the capture plugin holds, or org's captured writes would publish
// onto a bus audit's subscriptions never see, each half of the audit path
// working in isolation while no row ever lands. busCapabilities carries the
// declaration of whichever implementation openBus chose (zero for the
// in-process memory bus, the same declaration its "eventbus.memory" builtin
// registration carries; MultiReplicaSafe|SurvivesRestart for the Redis-backed
// one). The Redis-composed kv seam shares the SAME *redis.Client. A
// non-empty cfg.ObjectStoreRoot (APP_OBJECT_STORE_ROOT) is the local twin of
// the S3 composition and stays on the injection path because no registered
// name wraps its constructor: WithObjectStore injects
// pkgcore.NewLocalObjectStore over the fixed directory, declaring
// SurvivesRestart alone -- the directory genuinely survives a process
// restart, and genuinely nothing about a single-process local store is
// replica-safe, so MultiReplicaSafe is never claimed for it. cfg.Mailer is
// the in-process capture double the org invitation suites inject, declared
// Stateless -- the honest capability for a throwaway in-process test double.
// Injection always wins over the preset, per seam, so none of these and the
// channel entries above ever fight.
func (b *serverBuild) kernelOptions() []pkgcore.KernelOption {
	kernelOptions := []pkgcore.KernelOption{pkgcore.WithDeploymentMode(b.cfg.DeploymentMode)}
	kernelOptions = append(kernelOptions, pkgcore.WithEventBus(b.bus, b.busCapabilities))
	if b.cfg.RedisAddr != "" {
		kernelOptions = append(kernelOptions,
			pkgcore.WithKVStore(kvredis.NewKVStore(b.redisClient), pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart))
	}

	// The preset layer, composed explicitly: the standalone preset is the
	// same base NewKernel would default to anyway, and each configured
	// channel seam overrides its one entry. Preset.With returns a copy, so
	// the process-shared PresetStandalone map is never written through.
	preset := pkgcore.PresetStandalone
	if b.cfg.SMTPHost != "" {
		preset = preset.With("mailer", pkgcore.SeamPreset{
			Implementation: "mailer.smtp",
			Config: pkgcore.Config{
				"host":     b.cfg.SMTPHost,
				"port":     strconv.Itoa(b.cfg.SMTPPort),
				"username": b.cfg.SMTPUsername,
				"password": b.cfg.SMTPPassword,
				"reply_to": b.cfg.SMTPReplyTo,
			},
		})
	}
	if b.cfg.S3Endpoint != "" {
		preset = preset.With("objectstore", s3ObjectStoreSeamPreset(b.cfg))
	}
	kernelOptions = append(kernelOptions, pkgcore.WithPreset(preset))

	if b.cfg.ObjectStoreRoot != "" {
		kernelOptions = append(kernelOptions,
			pkgcore.WithObjectStore(pkgcore.NewLocalObjectStore(b.cfg.ObjectStoreRoot), pkgcore.SurvivesRestart))
	}
	if b.cfg.Mailer != nil {
		kernelOptions = append(kernelOptions, pkgcore.WithMailer(b.cfg.Mailer, pkgcore.Stateless))
	}
	return kernelOptions
}
