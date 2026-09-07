#!/usr/bin/env node
/**
 * fake-image-provider.mjs -- the browser end-to-end suite's stand-in
 * for the OpenAI-compatible images endpoint the smile-simulation
 * pipeline reaches (playwright.config.ts boots it as a third webServer
 * entry and points APP_AI_GATEWAY_IMAGE_BASE_URL/API_KEY at it). It
 * answers every POST /images/edits with one fixed, deterministic
 * "simulated smile" -- a tiny red PNG served as a base64 data entry
 * plus a fixed usage block -- the exact wire shape go/ai-gateway's
 * OpenAICompatibleImageProvider parses and cmd/server's own Go flow
 * tests fake with the identical answer (smilesim_flow_test.go's
 * fakeOpenAIImageServer): no live provider and no live key are
 * involved, only a real multipart request reaching a real server, so
 * the block-B gates run deterministically against a freshly booted
 * reference-app server.
 *
 * The response bytes differ from the e2e suite's patient photo (a 1x1
 * dark pixel) in byte length and color -- both are 1x1, so it is not a
 * difference in image DIMENSIONS -- and that is what makes the
 * before/after pair a gate examines genuinely two different images.
 * (The earlier wording here said "size", which a reader could check and
 * find false; a checkable claim that is wrong costs the rest of this
 * comment its credibility, which matters because the rest is what a
 * reviewer relies on to judge the seam.) The image passes
 * go/storage's probe exactly like the Go suite's canned gray PNG: a
 * small, structurally valid PNG with no metadata segments.
 *
 * Health: GET /healthz answers 200 so Playwright's webServer health
 * check can wait for the process.
 */

import { createServer } from 'node:http'

/** A fixed red 1x1 PNG: the deterministic "generated smile". */
const SIMULATED_SMILE_B64 =
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGO4o6EBAAMQAS0ujiXaAAAAAElFTkSuQmCC'

const port = process.env.PORT ?? '0'

const server = createServer((request, response) => {
  if (request.method === 'GET' && request.url === '/healthz') {
    response.writeHead(200, { 'content-type': 'text/plain' })
    response.end('ok')
    return
  }
  if (request.method === 'POST' && request.url === '/images/edits') {
    // A vendor that refuses, when the suite asks for one.
    //
    // Without this the fake could only ever succeed, so the most
    // expensive wrong path in a pay-per-use product had no browser
    // coverage at all: a generation that fails must give the credits
    // back, and a silent failure to refund is the defect nobody
    // complains about until they reconcile. The switch is the
    // environment rather than a request header so a gate sets it on the
    // whole server it boots, the way it sets every other server fact.
    if (process.env.FAKE_IMAGE_FAIL === '1') {
      request.on('data', () => {})
      request.on('end', () => {
        response.writeHead(500, { 'content-type': 'application/json' })
        response.end(JSON.stringify({ error: { message: 'fake provider refused on purpose' } }))
      })
      return
    }
    // Drain the multipart body the provider sent (its prompt and the
    // patient photo) -- the suite's Go twin asserts on those parts; a
    // browser e2e only needs the reply to be the reply.
    request.on('data', () => {})
    request.on('end', () => {
      response.writeHead(200, { 'content-type': 'application/json' })
      response.end(
        JSON.stringify({
          data: [{ b64_json: SIMULATED_SMILE_B64 }],
          // Consistent with the bytes actually returned. It used to say
          // 1024x1024 while answering a 1x1 image: harmless today, since
          // go/storage's probe of the stored bytes is the authority over
          // any claim about them, but it meant a gate could not have
          // caught a product that started trusting the vendor's claim
          // instead.
          usage: { image_count: 1, steps: 30, size: '1x1' },
        }),
      )
    })
    return
  }
  response.writeHead(404, { 'content-type': 'text/plain' })
  response.end('not found')
})

server.listen(Number(port), '127.0.0.1', () => {
  const address = server.address()
  const actual = typeof address === 'object' && address !== null ? address.port : port
  console.log(`fake-image-provider listening on ${actual}`)
})
