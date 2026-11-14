# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Contract tests for the on-demand dashboard publishing command."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "hack/scripts/gantry-ui-deploy.sh"
DIGEST = "sha256:" + "a" * 64


class DeployTests(unittest.TestCase):
    def setUp(self):
        (ROOT / "tmp").mkdir(exist_ok=True)
        self.temp = tempfile.TemporaryDirectory(dir=ROOT / "tmp")
        self.addCleanup(self.temp.cleanup)
        self.work = Path(self.temp.name)
        self.log = self.work / "calls"
        mock = self.work / "mock"
        mock.write_text(
            "#!/usr/bin/env python3\n"
            "import json,os,sys\n"
            "from pathlib import Path\n"
            "tool=Path(sys.argv[0]).name\n"
            "args=sys.argv[1:]\n"
            "with open(os.environ['CALL_LOG'],'a') as f: f.write(json.dumps([tool,*args])+'\\n')\n"
            "if os.environ.get('FAIL_TOOL')==tool and (not os.environ.get('FAIL_ARG') or os.environ['FAIL_ARG'] in args): sys.exit(1)\n"
            "if tool=='git': print('abc123')\n"
            f"if tool=='docker' and args[:3]==['buildx','imagetools','inspect']: print('Digest: {DIGEST}')\n"
            f"if tool=='podman' and '--digestfile' in args: Path(args[args.index('--digestfile')+1]).write_text('{DIGEST}')\n"
        )
        mock.chmod(0o755)
        for tool in ["docker", "podman", "kubectl", "helm", "git"]:
            (self.work / tool).symlink_to(mock)
        self.kube = self.work / "kube"
        self.values = self.work / "values.yaml"
        self.kube.write_text("test fixture\n")
        self.values.write_text("registry: example.azurecr.io\n")
        self.env = {
            **os.environ,
            "PATH": str(self.work) + os.pathsep + os.environ["PATH"],
            "CALL_LOG": str(self.log),
        }

    def run_deploy(self, *args):
        return subprocess.run(
            ["bash", str(SCRIPT), "--kubeconfig", str(self.kube),
             "--context", "explicit-test-context", "--values", str(self.values), *args],
            env=self.env, text=True, capture_output=True, check=False,
        )

    def calls(self):
        return [json.loads(line) for line in self.log.read_text().splitlines()] if self.log.exists() else []

    def test_docker_push_precedes_digest_pinned_helm_install(self):
        result = self.run_deploy("--engine", "docker", "--tag", "test")
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = self.calls()
        build = next(i for i, c in enumerate(calls) if c[:3] == ["docker", "buildx", "build"])
        install = next(i for i, c in enumerate(calls) if c[:3] == ["helm", "upgrade", "--install"])
        self.assertLess(build, install)
        self.assertIn("--push", calls[build])
        self.assertIn("image.digest=" + DIGEST, calls[install])
        self.assertIn("explicit-test-context", calls[install])
        self.assertFalse(any("delete" in c for c in calls))

    def test_podman_uses_registry_digest(self):
        result = self.run_deploy("--engine", "podman")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(any(c[:2] == ["podman", "push"] for c in self.calls()))
        install = next(c for c in self.calls() if c[:3] == ["helm", "upgrade", "--install"])
        self.assertIn("image.digest=" + DIGEST, install)

    def test_publish_failure_does_not_install(self):
        self.env["FAIL_TOOL"] = "docker"
        self.env["FAIL_ARG"] = "--push"
        result = self.run_deploy("--engine", "docker")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(c[:2] == ["helm", "upgrade"] for c in self.calls()))

    def test_bad_values_do_not_build_or_publish(self):
        self.env["FAIL_TOOL"] = "helm"
        self.env["FAIL_ARG"] = "template"
        result = self.run_deploy("--engine", "docker")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(c[:3] == ["docker", "buildx", "build"] for c in self.calls()))
        self.assertFalse(any(c[:2] == ["helm", "upgrade"] for c in self.calls()))

    def test_rejects_bad_options_before_mutation(self):
        for args in [
            ["--image", "ghcr.io/user/image:tag"], ["--engine", "unknown"],
            ["--namespace", "Bad"], ["--platform", "windows/amd64"], ["--tag"],
        ]:
            with self.subTest(args=args):
                result = self.run_deploy(*args)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(self.calls(), [])

    def test_requires_explicit_context(self):
        result = subprocess.run(["bash", str(SCRIPT), "--values", str(self.values)],
                                env=self.env, capture_output=True, check=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.calls(), [])


if __name__ == "__main__":
    unittest.main()
