/**
 * Public entry of @speed/tenancy-ui.
 *
 * TenantSwitcher, the tenant-switch affordance over an @speed/auth-core
 * session: a trigger showing the current tenant (host-supplied name, or
 * the noCurrentTenant text when the host has no current tenant yet) that
 * opens the host-supplied tenant list; picking a row that is not the
 * current tenant calls session.switchTenant(id), makes the control inert
 * while the switch is in flight behind a role="status" notice (the
 * trigger stays focusable, aria-disabled, so the menu-close focus
 * restore lands on a live control), stays silent on success (the host
 * observes the principal flip through its own auth-core hooks) and
 * fires onSwitched exactly once per committed switch, and renders a
 * reachable error's code text -- never a raw key -- on failure, leaving
 * the state unchanged and the row retryable. A switch that loses a
 * concurrent race on the same session (a second TenantSwitcher
 * instance, or another session operation committing first) is
 * superseded and stays lost: nothing renders, nothing is re-issued, no
 * onSwitched fires -- the winning operation's own commit already fired
 * its own, and the trigger converges to the tenant the session runs
 * under through the host's currentTenantId (see the component header
 * for the residual the lost-race rule records). The current-tenant row
 * is disabled and never re-triggers a switch.
 *
 * The component is controlled: the session arrives as a prop, the tenant
 * list is host data, and the current tenant id is a host value. Nothing
 * here consumes the auth-core hooks, attaches or reads session state,
 * navigates, or touches the network directly -- every request is a
 * session operation through the seam the host bound. Hosts decide what a
 * completed switch means for their own state: permission lists
 * (auth-core drops the tenant-domain list on switch; the host re-attaches
 * both lists from /me), query caches (tenant-namespaced keys must be
 * dropped for the previous tenant) and navigation are all host
 * responsibilities after onSwitched fires. Helpers shared between the
 * component and its suite live in src/internal/ and are deliberately not
 * exported.
 */

export {
  TENANCY_UI_NAMESPACE,
  tenancyUiResources,
} from './resources.js'
export { TenantSwitcher, type TenantSwitcherProps } from './TenantSwitcher.js'
export type { TenantOption } from './TenantSwitcher.js'
