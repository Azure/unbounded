#!/usr/bin/env python3
"""Explicit, bounded rollout phases. Requires kubectl, Python 3 and PyYAML."""
import argparse
import copy
import datetime
import json
from pathlib import Path
import signal
import subprocess
import sys

import yaml

NS = "unbounded-system"
IMAGES = {
    "operator": "ghcr.io/azure/unbounded-operator@sha256:e5641181717cab042dc4e7ff00d5a801d97fa1cd3e4f6e69de96f400265f6312",
    "controller": "ghcr.io/azure/racer-controller@sha256:393eae9c7033c28abae246744c4f0d89b147a28b3910cc7ff785721bacff7c1f",
    "dataplane": "ghcr.io/azure/racer-dataplane@sha256:40335701cc4c0b40b2fdc3a7d7588db6da196fd25492b7fc2ccb8476673f4018",
    "gantry": "ghcr.io/azure/gantry@sha256:a0ca623e157c6781ddb9e95d28f93c879b9b0fed293d218f096d456a00a8d565",
    "loadgen": "ghcr.io/azure/racer-loadgen@sha256:df51feac7accb1ad85ee83f34746c8635c068137e6160db659c31db25a911cd3",
}
TOOLS = "ghcr.io/azure/racer-guard@sha256:8d8447cf368e245dcc9e276f1cad311496f2d2f7a4c60c4ebd7102ce0b2ecb09"
VOLUMES = {"guard-launch", "guard-socket", "stage47-code", "stage47-host", "stage47-lock"}
INITS = {"guard-launch-copy", "underlay-guard"}
CLEANUP = "racer-firewall-cleanup"
ACTIVE_PODS = "status.phase!=Succeeded,status.phase!=Failed"
ROLLOUT_TARGETS = (
    ("deploy", "unbounded-operator", "operator", NS),
    # The new operator installs controller wiring and the override image together.
    ("ds", "racer-dataplane", "dataplane", NS),
    ("ds", "racer-dataplane-podnet", "dataplane", NS),
    ("ds", "gantry", "gantry", NS),
    ("ds", "racer-loadgen", "loadgen", NS),
    ("deploy", "racer-loadgen-client", "loadgen", NS),
    ("ds", "racer-loadgen", "loadgen", "racer-loadgen"),
)


def guard_key(key):
    return key.startswith(("RACER_", "UNBOUNDED_RACER_")) and "GUARD" in key


def sanitize_pod(pod):
    for field, names in (("volumes", VOLUMES), ("initContainers", INITS)):
        if field in pod:
            pod[field] = [x for x in pod[field] if x["name"] not in names]
    for container in pod.get("containers", []) + pod.get("initContainers", []):
        if container.get("command") == ["/guard-launch/racer-guard-launch"]:
            del container["command"]
        if "volumeMounts" in container:
            container["volumeMounts"] = [x for x in container["volumeMounts"] if x["name"] not in VOLUMES]
        if "env" in container:
            container["env"] = [x for x in container["env"] if not guard_key(x["name"])]


def strategy(spec, kind):
    if kind == "DaemonSet":
        spec["updateStrategy"] = {"type": "RollingUpdate", "rollingUpdate": {"maxSurge": 0, "maxUnavailable": "10%"}}


def transform_overrides(data):
    result = dict(data)
    found = set()
    for key, text in data.items():
        doc = yaml.safe_load(text)
        changed = False
        for entry in doc.get("overrides", []):
            component, kind = entry.get("component"), entry.get("kind")
            if component not in ("racer", "gantry"):
                continue
            spec = entry["patch"]["spec"]
            pod = spec["template"]["spec"]
            sanitize_pod(pod)
            if "addInitContainers" in entry:
                entry["addInitContainers"] = [x for x in entry["addInitContainers"] if x not in INITS]
            role = "gantry" if component == "gantry" else ("controller" if kind == "Deployment" else "dataplane")
            matches = [c for c in pod["containers"] if c["name"] == role]
            if len(matches) != 1:
                raise ValueError(f"{key}: expected one {role} container")
            matches[0]["image"] = IMAGES[role]
            strategy(spec, kind)
            found.add((component, kind, entry.get("name", "")))
            changed = True
        if changed:
            result[key] = yaml.safe_dump(doc, sort_keys=False)
    required = {("racer", "Deployment", ""), ("racer", "DaemonSet", "racer-dataplane"),
                ("racer", "DaemonSet", "racer-dataplane-podnet"), ("gantry", "DaemonSet", "")}
    if not required <= found:
        raise ValueError(f"live override layout changed; missing {required - found}")
    return result


