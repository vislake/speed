package notification

import (
	"github.com/vislake/speed/go/pkgcore"
)

// The domain events notification publishes. Names follow pkgcore.EventDecl's
// <module>.<entity>.<action> convention, and the module's own delivery
// machinery is the first consumer of its own inbox event -- the platform's
// other modules publish the events that BECOME notifications; notification
// publishes the events that announce its own deliveries.
const (
	// EventInboxCreated announces that one message has been delivered into
	// one recipient's in-app inbox: the row is committed before the event
	// goes out, so a subscriber that mirrors the inbox elsewhere can read
	// the row back without racing its writer. The delivery job publishes it
	// after every successful inbox-row write, and the module's own Hub --
	// subscribed to it during Register -- is the first subscriber, fanning
	// the announcement out to the connections of its replica.
	EventInboxCreated = "notification.inbox.created"
)

// InboxCreatedPayload is the payload EventInboxCreated carries.
//
// Every field carries a json tag because a payload crossing the distributed
// mode's Redis bus is marshalled to JSON and arrives at the subscriber as a
// map keyed by these names. The tags are therefore part of the public event
// contract, not a serialization detail.
//
// TenantID repeats pkgcore.Event's own TenantID field on purpose: the
// event's primary consumer is a queue job running on a worker, and a
// worker's context carries no tenant -- the job must rebuild it from
// pkgcore.WithTenant(job.TenantID) -- so the tenant must travel inside the
// payload itself, where Event.TenantID does not reach. TypeKey is the
// notification type the message was rendered from, and MessageID and
// RecipientUserID name the inbox row and its owner. A subscriber never
// needs the body: it reads the row through Repository, tenant context
// rebuilt from this payload.
type InboxCreatedPayload struct {
	MessageID       string `json:"message_id"`
	TenantID        string `json:"tenant_id"`
	RecipientUserID string `json:"recipient_user_id"`
	TypeKey         string `json:"type_key"`
}

// inboxEventDecls is the catalog entry for each of the module's events.
//
// They are declared here, in the module's single Register call, because
// pkgcore.Registry is where the platform's event catalog is assembled --
// observability, compliance and integration enumerate the declarations
// without subscribing to any of them.
var inboxEventDecls = []pkgcore.EventDecl{
	{
		Type:        EventInboxCreated,
		PayloadType: "notification.InboxCreatedPayload",
		Description: "A message was delivered into one recipient's in-app inbox.",
	},
}
