package safehttp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
)

// TestCheckAddr_RefusesEverythingNotPubliclyRoutable is the range table. Each
// case is an address a server-side request forgery would actually aim at, and
// each one has been used in a real incident: the cloud metadata endpoint at
// 169.254.169.254, the container network at 10.x, the loopback interface that
// so often hosts an unauthenticated admin API.
func TestCheckAddr_RefusesEverythingNotPubliclyRoutable(t *testing.T) {
	t.Parallel()

	blocked := []struct {
		name string
		addr string
	}{
		{name: "ipv4 loopback", addr: "127.0.0.1"},
		{name: "ipv4 loopback beyond .1", addr: "127.9.9.9"},
		{name: "ipv6 loopback", addr: "::1"},
		{name: "ipv4-mapped ipv6 loopback", addr: "::ffff:127.0.0.1"},
		{name: "cloud metadata link-local", addr: "169.254.169.254"},
		{name: "ipv6 link-local", addr: "fe80::1"},
		{name: "rfc1918 ten", addr: "10.0.0.1"},
		{name: "rfc1918 172.16", addr: "172.20.1.1"},
		{name: "rfc1918 192.168", addr: "192.168.1.1"},
		{name: "ipv4-mapped ipv6 rfc1918", addr: "::ffff:10.1.2.3"},
		{name: "carrier-grade nat", addr: "100.64.0.1"},
		{name: "benchmarking", addr: "198.18.0.1"},
		{name: "documentation", addr: "192.0.2.1"},
		{name: "ipv6 unique local", addr: "fd00::1"},
		{name: "ipv6 documentation", addr: "2001:db8::1"},
		{name: "unspecified v4", addr: "0.0.0.0"},
		{name: "unspecified v6", addr: "::"},
		{name: "multicast", addr: "224.0.0.1"},
		{name: "ipv6 multicast", addr: "ff02::1"},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			addr := netip.MustParseAddr(tc.addr)
			if err := CheckAddr(addr); !errors.Is(err, ErrBlockedAddress) {
				t.Errorf("CheckAddr(%s) error = %v, want ErrBlockedAddress", tc.addr, err)
			}
		})
	}

	allowed := []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "203.0.114.1"}
	for _, raw := range allowed {
		t.Run("allows "+raw, func(t *testing.T) {
			t.Parallel()
			if err := CheckAddr(netip.MustParseAddr(raw)); err != nil {
				t.Errorf("CheckAddr(%s) error = %v, want nil", raw, err)
			}
		})
	}
}

// TestGuard_CheckScheme_RefusesDisallowedSchemesWithoutResolving pins the
// scheme-only half of ValidateURL: it is the half the SMS sender runs before
// every request, so it must refuse a disallowed scheme without resolving the
// host -- a resolution would cost a network round trip per send and couple
// the refusal to the resolver. The injected resolver fails the test if it is
// ever consulted.
func TestGuard_CheckScheme_RefusesDisallowedSchemesWithoutResolving(t *testing.T) {
	t.Parallel()

	resolved := false
	guard := NewGuard(WithResolver(func(context.Context, string) ([]netip.Addr, error) {
		resolved = true
		return nil, errors.New("resolver must not be consulted by CheckScheme")
	}))

	for _, raw := range []string{
		"http://example.com/sms",
		"file:///etc/passwd",
		"gopher://example.com:70/",
		"ftp://example.com/",
		"/relative/only",
	} {
		if _, err := guard.CheckScheme(raw); !errors.Is(err, ErrBlockedScheme) {
			t.Errorf("CheckScheme(%q) error = %v, want ErrBlockedScheme", raw, err)
		}
	}
	if resolved {
		t.Error("CheckScheme resolved a host; the scheme check must not need resolution")
	}

	parsed, err := guard.CheckScheme(" https://idp.example.com/oidc ")
	if err != nil {
		t.Fatalf("CheckScheme(https) error = %v, want nil", err)
	}
	if parsed.Host != "idp.example.com" {
		t.Errorf("parsed host = %q, want %q", parsed.Host, "idp.example.com")
	}
}

