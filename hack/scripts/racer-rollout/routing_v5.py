#!/usr/bin/env python3
"""Prepare ONLY a guarded, coordinated Racer 3->5 plan (or fresh exact rollback)."""
import argparse
import copy
import difflib
import hashlib
import json
from pathlib import Path
import signal
import subprocess

import yaml

from peer_cap import CM, KEY, TARGETS, dataplane, guarded_patch, require
from rollout import NS, Runner

CONTEXT = "joolshev-scale-test"
ALGORITHM = "RACER_ROUTING_ALGORITHM"
IMAGE = "ghcr.io/azure/racer-dataplane:3645eaefd67342f9dd193f68b0a9cc52e220139f"
BASELINE = {"RACER_CONNECTIONS_PER_NEIGHBOR": "16", "RACER_RANGE_WINDOW_PAGES": "2",
            "RACER_OPAQUE_RELAY": "false"}


def mapping(node):
    require(isinstance(node, yaml.MappingNode), "expected YAML mapping")
    result = {}
    for key, value in node.value:
        require(isinstance(key, yaml.ScalarNode) and key.value not in result and key.value != "<<",
                "duplicate/complex/merge YAML key; re-review")
        result[key.value] = value
    return result


def parse(text):
    root = yaml.compose(text)
    seen = set()

    def check(node):
        if id(node) in seen:
            return
        seen.add(id(node))
        if isinstance(node, yaml.MappingNode):
            for child in mapping(node).values():
                check(child)
        elif isinstance(node, yaml.SequenceNode):
            for child in node.value:
                check(child)

    check(root)
    return yaml.safe_load(text), root


def transform(data):
    """Replace two quoted scalar digits; never serialize the source YAML."""
    text = data[KEY]
    doc, root = parse(text)
    # Break alias identity in the semantic oracle, so an edit must not change
    # any non-target alias consumer, even if it resolves to the same object.
    expected = json.loads(json.dumps(doc))
    nodes = mapping(root)["overrides"].value
    edits, found = [], []
    for index, entry in enumerate(doc["overrides"]):
        if entry.get("component") != "racer" or entry.get("kind") != "DaemonSet":
            continue
        name = entry.get("name")
        require(name in TARGETS and name not in found, "unexpected/duplicate Racer DaemonSet")
        found.append(name)
        env = dataplane(entry["patch"]["spec"])["env"]
        matches = [i for i, item in enumerate(env) if item.get("name") == ALGORITHM]
        require(len(matches) == 1 and env[matches[0]] == {"name": ALGORITHM, "value": "3"},
                "expected one existing explicit routing value '3'")
        node = nodes[index]
        for key in ("patch", "spec", "template", "spec", "containers"):
            node = mapping(node)[key]
        containers = [c for c in node.value if mapping(c)["name"].value == "dataplane"]
        require(len(containers) == 1, "expected one dataplane source")
        source_env = mapping(containers[0])["env"]
        value = mapping(source_env.value[matches[0]])["value"]
        start, end = value.start_mark.index, value.end_mark.index
        require(nodes[index].start_mark.index <= start < end <= nodes[index].end_mark.index,
                "routing alias crosses override boundary")
        require(text[start:end] in ("'3'", '"3"'), "routing scalar must be unanchored quoted '3'")
        edits.append(start + 1)
        dataplane(expected["overrides"][index]["patch"]["spec"])["env"][matches[0]]["value"] = "5"
    require(set(found) == set(TARGETS) and len(set(edits)) == 2, "missing or shared routing scalars")
    for index in sorted(edits, reverse=True):
        text = text[:index] + "5" + text[index + 1:]
    require(yaml.safe_load(text) == expected, "routing edit changes another alias consumer")
    result = dict(data)
    result[KEY] = text
    return result


def rollback_data(current, original):
    require(current["metadata"]["uid"] == original["metadata"]["uid"], "ConfigMap recreated")
    require(current["data"] == transform(original["data"]), "forward data drifted; rollback needs re-review")
    return copy.deepcopy(original["data"])


def effective(container, config, key):
    values = [e for e in container.get("env", []) if e["name"] == key]
    require(len(values) <= 1 and (not values or set(values[0]) == {"name", "value"}),
            f"duplicate/indirect {key}")
    return values[0]["value"] if values else config["data"].get(key)


