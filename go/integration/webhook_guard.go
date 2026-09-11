package integration

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore/safehttp"
)

// This file is go/integration's caller-side layer over pkgcore/safehttp --
// the shared outbound-request guard -- for outbound webhooks. The guard
// itself, and the DNS-rebinding rationale its package doc records, live in
// go/pkgcore/safehttp; what stays here is everything webhook-specific:
//
//  1. ValidateWebhookURL runs at subscription creation (and update) time --
//     webhook_service.go's CreateWebhookSubscription/UpdateWebhookSubscription
//     call it before any row is written. This is the cheap, early refusal,
//     plus this module's own error vocabulary: the coded errors in errors.go
//     with their reason params, and the deliberate no-IP-echo asymmetry
//     ErrWebhookURLBlocked's comment pins (an address the caller typed is
//     echoed back; an address reached through DNS resolution never is). It
//     also bounds the guard's DNS lookup, so a slow or unresponsive resolver
//     cannot hang a subscription-creation request.
//  2. newSafeHTTPClient builds the client every delivery attempt sends
//     through (webhook_delivery.go). The guard refuses to connect to any
//     non-public address at DIAL time -- the property that defeats DNS
//     rebinding, since the check runs on the exact address about to be
//     dialled -- and re-checks every redirect hop's scheme before following
//     it. On top of that, this layer replaces the dial refusal's TEXT before
//     it leaves the client: that text is persisted into the delivery row's
//     last_error and served back to the tenant through the delivery-log API,
//     so it must stay identifiable as a blocked-destination refusal while
//     naming no resolved address.
//
// ErrWebhookURLInvalid, ErrWebhookURLBlocked and ErrWebhookURLUnresolvable
// are declared in errors.go, alongside this module's other error codes;
// this file only implements the checks that raise them.

// webhookURLGuard is the shared guard as webhook destinations need it:
// exactly http and https -- the design doc does not mandate TLS, and a
// tenant's own internal test receiver may legitimately run plain HTTP -- and
// nothing else. newSafeHTTPClient's client carries the same list.
var webhookURLGuard = safehttp.NewGuard(safehttp.WithAllowedSchemes("http", "https"))

// webhookURLValidationTimeout bounds ValidateWebhookURL's DNS lookup, so a
// slow or unresponsive resolver cannot hang a subscription-creation request.
const webhookURLValidationTimeout = 5 * time.Second

// ValidateWebhookURL refuses a webhook subscription URL that is malformed,
// uses a scheme other than http/https, or resolves to any address the shared
// guard blocks. The guard's blocked set -- loopback, private, link-local,
// CGNAT, multicast and the IPv6 special-purpose ranges, all spelled out in
// pkgcore/safehttp -- is the one authority for what counts as blocked, and
// the dial-time half of the defense re-checks every address a delivery
// actually connects to (newSafeHTTPClient). See this file's own header
// comment for what this layer adds around it.
//
// Every one of the host's resolved addresses is checked, not just the first:
// a hostname resolving to both a public and a private address (a
// misconfigured split-horizon DNS, or a deliberate attacker setup) is
// refused on the private answer alone, since a caller cannot control which
// address a later dial actually picks.
func ValidateWebhookURL(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ErrWebhookURLInvalid.WithParam("reason", "unparseable").WithCause(err)
	}
	if u.Host == "" {
		return ErrWebhookURLInvalid.WithParam("reason", "missing_host")
	}
	host := u.Hostname()
	if host == "" {
		return ErrWebhookURLInvalid.WithParam("reason", "missing_host")
	}

	// The scheme gate runs through the guard, so the allowed-scheme list has
	// exactly one authority (webhookURLGuard's own options). The parse above
	// has already accepted rawURL, so the only refusal the gate can raise
	// here is the scheme one; it is mapped into this module's own reason
	// vocabulary.
	if _, err := webhookURLGuard.CheckScheme(rawURL); err != nil {
		return ErrWebhookURLInvalid.WithParam("reason", "scheme").WithParam("scheme", u.Scheme).WithCause(err)
	}

	// Which branch the refusal came from is what keeps the ip param
	// asymmetric: an address the caller typed into the URL is an echo of
	// what the caller already knows, while a DNS answer is information the
	// caller does not have -- for a name resolvable only inside the
	// platform's own network it is exactly the reconnaissance answer an
	// internal-DNS oracle would give (errors.go's ErrWebhookURLBlocked
	// comment).
	literal := net.ParseIP(host)

	resolveCtx, cancel := context.WithTimeout(ctx, webhookURLValidationTimeout)
	defer cancel()
	if _, err := webhookURLGuard.ValidateURL(resolveCtx, rawURL); err != nil {
		switch {
		case errors.Is(err, safehttp.ErrBlockedAddress):
			if literal != nil {
				return ErrWebhookURLBlocked.WithParam("ip", literal.String())
			}
			return ErrWebhookURLBlocked
		case errors.Is(err, safehttp.ErrUnresolvable):
			return ErrWebhookURLUnresolvable.WithParam("host", host).WithCause(err)
		default:
			return ErrWebhookURLInvalid.WithCause(err)
		}
	}
	return nil
}

