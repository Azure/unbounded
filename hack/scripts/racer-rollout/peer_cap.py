#!/usr/bin/env python3
"""Plan and server-dry-run ONLY a byte-preserving Racer peer-cap override."""
import argparse
import copy
import difflib
import json
from pathlib import Path
import signal

import yaml

from rollout import NS, Runner

CM = "unbounded-component-overrides"
KEY = "racer-v2.yaml"
CAP = "RACER_CONNECTIONS_PER_NEIGHBOR"
TARGETS = ("racer-dataplane", "racer-dataplane-podnet")


def require(condition, message):
    if not condition:
        raise ValueError(message)


def dataplane(spec):
    containers = [c for c in spec["template"]["spec"]["containers"] if c["name"] == "dataplane"]
    require(len(containers) == 1, "expected exactly one dataplane container")
    return containers[0]


def transform(data):
    """Insert at YAML source marks, then prove only the two env lists changed."""
    text = data[KEY]
    require(CAP not in text, "cap already present; inspect instead of replaying")
    doc = yaml.safe_load(text)
    expected = copy.deepcopy(doc)
    nodes = dict((k.value, v) for k, v in yaml.compose(text).value)["overrides"].value
    edits, found = [], []
    for index, entry in enumerate(doc["overrides"]):
        if entry.get("component") != "racer" or entry.get("kind") != "DaemonSet":
            continue
        name = entry.get("name")
        require(name in TARGETS and name not in found, "unexpected or duplicate Racer DaemonSet override")
        found.append(name)
        container = dataplane(entry["patch"]["spec"])
        require(isinstance(container.get("env"), list) and container["env"], "expected existing env list")
        node = nodes[index]
        for key in ("patch", "spec", "template", "spec", "containers"):
            node = dict((k.value, v) for k, v in node.value)[key]
        matches = [n for n in node.value if dict((k.value, v) for k, v in n.value)["name"].value == "dataplane"]
        require(len(matches) == 1, "unsupported aliased container")
        env = dict((k.value, v) for k, v in matches[0].value)["env"]
        mark = env.start_mark
        start = text.rfind("\n", 0, mark.index) + 1
        indent = text[start:mark.index]
        require(not indent.strip() and text[mark.index:mark.index + 2] == "- ", "expected block env list")
        require(nodes[index].start_mark.index <= mark.index < nodes[index].end_mark.index,
                "env alias crosses override boundary")
        edits.append((start, f'{indent}- name: {CAP}\n{indent}  value: "4"\n'))
        dataplane(expected["overrides"][index]["patch"]["spec"])["env"].insert(0, {"name": CAP, "value": "4"})
    require(set(found) == set(TARGETS), "missing named dataplane override")
    require(len({start for start, _ in edits}) == 2, "shared env insertion point")
    for start, insertion in sorted(edits, reverse=True):
        text = text[:start] + insertion + text[start:]
    require(yaml.safe_load(text) == expected, "change affects more than the two env lists")
    result = dict(data)
    result[KEY] = text
    return result


def guarded_patch(current, desired):
    require(current["kind"] == "ConfigMap" and current["metadata"]["name"] == CM
            and current["metadata"]["namespace"] == NS, "wrong ConfigMap")
    return [{"op": "test", "path": f"/metadata/{key}", "value": current["metadata"][key]}
            for key in ("uid", "resourceVersion")] + [
        {"op": "test", "path": "/data", "value": current["data"]},
        {"op": "replace", "path": f"/data/{KEY}", "value": desired[KEY]},
    ]


def verify_live(current, config, workloads, cap):
    require(config["data"].get(CAP) == "16", "inherited cap changed")
    for key, text in current["data"].items():
        for entry in yaml.safe_load(text).get("overrides", []):
            if entry.get("component") == "racer" and entry.get("kind") == "DaemonSet":
                require(key == KEY and entry.get("name") in TARGETS, "competing Racer override")
    for ds in workloads:
        c = dataplane(ds["spec"])
        require(c.get("envFrom") == [{"configMapRef": {"name": "racer-dataplane-config"}}],
                "unexpected envFrom sources")
        values = [e for e in c.get("env", []) if e["name"] == CAP]
        allowed = [[]] if cap == "16" else [[], [{"name": CAP, "value": "4"}]]
        require(values in allowed, "unexpected live explicit cap")
        overrides = yaml.safe_load(current["data"][KEY])["overrides"]
        entries = [e for e in overrides if e.get("name") == ds["metadata"]["name"]]
        require(len(entries) == 1 and dataplane(entries[0]["patch"]["spec"])["image"] == c["image"],
                "live and override images differ")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--state", required=True, help="new ignored artifact directory")
    parser.add_argument("--rollback-from", type=Path, help="forward plan directory; fetch fresh guards")
    args = parser.parse_args()
    require(not Path(args.state).exists(), "use a new state directory; never overwrite evidence")
    r = Runner(args.state)
    def interrupted(signum, frame):
        raise TimeoutError(f"phase interrupted by signal {signum}")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGALRM, interrupted)
    signal.alarm(240)
    r.note("before plan deadline=240s heartbeat<=60s next=snapshot and dry-run error=none")
    try:
        current = r.get("cm", CM)
        config = r.get("cm", "racer-dataplane-config")
        workloads = [r.get("ds", name) for name in TARGETS]
        verify_live(current, config, workloads, "4" if args.rollback_from else "16")
        if args.rollback_from:
            original = json.loads((args.rollback_from / "before.json").read_text())
            require(current["metadata"]["uid"] == original["metadata"]["uid"], "ConfigMap recreated; re-review")
            require(current["data"] == transform(original["data"]), "forward data drifted; re-review rollback")
            desired = original["data"]
        else:
            desired = transform(current["data"])
        patch = guarded_patch(current, desired)
        for name, value in (("before", current), ("config", config), ("dataplanes", workloads),
                            ("after-data", desired), ("patch", patch)):
            (r.state / f"{name}.json").write_text(json.dumps(value, indent=2) + "\n")
        diff = "".join(difflib.unified_diff(current["data"][KEY].splitlines(True), desired[KEY].splitlines(True),
                                            fromfile=KEY + ".before", tofile=KEY + ".after"))
        (r.state / "change.diff").write_text(diff)
        print(diff, end="", flush=True)
        # Cross-object preconditions are observations, not an atomic transaction.
        for kind, name, old in [("cm", "racer-dataplane-config", config)] + list(zip(["ds"] * 2, TARGETS, workloads)):
            fresh = r.get(kind, name)
            require(fresh["metadata"]["uid"] == old["metadata"]["uid"], f"{name} recreated")
            require(fresh.get("spec", fresh.get("data")) == old.get("spec", old.get("data")), f"{name} drifted")
        output = r.kubectl("patch", "cm", CM, "--type=json", "--dry-run=server",
                           "--patch-file", str(r.state / "patch.json"), "-o", "json")
        (r.state / "dry-run.json").write_text(output)
        require(json.loads(output)["data"] == desired, "server result differs from reviewed data")
        r.note("after plan success error=none next=parent review/drain/fresh plan/full dataplane rollout; NOT APPLIED")
    except Exception as exc:
        r.note(f"failed error={exc!r} next=inspect evidence; do not replay mutations")
        raise
    finally:
        signal.alarm(0)


if __name__ == "__main__":
    main()