def cleanup_manifest():
    security = {"runAsUser": 0, "runAsGroup": 0, "allowPrivilegeEscalation": False,
                "readOnlyRootFilesystem": True, "capabilities": {"drop": ["ALL"], "add": ["NET_ADMIN", "NET_RAW", "SYS_CHROOT"]}}
    return {"apiVersion": "apps/v1", "kind": "DaemonSet", "metadata": {"name": CLEANUP, "namespace": NS}, "spec": {
        "selector": {"matchLabels": {"app": CLEANUP}}, "template": {
            "metadata": {"labels": {"app": CLEANUP}}, "spec": {
                "hostNetwork": True, "dnsPolicy": "ClusterFirstWithHostNet", "automountServiceAccountToken": False,
                "nodeSelector": {"kubernetes.io/os": "linux"}, "tolerations": [{"operator": "Exists"}],
                "initContainers": [{"name": "cleanup", "image": TOOLS, "securityContext": security,
                    "command": ["timeout", "--signal=TERM", "--kill-after=10s", "240s", "python3", "-B", "-c", Path(__file__).with_name("firewall.py").read_text()],
                    "resources": {"requests": {"cpu": "10m", "memory": "32Mi"}, "limits": {"cpu": "250m", "memory": "128Mi"}},
                    "volumeMounts": [{"name": "host", "mountPath": "/host", "readOnly": True}, {"name": "lock", "mountPath": "/host/run/xtables.lock"}]}],
                "containers": [{"name": "idle", "image": TOOLS, "command": ["sleep", "infinity"],
                    "securityContext": {"runAsUser": 65532, "runAsNonRoot": True, "allowPrivilegeEscalation": False,
                                        "readOnlyRootFilesystem": True, "capabilities": {"drop": ["ALL"]}},
                    "resources": {"requests": {"cpu": "1m", "memory": "8Mi"}, "limits": {"cpu": "10m", "memory": "32Mi"}}}],
                "volumes": [{"name": "host", "hostPath": {"path": "/", "type": "Directory"}},
                            {"name": "lock", "hostPath": {"path": "/run/xtables.lock", "type": "FileOrCreate"}}],
            }}}}


