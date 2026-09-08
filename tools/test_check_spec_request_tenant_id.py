#!/usr/bin/env python3
"""Unit tests for check_spec_request_tenant_id.py's scanner rules.

Stdlib-only (unittest), matching this directory's conventions. Run
directly:

    python3 tools/test_check_spec_request_tenant_id.py

The planted fixtures spell each rule's firing and staying-silent shape;
the real tree (13 fragment files, zero findings) is the scanner's
end-to-end negative, run by the checker itself and by repo-checks.

Rules pinned here:

  * A requestBody $ref to a schema that declares tenant_id fires
    (test_request_schema_property_fires).
  * The same property referenced only from a RESPONSE stays silent --
    responses are not requests, and authn's AuthnPrincipal legitimately
    carries tenant_id (test_response_only_schema_stays_silent).
  * A nested $ref chain from a request schema into a schema that
    declares tenant_id fires (test_nested_ref_fires).
  * An inline requestBody schema (no $ref) declaring tenant_id fires
    (test_inline_request_body_fires).
  * A parameter named tenant_id fires (test_parameter_fires).
  * Prose that merely mentions tenant_id stays silent (the discipline
    headers in every spec say "there is deliberately no tenant_id") --
    the scanner keys on the property-declaration line shape
    (test_prose_mention_stays_silent).
  * authn's four sanctioned tenant-naming request schemas stay silent
    inside go/authn's own spec (test_authn_sanctioned_schemas_allowed),
    and the same schema names in another module's spec fire
    (test_sanctioned_name_outside_authn_fires).
"""

from __future__ import annotations

import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import check_spec_request_tenant_id as m  # noqa: E402


def spec(path: str, body: str) -> list[tuple[int, str]]:
    return m.parse_spec(body, path)


AUTHN_PATH = "go/authn/api/openapi.yaml"
OTHER_PATH = "go/someother/api/openapi.yaml"


class FiringRules(unittest.TestCase):
    def test_request_schema_property_fires(self):
        text = """openapi: 3.0.3
info:
  title: planted
  version: 0.0.0
paths:
  /api/v1/x:
    post:
      operationId: x_create
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/XCreateRequest'
      responses:
        '200':
          description: ok
components:
  schemas:
    XCreateRequest:
      type: object
      properties:
        title:
          type: string
        tenant_id:
          type: string
"""
        findings = spec(OTHER_PATH, text)
        self.assertEqual(len(findings), 1)
        self.assertIn("XCreateRequest", findings[0][1])
        self.assertIn("x_create", findings[0][1])

    def test_response_only_schema_stays_silent(self):
        text = """openapi: 3.0.3
info:
  title: planted
  version: 0.0.0
paths:
  /api/v1/x:
    get:
      operationId: x_get
      responses:
        '200':
          description: ok
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/XPrincipal'
components:
  schemas:
    XPrincipal:
      type: object
      properties:
        tenant_id:
          type: string
"""
        self.assertEqual(spec(OTHER_PATH, text), [])

    def test_nested_ref_fires(self):
        text = """openapi: 3.0.3
info:
  title: planted
  version: 0.0.0
paths:
  /api/v1/x:
    post:
      operationId: x_create
      requestBody:
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/XCreateRequest'
components:
  schemas:
    XCreateRequest:
      type: object
      properties:
        scope:
          $ref: '#/components/schemas/XScope'
    XScope:
      type: object
      properties:
        tenant_id:
          type: string
"""
        findings = spec(OTHER_PATH, text)
        self.assertEqual(len(findings), 1)
        self.assertIn("XScope", findings[0][1])

    def test_inline_request_body_fires(self):
        text = """openapi: 3.0.3
info:
  title: planted
  version: 0.0.0
paths:
  /api/v1/x:
    post:
      operationId: x_create
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                tenant_id:
                  type: string
"""
        findings = spec(OTHER_PATH, text)
        self.assertEqual(len(findings), 1)
        self.assertIn("inline", findings[0][1])

    def test_parameter_fires(self):
        text = """openapi: 3.0.3
info:
  title: planted
  version: 0.0.0
paths:
  /api/v1/x:
    get:
      operationId: x_list
      parameters:
        - name: tenant_id
          in: query
          schema:
            type: string
      responses:
        '200':
          description: ok
"""
        findings = spec(OTHER_PATH, text)
        self.assertEqual(len(findings), 1)
        self.assertIn("parameter named tenant_id", findings[0][1])

    def test_prose_mention_stays_silent(self):
        text = """openapi: 3.0.3
info:
  title: planted
  description: >-
    There is deliberately no tenant_id anywhere on this surface, in any
    parameter, header or body: the tenant comes from the access token.
  version: 0.0.0
paths:
  /api/v1/x:
    get:
      operationId: x_get
      responses:
        '200':
          description: ok
"""
        self.assertEqual(spec(OTHER_PATH, text), [])


class AuthnSanction(unittest.TestCase):
    def _authn_spec(self, schema_name: str) -> str:
        return """openapi: 3.0.3
info:
  title: authn
  version: 0.0.0
paths:
  /api/v1/authn/tenant/switch:
    post:
      operationId: authn_switch_tenant
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/%s'
components:
  schemas:
    %s:
      type: object
      required: [tenant_id]
      properties:
        tenant_id:
          type: string
""" % (schema_name, schema_name)

    def test_authn_sanctioned_schemas_allowed(self):
        for name in sorted(m.ALLOWED_TENANT_NAMING_SCHEMAS):
            with self.subTest(schema=name):
                self.assertEqual(spec(AUTHN_PATH, self._authn_spec(name)), [])

    def test_sanctioned_name_outside_authn_fires(self):
        name = "AuthnSwitchTenantRequest"
        findings = spec(OTHER_PATH, self._authn_spec(name))
        self.assertEqual(len(findings), 1)
        self.assertIn("sanctioned only inside authn", findings[0][1])

    def test_unsanctioned_authn_request_fires(self):
        # A NEW authn request schema carrying tenant_id must still fire:
        # the allowlist is the four named schemas, never the whole spec.
        text = self._authn_spec("AuthnBrandNewTenantNamingRequest")
        findings = spec(AUTHN_PATH, text)
        self.assertEqual(len(findings), 1)


if __name__ == "__main__":
    unittest.main()
