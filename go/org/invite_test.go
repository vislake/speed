package org

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/org/internal/testutil"
	"github.com/vislake/speed/go/org/migrations"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/ratelimit"
)

// inviteFixture is the shape every invitation test starts from: a wired
// module, a tenant tree, and one member doing the inviting.
type inviteFixture struct {
	m     *Module
	host  *testHost
	ctx   context.Context
	root  *OrgNode
	left  *OrgNode
	right *OrgNode
}

func newInviteFixture(t *testing.T) inviteFixture {
	t.Helper()
	m, host := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	root, left, right := seedTree(t, m.Tree(), ctx)
	if _, err := m.Members().Add(ctx, "u-inviter", root.ID); err != nil {
		t.Fatalf("Add(inviter): %v", err)
	}
	return inviteFixture{m: m, host: host, ctx: ctx, root: root, left: left, right: right}
}

// invite issues one invitation with the fixture's defaults.
func (f inviteFixture) invite(t *testing.T, email string) *InviteResult {
	t.Helper()
	result, err := f.m.Invitations().Invite(f.ctx, InviteRequest{
		Email:         email,
		NodeID:        f.left.ID,
		InviterUserID: "u-inviter",
		Locale:        "en-US",
	})
	if err != nil {
		t.Fatalf("Invite(%s): %v", email, err)
	}
	return result
}

func TestInviteService_Invite_CreatesStoresAndSends(t *testing.T) {
	f := newInviteFixture(t)
	result := f.invite(t, "Ada.Lovelace@Example.test")

	if result.Token == "" {
		t.Fatal("Invite returned no token")
	}
	inv := result.Invitation
	if inv.Status != InvitationStatusPending {
		t.Errorf("status = %q, want %q", inv.Status, InvitationStatusPending)
	}
	if inv.NodeID != f.left.ID {
		t.Errorf("node = %q, want %q", inv.NodeID, f.left.ID)
	}
	if inv.Locale != "en-US" {
		t.Errorf("locale = %q, want en-US", inv.Locale)
	}
	if !inv.ExpiresAt.After(time.Now()) {
		t.Errorf("ExpiresAt = %v, want a future time", inv.ExpiresAt)
	}

	// THE TOKEN IS NEVER STORED. Read the row back and confirm it holds the
	// hash and nothing else that could reconstruct the link.
	stored, err := f.m.Invitations().Repository().FindByID(f.ctx, inv.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if stored.TokenHash != hashInvitationToken(result.Token) {
		t.Error("the stored hash does not match the issued token")
	}
	if strings.Contains(stored.TokenHash, result.Token) {
		t.Error("the stored hash contains the token itself")
	}

	// One message, to the invited address, from the configured sender.
	sent := f.host.mailer.messages()
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sent))
	}
	if len(sent[0].To) != 1 || sent[0].To[0] != "Ada.Lovelace@Example.test" {
		t.Errorf("message To = %v, want the invited address", sent[0].To)
	}
	if sent[0].From != testMailFrom {
		t.Errorf("message From = %q, want %q", sent[0].From, testMailFrom)
	}
	if !strings.Contains(sent[0].Text, testLinkBase+result.Token) {
		t.Error("the message body does not carry the accept link")
	}

	// And the invitation was announced. The payload identifies the row it
	// announces -- never the invitee, and never a derivation of the invitee's
	// address: org.member.invited carries no blind index (MemberInvited's own
	// doc comment in events.go). The address discipline of the serialized
	// form a broker actually carries is pinned separately, against this same
	// real publish path, by
	// TestInviteService_Invite_PublishedMemberInvited_ExposesNoBlindIndex.
	invited := f.host.bus.events(EventMemberInvited)
	if len(invited) != 1 {
		t.Fatalf("published %d member-invited events, want 1", len(invited))
	}
	payload, ok := invited[0].Payload.(MemberInvited)
	if !ok {
		t.Fatalf("payload is %T, want org.MemberInvited", invited[0].Payload)
	}
	if payload.InvitationID != inv.ID {
		t.Errorf("payload invitation_id = %q, want the invitation's own id %q", payload.InvitationID, inv.ID)
	}
}

// TestInviteService_Invite_PublishedMemberInvited_ExposesNoBlindIndex pins
// the wire contract of org.member.invited at its real publish site: the
// payload identifies the invitation and its context, and it must carry
// NEITHER the invitee's address NOR the address's blind index.
//
// Why the blind index counts as much as the address: an HMAC digest of a
// low-entropy value is dictionary-reversible for any holder with a candidate
// list, and an event payload is written to a broker, logged by whoever
// subscribes and often traced -- in the distributed deployment mode the
// broker is a Redis Streams cross-process boundary. The in-process struct is
// not the contract; the JSON form the bus actually carries is (events.go's
// payload doc block), so the assertions run against the serialized payload:
// no "email_index" key, and not the invitation's index value in the bytes.
// A subscriber that must reach the invitee reads the invitation row through
// org, where the address is encrypted at rest.
func TestInviteService_Invite_PublishedMemberInvited_ExposesNoBlindIndex(t *testing.T) {
	f := newInviteFixture(t)
	result := f.invite(t, "ada@example.test")

	invited := f.host.bus.events(EventMemberInvited)
	if len(invited) != 1 {
		t.Fatalf("published %d member-invited events, want 1", len(invited))
	}
	payload, ok := invited[0].Payload.(MemberInvited)
	if !ok {
		t.Fatalf("payload is %T, want org.MemberInvited", invited[0].Payload)
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal the published payload: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("unmarshal the marshaled payload: %v", err)
	}
	for _, key := range []string{"email", "email_index"} {
		if _, present := fields[key]; present {
			t.Errorf("published payload carries the %q key: %s", key, encoded)
		}
	}
	if bytes.Contains(encoded, []byte(result.Invitation.EmailIndex)) {
		t.Errorf("serialized payload carries the invitation's blind index %q: %s",
			result.Invitation.EmailIndex, encoded)
	}
	for key, want := range map[string]string{
		"invitation_id":   result.Invitation.ID,
		"node_id":         result.Invitation.NodeID,
		"inviter_user_id": result.Invitation.InviterUserID,
	} {
		got, present := fields[key].(string)
		if !present || got != want {
			t.Errorf("payload %s = %v, want %q", key, fields[key], want)
		}
	}
}

