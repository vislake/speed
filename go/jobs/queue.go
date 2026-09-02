package jobs

import (
	"context"
	"time"
)

// Queue is the contract every deployment mode implements: submit a Task,
// poll a Job's status, ask for cancellation. This is the ONLY portable
// surface a caller outside this package should depend on — StandaloneQueue
// (this package's implementation for the standalone deployment mode)
// exposes additional methods (RegisterHandler, Start, Close) that
// configure and run it, which deliberately do not appear here, because
// the distributed deployment mode's Redis/asynq-backed implementation is
// expected to need a different setup shape of its own.
//
// ctx is never the source of a new Job's own tenant — that is
// Task.TenantID's job, since Enqueue is legitimately called from a context
// with no single ambient tenant — but it does participate in deciding
// which Jobs a caller may see or act on; see Get and Cancel.
type Queue interface {
	// Enqueue validates task (ErrInvalidTask for an empty Type or
	// TenantID) and creates a new Job for it, returning its JobID.
	//
	// A non-empty task.IdempotencyKey makes this call idempotent — see
	// Task.IdempotencyKey's own doc comment. Every other field creates a
	// new, independent Job on every call.
	Enqueue(ctx context.Context, task Task, opts ...EnqueueOption) (JobID, error)

	// Get returns the current status/progress/result/error of the Job
	// identified by id.
	//
	// ctx must carry either a tenant (pkgcore.WithTenant) matching the
	// Job's own TenantID, or a system context (pkgcore.WithSystemContext).
	// Anything else — including a context with no tenant and no system
	// context, or a tenant that does not match — reports ErrJobNotFound,
	// indistinguishable from "no such id", so a caller can never learn
	// from the response alone that a Job exists under a tenant it cannot
	// access.
	Get(ctx context.Context, id JobID) (*Job, error)

	// Cancel marks the Job identified by id as StatusCancelled. It is
	// idempotent: cancelling an already-terminal Job (including one
	// already cancelled) returns nil rather than an error. The same
	// tenant/system-context rule as Get decides whether id is visible to
	// ctx at all; when it is not, Cancel also reports ErrJobNotFound.
	//
	// A Job already StatusRunning when Cancel is called is allowed to keep
	// executing its current Handle call to completion or timeout, but
	// whatever outcome that call reaches is discarded in favor of
	// StatusCancelled — see AGENTS.md's Known limitations.
	Cancel(ctx context.Context, id JobID) error
}

// Package-level defaults applied when the corresponding EnqueueOption is
// not given. Named constants per the backend coding standard's
// configuration rule (§10): stable domain defaults belong in package-level
// constants, not scattered magic numbers.
const (
	// DefaultMaxRetries is how many retries beyond the first attempt a Job
	// gets when Enqueue is called with no WithMaxRetries option.
	DefaultMaxRetries = 3

	// DefaultTimeout bounds a single Handle call when Enqueue is called
	// with no WithTimeout option and the Queue itself was not configured
	// with a different default (StandaloneQueue's WithJobTimeout).
	DefaultTimeout = 5 * time.Minute
)

// enqueueOptions collects what the EnqueueOption functions configure.
// Unexported: callers only ever see the With* constructors below, matching
// this codebase's established functional-options convention (see
// observability.Option).
type enqueueOptions struct {
	priority       Priority
	delay          time.Duration
	scheduledAt    time.Time
	hasScheduledAt bool
	maxRetries     int
	timeout        time.Duration
}

// EnqueueOption configures one Enqueue call. See WithPriority, WithDelay,
// WithScheduledAt, WithMaxRetries and WithTimeout.
type EnqueueOption func(*enqueueOptions)

// WithPriority sets the Job's dispatch priority. Enqueue defaults to
// PriorityNormal when this option is not given.
func WithPriority(p Priority) EnqueueOption {
	return func(o *enqueueOptions) { o.priority = p }
}

// WithDelay makes the Job ineligible to run until d has elapsed from the
// Enqueue call. It is mutually exclusive with WithScheduledAt: whichever is
// passed to Enqueue LAST wins.
func WithDelay(d time.Duration) EnqueueOption {
	return func(o *enqueueOptions) {
		o.delay = d
		o.hasScheduledAt = false
	}
}

// WithScheduledAt makes the Job ineligible to run until the given absolute
// time. It is mutually exclusive with WithDelay: whichever is passed to
// Enqueue LAST wins. A time at or before Enqueue's own call time makes the
// Job eligible immediately, identical to omitting both options.
func WithScheduledAt(t time.Time) EnqueueOption {
	return func(o *enqueueOptions) {
		o.scheduledAt = t
		o.hasScheduledAt = true
	}
}

// WithMaxRetries overrides DefaultMaxRetries for one Job. A negative n is
// clamped to zero (no retries: a single failed attempt dead-letters
// immediately).
func WithMaxRetries(n int) EnqueueOption {
	return func(o *enqueueOptions) { o.maxRetries = n }
}

// WithTimeout overrides the default per-attempt timeout for one Job. A
// non-positive d is ignored (falls back to the Queue's configured default)
// rather than honored literally, since a zero or negative context.
// WithTimeout deadline would expire before the Handler ever ran.
func WithTimeout(d time.Duration) EnqueueOption {
	return func(o *enqueueOptions) { o.timeout = d }
}

// resolveEnqueueOptions applies opts over a set of defaults seeded from
// fallbackTimeout (StandaloneQueue's configured default, itself falling back to
// DefaultTimeout), clamps out-of-range values, and computes the Job's
// initial ScheduledAt from whichever of delay/scheduledAt was given,
// relative to now.
func resolveEnqueueOptions(now time.Time, fallbackTimeout time.Duration, opts []EnqueueOption) enqueueOptions {
	resolved := enqueueOptions{
		priority:   PriorityNormal,
		maxRetries: DefaultMaxRetries,
		timeout:    fallbackTimeout,
	}
	for _, opt := range opts {
		opt(&resolved)
	}
	if resolved.maxRetries < 0 {
		resolved.maxRetries = 0
	}
	if resolved.timeout <= 0 {
		resolved.timeout = fallbackTimeout
	}
	if !resolved.hasScheduledAt {
		resolved.scheduledAt = now.Add(resolved.delay)
	}
	return resolved
}
