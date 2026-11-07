#!/usr/bin/env python3
"""Report pure Racer crates that depend, transitively, on IO crates.

Uses `cargo metadata` (normal dependencies only, dev and build dependencies
are ignored). Report mode prints warnings and exits 0; pass --strict to exit 1
when any pure crate reaches an IO crate.
"""

import argparse
import json
import subprocess
import sys

# racer-authority is not listed: it owns the control-plane server and its IO
# (racer-io, http1). Its rules module is kept pure by authority/tests/purity.rs.
PURE = ["racer-core", "racer-flow-core", "topology", "racer-wire"]
IO = ["racer-io", "http1", "page-alloc", "flow-control", "rdma-verbs", "racer-gpu", "io-uring"]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest-path", default="cmd/racer/Cargo.toml")
    parser.add_argument("--strict", action="store_true", help="exit 1 on any violation")
    args = parser.parse_args()

    meta = json.loads(
        subprocess.run(
            ["cargo", "metadata", "--format-version", "1", "--locked", "--manifest-path", args.manifest_path],
            check=True,
            capture_output=True,
            text=True,
        ).stdout
    )
    names = {p["id"]: p["name"] for p in meta["packages"]}
    edges = {
        node["id"]: [
            d["pkg"]
            for d in node["deps"]
            if any(k["kind"] is None for k in d["dep_kinds"])
        ]
        for node in meta["resolve"]["nodes"]
    }

    violations = 0
    for root_id, root in names.items():
        if root not in PURE or root_id not in edges:
            continue
        # Breadth-first search keeps the shortest path to each IO crate.
        parent = {root_id: None}
        queue = [root_id]
        while queue:
            current = queue.pop(0)
            for dep in edges.get(current, []):
                if dep not in parent:
                    parent[dep] = current
                    queue.append(dep)
        for dep in sorted(parent, key=lambda i: names[i]):
            if names[dep] in IO:
                path = []
                node = dep
                while node is not None:
                    path.append(names[node])
                    node = parent[node]
                print(f"warning: pure crate {root} depends on IO crate {names[dep]}: {' -> '.join(reversed(path))}")
                violations += 1

    if violations == 0:
        print("racer purity graph: ok")
        return 0
    print(f"racer purity graph: {violations} violation(s)" + ("" if args.strict else " (report only)"))
    return 1 if args.strict else 0


if __name__ == "__main__":
    sys.exit(main())
