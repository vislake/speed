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
)

// This file is go/integration's SSRF (server-side request forgery) defense
// for outbound webhooks -- the most common security hole in outbound
// webhooks, and a blanket platform rule: "Do not let outbound webhooks
// reach internal addresses. SSRF protection is mandatory, including
// DNS-rebinding protection."
//
// Two checks exist, at two different times, because one check at creation
// time alone is not enough: DNS can change after a subscription is created
// (a URL that resolved to a public IP when configured can be repointed at
// an internal address minutes later -- "DNS rebinding"), so a check that
// only ran once, at Service.CreateWebhookSubscription, would be a one-time
// gate an attacker simply waits out.
//
//  1. ValidateWebhookURL runs at subscription creation (and update) time --
//     webhook_service.go's CreateWebhookSubscription/UpdateWebhookSubscription
//     call it before any row is written. This is the cheap, early refusal:
//     an obviously-internal URL is rejected before it is ever stored,
//     without waiting for the first delivery attempt to discover the
//     problem.
//  2. newSafeHTTPClient's Transport re-validates the CONNECTING address at
//     DIAL time, on every delivery attempt (webhook_delivery.go). This is
//     what defeats DNS rebinding: the resolution used to decide whether to
//     connect is the exact same resolution used to actually connect (the
//     dialer connects to the specific IP it just validated, by address, not
//     by re-resolving the hostname a second time after the check), so there
//     is no window between "looks safe" and "is used" for an attacker's DNS
//     server to switch the answer. It also automatically covers redirects:
//     Go's http.Client issues a fresh Transport.RoundTrip for a redirect's
//     Location, which dials again through this same guarded DialContext, so
//     a webhook receiver cannot 302 a delivery into an internal address
//     either.
//
// ErrWebhookURLInvalid, ErrWebhookURLBlocked and ErrWebhookURLUnresolvable
// are declared in errors.go, alongside this module's other error codes;
// this file only implements the checks that raise them.

// allowedWebhookSchemes is the closed set of URL schemes a webhook
// subscription may use. Both http and https are allowed -- the design doc
// does not mandate TLS, and a tenant's own internal test receiver may
// legitimately run plain HTTP -- but nothing outside these two is ever
// dialed.
var allowedWebhookSchemes = map[string]bool{"http": true, "https": true}

// blockedIPv4CIDRs are additional IPv4 ranges isBlockedIP checks beyond what
// net.IP's own IsLoopback/IsPrivate/IsLinkLocalUnicast/IsUnspecified already
// cover (which together already include RFC 1918 10/8, 172.16/12,
// 192.168/16, RFC 3927 169.254/16, and 127/8):
//
//   - 100.64.0.0/10 -- RFC 6598 carrier-grade NAT shared address space, not
//     private range in Go's own classification but still never a
//     legitimate public webhook receiver.
var blockedIPv4CIDRs = mustParseCIDRs("100.64.0.0/10")

