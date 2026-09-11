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
 *   - an import, re-export (export ... from), dynamic import or
 *     require of the axios or node-fetch module -- deliberately
 *     path-free: any source naming the package is this pattern,
 *     however deep its subpath (axios/dist/... is still axios) and
 *     whatever its binding, with template-literal sources
 *     (import(`axios`)) judged like literal ones.
 *
 * All the guarded forms carry one shadowing check: an identifier that
 * resolves to a genuine local or imported binding (a parameter named
 * fetch, a wrapper module, node-fetch itself) is not the environment
 * global, so only an identifier with no real declaration in scope is
 * reported. Member-object forms take the same check on the object
 * name: window.fetch called where a local binds window reads that
 * local's member, not the environment fetch, while an unbound
 * window/globalThis/self member keeps reporting. Only a real binding
 * counts -- declaring the name as an environment global (config
 * globals, a comment-declared global) creates a scope variable with no
 * declaration behind it and never silences the guard.
 *
 * The environment fetch is also flagged when it is captured rather
 * than called: taking `window.fetch` out of a call (an alias
 * `const f = window.fetch`, a `.bind`/`.call` capture) or
 * destructuring it out of the environment object (`const { fetch } =
 * window`) is the same hand-written HTTP waiting to happen, and a
 * rule that answered "no call, no violation" would leave every alias
 * form outside the check. What stays outside is the residual the
 * check deliberately does not chase: aliasing the environment OBJECT
 * itself (`const win = window; win.fetch(...)`) or the bare global
 * identifier (`const f = fetch`) needs dataflow across bindings,
 * which this rule does not do -- code review owns that residue.
 *
 * Scope is applied in eslint.config.mjs over package runtime source,
 * with packages/api-client as the single whitelist; test files and
 * test utilities are excluded by the config, not by the rule -- keep
 * the rule itself free of path assumptions.
 */

const BANNED_MODULES = new Set(['axios', 'node-fetch'])

/** The string an import-source node carries, or undefined. A source is
 * a plain string literal, except for dynamic import and require
 * arguments, which may also be a substitution-free template literal --
 * import(`axios`) names the same module as import('axios'). */
function importSourceValue(source) {
  if (source.type === 'Literal' && typeof source.value === 'string') {
    return source.value
  }
  if (
    source.type === 'TemplateLiteral' &&
    source.expressions.length === 0 &&
    source.quasis.length === 1
  ) {
    const cooked = source.quasis[0].value.cooked
    return typeof cooked === 'string' ? cooked : undefined
  }
  return undefined
}

/** The package name an import source names: its first path segment.
 * An unscoped package is the whole source when the import has no
 * subpath, so axios/dist/node/axios.cjs still names the axios package
 * and node-fetch/lib/index.js still names node-fetch. A relative
 * import or another package can never match: './axios' resolves to
 * '.' and 'axios-mock-adapter' is its own first segment. */
function packageName(source) {
  const slash = source.indexOf('/')
  return slash === -1 ? source : source.slice(0, slash)
}

/** Whether a module source string is an import of a banned package. */
function isBannedModuleSource(module) {
  return BANNED_MODULES.has(packageName(module))
}

/** Report an import/require/export source node that carries a banned
 * module, naming the whole source string as imported. */
function checkBannedSource(context, sourceNode) {
  const module = importSourceValue(sourceNode)
  if (module !== undefined && isBannedModuleSource(module)) {
    context.report({
      node: sourceNode,
      messageId: 'bannedModule',
      data: { module },
    })
  }
}

/** Whether any scope from the node upward binds `name`: shadowed.
 * "Binds" means a variable with a real declaration behind it -- a
 * local variable, parameter, function/class name or import binding.
 * Environment names the host merely *declares* (config globals, a
 * comment-declared global) become scope variables with no defs at all,
 * and declaring one must never silence the guard: the rule exists
 * because the environment fetch is dangerous in package src, not
 * because the host has not declared it. */
function isBound(name, node, sourceCode) {
  for (
    let scope = sourceCode.getScope(node);
    scope !== null && scope !== undefined;
    scope = scope.upper
  ) {
    if (
      scope.variables.some(
        (variable) => variable.name === name && variable.defs.length > 0,
      )
    ) {
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
 * different call shape. A local that genuinely binds the environment
 * object's name (a self parameter, a declared window) makes the member
 * access read that local's own member, so only an unbound object name
 * is the environment -- the same shadowing rule the bare identifier
 * path applies. */
function environmentFetchSource(node, sourceCode) {
  if (
    node.type !== 'MemberExpression' ||
    node.object.type !== 'Identifier' ||
    !ENVIRONMENT_OBJECTS.has(node.object.name) ||
    isBound(node.object.name, node, sourceCode)
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
          node.arguments.length === 1 &&
          node.arguments[0] !== undefined
        ) {
          checkBannedSource(context, node.arguments[0])
          return
        }
        const object = environmentFetchSource(callee, sourceCode)
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
        const object = environmentFetchSource(node, sourceCode)
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
          ENVIRONMENT_OBJECTS.has(node.init.name) &&
          !isBound(node.init.name, node.init, sourceCode)
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
        if (
          !ENVIRONMENT_OBJECTS.has(node.right.name) ||
          isBound(node.right.name, node.right, sourceCode)
        ) {
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
        checkBannedSource(context, node.source)
      },
      // Re-exports carry the same source: export * from 'node-fetch'
      // and export { default as axios } from 'axios' hand the banned
      // module's HTTP surface onward, so the importing package still
      // reaches the network through it. Local exports (export { x })
      // have no source and are untouched.
      ExportNamedDeclaration(node) {
        if (node.source !== null && node.source !== undefined) {
          checkBannedSource(context, node.source)
        }
      },
      ExportAllDeclaration(node) {
        checkBannedSource(context, node.source)
      },
      ImportExpression(node) {
        checkBannedSource(context, node.source)
      },
    }
  },
}
