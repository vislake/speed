/**
 * Public entry of @speed/account-ui.
 *
 * The signed-in account-management component family over an
 * @speed/auth-core session and the generated authn operations: the
 * session list (with per-session and bulk revocation), the login-history
 * surface, the social-binding surface and the step-up-gated two-factor
 * setup -- each rendering every built-in string from the bilingual
 * account-ui namespace (ACCOUNT_UI_NAMESPACE + accountUiResources,
 * which the host registers alongside ui-kit's). Components are
 * controlled and their reads go through the react-query hooks of
 * @speed/api-sdk over the host's QueryClient: nothing here reads
 * storage, attaches a session, navigates or touches the network
 * directly. Helpers shared between the components live in src/internal/
 * and are deliberately not exported.
 *
 * The session is a required prop exactly where a session operation
 * exists that the generated surface cannot express: the bindings add
 * area's authorize-URL request (session.socialAuthorizeUrl) and the
 * two-factor challenge dialog's verification (session.verifyStepUp).
 * The provider vocabulary (SocialProvider/SocialProviderConfig) is this
 * package's own definition, copied to match @speed/auth-ui's and kept
 * in sync with it -- same-layer packages never import each other, and
 * the authn spec is the shared source of truth for the provider set.
 */

export { ACCOUNT_UI_NAMESPACE, accountUiResources } from './resources.js'
export { SessionsSection } from './SessionsSection.js'
export { LoginHistorySection } from './LoginHistorySection.js'
export {
  SocialBindingsSection,
  type SocialBindingsSectionProps,
  type SocialProvider,
  type SocialProviderConfig,
} from './SocialBindingsSection.js'
export {
  BindingCallbackHandler,
  type BindingCallbackHandlerProps,
} from './BindingCallbackHandler.js'
export { MfaSection, type MfaSectionProps } from './MfaSection.js'
