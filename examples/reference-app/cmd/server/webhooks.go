// The reference app's demo glue for go/integration's round-2 outbound-
// webhook DELIVERY surface: the EventMapping that turns org's real
// "org.member.joined" domain event into a versioned public webhook payload.
// webhook_flow_test.go drives delivery through the composed HTTP stack
// against a real receiver process this app's own test controls.
//
// Round 4's own hand-mounted subscription-management route lived here too --
// wireIntegrationWebhooks mounting POST /api/v1/demo/webhooks/subscriptions
// by hand, outside the OpenAPI machinery, because go/integration shipped no
// HTTP surface of its own for webhook-subscription CRUD at the time.
// Round 7 (go/integration's webhook-subscription-CRUD HTTP round) retires
// that route: the spec-generated surface (go/integration/api/openapi.yaml,
// mounted under /api/v1/integration) now serves create, list, update,
// delete, restore and the recent-deliveries listing through the SAME generic
// route-mounting machinery every other module's fragment uses, and
// webhook_crud_flow_test.go drives it. webhook_flow_test.go's own create-
// subscription legs were re-pointed at that surface accordingly -- this file
// keeps the EventMapping machinery only, which no spec fragment can replace
// (it is host-side code closing a Transform over org's own payload shape).
// Round 4's delivery-side record survives in go/integration/AGENTS.md's
// "Reference app: mandatory first consumer (round 2's webhook surface)"
// section.
package main

import (
	"context"
	"encoding/json"

	"github.com/vislake/speed/go/integration"
	"github.com/vislake/speed/go/pkgcore"
)

// orgMemberJoinedWebhookPublicPayload is the public v1 schema
// orgMemberJoinedWebhookMapping's Transform produces -- a deliberately
// chosen SUBSET of org.MemberJoined's own fields, per docs/internal/07-
// platform-services.md's "only the deliberately chosen fields are exposed"
// rule: InvitationID, an internal bookkeeping detail, is left out of the
// public schema entirely. This mirrors go/integration's own
// webhook_example_test.go ExampleModule_webhookDelivery almost exactly --
// that example proves the mapping mechanism offline; this file is the same
// mapping wired into a real, running application.
type orgMemberJoinedWebhookPublicPayload struct {
	MembershipID string `json:"membership_id"`
	UserID       string `json:"user_id"`
	NodeID       string `json:"node_id"`
}

// orgMemberJoinedWebhookMapping declares this app's one EventMapping: org's
// real, already-shipped "org.member.joined" event (go/org/events.go's
// EventMemberJoined) fans out to any subscription naming the identical
// public type "org.member.joined" at schema version "v1".
//
// go/integration never imports go/org: Transform reads the payload
// STRUCTURALLY through JSON -- org.MemberJoined's own json tags
// ("membership_id", "user_id", "node_id", "invitation_id"), read as a map --
// the identical no-import technique org.userIDFromPayload itself uses to
// read authn's events (go/org/events.go) and go/integration's own
// webhook_example_test.go uses to read this exact event. This is exactly
// why EventMapping.Transform is a plain function this app supplies rather
// than something go/integration could derive on its own: only a host that
// already imports both org and go/integration can close a Transform over
// org's own payload shape.
var orgMemberJoinedWebhookMapping = integration.EventMapping{
	InternalType:  "org.member.joined",
	PublicType:    "org.member.joined",
	PublicVersion: "v1",
	Description:   "A person became a member of a tenant.",
	Transform: func(_ context.Context, evt pkgcore.Event) (json.RawMessage, error) {
		// A same-replica publish hands this function org's own
		// org.MemberJoined struct; the distributed mode's Redis bus would
		// hand it a map[string]any built from the identical json tags.
		// Round-tripping through JSON decodes both shapes identically
		// without this app needing a type assertion against a struct
		// go/integration itself is not allowed to import.
		raw, err := json.Marshal(evt.Payload)
		if err != nil {
			return nil, err
		}
		var fields orgMemberJoinedWebhookPublicPayload
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, err
		}
		return json.Marshal(fields)
	},
}
