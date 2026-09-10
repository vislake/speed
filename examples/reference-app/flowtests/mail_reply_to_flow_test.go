package flowtests

import (
	"net/http"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/app"
)

// mail_reply_to_flow_test.go drives the Reply-To wiring end to end through
// the real composed stack -- the modules' own send points reached over HTTP,
// the messages captured by the injected double. Two legs, one per module
// this app assembles with a Reply-To:
//
//   - org: an invitation created through the real route carries the
//     assembly-configured Reply-To on the captured mail;
//   - notification: the contact double opt-in's verification code email
//     carries it too (the contact-email send point, the module's other mail
//     path).
//
// Both legs assert against the literal internal/app/server.go wires into
// org.WithReplyTo and notification.WithReplyTo, so a change to either
// wiring value must be a deliberate edit here as well.

// assemblyReplyTo is the Reply-To address internal/app/server.go passes to
// org.WithReplyTo and notification.WithReplyTo at assembly time. The literal
// is the pin: editing either wiring value without updating it fails these
// legs.
const assemblyReplyTo = "support@reference-app.example"

// TestMailReplyTo_OrgInvitation_CarriesTheAssemblyReplyTo walks the
// invitation journey a browser drives -- register, authenticate, create the
// root node, invite -- and asserts the invitation mail the invitee receives
// tells replies to go to the assembly-configured address rather than the
// sender it went out from.
func TestMailReplyTo_OrgInvitation_CarriesTheAssemblyReplyTo(t *testing.T) {
	srv, cfg, mailer := buildOrgTestServer(t)
	token := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "reply-to-org")

	var root orgNode
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", token, "",
		map[string]string{"name": "Reply-To Group", "kind": "group"}, &root)
	if root.ID == "" {
		t.Fatal("created root carried no id")
	}

	const inviteeEmail = "reply-to-invitee@example.com"
	var invitation orgInvitation
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/invitations", token, "user-reply-to-inviter",
		map[string]string{"email": inviteeEmail, "nodeId": root.ID}, &invitation)
	if invitation.Status != "pending" {
		t.Fatalf("invitation = %+v, want status \"pending\"", invitation)
	}

	mail := mailer.last(t)
	if len(mail.To) != 1 || mail.To[0] != inviteeEmail {
		t.Errorf("invitation mail To = %v, want exactly [%q]", mail.To, inviteeEmail)
	}
	if mail.ReplyTo != assemblyReplyTo {
		t.Errorf("invitation mail ReplyTo = %q, want the assembly's %q", mail.ReplyTo, assemblyReplyTo)
	}
}

// TestMailReplyTo_NotificationContactCode_CarriesTheAssemblyReplyTo walks
// notification's synchronous mail path -- creating an external email contact
// sends its verification code before the create answers -- and asserts the
// captured message carries the assembly-configured Reply-To.
func TestMailReplyTo_NotificationContactCode_CarriesTheAssemblyReplyTo(t *testing.T) {
	srv, cfg, mailer, _ := buildNotifTestServer(t)
	token := registerAndAuthenticate(t, srv, cfg, app.DemoSingleTenantID, "reply-to-notif")
	subject := app.DemoNotesCreatorUserID

	const contactEmail = "reply-to-contact@example.com"
	var contact notifContact
	notifRequest(t, srv, http.MethodPost, "/api/v1/notifications/contacts", token, subject,
		map[string]string{"channel": "email", "address": contactEmail}, http.StatusCreated, &contact)
	if contact.Status != "pending" {
		t.Fatalf("created contact = %+v, want a pending email contact", contact)
	}

	mail := mailer.last(t)
	if len(mail.To) != 1 || mail.To[0] != contactEmail {
		t.Errorf("verification-code mail To = %v, want exactly [%q]", mail.To, contactEmail)
	}
	if mail.ReplyTo != assemblyReplyTo {
		t.Errorf("verification-code mail ReplyTo = %q, want the assembly's %q", mail.ReplyTo, assemblyReplyTo)
	}
}
