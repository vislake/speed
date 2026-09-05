package admin

import (
	"encoding/json"
	"net/http"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/rbac"

	"github.com/vislake/speed/go/admin/api"
)

const jsonContentType = "application/json; charset=utf-8"

// errInternal is written when an error reaching writeError is not an
// *apperr.Error at all -- an unexpected internal failure this module
// never classified, matching every other handler's identical fallback
// (see notes/handler.go's errInternal).
var errInternal = apperr.Internal("admin.internal")

// Handler serves admin's HTTP endpoints by implementing the
// spec-generated api.ServerInterface (see api/admin-server.gen.go,
// regenerated from api/openapi.yaml by task api:gen -- the compile-time
// assertion at the bottom of this file is what makes "spec changed,
// handler not" a compile failure instead of a runtime surprise).
//
// Every method reads the calling operator's identity from
// authn.PrincipalFromContext, never from a request parameter, header or
// body -- exactly like every other module's handler. Authorization (which
// rbac.SystemDomain permission gates which operation) is NOT this
// Handler's job: per docs/internal/23-admin.md section 6 and this round's
// scope, every admin route is gated externally, the same way the
// reference app gates notes and storage, by wrapping the mounted route in
// rbac.RequirePermission before it ever reaches here.
type Handler struct {
	tenants       *TenantService
	impersonation *ImpersonationService
	search        *SearchService
	auditSvc      *AuditService
	exportSvc     *ExportService
	roles         *RoleService
	usage         *UsageService
	sendRecords   *SendRecordSearchService
	mux           *http.ServeMux
}

// NewHandler returns a Handler serving the given services' operations.
//
// The returned Handler's routing is registered by the generated
// api.HandlerFromMux helper, mirroring every other module's identical
// handler-construction pattern (see notes/handler.go's NewHandler for the
// full rationale).
func NewHandler(tenants *TenantService, impersonation *ImpersonationService, search *SearchService, auditSvc *AuditService, exportSvc *ExportService, roles *RoleService, usage *UsageService, sendRecords *SendRecordSearchService) *Handler {
	h := &Handler{
		tenants:       tenants,
		impersonation: impersonation,
		search:        search,
		auditSvc:      auditSvc,
		exportSvc:     exportSvc,
		roles:         roles,
		usage:         usage,
		sendRecords:   sendRecords,
	}
	h.mux = http.NewServeMux()
	api.HandlerFromMux(h, h.mux)
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// callerUserID resolves the calling platform operator's user id from the
// verified Principal authn.Middleware installed on the request context.
// It never falls back to a header or any other unverified source.
func callerUserID(r *http.Request) (string, error) {
	principal, ok := authn.PrincipalFromContext(r.Context())
	if !ok || principal.UserID == "" {
		return "", ErrPrincipalRequired
	}
	return principal.UserID, nil
}

// --- D3: tenant ledger ---------------------------------------------------

// AdminListTenants implements api.ServerInterface.
func (h *Handler) AdminListTenants(w http.ResponseWriter, r *http.Request, params api.AdminListTenantsParams) {
	filter := TenantFilter{}
	if params.Status != nil {
		filter.Status = TenantStatus(*params.Status)
	}
	if params.Cursor != nil {
		filter.Cursor = *params.Cursor
	}
	if params.Limit != nil {
		filter.Limit = *params.Limit
	}

	rows, err := h.tenants.List(r.Context(), filter)
	if err != nil {
		writeError(w, err)
		return
	}
	resp := api.AdminListTenantsResponse{Tenants: make([]api.AdminTenant, 0, len(rows))}
	for _, row := range rows {
		resp.Tenants = append(resp.Tenants, toAdminTenant(row))
	}
	writeJSON(w, http.StatusOK, resp)
}

// AdminCreateTenant implements api.ServerInterface.
func (h *Handler) AdminCreateTenant(w http.ResponseWriter, r *http.Request) {
	callerID, err := callerUserID(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var req api.AdminCreateTenantRequest
	if decodeErr := json.NewDecoder(r.Body).Decode(&req); decodeErr != nil {
		writeError(w, ErrTenantIDRequired.WithCause(decodeErr))
		return
	}
	t := &Tenant{TenantID: req.TenantID, CreatedBy: callerID}
	if req.DisplayName != nil {
		t.DisplayName = *req.DisplayName
	}
	if req.Notes != nil {
		t.Notes = *req.Notes
	}
	if err := h.tenants.Create(r.Context(), t); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAdminTenant(*t))
}

// AdminGetTenant implements api.ServerInterface.
func (h *Handler) AdminGetTenant(w http.ResponseWriter, r *http.Request, id string) {
	t, err := h.tenants.Get(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAdminTenant(*t))
}

// AdminUpdateTenant implements api.ServerInterface.
func (h *Handler) AdminUpdateTenant(w http.ResponseWriter, r *http.Request, id string) {
	callerID, err := callerUserID(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var req api.AdminUpdateTenantRequest
	if decodeErr := json.NewDecoder(r.Body).Decode(&req); decodeErr != nil {
		writeError(w, ErrTenantIDRequired.WithCause(decodeErr))
		return
	}
	patch := TenantPatch{DisplayName: req.DisplayName, Notes: req.Notes, SuspendedReason: req.SuspendedReason}
	if req.Status != nil {
		status := TenantStatus(*req.Status)
		patch.Status = &status
	}
	actor := pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: callerID}
	t, err := h.tenants.SetStatus(r.Context(), id, patch, actor)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAdminTenant(*t))
}

