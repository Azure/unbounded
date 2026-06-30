#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Resolve GitHub Actions inputs for publishing agent offline artifacts."""

from __future__ import annotations

import json
import os
import re
import sys
from pathlib import Path

DEFAULT_KUBERNETES_VERSIONS = "v1.34.2"
TAG_PREFIX = "agent-artifacts/"


def main() -> int:
    event_name = os.environ.get("EVENT_NAME", "")
    ref_name = os.environ.get("REF_NAME", "")
    input_tag = os.environ.get("INPUT_TAG", "")
    input_versions = os.environ.get("INPUT_KUBERNETES_VERSIONS", "")
    github_sha = os.environ.get("GITHUB_SHA_VALUE", "")

    if event_name == "push":
        tag = resolve_tag_from_ref(ref_name)
        versions_raw = DEFAULT_KUBERNETES_VERSIONS
    else:
        tag = input_tag.strip() or github_sha[:12]
        versions_raw = input_versions.strip() or DEFAULT_KUBERNETES_VERSIONS

    if not tag:
        print("::error::artifact tag could not be resolved", file=sys.stderr)
        return 1

    versions = normalize_kubernetes_versions(versions_raw)
    if not versions:
        print("::error::at least one Kubernetes version is required", file=sys.stderr)
        return 1

    versions_json = json.dumps(versions)
    write_github_output({
        "tag": tag,
        "kubernetes_versions": versions_json,
    })

    print(f"Publishing tag prefix: {tag}")
    print(f"Kubernetes versions: {versions_json}")

    return 0


def resolve_tag_from_ref(ref_name: str) -> str:
    if ref_name.startswith(TAG_PREFIX):
        return ref_name[len(TAG_PREFIX):]

    return ref_name


def normalize_kubernetes_versions(raw: str) -> list[str]:
    versions = [value for value in re.split(r"[\s,]+", raw.strip()) if value]
    return [value if value.startswith("v") else "v" + value for value in versions]


def write_github_output(values: dict[str, str]) -> None:
    output_path = os.environ.get("GITHUB_OUTPUT")
    if not output_path:
        for key, value in values.items():
            print(f"{key}={value}")
        return

    with Path(output_path).open("a", encoding="utf-8") as f:
        for key, value in values.items():
            f.write(f"{key}={value}\n")


if __name__ == "__main__":
    raise SystemExit(main())
