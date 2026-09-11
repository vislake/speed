#!/usr/bin/env python3
"""Unit tests for check_host_composition.py's rules.

Stdlib-only (unittest + tempfile), matching this directory's
conventions. Run directly:

    python3 tools/test_check_host_composition.py

The real tree passes the gate clean, so these planted fixtures are the
rules' living proof, in the same shape as the sibling checker suites:

  * a host that merely USES the kernel (imports the platform module and
    assembles through the engine's options) stays silent -- the positive
    side, proving the gate is not simply firing on everything;
  * a forked declaration of a shared identifier in either host fires,
    while a test file's own use of the identifiers does not;
  * a re-grown kernel sentinel in a non-test file fires, while the same
    text in a test file stays silent;
  * the sentinel and call-ban allowances stay scoped to the one named
    file (the reference app's application component): the sanctioned
    shapes there stay silent, the same shapes in another host file --
    and in the skeleton's tree -- still fire;
  * a re-issued engine-owned assembly call (pkgcore.NewKernel, dbkit.Open,
    http.NewServeMux, chain.Chain under any alias, obs.Init, ...) in a
    non-test file fires, while the sanctioned engine options and the same
    text in a test file stay silent.
"""

from __future__ import annotations

import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import check_host_composition as m  # noqa: E402


def make_tree(files: dict[str, str]) -> pathlib.Path:
    root = pathlib.Path(tempfile.mkdtemp(prefix="host-composition-"))
    for (rel, text) in files.items():
        path = root / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")
    return root


class NoForkRules(unittest.TestCase):
    def test_a_host_importing_the_kernel_stays_silent(self):
        root = make_tree(
            {
                "examples/reference-app/internal/app/server.go": (
                    "package app\n\n"
                    "import speedapp \"github.com/vislake/speed/go/app\"\n\n"
                    "func build() { _ = speedapp.PreAuthAllowlist() }\n"
                ),
                "go/saasctl/internal/template/project/cmd/server/main.go": (
                    "package main\n\n"
                    "import speedapp \"github.com/vislake/speed/go/app\"\n\n"
                    "func run() { _ = speedapp.ServeUntilShutdown }\n"
                ),
            }
        )
        self.assertEqual(m.scan(root), [])

    def test_forked_declaration_in_the_app_fires(self):
        root = make_tree(
            {
                "examples/reference-app/internal/app/server.go": (
                    "package app\n\n// AuthnAPIPath re-declares the shared name.\n"
                    "const AuthnAPIPath = \"/api/v1/authn\"\n"
                ),
            }
        )
        findings = m.scan(root)
        # Two findings, both true: the declaration fork, and the
        # kernel-owned path literal it reintroduces.
        self.assertTrue(
            any("declares AuthnAPIPath" in f for f in findings), findings
        )
        self.assertTrue(
            any('"/api/v1/authn"' in f for f in findings), findings
        )

    def test_forked_declaration_in_the_template_fires(self):
        root = make_tree(
            {
                "go/saasctl/internal/template/project/cmd/server/server.go": (
                    "package main\n\nfunc PreAuthAllowlist() {}\n"
                ),
            }
        )
        findings = m.scan(root)
        self.assertEqual(len(findings), 1)
        self.assertIn("declares PreAuthAllowlist", findings[0])

    def test_forked_lifecycle_declaration_fires(self):
        root = make_tree(
            {
                "examples/reference-app/cmd/server/main.go": (
                    "package main\n\nfunc ServeUntilShutdown() error { return nil }\n"
                ),
            }
        )
        findings = m.scan(root)
        self.assertEqual(len(findings), 1)
        self.assertIn("declares ServeUntilShutdown", findings[0])

    def test_sentinel_in_a_non_test_file_fires(self):
        root = make_tree(
            {
                "examples/reference-app/internal/app/server.go": (
                    "package app\n\nfunc serve() { srv.ListenAndServe() }\n"
                ),
            }
        )
        findings = m.scan(root)
        self.assertEqual(len(findings), 1)
        self.assertIn("ListenAndServe", findings[0])

    def test_sentinel_in_a_test_file_stays_silent(self):
        root = make_tree(
            {
                "examples/reference-app/flowtests/probe_test.go": (
                    "package flowtests\n\n"
                    "func TestProbe(t *testing.T) { mux.HandleFunc(\"/healthz\", h) }\n"
                ),
            }
        )
        self.assertEqual(m.scan(root), [])

    def test_the_application_component_may_serve_its_own_face(self):
        # The reference app's application component owns the host's face:
        # the mux it composes and the request base context its listener
        # carries are sanctioned in that one file.
        root = make_tree(
            {
                "examples/reference-app/internal/app/component.go": (
                    "package app\n\n"
                    "func compose() {\n"
                    "\tmux := http.NewServeMux()\n"
                    "\tsrv := &http.Server{BaseContext: baseContext}\n"
                    "\t_ = mux\n"
                    "\t_ = srv\n"
                    "}\n"
                ),
            }
        )
        self.assertEqual(m.scan(root), [])

    def test_the_sanctioned_shapes_in_another_host_file_still_fire(self):
        root = make_tree(
            {
                "examples/reference-app/internal/app/server.go": (
                    "package app\n\n"
                    "func compose() {\n"
                    "\tmux := http.NewServeMux()\n"
                    "\tsrv := &http.Server{BaseContext: baseContext}\n"
                    "\t_ = mux\n"
                    "\t_ = srv\n"
                    "}\n"
                ),
                "go/saasctl/internal/template/project/cmd/server/server.go": (
                    "package main\n\n"
                    "func compose() { mux := http.NewServeMux(); _ = mux }\n"
                ),
            }
        )
        findings = m.scan(root)
        self.assertTrue(
            any("http.NewServeMux" in f for f in findings), findings
        )
        self.assertTrue(any("BaseContext" in f for f in findings), findings)
        self.assertEqual(
            sum("http.NewServeMux" in f for f in findings), 2, findings
        )


