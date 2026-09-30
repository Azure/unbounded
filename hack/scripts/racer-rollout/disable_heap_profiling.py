#!/usr/bin/env python3
"""Explicit heap-profiling override removal, separate from image-only upgrades."""
import argparse
import copy
import difflib
import json
import os
import re
import signal
import sys

import yaml

from upgrade import CONTEXT, NS, Runner, StrictLoader, configuration, encoded, identity, plan_hash

KEY = "racer-v2.yaml"
NAME = "unbounded-component-overrides"
TARGETS = {"racer-dataplane", "racer-dataplane-podnet"}
ADDRESS = "RACER_HEAP_PROFILE_ADDR"
ALLOCATORS = {"MALLOC_CONF", "_RJEM_MALLOC_CONF"}


class OccurrenceLoader(StrictLoader):
    """Keep alias occurrence spans, not just the anchor spans from compose()."""

    def __init__(self, text):
        super().__init__(text)
        self.spans = {}

    def compose_node(self, parent, index):
        event = self.peek_event()
        node = super().compose_node(parent, index)
        end = event.end_mark if isinstance(event, yaml.AliasEvent) else node.end_mark
        self.spans[(id(parent), index)] = (event.start_mark.index, end.index)
        return node


def child(node, key):
    matches = [value for name, value in node.value if name.value == key]
    if len(matches) != 1:
        raise ValueError(f"expected exactly one YAML field {key}")
    return matches[0]


def entry_span(text, start, end):
    beginning = text.rfind("\n", 0, start) + 1
    if not re.fullmatch(r" *- +", text[beginning:start]):
        raise ValueError("edited env entries must use standalone block sequence lines")
    end_line = text.rfind("\n", 0, end) + 1
    if text[end_line:end].strip():
        newline = text.find("\n", end)
        end = len(text) if newline < 0 else newline + 1
    else:
        end = end_line
    return beginning, end


def allocator_value(entry):
    if set(entry) != {"name", "value"} or not isinstance(entry["value"], str):
        raise ValueError("allocator env must have a literal string value only")
    value = entry["value"]
    if not value:
        return value
    keep = []
    removed = False
    for option in value.split(","):
        key, separator, val = option.partition(":")
        if not separator or not re.fullmatch(r"[a-zA-Z0-9_]+", key) or not val:
            raise ValueError("ambiguous allocator configuration")
        if key == "prof" or key.startswith(("prof_", "lg_prof_")):
            removed = True
        else:
            keep.append(option)
    return ",".join(keep) if removed else value


def disable(text):
    doc = yaml.load(text, Loader=StrictLoader)
    # Unlike deepcopy, this intentionally breaks sharing: edits must not leak to
    # any unrelated alias consumer. Cyclic YAML is rejected by JSON encoding.
    expected = json.loads(json.dumps(doc))
    if not isinstance(doc, dict) or not isinstance(doc.get("overrides"), list):
        raise ValueError("expected an overrides document")
    loader = OccurrenceLoader(text)
    try:
        root = loader.get_single_node()
        entries = child(root, "overrides")
        edits = set()
        found = set()
        for index, entry in enumerate(expected["overrides"]):
            if entry.get("component") != "racer" or entry.get("kind") != "DaemonSet":
                continue
            name = entry.get("name")
            if name not in TARGETS or name in found:
                raise ValueError("unexpected or duplicate Racer DaemonSet override")
            found.add(name)
            pod = entry["patch"]["spec"]["template"]["spec"]
            containers = pod["containers"]
            matches = [i for i, c in enumerate(containers) if c.get("name") == "dataplane"]
            if len(matches) != 1:
                raise ValueError("expected exactly one dataplane container")
            ci = matches[0]
            container = containers[ci]
            if container.get("envFrom"):
                raise ValueError("envFrom requires separate review for inherited profiling settings")
            env = container.get("env", [])
            if not isinstance(env, list) or len({e["name"] for e in env}) != len(env):
                raise ValueError("invalid or duplicate env names")
            node = entries.value[index]
            for field in ("patch", "spec", "template", "spec", "containers"):
                node = child(node, field)
            if "env" not in container:
                continue
            env_node = child(node.value[ci], "env")
            if env_node.flow_style:
                raise ValueError("edited env must use block sequence YAML")
            kept = []
            for ei, variable in enumerate(env):
                remove = variable["name"] == ADDRESS
                if variable["name"] in ALLOCATORS:
                    value = allocator_value(variable)
                    if value != variable["value"]:
                        if value:
                            value_node = child(env_node.value[ei], "value")
                            edits.add((value_node.start_mark.index, value_node.end_mark.index, json.dumps(value)))
                            variable["value"] = value
                        else:
                            remove = True
                if remove:
                    start, end = loader.spans[(id(env_node), ei)]
                    start, end = entry_span(text, start, end)
                    edits.add((start, end, ""))
                else:
                    kept.append(variable)
            if not kept and env:
                raise ValueError("removal would empty env; review this YAML layout separately")
            container["env"] = kept
        if found != TARGETS:
            raise ValueError("missing required Racer DaemonSet override")
        ordered = sorted(edits)
        if any(a[1] > b[0] for a, b in zip(ordered, ordered[1:])):
            raise ValueError("overlapping YAML edits")
        result = text
        for start, end, value in reversed(ordered):
            result = result[:start] + value + result[end:]
        if yaml.load(result, Loader=StrictLoader) != expected:
            raise ValueError("profiling edit changed unrelated fields or alias consumers")
        return result
    finally:
        loader.dispose()


