/**
 * Public entry of @speed/auth-ui.
 *
 * The sign-in component family over an @speed/auth-core session: the
 * password, SMS-code and registration channels, each driving its own
 * slice of the session contract and rendering every built-in string from
 * the bilingual auth-ui namespace (AUTH_UI_NAMESPACE + authUiResources,
 * which the host registers alongside ui-kit's). Components are controlled:
 * the session comes in as a prop, a successful sign-in fires the
 * onSignedIn callback and the host navigates -- nothing here reads
 * storage, attaches a session or touches the network directly. Helpers
 * shared between the components live in src/internal/ and are
 * deliberately not exported.
 */

export { AUTH_UI_NAMESPACE, authUiResources } from './resources.js'
export {
  PasswordSignInForm,
  type PasswordSignInFormProps,
} from './PasswordSignInForm.js'
export {
  SMSSignInForm,
  type SMSSignInFormProps,
} from './SMSSignInForm.js'
export { RegisterForm, type RegisterFormProps } from './RegisterForm.js'
