/**
 * ui-kit's scan tier: the widget-tier axe assertion plus the
 * single-rule heading-order probe (see @speed/test-utils/axe's header)
 * -- the core components are widgets rendered in isolation, so `region`
 * and landmark-one-main stay disabled, page-has-heading-one asks its
 * question against the page heading the render harness supplies, and
 * the level-dependent headings (EmptyState, the DataTable empty
 * placeholder) are probed through runHeadingOrderCheck instead.
 */
export {
  expectNoAxeViolations,
  runHeadingOrderCheck,
} from '@speed/test-utils/axe'
