// The reference app's demo glue for go/integration's round-2 outbound-
// webhook surface: the EventMapping that turns org's real "org.member.joined"
// domain event into a versioned public webhook payload, and the one
// hand-written route that lets a demo tenant create a subscription for it.
// webhook_flow_test.go drives both through the composed HTTP stack against
// a real receiver process this app's own test controls.
//
// go/integration ships no HTTP surface of its own for webhook-subscription
// CRUD this round (go/integration/AGENTS.md's "Deliberately not in scope"
// table -- "A mounted HTTP surface / OpenAPI fragment for webhook
// subscription CRUD ... A later round"), so this route is mounted by hand,
// outside the OpenAPI machinery, the same pattern consult.go's and
// smilesim.go's own demo routes already establish in this app.
package main

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/vislake/speed/go/integration"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/rbac"
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

// webhookSubscriptionsPath is this app's one hand-written webhook-management
// route: POST it with a JSON body naming a receiving URL and the public
// event types to subscribe to ({"url": "...", "event_types": [...]}), and
// the response is the created subscription, its one-time signing secret
// included -- integration.CreatedWebhookSubscription's own "shown once"
// contract (go/integration/webhook_service.go).
const webhookSubscriptionsPath = "/api/v1/demo/webhooks/subscriptions"

// integrationErrInternal folds any error CreateWebhookSubscription returns
// that is not itself an *apperr.Error into a stable code, the same fallback
// consult.go's own writeConsultError applies.
var integrationErrInternal = apperr.Internal("integration.internal_error")

// wireIntegrationWebhooks mounts webhookSubscriptionsPath on mux, backed by
// svc and gated on integration.PermissionWebhookManage. Unlike consult's and
// smilesim's own demo routes, this one IS gated: the module already
// declares integration.PermissionWebhookManage as its vocabulary
// (go/integration/module.go) with no enforcement point of its own
// (go/integration/AGENTS.md's own module doc: "this module does not check
// them itself -- it declares the vocabulary; enforcement is whatever
// authorization layer the host wires"), and this round's real HTTP
// consumer is exactly the place that vocabulary was always meant to be
// enforced from -- demoOwnerUserID's built-in owner role already carries it
// (demo_subject.go's own doc comment: "every permission any module
// declared"), so no additional grant seeding is needed for the demo owner
// account to use this route.
//
// # Why this calls az.Can directly rather than rbac.RequirePermissionFunc
//
// Every OTHER gated route in this app (guardModuleRoute, guardAdminRoute)
// wraps its handler in rbac.RequirePermissionFunc, which takes a single
// "<resource>:<action>" string and splits it on its FIRST colon
// (go/rbac/middleware.go's splitPermission) -- a shape every other module's
// permission vocabulary in this codebase happens to fit (one colon each:
// "notes:read", "admin:tenants_manage", ...). This module's own permission
// constants are declared "<module>:<entity>:<verb>"
// -- integration.PermissionWebhookManage is "integration:webhook:manage",
// TWO colons -- which splitPermission's own contract (its doc comment:
// "any string that is not \"<resource>:<action>\" denies the request")
// refuses outright, regardless of what the caller actually holds: this is
// exactly the shape mismatch a first real HTTP consumer of this
// permission was always going to expose, since no route ever drove it
// through rbac's gate before this round. Changing splitPermission's own
// one-colon contract is out of this round's scope -- every already-shipped
// gated route in this codebase relies on it unchanged -- so this route
// calls az.Can(ctx, sub, action, resource) directly with the split
// PermissionWebhookManage's own name already implies: resource
// "integration:webhook", action "manage". rbac.Permission(resource, action)
// (go/rbac's own helper, used identically by demoPermissionFor for every
// OTHER route) reassembles that back into the exact declared string
// "integration:webhook:manage" for the lookup, so this checks precisely the
// permission the module declares -- nothing weaker, nothing wider.
func wireIntegrationWebhooks(mux *http.ServeMux, az rbac.Authorizer, svc *integration.Service) {
	mux.HandleFunc(http.MethodPost+" "+webhookSubscriptionsPath, func(w http.ResponseWriter, r *http.Request) {
		sub, ok := demoSubjectResolver(r)
		if !ok {
			writeIntegrationError(w, rbac.ErrPermissionDenied.WithParam("permission", integration.PermissionWebhookManage))
			return
		}
		allowed, err := az.Can(r.Context(), sub, "manage", "integration:webhook")
		if err != nil || !allowed {
			writeIntegrationError(w, rbac.ErrPermissionDenied.WithParam("permission", integration.PermissionWebhookManage))
			return
		}

		var body struct {
			URL        string   `json:"url"`
			EventTypes []string `json:"event_types"`
		}
		if decodeErr := json.NewDecoder(r.Body).Decode(&body); decodeErr != nil {
			writeIntegrationError(w, apperr.Invalid("integration.invalid_request_body").WithCause(decodeErr))
			return
		}

		created, err := svc.CreateWebhookSubscription(r.Context(), integration.CreateWebhookSubscriptionInput{
			URL:        body.URL,
			EventTypes: body.EventTypes,
			CreatedBy:  sub.UserID,
		})
		if err != nil {
			writeIntegrationError(w, err)
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		//nolint:gosec // G117: created.Secret is the raw HMAC signing secret
		// integration.CreatedWebhookSubscription's own doc comment documents
		// as "the one and only place the raw signing secret is ever
		// available" -- a deliberate, one-time disclosure to the caller who
		// just created it, the identical shape round 1's CreatedAPIKey.Key
		// and go/org's invitation token already establish elsewhere in this
		// codebase, never a stored secret leaking into a response by
		// accident.
		_ = json.NewEncoder(w).Encode(created)
	})
}

// writeIntegrationError writes err to w as a JSON {code, params} body, the
// same structured-error envelope shape consult.go's own writeConsultError
// produces -- a stable code plus structured parameters, never localized
// text (backend coding standard §6.2).
func writeIntegrationError(w http.ResponseWriter, err error) {
	appErr, ok := apperr.As(err)
	if !ok {
		appErr = integrationErrInternal
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(appErr.Status)
	envelope := map[string]any{"code": appErr.Code}
	if appErr.Params != nil {
		envelope["params"] = appErr.Params
	}
	_ = json.NewEncoder(w).Encode(envelope)
}
