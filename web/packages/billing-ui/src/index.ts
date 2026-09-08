/**
 * Public entry of @speed/billing-ui.
 *
 * The billing-documents component family over the generated billing
 * read operations: InvoicesSection renders the caller's tenant's
 * invoices, newest first, through useBillingListInvoices over the
 * host's QueryClient, each row expandable into a document region that
 * re-reads the single invoice through useBillingGetInvoice. Every
 * built-in string renders from the bilingual billing-ui namespace
 * (BILLING_UI_NAMESPACE + billingUiResources, which the host registers
 * alongside ui-kit's -- the surface composes ui-kit's EmptyState, whose
 * own built-in texts speak ui-kit-namespace keys). The section is
 * controlled and takes no props: whose invoices are shown comes from
 * the caller's bound client and its access token, never from a tenant
 * prop -- the billing operations carry no tenant concept. Nothing here
 * reads storage, navigates or touches the network directly.
 */

export { BILLING_UI_NAMESPACE, billingUiResources } from './resources.js'
export { InvoicesSection } from './InvoicesSection.js'
