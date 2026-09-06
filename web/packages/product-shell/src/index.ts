/**
 * Public entry of @speed/product-shell.
 *
 * ProductShell, the tenant-facing assembly shell: one three-branch view
 * machine over an @speed/auth-core session (read through useAuthState,
 * never driven here) -- the AppShell frame from @speed/layout-kit around
 * the app children while authenticated, the sessionEnded slot (auth-ui's
 * SessionEndedScreen by default, with its sign-in-again action wired to
 * return to the sign-in branch) after an authenticated session turned
 * anonymous, and the signIn slot for the fresh or unattached visitor.
 * All chrome props pass through to the frame unchanged.
 *
 * The shell renders one string of its own -- the polite announcement of
 * the session-ended flip, which the host's screen readers hear through
 * the shell's role="status" live region -- from the product-shell
 * namespace below, registered by the host alongside every sibling's;
 * the frame's strings live in layout-kit's namespace, the default ended
 * screen's in auth-ui's, and every other string is host content
 * arriving through the slots and props. It performs no session
 * operations, navigates nowhere and makes no requests; the host
 * registers the namespaces its chosen views render under and attaches
 * the session before render.
 */

export { ProductShell, type ProductShellProps } from './components/ProductShell.js'
export {
  PRODUCT_SHELL_NAMESPACE,
  productShellResources,
} from './resources.js'
