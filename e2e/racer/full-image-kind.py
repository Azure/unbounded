#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Finite eight-layer image regression against an explicitly selected e2e kind cluster."""

import argparse
import json
import os
import pathlib
import signal
import subprocess
import tempfile
import time

ROOT = pathlib.Path(__file__).resolve().parents[2]
NAMESPACE = "unbounded-system"


def command(args, timeout=30, data=None, env=None):
    result = subprocess.run(args, input=data, text=True, capture_output=True,
                            timeout=timeout, env=env)
    if result.returncode:
        raise RuntimeError(f"{args}: {result.stdout}\n{result.stderr}")
    return result.stdout


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--kubeconfig", required=True)
    parser.add_argument("--concurrency", type=int, default=64, choices=range(1, 65))
    parser.add_argument("--seed", default="benchmark-v1")
    parser.add_argument("--direct-origin", action="store_true")
    args = parser.parse_args()
    signal.alarm(900)

    def kube(*params, timeout=30, data=None):
        return command(["kubectl", "--kubeconfig", args.kubeconfig,
                        "--request-timeout=20s", "-n", NAMESPACE, *params], timeout, data)

    context = kube("config", "current-context").strip()
    if not context.startswith("kind-racer-e2e-"):
        raise RuntimeError(f"not a retained Racer e2e kind context: {context}")
    cluster = context.removeprefix("kind-")
    if cluster not in command(["kind", "get", "clusters"]).splitlines():
        raise RuntimeError(f"local kind cluster not found: {cluster}")

    root_tmp = ROOT / "tmp"
    root_tmp.mkdir(exist_ok=True)
    out = pathlib.Path(tempfile.mkdtemp(prefix="racer-full-image-", dir=root_tmp))
    print(f"Evidence: {out}", flush=True)
    env = dict(os.environ, CGO_ENABLED="0")
    command(["go", "test", "-c", "-o", str(out / "full-image.test"),
             "./cmd/racer-loadgen"], timeout=600, env=env)
    (out / "Dockerfile").write_text(
        'FROM scratch\nCOPY full-image.test /full-image.test\n'
        'ENTRYPOINT ["/full-image.test", "-test.run=^TestFullImageDiagnostic$", '
        '"-test.v", "-test.timeout=115s"]\n')
    image = "full-image-probe:" + out.name
    command(["docker", "build", "-t", image, str(out)], timeout=120)
    command(["kind", "load", "docker-image", image, "--name", cluster], timeout=120)
    name = out.name
    origin_name, mirror_name = name + "-origin", name + "-mirror"
    config = json.loads(kube("get", "cm", "gantry-racer-e2e", "-o", "json"))["data"]["config.yaml"]
    if "loadgen.invalid" in config:
        raise RuntimeError("loadgen.invalid is already configured; restore the previous experiment first")
    (out / "original-config.txt").write_text(config)
    (out / "args.json").write_text(json.dumps(vars(args), indent=2))

    def apply(obj):
        kube("apply", "-f", "-", data=json.dumps(obj))

    def configure(value):
        kube("patch", "cm", "gantry-racer-e2e", "--type=merge", "-p",
             json.dumps({"data": {"config.yaml": value}}))
        kube("rollout", "restart", "ds/gantry-racer-e2e")
        kube("rollout", "status", "ds/gantry-racer-e2e", "--timeout=60s", timeout=70)

    try:
        for service, selector, port in [
            (origin_name, {"app": name}, 8080),
            (mirror_name, {"app": "gantry-racer-e2e"}, 5000),
        ]:
            apply({"apiVersion": "v1", "kind": "Service", "metadata": {"name": service},
                   "spec": {"selector": selector, "ports": [{"port": port}]}})
        configure(config + f"  - name: loadgen.invalid\n    endpoint: http://{origin_name}:8080\n")
        gantry = json.loads(kube("get", "pods", "-l", "app=gantry-racer-e2e", "-o", "json"))
        node = gantry["items"][0]["spec"]["nodeName"]
        target = "http://127.0.0.1:8080" if args.direct_origin else f"http://{mirror_name}:5000"
        apply({"apiVersion": "v1", "kind": "Pod", "metadata": {"name": name, "labels": {"app": name}},
               "spec": {"restartPolicy": "Never", "activeDeadlineSeconds": 120,
                        "nodeSelector": {"kubernetes.io/hostname": node},
                        "containers": [{"name": "probe", "image": image, "imagePullPolicy": "Never",
                                        "env": [{"name": "FULL_IMAGE_TARGET", "value": target},
                                                {"name": "FULL_IMAGE_CONCURRENCY", "value": str(args.concurrency)},
                                                {"name": "FULL_IMAGE_SEED", "value": args.seed}]}]}})
        deadline = time.monotonic() + 140
        phase = None
        while time.monotonic() < deadline:
            pod = json.loads(kube("get", "pod", name, "-o", "json"))
            phase = pod["status"]["phase"]
            if phase in ("Succeeded", "Failed"):
                break
            time.sleep(2)
        logs = kube("logs", name)
        (out / "probe.log").write_text(logs)
        print(logs, flush=True)
        for ds in ["gantry-racer-e2e", "racer-dataplane"]:
            (out / f"{ds}.log").write_text(kube("logs", f"ds/{ds}", "--since=5m"))
        (out / "pods.json").write_text(kube("get", "pods", "-o", "json"))
        if phase != "Succeeded":
            raise RuntimeError(f"full-image regression failed: {phase}; see {out}")
    finally:
        configure(config)
        kube("delete", "pod", name, "--ignore-not-found", "--timeout=20s")
        kube("delete", "svc", origin_name, mirror_name, "--ignore-not-found", "--timeout=20s")


if __name__ == "__main__":
    main()
