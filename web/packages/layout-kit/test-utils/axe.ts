/**
 * layout-kit's scan tier: the page-tier axe assertion (see
 * @speed/test-utils/axe's header) -- AppShell is page-level chrome with
 * repeated landmarks (header, nav, main), so `region` and
 * landmark-one-main stay ENABLED: the frame renders a real `main`, so
 * the scans can and must answer them. page-has-heading-one is live too:
 * a page-shaped scan's document IS a page, the chrome renders no page
 * heading of its own, so the test must supply the host content (inside
 * `main`) that does.
 */
export {
  expectNoAxeViolationsForPage as expectNoAxeViolations,
} from '@speed/test-utils/axe'