func TestGuard_ValidateURL_RefusesABlockedScheme(t *testing.T) {
	t.Parallel()

	guard := NewGuard()
	for _, raw := range []string{
		"http://example.com/",
		"file:///etc/passwd",
		"gopher://example.com:70/",
		"ftp://example.com/",
		"/relative/only",
	} {
		if _, err := guard.ValidateURL(t.Context(), raw); !errors.Is(err, ErrBlockedScheme) {
			t.Errorf("ValidateURL(%q) error = %v, want ErrBlockedScheme", raw, err)
		}
	}
}

func TestGuard_ValidateURL_RefusesALiteralPrivateHost(t *testing.T) {
	t.Parallel()

	guard := NewGuard()
	for _, raw := range []string{
		"https://127.0.0.1/token",
		"https://169.254.169.254/latest/meta-data/",
		"https://[::1]:8443/token",
		"https://10.1.2.3/token",
	} {
		if _, err := guard.ValidateURL(t.Context(), raw); !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("ValidateURL(%q) error = %v, want ErrBlockedAddress", raw, err)
		}
	}
}

func TestGuard_ValidateURL_AcceptsAPublicHost(t *testing.T) {
	t.Parallel()

	guard := NewGuard(WithResolver(func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}))

	parsed, err := guard.ValidateURL(t.Context(), " https://idp.example.com/oidc ")
	if err != nil {
		t.Fatalf("ValidateURL() error = %v, want nil", err)
	}
	if parsed.Host != "idp.example.com" {
		t.Errorf("parsed host = %q, want %q", parsed.Host, "idp.example.com")
	}
}

// TestGuard_ValidateURL_RefusesAHostWithAnyPrivateAnswer covers the host that
// answers with one public and one private address. Accepting it because one
// answer looked fine would hand the choice to whichever address the dialler
// happened to try first.
func TestGuard_ValidateURL_RefusesAHostWithAnyPrivateAnswer(t *testing.T) {
	t.Parallel()

	guard := NewGuard(WithResolver(func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{
			netip.MustParseAddr("93.184.216.34"),
			netip.MustParseAddr("127.0.0.1"),
		}, nil
	}))

	if _, err := guard.ValidateURL(t.Context(), "https://split.example.com/"); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("ValidateURL() error = %v, want ErrBlockedAddress", err)
	}
}

func TestGuard_ValidateURL_ReportsAnUnresolvableHost(t *testing.T) {
	t.Parallel()

	t.Run("resolver error", func(t *testing.T) {
		t.Parallel()
		guard := NewGuard(WithResolver(func(context.Context, string) ([]netip.Addr, error) {
			return nil, errors.New("no such host")
		}))
		if _, err := guard.ValidateURL(t.Context(), "https://nowhere.example/"); !errors.Is(err, ErrUnresolvable) {
			t.Errorf("ValidateURL() error = %v, want ErrUnresolvable", err)
		}
	})

	t.Run("empty answer", func(t *testing.T) {
		t.Parallel()
		guard := NewGuard(WithResolver(func(context.Context, string) ([]netip.Addr, error) {
			return nil, nil
		}))
		if _, err := guard.ValidateURL(t.Context(), "https://nowhere.example/"); !errors.Is(err, ErrUnresolvable) {
			t.Errorf("ValidateURL() error = %v, want ErrUnresolvable", err)
		}
	})
}

// TestGuard_DNSRebindingCannotGetPastTheConnectTimeCheck is the reason this
// package exists in the shape it does.
//
// The resolver here is the attacker's: it answers with a public address, so
// ValidateURL accepts the URL exactly as it would for an honest host. The
// address the dialler then actually connects to is a different one, because
// the second lookup returned something else. The connect-time check runs on
// THAT address, and refuses it -- which is what makes the guard a property of
// the connection rather than of a lookup that has already been superseded by
// the time it matters.
func TestGuard_DNSRebindingCannotGetPastTheConnectTimeCheck(t *testing.T) {
	t.Parallel()

	guard := NewGuard(WithResolver(func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}))

	if _, err := guard.ValidateURL(t.Context(), "https://rebind.example.com/"); err != nil {
		t.Fatalf("ValidateURL() error = %v; the attacker's first answer is deliberately clean", err)
	}
	if err := guard.CheckDialAddress("127.0.0.1:443"); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("CheckDialAddress() error = %v, want ErrBlockedAddress for the rebound answer", err)
	}
}

