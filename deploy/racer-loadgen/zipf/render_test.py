"""Offline contract checks; requires kubectl and PyYAML."""

import copy
import json
import pathlib
import subprocess
import unittest

import yaml

from override_patch import KEY, make_patch

ROOT = pathlib.Path(__file__).resolve().parent
SHA = "f9a088a22a29cf8549e1945af833f508a14199d1"


class OverlayTest(unittest.TestCase):
    def test_paused_render(self):
        result = subprocess.run(
            ["timeout", "--signal=TERM", "--kill-after=10s", "60s", "kubectl",
             "kustomize", "--load-restrictor", "LoadRestrictionsNone", str(ROOT.parent / "zipf-paused")],
            check=True, capture_output=True, text=True, timeout=75,
        )
        objects = list(yaml.safe_load_all(result.stdout))
        cm = next(o for o in objects if o["kind"] == "ConfigMap")
        self.assertEqual(cm["data"]["concurrency"], "0")

    def test_render(self):
        result = subprocess.run(
            ["timeout", "--signal=TERM", "--kill-after=10s", "60s", "kubectl",
             "kustomize", "--load-restrictor", "LoadRestrictionsNone", str(ROOT)],
            check=True, capture_output=True, text=True, timeout=75,
        )
        objects = list(yaml.safe_load_all(result.stdout))
        self.assertCountEqual([o["kind"] for o in objects],
                              ["ConfigMap", "Service", "DaemonSet", "ClusterVolume"])
        ds = next(o for o in objects if o["kind"] == "DaemonSet")
        pod = ds["spec"]["template"]["spec"]
        container = pod["containers"][0]
        args = dict(a[2:].split("=", 1) for a in container["args"])
        for key, value in {"backend": "uds", "volume": "racer-loadgen",
                           "catalog-blobs": "512", "blob-bytes": "2147483648",
                           "seed": "zipf-balanced-v1", "startup-timeout": "60m",
                           "profile": "zipf", "zipf-exponent": "0.5",
                           "verify": "false", "concurrency": "16",
                           "blob-concurrency": "1"}.items():
            self.assertEqual(args[key], value)
        self.assertEqual(container["image"], "ghcr.io/azure/racer-loadgen:" + SHA)
        self.assertEqual(pod["nodeSelector"]["agentpool"], "ddsv6")
        self.assertEqual(pod["terminationGracePeriodSeconds"], 0)
        self.assertEqual(ds["spec"]["updateStrategy"]["rollingUpdate"]["maxUnavailable"], "100%")
        self.assertNotIn("limits", container["resources"])
        self.assertNotIn("duration", args)
        self.assertEqual(container["startupProbe"]["httpGet"]["path"], "/readyz")
        self.assertEqual(int(args["catalog-blobs"]) * int(args["blob-bytes"]), 2**40)
        self.assertEqual(container["startupProbe"]["failureThreshold"], 390)
        self.assertEqual(container["startupProbe"]["periodSeconds"], 10)
        probe_budget = (container["startupProbe"]["failureThreshold"]
                        * container["startupProbe"]["periodSeconds"])
        self.assertGreater(probe_budget, int(args["startup-timeout"][:-1]) * 60)
        self.assertLessEqual(probe_budget, 65 * 60)
        self.assertEqual(args["concurrency-file"], "/etc/racer-loadgen-control/concurrency")
        self.assertTrue(any(v.get("configMap", {}).get("name") == "racer-loadgen-control"
                            for v in pod["volumes"]))
        cm = next(o for o in objects if o["kind"] == "ConfigMap")
        self.assertEqual(cm["data"]["concurrency"], "16")
        self.assertEqual(args["concurrency"], cm["data"]["concurrency"])
        volume = next(o for o in objects if o["kind"] == "ClusterVolume")
        self.assertNotIn("namespace", volume["metadata"])

    def test_scoped_patch_preserves_other_keys(self):
        current = {"kind": "ConfigMap", "metadata": {
            "name": "unbounded-component-overrides", "namespace": "unbounded-system",
            "resourceVersion": "123"}, "data": {"overrides.yaml": "net entries"}}
        before = copy.deepcopy(current)
        patch = make_patch(current, "racer entries")
        self.assertEqual(current, before)
        self.assertEqual(patch["data"], {KEY: "racer entries"})
        self.assertEqual(patch["metadata"]["resourceVersion"], "123")
        current["data"].update(patch["data"])
        self.assertEqual(current["data"]["overrides.yaml"], "net entries")
        self.assertEqual(make_patch(current, "racer entries"), patch)
        with self.assertRaises(ValueError):
            make_patch(current, "different entries")
        current["metadata"]["namespace"] = "other"
        with self.assertRaises(ValueError):
            make_patch(current, "racer entries")

    def test_racer_only_overrides(self):
        entries = yaml.safe_load((ROOT / "racer-overrides.yaml").read_text())["overrides"]
        self.assertEqual(len(entries), 2)
        for entry in entries:
            self.assertEqual(entry["component"], "racer")
            pod = entry["patch"]["spec"]["template"]["spec"]
            container = pod["containers"][0]
            self.assertTrue(container["image"].endswith(":" + SHA))
            self.assertNotIn("resources", container)
        dp = entries[1]["patch"]["spec"]
        self.assertEqual(dp["template"]["spec"]["nodeSelector"], {"agentpool": "ddsv6"})
        self.assertEqual(dp["updateStrategy"]["rollingUpdate"]["maxSurge"], 0)
        env = {e["name"]: e["value"] for e in dp["template"]["spec"]["containers"][0]["env"]}
        for key, value in {"RACER_PLAINTEXT_BYTES": "34359738368",
                           "RACER_CIPHERTEXT_BYTES": "68719476736",
                           "RACER_DIRTY_BYTES": "2147483648",
                           "RACER_REGISTERED_BYTES": "2147483648",
                           "RACER_REQUEST_CONTEXT_BYTES": "1073741824",
                           "RACER_FLIGHTS": "2048",
                           "RACER_QUEUE_ENTRIES": "16384",
                           "RACER_CLIENT_CONNECTIONS": "16384",
                           "RACER_PIPES": "1024",
                           "RACER_RELAY_TRANSFERS": "1024",
                           "RACER_CONNECTIONS_PER_NEIGHBOR": "8",
                           "RACER_PEER_INFLIGHT_MAX": "1024",
                           "RACER_PEER_PER_NEIGHBOR_MAX": "128",
                           "RACER_ACTIVE_PATH_SEARCHES": "64",
                           "RACER_ADMISSION_MODE": "second-sight"}.items():
            self.assertEqual(env[key], value)
        self.assertNotIn("RACER_MAX_THREADS", env)
        self.assertEqual(int(env["RACER_PLAINTEXT_BYTES"]) // 21, 1636178017)
        self.assertEqual(int(env["RACER_CIPHERTEXT_BYTES"]) // 21, 3272356035)
        self.assertEqual((int(env["RACER_CLIENT_CONNECTIONS"]) // 21) // 4, 195)
        for key in ("RACER_PLACEMENT_CACHE_BYTES", "RACER_CACHED_PATHS",
                    "RACER_PATH_CACHE_BYTES", "RACER_RETAINED_SNAPSHOTS"):
            self.assertNotIn(key, env)

    def test_dashboard_workload_contract(self):
        dashboard = json.loads((ROOT.parent.parent / "racer/grafana-direct-zipf.json").read_text())
        description = dashboard["description"]
        for value in ("512 x 2147483648-byte", "zipf-balanced-v1", "exponent 0.5",
                      "32 GiB plaintext", "64 GiB ciphertext",
                      "2 GiB each for dirty and registered"):
            self.assertIn(value, description)
        for panel in dashboard["panels"][:2]:
            expression = panel["targets"][0]["expr"]
            self.assertIn("* 2147483648 * 8 / 1e9", expression)
            self.assertIn('result="success"', expression)


if __name__ == "__main__":
    unittest.main()
