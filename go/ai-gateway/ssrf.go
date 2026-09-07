package aigateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// This file is go/ai-gateway's SSRF (server-side request forgery) defense
// for the base URL of a tenant BYOK credential -- the URL a tenant admin
// supplies over the credential HTTP surface (handler.go's
// AiGatewaySetTenantCredential) and Gateway.Chat / Gateway.ChatStream /
// Gateway.GenerateImage then dial from the platform's own network,
// presenting the credential's API key. The primitive is exactly the one
// docs/internal/07-platform-services.md names as the most common security
// hole in outbound HTTP, and root CLAUDE.md's own Security rules repeat as
// a blanket platform rule -- "Do not let outbound webhooks reach internal
// addresses. SSRF protection is mandatory, including DNS-rebinding
// protection." -- applied to the one other tenant-influenced outbound dial
// this codebase ships: webhooks are not the only destination a tenant can
// steer.
//
// The defense deliberately mirrors go/integration/ssrf.go's shape and its
// tests. ai-gateway sits on the same dependency tier as integration, so
// the two modules cannot import each other's helpers; duplicating the
// small, stdlib-only pure functions (the blocked-CIDR lists, isBlockedIP)
// is cheaper than a shared lower-tier package, and keeping the two
// implementations byte-for-byte equivalent in behavior is what the
// ErrBaseURLBlocked asymmetry comment in errors.go demands of any later
// consistency round.
//
// # Scope boundary: the tenant-scope write is the SSRF surface
//
// Only the TENANT tier of the credential table is validated -- at write
// time by SetTenantCredential and at dial time by Gateway.resolve /
// resolveImage -- because only that tier is tenant-influenceable. The
// platform-wide row is written by the operator under an audited system
// context (SetPlatformCredential; PermissionManagePlatform at the HTTP
// layer), and an OpenAI-compatible LLM gateway on the operator's own
// intranet is a legitimate platform default -- refusing private
// destinations there would break exactly that deployment. A tenant can
// neither write nor read the platform row, so it cannot steer the platform
// dial either. The reference app's own boot-time platform credential and
// its test harness live on this trusted side of the boundary.
//
// Two checks exist, at two different times, because one check at creation
// time alone is not enough: a tenant controls the DNS of the hostname it
// writes. A base URL that resolved to a public address when it was stored
// can be repointed at an internal address minutes later -- "DNS
// rebinding" -- so a check that only ran once, at SetTenantCredential,
// would be a one-time gate an attacker simply waits out.
//
//  1. ValidateBaseURL runs at credential-write time, from
//     SetTenantCredential before any row is written (credential.go). This
//     is the cheap, early refusal: an obviously-internal URL is rejected
//     before it is ever stored, without waiting for the first chat call to
//     discover the problem.
//  2. A pinned dial-time re-check runs on every provider call a tenant-
//     scope credential resolves (Gateway.resolve / resolveImage swap the
//     provider's HTTP client for guardedProviderHTTPClient). Its Transport
//     re-validates the CONNECTING address at DIAL time, which is what
//     defeats DNS rebinding: the resolution used to decide whether to
//     connect is the exact same resolution used to actually connect (the
//     dialer connects to the specific IP it just validated, by address,
//     not by re-resolving the hostname a second time after the check), so
//     there is no window between "looks safe" and "is used" for a
//     rebinding attacker's DNS answer to switch. It also automatically
//     covers redirects: Go's http.Client issues a fresh
//     Transport.RoundTrip for a redirect's Location, which dials again
//     through this same guarded DialContext, so a vendor cannot 302 a call
//     into an internal address either.
//
// The dial-time pin only applies to providers this module controls --
// its two OpenAI-compatible built-ins, which satisfy httpClientSettable.
// A third-party provider subpackage registered into ChatProviderRegistry
// or ImageProviderRegistry builds its own HTTP client and cannot carry
// the guarded one, so the tenant-tier-plus-unguardable-provider
// combination is refused at resolve time with ErrProviderNotSSRFGuardable
// (guardTenantScopeDial) instead of silently dialing unguarded -- see
// that function's own doc comment and errors.go.
//
// ErrBaseURLInvalid, ErrBaseURLBlocked and ErrBaseURLUnresolvable are
// declared in errors.go, alongside this module's other error codes; this
// file only implements the checks that raise them.