func toAdminTenant(t Tenant) api.AdminTenant {
	out := api.AdminTenant{
		TenantID:    t.TenantID,
		DisplayName: t.DisplayName,
		Status:      api.AdminTenantStatus(t.Status),
		CreatedAt:   t.CreatedAt,
		CreatedBy:   t.CreatedBy,
		Notes:       t.Notes,
	}
	if t.SuspendedReason != "" {
		out.SuspendedReason = &t.SuspendedReason
	}
	out.SuspendedAt = t.SuspendedAt
	return out
}

// --- D6: cross-tenant user search -----------------------------------------

// AdminSearchUsers implements api.ServerInterface.
func (h *Handler) AdminSearchUsers(w http.ResponseWriter, r *http.Request, params api.AdminSearchUsersParams) {
	q := authn.UserSearchQuery{}
	if params.Email != nil {
		q.Email = *params.Email
	}
	if params.Phone != nil {
		q.Phone = *params.Phone
	}
	if params.DisplayNamePrefix != nil {
		q.DisplayNamePrefix = *params.DisplayNamePrefix
	}
	if params.Limit != nil {
		q.Limit = *params.Limit
	}

	users, err := h.search.Users(r.Context(), q)
	if err != nil {
		writeError(w, err)
		return
	}
	resp := api.AdminSearchUsersResponse{Users: make([]api.AdminUser, 0, len(users))}
	for _, u := range users {
		out := api.AdminUser{ID: u.ID, DisplayName: u.DisplayName}
		if u.Email != "" {
			out.Email = &u.Email
		}
		if u.Phone != "" {
			out.Phone = &u.Phone
		}
		resp.Users = append(resp.Users, out)
	}
	writeJSON(w, http.StatusOK, resp)
}

