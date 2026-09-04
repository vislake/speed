package integration_test

// Runnable documentation for round 2's outbound-webhook surface, mirroring
// example_test.go's round-1 convention: compiled AND executed by `go test`,
// so a change to this module's public API that breaks the documented usage
// fails the build.
//
// This is also round 2's mandatory end-to-end proof of the internal-to-
// public event mapping mechanism (eventmapping.go's EventMapping,
// module.go's WithEventMapping): it maps a REAL, already-shipped domain
// event -- go/org's "org.member.joined" (go/org/events.go's
// EventMemberJoined, go/org's MemberJoined payload) -- onto a versioned
// public schema, entirely through this module's exported API.
//
// go/integration never imports go/org: this example proves it does not
// need to. The EventMapping below subscribes to the literal event type
// string "org.member.joined" and reads the payload STRUCTURALLY through
// JSON -- org.MemberJoined's own json tags ("membership_id", "user_id",
// "node_id", "invitation_id"), read as a map -- the identical no-import
// technique org.userIDFromPayload itself uses to read authn's events (see
// that function's own doc comment in go/org/events.go).
//
// # Why this example stops short of an actual HTTP delivery
//
// A full round trip needs a receiver this process can reach, and the only
// receiver an offline `go test` run can stand up is an httptest.Server --
// which listens on loopback. ValidateWebhookURL refuses loopback
// addresses by design (ssrf_test.go's own mandatory proof of exactly that
// refusal), so a subscription pointing at one is refused at creation, the
// same way it would be for any real tenant's misconfigured URL. This
// example therefore demonstrates subscription creation against a URL that
// genuinely passes validation (a public IP literal, which
// ValidateWebhookURL accepts without any network call at all) and the
// mapping mechanism's own output, while the signed HTTP round trip, the
// SSRF dial-time re-check, and delivery retry/dead-letter behavior are
// proven against a real httptest.Server in this module's own internal test
// suite (webhook_delivery_test.go, ssrf_test.go) -- compiled and run by
// `go test` exactly as this example is, just not as a godoc Example, since
// exercising the SSRF-guarded transport is something only a white-box test
// with this module's own unexported test seams can do without an actual
// public receiver to dial.
import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/integration"
)

// orgMemberJoinedPublicPayload is the public v1 schema this example maps
// org's real org.member.joined event onto -- a deliberately chosen SUBSET
// of org.MemberJoined's own fields (per docs/internal/07-platform-
// services.md's "only the deliberately chosen fields are exposed" rule):
// InvitationID, an internal bookkeeping detail, is left out of the public
// schema entirely.
type orgMemberJoinedPublicPayload struct {
	MembershipID string `json:"membership_id"`
	UserID       string `json:"user_id"`
	NodeID       string `json:"node_id"`
}

// ExampleModule_webhookDelivery declares the org.member.joined mapping,
// creates a tenant's webhook subscription for it, and shows the mapping's
// own Transform turning a real org.member.joined-shaped event into the
// versioned public payload a subscriber would receive.
func ExampleModule_webhookDelivery() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:integration_webhook_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	// A subscription's signing secret is encrypted at rest (see
	// integration.WebhookSecretSerializerName's own doc comment for why --
	// unlike an API key's hash, it must be readable again to sign every
	// delivery attempt). The key below is a literal only because this is an
	// example; a real host injects it from its own secret store.
	cipher, err := dbkit.NewCipher([]byte("example-webhook-secret-cipher-32"))
	if err != nil {
		fmt.Println("cipher:", err)
		return
	}
	dbkit.RegisterEncryptedSerializer(integration.WebhookSecretSerializerName, cipher)

	mapping := integration.EventMapping{
		InternalType:  "org.member.joined",
		PublicType:    "org.member.joined",
		PublicVersion: "v1",
		Transform: func(_ context.Context, evt pkgcore.Event) (json.RawMessage, error) {
			// Round-trip through JSON rather than a type assertion against
			// org.MemberJoined: a same-process publish would hand this
			// function org's own struct (whose json tags this code cannot
			// see at compile time without importing org), while the
			// distributed mode's Redis bus would hand it a map[string]any
			// built from those same tags. Both shapes decode identically
			// this way.
			raw, marshalErr := json.Marshal(evt.Payload)
			if marshalErr != nil {
				return nil, marshalErr
			}
			var fields orgMemberJoinedPublicPayload
			if unmarshalErr := json.Unmarshal(raw, &fields); unmarshalErr != nil {
				return nil, unmarshalErr
			}
			return json.Marshal(fields)
		},
	}

	m := integration.NewModule(db, integration.WithEventMapping(mapping))

	registry := dbkit.NewMigrationRegistry()
	if regErr := registry.Register(m); regErr != nil {
		fmt.Println("register migrations:", regErr)
		return
	}
	if applyErr := registry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations:", applyErr)
		return
	}

	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if regErr := m.Register(reg); regErr != nil {
		fmt.Println("register module:", regErr)
		return
	}
	svc, err := m.Attach(reg)
	if err != nil {
		fmt.Println("attach:", err)
		return
	}

	tenantCtx := pkgcore.WithTenant(ctx, pkgcore.TenantID("acme-dental"))

	// A public IP literal -- ValidateWebhookURL accepts it with no network
	// call at all (no DNS lookup is needed for a literal address), which is
	// what keeps this example self-contained. A loopback or private address
	// here would be refused exactly as ssrf_test.go's own mandatory proof
	// shows.
	created, err := svc.CreateWebhookSubscription(tenantCtx, integration.CreateWebhookSubscriptionInput{
		URL:        "https://93.184.216.34/webhooks/speed",
		EventTypes: []string{"org.member.joined"},
		CreatedBy:  "user-1",
	})
	if err != nil {
		fmt.Println("create subscription:", err)
		return
	}
	fmt.Println("subscription active:", created.Active)
	fmt.Println("subscription event types:", created.EventTypes)

	// The event a real org module publishes -- the exact JSON field names
	// org.MemberJoined's own struct tags carry (go/org/events.go).
	evt := pkgcore.Event{
		Type:     "org.member.joined",
		TenantID: pkgcore.TenantID("acme-dental"),
		Payload: map[string]any{
			"membership_id": "membership-1",
			"user_id":       "user-7",
			"node_id":       "node-1",
			"invitation_id": "", // deliberately not carried into the public payload
		},
	}
	body, err := mapping.Transform(ctx, evt)
	if err != nil {
		fmt.Println("transform:", err)
		return
	}
	fmt.Println("mapped payload:", string(body))

	// Output:
	// subscription active: true
	// subscription event types: [org.member.joined]
	// mapped payload: {"membership_id":"membership-1","user_id":"user-7","node_id":"node-1"}
}