// allowedProviderSchemes is the closed set of URL schemes a tenant BYOK
// credential's base URL may use. Both http and https are allowed -- a
// vendor's plain-HTTP test endpoint may legitimately be named -- but
// nothing outside these two is ever dialed.
var allowedProviderSchemes = map[string]bool{"http": true, "https": true}

// blockedIPv4CIDRs are additional IPv4 ranges isBlockedIP checks beyond
// what net.IP's own IsLoopback/IsPrivate/IsLinkLocalUnicast/IsUnspecified
// already cover (which together already include RFC 1918 10/8, 172.16/12,
// 192.168/16, RFC 3927 169.254/16, and 127/8):
//
//   - 100.64.0.0/10 -- RFC 6598 carrier-grade NAT shared address space, not
//     private range in Go's own classification but still never a
//     legitimate public OpenAI-compatible endpoint.
var blockedIPv4CIDRs = mustParseCIDRs("100.64.0.0/10")

// blockedIPv6CIDRs are additional IPv6 ranges isBlockedIP checks beyond
// what net.IP's own classification already covers, the IPv6 twin of the
// IPv4 gap blockedIPv4CIDRs exists for. Ranges the stdlib ALREADY refuses
// on the IPv6 side are deliberately absent here -- do not add them back,
// and do not read this list's existence as license to drop a stdlib call
// as "covered":
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
// treats them as ordinary global unicast) that are still never a
// legitimate public vendor endpoint:
//
//   - 64:ff9b::/96 -- RFC 6052's well-known NAT64 prefix. A NAT64 network
//     reaches IPv4 destinations in this form, so a hostile DNS answer can
//     present an internal IPv4 destination (metadata endpoint, loopback)
//     here with the stdlib seeing only a "public" address. The whole prefix
//     is refused rather than decoding each embedded IPv4, which would mean
//     duplicating every IPv4 rule above in IPv6 form. This module's SSRF
//     checks have no production-host relaxation, so a deployment that
//     genuinely reached public vendors only through NAT64 translation
//     would see a refused call, never a silent bypass -- the conservative
//     direction for a security check.
//   - ::/96 -- RFC 4291's IPv4-compatible addresses, deprecated but still
//     parseable: ::127.0.0.1 is loopback in a form To4 does not map. (The
//     prefix's own ::/128 and ::1/128 heads are already refused by the
//     stdlib calls above; listing the /96 additionally covering them
//     changes nothing.)
//   - fec0::/10 -- RFC 3879-deprecated site-local, never assigned and never
//     a legitimate endpoint.
var blockedIPv6CIDRs = mustParseCIDRs("64:ff9b::/96", "::/96", "fec0::/10")

func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(fmt.Sprintf("aigateway: invalid blocked CIDR literal %q: %v", c, err))
		}
		nets = append(nets, n)
	}
	return nets
}

// isBlockedIP reports whether ip must never be dialed as a tenant BYOK
// credential's vendor endpoint: loopback (127.0.0.0/8, ::1), link-local
// unicast or multicast (169.254.0.0/16, fe80::/10), unspecified (0.0.0.0,
// ::), private (RFC 1918, RFC 4193 fc00::/7 -- both covered by
// net.IP.IsPrivate since Go 1.17), multicast, or any supplementary range
// the blockedIPv4CIDRs and blockedIPv6CIDRs lists add beyond what net.IP
// classifies -- carrier-grade NAT (100.64.0.0/10) on the IPv4 side, and
// the IPv6 special-purpose ranges the stdlib misses on the IPv6 side
// (NAT64, IPv4-compatible, site-local; see blockedIPv6CIDRs's own comment
// for the full boundary). It is the identical predicate go/integration's
// ssrf.go applies to webhook destinations, duplicated here per this
// file's own header comment.
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