// blockedIPv6CIDRs are additional IPv6 ranges isBlockedIP checks beyond what
// net.IP's own classification already covers, the IPv6 twin of the IPv4 gap
// blockedIPv4CIDRs exists for. Ranges the stdlib ALREADY refuses on the IPv6
// side are deliberately absent here -- do not add them back, and do not read
// this list's existence as license to drop a stdlib call as "covered":
//
//   - ::1/128 and ::/128 -- loopback and unspecified (IsLoopback/
//     IsUnspecified);
//   - fe80::/10 and ff00::/8 -- link-local unicast and multicast
//     (IsLinkLocalUnicast/IsLinkLocalMulticast/IsMulticast);
//   - fc00::/7 -- RFC 4193 unique-local (IsPrivate);
//   - ::ffff:0:0/96 -- v4-mapped addresses are refused through the embedded
//     IPv4 net.IP.To4 exposes, so ::ffff:127.0.0.1 is caught by the same
//     loopback test as 127.0.0.1 itself and ::ffff:169.254.169.254 by the
//     same link-local one -- neither form needs (or would be reached by) an
//     entry here;
//   - 169.254.169.254 and the CGNAT range -- IsLinkLocalUnicast and
//     blockedIPv4CIDRs respectively, listed on the IPv4 side only.
//
// What this list adds is IPv6 ranges the stdlib leaves unclassified (Go
// treats them as ordinary global unicast) that are still never a legitimate
// public webhook receiver:
//
//   - 64:ff9b::/96 -- RFC 6052's well-known NAT64 prefix. A NAT64 network
//     reaches IPv4 destinations in this form, so a hostile DNS answer can
//     present an internal IPv4 destination (metadata endpoint, loopback)
//     here with the stdlib seeing only a "public" address. The whole prefix
//     is refused rather than decoding each embedded IPv4, which would mean
//     duplicating every IPv4 rule above in IPv6 form. This module's SSRF
//     checks have no production-host relaxation (the only overrides,
//     WithWebhookHTTPClient and WithWebhookURLValidator, are test/demo
//     seams -- see their doc comments), so a deployment that genuinely
//     reached public receivers only through NAT64 translation would see a
//     refused delivery, never a silent bypass -- the conservative direction
//     for a security check.
//   - ::/96 -- RFC 4291's IPv4-compatible addresses, deprecated but still
//     parseable: ::127.0.0.1 is loopback in a form To4 does not map. (The
//     prefix's own ::/128 and ::1/128 heads are already refused by the
//     stdlib calls above; listing the /96 additionally covering them
//     changes nothing.)
//   - fec0::/10 -- RFC 3879-deprecated site-local, never assigned and never
//     a legitimate receiver.
var blockedIPv6CIDRs = mustParseCIDRs("64:ff9b::/96", "::/96", "fec0::/10")

func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(fmt.Sprintf("integration: invalid blocked CIDR literal %q: %v", c, err))
		}
		nets = append(nets, n)
	}
	return nets
}

