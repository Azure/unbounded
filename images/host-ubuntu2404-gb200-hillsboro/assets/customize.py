#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Customize a pinned Metalman raw disk without mounting it."""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import tempfile

LINUX_FILESYSTEM_GUID = "0fc63daf-8483-4772-8e79-3d69d8477de4"
FORBIDDEN_ACCOUNTS = {"administrator", "unbounded", "unbounded-metal"}
PRIVATE_KEY_NAMES = {"id_dsa", "id_ecdsa", "id_ed25519", "id_rsa"}
LOCKED_PASSWORD = re.compile(r"^[!*]+$")


def run(*args: str, input_text: str | None = None) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        args,
        check=True,
        input=input_text,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )


def check_ext4(image: Path) -> None:
    result = subprocess.run(
        ["e2fsck", "-f", "-y", str(image)],
        check=False,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    if result.returncode not in {0, 1}:
        raise RuntimeError(
            f"e2fsck failed with status {result.returncode}: {result.stderr}"
        )


def parse_passwd(contents: str) -> dict[str, str]:
    users: dict[str, str] = {}
    for line in contents.splitlines():
        if not line:
            continue
        fields = line.split(":")
        if len(fields) != 7:
            raise RuntimeError("malformed /etc/passwd")
        users[fields[0]] = fields[5]
    return users


def validate_identity_state(
    passwd_contents: str,
    shadow_contents: str,
    credential_paths: list[str],
) -> None:
    users = parse_passwd(passwd_contents)
    captured_accounts = sorted(FORBIDDEN_ACCOUNTS.intersection(users))
    if captured_accounts:
        raise RuntimeError(
            "captured administrator account present: " + ", ".join(captured_accounts)
        )

    for line in shadow_contents.splitlines():
        if not line:
            continue
        fields = line.split(":")
        if len(fields) < 2:
            raise RuntimeError("malformed /etc/shadow")
        password = fields[1]
        if password and not LOCKED_PASSWORD.fullmatch(password):
            raise RuntimeError(f"captured password credential present for {fields[0]}")

    if credential_paths:
        raise RuntimeError(
            "captured SSH credential present: " + ", ".join(sorted(credential_paths))
        )


class Ext4:
    def __init__(self, image: Path):
        self.image = image

    def debugfs(self, command: str, *, write: bool = False) -> str:
        args = ["debugfs"]
        if write:
            args.append("-w")
        args.extend(["-R", command, str(self.image)])
        result = run(*args)
        return result.stdout

    def exists(self, path: str) -> bool:
        result = subprocess.run(
            ["debugfs", "-R", f"stat {path}", str(self.image)],
            check=False,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        return result.returncode == 0 and "Inode:" in result.stdout

    def dump(self, path: str) -> bytes:
        with tempfile.NamedTemporaryFile() as output:
            self.debugfs(f"dump -p {path} {output.name}")
            return Path(output.name).read_bytes()

    def entries(self, path: str) -> list[tuple[str, int]]:
        if not self.exists(path):
            return []
        entries = []
        for line in self.debugfs(f"ls -p {path}").splitlines():
            match = re.fullmatch(
                r"/\d+/([0-7]+)/\d+/\d+/(.*?)/(?:\d*)/", line.strip()
            )
            if not match:
                continue
            name = match.group(2)
            if name in {".", ".."}:
                continue
            entries.append((name, int(match.group(1), 8)))
        return entries

    def credential_paths(self, passwd_contents: str) -> list[str]:
        homes = set(parse_passwd(passwd_contents).values())
        homes.add("/root")
        credentials = []
        for home in sorted(homes):
            ssh_dir = f"{home.rstrip('/')}/.ssh"
            for name, mode in self.entries(ssh_dir):
                if name in {"authorized_keys", "authorized_keys2"} or name in PRIVATE_KEY_NAMES:
                    credentials.append(f"{ssh_dir}/{name}")
        return credentials

    def ensure_dir(self, path: str) -> None:
        if path == "/" or self.exists(path):
            return
        parent = str(Path(path).parent)
        self.ensure_dir(parent)
        self.debugfs(f"mkdir {path}", write=True)

    def remove_tree(self, path: str) -> None:
        if not self.exists(path):
            return
        for name, mode in self.entries(path):
            child = f"{path.rstrip('/')}/{name}"
            if stat.S_ISDIR(mode):
                self.remove_tree(child)
            else:
                self.debugfs(f"rm {child}", write=True)
        self.debugfs(f"rmdir {path}", write=True)

    def remove(self, path: str) -> None:
        if self.exists(path):
            self.debugfs(f"rm {path}", write=True)

    def write(self, source: Path, destination: str, mode: int) -> None:
        self.ensure_dir(str(Path(destination).parent))
        self.remove(destination)
        self.debugfs(f"write {source} {destination}", write=True)
        self.debugfs(
            f"set_inode_field {destination} mode 0{stat.S_IFREG | mode:o}",
            write=True,
        )
        if not self.exists(destination):
            raise RuntimeError(f"failed to write {destination}")

    def symlink(self, target: str, destination: str) -> None:
        self.ensure_dir(str(Path(destination).parent))
        self.remove(destination)
        self.debugfs(f"symlink {destination} {target}", write=True)
        if not self.exists(destination):
            raise RuntimeError(f"failed to create symlink {destination}")

    def install_tree(self, source: Path) -> None:
        for directory, names, files in os.walk(source):
            relative = Path(directory).relative_to(source)
            destination_dir = "/" if relative == Path(".") else f"/{relative}"
            self.ensure_dir(destination_dir)

            for name in names:
                path = Path(directory) / name
                destination = f"{destination_dir.rstrip('/')}/{name}"
                if path.is_symlink():
                    self.symlink(os.readlink(path), destination)

            for name in files:
                path = Path(directory) / name
                destination = f"{destination_dir.rstrip('/')}/{name}"
                if path.is_symlink():
                    self.symlink(os.readlink(path), destination)
                else:
                    self.write(path, destination, stat.S_IMODE(path.stat().st_mode))


def root_partition(disk: Path) -> tuple[int, int]:
    table = json.loads(run("sfdisk", "--json", str(disk)).stdout)["partitiontable"]
    sector_size = int(table.get("sectorsize", 512))
    candidates = [
        partition
        for partition in table["partitions"]
        if partition.get("type", "").lower() == LINUX_FILESYSTEM_GUID
    ]
    if len(candidates) != 1:
        raise RuntimeError(
            f"expected one Linux root partition, found {len(candidates)}"
        )
    partition = candidates[0]
    return int(partition["start"]) * sector_size, int(partition["size"]) * sector_size


def extract_partition(disk: Path, output: Path, offset: int, size: int) -> None:
    run(
        "dd",
        f"if={disk}",
        f"of={output}",
        "iflag=skip_bytes,count_bytes",
        f"skip={offset}",
        f"count={size}",
        "status=none",
    )


def replace_partition(disk: Path, source: Path, offset: int) -> None:
    run(
        "dd",
        f"if={source}",
        f"of={disk}",
        "oflag=seek_bytes",
        f"seek={offset}",
        "conv=notrunc",
        "status=none",
    )


def customize(disk: Path, assets: Path, ssh_import_id_package: Path) -> None:
    offset, size = root_partition(disk)
    root_image = disk.parent / "root.img"
    extract_partition(disk, root_image, offset, size)
    check_ext4(root_image)
    filesystem = Ext4(root_image)

    for required in (
        "/etc/passwd",
        "/etc/shadow",
        "/usr/bin/cloud-init",
        "/usr/bin/timeout",
        "/usr/sbin/useradd",
        "/usr/sbin/usermod",
        "/usr/sbin/visudo",
    ):
        if not filesystem.exists(required):
            raise RuntimeError(f"base image is missing required file: {required}")

    passwd_contents = filesystem.dump("/etc/passwd").decode()
    shadow_contents = filesystem.dump("/etc/shadow").decode()
    validate_identity_state(
        passwd_contents,
        shadow_contents,
        filesystem.credential_paths(passwd_contents),
    )

    filesystem.install_tree(ssh_import_id_package)
    if not filesystem.exists("/usr/bin/ssh-import-id"):
        raise RuntimeError("failed to install ssh-import-id")
    filesystem.remove_tree("/var/lib/cloud")
    for name, _ in filesystem.entries("/etc/ssh"):
        if name.startswith("ssh_host_"):
            filesystem.remove(f"/etc/ssh/{name}")

    with tempfile.NamedTemporaryFile() as empty:
        filesystem.write(Path(empty.name), "/etc/machine-id", 0o644)
        if filesystem.exists("/var/lib/dbus/machine-id"):
            filesystem.write(Path(empty.name), "/var/lib/dbus/machine-id", 0o644)

    filesystem.write(
        assets / "hillsboro-access",
        "/usr/local/sbin/unbounded-hillsboro-access",
        0o755,
    )
    filesystem.write(
        assets / "90-unbounded-dev",
        "/etc/sudoers.d/90-unbounded-dev",
        0o440,
    )
    filesystem.write(
        assets / "90-hillsboro-access.conf",
        "/etc/systemd/system/cloud-final.service.d/90-hillsboro-access.conf",
        0o644,
    )
    filesystem.write(
        assets / "access-policy.json",
        "/usr/share/unbounded/hillsboro-access.json",
        0o644,
    )

    if filesystem.dump("/etc/machine-id") != b"":
        raise RuntimeError("machine-id was not generalized")
    if (
        filesystem.exists("/var/lib/dbus/machine-id")
        and filesystem.dump("/var/lib/dbus/machine-id") != b""
    ):
        raise RuntimeError("D-Bus machine-id was not generalized")
    if any(name.startswith("ssh_host_") for name, _ in filesystem.entries("/etc/ssh")):
        raise RuntimeError("SSH host keys remain in the image")

    check_ext4(root_image)
    replace_partition(disk, root_image, offset)
    root_image.unlink()


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--disk", type=Path, required=True)
    parser.add_argument("--assets", type=Path, required=True)
    parser.add_argument("--ssh-import-id-package", type=Path, required=True)
    args = parser.parse_args()
    customize(args.disk, args.assets, args.ssh_import_id_package)


if __name__ == "__main__":
    main()
