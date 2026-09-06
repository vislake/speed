/**
 * no-literal-text: user-facing text must come from the i18n namespace,
 * never be written inline in a package's own JSX.
 *
 * This is the enforcement half of the frontend i18n rule (see
 * .claude/skills/frontend-coding-standards/SKILL.md §6): a package that
 * ships UI ships every visible string through t(), in both zh-CN and
 * en-US. The rule catches the two direct ways a literal reaches the
 * user:
 *
 *   - bare JSX text nodes (`<Button>Save</Button>`), and
 *   - text-bearing attribute values -- aria-label, aria-placeholder,
 *     aria-roledescription, placeholder, title, alt -- written as a
 *     string literal (`aria-label="Save"`, `placeholder={'Save'}`,
 *     `placeholder={`Save`}`).
 *
 * Scope is deliberately narrow and documented: JSX children that are
 * plain string/template literals are also flagged (the `{'Save'}` /
 * {`Save`} forms), as are the plain-string branches of conditional
 * expressions (`{ok ? 'Save' : 'Wait'}` renders user-facing text
 * exactly as the bare form does; each offending branch reports its own
 * literal, while branches through t() or a dynamic template stay the
 * sanctioned path) (reference-app-web.md P2-7). Computed expressions
 * (`{t('a.b')}`, `{row.name}`) are the sanctioned path and untouched.
 *
 * No aria-hidden exemption exists: aria-hidden removes content from
 * the accessibility tree only -- a sighted user still reads text that
 * is merely aria-hidden, so it is user-facing text like any other (and
 * text that matters to sighted users should not be hidden from
 * assistive technology in the first place). Decorative glyphs belong
 * in icons or CSS content, not in exempt text nodes
 * (reference-app-web.md P2-7). Attribute names outside the
 * text-bearing set are not inspected, so non-visible props like
 * `type="email"` stay legal.
 *
 * Hosts own their own literals: the rule runs over package src only
 * (wired in eslint.config.mjs), and test fixtures are excluded there --
 * test strings are data, not rendered product text. Test files and
 * helpers are exempted by the config, not by the rule; keep the rule
 * itself free of path assumptions.
 */

const TEXT_BEARING_ATTRIBUTES = new Set([
  'aria-label',
  'aria-placeholder',
  'aria-roledescription',
  'placeholder',
  'title',
  'alt',
])

/** A plain string or a substitution-free template literal. */
function isPlainStringLiteral(node) {
  if (node.type === 'Literal' && typeof node.value === 'string') {
    return node
  }
  if (
    node.type === 'TemplateLiteral' &&
    node.expressions.length === 0 &&
    node.quasis.length === 1
  ) {
    return node
  }
  return undefined
}

/** Literal text of a plain string/template literal, or undefined. */
function literalText(node) {
  const literal = isPlainStringLiteral(node)
  if (literal === undefined) {
    return undefined
  }
  const text =
    literal.type === 'Literal'
      ? literal.value
      : literal.quasis[0].value.cooked
  return text !== undefined && text.trim() !== '' ? text : undefined
}

/**
 * Every plain-string literal an expression can render as text, with
 * its text. A conditional expression contributes the plain literals of
 * its branches (a ternary's branch is exactly what renders when the
 * condition picks it; a nested ternary walks recursively), and a plain
 * literal or template literal contributes itself. Anything else -- a
 * t() call, an identifier, a dynamic template -- contributes nothing.
 */
function plainTextLiterals(node) {
  const found = []
  function walk(current) {
    if (current.type === 'ConditionalExpression') {
      walk(current.consequent)
      walk(current.alternate)
      return
    }
    const text = literalText(current)
    if (text !== undefined) {
      found.push({ node: current, text })
    }
  }
  walk(node)
  return found
}

export const noLiteralTextRule = {
  meta: {
    type: 'suggestion',
    docs: {
      description:
        'Disallow user-facing text written inline in JSX; every visible string must come from the i18n namespace.',
    },
    messages: {
      literalText:
        'User-facing text must come from the i18n namespace, not be written inline. Render "{{text}}" through a t() call (see the i18n section of the frontend coding standards).',
      literalAttribute:
        'The {{name}} value is user-facing text; pass it through a t() call instead of the literal "{{text}}" (see the i18n section of the frontend coding standards).',
    },
    schema: [],
  },
  create(context) {
    return {
      JSXText(node) {
        const text = node.value.trim()
        if (text !== '') {
          context.report({
            node,
            messageId: 'literalText',
            data: { text: text.slice(0, 40) },
          })
        }
      },
      JSXExpressionContainer(node) {
        // Attribute values ride the same JSXExpressionContainer AST
        // shape; those are reported by the JSXAttribute visitor below
        // with a more specific message, so do not double-report here.
        if (node.parent !== null && node.parent.type === 'JSXAttribute') {
          return
        }
        for (const { node: literal, text } of plainTextLiterals(
          node.expression,
        )) {
          context.report({
            node: literal,
            messageId: 'literalText',
            data: { text: text.slice(0, 40) },
          })
        }
      },
      JSXAttribute(node) {
        if (!TEXT_BEARING_ATTRIBUTES.has(node.name.name)) {
          return
        }
        const value = node.value
        if (value === null || value.type === 'JSXElement') {
          return
        }
        const candidate =
          value.type === 'JSXExpressionContainer' ? value.expression : value
        for (const { node: literal, text } of plainTextLiterals(candidate)) {
          context.report({
            node: literal,
            messageId: 'literalAttribute',
            data: { name: node.name.name, text: text.slice(0, 40) },
          })
        }
      },
    }
  },
}
