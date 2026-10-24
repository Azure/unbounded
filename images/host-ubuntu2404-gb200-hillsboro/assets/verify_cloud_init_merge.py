#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Verify native cloud-init merging with Metalman's rendered vendor-data."""

from __future__ import annotations

import argparse
from pathlib import Path

from cloudinit import util
import yaml

from render_boot_vendor_data import render_vendor_data


def load_yaml(contents: str, source: Path) -> dict:
    loaded = yaml.safe_load(contents)
    if not isinstance(loaded, dict):
        raise RuntimeError(f"{source} did not contain a cloud-config mapping")
    return loaded


def verify(
    image_config_path: Path,
    vendor_template_path: Path,
    install_script_path: Path,
    ssh_import_id_script_path: Path,
) -> None:
    image_config = load_yaml(image_config_path.read_text(), image_config_path)
    rendered_vendor_data = render_vendor_data(
        vendor_template_path,
        install_script_path,
        ssh_import_id_script_path,
        True,
    )
    vendor_config = load_yaml(rendered_vendor_data, vendor_template_path)
    if "users" in vendor_config:
        raise RuntimeError("MetalMan vendor-data must not replace image users")

    merged = util.mergemanydict([vendor_config, image_config])
    if merged.get("users") != image_config.get("users"):
        raise RuntimeError("image users did not survive vendor-data merging")
    for key in ("reporting", "write_files", "runcmd"):
        if merged.get(key) != vendor_config.get(key):
            raise RuntimeError(f"vendor-data {key} changed during merging")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--image-config", type=Path, required=True)
    parser.add_argument("--vendor-template", type=Path, required=True)
    parser.add_argument("--install-script", type=Path, required=True)
    parser.add_argument("--ssh-import-id-script", type=Path, required=True)
    args = parser.parse_args()
    verify(
        args.image_config,
        args.vendor_template,
        args.install_script,
        args.ssh_import_id_script,
    )


if __name__ == "__main__":
    main()