func TestGuard_CheckDialAddress_RefusesAnythingUnparsable(t *testing.T) {
	t.Parallel()

	guard := NewGuard()
	for _, address := range []string{"", "not-an-address", "example.com:443", "1.2.3.4"} {
		if err := guard.CheckDialAddress(address); !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("CheckDialAddress(%q) error = %v, want ErrBlockedAddress", address, err)
		}
	}
	if err := guard.CheckDialAddress("8.8.8.8:443"); err != nil {
		t.Errorf("CheckDialAddress(public) error = %v, want nil", err)
	}
}

// TestGuard_ClientCannotReachALoopbackServer proves the guard end to end: a
// real server is listening, a plain http.Client would reach it, and the
// guarded client cannot.
func TestGuard_ClientCannotReachALoopbackServer(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	if _, err := server.Client().Get(server.URL); err != nil {
		t.Fatalf("the plain client could not reach the test server: %v; the negative case below would prove nothing", err)
	}

	guard := NewGuard(WithAllowedSchemes("http", "https"))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	resp, err := guard.Client().Do(req) //nolint:bodyclose // the request must fail, so there is no body to close.
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("the guarded client reached a loopback server")
	}
	if !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("Do() error = %v, want it to wrap ErrBlockedAddress", err)
	}
}

// TestGuard_ClientUsesNoProxy pins the transport decision: a proxied request
// connects to the proxy, so the dialler would validate the proxy's address
// and never look at the destination at all.
func TestGuard_ClientUsesNoProxy(t *testing.T) {
	t.Parallel()

	transport, ok := NewGuard().Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Client().Transport is %T, want *http.Transport", NewGuard().Client().Transport)
	}
	if transport.Proxy != nil {
		t.Error("the guarded transport has a proxy; a proxied request never validates its real destination")
	}
	if transport.DialContext == nil {
		t.Fatal("the guarded transport has no DialContext, so nothing enforces the address check")
	}
}

// TestNewClient_AppliesItsOptions proves the package-level shorthand is not a
// second, unguarded construction path.
func TestNewClient_AppliesItsOptions(t *testing.T) {
	t.Parallel()

	client := NewClient()
	if client.Timeout <= 0 {
		t.Error("NewClient() returned a client with no timeout; a third party that never answers would hold the request open")
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://127.0.0.1:1/", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	resp, err := client.Do(req) //nolint:bodyclose // the request must fail, so there is no body to close.
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("NewClient() reached a loopback address")
	}
	if !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("Do() error = %v, want it to wrap ErrBlockedAddress", err)
	}
}

