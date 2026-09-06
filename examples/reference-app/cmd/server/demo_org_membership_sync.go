// wireDemoOrgMembershipSync closes the gap Finding 3 of the
// reference-app-go.md audit confirmed: org's own real accept-invitation
// flow (go/org/invite.go's InviteService.Accept) creates a genuine
// Membership row, but that row was permanently disconnected from
// demoMemberships -- this app's own authn.MembershipReader stand-in
// (server.go) -- so an invited user who really accepted through the real
// HTTP flow could never sign in: authn's own sign-in path answers 403
// authn.tenant_membership_required forever, since nothing told
// demoMemberships the new membership existed.
//
// The fix follows demo_notification.go's own canonical shape (its own
// package doc comment): a business module (here, org) publishes a domain
// event as a fact, and this app's own glue subscribes to it and reacts --
// never the reverse, and never a new import edge either module needs to
// carry. org.EventMemberJoined ("org.member.joined") is published on EVERY
// path that creates a Membership row -- both InviteService.Accept
// (InvitationID set) and the authn.user.created auto-provisioning
// subscriber (InvitationID empty) -- so subscribing to it once covers both,
// which is exactly right: demoMemberships.Grant is idempotent (its own doc
// comment), so a user this app already granted through some other path
// (demo_users.go's seed, registerAndAuthenticate's test shortcut) is
// unaffected by a redundant Grant call the auto-provisioning path's own
// event triggers.
package main

import (
	"context"
	"encoding/json"

	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
)

// wireDemoOrgMembershipSync subscribes memberships to org's own
// org.member.joined event, so a real Membership row org creates --
// through a real accepted invitation, or through org's own
// authn.user.created auto-provisioning -- is reflected into authn's
// sign-in path. bus is reg.EventBus(), the same bus org itself publishes
// on, mirroring wireDemoNotification's own wiring (server.go's call site).
//
// The subscription cannot fail: an unreadable or incomplete payload is
// logged and dropped, never returned as an error, for the identical reason
// wireDemoNotification's own note-created subscription gives -- a
// subscriber's confusion about a payload must never fail the write that
// published it (org's own membership creation, here).
func wireDemoOrgMembershipSync(bus pkgcore.EventBus, memberships *demoMemberships) {
	bus.Subscribe(org.EventMemberJoined, func(ctx context.Context, evt pkgcore.Event) error {
		logger := observability.FromContext(ctx)

		if evt.TenantID == "" {
			logger.Debug("demo membership-sync glue ignored an org.member.joined event with no tenant",
				"event_type", evt.Type)
			return nil
		}

		// This app is the assembling application, not a business module, so
		// it is permitted to decode straight into org's own exported
		// org.MemberJoined struct -- the same admin/tenant_service.go
		// precedent (go/admin) documents for its identical org.NodeCreated
		// case -- rather than hand-probing a JSON map the way a business
		// module crossing another business module's boundary must. The
		// round-trip through JSON, rather than a direct type assertion, is
		// still required: a cross-replica delivery over pkgcore's Redis
		// EventBus arrives as a map[string]any, never as org's own publishing
		// struct, exactly as that same precedent's doc comment explains.
		var payload org.MemberJoined
		encoded, err := json.Marshal(evt.Payload)
		if err != nil {
			logger.Warn("demo membership-sync glue dropped an org.member.joined event with an unmarshalable payload",
				"event_type", evt.Type, "error", err)
			return nil
		}
		if err := json.Unmarshal(encoded, &payload); err != nil {
			logger.Warn("demo membership-sync glue dropped an org.member.joined event with an unreadable payload",
				"event_type", evt.Type, "error", err)
			return nil
		}
		if payload.UserID == "" {
			logger.Warn("demo membership-sync glue dropped an org.member.joined event naming no user",
				"event_type", evt.Type)
			return nil
		}

		memberships.Grant(payload.UserID, evt.TenantID)
		return nil
	})
}