// TestInviteService_Invite_RateLimited_PerEmail is the dimension that
// protects the RECIPIENT: an unverified address may only be written to as
// part of the consent-establishing message, and a handful a day is the line
// between an invitation and harassment.
func TestInviteService_Invite_RateLimited_PerEmail(t *testing.T) {
	f := newInviteFixture(t)
	const address = "ada@example.test"

	for i := range invitesPerEmailRate {
		if _, err := f.m.Invitations().Invite(f.ctx, InviteRequest{
			Email: address, NodeID: f.left.ID, InviterUserID: "u-inviter",
		}); err != nil {
			t.Fatalf("invite %d: %v", i+1, err)
		}
	}

	_, err := f.m.Invitations().Invite(f.ctx, InviteRequest{
		Email: address, NodeID: f.left.ID, InviterUserID: "u-inviter",
	})
	if !hasCode(err, ErrInvitationRateLimited.Code) {
		t.Fatalf("invite %d error = %v, want org.invitation_rate_limited", invitesPerEmailRate+1, err)
	}
	if got := errParam(t, err, "dimension"); got != "email" {
		t.Errorf("denied dimension = %v, want email", got)
	}

	// A different address is unaffected: the dimensions are independent.
	if _, err := f.m.Invitations().Invite(f.ctx, InviteRequest{
		Email: "grace@example.test", NodeID: f.left.ID, InviterUserID: "u-inviter",
	}); err != nil {
		t.Errorf("a different address was denied too: %v", err)
	}
}

// TestInviteService_Invite_RateLimitKeyIsTheBlindIndexNotTheAddress consumes
// the per-address budget through the limiter directly, under the key built
// from the BLIND INDEX, and then shows the next invite is denied.
//
// If the service keyed on the plaintext address instead, the pre-consumed
// budget would not match and the invite would be allowed -- so this failing
// is exactly the PII leak it is written to prevent: a rate-limit key lives in
// the KV store, is visible in Redis and tends to appear in diagnostics.
func TestInviteService_Invite_RateLimitKeyIsTheBlindIndexNotTheAddress(t *testing.T) {
	f := newInviteFixture(t)
	const address = "ada@example.test"

	index, err := newTestEmailIndexer(t).Index(address)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if strings.Contains(index, "@") {
		t.Fatalf("the blind index %q still looks like an address", index)
	}

	limiter := ratelimit.New(f.host.kv)
	limit := ratelimit.Limit{Rate: invitesPerEmailRate, Per: invitesPerEmailWindow}
	for range invitesPerEmailRate {
		if _, allowErr := limiter.Allow(f.ctx, "org:invite:email:"+index, limit); allowErr != nil {
			t.Fatalf("Allow: %v", allowErr)
		}
	}

	_, err = f.m.Invitations().Invite(f.ctx, InviteRequest{
		Email: address, NodeID: f.left.ID, InviterUserID: "u-inviter",
	})
	if !hasCode(err, ErrInvitationRateLimited.Code) {
		t.Fatalf("Invite error = %v, want org.invitation_rate_limited under the blind-index key", err)
	}
}

// TestInviteService_Invite_RateLimited_PerTenant exercises the other
// dimension by consuming the tenant budget through the limiter directly,
// which also pins the key's shape.
func TestInviteService_Invite_RateLimited_PerTenant(t *testing.T) {
	f := newInviteFixture(t)

	limiter := ratelimit.New(f.host.kv)
	limit := ratelimit.Limit{Rate: invitesPerTenantRate, Per: invitesPerTenantWindow}
	for range invitesPerTenantRate {
		if _, err := limiter.Allow(f.ctx, "org:invite:tenant:tenant-a", limit); err != nil {
			t.Fatalf("Allow: %v", err)
		}
	}

	_, err := f.m.Invitations().Invite(f.ctx, InviteRequest{
		Email: "ada@example.test", NodeID: f.left.ID, InviterUserID: "u-inviter",
	})
	if !hasCode(err, ErrInvitationRateLimited.Code) {
		t.Fatalf("Invite error = %v, want org.invitation_rate_limited", err)
	}
	if got := errParam(t, err, "dimension"); got != "tenant" {
		t.Errorf("denied dimension = %v, want tenant", got)
	}

	// Another tenant is unaffected: the key carries the tenant.
	other := tenantCtx("tenant-b")
	otherRoot, err := f.m.Tree().CreateRoot(other, "their group", "group")
	if err != nil {
		t.Fatalf("CreateRoot(tenant-b): %v", err)
	}
	if _, err := f.m.Invitations().Invite(other, InviteRequest{
		Email: "ada@example.test", NodeID: otherRoot.ID, InviterUserID: "u-other",
	}); err != nil {
		t.Errorf("another tenant was denied too: %v", err)
	}
}

// TestInviteService_Invite_NoRateLimitBudgetIsSpentWhenTheFeatureIsOff pins
// the order of operations: the gate runs before the limiter, so a tenant with
// invitations off cannot burn another tenant's... its own budget on calls
// that were never going to send anything.
func TestInviteService_Invite_NoRateLimitBudgetIsSpentWhenTheFeatureIsOff(t *testing.T) {
	f := newInviteFixture(t)
	f.m.invites.gate = fixedGate{enabled: false}

	for range invitesPerEmailRate + 1 {
		if _, err := f.m.Invitations().Invite(f.ctx, InviteRequest{
			Email: "ada@example.test", NodeID: f.left.ID, InviterUserID: "u-inviter",
		}); !hasCode(err, ErrInvitationsDisabled.Code) {
			t.Fatalf("Invite error = %v, want org.invitations_disabled", err)
		}
	}

	f.m.invites.gate = nil
	if _, err := f.m.Invitations().Invite(f.ctx, InviteRequest{
		Email: "ada@example.test", NodeID: f.left.ID, InviterUserID: "u-inviter",
	}); err != nil {
		t.Errorf("Invite after re-enabling = %v, want success -- the refused calls spent budget", err)
	}
}

func TestInviteService_Invite_GateFailure_IsNotSilentlyEnabled(t *testing.T) {
	f := newInviteFixture(t)
	f.m.invites.gate = fixedGate{enabled: true, err: errors.New("config store unreachable")}

	_, err := f.m.Invitations().Invite(f.ctx, InviteRequest{
		Email: "ada@example.test", NodeID: f.left.ID, InviterUserID: "u-inviter",
	})
	if !hasCode(err, ErrInternal.Code) {
		t.Errorf("Invite with a failing gate error = %v, want org.internal_error", err)
	}
	if len(f.host.mailer.messages()) != 0 {
		t.Error("a message was sent despite the gate failing")
	}
}

