package billing

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"

	"github.com/vislake/speed/go/billing/api"
)

// The bound and default shared by the fragment's two list routes
// (credit transactions and invoices -- each route's handler narrows
// its full listing to this window). They must agree with
// api/openapi.yaml's own description of the same parameter (1-100,
// default 50, spelled identically on both operations) -- this module
// keeps no second copy of that contract, and a spec edit that widens
// the bound without touching these constants makes the handler refuse
// a value the spec advertises, which is exactly the drift direction a
// compile cannot catch.
const (
	defaultListPageSize = 50
	minListLimit        = 1
	maxListLimit        = 100
)

// jsonContentType is the media type every response this file writes
// carries -- the same named constant go/storage's handler.go uses.
const jsonContentType = "application/json; charset=utf-8"

// Handler implements api.ServerInterface -- the app-side implementation of
// the spec fragment's four read operations: GET /api/v1/billing/credits/
// balance and GET /api/v1/billing/credits/transactions over CreditService,
// and GET /api/v1/billing/invoices and GET /api/v1/billing/invoices/{id}
// over InvoiceRepository (the invoice list/detail pair served for a
// tenant-facing invoice page). It is the module's whole HTTP surface, and
// it is read-only by design (see
// api/openapi.yaml's own header for the decision and its consequence):
// every operation below answers from the module's own services and
// repositories -- never a credit or invoice mutation of any kind -- and
// the tenant is always the one tenancy.Middleware resolved into the
// request context, never anything the caller supplies.
//
// Handler is deliberately thin in the same way every other module
// handler in this codebase is thin: it extracts the request's inputs,
// delegates the reads to CreditService and InvoiceRepository in full,
// and maps the results -- with the coded error envelope of errors.go's
// own index, never localized text and never a raw Go error -- because
// the tenant scoping that actually matters lives one layer down, in the
// tenant-filtered reads both perform from ctx. A host that mounts this
// handler behind its own authorization layer (as the reference app
// does, gating the route on the module's declared billing:credit:read
// permission) gets exactly the surface the fragment promises; see
// module.go's Register for the mount.
type Handler struct {
	credits  *CreditService
	invoices *InvoiceRepository
	mux      *http.ServeMux
}

// NewHandler returns a Handler serving the fragment's read operations
// through credits and invoices -- the *CreditService and
// *InvoiceRepository Module.Register mounts it behind, in the same call
// that attaches it at apiPath.
//
// Unlike a bare api.HandlerFromMux, this constructor wires the mux
// through api.HandlerWithOptions with a custom ErrorHandlerFunc
// (bindingErrorHandler below): oapi-codegen's default would answer a
// request the spec-generated parameter binder itself rejects -- a limit
// query value that is not an integer at all -- with a plain http.Error
// text body, and this surface's contract (see api/openapi.yaml's 400
// description) promises a caller a mapped billing code instead, the same
// "never a raw error" promise go/sharing's handler.go makes for its own
// binder failures via the identical mechanism.
func NewHandler(credits *CreditService, invoices *InvoiceRepository) *Handler {
	h := &Handler{credits: credits, invoices: invoices}
	h.mux = http.NewServeMux()
	api.HandlerWithOptions(h, api.StdHTTPServerOptions{
		BaseRouter:       h.mux,
		ErrorHandlerFunc: bindingErrorHandler,
	})
	return h
}

