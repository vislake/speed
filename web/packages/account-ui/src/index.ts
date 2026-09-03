/**
 * Public entry of @speed/account-ui.
 *
 * The signed-in account-management component family over an
 * @speed/auth-core session and the generated authn operations: the
 * session list (with per-session revocation), the login-history surface,
 * the social-binding surface and the step-up-gated two-factor setup --
 * each driving its own slice of the session contract and rendering every
 * built-in string from the bilingual account-ui namespace
 * (ACCOUNT_UI_NAMESPACE + accountUiResources, which the host registers
 * alongside ui-kit's). Components are controlled and their reads go
 * through the react-query hooks of @speed/api-sdk over the host's
 * QueryClient: nothing here reads storage, attaches a session, navigates
 * or touches the network directly. Helpers shared between the components
 * live in src/internal/ and are deliberately not exported.
 *
 * This is the scaffold round of the package: the entry carries no
 * exports yet. The namespace resources land with the first component
 * round, which re-exports them here exactly as auth-ui's entry does.
 */
export {}