def build_plan(snapshot, context):
    if context != CONTEXT or snapshot.get("apiVersion") != "v1" or identity(snapshot) != ("ConfigMap", NS, NAME):
        raise ValueError("unexpected context or ConfigMap identity")
    for field in ("uid", "resourceVersion"):
        if not snapshot["metadata"].get(field):
            raise ValueError(f"missing {field}")
    before = snapshot["data"][KEY]
    after = disable(before)
    patch = [
        {"op": "test", "path": "/metadata/uid", "value": snapshot["metadata"]["uid"]},
        {"op": "test", "path": "/metadata/resourceVersion", "value": snapshot["metadata"]["resourceVersion"]},
        {"op": "test", "path": "/data", "value": snapshot["data"]},
        {"op": "replace", "path": "/data/" + KEY, "value": after},
    ]
    return {"version": 1, "context": context, "snapshot": snapshot, "patch": patch}


class ProfilingRunner(Runner):
    def plan(self):
        snapshot = self.kubectl("get", "configmap", NAME, "-o", "json")
        plan = build_plan(snapshot, self.context)
        self.save("snapshot.json", encoded(snapshot))
        self.save("plan.json", encoded(plan))
        self.save("patch.json", encoded(plan["patch"]))
        diff = "".join(difflib.unified_diff(snapshot["data"][KEY].splitlines(True),
                       plan["patch"][-1]["value"].splitlines(True), fromfile=KEY, tofile=KEY + " (profiling disabled)"))
        self.save("plan.diff", diff)
        print(diff, end="")
        print(f"Review plan.json, patch.json and plan.diff; --approved-plan {plan_hash(plan)}")

    def apply(self, approved):
        plan = json.loads((self.state / "plan.json").read_text())
        if not approved or approved != plan_hash(plan):
            raise ValueError("apply requires the reviewed plan hash")
        if plan != build_plan(plan["snapshot"], self.context):
            raise ValueError("plan is not the reproducible profiling-only transformation")
        if json.loads((self.state / "patch.json").read_text()) != plan["patch"]:
            raise ValueError("patch artifact differs from reviewed plan")
        live = self.kubectl("get", "configmap", NAME, "-o", "json")
        expected = copy.deepcopy(plan["snapshot"])
        expected["data"][KEY] = plan["patch"][-1]["value"]
        if configuration(live) == configuration(expected):
            self.note("already applied; no mutation; next=parent readiness gate error=none")
            return
        if live["metadata"]["resourceVersion"] != plan["snapshot"]["metadata"]["resourceVersion"] or configuration(live) != configuration(plan["snapshot"]):
            raise ValueError("snapshot drift; create and review a new plan")
        # Send the validated in-memory patch, not a file that could change after review.
        args = ("patch", "configmap", NAME, "--type=json", "--patch-file", "/dev/stdin", "-o", "json")
        admitted = self.kubectl(*args, "--dry-run=server", payload=plan["patch"])
        if configuration(admitted) != configuration(expected):
            raise ValueError("server dry-run changed unrelated configuration")
        actual = self.kubectl(*args, payload=plan["patch"])
        if configuration(actual) != configuration(expected):
            raise ValueError("apply response differs; parent must inspect admission and live state")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("phase", choices=("plan", "apply"))
    parser.add_argument("--context", required=True, choices=[CONTEXT])
    parser.add_argument("--state", required=True)
    parser.add_argument("--approved-plan")
    args = parser.parse_args()
    if args.phase == "plan" and args.approved_plan:
        parser.error("approval belongs only to apply")
    os.umask(0o077)
    runner = ProfilingRunner(args.state, args.context)

    def interrupted(signum, frame):
        raise TimeoutError(f"phase interrupted by signal {signum}")

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGALRM, interrupted)
    signal.alarm(285)
    runner.note(f"phase={args.phase} before deadline=285s heartbeat<=52s success=phase complete error=none")
    try:
        if args.phase == "plan":
            runner.plan()
        else:
            runner.apply(args.approved_plan)
        runner.note(f"phase={args.phase} complete error=none next=parent review/readiness gate")
    except Exception as exc:
        runner.note(f"phase={args.phase} failed error={exc!r} next=inspect live state; do not replay blindly")
        return 1
    finally:
        signal.alarm(0)
    return 0


if __name__ == "__main__":
    sys.exit(main())
