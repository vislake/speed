package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/vislake/speed/pkg/core"
	"github.com/vislake/speed/pkg/log"
)

// scenarioEnv names the scenario to run. It is read from the environment
// rather than from a positional argument because the configuration loader
// rejects positional arguments, and it stays outside the host's own prefix so
// that it is not reported as an input item nobody reads.
const scenarioEnv = "SPEED_LOG_E2E_CASE"

// The scenarios. Each one is a whole run of the host: one process, one
// registry, one assembly.
const (
	caseDefault       = "default"
	caseFallback      = "fallback"
	caseEmptyOutputs  = "empty-outputs"
	caseRivalProvider = "rival-provider"
	caseConcurrent    = "concurrent"
)

// The messages the cases look for. They are constants rather than literals in
// two places so that a case cannot drift from what the host writes.
const (
	infoMessage      = "default-info"
	debugMessage     = "default-debug"
	orphanMessage    = "orphan-record"
	injectedMessage  = "injected-record"
	silencedMessage  = "silenced-record"
	bootstrapMessage = "bootstrap-record"
	chainMessage     = "chain-record"
)

// sensitiveKey is the key the application registers as sensitive in its New,
// and plaintextValue is what it puts under that key. A record carrying them
// proves the redaction layer is on the configured chain of a real assembly,
// not only in the unit tests' handler stacks.
const (
	sensitiveKey   = "password"
	plaintextValue = "hunter2-PLAINTEXT"
)

// concurrentRecords is how many records each writer produces in the
// concurrency scenario, and payloadSize is the length of the attribute that
// makes each record longer than any platform's atomic pipe write.
const (
	concurrentRecords = 1000
	payloadSize       = 8000
)

// scenario reports which scenario this run was asked for.
func scenario() string { return os.Getenv(scenarioEnv) }

// runScenario performs the run's whole work.
func runScenario(ctx context.Context, logger *slog.Logger) {
	switch scenario() {
	case caseDefault:
		logger.InfoContext(ctx, infoMessage, sensitiveKey, plaintextValue)
		logger.DebugContext(ctx, debugMessage)
	case caseFallback:
		// No logger in this context: the record goes wherever the
		// fallback sends it, which is the question the case asks.
		log.FromContext(ctx).InfoContext(ctx, orphanMessage)
		// The positive control: with a logger injected, the record
		// travels the configured chain.
		injected := log.WithLogger(ctx, logger)
		log.FromContext(injected).InfoContext(injected, injectedMessage)
	case caseEmptyOutputs:
		logger.InfoContext(ctx, silencedMessage)
	case caseConcurrent:
		writeConcurrently(ctx, logger)
	default:
		say("loghost: no scenario given in " + scenarioEnv)
	}
}

// writeConcurrently drives the process logger and the configured chain at the
// same time, with records too long to reach the pipe in one write.
//
// Both destinations are standard output, so the two loggers reach the same
// file descriptor. What the case reads back says whether the records arrived
// whole.
func writeConcurrently(ctx context.Context, logger *slog.Logger) {
	payload := strings.Repeat("x", payloadSize)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range concurrentRecords {
			log.Default().InfoContext(ctx, bootstrapMessage, "payload", payload)
		}
	}()
	go func() {
		defer wg.Done()
		for range concurrentRecords {
			logger.InfoContext(ctx, chainMessage, "payload", payload)
		}
	}()
	wg.Wait()
}

// rivalModuleName is the name of the second provider of the logging
// capability, registered by the rival-provider scenario alone.
const rivalModuleName = "log.rival"

// rivalLoggerModule delivers the logging capability exclusively and states
// that it runs.
//
// It is what the logging module's own stance is measured against: a provider
// that claimed exclusivity and stated StateAuto would stand down in front of
// this one, and the assembly would quietly log through it instead. Stating
// StateEnabled on both sides fails the startup rather than choosing silently.
func rivalLoggerModule() core.Module {
	return core.Module{
		Name:     rivalModuleName,
		Provides: []core.Provision{{Token: (*log.Logger)(nil), Exclusive: true}},
		Prepare: func(_ context.Context, _ *core.Registry) (core.Enablement, error) {
			return core.Enablement{State: core.StateEnabled}, nil
		},
		New: func(_ context.Context, _ *core.Registry) (any, error) {
			return rivalLogger{}, nil
		},
	}
}

// rivalLogger is the rival's product. The startup never gets far enough to
// construct it; it exists because the descriptor has to deliver what it
// declares.
type rivalLogger struct{}

var _ log.Logger = rivalLogger{}

// Named returns a logger that discards everything.
func (rivalLogger) Named(string) *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

// Redaction returns a registration interface that keeps nothing.
func (rivalLogger) Redaction() log.Redaction { return rivalRedaction{} }

// rivalRedaction accepts registrations and drops them.
type rivalRedaction struct{}

func (rivalRedaction) AddKeys(...string)              {}
func (rivalRedaction) AddPattern(string, log.Matcher) {}
