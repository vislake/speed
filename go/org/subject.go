package org

import "net/http"

// SubjectResolver reports the user id of the request's authenticated
// caller.
//
// It is the seam authn fills in a later round -- precedented by config's
// own WithResolver seam for tenant resolution -- and until then a host
// injects its own, or none at all. Every org endpoint that needs a caller
// identity (creating or accepting an invitation) calls it through
// Handler.resolveSubject; an unwired resolver, or one that reports ok=false,
// makes that endpoint return 401 org.subject_unresolved. org never
// invents a default user: failing closed here is what keeps "who did this"
// answerable for every invitation and every acceptance.
//
// Structural nesting note (the same technique as FeatureGate and Scope):
// the signature is built from stdlib types only, so a host's own resolver
// -- eventually backed by authn's verified access-token claims -- satisfies
// this interface without org importing authn or authn importing org.
type SubjectResolver interface {
	// Subject reports r's authenticated caller's user id. ok is false when
	// no caller could be identified, in which case userID is meaningless
	// and must not be used.
	Subject(r *http.Request) (userID string, ok bool)
}
