#!/usr/bin/env python3
"""Staged, reviewed page-window 1->2 tuning; see page-window.md."""
import argparse
import copy
import difflib
import json
import os
from pathlib import Path
import signal
import sys

import yaml

from disable_heap_profiling import KEY, NAME, TARGETS, OccurrenceLoader, child
from opaque_relay import DEFAULTS, checked_snapshot
from upgrade import CONTEXT, NS, Runner, StrictLoader, configuration, encoded, identity, plan_hash

GANTRY = "gantry-config"
GKEY = "config.yaml"
ENV = "RACER_RANGE_WINDOW_PAGES"
FIELD = "racer_page_window"
CMS = (NAME, GANTRY, DEFAULTS)
WORKLOADS = ("gantry", *sorted(TARGETS))


def tune(text, racer=False):
    expected = json.loads(json.dumps(yaml.load(text, Loader=StrictLoader)))
    loader = OccurrenceLoader(text)
    edits = set()
    try:
        root = loader.get_single_node()
        if not racer:
            if type(expected.get(FIELD)) is not int or expected[FIELD] != 1:
                raise ValueError("Gantry window must be explicit integer 1")
            node = child(root, FIELD)
            if text[node.start_mark.index:node.end_mark.index] != "1":
                raise ValueError("Gantry window must be an unaliased plain 1")
            edits.add((node.start_mark.index, node.end_mark.index, "2"))
            expected[FIELD] = 2
        else:
            found = set()
            entries = child(root, "overrides")
            for index, entry in enumerate(expected["overrides"]):
                if entry.get("component") != "racer" or entry.get("kind") != "DaemonSet":
                    continue
                name = entry.get("name")
                if name not in TARGETS or name in found:
                    raise ValueError("unexpected or duplicate Racer DaemonSet")
                found.add(name)
                containers = entry["patch"]["spec"]["template"]["spec"]["containers"]
                matches = [i for i, c in enumerate(containers) if c.get("name") == "dataplane"]
                if len(matches) != 1:
                    raise ValueError("expected one dataplane container")
                ci = matches[0]
                container = containers[ci]
                env = container.get("env", [])
                if container.get("envFrom") or not env or len({e["name"] for e in env}) != len(env):
                    raise ValueError("ambiguous override environment")
                node = entries.value[index]
                for field in ("patch", "spec", "template", "spec", "containers"):
                    node = child(node, field)
                node = child(node.value[ci], "env")
                if node.flow_style:
                    raise ValueError("require block env sequence")
                matches = [i for i, e in enumerate(env) if e["name"] == ENV]
                if matches:
                    ei = matches[0]
                    if env[ei] != {"name": ENV, "value": "1"}:
                        raise ValueError("Racer override window must be literal string 1")
                    scalar = child(node.value[ei], "value")
                    start, end = scalar.start_mark.index, scalar.end_mark.index
                    if text[start:end] not in ("'1'", '"1"'):
                        raise ValueError("require unaliased quoted window")
                    edits.add((start, end, '"2"'))
                    env[ei]["value"] = "2"
                else:
                    start, _ = loader.spans[(id(node), 0)]
                    beginning = text.rfind("\n", 0, start) + 1
                    prefix = text[beginning:start]
                    dash = prefix.find("-")
                    if dash < 0 or prefix[:dash].strip() or prefix[dash + 1:].strip():
                        raise ValueError("require standalone env entry lines")
                    indent = prefix[:dash]
                    edits.add((beginning, beginning, f'{indent}- name: {ENV}\n{indent}  value: "2"\n'))
                    env.insert(0, {"name": ENV, "value": "2"})
            if found != TARGETS:
                raise ValueError("missing required Racer DaemonSet")
        ordered = sorted(edits)
        if any(a[1] > b[0] for a, b in zip(ordered, ordered[1:])):
            raise ValueError("overlapping edits")
        result = text
        for start, end, value in reversed(ordered):
            result = result[:start] + value + result[end:]
        if yaml.load(result, Loader=StrictLoader) != expected:
            raise ValueError("edit changed unrelated fields or alias consumers")
        return result
    finally:
        loader.dispose()


