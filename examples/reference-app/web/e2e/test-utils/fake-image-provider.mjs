#!/usr/bin/env node
/**
 * fake-image-provider.mjs -- the browser end-to-end suite's stand-in
 * for the OpenAI-compatible images endpoint the smile-simulation
 * pipeline reaches (playwright.config.ts boots it as a third webServer
 * entry and points APP_AI_GATEWAY_IMAGE_BASE_URL/API_KEY at it). It
 * answers every POST /images/edits with one fixed, deterministic
 * "simulated smile" -- a tiny red PNG served as a base64 data entry
 * plus a fixed usage block -- the exact wire shape go/ai-gateway's
 * OpenAICompatibleImageProvider parses and the flowtests suite fakes
 * with the identical answer (flowtests/smilesim_flow_test.go's
 * fakeOpenAIImageServer): no live provider and no live key are
 * involved, only a real multipart request reaching a real server, so
 * the simulation gates run deterministically against a freshly booted
 * reference-app server.
 *
 * The response bytes differ from the e2e suite's patient photo (a 1x1
 * dark pixel) in byte length and color -- both are 1x1, so it is not a
 * difference in image DIMENSIONS -- and that is what makes the
 * before/after pair a gate examines genuinely two different images.
 * The image passes go/storage's probe exactly like the Go suite's
 * canned gray PNG: a small, structurally valid PNG with no metadata
 * segments.
 *
 * It answers after a short delay (FAKE_IMAGE_DELAY_MS, 300ms by
 * default) rather than instantly, and that is fidelity rather than
 * padding: a real image generation takes seconds, which is why the
 * pipeline is asynchronous and why the product must tell a person their
 * simulation is running. An instant answer would make the in-flight
 * state nearly unobservable, and the gate for "a generation in flight
 * must say so" would pass or fail on whether it won a race against the
 * provider -- a gate that depends on winning a race is not a gate, it
 * is a source of red that gets blamed on the product. The delay makes
 * the requirement checkable; it does not make a broken product pass,
 * because a surface that announces nothing still announces nothing
 * after 300ms.
 *
 * Health: GET /healthz answers 200 so Playwright's webServer health
 * check can wait for the process.
 */

import { createServer } from "node:http";

/** A fixed red 1x1 PNG: the deterministic "generated smile". */
const SIMULATED_SMILE_B64 =
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGO4o6EBAAMQAS0ujiXaAAAAAElFTkSuQmCC";

const port = process.env.PORT ?? "0";

/** How long the fake vendor takes to answer, in milliseconds. */
const delayMs = Number(process.env.FAKE_IMAGE_DELAY_MS ?? "300");

const server = createServer((request, response) => {
  if (request.method === "GET" && request.url === "/healthz") {
    response.writeHead(200, { "content-type": "text/plain" });
    response.end("ok");
    return;
  }
  if (request.method === "POST" && request.url === "/images/edits") {
    // A vendor that refuses, when the suite asks for one.
    //
    // Without this the fake could only ever succeed, so the most
    // expensive wrong path in a pay-per-use product had no browser
    // coverage at all: a generation that fails must give the credits
    // back, and a silent failure to refund is the defect nobody
    // complains about until they reconcile. The switch is the
    // environment rather than a request header so a gate sets it on the
    // whole server it boots, the way it sets every other server fact.
    if (process.env.FAKE_IMAGE_FAIL === "1") {
      request.on("data", () => {});
      request.on("end", () => {
        response.writeHead(500, { "content-type": "application/json" });
        response.end(
          JSON.stringify({
            error: { message: "fake provider refused on purpose" },
          }),
        );
      });
      return;
    }
    // Drain the multipart body the provider sent (its prompt and the
    // patient photo) -- the suite's Go twin asserts on those parts; a
    // browser e2e only needs the reply to be the reply.
    request.on("data", () => {});
    request.on("end", () => {
      setTimeout(() => {
        response.writeHead(200, { "content-type": "application/json" });
        response.end(
          JSON.stringify({
            data: [{ b64_json: SIMULATED_SMILE_B64 }],
            // Consistent with the bytes actually returned: this answers
            // a 1x1 image and says so, because go/storage's probe of the
            // stored bytes is the authority over any claim about them --
            // a usage block that lied about the size would leave a gate
            // unable to catch a product that started trusting the
            // vendor's claim instead.
            usage: { image_count: 1, steps: 30, size: "1x1" },
          }),
        );
      }, delayMs);
    });
    return;
  }
  response.writeHead(404, { "content-type": "text/plain" });
  response.end("not found");
});

server.listen(Number(port), "127.0.0.1", () => {
  const address = server.address();
  const actual =
    typeof address === "object" && address !== null ? address.port : port;
  console.log(`fake-image-provider listening on ${actual}`);
});
