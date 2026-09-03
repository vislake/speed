// Package safehttp is authn's outbound-request guard.
//
// Two of this module's destinations are chosen by somebody other than the
// operator of the process making the request: the OIDC issuer URL a tenant
// administrator types into their SSO settings, and the SMS gateway endpoint a
// deployment configures. Both are exactly the shape of a server-side request
// forgery: an attacker who can set the URL makes the SERVER fetch it, from
// inside the network, with whatever the network trusts the server to have.
// "http://169.254.169.254/latest/meta-data/iam/security-credentials/" is the
// canonical example and it costs nothing to try.
//
// The guard therefore refuses to connect to any address that is not global
// unicast, and it enforces that at the moment of connection rather than only
// when the URL is saved. The distinction is the whole point: a hostname is
// resolved twice -- once when it is validated, once when it is dialled -- and
// a DNS server the attacker controls can answer differently each time (DNS
// rebinding). net.Dialer's Control hook runs after resolution and before
// connect, with the address that is actually about to be used, which is the
// one place a check cannot be raced.
//
// It lives under internal/ because it is authn's own defence and not a
// general-purpose HTTP facility: the moment it looks reusable it belongs in
// a shared module with its own tests, not copied.
package safehttp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"
)

// ErrBlockedAddress is returned when a destination resolves to an address the
// guard refuses to connect to. Callers wrap it in their own domain error
// rather than returning it to a client: the address itself is a detail of the
// deployment's network.
var ErrBlockedAddress = errors.New("safehttp: destination address is not a public unicast address")

// ErrBlockedScheme is returned for a URL whose scheme is not allowed.
var ErrBlockedScheme = errors.New("safehttp: destination scheme is not allowed")

// ErrUnresolvable is returned when a destination host has no address at all.
var ErrUnresolvable = errors.New("safehttp: destination host could not be resolved")

const (
	// defaultTimeout bounds a whole outbound request. Every destination
	// this guard fronts is a third party, and a third party that never
	// answers must not hold a request goroutine open indefinitely.
	defaultTimeout = 10 * time.Second

	// dialTimeout bounds the TCP connect itself.
	dialTimeout = 5 * time.Second

	// maxRedirects bounds redirect following. Each hop is re-validated by
	// the dialler, so the bound is about work rather than safety.
	maxRedirects = 5
)

// blockedPrefixes are the IPv4 and IPv6 ranges that must never be reached
// from a caller-supplied URL.
//
// netip.Addr's own predicates already cover loopback, link-local, multicast,
// the unspecified address and -- through IsPrivate -- the RFC 1918 and IPv6
// unique-local ranges. Those three are repeated here anyway, deliberately: a
// list that a reader has to cross-reference against the standard library to
// know whether 10.0.0.0/8 is handled is a list nobody audits. The entries
// that are NOT covered elsewhere are the carrier-grade NAT range that fronts
// many internal networks, the benchmarking range, the three documentation
// ranges, and 192.0.0.0/24 (IETF protocol assignments, which includes the
// NAT64 well-known prefix's IPv4 side).
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("2001:db8::/32"),
}

// Resolver is the name-resolution seam. It exists so a test can prove the
// connect-time check still refuses an address a resolver claimed was fine a
// moment earlier -- which is precisely the DNS-rebinding case.
type Resolver func(ctx context.Context, host string) ([]netip.Addr, error)

// config is a Guard's settings.
type config struct {
	schemes []string
	resolve Resolver
	timeout time.Duration
	maxHops int
}

// Option configures a Guard.
type Option func(*config)

// WithAllowedSchemes replaces the allowed URL schemes. The default is https
// only: a caller-supplied destination reached over plaintext http is readable
// and rewritable by anything on the path, and every destination this guard
// fronts carries a client secret or a one-time code.
func WithAllowedSchemes(schemes ...string) Option {
	return func(c *config) {
		if len(schemes) > 0 {
			c.schemes = slices.Clone(schemes)
		}
	}
}

// WithResolver replaces name resolution. A nil resolver is ignored.
func WithResolver(r Resolver) Option {
	return func(c *config) {
		if r != nil {
			c.resolve = r
		}
	}
}

