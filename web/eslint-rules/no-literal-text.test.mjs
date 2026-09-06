/**
 * Rule tests for no-literal-text. Run from the web workspace root:
 *
 *   pnpm exec vitest run eslint-rules/no-literal-text.test.mjs
 *
 * The vitest CLI filter keeps this single-file run inside the plain
 * node environment; the packages' own tests (which need their jsdom
 * configs) are not picked up.
 *
 * Test snippets are intentionally English -- they are fixture code for
 * the rule under test, not rendered UI text, so the CJK scan does not
 * apply to them.
 */

import { RuleTester } from 'eslint'
import { describe, expect, it } from 'vitest'
import { noLiteralTextRule } from './no-literal-text.js'

const ruleTester = new RuleTester({
  languageOptions: {
    parserOptions: {
      ecmaFeatures: { jsx: true },
      sourceType: 'module',
    },
  },
})

/**
 * RuleTester.run throws an aggregate error listing every failing case;
 * surface that error as the test failure instead of swallowing it.
 */
function runRule(valid, invalid) {
  try {
    ruleTester.run('no-literal-text', noLiteralTextRule, { valid, invalid })
  } catch (error) {
    expect.fail(`rule tests failed: ${error.message}`)
  }
}

describe('no-literal-text rule', () => {
  it('accepts text routed through t() and computed expressions', () => {
    runRule(
      [
        // The sanctioned shapes: children and attributes through t().
        "<Button>{t('empty.title')}</Button>",
        "<Button aria-label={t('empty.title')}>{t('empty.title')}</Button>",
        // Computed expressions are data, not literals.
        '<Button>{row.name}</Button>',
        '<img alt={row.altText} />',
        // Substitution templates are dynamic by construction.
        "<Button aria-label={`row ${row.id}`} />",
        // Whitespace-only text nodes are layout, not content.
        '<div> </div>',
        '<div>\n  \n</div>',
        // Attributes outside the text-bearing set are not content.
        '<input type="email" value={query} />',
        // Conditional branches through t() are the sanctioned shape;
        // only their literal branches are the violation.
        "<Button>{ok ? t('save.title') : t('wait.title')}</Button>",
        '<Button aria-label={ok ? t(\'a.b\') : `row ${row.id}`} />',
        // Empty attribute literals carry no text.
        '<div title="" />',
        '<div placeholder="   " />',
      ],
      [],
    )
  })

  it('flags bare JSX text nodes', () => {
    runRule(
      [],
      [
        { code: '<Button>Save</Button>', errors: [{ messageId: 'literalText' }] },
        {
          code: '<Button>Save your work before leaving</Button>',
          errors: [{ messageId: 'literalText' }],
        },
        { code: '<p>Hello world</p>', errors: [{ messageId: 'literalText' }] },
        // Mixed text and elements carries a bare text node per fragment.
        {
          code: '<Button>Save <b>now</b></Button>',
          errors: [{ messageId: 'literalText' }, { messageId: 'literalText' }],
        },
      ],
    )
  })

  it('flags string-literal children, including template forms', () => {
    runRule(
      [],
      [
        {
          code: "<Button>{'Save'}</Button>",
          errors: [{ messageId: 'literalText' }],
        },
        {
          code: '<Button>{`Save`}</Button>',
          errors: [{ messageId: 'literalText' }],
        },
      ],
    )
  })

  it('flags text under aria-hidden: aria-hidden hides from assistive technology only, not from sight', () => {
    // The former exemption treated any aria-hidden subtree as
    // presentational. aria-hidden removes content from the accessibility
    // tree; a sighted user still reads text that is merely aria-hidden
    // (and a text that matters to sighted users should not be hidden
    // from assistive technology in the first place), so no exemption is
    // defensible on aria-hidden alone (reference-app-web.md P2-7).
    // Decorative glyphs belong in icons or CSS, not as exempt text.
    runRule(
      [],
      [
        {
          code: '<span aria-hidden="true">·</span>',
          errors: [{ messageId: 'literalText' }],
        },
        {
          code: '<span aria-hidden>{`·`}</span>',
          errors: [{ messageId: 'literalText' }],
        },
        {
          code: '<span aria-hidden={true}>{`·`}</span>',
          errors: [{ messageId: 'literalText' }],
        },
        {
          code: '<div aria-hidden={"true"}><span>decorative</span></div>',
          errors: [{ messageId: 'literalText' }],
        },
        {
          code: '<div aria-hidden={true}><span>Status text</span></div>',
          errors: [{ messageId: 'literalText' }],
        },
      ],
    )
  })

  it('flags plain-string branches of conditional expressions in children and attributes', () => {
    // A ternary is a computed expression, but a plain-string branch is
    // still a literal reaching the user (reference-app-web.md P2-7):
    // {ok ? 'Save' : 'Wait'} renders user-facing text exactly as a bare
    // <Button>Save</Button> does. Each offending branch reports its own
    // literal; branches through t() or dynamic templates stay the
    // sanctioned path.
    runRule(
      [],
      [
        {
          code: "<Button>{ok ? 'Save' : 'Wait'}</Button>",
          errors: [
            { messageId: 'literalText' },
            { messageId: 'literalText' },
          ],
        },
        {
          code: "<Button>{ok ? 'Save' : t('wait.title')}</Button>",
          errors: [{ messageId: 'literalText' }],
        },
        {
          code: "<Button>{ok ? t('save.title') : 'Wait'}</Button>",
          errors: [{ messageId: 'literalText' }],
        },
        {
          code: "<Button>{ok ? (nope ? 'X' : 'Y') : 'Z'}</Button>",
          errors: [
            { messageId: 'literalText' },
            { messageId: 'literalText' },
            { messageId: 'literalText' },
          ],
        },
        {
          code: "<Button>{ok ? `still plain` : t('wait.title')}</Button>",
          errors: [{ messageId: 'literalText' }],
        },
        {
          code: '<Button aria-label={ok ? \'Save\' : \'Wait\'} />',
          errors: [
            { messageId: 'literalAttribute' },
            { messageId: 'literalAttribute' },
          ],
        },
        {
          code: "<img alt={ok ? 'logo' : t('a.placeholder')} />",
          errors: [{ messageId: 'literalAttribute' }],
        },
      ],
    )
  })

  it('flags literal text-bearing attribute values', () => {
    runRule(
      [],
      [
        {
          code: '<Button aria-label="Save" />',
          errors: [{ messageId: 'literalAttribute' }],
        },
        {
          code: "<Button placeholder={'Save'} />",
          errors: [{ messageId: 'literalAttribute' }],
        },
        {
          code: '<Button title={`Save`} />',
          errors: [{ messageId: 'literalAttribute' }],
        },
        {
          code: '<img alt="logo" />',
          errors: [{ messageId: 'literalAttribute' }],
        },
        {
          code: '<span aria-roledescription="slide" />',
          errors: [{ messageId: 'literalAttribute' }],
        },
      ],
    )
  })

  it('reports the offending text inside the message', () => {
    runRule(
      [],
      [
        {
          code: '<Button>Delete forever</Button>',
          errors: [
            {
              // RuleTester forbids pairing messageId with message; the
              // full text pins both id and interpolation in one assert.
              message:
                'User-facing text must come from the i18n namespace, not be written inline. Render "Delete forever" through a t() call (see the i18n section of the frontend coding standards).',
            },
          ],
        },
        {
          code: '<Button aria-label="Save" />',
          errors: [
            {
              message:
                'The aria-label value is user-facing text; pass it through a t() call instead of the literal "Save" (see the i18n section of the frontend coding standards).',
            },
          ],
        },
      ],
    )
  })

  it('reports a location-aware message for each violation site', () => {
    runRule(
      [],
      [
        {
          code: '<Button>Save</Button>',
          errors: [{ messageId: 'literalText', line: 1, column: 9 }],
        },
        {
          code: '<Button title="Save" />',
          // The report lands on the offending literal itself (the value
          // node, quote included), not on the whole attribute.
          errors: [{ messageId: 'literalAttribute', line: 1, column: 15 }],
        },
      ],
    )
  })
})