// isBlockedIP reports whether ip must never be dialed as a webhook
// destination: loopback (127.0.0.0/8, ::1), link-local unicast or multicast
// (169.254.0.0/16, fe80::/10), unspecified (0.0.0.0, ::), private
// (RFC 1918, RFC 4193 fc00::/7 -- both covered by net.IP.IsPrivate since Go
// 1.17), multicast, or any supplementary range the blockedIPv4CIDRs and
// blockedIPv6CIDRs lists add beyond what net.IP classifies -- carrier-grade
// NAT (100.64.0.0/10) on the IPv4 side, and the IPv6 special-purpose ranges
// the stdlib misses on the IPv6 side (NAT64, IPv4-compatible, site-local;
// see blockedIPv6CIDRs's own comment for the full boundary).
func isBlockedIP(ip net.IP) bool {
	switch {
	case ip.IsLoopback(),
		ip.IsLinkLocalUnicast(),
		ip.IsLinkLocalMulticast(),
		ip.IsUnspecified(),
		ip.IsPrivate(),
		ip.IsMulticast():
		return true
	}
	for _, n := range blockedIPv4CIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	for _, n := range blockedIPv6CIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ValidateWebhookURL refuses a webhook subscription URL that is malformed,
// uses a scheme other than http/https, or resolves to any address
// isBlockedIP rejects. See this file's own header comment for why this is
// only the FIRST of two checks -- the second runs at every delivery attempt,
// through newSafeHTTPClient's transport.
//
// Every one of ip's resolved addresses is checked, not just the first: a
// hostname resolving to both a public and a private address (a
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
	if !allowedWebhookSchemes[u.Scheme] {
		return ErrWebhookURLInvalid.WithParam("reason", "scheme").WithParam("scheme", u.Scheme)
	}

	host := u.Hostname()
	if host == "" {
		return ErrWebhookURLInvalid.WithParam("reason", "missing_host")
	}

	// A literal IP address needs no DNS resolution -- net.ParseIP succeeds
	// directly and LookupIPAddr would just echo it back.
	if literal := net.ParseIP(host); literal != nil {
		if isBlockedIP(literal) {
			return ErrWebhookURLBlocked.WithParam("ip", literal.String())
		}
		return nil
	}

	resolveCtx, cancel := context.WithTimeout(ctx, webhookURLValidationTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(resolveCtx, host)
	if err != nil {
		return ErrWebhookURLUnresolvable.WithParam("host", host).WithCause(err)
	}
	if len(addrs) == 0 {
		return ErrWebhookURLUnresolvable.WithParam("host", host)
	}
	for _, addr := range addrs {
		if isBlockedIP(addr.IP) {
			// Deliberately no WithParam("ip", addr.IP.String()) here: this
			// path reached the blocked address through DNS, so the resolved
			// address is information the caller does not have, and echoing
			// it back would turn the refusal into an internal-DNS
			// reconnaissance oracle -- submit hostnames, read back the
			// internal IPs they resolve to. The literal-IP branch above
			// (line 135) keeps its ip param, because there the caller typed
			// the address itself and the echo discloses nothing. The coded
			// error plus the generic webhook_url_blocked text -- which names
			// no address -- is the honest answer shape for this path. See
			// ErrWebhookURLBlocked's own doc comment (errors.go).
			return ErrWebhookURLBlocked
		}
	}
	return nil
}

// webhookURLValidationTimeout bounds ValidateWebhookURL's DNS lookup, so a
// slow or unresponsive resolver cannot hang a subscription-creation
// request.
const webhookURLValidationTimeout = 5 * time.Second

// webhookDialTimeout bounds one dial attempt inside newSafeHTTPClient's
// transport.
const webhookDialTimeout = 5 * time.Second

// errBlockedDialAddress is wrapped into the error newSafeHTTPClient's
// DialContext returns when every resolved address for a dial is blocked, so
// a caller inspecting the delivery failure (webhook_delivery.go's Handle)
// can tell a blocked-destination failure apart from an ordinary network
// failure if it ever needs to.
var errBlockedDialAddress = errors.New("integration: webhook delivery refused: destination resolves to a blocked address")

// webhookDialFunc performs the one TCP dial newSafeHTTPClient's transport
// issues for a single validated candidate address. It is a package-level
// function variable, not a private method, so a test can replace it and
// observe exactly what the guard hands to the dialer -- the seam the
// dial-time pinning property is asserted through: the dialed address must
// be the validated candidate's IP LITERAL, never the original hostname, or
// the rebinding window this file's header comment closes would be open (a
// re-resolution inside the dial would let a rebinding DNS answer steer the
// connection). The default implementation builds a fresh dialer per
// attempt, carrying webhookDialTimeout. This is the twin of
// go/ai-gateway/ssrf.go's providerDialFunc.
var webhookDialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: webhookDialTimeout}
	return dialer.DialContext(ctx, network, addr)
}

// resolveWebhookHost resolves host the way newSafeHTTPClient's transport
// needs it resolved at dial time. It is a package-level function variable
// for the same reason webhookDialFunc is: the dial-time pinning property
// cannot be exercised against the real resolver in an offline test, so the
// rebinding sequence (an answer that changes between resolutions) is
// scripted through this seam -- see ssrf_test.go's
// TestNewSafeHTTPClient_DialsTheValidatedIPLiteral.
var resolveWebhookHost = net.DefaultResolver.LookupIPAddr

// webhookTransportIdleTimeout is the IdleConnTimeout of the shared
// transport newSafeHTTPClient builds: how long an idle keep-alive
// connection to a receiver is kept for reuse before it is closed. Sane and
// finite on purpose -- a zero IdleConnTimeout would keep an idle
// connection to a dead or changed receiver around forever (Go's own
// default), while the shared transport this module lives on means one
// receiver's abandoned connection is not this module's problem to keep
// warm indefinitely.
const webhookTransportIdleTimeout = 90 * time.Second

