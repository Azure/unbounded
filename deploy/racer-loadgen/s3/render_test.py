"""Offline deployment and image resolver checks; requires kubectl and PyYAML."""

import os
import pathlib
import subprocess
import tempfile
import unittest

import yaml


HERE = pathlib.Path(__file__).resolve().parent
ROOT = HERE.parents[2]


def run(*args, **kwargs):
    return subprocess.run(
        ["timeout", "--signal=TERM", "--kill-after=10s", "60s", *args],
        capture_output=True, text=True, timeout=75, **kwargs,
    )


def flags(container):
    return dict(arg[2:].split("=", 1) for arg in container["args"] if arg.startswith("--"))


class S3RenderTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        result = run("kubectl", "kustomize", str(HERE), check=True)
        cls.resources = {
            (r["kind"], r["metadata"]["name"]): r
            for r in yaml.safe_load_all(result.stdout)
        }
        cls.origin = cls.resources["DaemonSet", "racer-loadgen"]
        cls.consumer = cls.resources["DaemonSet", "racer-s3-loadgen"]

    def test_scope_and_resource_inventory(self):
        self.assertEqual(set(self.resources), {
            ("ClusterVolume", "racer-object"),
            ("ConfigMap", "racer-s3-loadgen-control"),
            ("DaemonSet", "racer-loadgen"),
            ("DaemonSet", "racer-s3-loadgen"),
            ("Service", "racer-loadgen-origin"),
            ("Service", "racer-s3-loadgen-metrics"),
        })
        for (kind, _), resource in self.resources.items():
            if kind == "ClusterVolume":
                self.assertEqual(resource, {
                    "apiVersion": "racer.unbounded-cloud.io/v1alpha1",
                    "kind": kind, "metadata": {"name": "racer-object"},
                    "spec": {"type": "Cache"},
                })
            else:
                self.assertEqual(resource["metadata"]["namespace"], "unbounded-system")

    def test_selectors_and_rollout(self):
        old = yaml.safe_load((HERE.parent / "daemonset.yaml").read_text())
        self.assertEqual(self.origin["spec"]["selector"], old["spec"]["selector"])
        for ds in (self.origin, self.consumer):
            self.assertEqual(ds["spec"]["selector"]["matchLabels"], ds["spec"]["template"]["metadata"]["labels"])
            self.assertEqual(ds["spec"]["updateStrategy"]["rollingUpdate"], {"maxUnavailable": 1, "maxSurge": 0})
            pod = ds["spec"]["template"]["spec"]
            self.assertEqual(pod["affinity"]["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"]["nodeSelectorTerms"], [{
                "matchExpressions": [{"key": "racer.unbounded-cloud.io/exclude", "operator": "DoesNotExist"}],
            }])
        self.assertEqual(self.origin["spec"]["template"]["spec"]["nodeSelector"], {"kubernetes.io/os": "linux"})
        self.assertEqual(self.consumer["spec"]["template"]["spec"]["nodeSelector"], {
            "kubernetes.io/os": "linux", "racer-s3-benchmark": "enabled",
        })

    def test_catalog_and_transport(self):
        origin, adapter = self.origin["spec"]["template"]["spec"]["containers"]
        consumer, sidecar = self.consumer["spec"]["template"]["spec"]["containers"]
        for c in (origin, consumer):
            f = flags(c)
            for key, value in {
                "backend": "s3", "bucket": "benchmark", "object-count": "16",
                "object-bytes": "67108864", "seed": "benchmark-v1", "concurrency": "0",
                "verify": "true", "startup-timeout": "4m", "retry-delay": "1s",
                "metrics-listen": ":9090",
            }.items():
                self.assertEqual(f[key], value)
            for incompatible in ("target", "namespace", "cache", "volume", "layers", "layer-bytes", "catalog-blobs", "blob-concurrency"):
                self.assertNotIn(incompatible, f)
        self.assertEqual(flags(origin)["s3-origin"], "true")
        self.assertEqual(flags(origin)["listen"], ":8080")
        self.assertNotIn("concurrency-file", flags(origin))
        self.assertNotIn("volumeMounts", origin)
        self.assertEqual(flags(consumer)["s3-origin"], "false")
        self.assertEqual(flags(consumer)["endpoint"], "http://127.0.0.1:8080")
        self.assertEqual(flags(sidecar)["listen"], "127.0.0.1:8080")
        self.assertEqual(flags(adapter)["endpoint"], "http://127.0.0.1:8080")
        for c, mode in ((adapter, "origin"), (sidecar, "sidecar")):
            self.assertEqual(c["args"][0], mode)
            self.assertNotIn("cache", flags(c))
            for key, value in {"volume": "racer-object", "namespace": "s3-benchmark-v1", "bucket": "benchmark"}.items():
                self.assertEqual(flags(c)[key], value)
        self.assertEqual(adapter["env"], [
            {"name": "AWS_ACCESS_KEY_ID", "value": "synthetic-benchmark-only"},
            {"name": "AWS_SECRET_ACCESS_KEY", "value": "synthetic-benchmark-only"},
            {"name": "AWS_EC2_METADATA_DISABLED", "value": "true"},
        ])
        for c in (origin, consumer, sidecar):
            self.assertNotIn("env", c)

    def test_live_control_and_socket_boundaries(self):
        self.assertEqual(self.resources["ConfigMap", "racer-s3-loadgen-control"]["data"], {"concurrency": "0"})
        opod = self.origin["spec"]["template"]["spec"]
        cpod = self.consumer["spec"]["template"]["spec"]
        self.assertEqual(flags(cpod["containers"][0])["concurrency-file"], "/etc/loadgen-control/concurrency")
        self.assertEqual(cpod["containers"][0]["volumeMounts"], [{
            "name": "control", "mountPath": "/etc/loadgen-control", "readOnly": True,
        }])
        self.assertEqual(cpod["volumes"][0], {"name": "control", "configMap": {
            "name": "racer-s3-loadgen-control", "items": [{"key": "concurrency", "path": "concurrency"}],
        }})
        self.assertEqual(len(opod["volumes"]), 1)
        self.assertEqual(len(cpod["volumes"]), 2)
        for pod, role, path_type, read_only in ((opod, "origin", "DirectoryOrCreate", False), (cpod, "client", "Directory", True)):
            path = "/run/racer/racer-object/" + role
            self.assertEqual(pod["volumes"][-1], {"name": role, "hostPath": {"path": path, "type": path_type}})
            mount = pod["containers"][1]["volumeMounts"][0]
            self.assertEqual(mount["mountPath"], path)
            self.assertEqual(mount.get("readOnly", False), read_only)

    def test_security_resources_and_probes(self):
        for ds in (self.origin, self.consumer):
            pod = ds["spec"]["template"]["spec"]
            self.assertFalse(pod["automountServiceAccountToken"])
            for field in ("hostNetwork", "hostPID", "hostIPC", "initContainers"):
                self.assertFalse(pod.get(field))
            self.assertEqual(pod["securityContext"]["seccompProfile"]["type"], "RuntimeDefault")
            for i, c in enumerate(pod["containers"]):
                sc = c["securityContext"]
                self.assertEqual(sc["runAsUser"], 65532 if i == 0 else 0)
                self.assertEqual(sc["runAsGroup"], sc["runAsUser"])
                self.assertEqual(sc["runAsNonRoot"], i == 0)
                self.assertFalse(sc["privileged"])
                self.assertFalse(sc["allowPrivilegeEscalation"])
                self.assertTrue(sc["readOnlyRootFilesystem"])
                self.assertEqual(sc["capabilities"], {"drop": ["ALL"]})
                self.assertNotIn("envFrom", c)
                self.assertEqual(c["resources"]["limits"], {"memory": "1Gi"})
                self.assertEqual(c["resources"]["requests"]["memory"], "128Mi")
                self.assertEqual(c["image"], "ghcr.io/azure/" + ("racer-loadgen" if i == 0 else "racer-object") + ":dev")
            c = pod["containers"][0]
            for probe, path in (("startupProbe", "/readyz"), ("readinessProbe", "/readyz"), ("livenessProbe", "/healthz")):
                self.assertEqual(c[probe]["httpGet"], {"path": path, "port": "metrics"})
            self.assertGreater(c["startupProbe"]["failureThreshold"] * c["startupProbe"]["periodSeconds"], 240)

    def test_services_do_not_expose_sidecar(self):
        old = yaml.safe_load((HERE.parent / "service-origin.yaml").read_text())
        self.assertEqual(self.resources["Service", "racer-loadgen-origin"]["spec"], old["spec"])
        svc = self.resources["Service", "racer-s3-loadgen-metrics"]["spec"]
        self.assertEqual(svc["selector"], self.consumer["spec"]["selector"]["matchLabels"])
        self.assertEqual(svc["ports"], [{"name": "metrics", "port": 9090, "targetPort": "metrics", "protocol": "TCP"}])

    def test_published_image_overlay(self):
        with tempfile.TemporaryDirectory(dir=HERE) as tmp:
            overlay = pathlib.Path(tmp)
            base = overlay / "base"
            base.mkdir()
            for source in HERE.glob("*.yaml"):
                (base / source.name).write_text(source.read_text())
            (overlay / "kustomization.yaml").write_text(yaml.safe_dump({
                "apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization",
                "resources": ["base"],
                "images": [
                    {"name": "ghcr.io/azure/" + image, "newTag": "build-sha"}
                    for image in ("racer-loadgen", "racer-object")
                ],
            }))
            rendered = run("kubectl", "kustomize", str(overlay))
            self.assertEqual(rendered.returncode, 0, rendered.stderr)
            images = [
                c["image"] for r in yaml.safe_load_all(rendered.stdout)
                if r["kind"] == "DaemonSet" for c in r["spec"]["template"]["spec"]["containers"]
            ]
            self.assertCountEqual(images, [
                "ghcr.io/azure/" + image + ":build-sha"
                for image in ("racer-loadgen", "racer-object") for _ in range(2)
            ])


