package notification

// The error index of the notification module. Every exported error this
// module will ever declare is an *apperr.Error builder whose Code follows
// the <module>.<reason> convention -- notification.<reason> -- the backend
// coding standard requires: match a decorated error with apperr.As(err)
// and compare its Code, never with == or errors.Is against the var below.
//
// This file deliberately declares no errors yet. The codebase's rule is
// that a code ships with the producer that returns it -- the reference
// app's notes module declares its codes next to the handler that raises
// them and has no errors.go at all -- and this module's first producers
// arrive in later blocks of this round: the delivery subscriber (which
// validates recipients and preferences) and the HTTP surface (which
// validates the read-path operations the inbox gains). Until then, every
// operation this module exposes is a promoted dbkit.Repository method, and
// its failures already speak dbkit's own vocabulary -- dbkit.record_not_found
// above all -- which needs no re-declaration here.
//
// When the first producer lands, mirror org's errors.go: one commented
// sentinel per failure mode, plus the matching description entries in
// locales/{zh-CN,en-US}.toml under the identical id, and an errors_test.go
// that pins every declared code to its catalog row and both translations.
