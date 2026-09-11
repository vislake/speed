/**
 * Rule tests for no-direct-http. Run from the web workspace root:
 *
 *   pnpm exec vitest run eslint-rules/no-direct-http.test.mjs
 *
 * The vitest CLI filter keeps this single-file run inside the plain
 * node environment; the packages' own tests (which need their jsdom
 * configs) are not picked up.
 *
 * Test snippets are fixture code for the rule under test, not rendered
 * product text, so the CJK scan does not apply to them.
 */

import { RuleTester } from 'eslint'
import { describe, expect, it } from 'vitest'
import { noDirectHttpRule } from './no-direct-http.js'

const ruleTester = new RuleTester({
  languageOptions: {
    parserOptions: {
      ecmaVersion: 2022,
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
    ruleTester.run('no-direct-http', noDirectHttpRule, { valid, invalid })
  } catch (error) {
    expect.fail(`rule tests failed: ${error.message}`)
  }
}

describe('no-direct-http rule', () => {
  it('accepts calls that are not the global fetch', () => {
    runRule(
      [
        // Calls through a client instance are the sanctioned shape.
        "const data = await api('/notes');",
        "const data = await client.get('/notes');",
        "const data = await sdk.notes.list({ page: 1 });",
        // Identifiers other than fetch are not HTTP entry points.
        "fetchData('/api/notes');",
        "const response = myFetch('/api');",
        "window.postMessage({ kind: 'reload' }, '*');",
        // The fetch member of an arbitrary object is not the global.
        "store.fetch('/api');",
        "const response = await fetcher.fetch(url);",
        // require of modules other than the banned ones is untouched.
        "const { readFile } = require('fs');",
        "const format = require('./format.js');",
        // Importing the api-client itself is the sanctioned path.
        "import { createClient } from '@speed/api-client';",
        // Dynamic imports of anything else are untouched.
        "const mod = await import('./feature.js');",
      ],
      [],
    )
  })

  it('accepts a fetch identifier that resolves to a binding', () => {
    runRule(
      [
        // A parameter shadows the global.
        'function getData(fetch) { return fetch("/api"); }',
        // A local const shadows it, wherever it is declared.
        'const fetch = (url) => Promise.resolve(url); fetch("/api");',
        // An imported binding shadows it -- the import itself is only
        // banned for the axios/node-fetch modules.
        "import { fetch } from './local-http.js'; fetch('/api');",
        // The binding may live in an enclosing scope.
        'function outer() { const fetch = () => {}; return function inner() { return fetch("/api"); }; }',
        // A hoisted declaration still resolves.
        'fetch("/api"); function fetch() {}',
        // A local class shadows the XHR constructor.
        'class XMLHttpRequest {} new XMLHttpRequest();',
      ],
      [],
    )
  })

  it('flags bare global fetch calls', () => {
    runRule(
      [],
      [
        { code: 'fetch("/api/notes");', errors: [{ messageId: 'globalFetch' }] },
        {
          code: 'const data = await fetch("/api/notes", { method: "POST" });',
          errors: [{ messageId: 'globalFetch' }],
        },
        {
          code: 'async function load() { return fetch(url); }',
          errors: [{ messageId: 'globalFetch' }],
        },
      ],
    )
  })

  it('flags the environment fetch even when the host merely declares it a global', () => {
    // A scope variable declared by a comment directive or by config
    // globals has no defs: it is the host announcing an environment
    // name, not code binding it. Declaring fetch/XMLHttpRequest as a
    // global must never silence the guard -- the rule is armed precisely
    // because the environment fetch is reachable in package src.
    runRule(
      [],
      [
        {
          code: '/* global fetch */\nfetch("/api/notes");',
          errors: [{ messageId: 'globalFetch' }],
        },
        {
          code: '/* global fetch, XMLHttpRequest */\nnew XMLHttpRequest();',
          errors: [{ messageId: 'xmlHttpRequest' }],
        },
      ],
    )
    // Config globals take the same def-less shape as directive globals.
    const configuredTester = new RuleTester({
      languageOptions: {
        parserOptions: { ecmaVersion: 2022, sourceType: 'module' },
        globals: { fetch: 'readonly', XMLHttpRequest: 'readonly' },
      },
    })
    try {
      configuredTester.run('no-direct-http', noDirectHttpRule, {
        valid: [],
        invalid: [
          { code: 'fetch("/api");', errors: [{ messageId: 'globalFetch' }] },
          {
            code: 'window.fetch("/api");',
            errors: [{ messageId: 'memberFetch' }],
          },
          {
            code: 'new XMLHttpRequest();',
            errors: [{ messageId: 'xmlHttpRequest' }],
          },
        ],
      })
    } catch (error) {
      expect.fail(`rule tests failed: ${error.message}`)
    }
  })

  it('flags fetch through the environment object', () => {
    runRule(
      [],
      [
        {
          code: 'window.fetch("/api/notes");',
          errors: [{ messageId: 'memberFetch' }],
        },
        {
          code: 'const data = await globalThis.fetch("/api");',
          errors: [{ messageId: 'memberFetch' }],
        },
        {
          code: 'globalThis.fetch("/api"); window.fetch("/api");',
          errors: [
            { messageId: 'memberFetch' },
            { messageId: 'memberFetch' },
          ],
        },
        // Computed member access is the same environment fetch, not a
        // different call shape.
        {
          code: 'window["fetch"]("/api");',
          errors: [{ messageId: 'memberFetch' }],
        },
        {
          code: 'const data = await self.fetch("/api");',
          errors: [{ messageId: 'memberFetch' }],
        },
        {
          code: 'globalThis["fetch"]("/api"); self["fetch"]("/api");',
          errors: [
            { messageId: 'memberFetch' },
            { messageId: 'memberFetch' },
          ],
        },
      ],
    )
  })

  it('honours shadowing of the environment object name in member forms', () => {
    // A local that binds the environment name calls that local's own
    // member -- self.fetch inside a self parameter, a locally declared
    // window -- not the environment fetch. The member object identifier
    // is checked the same way the bare identifier is (only a real
    // binding counts; declaring the name an environment global does
    // not).
    runRule(
      [
        'function f(self) { return self.fetch("/api"); }',
        'const window = { fetch: (url) => url }; window.fetch("/api");',
        'function g(globalThis) { return globalThis.fetch("/api"); }',
        'function m(window) { const f = window.fetch.bind(window); return f; }',
        // Destructuring fetch out of a locally bound object of the same
        // name is a local member read, not the environment capture.
        'function h(window) { const { fetch } = window; }',
        'function k(self) { ({ fetch } = self); }',
      ],
      // The genuine environment member calls keep reporting.
      [
        {
          code: 'window.fetch("/api/notes");',
          errors: [{ messageId: 'memberFetch' }],
        },
        {
          code: 'self.fetch("/api/notes");',
          errors: [{ messageId: 'memberFetch' }],
        },
        {
          code: 'const f = window.fetch;',
          errors: [{ messageId: 'capturedFetch' }],
        },
      ],
    )
  })

  it('flags the environment fetch captured out of a call: aliasing, references and destructuring', () => {
    // A reference to window.fetch/globalThis.fetch/self.fetch that is
    // NOT itself a call is still the global fetch leaving the api-client
    // layer -- it exists to be called somewhere, and answering "no
    // call, no violation" would leave every alias form (const f =
    // window.fetch; f(url)) outside the rule. Destructuring fetch out
    // of the environment object is the same capture with a different
    // syntax.
    runRule(
      [],
      [
        {
          code: 'const f = window.fetch;',
          errors: [{ messageId: 'capturedFetch' }],
        },
        {
          code: 'const f = globalThis["fetch"];',
          errors: [{ messageId: 'capturedFetch' }],
        },
        {
          code: 'const f = window.fetch.bind(window);',
          errors: [{ messageId: 'capturedFetch' }],
        },
        {
          code: 'self.fetch.call(undefined, "/api");',
          errors: [{ messageId: 'capturedFetch' }],
        },
        {
          code: 'const { fetch } = window; fetch("/api");',
          errors: [{ messageId: 'capturedFetch' }],
        },
        {
          code: 'const { fetch: g } = globalThis;',
          errors: [{ messageId: 'capturedFetch' }],
        },
        {
          code: 'const { fetch = fallback } = self;',
          errors: [{ messageId: 'capturedFetch' }],
        },
      ],
    )
  })

  it('flags constructed XMLHttpRequest', () => {
    runRule(
      [],
      [
        {
          code: 'new XMLHttpRequest();',
          errors: [{ messageId: 'xmlHttpRequest' }],
        },
        {
          code: 'const xhr = new XMLHttpRequest(); xhr.open("GET", "/api");',
          errors: [{ messageId: 'xmlHttpRequest' }],
        },
      ],
    )
  })

  it('flags axios and node-fetch imports, requires and dynamic imports', () => {
    runRule(
      [],
      [
        {
          code: "import axios from 'axios';",
          errors: [{ messageId: 'bannedModule' }],
        },
        {
          code: "import { post } from 'axios';",
          errors: [{ messageId: 'bannedModule' }],
        },
        {
          code: "import fetch from 'node-fetch';",
          errors: [{ messageId: 'bannedModule' }],
        },
        {
          code: "const axios = require('axios');",
          errors: [{ messageId: 'bannedModule' }],
        },
        {
          code: "const { default: fetch } = require('node-fetch');",
          errors: [{ messageId: 'bannedModule' }],
        },
        {
          code: "const mod = await import('axios');",
          errors: [{ messageId: 'bannedModule' }],
        },
      ],
    )
  })

  it('flags banned modules through re-exports, subpaths and template sources', () => {
    runRule(
      [
        // Local re-exports and other packages are untouched.
        "export { format } from './format.js';",
        "export * from './local-http.js';",
        "export * as fetch from './local-http.js';",
        // A differently named package is not the banned module.
        "import adapter from 'axios-mock-adapter';",
        "import fromAxios from './axios.js';",
        "const mod = await import('./feature.js');",
        "const { readFile } = require('fs');",
      ],
      [
        // Re-exports ship the banned module's HTTP surface onward -- the
        // importing package still reaches the network through it.
        {
          code: "export * from 'node-fetch';",
          errors: [{ messageId: 'bannedModule' }],
        },
        {
          code: "export * as fetch from 'node-fetch';",
          errors: [{ messageId: 'bannedModule' }],
        },
        {
          code: "export { default as axios } from 'axios';",
          errors: [{ messageId: 'bannedModule' }],
        },
        {
          code: "export { fetch } from 'node-fetch';",
          errors: [{ messageId: 'bannedModule' }],
        },
        // Path-free: however deep the subpath, the source names the
        // banned package, so it is still an import of that package.
        {
          code: "import axios from 'axios/dist/node/axios.cjs';",
          errors: [{ messageId: 'bannedModule' }],
        },
        {
          code: "const fetch = require('node-fetch/lib/index.js');",
          errors: [{ messageId: 'bannedModule' }],
        },
        // Template-literal sources are judged like literal ones.
        {
          code: 'const mod = await import(`axios`);',
          errors: [{ messageId: 'bannedModule' }],
        },
        {
          code: 'const axios = require(`axios`);',
          errors: [{ messageId: 'bannedModule' }],
        },
        {
          code: "export { default as axios } from 'axios/dist/axios.cjs';",
          errors: [{ messageId: 'bannedModule' }],
        },
      ],
    )
  })

  it('reports each violation in mixed code, once per occurrence', () => {
    runRule(
      [],
      [
        {
          code: "import axios from 'axios'; fetch('/api'); window.fetch('/api');",
          errors: [
            { messageId: 'bannedModule' },
            { messageId: 'globalFetch' },
            { messageId: 'memberFetch' },
          ],
        },
      ],
    )
  })
})
