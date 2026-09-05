package notification

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/vislake/speed/go/notification/api"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/i18n"
)

// jsonContentType is the Content-Type every JSON response this handler
// writes carries. It matches what org's handler and the reference app's
// notes handler send, keeping the modules' HTTP surfaces uniform.
const jsonContentType = "application/json; charset=utf-8"

// Handler serves notification's HTTP surface: the inbox, the preference
// matrix, the tenant's contact roster and the inbox stream, exactly the
// eleven operations openapi.yaml's fragment declares plus the one route the
// fragment deliberately omits. It implements the api.ServerInterface the
// fragment's generated code declares (the compile-time assertion at the
// bottom of this file), mounted by Module.Register at apiPath through
// reg.Routes.Mount, and it is the module's own surface -- nothing here is
// shared with another module, unlike the services it reads, whose identity
// this comment keeps straight:
//
//   - The inbox Repository is the SAME instance Module's delivery pipeline
//     writes through (see module.go's inbox field): one data path for the
//     inbox, so a message this handler lists is the very row the pipeline
//     committed. The read methods it calls take the recipient as an
//     explicit argument because the row filter is the caller's identity,
//     resolved here through the subject seam -- never read from the
//     request, never assumed from the context.
//
//   - The PreferenceService and ContactService are the same instances
//     Module hands out through Preferences() and Contacts(). Their rows are
//     per-tenant data scoped by the ctx tenant (mustTenant below), so this
//     handler never reads a tenant from the request.
//
// # Own-data self-service
//
// Every endpoint this handler serves is own-data self-service, the "class
// of the authn /me surface" shape the fragment's header names: the inbox is
// the caller's own, the preference matrix is the caller's own, and the
// contact roster -- a tenant's whole -- is gated on an identified caller of
// that tenant because the module declares no permissions and mounts no
// admin surface; who may manage the roster is whoever the host's
// authorization layer lets reach the tenant at all, and identification is
// the layer below that, never a substitute for it. The handler therefore
// requires BOTH a tenant and an identified caller on every operation:
//
//   - mustTenant reads the tenant the tenancy middleware injected into the
//     request context. A request without one is refused -- the host's
//     middleware chain is misconfigured if a request reaches this handler
//     tenant-less, and no endpoint here invents a tenant.
//
//   - resolveSubject reads the caller's user id through the host's
//     SubjectResolver seam (declared at the bottom of this file). Every
//     operation calls it, and every operation refuses with
//     ErrSubjectUnresolved when it fails or is unwired: the module never
//     serves an anonymous caller, and never invents a default user. On the
//     per-recipient surfaces the resolved user id IS the row filter; on the
//     contact operations it is a gate only -- the roster belongs to the
//     ctx tenant as a whole, and the id's only further use is the audit
//     actor the services read from the context -- which is why those
//     operations' docs say so rather than pretending the caller's id
//     scopes rows it does not scope.
//
// A host that has not wired an identity layer yet can still boot the module
// (module.go's subject field), but none of its HTTP endpoints answer; the
// module's Go service faces -- Deliveries(), Contacts(), Preferences() --
// are where such a host exercises notification until the seam arrives.
//
// # Error envelope
//
// Every refusal travels as the fragment's NotificationError envelope: the
// {code, params} pair of errors.go's sentinels, never localized text and
// never an internal detail. The writeError helper below is the single place
// the envelope is built, so a host or a test can rely on its shape.
//
// # The stream route
//
// The fragment does not carry GET /api/v1/notifications/stream -- server-
// sent events are not an OpenAPI 3.0 media type (the fragment's header
// records the omission) -- so NewHandler mounts it on the inner mux by
// hand, as a method pattern of exactly the shape api.HandlerFromMux
// registers for the spec's own paths. It is own-data self-service like
// every other route: tenant and caller are resolved before the stream
// opens, and only that caller's announcements in that tenant's rows pass
// the filter.
// handlerHost is the registry slice the HTTP surface reads at call time:
// the merged message catalog the type directory renders its descriptions
// from. It is declared as an interface -- delivery.go's deliveryHost and
// contact.go's contactHost are the same pattern -- so the handler never
// names pkgcore.Registry for a single accessor, and a hand-built host in a
// test satisfies it too. *pkgcore.Registry satisfies it structurally (the
// assertion below pins that), and Module.Register hands the real registry
// over through attachHost.
type handlerHost interface {
	Locales() *i18n.Catalog
}

var _ handlerHost = (*pkgcore.Registry)(nil)

