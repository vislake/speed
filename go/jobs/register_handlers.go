package jobs

import (
	"errors"
	"fmt"
	"slices"
)

// RegisterHandlers drains decls onto q: every entry — keyed by job type, the
// shape pkgcore.JobHandlerRegistrar.Handlers() exposes and a host-owned
// declaration map is written in — must be a Handler, and each is registered
// through q.RegisterHandler, which keys the queue by the handler's own
// Type(). It is the drain primitive Wire composes with the queue's schema
// bootstrap; a host whose declarations do not live on a pkgcore.Registry
// calls it directly.
//
// Entries register in ascending job-type order, so a map carrying several
// bad entries fails the same way on every run. An entry of any other type is
// a wiring bug between a declaration and the queue contract, refused here —
// naming the job type and the offending Go type — rather than mis-registered
// into a worker at job-claim time. A nil or empty map is a no-op returning
// nil.
//
// Registration is not transactional: handlers registered before a failing
// entry stay registered. A type the queue already holds — a declaration map
// drained twice, or a type a host registered directly before calling this —
// refuses with RegisterHandler's own ErrDuplicateHandlerType (wrapped,
// naming the job type in decls).
func RegisterHandlers(q *StandaloneQueue, decls map[string]any) error {
	if q == nil {
		return errors.New("jobs: RegisterHandlers requires a non-nil *StandaloneQueue")
	}

	jobTypes := make([]string, 0, len(decls))
	for jobType := range decls {
		jobTypes = append(jobTypes, jobType)
	}
	slices.Sort(jobTypes)

	for _, jobType := range jobTypes {
		h, ok := decls[jobType].(Handler)
		if !ok {
			return fmt.Errorf("jobs: job handler %q is a %T, not a jobs.Handler", jobType, decls[jobType])
		}
		if err := q.RegisterHandler(h); err != nil {
			return fmt.Errorf("jobs: register job handler %q: %w", jobType, err)
		}
	}
	return nil
}