// ValidateBaseURL refuses a tenant BYOK credential base URL that is
// malformed, uses a scheme other than http/https, or resolves to any
// address isBlockedIP rejects. It is called by SetTenantCredential before
// any row is written -- see this file's own header comment for why that is
// only the FIRST of two checks, and for the scope boundary that keeps the
// platform-scope write off this validator entirely. The empty string is
// not this function's business: a credential with no base URL configured
// is legal to store (credentialRow.BaseURL's doc comment), so the caller
// skips validation for it rather than passing it here.
//
// Every one of the host's resolved addresses is checked, not just the
// first: a hostname resolving to both a public and a private address (a
// misconfigured split-horizon DNS, or a deliberate attacker setup) is
// refused on the private answer alone, since a caller cannot control which
// address a later dial actually picks.
func ValidateBaseURL(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ErrBaseURLInvalid.WithParam("reason", "unparseable").WithCause(err)
	}
	if u.Host == "" {
		return ErrBaseURLInvalid.WithParam("reason", "missing_host")
	}
	if !allowedProviderSchemes[u.Scheme] {
		return ErrBaseURLInvalid.WithParam("reason", "scheme").WithParam("scheme", u.Scheme)
	}

	host := u.Hostname()
	if host == "" {
		return ErrBaseURLInvalid.WithParam("reason", "missing_host")
	}

	// A literal IP address needs no DNS resolution -- net.ParseIP succeeds
	// directly and LookupIPAddr would just echo it back.
	if literal := net.ParseIP(host); literal != nil {
		if isBlockedIP(literal) {
			return ErrBaseURLBlocked.WithParam("ip", literal.String())
		}
		return nil
	}

	resolveCtx, cancel := context.WithTimeout(ctx, baseURLValidationTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(resolveCtx, host)
	if err != nil {
		return ErrBaseURLUnresolvable.WithParam("host", host).WithCause(err)
	}
	if len(addrs) == 0 {
		return ErrBaseURLUnresolvable.WithParam("host", host)
	}
	for _, addr := range addrs {
		if isBlockedIP(addr.IP) {
			// Deliberately no WithParam("ip", addr.IP.String()) here: this
			// path reached the blocked address through DNS, so the resolved
			// address is information the caller does not have, and echoing
			// it back would turn the refusal into an internal-DNS
			// reconnaissance oracle -- submit hostnames, read back the
			// internal IPs they resolve to. The literal-IP branch above
			// keeps its ip param, because there the caller typed the
			// address itself and the echo discloses nothing. The coded
			// error plus the generic base_url_blocked rendering -- which
			// names no address -- is the honest answer shape for this path.
			// See ErrBaseURLBlocked's own doc comment (errors.go).
			return ErrBaseURLBlocked
		}
	}
	return nil
}

// baseURLValidationTimeout bounds ValidateBaseURL's DNS lookup, so a slow
// or unresponsive resolver cannot hang a credential-write request.
const baseURLValidationTimeout = 5 * time.Second

// providerDialTimeout bounds one dial attempt inside
// guardedProviderHTTPClient's transport.
const providerDialTimeout = 5 * time.Second

// errBlockedDialAddress is wrapped into the error guardedProviderHTTPClient's
// DialContext returns when every resolved address for a dial is blocked,
// so a caller inspecting a failed provider call can tell a blocked-
// destination failure apart from an ordinary network failure: the dial
// error travels wrapped inside the provider's own ErrProviderRequestFailed,
// whose text then carries "destination ... blocked".
var errBlockedDialAddress = errors.New("aigateway: provider call refused: destination resolves to a blocked address")

// providerDialFunc performs the one TCP dial guardedProviderHTTPClient's
// transport issues for a single validated candidate address. It is a
// package-level function variable, not a private method, so a test can
// replace it and observe exactly what the guard hands to the dialer --
// the seam the dial-time pinning property is asserted through: the dialed
// address must be the validated candidate's IP LITERAL, never the original
// hostname, or the rebinding window this file's header comment closes
// would be open (a re-resolution inside the dial would let a rebinding
// DNS answer steer the connection). The default implementation builds a
// fresh dialer per attempt, carrying providerDialTimeout exactly as the
// pre-seam code did.
var providerDialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: providerDialTimeout}
	return dialer.DialContext(ctx, network, addr)
}

// resolveProviderHost resolves host the way guardedProviderHTTPClient's
// transport needs it resolved at dial time. It is a package-level function
// variable for the same reason providerDialFunc is: the dial-time pinning
// property cannot be exercised against the real resolver in an offline
// test, so the rebinding sequence (an answer that changes between
// resolutions) is scripted through this seam -- see ssrf_test.go's
// TestGuardedProviderHTTPClient_DialsTheValidatedIPLiteral.
var resolveProviderHost = net.DefaultResolver.LookupIPAddr

