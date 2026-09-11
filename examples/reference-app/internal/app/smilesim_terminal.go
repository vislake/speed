package app

import (
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/examples/reference-app/internal/smilesim"
)

// wireSmilesimTerminalSignal installs internal/smilesim's subscription to
// the queue's terminal signal, jobs.job.terminal: the publisher is the
// standalone queue itself (server.go constructs it with
// jobs.WithEventBus(bus), so every Job that reaches a terminal status owes
// one publish per terminal transition), and the subscriber is
// smilesim.Service.OnJobTerminal, which settles that job's credit
// reservation at the transition -- no client poll, no reconciliation-sweep
// delay -- and publishes EventSimulationCompleted for a job Simulate was
// given a recipient for.
//
// events is the assembly view's event seat, so the subscription rides the
// very bus the view resolved -- the bus the queue was handed -- and lands
// while the declaration seats are open, which is the app component's Init.
// Installing it there, at assembly time, is the host's side of the consumer
// contract jobs.EventJobTerminal documents: the signal lands on a bus with
// nothing subscribed to it as a side-effect-free no-op, so a host that
// wants settlement at the transition subscribes before it serves.
//
// The poll-driven call site (smilesim.go's job-status route,
// svc.NotifyOnCompletion) stays installed beside this subscription: it is
// the second leg for a transition this replica never saw a signal for, and
// both legs publish through the one latch, so the recipient still gets at
// most one delivery.
//
// The call cannot fail: events.Subscribe returns nothing, mirroring
// WireDemoNotification's own no-error shape.
func wireSmilesimTerminalSignal(events pkgcore.EventRegistrar, svc *smilesim.Service) {
	events.Subscribe(jobs.EventJobTerminal, svc.OnJobTerminal)
}
