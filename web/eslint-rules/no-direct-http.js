/**
 * no-direct-http: HTTP requests happen only inside @speed/api-client.
 *
 * This is the enforcement half of the API-contract rule -- the
 * frontend may only reach the backend through the api-client request
 * function (createClient), which wires the injectable fetch, the
 * timeout, the retry policy and the silent 401 refresh in one place
 * (CLAUDE.md's API contract section, docs/internal/21-api-contract.md).
 * A fetch, axios or XHR call anywhere else is hand-written API calling
 * that skips that layer. The rule catches the direct forms:
 *
 *   - a call to the bare global fetch identifier (`fetch('/api')`),
 *   - a call through the environment object (`window.fetch(...)`,
 *     `globalThis.fetch(...)`), and
 *   - a constructed XMLHttpRequest (`new XMLHttpRequest()`),
 *   - an import, dynamic import or require of the axios or node-fetch
 *     module -- deliberately path-free: any import of axios is this
 *     pattern, whatever its subpath or binding.
 *
 * The bare identifier forms carry a shadowing check: an identifier
 * that resolves to a local or imported binding (a parameter named
 * fetch, a wrapper module, node-fetch itself) is not the environment
 * global, so only an identifier with no binding in scope is reported.
 * Member-object forms report unconditionally -- a local binding cannot
 * shadow the environment object.
 *
 * Scope is applied in eslint.config.mjs over package runtime source,
 * with packages/api-client as the single whitelist; test files and
 * test utilities are excluded by the config, not by the rule -- keep
 * the rule itself free of path assumptions.
 */

const BANNED_MODULES = new Set(['axios', 'node-fetch'])

/** Whether any scope from the node upward binds `name`: shadowed. */
function isBound(name, node, sourceCode) {
  for (
    let scope = sourceCode.getScope(node);
    scope !== null && scope !== undefined;
    scope = scope.upper
  ) {
    if (scope.variables.some((variable) => variable.name === name)) {
      return true
    }
  }
  return false
}

/** The environment objects whose fetch member is the global fetch. */
const ENVIRONMENT_OBJECTS = new Set(['window', 'globalThis'])

export const noDirectHttpRule = {
  meta: {
    type: 'problem',
    docs: {
      description:
        'Disallow direct HTTP calls outside @speed/api-client; every request must go through the api-client request function.',
    },
    messages: {
      globalFetch:
        'A direct fetch() call is hand-written HTTP; route the request through the @speed/api-client request function (createClient) instead.',
      memberFetch:
        '{{object}}.fetch() bypasses the @speed/api-client request layer; route the request through the client instance instead.',
      xmlHttpRequest:
        'XMLHttpRequest bypasses the @speed/api-client request layer; route the request through the client instance instead.',
      bannedModule:
        'Importing {{module}} writes HTTP outside @speed/api-client, the only package allowed to touch the network; route the request through its request function instead.',
    },
    schema: [],
  },
  create(context) {
    const sourceCode = context.sourceCode

    function report(node, messageId, data) {
      context.report({ node, messageId, data })
    }

    return {
      CallExpression(node) {
        const callee = node.callee
        if (
          callee.type === 'Identifier' &&
          callee.name === 'fetch' &&
          !isBound('fetch', callee, sourceCode)
        ) {
          report(callee, 'globalFetch')
          return
        }
        if (
          callee.type === 'Identifier' &&
          callee.name === 'require' &&
          node.arguments.length === 1
        ) {
          const argument = node.arguments[0]
          if (
            argument !== undefined &&
            argument.type === 'Literal' &&
            typeof argument.value === 'string' &&
            BANNED_MODULES.has(argument.value)
          ) {
            report(argument, 'bannedModule', { module: argument.value })
          }
          return
        }
        if (
          callee.type === 'MemberExpression' &&
          !callee.computed &&
          callee.property.type === 'Identifier' &&
          callee.property.name === 'fetch' &&
          callee.object.type === 'Identifier' &&
          ENVIRONMENT_OBJECTS.has(callee.object.name)
        ) {
          report(callee, 'memberFetch', { object: callee.object.name })
        }
      },
      NewExpression(node) {
        const callee = node.callee
        if (
          callee.type === 'Identifier' &&
          callee.name === 'XMLHttpRequest' &&
          !isBound('XMLHttpRequest', callee, sourceCode)
        ) {
          report(callee, 'xmlHttpRequest')
        }
      },
      ImportDeclaration(node) {
        const module = node.source.value
        if (typeof module === 'string' && BANNED_MODULES.has(module)) {
          report(node.source, 'bannedModule', { module })
        }
      },
      ImportExpression(node) {
        const source = node.source
        if (
          source.type === 'Literal' &&
          typeof source.value === 'string' &&
          BANNED_MODULES.has(source.value)
        ) {
          report(source, 'bannedModule', { module: source.value })
        }
      },
    }
  },
}
