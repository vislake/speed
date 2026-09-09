/**
 * The hand-written seam of the app-owned SDK. Everything else in
 * src/app-api is orval-generated output carrying a DO-NOT-EDIT header
 * (see orval.config.ts in this directory); this seam exists so the
 * generated code imports a stable './runtime' target of its own and
 * still never learns how a host wires its HTTP transport.
 *
 * The seam re-exports the platform seam -- @speed/api-sdk/runtime --
 * instead of holding its own binding slot: the app host binds exactly
 * one RequestFn at bootstrap (main.tsx's bindRequestFn(createClient
 * (...))), and both generated surfaces this host renders -- the
 * platform @speed/api-sdk operations and this app-owned SDK's
 * operations -- travel through that one binding. A second module-level
 * slot would have to be kept in sync at every bind site (the bootstrap
 * and every test rig), a split with no payoff in this single-process
 * host. When the app-owned flow is productized (saasctl openapi
 * generate, recorded in docs/internal/24-deferral-roadmap.md), the
 * seam decision is re-made for the generated project's shape.
 *
 * Nothing here may be edited by regeneration: orval's output paths
 * cover only src/app-api/index.ts, never this file. The nodenext fixup
 * (web/scripts/orval-nodenext-fixup.mjs, run with this file and the
 * generated entry as its explicit paths by task api:gen:app) verifies
 * this seam exists after every regeneration.
 */

export {
  bindRequestFn,
  speedRequest,
  speedRequestCredentialless,
} from '@speed/api-sdk/runtime'