class EngineOwnedCallRules(unittest.TestCase):
    def test_engine_owned_call_in_a_non_test_file_fires(self):
        root = make_tree(
            {
                "go/saasctl/internal/template/project/cmd/server/server.go": (
                    "package main\n\n"
                    "func boot() {\n"
                    "\treg, _ := pkgcore.NewKernel(opts...).Bootstrap(ctx, mods...)\n"
                    "\t_ = dbkit.Open(ctx, dbkit.Options{})\n"
                    "\tmux := http.NewServeMux()\n"
                    "\t_ = signal.NotifyContext(ctx, syscall.SIGINT)\n"
                    "\t_ = obs.Init(ctx)\n"
                    "\t_ = reg\n"
                    "}\n"
                ),
            }
        )
        findings = m.scan(root)
        for label in (
            "pkgcore.NewKernel",
            ".Bootstrap(",
            "dbkit.Open",
            "http.NewServeMux",
            "signal.NotifyContext",
            "obs.Init",
        ):
            self.assertTrue(
                any(f"calls {label}" in f for f in findings), (label, findings)
            )

    def test_chain_entry_point_fires_under_every_alias(self):
        root = make_tree(
            {
                "examples/reference-app/internal/app/server.go": (
                    "package app\n\n"
                    "import speedchain \"github.com/vislake/speed/go/app/chain\"\n\n"
                    "func chain() { _, _ = speedchain.Chain(speedchain.Config{}) }\n"
                ),
                "go/saasctl/internal/template/project/selection/authn/server.go": (
                    "package main\n\n"
                    "func chain() { _, _ = chain.Chain(chain.Config{}) }\n"
                ),
            }
        )
        findings = m.scan(root)
        self.assertEqual(
            sum("calls chain.Chain" in f for f in findings), 2, findings
        )

    def test_the_engine_options_stay_silent(self):
        root = make_tree(
            {
                "go/saasctl/internal/template/project/selection/authn/server.go": (
                    "package main\n\n"
                    "import speedchain \"github.com/vislake/speed/go/app/chain\"\n\n"
                    "func boot() {\n"
                    "\topts := []speedapp.Option{\n"
                    "\t\tspeedapp.WithKernelOptions(pkgcore.WithDeploymentMode(mode)),\n"
                    "\t\tspeedapp.WithDatabase(speedapp.DatabaseSpec{Dialect: dbkit.DialectSQLite}),\n"
                    "\t\tspeedapp.WithHooks(speedapp.Hooks{PostBootstrap: attach}),\n"
                    "\t}\n"
                    "\t_ = speedchain.Standard(reg, verifier, mux)\n"
                    "\t_ = opts\n"
                    "}\n"
                ),
            }
        )
        self.assertEqual(m.scan(root), [])

    def test_a_longer_package_name_ending_in_the_selector_stays_silent(self):
        # Business-path file: the banned selectors' lookbehind precision is
        # what this pins (myobs.Init is not obs.Init; xpkgcore.NewKernel is
        # not pkgcore.NewKernel). It lives outside the composition paths
        # because the staged component-assembly bans do fire on any .Init(
        # inside them, which the staged class pins separately.
        root = make_tree(
            {
                "examples/reference-app/internal/notes/store.go": (
                    "package notes\n\n"
                    "func boot() {\n"
                    "\t_ = myobs.Init(ctx)\n"
                    "\t_ = xpkgcore.NewKernel()\n"
                    "}\n"
                ),
            }
        )
        self.assertEqual(m.scan(root), [])

    def test_engine_owned_calls_in_a_test_file_stay_silent(self):
        root = make_tree(
            {
                "examples/reference-app/flowtests/probe_test.go": (
                    "package flowtests\n\n"
                    "func TestProbe(t *testing.T) {\n"
                    "\t_ = pkgcore.NewKernel()\n"
                    "\tmux := http.NewServeMux()\n"
                    "\t_ = mux\n"
                    "}\n"
                ),
            }
        )
        self.assertEqual(m.scan(root), [])