// WithTimeout bounds a whole request made through Client. A non-positive
// duration is ignored.
func WithTimeout(d time.Duration) Option {
	return func(c *config) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// Guard validates destinations and hands out an *http.Client that cannot
// connect to a blocked one.
type Guard struct {
	cfg config
}

// NewGuard returns a Guard with the given options applied over the defaults.
func NewGuard(opts ...Option) *Guard {
	cfg := config{
		schemes: []string{"https"},
		resolve: defaultResolve,
		timeout: defaultTimeout,
		maxHops: maxRedirects,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return &Guard{cfg: cfg}
}

// defaultResolve resolves host with the process resolver.
func defaultResolve(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// ValidateURL parses raw, checks its scheme, resolves its host and refuses it
// when ANY resolved address is blocked.
//
// Refusing on any rather than on all is deliberate. A host that answers with
// both a public and a private address is a rebinding attempt written down in
// one response; accepting it because one of the two looked fine would leave
// the choice to whichever address the dialler happened to try first.
//
// It is a pre-flight check, run when a URL is saved so the operator gets a
// clear error at the moment they can fix it. It is NOT the security boundary
// -- Client's dialler is, and it re-checks every address it actually
// connects to.
func (g *Guard) ValidateURL(ctx context.Context, raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("safehttp: parse destination: %w", err)
	}
	if !parsed.IsAbs() || parsed.Host == "" {
		return nil, fmt.Errorf("%w: destination must be an absolute URL", ErrBlockedScheme)
	}
	if !slices.Contains(g.cfg.schemes, strings.ToLower(parsed.Scheme)) {
		return nil, fmt.Errorf("%w: %q", ErrBlockedScheme, parsed.Scheme)
	}

	host := parsed.Hostname()
	if host == "" {
		return nil, fmt.Errorf("%w: destination has no host", ErrUnresolvable)
	}

	// A literal address needs no resolution, and passing one to a
	// resolver would be a needless lookup that some resolvers answer
	// differently.
	if addr, parseErr := netip.ParseAddr(host); parseErr == nil {
		if checkErr := CheckAddr(addr); checkErr != nil {
			return nil, checkErr
		}
		return parsed, nil
	}

	addrs, err := g.cfg.resolve(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrUnresolvable, host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrUnresolvable, host)
	}
	for _, addr := range addrs {
		if checkErr := CheckAddr(addr); checkErr != nil {
			return nil, checkErr
		}
	}
	return parsed, nil
}

// CheckAddr reports whether addr may be connected to, returning
// ErrBlockedAddress when it may not.
//
// IPv4-mapped IPv6 addresses are unmapped first: "::ffff:127.0.0.1" is
// loopback wearing a different spelling, and a check that looked only at the
// IPv6 predicates would wave it straight through.
func CheckAddr(addr netip.Addr) error {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return fmt.Errorf("%w: invalid address", ErrBlockedAddress)
	}
	switch {
	case addr.IsUnspecified(),
		addr.IsLoopback(),
		addr.IsLinkLocalUnicast(),
		addr.IsLinkLocalMulticast(),
		addr.IsInterfaceLocalMulticast(),
		addr.IsMulticast(),
		addr.IsPrivate():
		return fmt.Errorf("%w: %s", ErrBlockedAddress, addr)
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(addr) {
			return fmt.Errorf("%w: %s", ErrBlockedAddress, addr)
		}
	}
	return nil
}

// checkDialAddress is the Control hook's check. address is "host:port" with
// host ALREADY resolved to a literal by the dialler, which is what makes this
// the one check DNS rebinding cannot get past.
func checkDialAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrBlockedAddress, address)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		// The dialler hands Control a literal address. Anything else
		// means an assumption here no longer holds, and the safe
		// reading of an unexpected value is "refuse".
		return fmt.Errorf("%w: %s", ErrBlockedAddress, address)
	}
	return CheckAddr(addr)
}

// CheckDialAddress exposes the connect-time check for tests, which is the
// only way to prove that a resolver answering with a public address does not
// let a private one through.
func (g *Guard) CheckDialAddress(address string) error { return checkDialAddress(address) }

// Client returns an *http.Client whose every connection -- including every
// redirect hop -- passes through the connect-time check.
//
// The transport takes no proxy: a proxied request connects to the proxy, so
// the address the dialler validates is the proxy's and the destination is
// never checked at all. An outbound guard and an HTTP proxy cannot both be
// in effect, and this module chooses the guard.
func (g *Guard) Client() *http.Client {
	dialer := &net.Dialer{
		Timeout: dialTimeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			return checkDialAddress(address)
		},
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   dialTimeout,
		ExpectContinueTimeout: time.Second,
	}
	maxHops := g.cfg.maxHops
	return &http.Client{
		Transport: transport,
		Timeout:   g.cfg.timeout,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= maxHops {
				return fmt.Errorf("safehttp: stopped after %d redirects", maxHops)
			}
			return nil
		},
	}
}

// NewClient is the shorthand for NewGuard(opts...).Client().
func NewClient(opts ...Option) *http.Client { return NewGuard(opts...).Client() }
