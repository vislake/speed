package pkgcore

import "errors"

// ErrTransportPermanent is the sentinel a transport implementation wraps --
// per Go convention, with an %w wrap -- around a send failure that is the
// destination's own permanent refusal of the message: the address rejected
// it (an SMTP 5xx reply to RCPT, a carrier's "invalid number" or
// "blacklisted number" verdict), so retrying the same message to the same
// address can never succeed. It is the one control signal the Mailer and
// SMSSender contracts share, matched by callers with errors.Is, and it is
// deliberately not an apperr: it is never surfaced to an API caller, it is
// the delivery side's terminal-versus-retry signal, and go/notification
// acts on it as the destination's verdict -- a wrapped failure settles the
// attempt as terminal and marks the module's own external contact bounced,
// so the consent ledger refuses the address before any later transport
// runs. It lives on the dependency floor rather than in any consumer
// because the implementations that must produce it (this package's SMTP
// mailer and the sms/aliyun and sms/tencent adapters) sit below every
// consumer in the module graph and could not import a sentinel declared
// above them.
//
// An implementation must wrap it ONLY for the destination's own verdict.
// Every other failure -- a dial, TLS or authentication failure, a 4xx "try
// again later" reply, a message-shape or configuration refusal such as an
// unmapped template, a provider frequency limit -- travels unwrapped,
// however permanently it would fail, because a consumer acts on the wrapped
// signal as the address's verdict: marking a contact bounced over an
// operator misconfiguration or an account-level problem would blacklist a
// healthy recipient. What an unwrapped terminal failure loses in
// retry-economy it regains in being operator-visible: the attempt retries
// into a dead-lettered job instead. The built-in implementations that wrap
// it are the SMTP mailer (a 5xx answer to RCPT) and the aliyun and tencent
// SMS adapters (their documented per-number rejection codes, named in each
// package's own classification table); the console mailer and the console
// and operator-gateway SMS senders have no permanent failures and never
// produce it.
var ErrTransportPermanent = errors.New("pkgcore: permanent transport failure")
