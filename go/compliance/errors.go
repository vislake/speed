package compliance

import "github.com/vislake/speed/go/pkgcore/apperr"

// The error index of the compliance module. Every exported error is an
// *apperr.Error builder whose Code follows the <module>.<reason>
// convention the backend coding standard requires: match a decorated
// error with apperr.As(err) and compare its Code, never with == or
// errors.Is against the var below, since WithParam/WithCause derive a new
// *apperr.Error rather than mutating the receiver -- the same convention
// dbkit, tenancy, org, pki and metering already document.
var (
	// ErrQueueRequired is returned by Module.Register when no jobs.Queue
	// was wired through WithQueue. The retention-sweep task handler this
	// module registers on reg.Jobs is meaningless without a queue to
	// drain it, mirroring go/storage's identical ErrQueueRequired
	// reasoning for its own expiry-sweep task.
	ErrQueueRequired = apperr.Internal("compliance.queue_required")

	// ErrTenantListerRequired is returned by RetentionService.
	// SweepAllTenants when no TenantLister was wired through
	// WithTenantLister. A per-tenant SweepTenant call needs no lister at
	// all -- only the "cover every tenant from one scheduled task"
	// convenience does.
	ErrTenantListerRequired = apperr.Internal("compliance.tenant_lister_required")

	// ErrConfigServiceRequired is returned by RetentionService.
	// RetentionWindow when the caller asks it to resolve a tenant
	// override but no *config.Service was wired through WithConfigService.
	// SweepTenant itself never returns this: it falls back to
	// defaultRetentionWindow instead of failing when no config service is
	// wired, exactly the same "optional wiring, honest fallback" shape
	// go/storage's LifecycleService gives a nil ObjectStore. Callers that
	// want RetentionWindow's own error instead of the fallback ask for it
	// directly.
	ErrConfigServiceRequired = apperr.Internal("compliance.config_service_required")

	// ErrAuditRecordFailed wraps a failed dbkit/audit.Emit call for a
	// sweep, an erasure or an export. Per docs/internal/10-compliance-and-
	// audit.md's rule that collection is asynchronous but a failed audit
	// write must alert rather than silently drop, a failed audit write is
	// never swallowed: the underlying
	// deletion or export already happened (and, for a HardDelete-backed
	// sweep or erasure, cannot be undone), so this error is returned
	// alongside the operation's own result rather than in place of it --
	// callers must check for it explicitly to alert an operator that the
	// operation completed with no audit record, rather than assuming a
	// non-nil error alone means nothing happened.
	ErrAuditRecordFailed = apperr.Internal("compliance.audit_record_failed")

	// ErrAuditQueryRequiresSystemContext is returned by AuditQuery.
	// QueryAcrossTenants when ctx carries no system context. A cross-
	// tenant audit read is exactly the kind of platform-admin operation
	// pkgcore.WithSystemContext exists to gate and audit -- see
	// AuditQuery's own doc comment.
	ErrAuditQueryRequiresSystemContext = apperr.Invalid("compliance.audit_query_requires_system_context")

	// ErrEmptySubjectRef is returned by ErasureService.Erase when the
	// given pkgcore.SubjectRef has an empty TenantID or SubjectID -- an
	// erasure request naming nothing to erase, or nothing to scope it to,
	// is a caller bug, not a legal no-op.
	ErrEmptySubjectRef = apperr.Invalid("compliance.empty_subject_ref")

	// ErrSweepPartialFailure is returned by RetentionService.SweepTenant,
	// alongside the full SweepResult, when at least one participant's
	// Sweep callback failed. A caller that only checks "err != nil" still
	// learns a sweep needs attention; SweepResult.Errors names which
	// participant.
	ErrSweepPartialFailure = apperr.Internal("compliance.sweep_partial_failure")

	// ErrErasurePartialFailure is returned by ErasureService.Erase,
	// alongside the full ErasureResult, when at least one participant's
	// Erase callback failed. See Erase's own doc comment for the retry
	// semantics this implies.
	ErrErasurePartialFailure = apperr.Internal("compliance.erasure_partial_failure")

	// ErrExportPartialFailure is returned by ExportService.Export,
	// alongside the manifest built so far, when at least one
	// participant's Export callback failed.
	ErrExportPartialFailure = apperr.Internal("compliance.export_partial_failure")
)

// hasCode reports whether err is (or wraps, via apperr.As's Unwrap chain
// walk) an *apperr.Error whose Code equals code. This is the standard way
// this codebase compares against an apperr sentinel once it may have been
// decorated with WithParam/WithCause -- see go/org/tree.go's and
// go/metering/errors.go's identical helper.
func hasCode(err error, code string) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == code
}