def validate_live(workloads):
    if set(workloads) != set(WORKLOADS):
        raise ValueError("missing workloads")
    for name, ds in workloads.items():
        if ds.get("apiVersion") != "apps/v1" or identity(ds) != ("DaemonSet", NS, name):
            raise ValueError("unexpected workload")
        if any(not ds["metadata"].get(k) for k in ("uid", "resourceVersion")):
            raise ValueError("missing workload UID/resourceVersion")
        pod = ds["spec"]["template"]["spec"]
        role = "gantry" if name == "gantry" else "dataplane"
        matches = [c for c in pod["containers"] if c.get("name") == role]
        if len(matches) != 1:
            raise ValueError("ambiguous workload container")
        c = matches[0]
        env = c.get("env", [])
        if len({e["name"] for e in env}) != len(env):
            raise ValueError("duplicate live env")
        if name == "gantry":
            if c.get("command") or c.get("args") != ["agent", "--config=/etc/gantry/config.yaml"]:
                raise ValueError("Gantry command/config precedence requires review")
            if c.get("envFrom") or any(e["name"] == "GANTRY_RACER_PAGE_WINDOW" for e in env):
                raise ValueError("Gantry environment overrides config")
            if c["resources"]["limits"].get("memory") != "2Gi":
                raise ValueError("memory assessment requires the reviewed 2Gi limit")
            mounts = [m for m in c["volumeMounts"] if m["mountPath"] == "/etc/gantry"]
            if len(mounts) != 1 or mounts[0].get("subPath"):
                raise ValueError("unexpected Gantry config mount")
            volumes = [v for v in pod["volumes"] if v["name"] == mounts[0]["name"]]
            if len(volumes) != 1 or volumes[0].get("configMap", {}).get("name") != GANTRY or volumes[0]["configMap"].get("items"):
                raise ValueError("unexpected Gantry config source")
        else:
            if c.get("command") or c.get("args") or c.get("envFrom") != [{"configMapRef": {"name": DEFAULTS}}]:
                raise ValueError("unexpected Racer config precedence")
            relay = [e for e in env if e["name"] == "RACER_OPAQUE_RELAY"]
            if relay and relay != [{"name": "RACER_OPAQUE_RELAY", "value": "false"}]:
                raise ValueError("finish parent relay rollback before window tuning")


def build_plan(snapshots, workloads, context, stage, source=None):
    if context != CONTEXT or stage not in ("gantry", "racer") or set(snapshots) != set(CMS):
        raise ValueError("unexpected context/stage/snapshots")
    for name, obj in snapshots.items():
        checked_snapshot(obj, name)
    validate_live(workloads)
    defaults = snapshots[DEFAULTS]["data"]
    if defaults.get(ENV) != "1" or defaults.get("RACER_OPAQUE_RELAY") != "false":
        raise ValueError("inherited window/relay defaults drift")
    targets = set()
    racer_windows = {}
    for key, value in snapshots[NAME]["data"].items():
        doc = yaml.load(value, Loader=StrictLoader)
        for entry in doc["overrides"]:
            if entry.get("component") not in ("racer", "gantry") or entry.get("kind") != "DaemonSet":
                continue
            target = (entry["component"], entry.get("name", ""))
            allowed = {("racer", n) for n in TARGETS} | {("gantry", "")}
            if target not in allowed or target in targets or (target[0] == "racer" and key != KEY):
                raise ValueError("unexpected or duplicate override target")
            targets.add(target)
            containers = entry["patch"]["spec"]["template"]["spec"]["containers"]
            for container in containers:
                if container.get("envFrom"):
                    raise ValueError("override envFrom requires separate review")
                for variable in container.get("env", []):
                    if variable["name"] == "GANTRY_RACER_PAGE_WINDOW":
                        raise ValueError("Gantry override shadows config")
                    if variable["name"] == "RACER_OPAQUE_RELAY" and variable != {"name": "RACER_OPAQUE_RELAY", "value": "false"}:
                        raise ValueError("finish parent relay rollback in overrides first")
                if target[0] == "racer" and container.get("name") == "dataplane":
                    windows = [e for e in container.get("env", []) if e["name"] == ENV]
                    desired = "2" if source is not None and stage == "racer" else "1"
                    if windows and windows != [{"name": ENV, "value": desired}]:
                        raise ValueError("unexpected Racer override window")
                    racer_windows[target[1]] = windows or [{"name": ENV, "value": defaults[ENV]}]
    if not {("racer", n) for n in TARGETS}.issubset(targets):
        raise ValueError("missing Racer overrides")
    for n in TARGETS:
        c = next(c for c in workloads[n]["spec"]["template"]["spec"]["containers"] if c["name"] == "dataplane")
        windows = [e for e in c.get("env", []) if e["name"] == ENV]
        if (windows or [{"name": ENV, "value": defaults[ENV]}]) != racer_windows.get(n):
            raise ValueError("Racer window has not converged to overrides")
    name, key = (GANTRY, GKEY) if stage == "gantry" else (NAME, KEY)
    snapshot = snapshots[name]
    if source is None:
        if stage == "racer" and yaml.load(snapshots[GANTRY]["data"][GKEY], Loader=StrictLoader).get(FIELD) != 2:
            raise ValueError("review and converge Gantry window 2 first")
        text = tune(snapshot["data"][key], racer=stage == "racer")
    else:
        if source.get("source") is not None or source["stage"] != stage or source != build_plan(source["snapshots"], source["workloads"], context, stage):
            raise ValueError("rollback source is not a reproducible forward plan")
        expected = copy.deepcopy(source["snapshots"])
        expected[name]["data"][key] = source["patch"][-1]["value"]
        if any(configuration(snapshots[n]) != configuration(expected[n]) for n in CMS):
            raise ValueError("rollback drift; reverse Racer before Gantry")
        text = source["snapshots"][name]["data"][key]
    patch = [{"op": "test", "path": "/metadata/" + k, "value": snapshot["metadata"][k]}
             for k in ("uid", "resourceVersion")]
    patch += [{"op": "test", "path": "/data", "value": snapshot["data"]},
              {"op": "replace", "path": "/data/" + key, "value": text}]
    return {"version": 1, "context": context, "stage": stage, "name": name, "key": key,
            "snapshots": snapshots, "workloads": workloads, "source": source, "patch": patch}


