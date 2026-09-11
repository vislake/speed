package aigateway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/vislake/speed/go/pkgcore/safehttp"
)

// This file is go/ai-gateway's caller-side layer over pkgcore/safehttp --
// the shared outbound-request guard -- for the base URL of a tenant BYOK
// credential, the URL a tenant admin supplies over the credential HTTP
// surface (handler.go's AiGatewaySetTenantCredential) and Gateway.Chat /
// Gateway.ChatStream / Gateway.GenerateImage then dial from the platform's
// own network, presenting the credential's API key. A tenant-supplied dial
// destination is the same attack class as an outbound webhook URL, so it
// gets the same guard; what stays here is everything this module's own:
//
//  1. ValidateBaseURL runs at credential-write time, from
//     SetTenantCredential before any row is written (credential.go) -- the
//     cheap, early refusal, plus this module's coded error vocabulary
//     (ErrBaseURLInvalid / ErrBaseURLBlocked / ErrBaseURLUnresolvable) with
//     the no-IP-echo asymmetry ErrBaseURLBlocked's own comment pins: an
//     address the caller typed into the URL is echoed back, an address
//     reached through DNS resolution never is. It also bounds the guard's
//     DNS lookup, so a slow or unresponsive resolver cannot hang a
//     credential-write request.
//  2. guardedProviderHTTPClient is the client Gateway.resolve and
//     resolveImage install on a provider built from a TENANT-tier
//     credential (httpClientSettable). It re-checks every address it
//     actually dials -- every redirect hop included -- which is what
//     defeats DNS rebinding: a tenant controls the DNS of the hostname it
//     wrote, so a base URL that resolved to a public address when stored
//     can be repointed at an internal one minutes later, and a check that
//     ran only at SetTenantCredential would be a one-time gate an attacker
//     simply waits out. It carries no overall timeout: Chat and the image
//     methods bound their own requests with context deadlines, and a stream
//     can legitimately run far longer than any fixed request timeout.
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
// dial either. guardTenantScopeDial below is where that boundary reaches
// the dial path; it also REFUSES the one combination the guard cannot cover
// (a tenant-tier credential resolving to a provider that cannot carry the
// guarded client) rather than letting that dial go out unguarded -- see its
// own doc comment.
//
// ErrBaseURLInvalid, ErrBaseURLBlocked and ErrBaseURLUnresolvable are
// declared in errors.go, alongside this module's other error codes; this
// file only implements the checks that raise them.

// baseURLGuard is the shared guard as tenant base URLs need it: exactly
// http and https -- a vendor's plain-HTTP test endpoint may legitimately be
// named -- and nothing else. newGuardedProviderHTTPClient's client carries
// the same list.
var baseURLGuard = safehttp.NewGuard(safehttp.WithAllowedSchemes("http", "https"))

// baseURLValidationTimeout bounds ValidateBaseURL's DNS lookup, so a slow
// or unresponsive resolver cannot hang a credential-write request.
const baseURLValidationTimeout = 5 * time.Second

// ValidateBaseURL refuses a tenant BYOK credential base URL that is
// malformed, uses a scheme other than http/https, or resolves to any
// address the shared guard blocks. The guard's blocked set -- loopback,
// private, link-local, CGNAT, multicast and the IPv6 special-purpose
// ranges, all spelled out in pkgcore/safehttp -- is the one authority for
// what counts as blocked, and the dial-time half of the defense re-checks
// every address a provider call actually connects to
// (guardedProviderHTTPClient). See this file's own header comment for the
// scope boundary that keeps the platform-scope write off this validator.
//
// The empty string is not this function's business: a credential with no
// base URL configured is legal to store (credentialRow.BaseURL's doc
// comment), so the caller skips validation for it rather than passing it
// here.
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
	host := u.Hostname()
	if host == "" {
		return ErrBaseURLInvalid.WithParam("reason", "missing_host")
	}

	// The scheme gate runs through the guard, so the allowed-scheme list has
	// exactly one authority (baseURLGuard's own options). The parse above has
	// already accepted rawURL, so the only refusal the gate can raise here is
	// the scheme one; it is mapped into this module's own reason vocabulary.
	if _, err := baseURLGuard.CheckScheme(rawURL); err != nil {
		return ErrBaseURLInvalid.WithParam("reason", "scheme").WithParam("scheme", u.Scheme).WithCause(err)
	}

	// Which branch the refusal came from is what keeps the ip param
	// asymmetric: the address the caller typed is an echo of what the
	// caller already knows, while a DNS answer is information the caller
	// does not have -- for a name resolvable only inside the platform's
	// own network, exactly the answer an internal-DNS reconnaissance
	// oracle would give (errors.go's ErrBaseURLBlocked comment, the
	// asymmetry a refactor must not flatten).
	literal := net.ParseIP(host)

	resolveCtx, cancel := context.WithTimeout(ctx, baseURLValidationTimeout)
	defer cancel()
	if _, err := baseURLGuard.ValidateURL(resolveCtx, rawURL); err != nil {
		switch {
		case errors.Is(err, safehttp.ErrBlockedAddress):
			if literal != nil {
				return ErrBaseURLBlocked.WithParam("ip", literal.String())
			}
			return ErrBaseURLBlocked
		case errors.Is(err, safehttp.ErrUnresolvable):
			return ErrBaseURLUnresolvable.WithParam("host", host).WithCause(err)
		default:
			return ErrBaseURLInvalid.WithCause(err)
		}
	}
	return nil
}

// guardedProviderHTTPClient is the *http.Client Gateway.resolve and
// Gateway.resolveImage install on a provider when the credential that
// built it resolved at the TENANT tier (httpClientSettable.setHTTPClient)
// -- built ONCE at package init, never per call, so consecutive calls to
// the same vendor endpoint reuse one TCP connection (and TLS session)
// instead of dialing afresh for every provider instance, exactly like
// go/integration's defaultWebhookHTTPClient.
var guardedProviderHTTPClient = newGuardedProviderHTTPClient()

// newGuardedProviderHTTPClient returns the http.Client above: the shared
// guard's own client, so every connection -- every redirect hop included --
// passes the connect-time address check and the per-hop scheme re-check. It
// carries no Timeout of its own (WithTimeout(0)): Chat and the image
// methods bound their own requests with context deadlines
// (defaultHTTPTimeout), and a stream can legitimately run far longer than
// any fixed request timeout, so streaming calls are bounded only by ctx --
// the identical timeout posture the providers' default clients already
// keep.
func newGuardedProviderHTTPClient() *http.Client {
	return safehttp.NewGuard(
		safehttp.WithAllowedSchemes("http", "https"),
		safehttp.WithTimeout(0),
	).Client()
}

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
// tenant-scope dial exactly as safe as the write-time-only validation this
// file's own header comment says is not enough. Only the tenant tier is
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