class ComponentAssemblyStagedRules(unittest.TestCase):
    """The staged half: the component assembly's forbidden shapes fire, and
    the calls the hosts legitimately make today stay silent."""

    def test_new_world_wrong_shapes_fire(self):
        root = make_tree(
            {
                "go/saasctl/internal/template/project/cmd/server/server.go": (
                    "package main\n\n"
                    "func boot(ctx context.Context) {\n"
                    "\treg := pkgcore.NewComponentRegistry()\n"
                    "\t_ = reg.Prepare(ctx)\n"
                    "\t_ = reg.Construct(ctx)\n"
                    "\t_ = reg.Verify(ctx)\n"
                    "\t_ = reg.Init(ctx)\n"
                    "}\n"
                ),
            }
        )
        findings = m.scan(root)
        for label in (
            "pkgcore.NewComponentRegistry",
            "a component-registry Prepare call",
            "a component-registry Construct call",
            "a component-registry Init call",
            "a component-registry Verify call",
        ):
            self.assertTrue(
                any(f"calls {label}" in f for f in findings), (label, findings)
            )

    def test_business_code_and_test_files_stay_silent(self):
        root = make_tree(
            {
                # Module-level code outside the composition paths: its own
                # Prepare/Init methods are not the assembly's stages.
                "examples/reference-app/internal/notes/handler.go": (
                    "package notes\n\n"
                    "func (h *Handler) Init() {}\n"
                    "func (h *Handler) Prepare() {}\n"
                ),
                # A test file inside a composition path: exempt.
                "examples/reference-app/internal/app/server_test.go": (
                    "package app\n\n"
                    "func TestBoot(t *testing.T) { _ = reg.Prepare(context.Background()) }\n"
                ),
            }
        )
        self.assertEqual(m.scan(root), [])

    def test_host_service_lifecycle_calls_stay_silent(self):
        # The hosts' composition files call Start/Stop/Close on their own
        # services today; those verbs stage out of the ban set until the
        # hosts migrate.
        root = make_tree(
            {
                "examples/reference-app/internal/app/attach.go": (
                    "package app\n\n"
                    "func wire(q *jobs.StandaloneQueue, s *jobs.Scheduler) {\n"
                    "\t_ = q.Start(ctx)\n"
                    "\t_ = q.Close(ctx)\n"
                    "\ts.Stop()\n"
                    "}\n"
                ),
            }
        )
        self.assertEqual(m.scan(root), [])


class AllowedFileRules(unittest.TestCase):
    """The one-file allowances the reference app's assembly relies on: the
    assembly core creates the registry and owns the process's signal
    handling, and the host's database component opens the connection with
    its write-capture scope. Each stays silent in its own file and fires
    from any other."""

    ALLOWED_BODIES = {
        "examples/reference-app/internal/app/server.go": (
            "package app\n\n"
            "func assemble(ctx *Context) {\n"
            "\treg := pkgcore.NewComponentRegistry()\n"
            "\tctx, stop := signal.NotifyContext(ctx, syscall.SIGINT)\n"
            "\t_ = reg\n"
            "\t_ = stop\n"
            "}\n"
        ),
        "examples/reference-app/internal/app/host_wiring.go": (
            "package app\n\n"
            "func (b *serverBuild) dbComponent() {\n"
            "\t_ = dbkit.Open(ctx, dbkit.Options{})\n"
            "}\n"
        ),
    }

    def test_the_allowed_files_stay_silent(self):
        root = make_tree(
            {
                path: body
                for path, body in self.ALLOWED_BODIES.items()
            }
        )
        self.assertEqual(m.scan(root), [])

    def test_the_same_calls_fire_from_another_composition_file(self):
        root = make_tree(
            {
                "examples/reference-app/internal/app/serve.go": (
                    "package app\n\n"
                    "func assemble(ctx *Context) {\n"
                    "\treg := pkgcore.NewComponentRegistry()\n"
                    "\tctx, stop := signal.NotifyContext(ctx, syscall.SIGINT)\n"
                    "\t_ = dbkit.Open(ctx, dbkit.Options{})\n"
                    "\t_ = reg\n"
                    "\t_ = stop\n"
                    "}\n"
                ),
            }
        )
        findings = m.scan(root)
        for label in (
            "pkgcore.NewComponentRegistry",
            "signal.NotifyContext",
            "dbkit.Open",
        ):
            self.assertTrue(
                any(f"calls {label}" in f for f in findings), (label, findings)
            )


if __name__ == "__main__":
    unittest.main()