func TestInviteService_Invite_RejectedInputs(t *testing.T) {
	f := newInviteFixture(t)
	foreign, err := f.m.Tree().CreateRoot(tenantCtx("tenant-b"), "their group", "group")
	if err != nil {
		t.Fatalf("CreateRoot(tenant-b): %v", err)
	}

	tests := []struct {
		name string
		req  InviteRequest
		code string
	}{
		{"an unusable address", InviteRequest{Email: "not-an-address", NodeID: f.left.ID}, ErrInvalidEmail.Code},
		{"a blank address", InviteRequest{Email: "  ", NodeID: f.left.ID}, ErrInvalidEmail.Code},
		{"a non-ASCII address", InviteRequest{Email: "müller@example.test", NodeID: f.left.ID}, ErrInvalidEmail.Code},
		{"an unknown node", InviteRequest{Email: "ada@example.test", NodeID: "00000000-0000-4000-8000-000000000000"}, ErrNodeNotFound.Code},
		{"another tenant's node", InviteRequest{Email: "ada@example.test", NodeID: foreign.ID}, ErrNodeNotFound.Code},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.m.Invitations().Invite(f.ctx, tc.req); !hasCode(err, tc.code) {
				t.Errorf("Invite error = %v, want %s", err, tc.code)
			}
		})
	}
	if len(f.host.mailer.messages()) != 0 {
		t.Error("a rejected invitation sent a message anyway")
	}
}

