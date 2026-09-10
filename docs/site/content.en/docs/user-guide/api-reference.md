---
title: API reference
weight: 5
bookToC: false
description: "The complete platform API reference, rendered from the merged OpenAPI contract of the eleven platform modules — every operation, parameter, schema and error a speed-based service exposes."
---

# API reference

The complete HTTP API of a speed-based service, rendered from the
merged OpenAPI contract of the eleven platform modules (authn,
notification, billing, admin, ai-gateway, integration, org, pki,
sharing, storage and config). The contract is the single source of truth —
spec first, generated surfaces in lockstep — so what you see here is
exactly what a composed service answers on `/api/v1/...`.

Things worth knowing before you browse:

- **Authentication** rides the access token: requests carry
  `Authorization: Bearer <token>`. There is no tenant header — the
  tenant travels inside the token's claims, resolved by the service's
  own middleware, never taken from the request.
- **Errors are structured**: an error response carries a machine
  code (`module.snake_case`), a trace id, and parameters. See the
  [error code index](../error-codes/) for the code conventions and
  the complete list of codes.
- **Operations are grouped by module tag**; the modules behind each
  tag are covered in their own [module pages](../modules/), and the
  [domain guides](../domains/) walk whole product flows end to end.

<div id="redoc"></div>
<script src="/apidocs/redoc.standalone.js"></script>
<script>
  Redoc.init('/apidocs/speed.yaml', {
    hideLoading: true,
    requiredPropsFirst: true,
    expandDefaultServerVariables: true,
    scrollYOffset: 8
  }, document.getElementById('redoc'));
</script>
