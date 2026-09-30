#!/usr/bin/env python3
"""Image-only upgrades. Requires kubectl, Python 3, PyYAML; see upgrade.md."""
import argparse
import copy
import datetime
import difflib
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time

import yaml

CONTEXT = "joolshev-scale-test"
PROJECT = Path(__file__).resolve().parents[3]
NS = "unbounded-system"
REPOSITORIES = {role: f"ghcr.io/azure/{name}" for role, name in {
    "operator": "unbounded-operator", "controller": "racer-controller",
    "dataplane": "racer-dataplane", "gantry": "gantry", "loadgen": "racer-loadgen",
}.items()}
OVERRIDES = {
    ("racer", "Deployment", ""): "controller",
    ("racer", "DaemonSet", "racer-dataplane"): "dataplane",
    ("racer", "DaemonSet", "racer-dataplane-podnet"): "dataplane",
    ("gantry", "DaemonSet", ""): "gantry",
}
PHASES = ("snapshot", "plan", "apply-overrides", "apply-operator", "apply-loadgen")


class StrictLoader(yaml.SafeLoader):
    """Reject duplicate keys instead of silently discarding configuration."""

    def construct_mapping(self, node, deep=False):
        result = super().construct_mapping(node, deep=deep)
        if len(result) != len(node.value):
            raise ValueError("duplicate or merged YAML keys are not supported")
        return result


def images_for(sha=None, **refs):
    if sha is not None and not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise ValueError("--sha must be a full 40-character lowercase commit SHA")
    images = {}
    for role, repo in REPOSITORIES.items():
        image = refs.get(role) or (f"{repo}:{sha}" if sha else None)
        if not image or not re.fullmatch(re.escape(repo) + r"(?::[0-9a-f]{40}|@sha256:[0-9a-f]{64})", image):
            raise ValueError(f"{role}: require {repo}:<full SHA> or {repo}@sha256:<digest>")
        images[role] = image
    return images


def named_container(pod, name):
    matches = [c for c in pod["containers"] if c.get("name") == name]
    if len(matches) != 1 or not isinstance(matches[0].get("image"), str):
        raise ValueError(f"expected exactly one existing image for container {name}")
    return matches[0]


def yaml_node(node, *path, aliased=()):
    for key in path:
        if node.start_mark.index in aliased:
            raise ValueError("YAML anchors/aliases cannot share an edited image or its containing structure")
        if isinstance(key, int):
            node = node.value[key]
        else:
            matches = [v for k, v in node.value if k.value == key]
            if len(matches) != 1:
                raise ValueError(f"expected one YAML key {key}")
            node = matches[0]
    if node.start_mark.index in aliased:
        raise ValueError("YAML anchors/aliases cannot share an edited image or its containing structure")
    return node


def upgrade_overrides(data, images):
    result = dict(data)
    found = set()
    for key, text in data.items():
        doc = yaml.load(text, Loader=StrictLoader)
        if not isinstance(doc, dict) or not isinstance(doc.get("overrides"), list):
            raise ValueError(f"{key}: expected overrides document")
        expected = copy.deepcopy(doc)
        root = yaml.compose(text, Loader=StrictLoader)
        # Composed aliases point back to the anchor's node and source span.
        # Reject shared nodes along an image path, not unrelated env/volume aliases.
        # deepcopy also preserves sharing, so the semantic check alone is not enough.
        anchors = {}
        aliased = set()
        for event in yaml.parse(text, Loader=StrictLoader):
            if isinstance(event, yaml.AliasEvent):
                aliased.add(anchors[event.anchor])
            elif getattr(event, "anchor", None):
                anchors[event.anchor] = event.start_mark.index
        edits = []
        for index, entry in enumerate(expected["overrides"]):
            component = entry.get("component")
            if component not in ("racer", "gantry"):
                continue
            target = (component, entry.get("kind"), entry.get("name", ""))
            if target not in OVERRIDES or target in found:
                raise ValueError(f"unexpected or duplicate override target {target}")
            role = OVERRIDES[target]
            pod = entry["patch"]["spec"]["template"]["spec"]
            container = named_container(pod, role)
            container["image"] = images[role]
            ci = pod["containers"].index(container)
            node = yaml_node(root, "overrides", index, "patch", "spec", "template", "spec", "containers", ci, "image", aliased=aliased)
            if not isinstance(node, yaml.ScalarNode) or node.style in ("|", ">"):
                raise ValueError("image must be a plain or quoted YAML scalar")
            edits.append((node.start_mark.index, node.end_mark.index, json.dumps(images[role])))
            found.add(target)
        for start, end, replacement in sorted(edits, reverse=True):
            text = text[:start] + replacement + text[end:]
        if yaml.load(text, Loader=StrictLoader) != expected:
            raise ValueError(f"{key}: image edit affected other fields")
        result[key] = text
    if found != set(OVERRIDES):
        raise ValueError(f"missing required override targets: {set(OVERRIDES) - found}")
    return result


