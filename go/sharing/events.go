package sharing

import (
	"context"
	"errors"

	"github.com/vislake/speed/go/pkgcore"
)

// This file is Service's shared home for the host seams it reads at call
// time and the domain-event payload vocabulary it publishes on them.
//
// Service never imports infrastructure: domain events go out on whatever
// pkgcore.EventBus the registry carries, and the sensitive-share audit
// action (service.go's emitSensitiveAudit) goes through whatever
// pkgcore.AuditActionRegistrar the registry declared its catalog against.
// Both are read from the registry at call time, never captured at
// construction, so a registry substituted after Module.Register (which
// does not happen in production, but does in tests) is honored by the next
// call -- the identical seam discipline go/storage's serviceHost and
// go/org's hostSeams document for their own services.

// hostSeams is the slice of *pkgcore.Registry Service reads. Declaring it
// as its own interface, rather than holding a *pkgcore.Registry directly,
// keeps Service testable against a fake and honest about the one thing it
// actually needs from the host: a bus to publish on.
type hostSeams interface {
	// EventBus returns the bus the registry's Events registrar installs
	// subscriptions on.
	EventBus() pkgcore.EventBus
}

// compile-time check that the concrete registry satisfies the seam.
var _ hostSeams = (*pkgcore.Registry)(nil)

// Plain, unexported wiring errors -- never returned to a caller as the
// final answer, only logged (Service.Create's emitSensitiveAudit call site)
// or wrapped, mirroring go/storage's identical plain-error convention for
// "the host never attached a registry" failures that are a wiring bug, not
// a business-rule refusal.
var (
	errShareNoHostRegistry = errors.New("sharing: no host registry wired")
	errShareNoEventBus     = errors.New("sharing: registry carries no event bus")
	errShareNoAuditActions = errors.New("sharing: registry carries no audit action registrar")
)

// attach wires the registry Module.Register attached against into Service.
// It is called exactly once, by Module.Register, and performs no I/O: it
// stores the registry slice and the audit-action registrar Service reads at
// call time, so Service always uses the registry's final wiring.
func (s *Service) attach(reg *pkgcore.Registry) {
	s.host = reg
	s.auditActions = reg.AuditActions
}

// publish sends one domain event on the registry's bus. Callers treat a
// returned error as a logged, recovered anomaly: the durable fact the event
// announces was already committed before publishing was attempted --
// go/storage's serviceHost.publish documents the identical "warn and stand"
// contract this mirrors.
func (s *Service) publish(ctx context.Context, evt pkgcore.Event) error {
	if s.host == nil {
		return errShareNoHostRegistry
	}
	bus := s.host.EventBus()
	if bus == nil {
		return errShareNoEventBus
	}
	return bus.Publish(ctx, evt)
}

// ShareCreatedPayload is the JSON payload of EventShareCreated.
type ShareCreatedPayload struct {
	ShareID     string `json:"share_id"`
	ResourceRef string `json:"resource_ref"`
	Sensitive   bool   `json:"sensitive"`
}

// ShareAccessedPayload is the JSON payload of EventShareAccessed -- the
// event docs/internal/07-platform-services.md's "relationship to other
// modules" section describes as flowing into compliance's own audit trail
// once that module exists (M4). This module only publishes; there is no
// subscriber in this history, the identical publish-now-subscriber-later
// shape go/dbkit/audit's write-capture events used before go/dbkit/audit
// itself existed to consume them.
type ShareAccessedPayload struct {
	ShareID  string `json:"share_id"`
	Granted  bool   `json:"granted"`
	IP       string `json:"ip"`
	Referrer string `json:"referrer"`
}

// ShareRevokedPayload is the JSON payload of EventShareRevoked.
type ShareRevokedPayload struct {
	ShareID string `json:"share_id"`
}
