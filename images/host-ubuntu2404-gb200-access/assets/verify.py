#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Verify the GB200 access fragment and MetalMan cloud-init merge."""

from __future__ import annotations

import argparse
import hashlib
from pathlib import Path
import struct
import tarfile

import yaml


USERS = [
    "bcho",
    "cchildress",
    "chokevin",
    "imiller31",
    "jveski",
    "jwilder",
    "phealy",
    "plombardi89",
    "qike-ms",
    "vpatelsj",
]
SSH_IMPORT_ID_VERSION = "5.11-0ubuntu2.24.04.1"
AGENT_ARCHIVE_SHA256 = (
    "faf21645f33cd5ef80cce04ccb2d8c9fc289cfb1dedbaeb47cf85e58186a8ff0"
)
AGENT_ARCHIVE_MEMBERS = ["LICENSE", "NOTICE", "unbounded-agent"]
ELF_MACHINE_AARCH64 = 183
GENERIC_USERS = """users:
- name: unbounded-metal
  lock_passwd: true
  shell: /bin/bash
  sudo: ALL=(ALL) NOPASSWD:ALL
"""
USERS_TEMPLATE = f"""{{{{ if not .PreserveImageUsers }}}}
{GENERIC_USERS}{{{{ end }}}}
"""
INSTALL_ENV_TEMPLATE = """    {{ range .InstallEnv }}
    export {{ . }}
    {{ end }}
"""


def load_mapping(path: Path) -> dict:
    loaded = yaml.safe_load(path.read_text())
    if not isinstance(loaded, dict):
        raise RuntimeError(f"{path} must contain one cloud-config mapping")
    return loaded


def validate_users(config: dict) -> None:
    users = config.get("users")
    if not isinstance(users, list):
        raise RuntimeError("users must be a list")
    if [user.get("name") for user in users] != USERS:
        raise RuntimeError("users do not match the reviewed account list")

    for user in users:
        name = user["name"]
        if user.get("lock_passwd") is not True:
            raise RuntimeError(f"{name} password must be locked")
        if user.get("shell") != "/bin/bash":
            raise RuntimeError(f"{name} shell must be /bin/bash")
        if user.get("sudo") != "ALL=(ALL:ALL) NOPASSWD:ALL":
            raise RuntimeError(f"{name} sudo policy changed")

        identity = user.get("ssh_import_id")
        if name == "bcho":
            if identity is not None:
                raise RuntimeError("bcho must remain keyless")
        elif identity != [f"gh:{name}"]:
            raise RuntimeError(f"{name} must import its same-named GitHub identity")


def render_vendor_data(template: str, preserve_image_users: bool) -> dict:
    if USERS_TEMPLATE not in template:
        raise RuntimeError("MetalMan users template changed")
    rendered = template.replace(
        USERS_TEMPLATE,
        "" if preserve_image_users else GENERIC_USERS,
        1,
    )
    rendered = rendered.replace("{{ .ServeURL }}", "https://example.invalid")
    rendered = rendered.replace("{{ .AgentConfigJSON }}", "{}")
    rendered = rendered.replace(
        "{{ indent 4 .InstallScript }}",
        "    #!/bin/sh\n    exit 0",
    )
    if INSTALL_ENV_TEMPLATE not in rendered:
        raise RuntimeError("MetalMan install environment template changed")
    rendered = rendered.replace(INSTALL_ENV_TEMPLATE, "", 1)
    if "{{" in rendered or "}}" in rendered:
        raise RuntimeError("unrendered MetalMan vendor-data template expression")

    loaded = yaml.safe_load(rendered)
    if not isinstance(loaded, dict):
        raise RuntimeError("rendered MetalMan vendor-data is not a mapping")
    return loaded


def verify_merge(image_config: dict, vendor_template: Path) -> None:
    from cloudinit import util

    template = vendor_template.read_text()
    dedicated_vendor = render_vendor_data(template, True)
    if "users" in dedicated_vendor:
        raise RuntimeError("dedicated vendor-data replaces image users")

    merged = util.mergemanydict([dedicated_vendor, image_config])
    if merged.get("users") != image_config["users"]:
        raise RuntimeError("image users did not survive cloud-init merging")
    for key in ("reporting", "write_files", "runcmd"):
        if merged.get(key) != dedicated_vendor.get(key):
            raise RuntimeError(f"vendor-data {key} changed during merging")

    generic_vendor = render_vendor_data(template, False)
    if generic_vendor.get("users", [{}])[0].get("name") != "unbounded-metal":
        raise RuntimeError("generic vendor-data lost the unbounded-metal user")


def installed_packages(status_path: Path) -> dict[str, str]:
    packages = {}
    for paragraph in status_path.read_text().split("\n\n"):
        fields = {}
        for line in paragraph.splitlines():
            if ": " in line:
                key, value = line.split(": ", 1)
                fields[key] = value
        if fields.get("Status") == "install ok installed":
            packages[fields.get("Package", "")] = fields.get("Version", "")
    return packages


def validate_agent_archive(
    archive_path: Path, expected_sha256: str = AGENT_ARCHIVE_SHA256
) -> None:
    digest = hashlib.sha256(archive_path.read_bytes()).hexdigest()
    if digest != expected_sha256:
        raise RuntimeError(
            f"agent archive checksum is {digest}, expected {expected_sha256}"
        )

    with tarfile.open(archive_path, "r:gz") as archive:
        if archive.getnames() != AGENT_ARCHIVE_MEMBERS:
            raise RuntimeError("agent archive members changed")
        member = archive.getmember("unbounded-agent")
        if not member.isfile() or member.mode & 0o111 == 0:
            raise RuntimeError("agent archive binary is not executable")
        binary = archive.extractfile(member)
        if binary is None:
            raise RuntimeError("agent archive binary cannot be read")
        header = binary.read(20)

    if len(header) < 20 or header[:4] != b"\x7fELF":
        raise RuntimeError("agent archive binary is not ELF")
    byte_order = {1: "<", 2: ">"}.get(header[5])
    if byte_order is None:
        raise RuntimeError("agent archive binary has invalid ELF byte order")
    machine = struct.unpack(f"{byte_order}H", header[18:20])[0]
    if machine != ELF_MACHINE_AARCH64:
        raise RuntimeError("agent archive binary is not ARM64")


def verify(
    image_config_path: Path,
    vendor_template_path: Path,
    status_path: Path,
    agent_archive_path: Path,
) -> None:
    image_config = load_mapping(image_config_path)
    validate_users(image_config)
    verify_merge(image_config, vendor_template_path)
    validate_agent_archive(agent_archive_path)
    packages = installed_packages(status_path)
    if packages.get("ssh-import-id") != SSH_IMPORT_ID_VERSION:
        raise RuntimeError("pinned ssh-import-id package is not installed")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--image-config", type=Path, required=True)
    parser.add_argument("--vendor-template", type=Path, required=True)
    parser.add_argument("--dpkg-status", type=Path, required=True)
    parser.add_argument("--agent-archive", type=Path, required=True)
    args = parser.parse_args()
    verify(
        args.image_config,
        args.vendor_template,
        args.dpkg_status,
        args.agent_archive,
    )


if __name__ == "__main__":
    main()
