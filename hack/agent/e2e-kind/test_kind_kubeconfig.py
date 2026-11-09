# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import shutil
import stat
import tempfile
import textwrap
import threading
import unittest
from pathlib import Path

import e2e
import isolation_plan as plan
import kind_kubeconfig as kk


def fake_kubeconfig(contexts: list[str], current: str) -> str:
    """A stand-in kubeconfig the fake kubectl below reads."""
    return "".join(f"context: {c}\n" for c in contexts) + f"current: {current}\n"


class FakeTools:
    """kind and kubectl over fake kubeconfig files.

    kubectl answers from the file passed with --kubeconfig, so a check that
    reads any other file sees nothing.
    """

    def __init__(self, kind_output: str | None) -> None:
        self.kind_output = kind_output
        self.calls: list[list[str]] = []

    def __call__(self, argv: list[str]) -> tuple[int, str, str]:
        self.calls.append(argv)
        if argv[:3] == ["kind", "get", "kubeconfig"]:
            if self.kind_output is None:
                return 1, "", "ERROR: could not locate any control plane nodes"
            return 0, self.kind_output, ""
        if argv[:2] == ["kubectl", "--kubeconfig"]:
            path = Path(argv[2])
            if not path.is_file():
                return 1, "", "no such file"
            lines = path.read_text().splitlines()
            contexts = [line.split(": ", 1)[1] for line in lines if line.startswith("context: ")]
            current = next((line.split(": ", 1)[1] for line in lines if line.startswith("current: ")), "")
            if argv[3:] == ["config", "get-contexts", "-o", "name"]:
                return 0, "".join(f"{c}\n" for c in contexts), ""
            if argv[3:] == ["config", "current-context"]:
                return (0, current + "\n", "") if current else (1, "", "current-context is not set")
        raise AssertionError(f"unexpected command {argv}")