type Handler struct {
	// inbox is the inbox's repository, the same instance Module's delivery
	// pipeline writes through (module.go's doc comment on its inbox field).
	inbox *Repository

	// prefs is the preference matrix's decision layer, Module's own
	// instance (Preferences()). Its type taxonomy is live -- read at
	// request time -- so the directory and the preference list always
	// answer from the same declarations.
	prefs *PreferenceService

	// contacts is the consent ledger's decision layer, Module's own
	// instance (Contacts()). Contact operations use the resolved caller's
	// id as an identification gate only (see the file comment).
	contacts *ContactService

	// hub is this replica's inbox-announcement fan-out, the producer side
	// of the stream route. handleStream subscribes per request; Close on
	// disconnect removes the connection.
	hub *Hub

	// subject resolves the HTTP caller's identity for every operation (see
	// the file comment). Nil is a legal, if unusable, wiring: resolveSubject
	// fails every endpoint closed with ErrSubjectUnresolved rather than
	// Handler being constructed without it.
	subject SubjectResolver

	// host is the host registry's slice this handler reads at request time:
	// the merged message catalog, for the type directory's description
	// rendering. It is bound by Module.Register through attachHost after the
	// module graph has registered, and read per request -- never captured
	// earlier, because reg.Locales() is nil inside Register (module.go's
	// Register doc comment records the rule, delivery.go's attachHost doc
	// comment repeats it from its side). The field is the handlerHost
	// interface, not pkgcore.Registry itself, so the handler names exactly
	// the one accessor it reads (delivery.go's deliveryHost and contact.go's
	// contactHost are the same pattern); the compile-time assertion below
	// pins that the real registry satisfies it.
	host handlerHost

	// mux is the inner router NewHandler built: the generated
	// api.HandlerFromMux registration of the fragment's eleven paths plus
	// the hand-mounted stream route. ServeHTTP delegates to it.
	mux *http.ServeMux
}

// NewHandler returns a Handler over the module's own services. inbox,
// prefs, contacts and hub are Module's instances (a Module constructs them
// at NewModule time); subject is the host's seam, nil when the host has
// none. NewHandler performs no I/O and reads nothing from the registry.
func NewHandler(inbox *Repository, prefs *PreferenceService, contacts *ContactService, hub *Hub, subject SubjectResolver) *Handler {
	h := &Handler{
		inbox:    inbox,
		prefs:    prefs,
		contacts: contacts,
		hub:      hub,
		subject:  subject,
	}
	h.mux = http.NewServeMux()
	// The fragment's eleven paths, registered by the generated code as
	// absolute method patterns ("GET /api/v1/notifications/contacts", ...)
	// on h.mux, each dispatching to its ServerInterface method on h.
	api.HandlerFromMux(h, h.mux)
	// The stream route the fragment deliberately omits, hand-mounted as the
	// same method-pattern shape the generated registrations use.
	h.mux.HandleFunc(http.MethodGet+" "+apiPath+"/stream", h.handleStream)
	return h
}

// attachHost binds the host's registry to the handler. Module.Register
// calls it after every module has registered, so the catalog Handler reads
// at request time (h.catalog) is the merged one -- read from the registry
// at call time, never captured here, when reg.Locales() is still nil.
func (h *Handler) attachHost(reg *pkgcore.Registry) {
	h.host = reg
}

// ServeHTTP is what reg.Routes.Mount(apiPath, h.mux) mounts: every request
// under apiPath is handed to the inner mux, which routes by method and the
// absolute spec path.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// catalog returns the merged message catalog the host registry carries, or
// nil before attachHost ran. Renderers treat nil as "no catalog" and fail
// closed (render.go's failure mode).
func (h *Handler) catalog() *i18n.Catalog {
	if h.host == nil {
		return nil
	}
	return h.host.Locales()
}

// NotificationListContacts serves GET /api/v1/notifications/contacts: the
// tenant's verified and pending contacts, newest first, as the roster a
// host-side management surface or a delivery flow renders.
//
// The caller's user id gates this operation but does not scope its rows --
// the roster is the tenant's whole (see the file comment), and the id's
// only further use is as the audit actor the create/verify/resend flows
// record. The Address of a contact never appears in a response (the spec
// serves id, channel, status and timestamps only; toContactResponse strips
// it), because a plaintext address is PII and the blind index would be an
// offline brute-force surface.
func (h *Handler) NotificationListContacts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := h.mustTenant(w, r); !ok {
		return
	}
	if _, ok := h.resolveSubject(w, r); !ok {
		return
	}

	contacts, err := h.contacts.List(ctx)
	if err != nil {
		h.writeError(w, err)
		return
	}
	items := make([]api.NotificationContact, 0, len(contacts))
	for i := range contacts {
		items = append(items, toContactResponse(&contacts[i]))
	}
	h.writeJSON(w, http.StatusOK, api.NotificationListContactsResponse{Items: items})
}