// bindingErrorHandler is NewHandler's ErrorHandlerFunc: it runs in place
// of oapi-codegen's default (a bare http.Error) whenever the
// spec-generated parameter binder rejects a request before Handler's own
// method is ever called. It answers the same BillingError JSON envelope
// every other refusal on this surface produces, with ErrInvalidRequest
// carrying the parameter the binder named as a structured param -- the
// malformed-query half of this surface's "a refused or malformed query
// answers a mapped bilingual code, never a raw error" contract. Only the
// two binder error shapes this surface's single bindable parameter can
// actually produce are mapped; anything else (an error shape a future
// spec edit could introduce before its handler exists) folds to the same
// coded envelope rather than ever leaking the binder's own text.
func bindingErrorHandler(w http.ResponseWriter, _ *http.Request, err error) {
	appErr := ErrInvalidRequest.WithCause(err)
	var invalid *api.InvalidParamFormatError
	if errors.As(err, &invalid) {
		appErr = appErr.WithParam("parameter", invalid.ParamName)
	}
	var required *api.RequiredParamError
	if errors.As(err, &required) {
		appErr = appErr.WithParam("parameter", required.ParamName)
	}
	writeError(w, appErr)
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// mustTenant resolves the caller's tenant, annotating the request's span.
// Unreachable in normal operation -- see go/storage's identical comment
// on its own equivalent call -- because a host must never allowlist this
// module's routes, so tenancy.Middleware has already rejected anything
// that could reach here with no resolved tenant. Handled anyway, never
// assumed away: every service call underneath would otherwise fail closed
// with this exact unwrapped error one layer down, with a less specific
// log line.
func mustTenant(w http.ResponseWriter, r *http.Request) (pkgcore.TenantID, bool) {
	tenant, err := pkgcore.MustTenantFromContext(r.Context())
	if err != nil {
		writeError(w, ErrInternal.WithCause(err))
		return "", false
	}
	observability.AnnotateTenant(r.Context())
	return tenant, true
}

// BillingGetCreditBalance implements api.ServerInterface: GET
// /api/v1/billing/credits/balance. Answers the caller's tenant balance --
// the number a credit view renders -- read through the same
// CreditService.Balance call business code uses, so a route read and a
// service-side reservation never disagree about what the tenant holds. A
// tenant that has never touched credits answers an all-zero balance (the
// service materializes the row on first read), never a 404.
func (h *Handler) BillingGetCreditBalance(w http.ResponseWriter, r *http.Request) {
	if _, ok := mustTenant(w, r); !ok {
		return
	}

	bal, err := h.credits.Balance(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, api.BillingCreditBalance{
		Available: bal.Available,
		Reserved:  bal.Reserved,
		UpdatedAt: bal.UpdatedAt,
	})
}

// BillingListCreditTransactions implements api.ServerInterface: GET
// /api/v1/billing/credits/transactions. Answers the recent window of the
// caller's tenant credit ledger: the rows CreditService.Transactions
// returns, newest first, narrowed to at most limit (1-100, default 50).
// A refund is observable here as its deduct row's status having become
// "refunded" -- never as a row that vanishes, and never as a balance-only
// change with no ledger trace (the classification vocabulary is spelled
// out in api/openapi.yaml's own operation description).
//
// The narrowing happens after the service's full listing rather than as a
// SQL LIMIT: CreditService.Transactions returns the whole ledger, and the
// recent window is served by truncating it. That is a memory trade-off,
// never a correctness one; a keyset-paginated service read is what a real
// ledger-sized history would need (see the module's Known limitations).
func (h *Handler) BillingListCreditTransactions(w http.ResponseWriter, r *http.Request, params api.BillingListCreditTransactionsParams) {
	if _, ok := mustTenant(w, r); !ok {
		return
	}

	limit := defaultListPageSize
	if params.Limit != nil {
		limit = *params.Limit
		if limit < minListLimit || limit > maxListLimit {
			writeError(w, ErrInvalidLimit.
				WithParam("limit", limit).
				WithParam("min", minListLimit).
				WithParam("max", maxListLimit))
			return
		}
	}

	rows, err := h.credits.Transactions(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}

	// An explicit make, so an empty ledger marshals as [] -- never null --
	// and the response always carries the transactions array the schema
	// promises.
	items := make([]api.BillingCreditTransaction, 0, len(rows))
	for i := range rows {
		items = append(items, toTransactionResponse(&rows[i]))
	}
	writeJSON(w, http.StatusOK, api.BillingListCreditTransactionsResponse{Transactions: items})
}

// toTransactionResponse maps one ledger row to its wire shape. Amount and
// Reason travel verbatim; Type and Status are casts of the module's own
// closed vocabulary onto the spec's enum of the same values (a row can
// only carry grant/deduct/expire and pending/confirmed/refunded -- the
// type "refund" exists in model.go's vocabulary as the name of the
// deduct-row resolution, never as a row's own Type), and CreatedAt
// renders on the wire the same RFC3339Nano form time.Time always takes.
func toTransactionResponse(tx *CreditTransaction) api.BillingCreditTransaction {
	return api.BillingCreditTransaction{
		ID:        tx.ID,
		Type:      api.BillingCreditTransactionType(tx.Type),
		Status:    api.BillingCreditTransactionStatus(tx.Status),
		Amount:    tx.Amount,
		Reason:    tx.Reason,
		CreatedAt: tx.CreatedAt,
	}
}

// BillingListInvoices implements api.ServerInterface: GET
// /api/v1/billing/invoices. Answers the caller's tenant's billing
// documents, newest first -- the rows InvoiceRepository.ListByTenant
// returns (created_at DESC, the issue order that is total where cycle
// dates are not), narrowed to at most limit (1-100, default 50). The
// status vocabulary on the wire is the lifecycle's own: open awaiting
// payment, paid settled in full, void canceled before payment.
//
// The narrowing happens after the repository's full listing rather than
// as a SQL LIMIT, the identical trade-off the transactions route
// documents for its own recent window (see
// BillingListCreditTransactions): a memory trade-off, never a
// correctness one; a keyset-paginated service read is what a real
// history-sized invoice set would need (see the module's Known
// limitations).
func (h *Handler) BillingListInvoices(w http.ResponseWriter, r *http.Request, params api.BillingListInvoicesParams) {
	if _, ok := mustTenant(w, r); !ok {
		return
	}

	limit := defaultListPageSize
	if params.Limit != nil {
		limit = *params.Limit
		if limit < minListLimit || limit > maxListLimit {
			writeError(w, ErrInvalidLimit.
				WithParam("limit", limit).
				WithParam("min", minListLimit).
				WithParam("max", maxListLimit))
			return
		}
	}

	rows, err := h.invoices.ListByTenant(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}

	// An explicit make, so a never-invoiced tenant marshals as [] --
	// never null -- and the response always carries the invoices array
	// the schema promises.
	items := make([]api.BillingInvoice, 0, len(rows))
	for i := range rows {
		items = append(items, toInvoiceResponse(&rows[i]))
	}
	writeJSON(w, http.StatusOK, api.BillingListInvoicesResponse{Invoices: items})
}

// BillingGetInvoice implements api.ServerInterface: GET
// /api/v1/billing/invoices/{id}. Answers one of the caller's tenant's
// billing documents in full -- the row InvoiceRepository.FindByID
// returns under the isolation plugin's own tenant filter, so id must
// name an invoice of the caller's own tenant. An id that names no such
// invoice -- never created, or another tenant's -- answers the
// identical billing.invoice_not_found 404, so the route discloses
// nothing about whether the id exists at all (the same answer shape
// go/sharing's own get route documents for its owner metadata).
func (h *Handler) BillingGetInvoice(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := mustTenant(w, r); !ok {
		return
	}

	inv, err := h.invoices.FindByID(r.Context(), id)
	if err != nil {
		if dbkit.IsRecordNotFound(err) {
			writeError(w, ErrInvoiceNotFound.WithParam("id", id))
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toInvoiceResponse(inv))
}

// toInvoiceResponse maps one Invoice row to its wire shape. Amount,
// period, issue and touch times travel verbatim; Status is a cast of
// the module's own closed vocabulary onto the spec's enum of the same
// values (a row can only ever carry open/paid/void -- the lifecycle's
// legal-transition table guarantees it), and Currency the code the
// model stored. The response deliberately carries no tenant_id, the
// same rule every other operation on this surface follows.
func toInvoiceResponse(inv *Invoice) api.BillingInvoice {
	return api.BillingInvoice{
		ID:             inv.ID,
		SubscriptionID: inv.SubscriptionID,
		Status:         api.BillingInvoiceStatus(inv.Status),
		AmountCents:    inv.AmountCents,
		Currency:       inv.Currency,
		PeriodStart:    inv.PeriodStart,
		PeriodEnd:      inv.PeriodEnd,
		CreatedAt:      inv.CreatedAt,
		UpdatedAt:      inv.UpdatedAt,
	}
}

// writeError writes err to w as a JSON {code, params} body, the
// structured-error envelope shape every other module handler in this
// codebase produces (backend coding standard §6.2): a stable code plus
// structured parameters, never localized text -- the client resolves the
// code through its own i18n catalog. An error that is not itself an
// *apperr.Error -- a database failure wrapped in a plain fmt.Errorf, say
// -- folds into ErrInternal, so a caller never sees raw Go error text.
func writeError(w http.ResponseWriter, err error) {
	appErr, ok := apperr.As(err)
	if !ok {
		appErr = ErrInternal
	}
	envelope := api.BillingError{Code: appErr.Code}
	if appErr.Params != nil {
		envelope.Params = &appErr.Params
	}
	writeJSON(w, appErr.Status, envelope)
}

// writeJSON writes body to w as JSON with the surface's content type.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// compile-time check that *Handler implements every operation the spec
// fragment declares -- a spec change whose generated interface outgrew
// this file stops the module from compiling.
var _ api.ServerInterface = (*Handler)(nil)