def verify_live(current, config, workloads, rollback=False):
    entries = []
    for key, text in current["data"].items():
        doc, _ = parse(text)
        for entry in doc.get("overrides", []):
            if entry.get("component") == "racer" and entry.get("kind") == "DaemonSet":
                require(key == KEY and entry.get("name") in TARGETS, "competing Racer override")
                entries.append(entry)
    require(sorted(e["name"] for e in entries) == sorted(TARGETS), "missing/duplicate overrides")
    require(sorted(ds["metadata"]["name"] for ds in workloads) == sorted(TARGETS), "wrong fleet targets")
    for ds in workloads:
        c = dataplane(ds["spec"])
        require(c.get("envFrom") == [{"configMapRef": {"name": "racer-dataplane-config"}}],
                "unexpected envFrom")
        require(not c.get("command") and not c.get("args"), "custom entrypoint; re-review")
        override = dataplane(next(e for e in entries if e["name"] == ds["metadata"]["name"])["patch"]["spec"])
        require(c["image"] == override["image"] == IMAGE, "production image changed")
        explicit = [e for e in c.get("env", []) if e["name"] == ALGORITHM]
        allowed = ("3", "5") if rollback else ("3",)
        require(explicit in [[{"name": ALGORITHM, "value": v}] for v in allowed],
                "unexpected live routing algorithm")
        for key, value in BASELINE.items():
            require(effective(c, config, key) == value, f"effective {key} changed")
            require(effective(override, config, key) == value, f"override {key} changed")


class PlanRunner(Runner):
    def kubectl(self, *args, **kwargs):
        # Pin context per command, without changing the parent's kubeconfig.
        return super().kubectl("--context", CONTEXT, *args, **kwargs)


def save_json(path, value):
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")


def prepare(r, rollback_from=None):
    current = r.get("cm", CM)
    config = r.get("cm", "racer-dataplane-config")
    workloads = [r.get("ds", name) for name in TARGETS]
    verify_live(current, config, workloads, bool(rollback_from))
    if rollback_from:
        original = json.loads((rollback_from / "before.json").read_text())
        desired = rollback_data(current, original)
    else:
        desired = transform(current["data"])
    patch = guarded_patch(current, desired)
    for name, value in (("before", current), ("config", config), ("dataplanes", workloads),
                        ("after-data", desired), ("patch", patch)):
        save_json(r.state / f"{name}.json", value)
    diff = "".join(difflib.unified_diff(current["data"][KEY].splitlines(True), desired[KEY].splitlines(True),
                                        fromfile=KEY + ".before", tofile=KEY + ".after"))
    (r.state / "change.diff").write_text(diff)
    print(diff, end="", flush=True)
    # Observational cross-object guards, not an atomic multi-resource transaction.
    for kind, name, old in [("cm", "racer-dataplane-config", config)] + list(zip(["ds"] * 2, TARGETS, workloads)):
        fresh = r.get(kind, name)
        require(fresh["metadata"]["uid"] == old["metadata"]["uid"], f"{name} recreated")
        require(fresh.get("spec", fresh.get("data")) == old.get("spec", old.get("data")), f"{name} drifted")
    output = r.kubectl("patch", "cm", CM, "--type=json", "--dry-run=server",
                       "--patch-file", str(r.state / "patch.json"), "-o", "json")
    (r.state / "dry-run.json").write_text(output)
    require(json.loads(output)["data"] == desired, "server result differs from review")
    names = ("before.json", "config.json", "dataplanes.json", "after-data.json", "patch.json", "change.diff", "dry-run.json")
    sums = "".join(f"{hashlib.sha256((r.state / name).read_bytes()).hexdigest()}  {name}\n" for name in names)
    (r.state / "SHA256SUMS").write_text(sums)
    save_json(r.state / "ready.json", {"context": CONTEXT, "namespace": NS, "applied": False,
              "patch_sha256": hashlib.sha256((r.state / "patch.json").read_bytes()).hexdigest(),
              "direction": "5->3" if rollback_from else "3->5"})
    r.note("after plan success error=none next=parent pause/drain/review/full-fleet rollout; NOT APPLIED")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--state", required=True, type=Path, help="new ignored directory under this worktree's tmp/")
    parser.add_argument("--rollback-from", type=Path, help="forward plan directory; fetch fresh guards")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[3]
    state = args.state.resolve()
    require(state.is_relative_to(root / "tmp") and not state.exists(), "use a new worktree tmp/ directory")
    subprocess.run(["timeout", "--signal=TERM", "--kill-after=10s", "10s", "git", "check-ignore", "-q", str(state)],
                   cwd=root, check=True, timeout=22)
    r = PlanRunner(state)

    def interrupted(signum, frame):
        raise TimeoutError(f"phase interrupted by signal {signum}")

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGALRM, interrupted)
    signal.alarm(270)
    r.note("before plan deadline=270s phasebudget=280s heartbeat<=60s next=snapshot/dry-run error=none")
    try:
        prepare(r, args.rollback_from)
    except Exception as exc:
        r.note(f"failed error={exc!r} next=inspect; no ready marker, do not apply")
        raise
    finally:
        signal.alarm(0)


if __name__ == "__main__":
    main()