// TestInviteService_Invite_SupersedesTheEarlierPendingInvitation pins that
// one address holds at most one live token: issuing a new invitation revokes
// the old one, so an older link stops working immediately.
func TestInviteService_Invite_SupersedesTheEarlierPendingInvitation(t *testing.T) {
	f := newInviteFixture(t)
	first := f.invite(t, "ada@example.test")
	second := f.invite(t, "ada@example.test")

	stored, err := f.m.Invitations().Repository().FindByID(f.ctx, first.Invitation.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if stored.Status != InvitationStatusRevoked {
		t.Errorf("the superseded invitation is %q, want %q", stored.Status, InvitationStatusRevoked)
	}
	if _, err := f.m.Invitations().Accept(f.ctx, first.Token, "u-new"); !hasCode(err, ErrInvitationRevoked.Code) {
		t.Errorf("accepting the superseded token error = %v, want org.invitation_revoked", err)
	}
	if _, err := f.m.Invitations().Accept(f.ctx, second.Token, "u-new"); err != nil {
		t.Errorf("accepting the current token: %v", err)
	}
}

// TestInviteService_Invite_DeliveryFailure_RevokesTheInvitation pins the
// compensation: the invitee never received the link, so leaving a pending row
// behind would only mean a token nobody holds sitting acceptable for a week.
func TestInviteService_Invite_DeliveryFailure_RevokesTheInvitation(t *testing.T) {
	f := newInviteFixture(t)
	f.host.mailer.failWith = errors.New("smtp is down")

	_, err := f.m.Invitations().Invite(f.ctx, InviteRequest{
		Email: "ada@example.test", NodeID: f.left.ID, InviterUserID: "u-inviter",
	})
	if err == nil {
		t.Fatal("Invite reported success despite the delivery failing")
	}
	pending, listErr := f.m.Invitations().List(f.ctx)
	if listErr != nil {
		t.Fatalf("List: %v", listErr)
	}
	if len(pending) != 0 {
		t.Errorf("%d invitations are still pending after a failed delivery, want 0", len(pending))
	}
}

// TestInviteService_Invite_EmailDisabled_StillCreatesAndAnnounces is the
// arrangement the M2 notification module takes over: no message from org, but
// the invitation exists and org.member.invited was published.
func TestInviteService_Invite_EmailDisabled_StillCreatesAndAnnounces(t *testing.T) {
	f := newInviteFixture(t)
	f.m.invites.emailEnabled = false

	result, err := f.m.Invitations().Invite(f.ctx, InviteRequest{
		Email: "ada@example.test", NodeID: f.left.ID, InviterUserID: "u-inviter",
	})
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	if len(f.host.mailer.messages()) != 0 {
		t.Error("org sent a message with the invitation email disabled")
	}
	if len(f.host.bus.events(EventMemberInvited)) != 1 {
		t.Error("the invitation was not announced")
	}
	if _, err := f.m.Invitations().Accept(f.ctx, result.Token, "u-new"); err != nil {
		t.Errorf("the invitation is not acceptable: %v", err)
	}
}

func TestInviteService_Accept_CreatesTheMembership(t *testing.T) {
	f := newInviteFixture(t)
	result := f.invite(t, "ada@example.test")

	membership, err := f.m.Invitations().Accept(f.ctx, result.Token, "u-ada")
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if membership.NodeID != f.left.ID || !membership.IsActive() {
		t.Errorf("membership = %+v, want an active one at %q", membership, f.left.ID)
	}

	stored, err := f.m.Invitations().Repository().FindByID(f.ctx, result.Invitation.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if stored.Status != InvitationStatusAccepted {
		t.Errorf("invitation status = %q, want %q", stored.Status, InvitationStatusAccepted)
	}
	if stored.AcceptedAt == nil {
		t.Error("AcceptedAt was not recorded")
	}

	joined := f.host.bus.events(EventMemberJoined)
	if len(joined) != 1 {
		t.Fatalf("published %d member-joined events, want 1", len(joined))
	}
	payload, ok := joined[0].Payload.(MemberJoined)
	if !ok {
		t.Fatalf("payload is %T, want org.MemberJoined", joined[0].Payload)
	}
	if payload.UserID != "u-ada" || payload.InvitationID != result.Invitation.ID {
		t.Errorf("payload = %+v, want the accepted invitation", payload)
	}
}

// TestInviteService_Accept_NoTenantInContext_ResolvesTenantFromToken is the
// service-level half of the tenantless-accept flow: a caller with NO tenant
// context at all -- the freshly invited person, who has no membership in
// and typically no bearer token for the inviting tenant -- presents the
// token, and Accept resolves the invitation's own tenant from it through
// the narrow org_invitation_token_index row createPending wrote alongside
// the invitation, then creates the membership inside that tenant exactly as
// the tenant-scoped path always has. Before this round such a caller could
// not accept at all: every tenant-scoped read underneath failed closed on
// the missing tenant (and a host's middleware refused the request before
// org's handler was ever reached).
func TestInviteService_Accept_NoTenantInContext_ResolvesTenantFromToken(t *testing.T) {
	f := newInviteFixture(t)
	result := f.invite(t, "ada@example.test")

	tenantless := context.Background()
	membership, err := f.m.Invitations().Accept(tenantless, result.Token, "u-ada")
	if err != nil {
		t.Fatalf("Accept with no tenant context: %v", err)
	}
	if membership.NodeID != f.left.ID || !membership.IsActive() {
		t.Errorf("membership = %+v, want an active one at %q", membership, f.left.ID)
	}
	// The membership landed in the invitation's OWN tenant, not in whatever
	// (nothing, here) the caller's context named.
	if membership.TenantID != "tenant-a" {
		t.Errorf("membership.TenantID = %q, want tenant-a", membership.TenantID)
	}

	// The invitation row flipped to accepted under that same tenant, and the
	// joined event announced the tenant it actually happened in.
	stored, err := f.m.Invitations().Repository().FindByID(tenantCtx("tenant-a"), result.Invitation.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if stored.Status != InvitationStatusAccepted || stored.AcceptedAt == nil {
		t.Errorf("stored invitation = status %q, acceptedAt %v; want accepted with a timestamp",
			stored.Status, stored.AcceptedAt)
	}
	joined := f.host.bus.events(EventMemberJoined)
	if len(joined) != 1 {
		t.Fatalf("published %d member-joined events, want 1", len(joined))
	}
	if joined[0].TenantID != "tenant-a" {
		t.Errorf("joined event TenantID = %q, want tenant-a -- the re-entered tenant must reach the event",
			joined[0].TenantID)
	}
}

// TestInviteService_Accept_NoTenantInContext_ReplayStillReportsAlreadyAccepted
// pins the single-use answer on the tenantless path: a token accepted once
// through the tenantless entry point is terminal for a second tenantless
// presentation too -- the status checks and the compare-and-swap run inside
// the resolved tenant exactly as they do for a tenant-scoped caller, so the
// replay is reported, not silently accepted, and creates no second
// membership.
func TestInviteService_Accept_NoTenantInContext_ReplayStillReportsAlreadyAccepted(t *testing.T) {
	f := newInviteFixture(t)
	result := f.invite(t, "ada@example.test")

	tenantless := context.Background()
	if _, err := f.m.Invitations().Accept(tenantless, result.Token, "u-ada"); err != nil {
		t.Fatalf("first Accept: %v", err)
	}
	if _, err := f.m.Invitations().Accept(tenantless, result.Token, "u-ada"); !hasCode(err, ErrInvitationAlreadyAccepted.Code) {
		t.Errorf("second tenantless Accept error = %v, want org.invitation_already_accepted", err)
	}
}

// TestInviteService_Accept_NoTenantInContext_TerminalStatesStillReported pins
// the revoked and expired answers on the tenantless path: the index row is
// deliberately never updated (see invitationTokenIndex's own doc comment),
// so a revoked or expired token still resolves its tenant and reaches the
// ordinary status checks -- which answer org.invitation_revoked /
// org.invitation_expired, never a misleading "no such invitation".
func TestInviteService_Accept_NoTenantInContext_TerminalStatesStillReported(t *testing.T) {
	t.Run("revoked", func(t *testing.T) {
		f := newInviteFixture(t)
		result := f.invite(t, "ada@example.test")
		if err := f.m.Invitations().Revoke(f.ctx, result.Invitation.ID); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		if _, err := f.m.Invitations().Accept(context.Background(), result.Token, "u-ada"); !hasCode(err, ErrInvitationRevoked.Code) {
			t.Errorf("tenantless Accept(revoked) error = %v, want org.invitation_revoked", err)
		}
	})
	t.Run("expired", func(t *testing.T) {
		f := newInviteFixture(t)
		result := f.invite(t, "ada@example.test")
		f.m.invites.now = func() time.Time { return result.Invitation.ExpiresAt.Add(time.Second) }
		if _, err := f.m.Invitations().Accept(context.Background(), result.Token, "u-ada"); !hasCode(err, ErrInvitationExpired.Code) {
			t.Errorf("tenantless Accept(expired) error = %v, want org.invitation_expired", err)
		}
	})
}

// TestInviteService_Accept_NoTenantInContext_UnknownToken_IsNotFound pins the
// unrecognized-token answer on the tenantless path: a hash the index has
// never seen reports org.invitation_not_found, the same sentinel a
// tenant-scoped caller sees for an unrecognized token under its own tenant.
func TestInviteService_Accept_NoTenantInContext_UnknownToken_IsNotFound(t *testing.T) {
	f := newInviteFixture(t)
	f.invite(t, "ada@example.test")

	if _, err := f.m.Invitations().Accept(context.Background(), "a-token-nobody-issued", "u-ada"); !hasCode(err, ErrInvitationNotFound.Code) {
		t.Errorf("tenantless Accept(unknown token) error = %v, want org.invitation_not_found", err)
	}
}

// TestInviteService_Accept_NoTenantInContext_EmptyUserIsRefused pins the
// fail-closed order on the tenantless path: an unidentified acceptor is
// refused before the token is even resolved, exactly as on the tenant-scoped
// path.
func TestInviteService_Accept_NoTenantInContext_EmptyUserIsRefused(t *testing.T) {
	f := newInviteFixture(t)
	result := f.invite(t, "ada@example.test")

	if _, err := f.m.Invitations().Accept(context.Background(), result.Token, ""); !hasCode(err, ErrMembershipNotFound.Code) {
		t.Errorf("tenantless Accept with no user error = %v, want org.membership_not_found", err)
	}
}

// TestInviteService_Accept_CrossTenantToken_ReturnsInvitationNotFound is the
// assertion behind "never accept a caller-supplied tenant id": the token is
// resolved strictly inside the tenant the context already carries, so a token
// minted elsewhere reveals nothing at all.
func TestInviteService_Accept_CrossTenantToken_ReturnsInvitationNotFound(t *testing.T) {
	f := newInviteFixture(t)
	result := f.invite(t, "ada@example.test")

	other := tenantCtx("tenant-b")
	if _, err := f.m.Tree().CreateRoot(other, "their group", "group"); err != nil {
		t.Fatalf("CreateRoot(tenant-b): %v", err)
	}
	if _, err := f.m.Invitations().Accept(other, result.Token, "u-ada"); !hasCode(err, ErrInvitationNotFound.Code) {
		t.Fatalf("Accept from another tenant error = %v, want org.invitation_not_found", err)
	}
	// And nothing leaked into that tenant.
	if _, err := f.m.Members().Get(other, "u-ada"); !hasCode(err, ErrMembershipNotFound.Code) {
		t.Errorf("a membership appeared in the wrong tenant: %v", err)
	}
}

func TestInviteService_Accept_Twice_ReturnsAlreadyAccepted(t *testing.T) {
	f := newInviteFixture(t)
	result := f.invite(t, "ada@example.test")

	if _, err := f.m.Invitations().Accept(f.ctx, result.Token, "u-ada"); err != nil {
		t.Fatalf("first Accept: %v", err)
	}
	if _, err := f.m.Invitations().Accept(f.ctx, result.Token, "u-ada"); !hasCode(err, ErrInvitationAlreadyAccepted.Code) {
		t.Errorf("second Accept error = %v, want org.invitation_already_accepted", err)
	}
	// The replay created no second membership.
	memberships, err := f.m.Members().Repository().List(f.ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(memberships) != 2 { // the inviter plus the invitee
		t.Errorf("the tenant holds %d memberships, want 2", len(memberships))
	}
}

func TestInviteService_Accept_Expired(t *testing.T) {
	f := newInviteFixture(t)
	result := f.invite(t, "ada@example.test")

	// Move the service clock past the expiry rather than sleeping.
	f.m.invites.now = func() time.Time { return result.Invitation.ExpiresAt.Add(time.Second) }
	if _, err := f.m.Invitations().Accept(f.ctx, result.Token, "u-ada"); !hasCode(err, ErrInvitationExpired.Code) {
		t.Errorf("Accept(expired) error = %v, want org.invitation_expired", err)
	}
}

func TestInviteService_Accept_Revoked(t *testing.T) {
	f := newInviteFixture(t)
	result := f.invite(t, "ada@example.test")

	if err := f.m.Invitations().Revoke(f.ctx, result.Invitation.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := f.m.Invitations().Accept(f.ctx, result.Token, "u-ada"); !hasCode(err, ErrInvitationRevoked.Code) {
		t.Errorf("Accept(revoked) error = %v, want org.invitation_revoked", err)
	}
}

func TestInviteService_Accept_UnknownToken(t *testing.T) {
	f := newInviteFixture(t)
	f.invite(t, "ada@example.test")

	if _, err := f.m.Invitations().Accept(f.ctx, "a-token-nobody-issued", "u-ada"); !hasCode(err, ErrInvitationNotFound.Code) {
		t.Errorf("Accept(unknown token) error = %v, want org.invitation_not_found", err)
	}
}

func TestInviteService_Accept_EmptyUser(t *testing.T) {
	f := newInviteFixture(t)
	result := f.invite(t, "ada@example.test")

	if _, err := f.m.Invitations().Accept(f.ctx, result.Token, ""); !hasCode(err, ErrMembershipNotFound.Code) {
		t.Errorf("Accept with no user error = %v, want org.membership_not_found", err)
	}
}

// TestInviteService_Accept_ExistingMember_KeepsTheirSeat pins that acceptance
// never moves somebody who is already placed in the tree.
func TestInviteService_Accept_ExistingMember_KeepsTheirSeat(t *testing.T) {
	f := newInviteFixture(t)
	result := f.invite(t, "ada@example.test")

	if _, err := f.m.Invitations().Accept(f.ctx, result.Token, "u-inviter"); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	membership, err := f.m.Members().Get(f.ctx, "u-inviter")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if membership.NodeID != f.root.ID {
		t.Errorf("the existing member moved to %q, want to stay at the root %q", membership.NodeID, f.root.ID)
	}
	// Nothing changed, so nothing was announced.
	if joined := f.host.bus.events(EventMemberJoined); len(joined) != 0 {
		t.Errorf("published %d member-joined events, want 0", len(joined))
	}
}

// TestInviteService_Accept_ConcurrentAcceptsBySeparateUsers_ExactlyOneWins
// reproduces the TOCTOU race an invitation token is exposed to: the token
// can be forwarded or shared, or a client can fire the request twice, so two
// DIFFERENT authenticated users can present the same still-pending token to
// Accept at the same time. The invitation's own doc comment, and Accept's,
// both describe the token as a single-use bearer credential -- "whoever
// holds it joins the tenant" -- so at most one of the two racing callers may
// ever come away with a membership.
//
// Before the compare-and-swap guard in InvitationRepository.acceptIfPending,
// Accept read the invitation's status, then only much later (after calling
// MemberService.ensure, itself several DB round trips) wrote
// Status = Accepted with a plain, unconditional Update. Both goroutines
// below can complete their read while the row is still Pending, so both
// proceed to create their own membership and both writes of
// Status = Accepted then "succeed" -- one becomes an unnoticed second
// membership out of a supposedly single-use invitation. This test runs the
// race many times, under -race, to make that window practically certain to
// be hit at least once if it exists.
func TestInviteService_Accept_ConcurrentAcceptsBySeparateUsers_ExactlyOneWins(t *testing.T) {
	f := newInviteFixture(t)

	const trials = 25
	for trial := 0; trial < trials; trial++ {
		result := f.invite(t, fmt.Sprintf("race-%d@example.test", trial))
		userA := fmt.Sprintf("u-race-%d-a", trial)
		userB := fmt.Sprintf("u-race-%d-b", trial)

		var (
			wg               sync.WaitGroup
			start            = make(chan struct{})
			mu               sync.Mutex
			successes        int
			membershipErrors int
			otherErrors      []error
		)
		wg.Add(2)
		for _, userID := range []string{userA, userB} {
			userID := userID
			go func() {
				defer wg.Done()
				<-start
				_, err := f.m.Invitations().Accept(f.ctx, result.Token, userID)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					successes++
				case hasCode(err, ErrInvitationAlreadyAccepted.Code):
					membershipErrors++
				default:
					otherErrors = append(otherErrors, err)
				}
			}()
		}
		close(start)
		wg.Wait()

		if len(otherErrors) != 0 {
			t.Fatalf("trial %d: unexpected error(s) racing Accept: %v", trial, otherErrors)
		}
		if successes != 1 || membershipErrors != 1 {
			t.Fatalf("trial %d: got %d successes and %d already-accepted errors racing Accept on one token, want exactly 1 and 1 (single-use violated)",
				trial, successes, membershipErrors)
		}

		// Confirm only ONE of the two users actually got a seat: the
		// invariant that matters is not just the return values above but
		// that a second membership was never persisted.
		haveA, haveB := f.userHasMembership(t, userA), f.userHasMembership(t, userB)
		if haveA == haveB {
			t.Fatalf("trial %d: membership(A)=%v membership(B)=%v, want exactly one of the two racing users seated", trial, haveA, haveB)
		}
	}
}

// userHasMembership reports whether userID holds a membership in the
// fixture's tenant.
func (f inviteFixture) userHasMembership(t *testing.T, userID string) bool {
	t.Helper()
	_, err := f.m.Members().Get(f.ctx, userID)
	switch {
	case err == nil:
		return true
	case hasCode(err, ErrMembershipNotFound.Code):
		return false
	default:
		t.Fatalf("Members().Get(%s): %v", userID, err)
		return false
	}
}

func TestInviteService_Revoke(t *testing.T) {
	f := newInviteFixture(t)
	result := f.invite(t, "ada@example.test")

	if err := f.m.Invitations().Revoke(f.ctx, result.Invitation.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	// Revoking twice is a no-op, not an error: the caller's intent is
	// already satisfied.
	if err := f.m.Invitations().Revoke(f.ctx, result.Invitation.ID); err != nil {
		t.Errorf("second Revoke: %v", err)
	}
	if err := f.m.Invitations().Revoke(f.ctx, "30000000-0000-4000-8000-000000000099"); !hasCode(err, ErrInvitationNotFound.Code) {
		t.Errorf("Revoke(unknown) error = %v, want org.invitation_not_found", err)
	}
}

// TestInviteService_Revoke_Accepted_IsRefused pins the boundary between the
// two operations: withdrawing an invitation is not a way to remove a member.
func TestInviteService_Revoke_Accepted_IsRefused(t *testing.T) {
	f := newInviteFixture(t)
	result := f.invite(t, "ada@example.test")
	if _, err := f.m.Invitations().Accept(f.ctx, result.Token, "u-ada"); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	if err := f.m.Invitations().Revoke(f.ctx, result.Invitation.ID); !hasCode(err, ErrInvitationAlreadyAccepted.Code) {
		t.Errorf("Revoke(accepted) error = %v, want org.invitation_already_accepted", err)
	}
	if _, err := f.m.Members().Get(f.ctx, "u-ada"); err != nil {
		t.Errorf("the refused revoke removed the membership: %v", err)
	}
}

func TestInviteService_List(t *testing.T) {
	f := newInviteFixture(t)
	first := f.invite(t, "ada@example.test")
	f.invite(t, "grace@example.test")

	pending, err := f.m.Invitations().List(f.ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("List returned %d invitations, want 2", len(pending))
	}

	if revokeErr := f.m.Invitations().Revoke(f.ctx, first.Invitation.ID); revokeErr != nil {
		t.Fatalf("Revoke: %v", revokeErr)
	}
	pending, err = f.m.Invitations().List(f.ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(pending) != 1 {
		t.Errorf("List returned %d invitations after a revoke, want 1", len(pending))
	}

	// Another tenant sees none of them.
	other, err := f.m.Invitations().List(tenantCtx("tenant-b"))
	if err != nil {
		t.Fatalf("List(tenant-b): %v", err)
	}
	if len(other) != 0 {
		t.Errorf("another tenant sees %d invitations, want 0", len(other))
	}
}

func TestInviteService_NoTenantContext_FailsClosed(t *testing.T) {
	f := newInviteFixture(t)
	ctx := context.Background()

	operations := map[string]func() error{
		"Invite": func() error {
			_, err := f.m.Invitations().Invite(ctx, InviteRequest{Email: "ada@example.test", NodeID: f.left.ID})
			return err
		},
		"Accept": func() error { _, err := f.m.Invitations().Accept(ctx, "token", "u-1"); return err },
		"Revoke": func() error { return f.m.Invitations().Revoke(ctx, "inv-1") },
		"List":   func() error { _, err := f.m.Invitations().List(ctx); return err },
	}
	for name, op := range operations {
		t.Run(name, func(t *testing.T) {
			if err := op(); err == nil {
				t.Errorf("%s without a tenant in context succeeded; it must fail closed", name)
			}
		})
	}
}

// TestInviteService_NoIndexer_RefusesToInvite pins the runtime half of the
// boot-time ErrEmailIndexerRequired check, for a service built directly
// rather than through a bootstrapped module.
func TestInviteService_NoIndexer_RefusesToInvite(t *testing.T) {
	m, host := newTestModule(t)
	m.invites.indexer = nil
	ctx := tenantCtx("tenant-a")
	root, _, _ := seedTree(t, m.Tree(), ctx)

	_, err := m.Invitations().Invite(ctx, InviteRequest{Email: "ada@example.test", NodeID: root.ID})
	if !hasCode(err, ErrEmailIndexerRequired.Code) {
		t.Errorf("Invite without an indexer error = %v, want org.email_indexer_required", err)
	}
	if len(host.mailer.messages()) != 0 {
		t.Error("a message was sent without an indexer")
	}
}

// TestInviteService_NoKVStore_DeniesRatherThanAllows pins the fail-closed
// reading of an unavailable rate limiter: a limit that cannot be evaluated
// must deny, never allow.
func TestInviteService_NoKVStore_DeniesRatherThanAllows(t *testing.T) {
	f := newInviteFixture(t)
	f.host.kv = nil

	_, err := f.m.Invitations().Invite(f.ctx, InviteRequest{
		Email: "ada@example.test", NodeID: f.left.ID, InviterUserID: "u-inviter",
	})
	if err == nil {
		t.Fatal("Invite succeeded with no store to count in; the limiter must fail closed")
	}
	if len(f.host.mailer.messages()) != 0 {
		t.Error("a message was sent without a working rate limiter")
	}
}

// livePendingCount returns how many currently-pending invitations exist for
// email in the fixture's tenant, read straight through the repository's own
// blind-index lookup -- the same query InviteService.Invite's atomic
// createPending step revokes against, so it is the authoritative answer to
// "how many live tokens does this address have right now".
func (f inviteFixture) livePendingCount(t *testing.T, email string) int {
	t.Helper()
	pending, err := f.m.Invitations().Repository().pendingByEmail(f.ctx, newTestEmailIndexer(t), email)
	if err != nil {
		t.Fatalf("pendingByEmail(%s): %v", email, err)
	}
	return len(pending)
}

// TestInviteService_Invite_ConcurrentSameAddress_ExactlyOneLiveToken is the
// D4 (org-rbac P3) regression test: two goroutines both call Invite for the
// SAME address at the same time, over the real repository against a real,
// file-backed SQLite database.
//
// # Two legitimate outcomes, and one that is never legitimate
//
// Two calls racing for the same address can genuinely resolve two different
// ways even on correct code, depending on how much they actually overlap:
//
//   - Little or no overlap (the first call's whole transaction, including
//     its own revoke-then-insert, commits before the second one reaches the
//     database at all): both calls succeed with no error, because the
//     second call's own revoke step correctly finds and revokes the
//     first's now-committed row before inserting its own. This is not a
//     bug -- it is what Invite is supposed to do when asked to invite an
//     address that already has a pending invitation, concurrently or not.
//   - Genuine overlap (both calls' revoke steps run before either has
//     committed its insert): each observes nothing pending to revoke, and
//     whichever call's INSERT reaches the database second is refused by
//     the partial unique index (migrations/{postgres,sqlite}/
//     0005_unique_pending_invitation.sql), surfaced as the coded
//     ErrInvitationAlreadyPending rather than a raw gorm.ErrDuplicatedKey.
//
// Both are correct; this test accepts either distribution of
// successes/coded-refusals (any error OTHER than the coded one is still a
// failure). The one outcome that must NEVER happen, on the fixed code, is
// what the D4 finding names: BOTH calls succeeding AND leaving two
// simultaneously live tokens. That is exactly what the pre-fix code's
// revokePendingFor (a read via pendingByEmail, then a per-row Update loop,
// itself a separate transaction from the row's own later, separate Create)
// allows, since neither of its two transactions ever overlapped with the
// other's -- there was no atomicity to race against in the first place, so
// "genuine overlap" above always looked exactly like the always-safe first
// bullet from the caller's point of view, yet still doubled the live token
// count. The one invariant this test actually enforces, every single trial
// regardless of which legitimate distribution occurred, is therefore the
// live-token COUNT rather than the win/loss split: exactly one live
// (Pending) row for the address once both goroutines have returned.
func TestInviteService_Invite_ConcurrentSameAddress_ExactlyOneLiveToken(t *testing.T) {
	const trials = 25
	for trial := 0; trial < trials; trial++ {
		f := newInviteFixture(t)
		email := fmt.Sprintf("race-invite-%d@example.test", trial)

		var (
			wg        sync.WaitGroup
			start     = make(chan struct{})
			mu        sync.Mutex
			successes int
			coded     int
			other     []error
		)
		wg.Add(2)
		for i := 0; i < 2; i++ {
			go func() {
				defer wg.Done()
				<-start
				_, err := f.m.Invitations().Invite(f.ctx, InviteRequest{
					Email:         email,
					NodeID:        f.left.ID,
					InviterUserID: "u-inviter",
					Locale:        "en-US",
				})
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					successes++
				case hasCode(err, ErrInvitationAlreadyPending.Code):
					coded++
				default:
					other = append(other, err)
				}
			}()
		}
		close(start)
		wg.Wait()

		if len(other) != 0 {
			t.Fatalf("trial %d: unexpected error(s) racing Invite: %v", trial, other)
		}
		if successes+coded != 2 {
			t.Fatalf("trial %d: got %d successes and %d org.invitation_already_pending errors, want exactly 2 total",
				trial, successes, coded)
		}
		if live := f.livePendingCount(t, email); live != 1 {
			t.Fatalf("trial %d: %d live (pending) tokens exist for %s after the race (successes=%d coded=%d), want exactly 1",
				trial, live, email, successes, coded)
		}
	}
}

// errParam reads one structured parameter off an apperr-carrying error.
func errParam(t *testing.T, err error, key string) any {
	t.Helper()
	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("error %v carries no parameters", err)
	}
	return appErr.Params[key]
}

// TestInviteService_Revoke_RacingAccept_NoRevokedStatusWithLiveMembership is
// the P2-6 regression proof: Revoke used to read the invitation's status and
// then write Status = Revoked with a plain, unconditional Update. Racing an
// Accept of the same invitation, that unconditional write could land AFTER
// Accept's compare-and-swap claim (pending -> accepted) and its membership
// creation had committed -- overwriting the accepted status with revoked and
// leaving the invitation terminal-revoked while the membership Accept
// created stays live: the row and the roster disagree about whether the
// invitee joined. (Accept itself has always been single-use through
// acceptIfPending's CAS; Revoke was the unguarded writer on the same row.)
//
// # Deterministic, exactly like the P1-2/P1-3 delete tests
//
// A second connection holds an open transaction that has already performed
// Accept's two writes -- acceptIfPending's compare-and-swap UPDATE on the
// invitation and ensure's membership INSERT (its node touch omitted: on
// SQLite any writer holds the whole file, so the extra row lock is not
// load-bearing for the interleaving this file-lock rig pins). Revoke's read
// sees the still-pending committed state; its write parks behind the
// holder; release lets the accept commit first, and Revoke's resumed write
// executes against the now-accepted row -- the pre-fix unconditional Update
// stamps revoked over it (assertions fail), while the fixed compare-and-swap
// (revokeIfPending, gated on status = 'pending') matches nothing, re-reads,
// and answers the same org.invitation_already_accepted a caller who had
// observed the accepted state directly would have gotten.
func TestInviteService_Revoke_RacingAccept_NoRevokedStatusWithLiveMembership(t *testing.T) {
	ctx := tenantCtx("tenant-a")

	dsn := filepath.Join(t.TempDir(), "revoke-accept-race.sqlite")
	db1, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open: %v", err)
	}
	db2, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open (acceptor connection): %v", err)
	}
	t.Cleanup(func() {
		for _, db := range []*gorm.DB{db1, db2} {
			if sqlDB, dbErr := db.DB(); dbErr == nil {
				_ = sqlDB.Close()
			}
		}
	})
	testutil.Migrate(t, db1, dbkit.DialectSQLite, moduleName, migrations.FS)

	// The module wiring on db1, Invitation's encrypted serializer included
	// (newInvitationTestDB's registration is a no-op repeat, so registering
	// again here is harmless).
	host := newTestHost(t)
	m := NewModule(db1,
		WithEmailIndexer(newTestEmailIndexer(t)),
		WithMailFrom(testMailFrom),
		WithInvitationLinkBuilder(testLinkBuilder),
	)
	m.attach(host)
	cipher, err := dbkit.NewCipher(testEncryptionKey)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	dbkit.RegisterEncryptedSerializer(EmailSerializerName, cipher)

	tree, invites, members := m.Tree(), m.Invitations(), m.Members()
	root := mustCreateRoot(t, tree, ctx, "root")
	node := mustCreateChild(t, tree, ctx, root.ID, "store")
	if _, addErr := members.Add(ctx, "u-inviter", root.ID); addErr != nil {
		t.Fatalf("Add(inviter): %v", addErr)
	}
	result, err := invites.Invite(ctx, InviteRequest{
		Email:         "race@example.test",
		NodeID:        node.ID,
		InviterUserID: "u-inviter",
		Locale:        "en-US",
	})
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}

	// The concurrent acceptor on db2: perform Accept's two writes and hold
	// the transaction open until release. The CAS's own completion closes
	// `held`, never timing luck.
	acceptedAt := time.Now()
	held := make(chan struct{})
	release := make(chan struct{})
	holderErr := make(chan error, 1)
	go func() {
		holderErr <- dbkit.WithTenantSession(ctx, db2, func(tx *gorm.DB) error {
			res := tx.
				Where("id = ?", result.Invitation.ID).
				Where("status = ?", InvitationStatusPending).
				Updates(&Invitation{Status: InvitationStatusAccepted, AcceptedAt: &acceptedAt})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return fmt.Errorf("accept CAS matched %d rows, want 1", res.RowsAffected)
			}
			membership := &Membership{
				ID:     "00000000-0000-4000-8000-00000000ace5",
				UserID: "u-accept-racer",
				NodeID: node.ID,
				Status: MembershipStatusActive,
			}
			if createErr := tx.Create(membership).Error; createErr != nil {
				return createErr
			}
			close(held)
			<-release
			return nil
		})
	}()

	select {
	case <-held:
	case err = <-holderErr:
		t.Fatalf("acceptor failed before holding the accept open: %v", err)
	}

	// Revoke races the held accept: its read sees the still-pending
	// committed state, and its write cannot complete until the acceptor
	// commits.
	revokeDone := make(chan struct{})
	var revokeErr error
	go func() {
		defer close(revokeDone)
		revokeErr = invites.Revoke(ctx, result.Invitation.ID)
	}()
	time.Sleep(200 * time.Millisecond) // Revoke's read has certainly landed by now
	close(release)
	<-revokeDone
	if err = <-holderErr; err != nil {
		t.Fatalf("acceptor commit: %v", err)
	}

	// The accept won the race, so the revoke must report that -- the same
	// coded error a caller who had observed the accepted state directly
	// would have gotten -- never a silent success.
	assertCode(t, revokeErr, ErrInvitationAlreadyAccepted.Code)

	// And the two sides must agree: the invitation reads back accepted, and
	// the membership the accept created is live.
	current, err := invites.repo.FindByID(ctx, result.Invitation.ID)
	if err != nil {
		t.Fatalf("re-read the invitation: %v", err)
	}
	if current.Status != InvitationStatusAccepted {
		t.Fatalf("invitation status = %q after a revoke raced the winning accept, want %q -- the unconditional revoke overwrote the accepted status while the membership stayed live",
			current.Status, InvitationStatusAccepted)
	}
	if _, err := members.Get(ctx, "u-accept-racer"); err != nil {
		t.Fatalf("the membership the accept created is not live: %v", err)
	}
}

