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
        // different call shape (reference-app-web.md P2-6).
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

  it('flags the environment fetch captured out of a call: aliasing, references and destructuring', () => {
    // A reference to window.fetch/globalThis.fetch/self.fetch that is
    // NOT itself a call is still the global fetch leaving the api-client
    // layer -- it exists to be called somewhere, and answering "no
    // call, no violation" left every alias form (const f =
    // window.fetch; f(url)) outside the rule (reference-app-web.md
    // P2-6). Destructuring fetch out of the environment object is the
    // same capture with a different syntax.
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