// errBlockedDialAddress is wrapped into the error newSafeHTTPClient's client
// returns when a delivery's dial is refused because the destination resolves
// to a blocked address, so a caller inspecting the delivery failure
// (webhook_delivery.go's attemptDelivery) can tell a blocked-destination
// failure apart from an ordinary network failure.
var errBlockedDialAddress = errors.New("integration: webhook delivery refused: destination resolves to a blocked address")

// defaultWebhookHTTPClient is the module-level http.Client every webhook
// delivery attempt sends through when the Service has no
// WithWebhookHTTPClient override -- built ONCE at package init, never per
// delivery. Consecutive deliveries to the same receiver therefore reuse one
// TCP connection (and its TLS session) instead of dialing afresh for every
// attempt; http.Transport is safe for concurrent use by design, and each
// delivery attempt's own per-request timeout (http.Client.Timeout) still
// bounds that attempt individually.
var defaultWebhookHTTPClient = newSafeHTTPClient(webhookDeliveryTimeout)

// newSafeHTTPClient returns the http.Client webhook delivery attempts send
// through (webhook_delivery.go, via defaultWebhookHTTPClient): the shared
// guard's own client, so every connection -- every redirect hop included --
// passes the connect-time address check and the per-hop scheme re-check. It
// carries the delivery timeout as the client's own overall bound.
//
// The dial refusal's text is then replaced at the one seam this layer owns:
// the guard's refusal names the address it refused, and that text would be
// persisted into the delivery row's last_error and served back to the
// tenant through the delivery-log API -- an internal-DNS reconnaissance
// oracle (submit hostnames, read back the internal addresses they resolve
// to). The replacement names the host the caller configured instead and
// stays identifiable as the blocked-destination refusal; the underlying
// error, resolved address included, goes to the server-side log, which is
// where that detail belongs.
func newSafeHTTPClient(timeout time.Duration) *http.Client {
	client := safehttp.NewGuard(
		safehttp.WithAllowedSchemes("http", "https"),
		safehttp.WithTimeout(timeout),
	).Client()

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		// Guard.Client always builds an *http.Transport; reaching this means
		// that contract changed, and continuing without the text replacement
		// would leak resolved addresses into tenant-visible delivery rows.
		panic(fmt.Sprintf("integration: guarded client transport is %T, want *http.Transport", client.Transport))
	}
	innerDial := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := innerDial(ctx, network, addr)
		if err != nil && errors.Is(err, safehttp.ErrBlockedAddress) {
			// The dial address here is the transport's own target, so its
			// host is the hostname the subscription named -- never a
			// resolved literal, which is exactly what must not ride out.
			host, _, splitErr := net.SplitHostPort(addr)
			if splitErr != nil {
				host = addr
			}
			obs.FromContext(ctx).Warn("integration refused a webhook dial to a blocked address",
				"host", host, "error", err)
			return nil, fmt.Errorf("%w: %s", errBlockedDialAddress, host)
		}
		return conn, err
	}
	return client
}