def identity(obj):
    return obj["kind"], obj["metadata"]["namespace"], obj["metadata"]["name"]


def configuration(obj):
    """Only server bookkeeping may drift between review and replacement."""
    result = copy.deepcopy(obj)
    result.pop("status", None)
    for key in ("resourceVersion", "managedFields", "generation"):
        result["metadata"].pop(key, None)
    return result


def build_changes(objects, images):
    images_for(**images)
    by_id = {identity(obj): obj for obj in objects}
    if len(by_id) != len(objects):
        raise ValueError("duplicate snapshot identities")
    required = [("ConfigMap", NS, "unbounded-component-overrides"),
                ("Deployment", NS, "unbounded-operator"),
                ("Deployment", NS, "racer-controller"),
                ("DaemonSet", NS, "racer-dataplane"),
                ("DaemonSet", NS, "racer-dataplane-podnet"),
                ("DaemonSet", NS, "gantry")]
    for target in required:
        if target not in by_id:
            raise ValueError(f"missing required live resource: {target}")
    # Validate the live managed workload layout, but let the operator reconcile it.
    for target, role in zip(required[2:], ("controller", "dataplane", "dataplane", "gantry")):
        named_container(by_id[target]["spec"]["template"]["spec"], role)
    changes = []
    loadgens = 0
    for old in objects:
        if old["kind"] not in ("Deployment", "DaemonSet", "ConfigMap"):
            raise ValueError("snapshot must contain only non-secret workloads and ConfigMaps")
        for field in ("uid", "resourceVersion"):
            if not old["metadata"].get(field):
                raise ValueError(f"missing {field}: {identity(old)}")
        new = copy.deepcopy(old)
        target = identity(old)
        phase = None
        if target == required[0]:
            new["data"] = upgrade_overrides(old["data"], images)
            phase = "apply-overrides"
        elif target == required[1]:
            named_container(new["spec"]["template"]["spec"], "controller")["image"] = images["operator"]
            phase = "apply-operator"
        elif old["kind"] in ("Deployment", "DaemonSet"):
            containers = new["spec"]["template"]["spec"]["containers"]
            matches = [c for c in containers if re.split(r"[@:]", c.get("image", ""))[0] == REPOSITORIES["loadgen"]]
            if target[2] in ("racer-loadgen", "racer-loadgen-client") and not matches:
                raise ValueError(f"loadgen workload has no recognized loadgen image: {target}")
            if matches:
                for container in matches:
                    container["image"] = images["loadgen"]
                loadgens += 1
                phase = "apply-loadgen"
        if phase:
            changes.append({"phase": phase, "before": old, "after": new})
    if not loadgens:
        raise ValueError("missing live loadgen workload")
    return changes


def encoded(value):
    return json.dumps(value, indent=2, sort_keys=True) + "\n"


def plan_hash(plan):
    return hashlib.sha256(encoded(plan).encode()).hexdigest()


def state_path(value):
    path = Path(value).resolve()
    if not path.is_relative_to(PROJECT) or path == PROJECT:
        raise ValueError("--state must be a dedicated directory inside this project/worktree")
    return path


