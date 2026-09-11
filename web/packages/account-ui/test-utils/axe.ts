/**
 * account-ui's scan tier: the widget-tier axe assertion (see
 * @speed/test-utils/axe's header) -- the account surfaces are components
 * rendered in isolation, so `region` and landmark-one-main stay
 * disabled and page-has-heading-one asks its question against the page
 * heading the render harness supplies.
 */
export { expectNoAxeViolations } from '@speed/test-utils/axe'