class PinTest(unittest.TestCase):
    def setUp(self) -> None:
        self.root = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.root, True)
        self.environ: dict[str, str] = {"HOME": str(self.root / "home"), "KUBECONFIG": "/home/user/.kube/config"}

    def test_writes_the_kind_kubeconfig_and_points_every_child_at_it(self) -> None:
        tools = FakeTools(fake_kubeconfig(["kind-e2e"], "kind-e2e"))
        path = kk.pin(self.root, "e2e", runner=tools, environ=self.environ)

        self.assertEqual(self.root / "tmp" / "e2e-kind" / "e2e.kubeconfig", path)
        self.assertEqual(str(path), self.environ["KUBECONFIG"], "replaces an inherited KUBECONFIG")
        self.assertEqual(fake_kubeconfig(["kind-e2e"], "kind-e2e"), path.read_text())
        self.assertEqual(0o600, stat.S_IMODE(path.stat().st_mode), "holds cluster-admin credentials")
        self.assertEqual(["kind", "get", "kubeconfig", "--name", "e2e"], tools.calls[0])

    def test_concurrent_writers_do_not_take_each_others_file(self) -> None:
        path = self.root / "tmp" / "e2e-kind" / "e2e.kubeconfig"
        content = fake_kubeconfig(["kind-e2e"], "kind-e2e")
        errors: list[BaseException] = []

        def write() -> None:
            try:
                for _ in range(50):
                    kk.write_from_kind("e2e", path, runner=FakeTools(content))
            except BaseException as exc:  # noqa: BLE001 - collected for the assertion
                errors.append(exc)

        threads = [threading.Thread(target=write) for _ in range(8)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()

        self.assertEqual([], errors)
        self.assertEqual(content, path.read_text())
        self.assertEqual(0o600, stat.S_IMODE(path.stat().st_mode))
        self.assertEqual([path], list(path.parent.iterdir()), "no temporary file is left behind")

    def test_refuses_a_kubeconfig_with_another_context(self) -> None:
        tools = FakeTools(fake_kubeconfig(["kind-e2e", "prod-aks"], "kind-e2e"))
        with self.assertRaisesRegex(kk.KubeconfigError, r"must hold only context 'kind-e2e'.*prod-aks"):
            kk.pin(self.root, "e2e", runner=tools, environ=self.environ)

    def test_refuses_when_the_current_context_is_not_the_kind_cluster(self) -> None:
        for current in ("prod-aks", ""):
            with self.subTest(current=current):
                tools = FakeTools(fake_kubeconfig(["kind-e2e"], current))
                with self.assertRaisesRegex(kk.KubeconfigError, "current-context"):
                    kk.pin(self.root, "e2e", runner=tools, environ=self.environ)

    def test_a_missing_cluster_fails_closed(self) -> None:
        stale = kk.default_path(self.root, "e2e")
        stale.parent.mkdir(parents=True)
        stale.write_text(fake_kubeconfig(["kind-e2e"], "kind-e2e"))

        with self.assertRaisesRegex(kk.KubeconfigError, "kind get kubeconfig --name e2e failed"):
            kk.pin(self.root, "e2e", runner=FakeTools(None), environ=self.environ)
        self.assertEqual(str(stale), self.environ["KUBECONFIG"], "never left on the inherited kubeconfig")

        self.assertIsNone(kk.pin(self.root, "e2e", required=False, runner=FakeTools(None), environ=self.environ))
        self.assertEqual(str(stale), self.environ["KUBECONFIG"])
        self.assertFalse(stale.exists(), "a stale file of an earlier cluster does not stand in for it")

    def test_an_optional_command_still_refuses_another_cluster(self) -> None:
        tools = FakeTools(fake_kubeconfig(["prod-aks"], "prod-aks"))
        with self.assertRaises(kk.KubeconfigError):
            kk.pin(self.root, "e2e", required=False, runner=tools, environ=self.environ)

    def test_an_explicit_path_is_written_from_kind(self) -> None:
        tools = FakeTools(fake_kubeconfig(["kind-kind"], "kind-kind"))
        path = kk.pin(self.root, "kind", path=self.root / "smoke" / "kind.kubeconfig", runner=tools, environ=self.environ)
        self.assertEqual(self.root / "smoke" / "kind.kubeconfig", path)
        self.assertEqual(str(path), self.environ["KUBECONFIG"])

    def test_the_override_is_used_as_it_is_and_still_checked(self) -> None:
        override = self.root / "mine.kubeconfig"
        override.write_text(fake_kubeconfig(["kind-e2e"], "kind-e2e"))
        self.environ[kk.ENV_OVERRIDE] = str(override)
        tools = FakeTools(None)

        self.assertEqual(override, kk.pin(self.root, "e2e", runner=tools, environ=self.environ))
        self.assertEqual(str(override), self.environ["KUBECONFIG"])
        self.assertNotIn(["kind", "get", "kubeconfig", "--name", "e2e"], tools.calls, "not overwritten")

        override.write_text(fake_kubeconfig(["kind-e2e"], "prod-aks"))
        with self.assertRaises(kk.KubeconfigError):
            kk.pin(self.root, "e2e", runner=tools, environ=self.environ)

        missing = self.root / "missing.kubeconfig"
        self.environ[kk.ENV_OVERRIDE] = str(missing)
        self.assertIsNone(kk.pin(self.root, "e2e", required=False, runner=tools, environ=self.environ))
        with self.assertRaisesRegex(kk.KubeconfigError, "does not exist"):
            kk.pin(self.root, "e2e", runner=tools, environ=self.environ)

    def test_refuses_the_users_default_kubeconfig(self) -> None:
        default = Path(self.environ["HOME"]) / ".kube" / "config"
        default.parent.mkdir(parents=True)
        default.write_text(fake_kubeconfig(["kind-e2e"], "kind-e2e"))
        self.environ[kk.ENV_OVERRIDE] = str(default)

        with self.assertRaisesRegex(kk.KubeconfigError, "refusing to use the default kubeconfig"):
            kk.pin(self.root, "e2e", runner=FakeTools(None), environ=self.environ)


@unittest.skipUnless(shutil.which("kubectl"), "kubectl is not installed")
class RealKubectlCheckTest(unittest.TestCase):
    """check() against the real kubectl, which never contacts a cluster here."""

    def kubeconfig(self, contexts: list[str], current: str) -> Path:
        directory = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, directory, True)
        path = directory / "config"
        entries = "".join(textwrap.dedent(f"""\
            - name: {c}
              context: {{cluster: {c}, user: {c}}}
            """) for c in contexts)
        clusters = "".join(f"- name: {c}\n  cluster: {{server: 'https://127.0.0.1:1'}}\n" for c in contexts)
        users = "".join(f"- name: {c}\n  user: {{}}\n" for c in contexts)
        path.write_text(f"apiVersion: v1\nkind: Config\ncurrent-context: {current}\n"
                        f"clusters:\n{clusters}contexts:\n{entries}users:\n{users}")
        return path

    def test_accepts_only_the_kind_context(self) -> None:
        environ = {"HOME": "/nonexistent"}
        kk.check("e2e", self.kubeconfig(["kind-e2e"], "kind-e2e"), environ=environ)

        with self.assertRaises(kk.KubeconfigError):
            kk.check("e2e", self.kubeconfig(["kind-e2e", "prod-aks"], "kind-e2e"), environ=environ)
        with self.assertRaises(kk.KubeconfigError):
            kk.check("e2e", self.kubeconfig(["kind-e2e", "prod-aks"], "prod-aks"), environ=environ)


