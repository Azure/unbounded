#!/usr/bin/env python3
"""Reviewed relay-only override enablement and exact-byte rollback; see opaque-relay.md."""
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
from upgrade import CONTEXT, NS, Runner, StrictLoader, configuration, encoded, identity, plan_hash

ENV = "RACER_OPAQUE_RELAY"
DEFAULTS = "racer-dataplane-config"


def enable(text):
    doc = yaml.load(text, Loader=StrictLoader)
    expected = json.loads(json.dumps(doc))  # Break alias sharing before semantic comparison.
    if not isinstance(doc, dict) or not isinstance(doc.get("overrides"), list):
        raise ValueError("expected overrides document")
    loader = OccurrenceLoader(text)
    originals, edits = {}, set()
    try:
        entries = child(loader.get_single_node(), "overrides")
        for index, entry in enumerate(expected["overrides"]):
            if entry.get("component") != "racer" or entry.get("kind") != "DaemonSet":
                continue
            name = entry.get("name")
            if name not in TARGETS or name in originals:
                raise ValueError("unexpected or duplicate Racer DaemonSet")
            containers = entry["patch"]["spec"]["template"]["spec"]["containers"]
            matches = [i for i, c in enumerate(containers) if c.get("name") == "dataplane"]
            if len(matches) != 1:
                raise ValueError("expected exactly one dataplane container")
            ci = matches[0]
            container = containers[ci]
            if container.get("envFrom"):
                raise ValueError("override envFrom requires separate review")
            env = container.get("env")
            if not isinstance(env, list) or not env or len({e["name"] for e in env}) != len(env):
                raise ValueError("expected nonempty env with unique names")
            node = entries.value[index]
            for field in ("patch", "spec", "template", "spec", "containers"):
                node = child(node, field)
            env_node = child(node.value[ci], "env")
            if env_node.flow_style:
                raise ValueError("env must use block sequence YAML")
            matches = [i for i, e in enumerate(env) if e["name"] == ENV]
            if matches:
                ei = matches[0]
                variable = env[ei]
                if variable != {"name": ENV, "value": "false"}:
                    raise ValueError("relay must be a literal false string before enablement")
                value = child(env_node.value[ei], "value")
                if not isinstance(value, yaml.ScalarNode) or value.style in ("|", ">"):
                    raise ValueError("relay must be a plain or quoted scalar")
                start, end = value.start_mark.index, value.end_mark.index
                # Anchored scalars can have unrelated consumers; reject rather than rename anchors.
                if text[start:end].lstrip().startswith(("&", "*", "!")):
                    raise ValueError("aliased relay scalar requires separate review")
                edits.add((start, end, '"true"'))
                variable["value"] = "true"
                originals[name] = "false"
            else:
                start, _ = loader.spans[(id(env_node), 0)]
                beginning = text.rfind("\n", 0, start) + 1
                prefix = text[beginning:start]
                # First occurrence may be an anchor or ordinary mapping, but not an env-list alias.
                dash = prefix.find("-")
                if dash < 0 or prefix[:dash].strip() or prefix[dash + 1:].strip():
                    raise ValueError("env entries must start on standalone block sequence lines")
                indent = prefix[:dash]
                edits.add((beginning, beginning, f'{indent}- name: {ENV}\n{indent}  value: "true"\n'))
                env.insert(0, {"name": ENV, "value": "true"})
                originals[name] = None
        if set(originals) != TARGETS:
            raise ValueError("missing required Racer DaemonSet")
        ordered = sorted(edits)
        if any(a[1] > b[0] for a, b in zip(ordered, ordered[1:])):
            raise ValueError("overlapping edits")
        result = text
        for start, end, replacement in reversed(ordered):
            result = result[:start] + replacement + result[end:]
        if yaml.load(result, Loader=StrictLoader) != expected:
            raise ValueError("relay edit changed unrelated fields or alias consumers")
        return result, originals
    finally:
        loader.dispose()


def checked_snapshot(obj, name):
    if obj.get("apiVersion") != "v1" or identity(obj) != ("ConfigMap", NS, name):
        raise ValueError("unexpected ConfigMap identity")
    if any(not obj["metadata"].get(k) for k in ("uid", "resourceVersion")):
        raise ValueError("missing UID/resourceVersion")


def patch_for(snapshot, text):
    return [{"op": "test", "path": "/metadata/" + k, "value": snapshot["metadata"][k]}
            for k in ("uid", "resourceVersion")] + [
        {"op": "test", "path": "/data", "value": snapshot["data"]},
        {"op": "replace", "path": "/data/" + KEY, "value": text}]


