#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""Merge the bundled machina-config ConfigMap with the live cluster ConfigMap.

WORKAROUND for https://github.com/Azure/unbounded/issues/235.

The release manifests tarball ships a `machina-config` ConfigMap whose inner
``data.config.yaml`` lacks the per-cluster ``apiServerEndpoint`` field that
machina-controller v0.1.5+ requires. Only ``kubectl unbounded site init``
populates that field on the live cluster. If the bundled template were
applied unmodified during an upgrade, it would silently overwrite the live
value and the controller would crash on the next pod restart.

This script reads the bundled ConfigMap manifest and the live ConfigMap's
inner config.yaml, merges them with **policy A** (live wins on conflicts;
bundle contributes new keys only), and rewrites the bundle file in place.
The workflow's subsequent ``kubectl apply`` then pushes a manifest that
preserves all operator state while adding any new keys this release
introduces.

Strict round-trip verification at the end aborts the process if the
rewritten manifest does not reparse.

Inputs (environment):
    BUNDLE_FILE  Path to the bundled 03-config.yaml manifest. Required.
    LIVE_INNER   The live ConfigMap's ``data.config.yaml`` value, as a
                 string. Empty when no live ConfigMap exists (fresh
                 cluster); the bundle is used as-is in that case.

Caveats:
    - YAML comments in the bundled template are lost; PyYAML's safe_dump
      does not preserve them. The active values are what matter for
      controller behavior.
    - Policy A means an operator-set value is never overwritten by a new
      bundle default. Operators who want a new default must update the
      ConfigMap manually.

Remove this script (and the workflow step that invokes it) once issue
#235 is resolved upstream.
"""

import os
import sys

import yaml


def _str_representer(dumper, data):
    """Render multi-line strings as block scalars (``|``) for readability."""

    style = "|" if "\n" in data else None
    return dumper.represent_scalar("tag:yaml.org,2002:str", data, style=style)


yaml.add_representer(str, _str_representer, Dumper=yaml.SafeDumper)


def main() -> int:
    bundle_file = os.environ["BUNDLE_FILE"]
    live_inner = os.environ.get("LIVE_INNER", "")

    # Parse the bundle file. Strict: any parse failure exits non-zero.
    with open(bundle_file) as f:
        bundle = yaml.safe_load(f)
    if not isinstance(bundle, dict):
        sys.exit(f"bundle file {bundle_file} did not parse as a YAML mapping")

    bundle_inner_str = (bundle.get("data") or {}).get("config.yaml") or ""
    bundle_inner = yaml.safe_load(bundle_inner_str) or {}
    if not isinstance(bundle_inner, dict):
        sys.exit("bundled data.config.yaml did not parse as a YAML mapping")

    if live_inner.strip():
        live = yaml.safe_load(live_inner) or {}
        if not isinstance(live, dict):
            sys.exit("live data.config.yaml did not parse as a YAML mapping")

        # Policy A: live wins on conflicts; bundle contributes new keys only.
        # Output order: live keys in live order, then bundle-only keys appended.
        merged = dict(live)
        added = []
        for k, v in bundle_inner.items():
            if k not in merged:
                merged[k] = v
                added.append(k)

        print(f"Preserved {len(live)} key(s) from live ConfigMap")
        if added:
            print(f"Added {len(added)} new key(s) from bundle: {', '.join(added)}")
        else:
            print("No new keys from bundle")
    else:
        print("No live machina-config ConfigMap; using bundle as-is")
        merged = bundle_inner

    # Ensure the inner string ends with a newline so the block scalar renders
    # cleanly and matches kubectl's normal serialization.
    inner_dump = yaml.safe_dump(merged, default_flow_style=False, sort_keys=False)
    if not inner_dump.endswith("\n"):
        inner_dump += "\n"
    bundle["data"]["config.yaml"] = inner_dump

    with open(bundle_file, "w") as f:
        yaml.safe_dump(bundle, f, default_flow_style=False)

    # Strict round-trip verification: the rewritten manifest must reparse
    # cleanly and the inner config.yaml must reparse as a mapping.
    with open(bundle_file) as f:
        check = yaml.safe_load(f)
    if not isinstance(check, dict) or "data" not in check:
        sys.exit("rewritten bundle file failed to round-trip")
    inner_check = yaml.safe_load(check["data"].get("config.yaml", ""))
    if not isinstance(inner_check, dict):
        sys.exit("rewritten inner data.config.yaml failed to round-trip as a mapping")

    return 0


if __name__ == "__main__":
    sys.exit(main())