// NotificationCreateContact serves POST /api/v1/notifications/contacts:
// registering one address through double opt-in. The contact is created
// pending and a verification code goes out over its channel (the module's
// synchronous verification-code exception); the row answers 201 with status
// "pending" until NotificationVerifyContact proves consent.
//
// The HTTP surface deliberately carries no consent-ref: the API never
// attests an address, only the double opt-in code proves consent, and the
// attestation path (ContactService.CreateContact's ConsentRef) belongs to a
// host-side business flow speaking to the service directly. The caller's
// user id gates the operation and is the audit actor of the created row's
// audit event.
func (h *Handler) NotificationCreateContact(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := h.mustTenant(w, r); !ok {
		return
	}
	if _, ok := h.resolveSubject(w, r); !ok {
		return
	}

	var body api.NotificationCreateContactRequest
	if !h.decodeJSON(w, r, &body) {
		return
	}
	contact, err := h.contacts.CreateContact(ctx, ContactCreateInput{
		Channel: string(body.Channel),
		Address: body.Address,
	})
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusCreated, toContactResponse(contact))
}

// NotificationVerifyContact serves POST
// /api/v1/notifications/contacts/{contactId}/verify: proving one pending
// contact's consent with the code its address received. The answers are the
// consent ledger's own -- a wrong, expired or replayed code is one refusal
// (ErrContactCodeInvalid), a terminal contact answers its own conflict, and
// rate limits fail closed (the contact-service errors travel through
// unchanged, each with the status errors.go assigns it).
func (h *Handler) NotificationVerifyContact(w http.ResponseWriter, r *http.Request, contactID string) {
	ctx := r.Context()
	if _, ok := h.mustTenant(w, r); !ok {
		return
	}
	if _, ok := h.resolveSubject(w, r); !ok {
		return
	}

	var body api.NotificationVerifyContactRequest
	if !h.decodeJSON(w, r, &body) {
		return
	}
	contact, err := h.contacts.VerifyCode(ctx, VerifyCodeInput{ContactID: contactID, Code: body.Code})
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, toContactResponse(contact))
}

