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
        // aria-hidden trees are presentational glyphs, exempt by design.
        '<span aria-hidden="true">·</span>',
        '<span aria-hidden>{`·`}</span>',
        '<span aria-hidden={true}>{`·`}</span>',
        '<div aria-hidden={"true"}><span>decorative</span></div>',
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
          errors: [{ messageId: 'literalAttribute', line: 1, column: 9 }],
        },
      ],
    )
  })
})