class Runner:
    def __init__(self, state):
        self.state = Path(state)
        self.state.mkdir(parents=True, exist_ok=True)
        self.last = "none"

    def note(self, text):
        line = f"{datetime.datetime.now(datetime.timezone.utc).isoformat()} {text} last={self.last}\n"
        print(line, end="", flush=True)
        with (self.state / "checkpoint.log").open("a") as out:
            out.write(line)

    def kubectl(self, *args, namespace=NS, payload=None, seconds=45):
        cmd = ["timeout", "--signal=TERM", "--kill-after=10s", f"{seconds}s", "kubectl", "--request-timeout=30s", "-n", namespace, *args]
        self.note(f"before command={cmd!r} error=none")
        output = subprocess.run(cmd, input=json.dumps(payload) if payload is not None else None,
                                text=True, capture_output=True, timeout=seconds + 12, check=True).stdout
        self.last = " ".join(args)
        self.note("after command success error=none")
        return output

    def get(self, kind, name, namespace=NS):
        return json.loads(self.kubectl("get", kind, name, "-o", "json", namespace=namespace))

    def replace(self, old, new):
        if old == new:
            return
        meta = old["metadata"]
        backup = self.state / f'{meta["namespace"]}-{old["kind"]}-{meta["name"]}-{meta["resourceVersion"]}.json'
        backup.write_text(json.dumps(old, indent=2) + "\n")
        # PUT with resourceVersion removes fields exactly and rejects concurrent changes.
        new.pop("status", None)
        new["metadata"].pop("managedFields", None)
        new["metadata"].get("annotations", {}).pop("kubectl.kubernetes.io/last-applied-configuration", None)
        self.kubectl("replace", "-f", "-", payload=new, namespace=meta["namespace"])

    def stopped(self):
        operator = self.get("deploy", "unbounded-operator")
        if operator["spec"].get("replicas", 1) != 0:
            raise RuntimeError("run stop first")
        pods = json.loads(self.kubectl("get", "pods", "-l", "app.kubernetes.io/name=unbounded-operator", "-o", "json"))
        if any(pod.get("status", {}).get("phase") not in ("Succeeded", "Failed") for pod in pods["items"]):
            raise RuntimeError("operator pods still present; inspect and finish stop")

    def stop(self):
        self.kubectl("scale", "deployment/unbounded-operator", "--replicas=0")
        self.kubectl("wait", "--for=delete", "pod", "-l", "app.kubernetes.io/name=unbounded-operator",
                     f"--field-selector={ACTIVE_PODS}", "--timeout=40s", seconds=50)
        self.stopped()

    def configure(self):
        self.stopped()
        for name in ("unbounded-component-overrides", "unbounded-operator-config", "racer-config", "racer-dataplane-config"):
            old = self.get("cm", name)
            new = copy.deepcopy(old)
            if name == "unbounded-component-overrides":
                new["data"] = transform_overrides(old["data"])
            else:
                new["data"] = {k: v for k, v in old["data"].items() if not guard_key(k)}
                if name == "racer-config":
                    new["data"]["RACER_DATAPLANE_IMAGE"] = IMAGES["dataplane"]
            self.replace(old, new)

    def workload(self, kind, name, role, namespace=NS):
        self.stopped()
        old = self.get(kind, name, namespace)
        new = copy.deepcopy(old)
        pod = new["spec"]["template"]["spec"]
        sanitize_pod(pod)
        containers = [c for c in pod["containers"] if c["image"].split("@")[0].split(":")[0] == IMAGES[role].split("@")[0]]
        if not containers:
            raise ValueError(f"no {role} container in {namespace}/{name}")
        for container in containers:
            container["image"] = IMAGES[role]
        strategy(new["spec"], new["kind"])
        self.replace(old, new)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("phase", choices=["stop", "writers", "cleanup-start", "cleanup-wait", "configure", "rollout", "start", "wait", "cleanup-delete", "manifest"])
    parser.add_argument("--state", default="tmp/racer-rollout")
    parser.add_argument("--target", help="deployment/name or daemonset/name for wait")
    parser.add_argument("--namespace", default=NS)
    args = parser.parse_args()
    if args.phase == "manifest":
        print(yaml.safe_dump(cleanup_manifest(), sort_keys=False))
        return
    r = Runner(args.state)
    def interrupted(signum, frame):
        raise TimeoutError(f"phase interrupted by signal {signum}")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGALRM, interrupted)
    signal.alarm(285)
    r.note(f"phase={args.phase} deadline=285s heartbeat<=60s before error=none")
    try:
        if args.phase == "stop":
            r.stop()
        elif args.phase == "writers":
            r.stopped()
            for target in ("deployment/racer-stage47-source-owner", "daemonset/racer-stage47-guard"):
                r.kubectl("delete", target, "--ignore-not-found", "--cascade=foreground", "--wait=true", "--timeout=40s", seconds=50)
        elif args.phase == "cleanup-start":
            r.stopped()
            for target in ("deployment/racer-stage47-source-owner", "daemonset/racer-stage47-guard"):
                if r.kubectl("get", target, "--ignore-not-found", "-o", "name").strip():
                    raise RuntimeError("delete writers first")
            r.kubectl("apply", "-f", "-", payload=cleanup_manifest())
        elif args.phase in ("cleanup-wait", "wait"):
            target = f"daemonset/{CLEANUP}" if args.phase == "cleanup-wait" else args.target
            if not target:
                raise ValueError("wait requires --target")
            # One bounded observation, not a retry loop. Parent diagnoses timeout.
            print(r.kubectl("rollout", "status", target, "--timeout=40s", namespace=args.namespace, seconds=50))
        elif args.phase == "configure":
            r.configure()
        elif args.phase == "rollout":
            for kind, name, role, ns in ROLLOUT_TARGETS:
                r.workload(kind, name, role, ns)
        elif args.phase == "start":
            operator = r.get("deploy", "unbounded-operator")
            if operator["spec"]["template"]["spec"]["containers"][0]["image"] != IMAGES["operator"]:
                raise RuntimeError("install published operator before start")
            r.kubectl("scale", "deployment/unbounded-operator", "--replicas=1")
        elif args.phase == "cleanup-delete":
            r.kubectl("delete", f"daemonset/{CLEANUP}", "--ignore-not-found", "--wait=false")
        r.note(f"phase={args.phase} complete error=none next=next documented phase")
    except Exception as exc:
        r.note(f"phase={args.phase} failed error={exc!r} next=inspect live state; do not restart completed phases")
        if isinstance(exc, subprocess.CalledProcessError):
            print(exc.stderr, file=sys.stderr)
        raise


if __name__ == "__main__":
    main()