class ImageResolverTest(unittest.TestCase):
    def test_containerfile_first_fallback_and_failure(self):
        workflow = yaml.safe_load((ROOT / ".github/workflows/images.yaml").read_text())
        script = next(s["run"] for s in workflow["jobs"]["resolve"]["steps"] if s.get("id") == "resolve")
        for files, event, image, expected, status in (
            (["Containerfile"], "workflow_dispatch", "racer-loadgen", "Containerfile", 0),
            (["Dockerfile"], "workflow_dispatch", "racer-object", "Dockerfile", 0),
            (["Containerfile", "Dockerfile"], "push", "racer-object", "Containerfile", 0),
            ([], "workflow_dispatch", "missing", None, 1),
            ([], "push", "playpen-test", None, 0),
        ):
            with self.subTest(files=files, event=event, image=image), tempfile.TemporaryDirectory(dir=HERE) as tmp:
                root = pathlib.Path(tmp)
                image_dir = root / "images" / image
                image_dir.mkdir(parents=True)
                for file in files:
                    (image_dir / file).touch()
                output = root / "output"
                output.touch()
                rendered = script
                for expression, value in {
                    "github.event_name": event, "github.ref_name": "images/" + image + "/v1",
                    "inputs.image": image, "github.sha": "test-sha",
                }.items():
                    rendered = rendered.replace("${{ " + expression + " }}", value)
                result = run("bash", "-e", "-c", rendered, cwd=root, env={**os.environ, "GITHUB_OUTPUT": str(output), "INPUT_PLATFORMS": ""})
                self.assertEqual(result.returncode, status, result.stderr)
                values = dict(line.split("=", 1) for line in output.read_text().splitlines())
                if expected:
                    self.assertEqual(values["file"], "images/" + image + "/" + expected)
                    self.assertEqual(values["should_build"], "true")
                    self.assertEqual(values["platforms"], "linux/amd64,linux/arm64")
                elif status:
                    self.assertIn("No Containerfile or Dockerfile", result.stdout)
                else:
                    self.assertEqual(values, {"should_build": "false"})


if __name__ == "__main__":
    unittest.main()