class Runner:
    def __init__(self, state, context):
        if context != CONTEXT:
            raise ValueError(f"--context must be {CONTEXT}")
        self.context = context
        self.state = state_path(state)
        self.state.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.deadline = time.monotonic() + 280
        self.last = "none"

    def note(self, text):
        line = f"{datetime.datetime.now(datetime.timezone.utc).isoformat()} {text}; last={self.last}\n"
        print(line, end="", flush=True)
        with (self.state / "checkpoint.log").open("a") as out:
            out.write(line)

    def save(self, name, value):
        self.note(f"before write={name} error=none")
        # Exclusive creation prevents accidentally overwriting reviewed artifacts.
        with (self.state / name).open("x") as out:
            out.write(value)
        self.note(f"after write={name} error=none")

    def kubectl(self, *args, namespace=NS, payload=None):
        seconds = min(40, int(self.deadline - time.monotonic()) - 12)
        if seconds <= 0:
            raise TimeoutError("phase budget exhausted; inspect checkpoint before resuming")
        cmd = ["timeout", "--signal=TERM", "--kill-after=10s", f"{seconds}s", "kubectl",
               "--context", self.context, "--request-timeout=25s", "-n", namespace, *args]
        self.note(f"before command={cmd!r} error=none")
        # A process group lets interruption clean up timeout, kubectl, and auth children.
        proc = subprocess.Popen(cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, text=True, start_new_session=True)
        try:
            stdout, _ = proc.communicate(None if payload is None else encoded(payload), timeout=seconds + 11)
        except BaseException:
            os.killpg(proc.pid, signal.SIGTERM)
            try:
                proc.communicate(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(proc.pid, signal.SIGKILL)
                proc.communicate()
            raise
        if proc.returncode:
            # Do not persist arbitrary server responses, which can contain config data.
            raise RuntimeError(f"kubectl failed (exit {proc.returncode}); inspect command, credentials and live state")
        self.last = " ".join(args)
        self.note("after command success error=none")
        return json.loads(stdout)

    def inventory(self):
        objects = []
        for namespace in (NS, "racer-loadgen"):
            result = self.kubectl("get", "deployments,daemonsets,configmaps", "-o", "json", namespace=namespace)
            objects.extend(result["items"])
        return sorted(objects, key=identity)

    def snapshot(self):
        objects = self.inventory()
        self.save("snapshot.json", encoded({"context": self.context, "objects": objects}))

    def plan(self, images):
        snapshot = json.loads((self.state / "snapshot.json").read_text())
        if snapshot["context"] != self.context:
            raise ValueError("snapshot context mismatch")
        objects = self.inventory()
        changes = build_changes(objects, images)
        plan = {"version": 1, "context": self.context, "images": images, "objects": objects, "changes": changes}
        diff = ""
        for change in changes:
            label = "/".join(identity(change["before"]))
            # YAML renders embedded override strings legibly, without touching their bytes.
            before = yaml.safe_dump(change["before"], sort_keys=False).splitlines(keepends=True)
            after = yaml.safe_dump(change["after"], sort_keys=False).splitlines(keepends=True)
            diff += "".join(difflib.unified_diff(before, after, fromfile=label, tofile=label + " (planned)"))
        self.save("plan.json", encoded(plan))
        self.save("plan.diff", diff)
        print(diff, end="")
        print(f"Review plan.json and plan.diff; --approved-plan {plan_hash(plan)}")

    def apply(self, phase, approved):
        plan = json.loads((self.state / "plan.json").read_text())
        if not approved or plan_hash(plan) != approved:
            raise ValueError("apply requires --approved-plan matching the reviewed plan hash")
        if plan["version"] != 1 or plan["context"] != self.context:
            raise ValueError("plan version/context mismatch")
        if build_changes(plan["objects"], plan["images"]) != plan["changes"]:
            raise ValueError("plan is not the reproducible image-only transformation")
        pending = []
        # Validate the whole phase before its first write. Every PUT still uses RV.
        for change in plan["changes"]:
            if change["phase"] != phase:
                continue
            old, new = change["before"], change["after"]
            kind, namespace, name = identity(old)
            live = self.kubectl("get", kind, name, "-o", "json", namespace=namespace)
            if configuration(live) == configuration(new):
                self.note(f"already applied target={kind}/{namespace}/{name} error=none")
                continue
            if configuration(live) != configuration(old):
                raise ValueError(f"configuration/identity drift: {kind}/{namespace}/{name}; create a new snapshot/plan")
            replacement = copy.deepcopy(new)
            replacement.pop("status", None)
            replacement["metadata"] = copy.deepcopy(live["metadata"])
            pending.append((namespace, replacement))
        for namespace, replacement in pending:
            admitted = self.kubectl("replace", "--dry-run=server", "-f", "-", "-o", "json", namespace=namespace, payload=replacement)
            if configuration(admitted) != configuration(replacement):
                raise ValueError(f"server dry-run changed configuration: {identity(replacement)}; inspect admission before applying")
        for namespace, replacement in pending:
            self.kubectl("replace", "-f", "-", "-o", "json", namespace=namespace, payload=replacement)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("phase", choices=PHASES)
    parser.add_argument("--context", required=True, choices=[CONTEXT])
    parser.add_argument("--state", required=True)
    parser.add_argument("--sha")
    for role in REPOSITORIES:
        parser.add_argument(f"--{role}-image", dest=role)
    parser.add_argument("--approved-plan")
    args = parser.parse_args()
    refs = {role: getattr(args, role) for role in REPOSITORIES}
    if args.phase != "plan" and (args.sha or any(refs.values())):
        parser.error("image selection belongs only to plan; apply uses the reviewed plan")
    images = images_for(args.sha, **refs) if args.phase == "plan" else None
    os.umask(0o077)
    runner = Runner(args.state, args.context)

    def interrupted(signum, frame):
        raise TimeoutError(f"phase interrupted by signal {signum}")

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGALRM, interrupted)
    signal.alarm(285)
    runner.note(f"phase={args.phase} before deadline=285s heartbeat<=52s success=phase complete error=none")
    try:
        if args.phase == "snapshot":
            runner.snapshot()
        elif args.phase == "plan":
            runner.plan(images)
        else:
            runner.apply(args.phase, args.approved_plan)
        runner.note(f"phase={args.phase} complete error=none next=parent review/gate")
    except Exception as exc:
        runner.note(f"phase={args.phase} failed error={exc!r} next=inspect live state; resume only unapplied work")
        return 1
    finally:
        signal.alarm(0)
    return 0


if __name__ == "__main__":
    sys.exit(main())
