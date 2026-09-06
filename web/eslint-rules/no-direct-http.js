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
 *     `globalThis.fetch(...)`, `self.fetch(...)`, computed member
 *     access included), and
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
 * The environment fetch is also flagged when it is captured rather
 * than called: taking `window.fetch` out of a call (an alias
 * `const f = window.fetch`, a `.bind`/`.call` capture) or
 * destructuring it out of the environment object (`const { fetch } =
 * window`) is the same hand-written HTTP waiting to happen, and a rule
 * that answered "no call, no violation" left every alias form outside
 * the check (reference-app-web.md P2-6). What stays outside is the
 * residual the check deliberately does not chase: aliasing the
 * environment OBJECT itself (`const win = window; win.fetch(...)`) or
 * the bare global identifier (`const f = fetch`) needs dataflow across
 * bindings, which this rule does not do -- code review owns that
 * residue.
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
const ENVIRONMENT_OBJECTS = new Set(['window', 'globalThis', 'self'])

/** The environment object a member/destructure expression reads fetch
 * from, or undefined when the expression is not one of the environment
 * objects' fetch members. Computed access counts when its key is the
 * literal 'fetch' -- window['fetch'] is the same member, not a
 * different call shape. */
function environmentFetchSource(node) {
  if (
    node.type !== 'MemberExpression' ||
    node.object.type !== 'Identifier' ||
    !ENVIRONMENT_OBJECTS.has(node.object.name)
  ) {
    return undefined
  }
  if (!node.computed) {
    return node.property.type === 'Identifier' &&
      node.property.name === 'fetch'
      ? node.object.name
      : undefined
  }
  return node.property.type === 'Literal' &&
    node.property.value === 'fetch'
    ? node.object.name
    : undefined
}

/** The environment object an ObjectPattern destructures fetch out of,
 * or undefined. */
function environmentFetchPattern(pattern) {
  if (pattern.type !== 'ObjectPattern') {
    return undefined
  }
  const property = pattern.properties.find(
    (candidate) =>
      candidate.type === 'Property' &&
      candidate.key.type === 'Identifier' &&
      candidate.key.name === 'fetch',
  )
  return property === undefined ? undefined : property.key
}

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
      capturedFetch:
        'Capturing {{object}}.fetch (or destructuring fetch out of {{object}}) takes the global fetch outside a call and outside @speed/api-client, the only package allowed to touch the network; route requests through its request function instead.',
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
        const object = environmentFetchSource(callee)
        if (object !== undefined) {
          report(callee, 'memberFetch', { object })
        }
      },
      // A reference to the environment fetch that is not a call: the
      // alias/capture forms (const f = window.fetch, a .bind or .call
      // capture, window['fetch'] taken as a value). The call-callee
      // case above reports first, so this visitor skips callee
      // positions to avoid a double report.
      MemberExpression(node) {
        if (
          node.parent !== null &&
          node.parent.type === 'CallExpression' &&
          node.parent.callee === node
        ) {
          return
        }
        const object = environmentFetchSource(node)
        if (object !== undefined) {
          report(node, 'capturedFetch', { object })
        }
      },
      VariableDeclarator(node) {
        if (node.init === null || node.init === undefined) {
          return
        }
        if (
          node.init.type === 'Identifier' &&
          ENVIRONMENT_OBJECTS.has(node.init.name)
        ) {
          const key = environmentFetchPattern(node.id)
          if (key !== undefined) {
            report(key, 'capturedFetch', { object: node.init.name })
          }
        }
      },
      AssignmentExpression(node) {
        if (node.right.type !== 'Identifier') {
          return
        }
        if (!ENVIRONMENT_OBJECTS.has(node.right.name)) {
          return
        }
        const key = environmentFetchPattern(node.left)
        if (key !== undefined) {
          report(key, 'capturedFetch', { object: node.right.name })
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
