package testutil

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// RecordingBus is an EventBus test double for the terminal-signal tests of
// both Queue implementations: it records every published event, in publish
// order, so a test can assert exactly which events a terminal transition
// produced -- and an optional publish hook lets a test make Publish fail,
// block or observe, without a real broker.
//
// Deliberately generic in pkgcore types only: this package is imported by
// the root jobs package's own in-package tests, so it must not import jobs
// itself (the import cycle the module's test-layout record documents for
// this exact directory). Callers type-assert Payload to the concrete event
// payload.
type RecordingBus struct {
	mu     sync.Mutex
	events []pkgcore.Event
	hook   func(ctx context.Context, evt pkgcore.Event) error
}

// NewRecordingBus returns an empty RecordingBus.
func NewRecordingBus() *RecordingBus { return &RecordingBus{} }

// Publish records evt and then consults the optional publish hook, whose
// error is returned to the publisher unchanged. The record happens BEFORE
// the hook, so a test can both count publish attempts that the hook failed
// and let a failing hook drive the publisher's retry path.
func (b *RecordingBus) Publish(ctx context.Context, evt pkgcore.Event) error {
	b.mu.Lock()
	b.events = append(b.events, evt)
	hook := b.hook
	b.mu.Unlock()
	if hook != nil {
		return hook(ctx, evt)
	}
	return nil
}

// Subscribe satisfies EventBus as a no-op: these tests read what was
// published off the recording itself rather than installing handlers.
func (b *RecordingBus) Subscribe(string, pkgcore.EventHandler) {}

// SetPublishHook installs fn as the bus's publish behavior: it runs after
// each event is recorded, and its error becomes Publish's return value.
// A nil fn restores the default (record and succeed). The hook must be
// safe for concurrent use if the publishing queue's own goroutines may
// call Publish concurrently -- a Queue implementation's terminal points
// can publish from different goroutines.
func (b *RecordingBus) SetPublishHook(fn func(ctx context.Context, evt pkgcore.Event) error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hook = fn
}

// Events returns a snapshot of every event recorded so far, in publish
// order.
func (b *RecordingBus) Events() []pkgcore.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	snapshot := make([]pkgcore.Event, len(b.events))
	copy(snapshot, b.events)
	return snapshot
}

// WaitForPublish polls the recording until an event matching match appears
// (returning it) or timeout elapses, in which case the test fails with what
// and the events observed. Event-driven, so the timeout is paid only when
// the event never arrives; the poll cadence is short enough for the
// millisecond-scale dispatch intervals the jobs tests run their queues at.
func (b *RecordingBus) WaitForPublish(t *testing.T, timeout time.Duration, what string, match func(pkgcore.Event) bool) pkgcore.Event {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for _, evt := range b.Events() {
			if match(evt) {
				return evt
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s; recorded events = %+v", timeout, what, b.Events())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