// AdminListUserMemberships implements api.ServerInterface.
func (h *Handler) AdminListUserMemberships(w http.ResponseWriter, r *http.Request, id string) {
	callerID, err := callerUserID(r)
	if err != nil {
		writeError(w, err)
		return
	}
	tenants, err := h.search.MembershipsOf(r.Context(), id, callerID)
	if err != nil {
		writeError(w, err)
		return
	}
	resp := api.AdminListUserMembershipsResponse{TenantIds: make([]string, 0, len(tenants))}
	for _, t := range tenants {
		resp.TenantIds = append(resp.TenantIds, string(t))
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- D5: impersonation -----------------------------------------------------

// AdminStartImpersonation implements api.ServerInterface.
func (h *Handler) AdminStartImpersonation(w http.ResponseWriter, r *http.Request) {
	callerID, err := callerUserID(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var req api.AdminStartImpersonationRequest
	if decodeErr := json.NewDecoder(r.Body).Decode(&req); decodeErr != nil {
		writeError(w, ErrImpersonationTargetRequired.WithCause(decodeErr))
		return
	}
	locale := ""
	if req.Locale != nil {
		locale = *req.Locale
	}
	grant, err := h.impersonation.Start(r.Context(), StartInput{
		AdminUserID:    callerID,
		TargetUserID:   req.TargetUserID,
		TargetTenantID: pkgcore.TenantID(req.TargetTenantID),
		Reason:         req.Reason,
		Locale:         locale,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAdminGrant(*grant))
}

// AdminEndImpersonation implements api.ServerInterface.
func (h *Handler) AdminEndImpersonation(w http.ResponseWriter, r *http.Request, id string) {
	callerID, err := callerUserID(r)
	if err != nil {
		writeError(w, err)
		return
	}
	grant, err := h.impersonation.End(r.Context(), id, callerID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAdminGrant(*grant))
}

// AdminListImpersonationGrants implements api.ServerInterface.
func (h *Handler) AdminListImpersonationGrants(w http.ResponseWriter, r *http.Request) {
	grants, err := h.impersonation.ListActive(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	resp := api.AdminListImpersonationGrantsResponse{Grants: make([]api.AdminImpersonationGrant, 0, len(grants))}
	for _, g := range grants {
		resp.Grants = append(resp.Grants, toAdminGrant(g))
	}
	writeJSON(w, http.StatusOK, resp)
}

func toAdminGrant(g ImpersonationGrant) api.AdminImpersonationGrant {
	out := api.AdminImpersonationGrant{
		ID:             g.ID,
		AdminUserID:    g.AdminUserID,
		TargetUserID:   g.TargetUserID,
		TargetTenantID: g.TargetTenantID,
		Reason:         g.Reason,
		CreatedAt:      g.CreatedAt,
		ExpiresAt:      g.ExpiresAt,
		EndedAt:        g.EndedAt,
	}
	if g.EndedBy != "" {
		out.EndedBy = &g.EndedBy
	}
	return out
}

// --- D7: audit query -------------------------------------------------------

// AdminListAuditEvents implements api.ServerInterface.
func (h *Handler) AdminListAuditEvents(w http.ResponseWriter, r *http.Request, params api.AdminListAuditEventsParams) {
	callerID, err := callerUserID(r)
	if err != nil {
		writeError(w, err)
		return
	}
	filter := AuditFilter{}
	if params.TenantID != nil {
		filter.TenantID = *params.TenantID
	}
	if params.Actor != nil {
		filter.Actor = *params.Actor
	}
	if params.Resource != nil {
		filter.Resource = *params.Resource
	}
	if params.Action != nil {
		filter.Action = *params.Action
	}
	if params.From != nil {
		filter.From = *params.From
	}
	if params.To != nil {
		filter.To = *params.To
	}
	if params.Success != nil {
		filter.Success = params.Success
	}

	events, err := h.auditSvc.Query(r.Context(), callerID, filter)
	if err != nil {
		writeError(w, err)
		return
	}

	// Pagination is admin's own HTTP-layer translation (D7: an HTTP shell
	// plus pagination-parameter translation) over compliance.AuditQuery's already-filtered,
	// already-sorted (newest first) result -- compliance itself does no
	// pagination (its own known limitation, inherited here rather than
	// re-solved), so this is a plain slice operation, never a second SQL
	// query.
	offset := 0
	if params.Offset != nil && *params.Offset > 0 {
		offset = *params.Offset
	}
	limit := len(events)
	if params.Limit != nil && *params.Limit > 0 {
		limit = *params.Limit
	}
	events = paginate(events, offset, limit)

	resp := api.AdminListAuditEventsResponse{Events: make([]api.AdminAuditEvent, 0, len(events))}
	for _, e := range events {
		resp.Events = append(resp.Events, toAdminAuditEvent(e))
	}
	writeJSON(w, http.StatusOK, resp)
}

// paginate returns events[offset:offset+limit], clamped to a valid slice
// range for any offset/limit combination -- including an offset beyond
// the slice's length, which returns an empty (never a panicking) result.
func paginate(events []audit.AuditEvent, offset, limit int) []audit.AuditEvent {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(events) {
		return nil
	}
	end := offset + limit
	if end > len(events) || limit < 0 {
		end = len(events)
	}
	return events[offset:end]
}

func toAdminAuditEvent(e audit.AuditEvent) api.AdminAuditEvent {
	out := api.AdminAuditEvent{
		ID:           e.ID,
		ActorType:    e.ActorType,
		ActorID:      e.ActorID,
		Action:       e.Action,
		ResourceType: e.ResourceType,
		ResourceID:   e.ResourceID,
		Success:      e.Success,
		OccurredAt:   e.OccurredAt,
	}
	if e.ActorDisplayName != "" {
		out.ActorDisplayName = &e.ActorDisplayName
	}
	if e.OnBehalfOfType != nil {
		out.OnBehalfOfType = e.OnBehalfOfType
	}
	if e.OnBehalfOfID != nil {
		out.OnBehalfOfID = e.OnBehalfOfID
	}
	if e.FailureReason != "" {
		out.FailureReason = &e.FailureReason
	}
	if e.TenantID != "" {
		tenantID := e.TenantID
		out.TenantID = &tenantID
	}
	return out
}

// --- D7: audit export -------------------------------------------------------

// AdminExportAuditEvents implements api.ServerInterface.
func (h *Handler) AdminExportAuditEvents(w http.ResponseWriter, r *http.Request) {
	var req api.AdminExportAuditEventsRequest
	if decodeErr := json.NewDecoder(r.Body).Decode(&req); decodeErr != nil {
		writeError(w, ErrTenantIDRequired.WithCause(decodeErr))
		return
	}
	jobID, err := h.exportSvc.Enqueue(r.Context(), req.TenantID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, api.AdminExportAuditEventsResponse{JobID: string(jobID)})
}

// --- D8: role management -----------------------------------------------------

// AdminListDeclaredPermissions implements api.ServerInterface.
func (h *Handler) AdminListDeclaredPermissions(w http.ResponseWriter, r *http.Request) {
	perms, err := h.roles.DeclaredPermissions()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.AdminListDeclaredPermissionsResponse{Permissions: perms})
}

// AdminDefineRole implements api.ServerInterface.
func (h *Handler) AdminDefineRole(w http.ResponseWriter, r *http.Request) {
	var req api.AdminDefineRoleRequest
	if decodeErr := json.NewDecoder(r.Body).Decode(&req); decodeErr != nil {
		writeError(w, ErrTenantIDRequired.WithCause(decodeErr))
		return
	}
	def := rbac.RoleDefinition{Key: req.Key, Permissions: req.Permissions}
	if req.DescriptionKey != nil {
		def.DescriptionKey = *req.DescriptionKey
	}
	role, err := h.roles.DefineRole(r.Context(), req.TenantID, def)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, api.AdminRole{
		ID:             role.ID,
		TenantID:       role.TenantID,
		Key:            role.Key,
		DescriptionKey: role.DescriptionKey,
		Permissions:    req.Permissions,
	})
}

// AdminCreateRoleBinding implements api.ServerInterface.
func (h *Handler) AdminCreateRoleBinding(w http.ResponseWriter, r *http.Request, id string) {
	var req api.AdminCreateRoleBindingRequest
	if decodeErr := json.NewDecoder(r.Body).Decode(&req); decodeErr != nil {
		writeError(w, ErrTenantIDRequired.WithCause(decodeErr))
		return
	}
	nodeID := ""
	if req.NodeID != nil {
		nodeID = *req.NodeID
	}
	if err := h.roles.AssignRole(r.Context(), req.TenantID, req.UserID, id, nodeID); err != nil {
		writeError(w, err)
		return
	}
	out := api.AdminRoleBinding{TenantID: req.TenantID, UserID: req.UserID, Role: id}
	if nodeID != "" {
		out.NodeID = &nodeID
	}
	writeJSON(w, http.StatusCreated, out)
}

// --- D9: usage/billing dashboard ---------------------------------------------

// AdminGetUsageSummary implements api.ServerInterface.
func (h *Handler) AdminGetUsageSummary(w http.ResponseWriter, r *http.Request) {
	callerID, err := callerUserID(r)
	if err != nil {
		writeError(w, err)
		return
	}
	rows, err := h.usage.Summary(r.Context(), callerID)
	if err != nil {
		writeError(w, err)
		return
	}
	resp := api.AdminUsageSummaryResponse{Rows: make([]api.AdminUsageSummaryRow, 0, len(rows))}
	for _, row := range rows {
		resp.Rows = append(resp.Rows, toAdminUsageSummaryRow(row))
	}
	writeJSON(w, http.StatusOK, resp)
}

func toAdminUsageSummaryRow(row UsageSummaryRow) api.AdminUsageSummaryRow {
	out := api.AdminUsageSummaryRow{TenantID: row.TenantID, DisplayName: row.DisplayName}
	if row.MeteringSummaries != nil {
		summaries := make([]api.AdminUsageFeatureSummary, 0, len(row.MeteringSummaries))
		for _, s := range row.MeteringSummaries {
			summaries = append(summaries, api.AdminUsageFeatureSummary{
				Feature:     s.Feature,
				PeriodStart: s.PeriodStart,
				PeriodEnd:   s.PeriodEnd,
				Quantity:    float32(s.Quantity),
			})
		}
		out.MeteringSummaries = &summaries
	}
	if row.CreditBalance != nil {
		out.CreditBalance = &api.AdminCreditBalance{
			Available: int(row.CreditBalance.Available),
			Reserved:  int(row.CreditBalance.Reserved),
		}
	}
	if row.ActiveSubscription != nil {
		out.ActiveSubscription = &api.AdminSubscription{
			ID:        row.ActiveSubscription.ID,
			PlanID:    row.ActiveSubscription.PlanID,
			Status:    row.ActiveSubscription.Status,
			CreatedAt: row.ActiveSubscription.CreatedAt,
		}
	}
	return out
}

// --- D10: notification send-record search ------------------------------------

// defaultSendRecordSearchLimit/maxSendRecordSearchLimit resolve D10's own
// limit before SendRecordRepository.ListByFilter is ever called: that
// method never clamps (its own doc comment: the caller resolves the
// default and cap first), and gorm's Limit(0) means "zero rows", not "no
// limit" -- an unresolved zero limit here would silently return an empty
// result for a caller that simply omitted the parameter.
const (
	defaultSendRecordSearchLimit = 50
	maxSendRecordSearchLimit     = 500
)

// AdminListSendRecords implements api.ServerInterface.
func (h *Handler) AdminListSendRecords(w http.ResponseWriter, r *http.Request, params api.AdminListSendRecordsParams) {
	callerID, err := callerUserID(r)
	if err != nil {
		writeError(w, err)
		return
	}

	filter := notification.SendRecordFilter{Limit: defaultSendRecordSearchLimit}
	if params.Channel != nil {
		filter.Channel = *params.Channel
	}
	if params.Status != nil {
		filter.Status = *params.Status
	}
	if params.From != nil {
		filter.From = *params.From
	}
	if params.To != nil {
		filter.To = *params.To
	}
	if params.Limit != nil && *params.Limit > 0 {
		filter.Limit = *params.Limit
	}
	if filter.Limit > maxSendRecordSearchLimit {
		filter.Limit = maxSendRecordSearchLimit
	}
	if params.Offset != nil {
		filter.Offset = *params.Offset
	}

	var tenantID string
	if params.TenantID != nil {
		tenantID = *params.TenantID
	}

	records, err := h.sendRecords.Query(r.Context(), callerID, tenantID, filter)
	if err != nil {
		writeError(w, err)
		return
	}
	resp := api.AdminListSendRecordsResponse{Records: make([]api.AdminSendRecord, 0, len(records))}
	for _, rec := range records {
		resp.Records = append(resp.Records, toAdminSendRecord(rec))
	}
	writeJSON(w, http.StatusOK, resp)
}

func toAdminSendRecord(rec notification.SendRecord) api.AdminSendRecord {
	out := api.AdminSendRecord{
		ID:             rec.ID,
		TenantID:       rec.TenantID,
		TypeKey:        rec.TypeKey,
		Channel:        rec.Channel,
		RecipientClass: rec.RecipientClass,
		Status:         rec.Status,
		CreatedAt:      rec.CreatedAt,
	}
	if rec.RecipientUserID != "" {
		out.RecipientUserID = &rec.RecipientUserID
	}
	if rec.ContactID != "" {
		out.ContactID = &rec.ContactID
	}
	if rec.Error != "" {
		out.Error = &rec.Error
	}
	if rec.IdempotencyKey != "" {
		out.IdempotencyKey = &rec.IdempotencyKey
	}
	return out
}

// --- shared response helpers ------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, err error) {
	appErr, ok := apperr.As(err)
	if !ok {
		appErr = errInternal
	}
	envelope := api.AdminError{Code: &appErr.Code}
	if appErr.Params != nil {
		params := appErr.Params
		envelope.Params = &params
	}
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(appErr.Status)
	_ = json.NewEncoder(w).Encode(envelope)
}

// compile-time check that *Handler satisfies api.ServerInterface.
var _ api.ServerInterface = (*Handler)(nil)