// NotificationResendContactCode serves POST
// /api/v1/notifications/contacts/{contactId}/resend: issuing a fresh code
// to a still-pending contact. Only pending contacts can be resent to (the
// ledger refuses every other status with its own error, without burning
// rate-limit budget); 204 answers a code that went out.
func (h *Handler) NotificationResendContactCode(w http.ResponseWriter, r *http.Request, contactID string) {
	ctx := r.Context()
	if _, ok := h.mustTenant(w, r); !ok {
		return
	}
	if _, ok := h.resolveSubject(w, r); !ok {
		return
	}

	if err := h.contacts.ResendCode(ctx, ResendCodeInput{ContactID: contactID}); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listLimitDefault and listLimitMax bound every page this handler serves,
// mirroring openapi.yaml's list operation (its minimum of 1, its cap of
// 200, its default of 50) and repository.go's own doc-comment promise --
// "the HTTP surface resolves the spec's default and cap before this method
// is called". The two constants and the spec's numbers must never drift:
// a page size outside the bounds is refused (listPage), never truncated.
const (
	listLimitDefault = 50
	listLimitMax     = 200
)

// NotificationListMessages serves GET /api/v1/notifications/messages: one
// page of the caller's own inbox rows, newest first, stable across pages
// (created_at DESC with id DESC as the tiebreak). The page is bounded
// before the repository is reached: limit below the spec's minimum of 1 or
// above its cap of 200, or a negative offset, is refused whole with
// ErrInvalidListParams naming the offending parameter -- never silently
// truncated, per the spec's own words. group restricts the page to one
// notification group; absent or empty it lists every group.
func (h *Handler) NotificationListMessages(w http.ResponseWriter, r *http.Request, params api.NotificationListMessagesParams) {
	ctx := r.Context()
	if _, ok := h.mustTenant(w, r); !ok {
		return
	}
	userID, ok := h.resolveSubject(w, r)
	if !ok {
		return
	}

	group, limit, offset, ok := h.listPage(w, params)
	if !ok {
		return
	}
	msgs, err := h.inbox.ListForRecipient(ctx, userID, group, limit, offset)
	if err != nil {
		h.writeError(w, ErrInternal.WithCause(err))
		return
	}
	items := make([]api.NotificationMessage, 0, len(msgs))
	for i := range msgs {
		item, err := toMessageResponse(&msgs[i])
		if err != nil {
			h.writeError(w, ErrInternal.WithCause(err))
			return
		}
		items = append(items, item)
	}
	h.writeJSON(w, http.StatusOK, api.NotificationListMessagesResponse{Items: items})
}

// listPage resolves a list request's paging parameters: the spec's default
// of 50 and offset of 0 fill the absent ones, and a limit outside 1..200 or
// a negative offset is refused with ErrInvalidListParams before any
// repository call -- the whole page request, never a silently clamped page.
func (h *Handler) listPage(w http.ResponseWriter, params api.NotificationListMessagesParams) (group string, limit, offset int, ok bool) {
	limit = listLimitDefault
	if params.Limit != nil {
		limit = *params.Limit
	}
	if params.Offset != nil {
		offset = *params.Offset
	}
	if limit < 1 || limit > listLimitMax {
		h.writeError(w, ErrInvalidListParams.
			WithParam("field", "limit").
			WithParam("value", strconv.Itoa(limit)))
		return "", 0, 0, false
	}
	if offset < 0 {
		h.writeError(w, ErrInvalidListParams.
			WithParam("field", "offset").
			WithParam("value", strconv.Itoa(offset)))
		return "", 0, 0, false
	}
	if params.Group != nil {
		group = *params.Group
	}
	return group, limit, offset, true
}

// NotificationGetUnreadCount serves
// GET /api/v1/notifications/messages/unread-count: how many of the caller's
// own inbox rows are unread, counting only messages whose expiry has not
// passed (expiry governs the unread predicate, never list membership).
func (h *Handler) NotificationGetUnreadCount(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := h.mustTenant(w, r); !ok {
		return
	}
	userID, ok := h.resolveSubject(w, r)
	if !ok {
		return
	}

	count, err := h.inbox.UnreadCount(ctx, userID)
	if err != nil {
		h.writeError(w, ErrInternal.WithCause(err))
		return
	}
	h.writeJSON(w, http.StatusOK, api.NotificationUnreadCountResponse{Count: count})
}

// NotificationMarkAllMessagesRead serves
// POST /api/v1/notifications/messages/read-all: marking every unread
// message of the caller as read, answering how many this call marked.
func (h *Handler) NotificationMarkAllMessagesRead(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := h.mustTenant(w, r); !ok {
		return
	}
	userID, ok := h.resolveSubject(w, r)
	if !ok {
		return
	}

	read, err := h.inbox.ReadAll(ctx, userID)
	if err != nil {
		h.writeError(w, ErrInternal.WithCause(err))
		return
	}
	h.writeJSON(w, http.StatusOK, api.NotificationReadAllResponse{ReadCount: read})
}

// NotificationMarkMessageRead serves
// POST /api/v1/notifications/messages/{messageId}/read: marking one of the
// caller's own messages read. A message id no row of the caller's own inbox
// holds -- another recipient's message, another tenant's row, or nothing at
// all -- answers one 404 (ErrMessageNotFound), never an existence-
// disclosing distinction; a second read of an already-read message answers
// 204 exactly like a first read (the spec's idempotence promise).
func (h *Handler) NotificationMarkMessageRead(w http.ResponseWriter, r *http.Request, messageID string) {
	ctx := r.Context()
	if _, ok := h.mustTenant(w, r); !ok {
		return
	}
	userID, ok := h.resolveSubject(w, r)
	if !ok {
		return
	}

	if err := h.inbox.MarkRead(ctx, userID, messageID); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// NotificationListTypes serves GET /api/v1/notifications/types: the type
// directory -- every notification type this deployment's modules declared,
// in declaration order, each carrying its copy (Description) rendered from
// the declaring module's own locale bundle in the language the request
// negotiates. The directory is how a client renders a message's richer
// copy: each inbox row carries its type key and the params that produced
// its title and body, and this operation supplies the description those
// params interpolate into.
//
// The description convention lives in render.go next to the send-time
// template convention: for a type key <module>.<entity>.<action>, the
// declaring module ships "<type_key>.description" in both languages of its
// own bundle, and renderTypeDescription resolves it strictly -- a type
// whose declaring module shipped no description fails the whole listing
// with a coded error naming the type, never a half-rendered directory (the
// same no-fallback rule renderContent documents for send-time copy).
func (h *Handler) NotificationListTypes(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.mustTenant(w, r); !ok {
		return
	}
	if _, ok := h.resolveSubject(w, r); !ok {
		return
	}

	types := h.prefs.NotificationTypes()
	locale := negotiateLocale(h.catalog(), r.Header.Get("Accept-Language"))
	items := make([]api.NotificationType, 0, len(types))
	for _, typ := range types {
		description, err := renderTypeDescription(h.catalog(), locale, typ.Key)
		if err != nil {
			h.writeError(w, err)
			return
		}
		items = append(items, toTypeResponse(typ, description))
	}
	h.writeJSON(w, http.StatusOK, api.NotificationListTypesResponse{Items: items})
}

// NotificationListPreferences serves
// GET /api/v1/notifications/preferences: the caller's channel state per
// declared notification type -- one row per declared type (in declaration
// order, the matrix's rendering order), never only the stored ones, so the
// panel and the directory always agree. Each row carries the caller's
// effective channels: the stored preference where one exists, the type's
// declared defaults otherwise (a type the caller never chose is present
// with its defaults, exactly as the delivery path resolves them), both in
// canonical vocabulary order.
func (h *Handler) NotificationListPreferences(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := h.mustTenant(w, r); !ok {
		return
	}
	userID, ok := h.resolveSubject(w, r)
	if !ok {
		return
	}

	rows, err := h.prefs.ListForUser(ctx, userID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	stored := make(map[string][]string, len(rows))
	for i := range rows {
		channels, err := parseChannels(rows[i].Channels)
		if err != nil {
			h.writeError(w, ErrInternal.WithCause(err))
			return
		}
		stored[rows[i].TypeKey] = channels
	}
	types := h.prefs.NotificationTypes()
	items := make([]api.NotificationPreference, 0, len(types))
	for _, typ := range types {
		channels, has := stored[typ.Key]
		if !has {
			channels = sortedChannels(typ.DefaultChannels)
		}
		items = append(items, toPreferenceResponse(typ.Key, channels))
	}
	h.writeJSON(w, http.StatusOK, api.NotificationListPreferencesResponse{Items: items})
}

// NotificationUpdatePreference serves
// PUT /api/v1/notifications/preferences/{typeKey}/{channel}: one channel
// toggle of one notification type for the caller. Enabled=true adds the
// channel to the caller's effective set, Enabled=false removes it; the
// answer is the type's new effective state, in canonical vocabulary order,
// exactly what a subsequent GET /preferences row for the type would carry.
//
// The toggle reasons from the caller's CURRENT effective state --
// ResolveChannels, the stored preference where one exists and the type's
// defaults otherwise -- and a toggle that changes nothing (the channel was
// already in the effective set, or already absent from it) answers 200
// without writing a row: the recipient who never chose keeps receiving the
// type's declared defaults, and a no-op must not freeze them into a stored
// preference that would outlive a later change to the declaration.
//
// The ledger's own validation applies to what this call does store: a
// type nobody declared is 404 (ErrTypeNotFound), a channel the type never
// uses or outside the closed vocabulary is refused (ErrPreference-
// InvalidChannels -- checked before the no-op branch, so an unknown
// channel's off-toggle cannot silently succeed as "already absent"), and
// switching the last channel off a type whose declaration does not permit
// opting out is refused with ErrPreferenceOptoutNotAllowed.
func (h *Handler) NotificationUpdatePreference(w http.ResponseWriter, r *http.Request, typeKey string, channel api.NotificationUpdatePreferenceParamsChannel) {
	ctx := r.Context()
	if _, ok := h.mustTenant(w, r); !ok {
		return
	}
	userID, ok := h.resolveSubject(w, r)
	if !ok {
		return
	}

	ch := string(channel)
	if !isKnownChannel(ch) {
		h.writeError(w, ErrPreferenceInvalidChannels.
			WithParam("type_key", typeKey).
			WithParam("channels", ch))
		return
	}
	var body api.NotificationUpdatePreferenceRequest
	if !h.decodeJSON(w, r, &body) {
		return
	}

	current, err := h.prefs.ResolveChannels(ctx, userID, typeKey)
	if err != nil {
		h.writeError(w, err)
		return
	}
	wanted := slices.Clone(current)
	if body.Enabled {
		if !slices.Contains(wanted, ch) {
			wanted = append(wanted, ch)
		}
	} else {
		wanted = slices.DeleteFunc(wanted, func(c string) bool { return c == ch })
	}
	if slices.Equal(wanted, current) {
		h.writeJSON(w, http.StatusOK, toPreferenceResponse(typeKey, sortedChannels(wanted)))
		return
	}
	if err := h.prefs.Set(ctx, userID, typeKey, wanted); err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, toPreferenceResponse(typeKey, sortedChannels(wanted)))
}

// handleStream serves GET /api/v1/notifications/stream -- the inbox stream
// the fragment deliberately omits (see NewHandler): a server-sent-events
// connection that announces each message delivered into the caller's own
// inbox, in the caller's tenant, the moment the delivery job commits its
// row.
//
// The frames are the bus event the delivery published (InboxCreated-
// Payload: message_id, tenant_id, recipient_user_id and type_key), filtered
// here to the caller's own recipient id and tenant -- an announcement for
// another recipient of the same tenant, or for this recipient in another
// tenant (a user of several), is skipped, never forwarded. Each frame is
//
//	event: message
//	data: {the payload's JSON}
//
// followed by a flush. The connection carries no inbox content: the row is
// durable in the database before the announcement goes out, and the
// consumer reads it back through GET /messages -- the hub's whole value is
// latency, and a frame dropped for a slow or disconnected consumer loses
// nothing (hub.go's doc comment). There is no replay and no resume: a
// client that reconnects starts reading announcements from that moment and
// catches up over the list surface.
//
// The stream sends no heartbeat. A connection that survives with no
// announcements is indistinguishable from a dead one until a proxy or the
// client times it out, and the cadence that would keep one alive is a
// deployment-tuned value -- what a proxy keeps alive differs per
// installation -- so none is guessed at here. The absence is a deliberate
// non-goal of this round: the loss a heartbeat would mask, a connection a
// proxy already dropped, is recovered by the same reconnect-and-catch-up
// path any disconnection takes, and hardening the stream with a heartbeat
// is deferred alongside the platform-staff push consumer of a later round
// (AGENTS.md's Known limitations records the deferral).
//
// Requests that cannot flush (a response writer without http.Flusher) are
// refused before the stream opens; a write that fails mid-stream (the
// client went away) ends the stream.
func (h *Handler) handleStream(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.mustTenant(w, r)
	if !ok {
		return
	}
	userID, ok := h.resolveSubject(w, r)
	if !ok {
		return
	}

	flusher, ok := findFlusher(w)
	if !ok {
		h.writeError(w, ErrInternal.WithCause(errors.New("notification: the stream route needs a flushing response writer")))
		return
	}
	conn := h.hub.Subscribe()
	defer conn.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	tenantID := string(tenant)
	for {
		select {
		case <-r.Context().Done():
			// The client went away or the server is shutting down. Close
			// (deferred) removes the connection from the hub.
			return
		case msg := <-conn.Messages():
			var ann InboxCreatedPayload
			if err := json.Unmarshal(msg, &ann); err != nil {
				// The hub only ever marshals declared payload structs, so
				// an unmarshalable frame is impossible in a running system;
				// skip rather than fail the stream over it.
				continue
			}
			if ann.RecipientUserID != userID || ann.TenantID != tenantID {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// findFlusher looks for an http.Flusher underneath w, unwrapping through any
// number of http.ResponseWriter wrapper layers that expose the standard
// library's Go 1.20+ "Unwrap() http.ResponseWriter" convention (the same
// convention http.NewResponseController's own Flush/Hijack/etc. use) --
// mirroring that unwrapping algorithm directly, rather than calling
// http.NewResponseController(w).Flush() itself, since that call ALSO
// implicitly sends headers (status 200) the moment it succeeds, which would
// make it impossible for handleStream to still answer a clean, un-started
// ErrInternal response on the failure path below.
//
// A naive w.(http.Flusher) type assertion -- what this function replaced --
// is correct only when w is untouched net/http-provided ResponseWriter; it
// silently fails the instant ANY middleware wraps w in a struct that embeds
// http.ResponseWriter without separately implementing Flush itself, exactly
// the shape go/observability's own Middleware uses for its statusRecorder
// (see that type's own Unwrap doc comment, which names this very stream
// route as the reason Unwrap exists) -- a real bug this function fixes:
// every request through this app's real composed HTTP chain reaches
// handleStream through that middleware, so the naive assertion always
// failed with a 500 notification.internal_error the moment a real client
// connected, never caught by any earlier test because no earlier test in
// this repository had exercised GET /api/v1/notifications/stream through
// the fully composed chain (only through hand-built httptest requests that
// bypass Middleware) until
// examples/reference-app/integration_test/distributed_mode_test.go's
// TestServer_DistributedMode_TwoReplicas_NotificationCrossesRealInfrastructure
// did, over a real, fully composed reference-app server.
func findFlusher(w http.ResponseWriter) (http.Flusher, bool) {
	for {
		if f, ok := w.(http.Flusher); ok {
			return f, true
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil, false
		}
		w = unwrapper.Unwrap()
	}
}

// mustTenant reads the tenant the host's tenancy middleware injected into
// the request context, and refuses the request when there is none -- a
// tenant-less request reaching this handler is a host wiring error, and no
// endpoint here invents a tenant. The refusal travels as an internal
// failure with the tenant error as its cause (there is no caller-side
// tenant error to answer with: the middleware chain is the only legitimate
// source of a tenant, and its absence is a host bug). AnnotateTenant keeps
// the tenant on the request's trace for whatever this handler logs.
func (h *Handler) mustTenant(w http.ResponseWriter, r *http.Request) (pkgcore.TenantID, bool) {
	tenant, err := pkgcore.MustTenantFromContext(r.Context())
	if err != nil {
		h.writeError(w, ErrInternal.WithCause(err))
		return "", false
	}
	obs.AnnotateTenant(r.Context())
	return tenant, true
}

// resolveSubject resolves the HTTP caller's identity through the host's
// SubjectResolver seam, refusing with ErrSubjectUnresolved -- a 401 -- when
// the seam is unwired, fails, or answers an empty user id. It is the
// identification gate every operation of this handler passes (see the file
// comment): an anonymous caller gets the refusal rather than a default
// user or an empty identity, the same refusal org's handler returns when
// its own subject seam is unwired or failing. The returned id is never
// taken from the request, the context or a header -- only the host's seam
// says who the caller is.
func (h *Handler) resolveSubject(w http.ResponseWriter, r *http.Request) (string, bool) {
	if h.subject == nil {
		h.writeError(w, ErrSubjectUnresolved)
		return "", false
	}
	userID, ok := h.subject.Subject(r)
	if !ok || userID == "" {
		h.writeError(w, ErrSubjectUnresolved)
		return "", false
	}
	return userID, true
}

// decodeJSON decodes the request body into dst, refusing the request whole
// with ErrInvalidRequestBody -- never partially decoded -- when the body is
// missing or is not the JSON the operation's schema requires. A malformed
// payload therefore changes nothing.
func (h *Handler) decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		h.writeError(w, ErrInvalidRequestBody.WithCause(errors.New("notification: empty request body")))
		return false
	}
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		h.writeError(w, ErrInvalidRequestBody.WithCause(err))
		return false
	}
	return true
}

// writeJSON writes body as the JSON response of status. The Content-Type
// carries the charset explicitly, matching org and the reference app's
// notes handler. An encode failure after the status line went out is a
// broken client connection, not a response defect; there is nothing to do
// about it but stop.
func (h *Handler) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		return
	}
}

// writeError writes err as the fragment's NotificationError envelope: an
// error that is an *apperr.Error travels as its own code, params and
// status; anything else is an internal failure with err as its cause. This
// is the single place the envelope is built, so every refusal this handler
// sends has the same shape.
func (h *Handler) writeError(w http.ResponseWriter, err error) {
	appErr, ok := apperr.As(err)
	if !ok {
		appErr = ErrInternal.WithCause(err)
	}
	body := api.NotificationError{Code: &appErr.Code}
	if appErr.Params != nil {
		params := appErr.Params
		body.Params = &params
	}
	h.writeJSON(w, appErr.Status, body)
}

// toMessageResponse converts one inbox row to its API shape. Params -- the
// row's template-parameter JSON -- is always an object in the response,
// never null and never absent (the generated type carries no omitempty on
// it): a row whose params column is empty or NULL was rendered from no
// parameters and answers an empty map. DedupeKey never appears: it is a
// delivery-internal id (the redelivery probe's key) and the spec serves the
// row without it.
func toMessageResponse(m *InboxMessage) (api.NotificationMessage, error) {
	var params map[string]interface{}
	if m.Params != nil {
		if err := json.Unmarshal(m.Params, &params); err != nil {
			return api.NotificationMessage{}, fmt.Errorf("notification: corrupt params JSON on inbox row %s: %w", m.ID, err)
		}
	}
	if params == nil {
		params = map[string]interface{}{}
	}
	return api.NotificationMessage{
		ID:        m.ID,
		TypeKey:   m.TypeKey,
		Group:     m.Group,
		Title:     m.Title,
		Body:      m.Body,
		Params:    params,
		Link:      m.Link,
		ExpiryAt:  m.ExpiryAt,
		ReadAt:    m.ReadAt,
		CreatedAt: m.CreatedAt,
	}, nil
}

// toContactResponse converts one ledger row to its API shape, stripping
// everything the spec never serves: the address (plaintext PII), its blind
// index (an offline brute-force surface) and the consent bookkeeping
// (ConsentAt/VerifiedAt are audit concerns). The API row is id, channel,
// status and timestamps only.
func toContactResponse(c *VerifiedContact) api.NotificationContact {
	return api.NotificationContact{
		ID:        c.ID,
		Channel:   api.NotificationContactChannel(c.Channel),
		Status:    api.NotificationContactStatus(c.Status),
		CreatedAt: c.CreatedAt,
	}
}

// toPreferenceResponse converts an effective channel set to its API shape.
// The channels are copied, never aliased, so a caller mutating the
// response cannot corrupt the caller's stored preference or a type's
// declared defaults.
func toPreferenceResponse(typeKey string, channels []string) api.NotificationPreference {
	return api.NotificationPreference{
		TypeKey:  typeKey,
		Channels: slices.Clone(channels),
	}
}

// toTypeResponse converts one declared type to its API shape, with its
// directory copy (the description rendered by the caller) attached.
func toTypeResponse(t pkgcore.NotificationType, description string) api.NotificationType {
	return api.NotificationType{
		TypeKey:         t.Key,
		Group:           t.Group,
		Description:     description,
		DefaultChannels: slices.Clone(t.DefaultChannels),
		Unsubscribable:  t.Unsubscribable,
	}
}

// negotiateLocale picks the language a request's type-directory copy (and
// any future locale-sensitive rendering) is served in: the first
// Accept-Language tag the merged catalog supports, where a header tag
// matches a supported locale exactly or as its language prefix ("en"
// matches "en-US"), and zh-CN -- the module's default language -- when
// nothing matches, the header is empty, or no catalog is attached yet.
//
// The header's relative q-values are deliberately ignored beyond q=0: a
// part explicitly weighted 0 is "not acceptable" and is skipped, and the
// remaining parts answer in header order, which is exactly the latitude
// RFC 7231's section 5.3.1 gives a server ("the most specific reference
// has precedence"; order of preference is expressed by the header's own
// ordering). The fallback never crosses languages: a request that accepts
// nothing the catalog supports gets the default language, never a
// half-rendered or silently substituted one.
func negotiateLocale(catalog *i18n.Catalog, header string) string {
	if catalog == nil {
		return i18n.LocaleZHCN
	}
	supported := catalog.Locales()
	if len(supported) == 0 || strings.TrimSpace(header) == "" {
		return i18n.LocaleZHCN
	}
	for _, part := range strings.Split(header, ",") {
		tag := strings.TrimSpace(part)
		if tag == "" || tag == "*" {
			continue
		}
		if params := strings.Split(tag, ";"); len(params) > 1 {
			for _, p := range params[1:] {
				p = strings.TrimSpace(p)
				if strings.HasPrefix(p, "q=") && p[2:] == "0" {
					goto nextPart
				}
			}
			tag = strings.TrimSpace(params[0])
		}
		if tag == "" {
			continue
		}
		for _, have := range supported {
			if have == tag || strings.HasPrefix(have, tag+"-") {
				return have
			}
		}
	nextPart:
	}
	return i18n.LocaleZHCN
}

// SubjectResolver is the seam that answers "who is the HTTP caller" for
// every operation this handler serves. It is declared here -- in the file
// whose consumers (resolveSubject) read it -- the way org declares its own
// seam in org's handler package, and it is structurally identical to org's
// SubjectResolver: the two interfaces declare the same single method over
// stdlib types only, so any type org's host wired for org satisfies
// notification's seam too, and neither module imports the other.
//
// The implementation is the host's to supply: in the reference app, the
// demo identity layer reads a signed-in demo user from the same request
// attribute its tenancy resolution wrote (cmd/server's demo_subject.go);
// in a host running the authn module, whatever connects a verified
// principal to the request answers here. The module itself never reads the
// caller's identity from a header, the context or the request path, and it
// never imports an authenticating module's types -- the seam is the whole
// of its knowledge of who its callers are.
//
// A resolver that returns ok=false -- or is not wired at all -- fails
// every endpoint closed with ErrSubjectUnresolved (see resolveSubject): an
// anonymous caller is refused, never served a default user or an empty
// identity.
type SubjectResolver interface {
	// Subject returns the caller's user id for the request, and whether
	// the request could be attributed to a user at all. A nil seam, a
	// failing seam and a seam that cannot attribute the request all
	// answer ok=false; the handler refuses with ErrSubjectUnresolved in
	// every such case, and treats an empty user id the same as no user.
	Subject(r *http.Request) (userID string, ok bool)
}

// compile-time check that *Handler satisfies the generated ServerInterface
// for this module's fragment -- the guarantee that a spec whose generated
// interface outgrew this handler cannot compile (docs/internal/21's
// spec-first order).
var _ api.ServerInterface = (*Handler)(nil)
