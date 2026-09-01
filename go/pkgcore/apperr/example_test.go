package apperr_test

// Runnable documentation for the apperr public API. Every example here is
// compiled and executed by `go test`.

import (
	"errors"
	"fmt"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// ExampleError shows the six constructors and the HTTP status each suggests.
// The code is the contract; it is never a human-readable message, because the
// client resolves it through its own i18n catalog.
func ExampleError() {
	for _, err := range []*apperr.Error{
		apperr.Invalid("org.name_too_long"),
		apperr.Unauthorized("authn.token_expired"),
		apperr.Forbidden("rbac.permission_denied"),
		apperr.NotFound("billing.subscription_not_found"),
		apperr.Conflict("org.slug_taken"),
		apperr.Internal("billing.provider_unavailable"),
	} {
		fmt.Println(err.Status, err.Code)
	}

	// Output:
	// 400 org.name_too_long
	// 401 authn.token_expired
	// 403 rbac.permission_denied
	// 404 billing.subscription_not_found
	// 409 org.slug_taken
	// 500 billing.provider_unavailable
}

// ExampleNotFound shows the usual construction: a stable code plus structured
// parameters the client interpolates into its own translated message.
func ExampleNotFound() {
	err := apperr.NotFound("billing.subscription_not_found").
		WithParam("id", "sub_42").
		WithParam("tenant", "acme")

	fmt.Println(err)
	fmt.Println(err.Status, err.Params["id"], err.Params["tenant"])

	// Output:
	// billing.subscription_not_found
	// 404 sub_42 acme
}

// ExampleError_WithParam shows that decorating derives a new error and leaves
// the receiver untouched, so a package-level error can be shared and decorated
// per request without one request's parameters leaking into another's.
func ExampleError_WithParam() {
	// A shared, package-level error value.
	errSubscriptionNotFound := apperr.NotFound("billing.subscription_not_found")

	first := errSubscriptionNotFound.WithParam("id", "sub_1")
	second := errSubscriptionNotFound.WithParam("id", "sub_2")

	fmt.Println(first.Params["id"], second.Params["id"])
	fmt.Println(errSubscriptionNotFound.Params == nil)

	// Output:
	// sub_1 sub_2
	// true
}

// ExampleError_WithCause shows a cause being attached. It stays reachable
// through errors.Is and errors.As, and never reaches the API response.
func ExampleError_WithCause() {
	cause := errors.New("dial tcp 10.0.0.7:5432: connection refused")
	err := apperr.Internal("billing.provider_unavailable").WithCause(cause)

	fmt.Println(err)
	fmt.Println(errors.Is(err, cause))

	// Output:
	// billing.provider_unavailable: dial tcp 10.0.0.7:5432: connection refused
	// true
}

// ExampleAs shows the transport boundary recovering the structured error from
// a wrapped one, so the handler can map it onto a response.
func ExampleAs() {
	err := fmt.Errorf("charging invoice inv_7: %w",
		apperr.Forbidden("rbac.permission_denied").WithParam("permission", "billing:write"))

	appErr, ok := apperr.As(err)
	if !ok {
		fmt.Println("not an application error")
		return
	}
	fmt.Println(appErr.Status, appErr.Code, appErr.Params["permission"])

	// A plain error is reported as such rather than mapped onto a default code.
	_, ok = apperr.As(errors.New("boom"))
	fmt.Println(ok)

	// Output:
	// 403 rbac.permission_denied billing:write
	// false
}