// guardedProviderTransportIdleTimeout is the IdleConnTimeout of the shared
// transport guardedProviderHTTPClient uses: how long an idle keep-alive
// connection to a vendor is kept for reuse before it is closed. Sane and
// finite on purpose -- a zero IdleConnTimeout would keep an idle connection
// to a dead or changed vendor around forever (Go's own default).
const guardedProviderTransportIdleTimeout = 90 * time.Second

// guardedProviderHTTPClient is the *http.Client Gateway.resolve and
// Gateway.resolveImage install on a provider when the credential that
// built it resolved at the TENANT tier (httpClientSettable.setHTTPClient)
// -- built ONCE at package init, never per call, so consecutive calls to
// the same vendor endpoint reuse one TCP connection (and TLS session)
// instead of dialing afresh for every provider instance, exactly like
// go/integration's defaultWebhookHTTPClient. Its Transport re-validates
// the destination at DIAL time -- see this file's own header comment for
// why that is what actually defeats DNS rebinding, which a creation-time-
// only check cannot.
//
// CheckRedirect caps the redirect chain at maxProviderRedirects: an
// unbounded chain from an untrusted endpoint is both a resource-
// exhaustion surface and, per Go's default of 10, more hops than any
// legitimate OpenAI-compatible vendor needs; each hop still re-dials
// through this same guarded transport.
var guardedProviderHTTPClient = newGuardedProviderHTTPClient()

// newGuardedProviderHTTPClient returns the http.Client above. It carries
// no Timeout of its own: Chat and the image methods bound their own
// requests with context deadlines (defaultHTTPTimeout), and a stream can
// legitimately run far longer than any fixed request timeout, so streaming
// calls are bounded only by ctx -- the identical timeout posture the
// providers' default clients already keep.
func newGuardedProviderHTTPClient() *http.Client {
	transport := &http.Transport{
		IdleConnTimeout:     guardedProviderTransportIdleTimeout,
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
				resolved, err := resolveProviderHost(ctx, host)
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
				// The dial itself goes through providerDialFunc so a test
				// can assert this property -- see that seam's own comment.
				conn, err := providerDialFunc(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			if lastErr == nil {
				lastErr = fmt.Errorf("aigateway: no address resolved for %s", host)
			}
			return nil, lastErr
		},
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxProviderRedirects {
				return fmt.Errorf("aigateway: provider call: stopped after %d redirects", maxProviderRedirects)
			}
			return nil
		},
	}
}

// maxProviderRedirects bounds the redirect chain guardedProviderHTTPClient
// follows.
const maxProviderRedirects = 3

// httpClientSettable is implemented by this module's own providers so
// Gateway.resolve / resolveImage can install the SSRF-guarded client on a
// freshly built provider whose credential resolved at the tenant tier. It
// is deliberately an unexported, structural interface: hosts never see it,
// and a third-party provider that does not implement it can never carry a
// tenant-scope credential -- guardTenantScopeDial refuses that combination
// with ErrProviderNotSSRFGuardable rather than letting the tenant-
// influenced dial go out unguarded (see that error's own doc comment).
type httpClientSettable interface {
	setHTTPClient(c *http.Client)
}

// guardTenantScopeDial installs guardedProviderHTTPClient on provider when
// scope is the tenant tier, and leaves it untouched otherwise -- the one
// place the scope boundary this file's own header comment draws is
// enforced on the dial path. resolve and resolveImage both call it after
// every Build.
//
// A tenant-scope credential naming a provider that cannot carry the
// guarded client is REFUSED with ErrProviderNotSSRFGuardable rather than
// silently allowed to dial unguarded: a provider without httpClientSettable
// has no rebinding-defeating dial-time re-check at all, which makes the
// tenant-scope dial exactly as safe as the write-time-only validation
// ssrf.go's own header comment says is not enough. Only the tenant tier is
// refused -- the platform tier stays on the provider's own client by the
// scope boundary, the operator's legitimate intranet-gateway default.
func guardTenantScopeDial(provider any, scope CredentialScope) error {
	if scope != CredentialScopeTenant {
		return nil
	}
	settable, ok := provider.(httpClientSettable)
	if !ok {
		return ErrProviderNotSSRFGuardable
	}
	settable.setHTTPClient(guardedProviderHTTPClient)
	return nil
}
