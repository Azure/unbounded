# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]


class ImagesWorkflowTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        # BaseLoader preserves GitHub's "on" key rather than YAML 1.1's boolean.
        cls.workflow = yaml.load(
            (ROOT / ".github/workflows/images.yaml").read_text(),
            Loader=yaml.BaseLoader,
        )
        cls.resolve = next(
            step for step in cls.workflow["jobs"]["resolve"]["steps"]
            if step.get("id") == "resolve"
        )

    def test_input_and_build_argument_contract(self):
        inputs = self.workflow["on"]["workflow_dispatch"]["inputs"]
        self.assertEqual(inputs["heap_profiling"]["type"], "boolean")
        self.assertEqual(inputs["heap_profiling"]["default"], "false")
        self.assertEqual(inputs["heap_profiling"]["required"], "false")
        self.assertEqual(self.resolve["env"]["INPUT_HEAP_PROFILING"],
                         "${{ inputs.heap_profiling || false }}")
        self.assertEqual(
            self.workflow["jobs"]["resolve"]["outputs"]["heap_profiling"],
            "${{ steps.resolve.outputs.heap_profiling }}",
        )
        build = next(step for step in self.workflow["jobs"]["build"]["steps"]
                     if step.get("name") == "Build and push")
        self.assertEqual(build["with"]["build-args"].splitlines(), [
            "VERSION=${{ needs.resolve.outputs.tag }}",
            "CONTAINER_REGISTRY=${{ env.CONTAINER_REGISTRY }}",
            "RACER_HEAP_PROFILING=${{ needs.resolve.outputs.heap_profiling }}",
        ])

    def resolve_image(self, image, heap="false", event="workflow_dispatch",
                      platforms=""):
        script = self.resolve["run"]
        for expression, value in {
            "github.event_name": event,
            "github.ref_name": f"images/{image}/v1.2.3",
            "inputs.image": image,
            "github.sha": "test-commit-sha",
        }.items():
            script = script.replace("${{ " + expression + " }}", value)
        self.assertNotIn("${{", script)
        with tempfile.TemporaryDirectory(dir=ROOT) as directory:
            output = Path(directory) / "output"
            result = subprocess.run(
                ["timeout", "--signal=TERM", "--kill-after=10s", "10s",
                 "bash", "--noprofile", "--norc", "-eo", "pipefail", "-c", script],
                cwd=ROOT, capture_output=True, text=True, timeout=25,
                env={**os.environ, "GITHUB_OUTPUT": str(output),
                     "INPUT_HEAP_PROFILING": heap, "INPUT_PLATFORMS": platforms},
            )
            values = dict(line.split("=", 1) for line in
                          output.read_text().splitlines()) if output.exists() else {}
        return result, values

    def test_defaults_and_opt_in(self):
        for image, heap, event, platforms in [
            ("racer-dataplane", "true", "workflow_dispatch", "linux/amd64"),
            ("racer-dataplane", "false", "workflow_dispatch", ""),
            ("agent-ubuntu2404", "false", "workflow_dispatch", ""),
            ("metalman", "false", "workflow_dispatch", ""),
            ("racer-dataplane", "false", "push", ""),
            ("agent-ubuntu2404", "false", "push", ""),
        ]:
            with self.subTest(image=image, heap=heap, event=event):
                result, outputs = self.resolve_image(image, heap, event, platforms)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual(outputs, {
                    "name": image,
                    "tag": "v1.2.3" if event == "push" else "test-commit-sha",
                    "file": f"images/{image}/Containerfile",
                    "platforms": platforms or "linux/amd64,linux/arm64",
                    "should_build": "true", "heap_profiling": heap,
                })

    def test_rejects_profiling_for_other_images(self):
        for image in ("agent-ubuntu2404", "metalman", "playpen", "racer-controller"):
            with self.subTest(image=image):
                result, outputs = self.resolve_image(image, "true")
                self.assertEqual(result.returncode, 1)
                self.assertIn("heap_profiling is only supported for racer-dataplane",
                              result.stdout)
                self.assertEqual(outputs, {})

    def test_existing_skip_and_missing_image_behavior(self):
        result, outputs = self.resolve_image("playpen", event="push")
        self.assertEqual(result.returncode, 0)
        self.assertEqual(outputs, {"should_build": "false"})
        result, outputs = self.resolve_image("nonexistent-image")
        self.assertEqual(result.returncode, 1)
        self.assertIn("No Containerfile found", result.stdout)
        self.assertEqual(outputs, {})