class HarnessPinTest(unittest.TestCase):
    def test_a_command_that_needs_the_cluster_stops_when_it_cannot_pin(self) -> None:
        with mock.patch.object(e2e.kind_kubeconfig, "pin", side_effect=kk.KubeconfigError("not kind")) as pin, \
                mock.patch("sys.stderr"):
            with self.assertRaises(SystemExit):
                e2e.pin_kubeconfig("run-agent")
        pin.assert_called_once_with(e2e.REPO_ROOT, e2e.KIND_CLUSTER_NAME, required=True)

    def test_commands_that_run_without_a_cluster_do_not_require_one(self) -> None:
        for command in ("resolve-host-image", "cleanup", "list-suite"):
            with self.subTest(command=command), mock.patch.object(e2e.kind_kubeconfig, "pin", return_value=None) as pin:
                e2e.pin_kubeconfig(command)
                pin.assert_called_once_with(e2e.REPO_ROOT, e2e.KIND_CLUSTER_NAME, required=False)

    def test_every_cluster_free_command_exists(self) -> None:
        known = {*e2e.COMMANDS, "run-suite", "list-suite"}
        self.assertLessEqual(e2e.CLUSTER_OPTIONAL_COMMANDS, known)
        self.assertNotIn("run-suite", e2e.CLUSTER_OPTIONAL_COMMANDS)
        self.assertNotIn("run-agent", e2e.CLUSTER_OPTIONAL_COMMANDS)

    def test_main_pins_before_it_runs_a_command(self) -> None:
        order: list[str] = []
        with mock.patch.object(e2e, "pin_kubeconfig", side_effect=lambda c: order.append(f"pin {c}")), \
                mock.patch.dict(e2e.COMMANDS, {"cleanup": lambda _nc: order.append("cleanup")}), \
                mock.patch("sys.argv", ["e2e.py", "cleanup"]):
            e2e.main()
        self.assertEqual(["pin cleanup", "cleanup"], order)


class PlanEnvironmentTest(unittest.TestCase):
    def test_children_get_the_kind_kubeconfig_not_the_inherited_one(self) -> None:
        root = Path("/repo")
        driver = plan.Driver(runner=None, repo_root=root,  # type: ignore[arg-type]
                             base_env={"KUBECONFIG": "/home/user/.kube/config", "KIND_CLUSTER_NAME": "e2e"})
        self.assertEqual(str(root / "tmp" / "e2e-kind" / "e2e.kubeconfig"), driver._env()["KUBECONFIG"])

        driver = plan.Driver(runner=None, repo_root=root, base_env={})  # type: ignore[arg-type]
        self.assertEqual(str(root / "tmp" / "e2e-kind" / "kind.kubeconfig"), driver._env()["KUBECONFIG"])

        driver = plan.Driver(runner=None, repo_root=root,  # type: ignore[arg-type]
                             base_env={kk.ENV_OVERRIDE: "/work/kind.kubeconfig"})
        self.assertEqual("/work/kind.kubeconfig", driver._env()["KUBECONFIG"])


class ShellRunnerTest(unittest.TestCase):
    """run-local.sh exports the dedicated kubeconfig before kind and kubectl run."""

    def test_run_local_never_uses_the_default_kubeconfig(self) -> None:
        script = (Path(__file__).parent / "run-local.sh").read_text()
        export = script.index('export KUBECONFIG="${E2E_KUBECONFIG:-${REPO_ROOT}/tmp/e2e-kind/')
        self.assertLess(export, script.index("kind create cluster"))
        self.assertIn('kind create cluster --name "${KIND_CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}"', script)
        for line in script.splitlines():
            stripped = line.strip()
            if stripped.startswith("kubectl ") and "config " not in stripped:
                self.assertIn('--context "${KIND_CONTEXT}"', stripped)


if __name__ == "__main__":
    unittest.main()