def build_plan(snapshot, defaults, context, source=None):
    if context != CONTEXT:
        raise ValueError("unexpected context")
    checked_snapshot(snapshot, NAME)
    checked_snapshot(defaults, DEFAULTS)
    if defaults["data"].get(ENV) != "false":
        raise ValueError("inherited relay default must remain false")
    if source is None:
        text, originals = enable(snapshot["data"][KEY])
    else:
        if source.get("source") is not None or source != build_plan(source["snapshot"], source["defaults"], context):
            raise ValueError("rollback source is not an enable plan")
        expected = copy.deepcopy(source["snapshot"])
        expected["data"][KEY] = source["patch"][-1]["value"]
        if configuration(snapshot) != configuration(expected) or defaults != source["defaults"]:
            raise ValueError("rollback drift; refusing to overwrite unrelated changes")
        text, originals = source["snapshot"]["data"][KEY], source["original_values"]
    return {"version": 1, "context": context, "snapshot": snapshot, "defaults": defaults,
            "original_values": originals, "source": source, "patch": patch_for(snapshot, text)}


class RelayRunner(Runner):
    def plan(self, source=None):
        snapshot = self.kubectl("get", "configmap", NAME, "-o", "json")
        defaults = self.kubectl("get", "configmap", DEFAULTS, "-o", "json")
        plan = build_plan(snapshot, defaults, self.context, source)
        self.save("snapshot.json", encoded(snapshot))
        self.save("plan.json", encoded(plan))
        self.save("patch.json", encoded(plan["patch"]))
        diff = "".join(difflib.unified_diff(snapshot["data"][KEY].splitlines(True),
                       plan["patch"][-1]["value"].splitlines(True), fromfile=KEY, tofile=KEY + " (reviewed)"))
        self.save("plan.diff", diff)
        print(diff, end="")
        print(f"Review all artifacts; --approved-plan {plan_hash(plan)}")

    def apply(self, approved, dry_run=False):
        plan = json.loads((self.state / "plan.json").read_text())
        if not approved or approved != plan_hash(plan):
            raise ValueError("requires reviewed plan hash")
        if plan != build_plan(plan["snapshot"], plan["defaults"], self.context, plan["source"]):
            raise ValueError("plan is not reproducible")
        if json.loads((self.state / "patch.json").read_text()) != plan["patch"]:
            raise ValueError("patch artifact differs")
        defaults = self.kubectl("get", "configmap", DEFAULTS, "-o", "json")
        if defaults != plan["defaults"]:
            raise ValueError("inherited defaults drift")
        live = self.kubectl("get", "configmap", NAME, "-o", "json")
        expected = copy.deepcopy(plan["snapshot"])
        expected["data"][KEY] = plan["patch"][-1]["value"]
        if configuration(live) == configuration(expected):
            self.note("already applied; no mutation; next=parent readiness gate error=none")
            return
        if live["metadata"]["resourceVersion"] != plan["snapshot"]["metadata"]["resourceVersion"] or configuration(live) != configuration(plan["snapshot"]):
            raise ValueError("snapshot drift; create and review a fresh plan")
        args = ("patch", "configmap", NAME, "--type=json", "--patch-file", "/dev/stdin", "-o", "json")
        admitted = self.kubectl(*args, "--dry-run=server", payload=plan["patch"])
        if configuration(admitted) != configuration(expected):
            raise ValueError("server dry-run changed unrelated configuration")
        if dry_run:
            self.note("server dry-run verified; no persisted cluster mutation error=none")
            return
        actual = self.kubectl(*args, payload=plan["patch"])
        if configuration(actual) != configuration(expected):
            raise ValueError("apply differs; parent must inspect live state")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("phase", choices=("plan", "rollback-plan", "verify", "apply"))
    parser.add_argument("--context", required=True, choices=[CONTEXT])
    parser.add_argument("--state", required=True)
    parser.add_argument("--source-plan")
    parser.add_argument("--approved-plan")
    args = parser.parse_args()
    if bool(args.source_plan) != (args.phase == "rollback-plan"):
        parser.error("--source-plan is required only for rollback-plan")
    if bool(args.approved_plan) != (args.phase in ("verify", "apply")):
        parser.error("--approved-plan is required only for verify/apply")
    os.umask(0o077)
    runner = RelayRunner(args.state, args.context)

    def interrupted(signum, frame):
        raise TimeoutError(f"phase interrupted by signal {signum}")

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGALRM, interrupted)
    signal.alarm(285)
    runner.note(f"phase={args.phase} before deadline=285s heartbeat<=52s error=none")
    try:
        if args.phase in ("plan", "rollback-plan"):
            source = json.loads(Path(args.source_plan).read_text()) if args.source_plan else None
            runner.plan(source)
        else:
            runner.apply(args.approved_plan, dry_run=args.phase == "verify")
        runner.note(f"phase={args.phase} complete next=parent review/readiness error=none")
    except Exception as exc:
        runner.note(f"phase={args.phase} failed error={exc!r} next=inspect; do not replay blindly")
        return 1
    finally:
        signal.alarm(0)
    return 0


if __name__ == "__main__":
    sys.exit(main())