// defaultWebhookHTTPClient is the module-level http.Client every webhook
// delivery attempt sends through when the Service has no
// WithWebhookHTTPClient override -- built ONCE at package init, never per
// delivery. See newSafeHTTPClient for what its transport does; the
// built-once shape is what lets consecutive deliveries to the same
// receiver reuse one TCP connection (and its TLS session) instead of
// dialing afresh for every attempt -- http.Transport is safe for
// concurrent use by design, and each delivery attempt's own per-request
// timeout (http.Client.Timeout) still bounds that attempt individually.
// webhookTransportIdleTimeout above keeps the shared pool from holding
// idle connections open indefinitely.
var defaultWebhookHTTPClient = newSafeHTTPClient(webhookDeliveryTimeout)

// newSafeHTTPClient returns the http.Client webhook delivery attempts send
// through (webhook_delivery.go, via defaultWebhookHTTPClient). Its
// Transport re-validates the destination at DIAL time -- see this file's
// own header comment for why that is what actually defeats DNS rebinding,
// which a creation-time-only check cannot.
//
// CheckRedirect caps the redirect chain at maxWebhookRedirects: an
// unbounded chain from an untrusted receiver is both a resource-exhaustion
// surface and, per Go's default of 10, more hops than any legitimate
// webhook receiver needs.
func newSafeHTTPClient(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		// Sane idle-connection hygiene for the module-level shared
		// transport (defaultWebhookHTTPClient): IdleConnTimeout bounds how
		// long a receiver's idle keep-alive connection is kept, and
		// MaxIdleConnsPerHost caps how many concurrent deliveries to one
		// receiver may keep open. Both stay above the concurrency a single
		// tenant's fan-out realistically produces; see
		// webhookTransportIdleTimeout's own comment.
		IdleConnTimeout:     webhookTransportIdleTimeout,
		MaxIdleConnsPerHost: 8,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}

			var candidates []net.IP
			if literal := net.ParseIP(host); literal != nil {
				candidates = []net.IP{literal}
			} else {
				resolved, err := resolveWebhookHost(ctx, host)
				if err != nil {
					return nil, err
				}
				for _, r := range resolved {
					candidates = append(candidates, r.IP)
				}
			}

			var lastErr error
			for _, ip := range candidates {
				if isBlockedIP(ip) {
					// The resolved address must NOT ride in the refusal text
					// this transport returns: that text is persisted into the
					// delivery row's last_error (webhook_delivery.go) and
					// served back to the tenant through the delivery-log API,
					// so echoing the resolution here would hand a tenant admin
					// the identical internal-DNS reconnaissance oracle
					// errors.go's own ErrWebhookURLBlocked comment rules out
					// of the creation-time answer -- submit hostnames, read
					// back the internal addresses they resolve to. The host
					// (the caller's own) stays in the text; the IP detail
					// belongs in the server-side log below, never in
					// LastError.
					obs.FromContext(ctx).Warn("integration refused a webhook dial to a blocked address",
						"host", host, "ip", ip.String())
					lastErr = fmt.Errorf("%w: %s", errBlockedDialAddress, host)
					continue
				}
				// Dial the validated IP directly (not the original
				// hostname), so nothing in between this check and the
				// connection performs a second, independent DNS lookup
				// that a rebinding attacker could answer differently.
				// The dial itself goes through webhookDialFunc so a test
				// can assert this property -- see that seam's own comment.
				conn, err := webhookDialFunc(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			if lastErr == nil {
				lastErr = fmt.Errorf("integration: no address resolved for %s", host)
			}
			return nil, lastErr
		},
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxWebhookRedirects {
				return fmt.Errorf("integration: webhook delivery: stopped after %d redirects", maxWebhookRedirects)
			}
			return nil
		},
	}
}

// maxWebhookRedirects bounds the redirect chain newSafeHTTPClient follows.
const maxWebhookRedirects = 3