// TestInviteService_Accept_NodeDeletedUnderTheInvitation_SettlesToRevoked is
// the P2-org-12 regression proof. Accept claims an invitation (pending ->
// accepted), and only then creates the membership; when that creation fails
// because the very node the invitation points into no longer exists
// (members.ensure answers ErrNodeNotFound), the invitation can NEVER be
// fulfilled. The pre-fix failure path (revertAcceptClaim) answered that
// permanent failure with an unconditional full-row Update putting the row
// back to pending: a bearer token stayed acceptable for the rest of its TTL
// while every accept it admitted failed identically, List kept showing an
// invitation that could only fail, and a concurrent caller who observed the
// brief accepted interlude was told org.invitation_already_accepted though
// no membership ever came of it. The honest end state is revoked: the claim
// is settled, the token is dead, and the row records the withdrawal.
func TestInviteService_Accept_NodeDeletedUnderTheInvitation_SettlesToRevoked(t *testing.T) {
	f := newInviteFixture(t)
	result := f.invite(t, "ada@example.test") // the invitation points into f.left

	// The sub-org the invitation points into is deleted before the invitee
	// ever clicks the link -- an admin removes it.
	if err := f.m.Tree().Delete(f.ctx, f.left.ID, false); err != nil {
		t.Fatalf("Delete(the invited node): %v", err)
	}

	if _, err := f.m.Invitations().Accept(f.ctx, result.Token, "u-ada"); !hasCode(err, ErrNodeNotFound.Code) {
		t.Fatalf("Accept into a deleted node error = %v, want org.node_not_found", err)
	}

	// The failed accept must NOT have given the invitation back to pending
	// with its token still acceptable: it ends revoked.
	current, err := f.m.Invitations().Repository().FindByID(f.ctx, result.Invitation.ID)
	if err != nil {
		t.Fatalf("re-read the invitation: %v", err)
	}
	if current.Status != InvitationStatusRevoked {
		t.Fatalf("invitation status = %q after an accept whose membership creation failed, want %q -- the failed accept must settle the claim to revoked, never back to pending with a live token",
			current.Status, InvitationStatusRevoked)
	}
	if current.AcceptedAt != nil {
		t.Errorf("AcceptedAt = %v on a revoked invitation, want nil", current.AcceptedAt)
	}

	// The token is unusable: a second accept of the same token answers
	// revoked -- never a false already-accepted, never another doomed
	// acceptance attempt.
	if _, replayErr := f.m.Invitations().Accept(f.ctx, result.Token, "u-ada"); !hasCode(replayErr, ErrInvitationRevoked.Code) {
		t.Fatalf("second Accept error = %v, want org.invitation_revoked -- the token stayed live after the failed accept", replayErr)
	}

	// No membership ever came of the claim, and no pending zombie stays on
	// the tenant's list.
	if f.userHasMembership(t, "u-ada") {
		t.Error("the failed accept left a membership behind")
	}
	pending, err := f.m.Invitations().List(f.ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, inv := range pending {
		if inv.ID == result.Invitation.ID {
			t.Errorf("List still shows the unfulfillable invitation %q as pending", inv.ID)
		}
	}
}
