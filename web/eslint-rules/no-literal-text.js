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
 * {`Save`} forms), while computed expressions (`{t('a.b')}`,
 * `{row.name}`) are the sanctioned path and untouched. Decorative
 * glyphs that are invisible to assistive technology (any text under an
 * aria-hidden element) are exempt -- a middot separator or a bullet is
 * presentational, not user-facing. Attribute names outside the
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
 * Whether the node sits inside a JSX element whose aria-hidden is
 * true. Content hidden from assistive technology is presentational --
 * decorative glyphs are exempt from the i18n rule. Recognized forms:
 * aria-hidden (no value), aria-hidden="true", aria-hidden={'true'},
 * aria-hidden={true}.
 */
function isUnderAriaHidden(node) {
  for (
    let current = node.parent;
    current !== undefined && current !== null;
    current = current.parent
  ) {
    if (current.type !== 'JSXElement') {
      continue
    }
    const hidden = current.openingElement.attributes.some((attribute) => {
      if (
        attribute.type !== 'JSXAttribute' ||
        attribute.name.name !== 'aria-hidden'
      ) {
        return false
      }
      const value = attribute.value
      if (value === null) {
        // A value-less aria-hidden compiles to aria-hidden={true} in
        // React's JSX transform.
        return true
      }
      if (value.type === 'JSXExpressionContainer') {
        const expression = value.expression
        // `true` inside an expression parses as a boolean Literal in
        // espree and in the typescript-eslint parser alike.
        return (
          (expression.type === 'Literal' && expression.value === true) ||
          (expression.type === 'Identifier' && expression.name === 'true') ||
          literalText(expression) === 'true'
        )
      }
      return literalText(value) === 'true'
    })
    if (hidden) {
      return true
    }
  }
  return false
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
        if (text !== '' && !isUnderAriaHidden(node)) {
          context.report({
            node,
            messageId: 'literalText',
            data: { text: text.slice(0, 40) },
          })
        }
      },
      'JSXExpressionContainer > Literal, JSXExpressionContainer > TemplateLiteral'(
        node,
      ) {
        // Attribute values ride the same JSXExpressionContainer AST shape;
        // those are reported by the JSXAttribute visitor below with a
        // more specific message, so do not double-report here.
        if (
          node.parent !== null &&
          node.parent.type === 'JSXExpressionContainer' &&
          node.parent.parent !== null &&
          node.parent.parent.type === 'JSXAttribute'
        ) {
          return
        }
        if (isUnderAriaHidden(node)) {
          return
        }
        const text = literalText(node)
        if (text !== undefined) {
          context.report({
            node,
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
        const text = literalText(candidate)
        if (text !== undefined) {
          context.report({
            node,
            messageId: 'literalAttribute',
            data: { name: node.name.name, text: text.slice(0, 40) },
          })
        }
      },
    }
  },
}
