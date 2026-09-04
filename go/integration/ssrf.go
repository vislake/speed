package integration

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// This file is go/integration's SSRF (server-side request forgery) defense
// for outbound webhooks -- docs/internal/07-platform-services.md names it
// explicitly as the most common security hole in outbound webhooks, and the
// root CLAUDE.md's own Security rules
// repeat it as a blanket platform rule: "Do not let outbound webhooks reach
// internal addresses. SSRF protection is mandatory, including DNS-rebinding
// protection."
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
// 1.17), multicast, or carrier-grade NAT (100.64.0.0/10).
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
			return ErrWebhookURLBlocked.WithParam("ip", addr.IP.String())
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

// newSafeHTTPClient returns the http.Client every webhook delivery attempt
// sends through (webhook_delivery.go). Its Transport re-validates the
// destination at DIAL time -- see this file's own header comment for why
// that is what actually defeats DNS rebinding, which a creation-time-only
// check cannot.
//
// CheckRedirect caps the redirect chain at maxWebhookRedirects: an
// unbounded chain from an untrusted receiver is both a resource-exhaustion
// surface and, per Go's default of 10, more hops than any legitimate
// webhook receiver needs.
func newSafeHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: webhookDialTimeout}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}

			var candidates []net.IP
			if literal := net.ParseIP(host); literal != nil {
				candidates = []net.IP{literal}
			} else {
				resolved, err := net.DefaultResolver.LookupIPAddr(ctx, host)
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
					lastErr = fmt.Errorf("%w: %s -> %s", errBlockedDialAddress, host, ip)
					continue
				}
				// Dial the validated IP directly (not the original
				// hostname), so nothing in between this check and the
				// connection performs a second, independent DNS lookup
				// that a rebinding attacker could answer differently.
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
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
