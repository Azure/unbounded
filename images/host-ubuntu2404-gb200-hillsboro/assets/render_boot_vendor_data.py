#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Render MetalMan vendor-data for deterministic boot qualification."""

from __future__ import annotations

import argparse
from pathlib import Path
import re


REPORTING_TEMPLATE = """reporting:
  unbounded:
    type: webhook
    endpoint: {{ .ServeURL }}/cloudinit/log
"""
SAFE_REPORTING = """reporting:
  unbounded:
    type: log
"""
INSTALL_ENV_TEMPLATE = """    {{ range .InstallEnv }}
    export {{ . }}
    {{ end }}
"""


def indent_block(contents: str) -> str:
    return "\n".join(f"    {line}" for line in contents.rstrip().splitlines())


def render_vendor_data(
    template_path: Path,
    install_script_path: Path,
    ssh_import_id_path: Path,
) -> str:
    contents = template_path.read_text()
    if REPORTING_TEMPLATE not in contents:
        raise RuntimeError("MetalMan reporting template changed")
    contents = contents.replace(REPORTING_TEMPLATE, SAFE_REPORTING, 1)
    contents = contents.replace("{{ .ServeURL }}", "http://127.0.0.1:9")
    contents = contents.replace("{{ .AgentConfigJSON }}", "{}")
    contents = contents.replace(
        "{{ indent 4 .InstallScript }}",
        indent_block(install_script_path.read_text()),
    )
    if INSTALL_ENV_TEMPLATE not in contents:
        raise RuntimeError("MetalMan install environment template changed")
    contents = contents.replace(INSTALL_ENV_TEMPLATE, "", 1)

    helper = f"""write_files:
- path: /usr/local/bin/ssh-import-id
  permissions: '0755'
  content: |
{indent_block(ssh_import_id_path.read_text())}
"""
    if "write_files:\n" not in contents:
        raise RuntimeError("MetalMan write_files template changed")
    contents = contents.replace("write_files:\n", helper, 1)

    if "{{" in contents or "}}" in contents:
        raise RuntimeError("unrendered MetalMan vendor-data template expression")
    if re.search(r"^users:", contents, re.MULTILINE):
        raise RuntimeError("boot vendor-data must not replace image users")
    for key in ("reporting:", "write_files:", "runcmd:"):
        if contents.count(key) != 1:
            raise RuntimeError(f"boot vendor-data must contain exactly one {key}")

    return contents


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--template", type=Path, required=True)
    parser.add_argument("--install-script", type=Path, required=True)
    parser.add_argument("--ssh-import-id-script", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()

    args.output.write_text(
        render_vendor_data(
            args.template,
            args.install_script,
            args.ssh_import_id_script,
        )
    )


if __name__ == "__main__":
    main()