// TestGuard_Client_RefusesA307Or308RedirectToAForbiddenScheme is the
// scheme half of the redirect policy: the scheme of every hop is
// re-checked before the hop is followed, not only hop zero's.
//
// The harm is specific to 307 and 308: those two statuses re-send the
// request body, and this guard fronts destinations whose bodies carry
// credentials -- the SMS gateway's phone-number-and-verification-code body,
// an OIDC exchange's authorization code. An https destination answering
// 307/308 with an http Location must therefore be refused BY SCHEME before
// the body is re-sent in cleartext across the public internet: the dialler
// would admit the hop (it re-checks addresses, never schemes), so without
// this check a redirect silently downgrades the transport the body travels
// on.
//
// The guarded client's own transport is deliberately replaced here: its
// dialler refuses loopback -- the very property TestGuard_ClientCannotReachALoopbackServer
// pins -- so no httptest server can be reached through it. The replacement
// is the TLS test server's own transport, which keeps everything else about
// Client() intact, including the redirect policy under test.
//
// Before the policy checked schemes, this test failed: the client followed
// the downgrading redirect and the capture server received the re-sent
// body.
func TestGuard_Client_RefusesA307Or308RedirectToAForbiddenScheme(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(fmt.Sprintf("%d", status), func(t *testing.T) {
			var (
				captureMu sync.Mutex
				captured  []byte
			)
			capture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				captureMu.Lock()
				captured = body
				captureMu.Unlock()
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(capture.Close)

			hop0 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", capture.URL+"/capture")
				w.WriteHeader(status)
			}))
			t.Cleanup(hop0.Close)

			client := NewGuard().Client()
			client.Transport = hop0.Client().Transport

			// The body shape of the flow whose leak this check exists to
			// stop: a phone number and a verification code.
			body := []byte(`{"to":"+15551234567","text":"your code is 123456"}`)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, hop0.URL+"/sms", bytes.NewBuffer(body))
			if err != nil {
				t.Fatalf("build the request: %v", err)
			}
			resp, err := client.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if !errors.Is(err, ErrBlockedScheme) {
				t.Errorf("Do() error = %v; a %d redirect to an http Location must be refused by scheme, not followed", err, status)
			}

			captureMu.Lock()
			reSent := len(captured) > 0
			captureMu.Unlock()
			if reSent {
				t.Errorf("the %d redirect re-sent the request body to the capture server; the scheme-downgrading hop must never be dialled", status)
			}
		})
	}
}

// TestGuard_Client_StillFollowsARedirectThatKeepsAnAllowedScheme pins the
// no-over-blocking side of the redirect policy's scheme re-check: a hop
// whose scheme is still allowed is followed exactly as it was before the
// check existed. The chain here stays https end to end under the guard's
// default https-only allowlist.
func TestGuard_Client_StillFollowsARedirectThatKeepsAnAllowedScheme(t *testing.T) {
	var hop *httptest.Server
	hop = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			fmt.Fprint(w, "final reached")
			return
		}
		w.Header().Set("Location", hop.URL+"/final")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(hop.Close)

	client := NewGuard().Client()
	client.Transport = hop.Client().Transport // loopback; see TestGuard_Client_RefusesA307Or308RedirectToAForbiddenScheme

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, hop.URL+"/start", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v; an https-to-https redirect must still be followed", err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the final response: %v", err)
	}
	if string(got) != "final reached" {
		t.Errorf("final response body = %q, want %q", got, "final reached")
	}
	if resp.Request.URL.Path != "/final" {
		t.Errorf("final request path = %q, want %q", resp.Request.URL.Path, "/final")
	}
}

// TestGuard_Client_DistinguishesASchemeRefusalFromAHopCountRefusal proves
// the two refusals keep their own vocabularies: a chain that ends on the
// hop-count bound refuses with the hop-count error -- never with a scheme
// refusal -- and the scheme refusal the tests above pin never masquerades
// as a hop-count one.
func TestGuard_Client_DistinguishesASchemeRefusalFromAHopCountRefusal(t *testing.T) {
	// http is in the allowlist here on purpose: the hop that ends this
	// chain is refused for its LENGTH, so the error must be the hop-count
	// one, distinguishable from ErrBlockedScheme.
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", server.URL+"/hop")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(server.Close)

	client := NewGuard(WithAllowedSchemes("http", "https")).Client()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil        // no environment proxy inside a test
	client.Transport = transport // loopback; see TestGuard_Client_RefusesA307Or308RedirectToAForbiddenScheme

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/start", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("Do() followed the chain past the hop bound")
	}
	if errors.Is(err, ErrBlockedScheme) {
		t.Errorf("Do() error = %v; an over-long chain of allowed-scheme hops must not be reported as a scheme refusal", err)
	}
	if !strings.Contains(err.Error(), "stopped after 5 redirects") {
		t.Errorf("Do() error = %v, want the hop-count refusal message", err)
	}
}
