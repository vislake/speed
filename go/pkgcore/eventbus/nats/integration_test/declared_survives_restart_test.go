//go:build integration

package nats_test

// The SurvivesRestart half of this implementation's declaration, verified:
// the component descriptor (component.go) declares MultiReplicaSafe |
// SurvivesRestart (component_test.go pins the descriptor's declaration to
// the exported Capabilities constant), the
// shared eventbustest suite verifies the MultiReplicaSafe half, and this
// file verifies the SurvivesRestart half against a genuine restart of the
// state-holding service -- the NATS container itself, restarted between a
// committed publish and the read-back of the committed stream state. See
// EventBus's own package doc comment for what the declaration promises (and
// what it does not): the bus keeps no state of its own; the JetStream
// stream entries it reads and writes live inside the NATS server, on the
// file storage that is the zero value of StreamConfig.Storage and that this
// package never overrides -- and, because the server's JetStream store
// directory sits in the container's own filesystem, the same container's
// stop/start is a genuine restart of that storage rather than a
// recreation.

import (
	"bytes"
	"context"
	"testing"

	natslib "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/vislake/speed/go/pkgcore"
	eventbusnats "github.com/vislake/speed/go/pkgcore/eventbus/nats"
)

// TestEventBus_DeclaredSurvivesRestart_CommittedStateSurvivesServerRestart
// is the verification the registration's SurvivesRestart declaration rests
// on: state the bus committed through the NATS server before a genuine
// restart of that server must still be there afterwards, and the live bus
// must resume ordinary operation across the restart. The proof has teeth
// against exactly the failure mode the declaration's premise names: a
// JetStream stream on memory storage loses every message when the server
// restarts (the failure mode kv/nats's own MemoryStorage-adoption refusal
// closes one seam over), so the post-restart stream-state assertions below
// fail against such a server and pass only against the file storage this
// bus's streams default to and never override.
func TestEventBus_DeclaredSurvivesRestart_CommittedStateSurvivesServerRestart(t *testing.T) {
	ctx := context.Background()
	container, connA, connB := startNATSConnPairWithContainer(t, ctx)

	busA := eventbusnats.NewEventBus(connA) // publisher only: subscribes no handler
	busB := eventbusnats.NewEventBus(connB)
	t.Cleanup(busA.Close)
	t.Cleanup(busB.Close)

	const eventType = "eventbus_nats_test.survives-restart"
	rec := &eventRecorder{}
	busB.Subscribe(eventType, rec.handler())

	// Warm up: proves busB's durable consumer exists on the stream and is
	// actually consuming (see warmUp's own doc comment for why a single
	// publish-and-wait would not), so the counted event below is delivered
	// before the restart.
	warmUp(t, busA, rec, eventType)
	rec.clear()

	// Commit the event whose survival the restart must not disturb. Its
	// delivery to the receiver is asserted first, so the state captured
	// below is the steady state a live, caught-up consumer left behind.
	if err := busA.Publish(ctx, pkgcore.Event{
		Type:     eventType,
		TenantID: pkgcore.TenantID("tenant-restart"),
		Payload:  invoicePaid{ID: "inv-restart", Amount: 7},
	}); err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}
	eventually(t, "the receiver to deliver the pre-restart event", func() bool {
		return rec.count() == 1
	})

	// Capture the committed server-side state the declaration promises will
	// outlive the restart: the stream's message count and last sequence, and
	// the exact bytes of the last (counted) message. The captured values are
	// the pre-restart baseline the post-restart assertions compare against.
	js, err := jetstream.New(connB)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	streamName := streamNameForEventType(eventType)
	stream, err := js.Stream(ctx, streamName)
	if err != nil {
		t.Fatalf("js.Stream(%q): %v", streamName, err)
	}
	preInfo, err := stream.Info(ctx)
	if err != nil {
		t.Fatalf("stream.Info() before the restart: %v", err)
	}
	preLastSeq := preInfo.State.LastSeq
	preMsgs := preInfo.State.Msgs
	if preMsgs == 0 {
		t.Fatal("stream carries no messages before the restart, want the committed event present")
	}
	preMessage, err := stream.GetMsg(ctx, preLastSeq)
	if err != nil {
		t.Fatalf("stream.GetMsg(%d) before the restart: %v", preLastSeq, err)
	}

	// The genuine restart of the state-holding service.
	restartNATSContainer(t, ctx, container, connB)

	// The committed stream state must still be there, recovered from the
	// server's JetStream store. These are the assertions that fail against a
	// memory-storage stream: its messages die with the server, so the stream
	// comes back empty (or not at all) and the counted message is gone. The
	// recovery of a file-storage stream can lag the connection's own
	// reconnect by a moment, so the state's arrival is polled and then
	// compared strictly.
	eventually(t, "the stream to be recovered with its pre-restart message count and last sequence intact", func() bool {
		recovered, err := js.Stream(ctx, streamName)
		if err != nil {
			return false
		}
		info, err := recovered.Info(ctx)
		if err != nil {
			return false
		}
		return info.State.Msgs == preMsgs && info.State.LastSeq == preLastSeq
	})

	recoveredStream, err := js.Stream(ctx, streamName)
	if err != nil {
		t.Fatalf("js.Stream(%q) after the restart: %v", streamName, err)
	}
	postInfo, err := recoveredStream.Info(ctx)
	if err != nil {
		t.Fatalf("stream.Info() after the restart: %v", err)
	}
	if got := postInfo.State.Msgs; got != preMsgs {
		t.Errorf("stream carries %d messages after the restart, want the %d committed before it", got, preMsgs)
	}
	postMessage, err := recoveredStream.GetMsg(ctx, preLastSeq)
	if err != nil {
		t.Fatalf("stream.GetMsg(%d) after the restart: %v", preLastSeq, err)
	}
	if !bytes.Equal(postMessage.Data, preMessage.Data) {
		t.Errorf("the message at the pre-restart last sequence changed across the server restart:\nbefore: %q\nafter:  %q", preMessage.Data, postMessage.Data)
	}
	// The receiver's durable consumer must have been recovered alongside the
	// stream -- the server-side reader state the bus relies on.
	names := consumerNames(t, ctx, js, streamName)
	if len(names) != 1 {
		t.Fatalf("stream %q carries %d consumers after the restart, want the receiver's single durable one", streamName, len(names))
	}

	// And the live bus must resume ordinary operation across the restart:
	// its consumer (recovered server-side, adopted by the bus's own reader
	// recovery path) delivers a post-restart publish. The publisher's
	// connection must be back up first -- its reconnect is what the publish
	// below travels over.
	eventually(t, "the publisher connection to reconnect to the restarted server", func() bool {
		return connA.Status() == natslib.CONNECTED
	})
	rec.clear()
	if err := busA.Publish(ctx, pkgcore.Event{
		Type:     eventType,
		TenantID: pkgcore.TenantID("tenant-restart"),
		Payload:  invoicePaid{ID: "inv-restart-after", Amount: 8},
	}); err != nil {
		t.Fatalf("Publish() after the restart error = %v, want nil", err)
	}
	eventually(t, "the receiver to deliver the post-restart event", func() bool {
		return rec.count() == 1
	})
}
