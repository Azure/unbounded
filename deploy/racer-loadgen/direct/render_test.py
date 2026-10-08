"""Offline render checks. Requires kubectl and PyYAML; never contacts a cluster."""

import pathlib
import shutil
import subprocess
import unittest

import yaml


@unittest.skipUnless(shutil.which("kubectl"), "kubectl is required to render")
class DirectRenderTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        direct = pathlib.Path(__file__).resolve().parent

        def render(path, *flags):
            result = subprocess.run(
                [
                    "timeout", "--signal=TERM", "--kill-after=10s", "60s",
                    "kubectl", "kustomize", *flags, str(path),
                ],
                check=True,
                capture_output=True,
                text=True,
                timeout=75,
            )
            return list(yaml.safe_load_all(result.stdout))

        cls.base = render(direct.parent)
        cls.direct = render(direct, "--load-restrictor", "LoadRestrictionsNone")
        cls.daemonset = next(r for r in cls.direct if r["kind"] == "DaemonSet")
        cls.pod = cls.daemonset["spec"]["template"]["spec"]
        cls.container = cls.pod["containers"][0]

    def test_resources_and_scope(self):
        self.assertCountEqual(
            [(r["kind"], r["metadata"]["name"]) for r in self.direct],
            [
                ("ClusterCache", "racer-loadgen"),
                ("DaemonSet", "racer-loadgen"),
                ("Service", "racer-loadgen-metrics"),
            ],
        )
        for resource in self.direct:
            if resource["kind"] == "ClusterCache":
                self.assertEqual(resource["apiVersion"], "racer.unbounded-cloud.io/v1alpha1")
                self.assertNotIn("namespace", resource["metadata"])
                self.assertNotIn("spec", resource)
            else:
                self.assertEqual(resource["metadata"]["namespace"], "unbounded-system")

    def test_direct_args_and_memory(self):
        args = dict(arg[2:].split("=", 1) for arg in self.container["args"])
        for key, value in {
            "backend": "uds",
            "volume": "racer-loadgen",
            "catalog-blobs": "128",
            "blob-bytes": "67108864",
            "concurrency": "8",
            "blob-concurrency": "1",
            "seed": "benchmark-v1",
            "verify": "true",
        }.items():
            self.assertEqual(args[key], value)
        for incompatible in ("cache", "target", "namespace", "layers", "layer-bytes", "jitter", "catalog-images"):
            self.assertNotIn(incompatible, args)
        self.assertEqual(self.container["resources"]["requests"]["memory"], "256Mi")
        self.assertEqual(self.container["image"], "racer-loadgen:dev")

    def test_security_and_mount_boundary(self):
        self.assertEqual(len(self.pod["containers"]), 1)
        self.assertNotIn("initContainers", self.pod)
        self.assertFalse(self.pod["automountServiceAccountToken"])
        self.assertEqual(self.pod["securityContext"], {
            "runAsUser": 0,
            "runAsGroup": 0,
            "runAsNonRoot": False,
            "seccompProfile": {"type": "RuntimeDefault"},
        })
        self.assertEqual(self.container["securityContext"], {
            "allowPrivilegeEscalation": False,
            "readOnlyRootFilesystem": True,
            "capabilities": {"drop": ["ALL"]},
        })
        self.assertEqual(self.pod["volumes"], [{
            "name": "racer-sockets",
            "hostPath": {"path": "/run/racer/racer-loadgen", "type": "DirectoryOrCreate"},
        }])
        self.assertEqual(self.container["volumeMounts"], [{
            "name": "racer-sockets", "mountPath": "/run/racer/racer-loadgen",
        }])

    def test_inherited_probes_metrics_and_unchanged_base(self):
        base_ds = next(r for r in self.base if r["kind"] == "DaemonSet")
        base_pod = base_ds["spec"]["template"]
        base_container = base_pod["spec"]["containers"][0]
        for probe in ("startupProbe", "readinessProbe", "livenessProbe"):
            self.assertEqual(self.container[probe], base_container[probe])
            self.assertEqual(self.container[probe]["httpGet"]["port"], "metrics")
        self.assertEqual(self.daemonset["spec"]["template"]["metadata"], base_pod["metadata"])
        self.assertEqual(self.container["ports"], [{"name": "metrics", "containerPort": 9090}])
        service = next(r for r in self.direct if r["kind"] == "Service")
        self.assertEqual(service["spec"]["selector"], base_ds["spec"]["selector"]["matchLabels"])
        self.assertEqual(service["spec"]["ports"][0]["targetPort"], "metrics")
        self.assertCountEqual(
            [r["metadata"]["name"] for r in self.base if r["kind"] == "Service"],
            ["racer-loadgen-origin", "racer-loadgen-gantry"],
        )
        self.assertIn("--target=http://racer-loadgen-gantry:5000", base_container["args"])
        self.assertTrue(base_pod["spec"]["securityContext"]["runAsNonRoot"])
        self.assertNotIn("volumes", base_pod["spec"])

    def test_base_service_routing(self):
        services = {r["metadata"]["name"]: r for r in self.base if r["kind"] == "Service"}
        for service in services.values():
            self.assertEqual(service["metadata"]["namespace"], "unbounded-system")
            self.assertEqual(service["spec"]["internalTrafficPolicy"], "Local")
        self.assertEqual(services["racer-loadgen-origin"]["spec"]["selector"], {
            "app.kubernetes.io/name": "racer-loadgen",
        })
        self.assertEqual(services["racer-loadgen-origin"]["spec"]["ports"][0]["targetPort"], "origin")
        self.assertEqual(services["racer-loadgen-gantry"]["spec"]["selector"], {
            "app.kubernetes.io/name": "gantry",
            "app.kubernetes.io/component": "agent",
        })
        self.assertEqual(services["racer-loadgen-gantry"]["spec"]["ports"][0]["targetPort"], 5000)

    def test_direct_requires_explicit_load_permission(self):
        result = subprocess.run(
            [
                "timeout", "--signal=TERM", "--kill-after=10s", "60s",
                "kubectl", "kustomize", str(pathlib.Path(__file__).resolve().parent),
            ],
            capture_output=True,
            text=True,
            timeout=75,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("security", result.stderr.lower())


if __name__ == "__main__":
    unittest.main()