class WindowRunner(Runner):
    def collect(self):
        cms = self.kubectl("get", "configmap", *CMS, "-o", "json")["items"]
        ds = self.kubectl("get", "daemonset", *WORKLOADS, "-o", "json")["items"]
        return ({o["metadata"]["name"]: o for o in cms}, {o["metadata"]["name"]: o for o in ds})

    def plan(self, stage, source=None):
        snapshots, workloads = self.collect()
        plan = build_plan(snapshots, workloads, self.context, stage, source)
        self.save("plan.json", encoded(plan))
        self.save("patch.json", encoded(plan["patch"]))
        old = snapshots[plan["name"]]["data"][plan["key"]]
        diff = "".join(difflib.unified_diff(old.splitlines(True), plan["patch"][-1]["value"].splitlines(True),
                                           fromfile=plan["name"], tofile=plan["name"] + " (reviewed)"))
        self.save("plan.diff", diff)
        print(diff, end="")
        print(f"Review all artifacts; --approved-plan {plan_hash(plan)}")

    def apply(self, approved, dry_run=False):
        plan = json.loads((self.state / "plan.json").read_text())
        if not approved or approved != plan_hash(plan):
            raise ValueError("requires reviewed plan hash")
        if plan != build_plan(plan["snapshots"], plan["workloads"], self.context, plan["stage"], plan["source"]):
            raise ValueError("plan is not reproducible")
        if json.loads((self.state / "patch.json").read_text()) != plan["patch"]:
            raise ValueError("patch artifact differs")
        live, workloads = self.collect()
        validate_live(workloads)
        name, key = plan["name"], plan["key"]
        expected = copy.deepcopy(plan["snapshots"][name])
        expected["data"][key] = plan["patch"][-1]["value"]
        for n in CMS:
            if n != name and live[n] != plan["snapshots"][n]:
                raise ValueError("guard ConfigMap drift; replan")
        if configuration(live[name]) == configuration(expected):
            self.note("already applied; no mutation; next=parent readiness gate error=none")
            return
        if live[name] != plan["snapshots"][name] or any(configuration(workloads[n]) != configuration(plan["workloads"][n]) for n in WORKLOADS):
            raise ValueError("snapshot/workload drift; replan")
        args = ("patch", "configmap", name, "--type=json", "--patch-file", "/dev/stdin", "-o", "json")
        admitted = self.kubectl(*args, "--dry-run=server", payload=plan["patch"])
        if configuration(admitted) != configuration(expected):
            raise ValueError("server dry-run changed unrelated configuration")
        if not dry_run:
            actual = self.kubectl(*args, payload=plan["patch"])
            if configuration(actual) != configuration(expected):
                raise ValueError("apply differs; parent must inspect live state")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("phase", choices=("plan", "rollback-plan", "verify", "apply"))
    parser.add_argument("--context", required=True, choices=[CONTEXT])
    parser.add_argument("--state", required=True)
    parser.add_argument("--stage", choices=("gantry", "racer"))
    parser.add_argument("--source-plan")
    parser.add_argument("--approved-plan")
    args = parser.parse_args()
    if bool(args.source_plan) != (args.phase == "rollback-plan") or bool(args.stage) != (args.phase == "plan"):
        parser.error("--stage only for plan; --source-plan only for rollback-plan (required)")
    if bool(args.approved_plan) != (args.phase in ("verify", "apply")):
        parser.error("--approved-plan required only for verify/apply")
    os.umask(0o077)
    runner = WindowRunner(args.state, args.context)

    def interrupted(signum, frame):
        raise TimeoutError(f"phase interrupted by signal {signum}")

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGALRM, interrupted)
    signal.alarm(285)
    runner.note(f"phase={args.phase} before deadline=285s heartbeat<=52s error=none")
    try:
        if args.phase in ("plan", "rollback-plan"):
            source = json.loads(Path(args.source_plan).read_text()) if args.source_plan else None
            runner.plan(source["stage"] if source else args.stage, source)
        else:
            runner.apply(args.approved_plan, args.phase == "verify")
        runner.note(f"phase={args.phase} complete next=parent review/readiness error=none")
    except Exception as exc:
        runner.note(f"phase={args.phase} failed error={exc!r} next=inspect; do not replay blindly")
        return 1
    finally:
        signal.alarm(0)
    return 0


if __name__ == "__main__":
    sys.exit(main())
