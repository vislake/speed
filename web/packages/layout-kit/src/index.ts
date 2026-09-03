/**
 * Public entry of @speed/layout-kit.
 *
 * Shared, auth-agnostic app chrome: AppShell (responsive header + nav
 * drawer + content region) and RouteGuard (gates children on an
 * injected `RouteGuardStatus`, no concrete auth dependency) with their
 * layout-kit-namespace translations (LAYOUT_KIT_NAMESPACE +
 * layoutKitResources) for the host to register. Everything else stays
 * internal: helpers shared between components live in src/internal/ and
 * are deliberately not exported.
 */

export { LAYOUT_KIT_NAMESPACE, layoutKitResources } from './resources.js'
export {
  AppShell,
  type AppShellNavItem,
  type AppShellProps,
} from './components/AppShell.js'
export {
  RouteGuard,
  type RouteGuardProps,
  type RouteGuardStatus,
} from './components/RouteGuard.js'
