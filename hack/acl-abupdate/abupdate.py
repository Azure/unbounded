#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""Drive A/B updates of Azure Container Linux with Trident in a local KVM VM, and check them.

    hack/acl-abupdate/abupdate.py image fetch [TAG...]   pull the installer ISO and COSIs from ACR
    hack/acl-abupdate/abupdate.py image build NAME ...   build a COSI with your files, to update to
    hack/acl-abupdate/abupdate.py up                     install the base image, save snapshot base
    hack/acl-abupdate/abupdate.py test                   run every scenario, each from snapshot base
    hack/acl-abupdate/abupdate.py run SCENARIO...        run scenarios; `scenarios` lists them
    hack/acl-abupdate/abupdate.py update --to TARGET     stage, finalize, and commit one A/B update
    hack/acl-abupdate/abupdate.py status                 the VM, its booted slot and image, Trident's state
    hack/acl-abupdate/abupdate.py down                   stop the VM and remove its state

The flow is the one ACL's KVM images document: the installer ISO streams the base image to the VM's
disk, an Ignition config on a volume labelled ACLSEED provisions SSH access on its first boot, and
`trident update`, given an image-only Host Configuration, writes a later COSI to the inactive A/B
slot. Only the download uses Azure. See README.md for the settings, commands, scenarios, and checks;
scenarios.py defines the scenarios.
"""

from __future__ import annotations

import argparse
import contextlib
import dataclasses
import datetime
import fcntl
import hashlib
import io
import json
import os
import re
import secrets
import shlex
import shutil
import signal
import socket
import subprocess
import sys
import tarfile
import textwrap
import time
import urllib.parse
import urllib.request
import uuid
from collections.abc import Callable, Iterator
from pathlib import Path
from typing import Any, NoReturn

sys.path.insert(0, str(Path(__file__).resolve().parent))
import cosibuild  # noqa: E402
from errors import StepError  # noqa: E402
from scenarios import REGISTRY, Needs, Scenario, Skip  # noqa: E402

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[1]
PROG = "hack/acl-abupdate/abupdate.py"


def _env_int(name: str, default: int) -> int:
    value = os.environ.get(name, "").strip()
    if not value:
        return default
    if not value.isdigit():
        print(f"[acl-ab] error: {name} must be a whole number, not {value!r}", file=sys.stderr)
        sys.exit(2)
    return int(value)


DEFAULT_STATE = REPO / ".vm" / "acl-abupdate"
STATE = Path(os.environ.get("ACL_AB_STATE_DIR", str(DEFAULT_STATE)))
# up makes the state directory, or takes an empty one, and marks it; down removes it only if it's
# the default or marked: ACL_AB_STATE_DIR could name any directory.
STATE_MARKER = STATE / ".acl-ab-state"
STATE_FILE = STATE / "state.json"
LOCK_FILE = STATE / "lock"
VM_DISK = STATE / "disk.qcow2"
# The UEFI variables are qcow2 rather than raw, so QEMU's snapshots include them: BootNext and
# BootOrder are part of what an A/B update changes.
VM_VARS = STATE / "vars.qcow2"
PID_FILE = STATE / "qemu.pid"
QMP_SOCKET = STATE / "qmp.sock"
SERIAL_LOG = STATE / "serial.log"
SNAPSHOT_DIR = STATE / "snapshots"
RUNS_DIR = STATE / "runs"
SSH_KEY = STATE / "ssh" / "id_ed25519"

# Downloads and installed base disks outlive down; remove the directory to reclaim the space.
CACHE = Path(os.environ.get("ACL_AB_CACHE_DIR", str(REPO / ".vm" / "acl-abupdate-cache")))
IMAGES = CACHE / "images"
INSTALLED = CACHE / "installed"
TOOLS = CACHE / "tools"
BUILDS = CACHE / "builds"
ORAS_AUTH = CACHE / "oras-auth.json"

# Where the images come from: an OCI registry and namespace, and the repositories of the KVM
# installer ISO and of the COSIs, each holding the file as an ORAS artifact's one layer, by build
# tag. There are no defaults: set them, or use local files. up keeps them with the VM's state, so
# later commands don't need them set; see use_images.
REGISTRY_PATH = os.environ.get("ACL_AB_REGISTRY", "")
ISO_REPO = os.environ.get("ACL_AB_ISO_REPO", "")
COSI_REPO = os.environ.get("ACL_AB_COSI_REPO", "")
BASE_TAG = os.environ.get("ACL_AB_BASE_TAG", "")
UPDATE_TAG = os.environ.get("ACL_AB_UPDATE_TAG", "")
# Local files instead of the registry's, for the base install and the base and update targets.
BASE_ISO = os.environ.get("ACL_AB_BASE_ISO", "")
BASE_COSI = os.environ.get("ACL_AB_BASE_COSI", "")
UPDATE_COSI = os.environ.get("ACL_AB_UPDATE_COSI", "")
# The subscription `az acr login` looks the registry up in; az's current one if unset.
SUBSCRIPTION = os.environ.get("ACL_AB_SUBSCRIPTION", "")
# ACR takes this placeholder username with an access token from `az acr login --expose-token`.
ACR_TOKEN_USER = "00000000-0000-0000-0000-000000000000"
# ACR access tokens last three hours; logging in again before then keeps a pull from failing midway.
ACR_TOKEN_SECONDS = 2 * 3600
ORAS_VERSION = "1.3.2"
ORAS_SHA256 = "9229ccc6d17bb282039ad4a69abb16dcb887a5bce567c075d731d9b3c7ad8eaf"
TAG_PATTERN = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}")
SNAPSHOT_PATTERN = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}")
# A build's name: lowercase, starting with a letter, so it's never a build tag, and usable in a URL
# path and in its semver VERSION_ID's prerelease.
BUILD_NAME_PATTERN = re.compile(r"[a-z][a-z0-9-]{0,39}")
# The Image Customizer container that `image build` runs; see cosibuild.py.
IMAGE_CUSTOMIZER = os.environ.get("ACL_AB_IMAGE_CUSTOMIZER", cosibuild.DEFAULT_IMAGE_CUSTOMIZER)

VM_MEMORY = _env_int("ACL_AB_VM_MEMORY", 6144)
VM_CPUS = _env_int("ACL_AB_VM_CPUS", 2)
DISK_SIZE = os.environ.get("ACL_AB_DISK_SIZE", "64G")
SSH_PORT = _env_int("ACL_AB_SSH_PORT", 2222)
# Where COSIs are copied in the VM. /tmp may be in memory, and a COSI is gigabytes.
GUEST_DIR = os.environ.get("ACL_AB_GUEST_DIR", "/var/tmp/acl-abupdate")
INSTALL_TIMEOUT = _env_int("ACL_AB_INSTALL_TIMEOUT", 2400)
BOOT_TIMEOUT = _env_int("ACL_AB_BOOT_TIMEOUT", 900)
STAGE_TIMEOUT = _env_int("ACL_AB_STAGE_TIMEOUT", 3600)
COMMAND_TIMEOUT = 600
SSH_USER = "core"

SEED_LABEL = "ACLSEED"
HEALTH_CHECK_NAME = "acl-abupdate-forced-failure"
# Written before each update, and checked after it, as the agent's state would have to survive:
# it is under /etc, /opt, and /var (docs/usage/azure-container-linux.md#os-updates).
MARKER_PATHS = (
    "/etc/acl-abupdate/marker",
    "/var/lib/acl-abupdate/marker",
    "/opt/acl-abupdate/marker",
    "/home/core/.acl-abupdate-marker",
)
# Trident's agent for annotation-driven A/B updates, which status reports on. Unbounded's host agent
# configures it on a node centerpivot updates; the harness leaves it as the image has it.
TRIDENT_AGENT_UNIT = "trident-acl-agent.service"
TRIDENT_SOCKET = "/run/trident/trident.sock"
# The VM's hostname, which the seed sets: the image's is localhost. `up --hostname` gives a VM its
# own, with its own machine ID, when several start from one installed base.
DEFAULT_HOSTNAME = "acl-abupdate"
# The second NIC, on a bridge the caller owns, which `up --nic-tap` attaches. QEMU's user NIC keeps
# QEMU's default MAC, and stays first, as eth0. The second is at a fixed PCI address, which the
# seed's network file matches, so VMs from one installed base can each have their own MAC.
NIC_PCI_SLOT = 0x10
NIC_NETWORK = "/etc/systemd/network/10-acl-abupdate-nic.network"
# The image's catch-all network runs DHCP on every link, and systemd-networkd-wait-online is
# enabled, so a NIC this file doesn't claim on its first boot would hold boot up waiting for a lease
# that never comes. Its address is a drop-in up writes.
NIC_NETWORK_CONTENTS = f"""\
[Match]
Path=pci-0000:00:{NIC_PCI_SLOT:02x}.0

[Link]
RequiredForOnline=no

[Network]
DHCP=no
LinkLocalAddressing=no
IPv6AcceptRA=no
"""
# Trident's servicingState values after a finalize reboot, which commit takes to provisioned.
FINALIZED_STATES = ("ab-update-finalized", "manual-rollback-ab-finalized")
HEALTH_FAILED_STATE = "ab-update-health-check-failed"
EFI_GLOBAL_GUID = "8be4df61-93ca-11d2-aa0d-00e098032b8c"

OVMF_CANDIDATES = [
    ("/usr/share/OVMF/OVMF_CODE.fd", "/usr/share/OVMF/OVMF_VARS.fd"),
    ("/usr/share/OVMF/OVMF_CODE_4M.fd", "/usr/share/OVMF/OVMF_VARS_4M.fd"),
    ("/usr/share/edk2/ovmf/OVMF_CODE.fd", "/usr/share/edk2/ovmf/OVMF_VARS.fd"),
    ("/usr/share/qemu/ovmf-x86_64-code.bin", "/usr/share/qemu/ovmf-x86_64-vars.bin"),
]

SSH_OPTS = [
    "-o", "StrictHostKeyChecking=no",
    "-o", "UserKnownHostsFile=/dev/null",
    "-o", "LogLevel=ERROR",
    "-o", "ConnectTimeout=10",
    "-o", "BatchMode=yes",
    "-o", "IdentitiesOnly=yes",
    "-o", "ServerAliveInterval=10",
    "-o", "ServerAliveCountMax=6",
    "-i", str(SSH_KEY),
]

HEALTH_CHECK_CONFIG = f"""\
health:
  checks:
  - name: {HEALTH_CHECK_NAME}
    runOn:
    - ab-update
    content: |
      echo "{HEALTH_CHECK_NAME}: this check fails on purpose"
      exit 1
"""


# ---------------------------------------------------------------------------------------------
# Process helpers

_json_output = False
_ssh_port = SSH_PORT


def _stream():
    return sys.stderr if _json_output else sys.stdout


def log(message: str) -> None:
    print(f"[acl-ab] {message}", file=_stream(), flush=True)


def warn(message: str) -> None:
    """Report a problem that doesn't fail the run. The message may hold another program's output.
    In GitHub Actions every line of the log is read for workflow commands, so it is printed once,
    escaped, as a warning, where a line break in it can't start a command."""
    if os.environ.get("GITHUB_ACTIONS") == "true":
        escaped = message.replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A")
        print(f"::warning::{escaped}", file=_stream(), flush=True)
    else:
        print(f"[acl-ab] warning: {message}", file=_stream(), flush=True)


def die(message: str, code: int = 2) -> NoReturn:
    print(f"[acl-ab] error: {message}", file=sys.stderr, flush=True)
    sys.exit(code)


def run(cmd: list[str], *, check: bool = True, capture: bool = False,
        input: str | None = None) -> subprocess.CompletedProcess[str]:
    result = subprocess.run(cmd, check=False, text=True, input=input,
                            stdout=subprocess.PIPE if capture else None,
                            stderr=subprocess.PIPE if capture else None)
    if check and result.returncode != 0:
        detail = f": {last_lines(result.stderr)}" if capture and result.stderr else ""
        raise StepError(f"{' '.join(cmd)} exited {result.returncode}{detail}")
    return result


def host_run(cmd: list[str], *, input: str | None = None, timeout: float | None = None,
             stream: bool = False) -> subprocess.CompletedProcess[str]:
    """The `run` the cluster modules take: it never raises, and leaves checking the status to them.
    With stream, the output goes where the log does, for long builds and pulls."""
    try:
        if stream:
            return subprocess.run(cmd, text=True, input=input, timeout=timeout, stdout=_stream(),
                                  stderr=subprocess.STDOUT, check=False)
        return subprocess.run(cmd, text=True, input=input, timeout=timeout, capture_output=True, check=False)
    except subprocess.TimeoutExpired:
        return subprocess.CompletedProcess(cmd, 124, "", f"timed out after {duration(timeout or 0)}")
    except FileNotFoundError as error:
        return subprocess.CompletedProcess(cmd, 127, "", str(error))


def poll(timeout: float, probe: Callable[[], bool], interval: float = 5) -> bool:
    """Return whether probe returned true within the timeout."""
    deadline = time.monotonic() + timeout
    while True:
        if probe():
            return True
        if time.monotonic() >= deadline:
            return False
        time.sleep(interval)


def duration(seconds: float) -> str:
    minutes, seconds = divmod(round(seconds), 60)
    return f"{minutes}m{seconds:02d}s" if minutes else f"{seconds}s"


def human_size(size: int) -> str:
    value = float(size)
    for unit in ("B", "KiB", "MiB", "GiB"):
        if value < 1024 or unit == "GiB":
            return f"{value:.1f} {unit}" if unit != "B" else f"{size} B"
        value /= 1024
    return f"{size} B"


def last_lines(text: str, count: int = 12) -> str:
    return "\n".join((text or "").strip().splitlines()[-count:])


def first_line(text: str) -> str:
    lines = (text or "").strip().splitlines()
    return lines[0] if lines else ""


def now_iso() -> str:
    return datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds")


def short(digest: str | None) -> str:
    return (digest or "")[:12] or "none"


# ---------------------------------------------------------------------------------------------
# Parsing

# The value parse_yaml gives a key with nested content.
BLOCK = "<block>"
_YAML_KEY = re.compile(r"""^("(?:[^"\\]|\\.)*"|'(?:[^']|'')*'|[^\s#'"-][^:]*?|-[^\s][^:]*?)\s*:(?:\s+(.*))?$""")
_BLOCK_SCALARS = {"|", "|-", "|+", ">", ">-", ">+"}


def yaml_scalar(value: str) -> str | None:
    value = value.strip()
    if value.startswith('"') and value.endswith('"') and len(value) >= 2:
        try:
            return str(json.loads(value))
        except json.JSONDecodeError:
            return value[1:-1]
    if value.startswith("'") and value.endswith("'") and len(value) >= 2:
        return value[1:-1].replace("''", "'")
    if " #" in value:
        value = value.split(" #", 1)[0].rstrip()
    return None if value in ("", "~", "null", "Null", "NULL") else value


def parse_yaml(text: str) -> dict[str, str | None]:
    """Flatten a YAML document's mappings to dotted keys, as far as reading Trident's Host Status
    needs: scalars, nested mappings, and block scalars. Sequences are skipped. A key with nested
    content or a sequence maps to BLOCK, and a key with neither to None. Python has no YAML parser
    of its own, and the harness uses the standard library only."""
    flat: dict[str, str | None] = {}
    parents: set[str] = set()
    stack: list[tuple[int, str]] = []
    sequence_indent: int | None = None
    block: tuple[str, int] | None = None
    block_lines: list[str] = []

    def path_of(entries: list[tuple[int, str]]) -> str:
        return ".".join(key for _, key in entries)

    for line in text.splitlines():
        stripped = line.strip()
        indent = len(line) - len(line.lstrip(" "))
        if block is not None:
            if not stripped or indent > block[1]:
                block_lines.append(line)
                continue
            flat[block[0]] = textwrap.dedent("\n".join(block_lines)).strip("\n")
            block = None
        if not stripped or stripped.startswith("#") or stripped in ("---", "..."):
            continue
        is_item = stripped == "-" or stripped.startswith("- ")
        if sequence_indent is not None:
            if indent > sequence_indent or (indent == sequence_indent and is_item):
                continue
            sequence_indent = None
        if is_item:
            # A sequence belongs to the key above it, whose indent may equal the items'.
            while stack and stack[-1][0] > indent:
                stack.pop()
            if stack:
                parents.add(path_of(stack))
            sequence_indent = indent
            continue
        while stack and stack[-1][0] >= indent:
            stack.pop()
        match = _YAML_KEY.match(stripped)
        if not match:
            continue
        key = yaml_scalar(match.group(1)) or ""
        if stack:
            parents.add(path_of(stack))
        path = ".".join([*(k for _, k in stack), key])
        value = (match.group(2) or "").strip()
        if value in _BLOCK_SCALARS:
            block, block_lines = (path, indent), []
        elif value == "":
            flat[path] = BLOCK
            stack.append((indent, key))
        else:
            flat[path] = yaml_scalar(value)
    if block is not None:
        flat[block[0]] = textwrap.dedent("\n".join(block_lines)).strip("\n")
    return {key: None if value == BLOCK and key not in parents else value for key, value in flat.items()}


def yaml_block(text: str, key: str) -> str:
    """The text of a top-level key's value, for showing a nested value such as lastError."""
    lines: list[str] = []
    inside = False
    for line in text.splitlines():
        if inside:
            if line and not line[0].isspace() and not line.startswith("- "):
                break
            lines.append(line)
        elif line.startswith(f"{key}:"):
            rest = line[len(key) + 1:].strip()
            if rest:
                return rest
            inside = True
    return textwrap.dedent("\n".join(lines)).strip()


def parse_sections(text: str) -> dict[str, str]:
    """Split the probe script's output into its `### name` sections."""
    sections: dict[str, list[str]] = {}
    current: list[str] | None = None
    for line in text.splitlines():
        if line.startswith("### "):
            current = sections.setdefault(line[4:].strip(), [])
        elif current is not None:
            current.append(line)
    return {name: "\n".join(lines).strip("\n") for name, lines in sections.items()}


def parse_verity(text: str) -> dict[str, str]:
    """`veritysetup status`'s fields, keyed by their lowercased names: data device, hash device,
    root hash, status."""
    fields: dict[str, str] = {}
    for line in text.splitlines():
        key, separator, value = line.strip().partition(":")
        if separator and key and value.strip():
            fields[key.strip().lower()] = value.strip()
    return fields


def parse_os_release(text: str) -> dict[str, str]:
    values: dict[str, str] = {}
    for line in text.splitlines():
        key, separator, value = line.strip().partition("=")
        if not separator or not key or key.startswith("#"):
            continue
        try:
            parts = shlex.split(value)
        except ValueError:
            parts = [value.strip("\"'")]
        values[key] = parts[0] if parts else ""
    return values


def parse_properties(text: str) -> dict[str, str]:
    """KEY=value lines, as `systemctl show` prints them."""
    return dict(line.split("=", 1) for line in text.splitlines() if "=" in line)


def efi_payload(hex_value: str) -> bytes:
    """A UEFI variable's data, from efivarfs's hex, which starts with 4 bytes of attributes."""
    try:
        return bytes.fromhex(hex_value)[4:]
    except ValueError:
        return b""


def efi_boot_number(hex_value: str) -> str:
    data = efi_payload(hex_value)
    return f"{int.from_bytes(data[:2], 'little'):04X}" if len(data) >= 2 else ""


def efi_boot_order(hex_value: str) -> list[str]:
    data = efi_payload(hex_value)
    return [f"{int.from_bytes(data[i:i + 2], 'little'):04X}" for i in range(0, len(data) - 1, 2)]


def efi_load_option_description(hex_value: str) -> str:
    """The description of a Boot#### variable's EFI_LOAD_OPTION: a null-terminated UCS-2 string
    after its 4-byte attributes and 2-byte file path list length."""
    data = efi_payload(hex_value)[6:]
    for end in range(0, len(data) - 1, 2):
        if data[end:end + 2] == b"\0\0":
            return data[:end].decode("utf-16-le", "replace")
    return ""


def iso_volume_id(path: Path) -> str | None:
    """The volume identifier of an ISO 9660 image's primary volume descriptor, or None if path
    isn't one."""
    with path.open("rb") as image:
        image.seek(0x8000)
        descriptor = image.read(72)
    if len(descriptor) < 72 or descriptor[1:6] != b"CD001":
        return None
    return descriptor[40:72].decode("ascii", "replace").strip()


def read_cosi(path: Path) -> dict[str, Any]:
    """What the harness needs of a COSI (docs/Reference/Composable-OS-Image.md in Trident's
    repository): the SHA-384 of its metadata.json, which a Host Configuration's image.sha384
    names, and the verity root hashes of its filesystems, which identify the image booted."""
    with path.open("rb") as archive:
        if archive.read(11) != b"cosi-marker":
            raise ValueError(f"{path} isn't a COSI: it doesn't start with the cosi-marker entry")
    raw = None
    with tarfile.open(path, "r:") as tar:
        # metadata.json is the second entry, so this reads no further.
        for member in tar:
            if member.name == "metadata.json" and member.isfile():
                extracted = tar.extractfile(member)
                raw = extracted.read() if extracted else None
                break
    if raw is None:
        raise ValueError(f"{path} has no metadata.json")
    metadata = json.loads(raw)
    roothashes = {}
    for image in metadata.get("images") or []:
        verity = image.get("verity") or {}
        if verity.get("roothash"):
            roothashes[str(image.get("mountPoint", ""))] = str(verity["roothash"])
    os_release = parse_os_release(str(metadata.get("osRelease", "")))
    return {
        "metadataSha384": hashlib.sha384(raw).hexdigest(),
        "cosiVersion": str(metadata.get("version", "")),
        "osArch": str(metadata.get("osArch", "")),
        "roothashes": roothashes,
        "roothash": roothashes.get("/usr") or roothashes.get("/", ""),
        "osRelease": {key: os_release[key] for key in ("NAME", "VERSION", "VERSION_ID", "BUILD_ID", "PRETTY_NAME")
                      if key in os_release},
    }


# ---------------------------------------------------------------------------------------------
# Images


@dataclasses.dataclass(frozen=True)
class Image:
    """A downloaded or local installer ISO or COSI."""

    name: str
    path: Path
    kind: str
    info: dict[str, Any] = dataclasses.field(default_factory=dict, compare=False, hash=False)

    @property
    def roothash(self) -> str:
        return str(self.info.get("roothash", ""))

    @property
    def sha384(self) -> str:
        return str(self.info.get("metadataSha384", ""))

    @property
    def version(self) -> str:
        release = self.info.get("osRelease") or {}
        return str(release.get("VERSION") or release.get("VERSION_ID") or "")

    @property
    def build(self) -> str:
        return str((self.info.get("annotations") or {}).get("com.microsoft.acl.buildNumber", ""))

    @property
    def built(self) -> dict[str, Any]:
        """What `image build` recorded of the build this image is, or nothing for any other image."""
        return self.info.get("built") or {}

    def describe(self) -> str:
        parts = [f"{self.kind}", human_size(int(self.info.get("size", 0)))]
        if self.build:
            parts.append(f"build {self.build}")
        if self.built:
            parts.append(f"built from {self.built.get('from', {}).get('name', '?')}, version {self.built.get('version', '?')}")
        if self.kind == "cosi":
            parts += [f"OS {self.version or '?'}", f"/usr root hash {short(self.roothash)}"]
        return f"{self.name}: {', '.join(parts)}"


def describe_file(path: Path, kind: str) -> dict[str, Any]:
    info: dict[str, Any] = {"file": path.name, "size": path.stat().st_size}
    if kind == "cosi":
        try:
            info.update(read_cosi(path))
        except (ValueError, OSError, tarfile.TarError, json.JSONDecodeError) as error:
            raise StepError(f"can't read {path} as a COSI: {error}") from error
        if info["osArch"].lower() not in ("x86_64", "amd64", "x64"):
            raise StepError(f"{path.name} is for {info['osArch']}; the VM is x86_64")
    elif iso_volume_id(path) is None:
        raise StepError(f"{path.name} isn't an ISO 9660 image")
    return info


_local_images: dict[Path, Image] = {}


def local_image(path: Path, kind: str, name: str = "") -> Image:
    path = path.expanduser().resolve()
    if path not in _local_images:
        if not path.is_file():
            raise StepError(f"{path} doesn't exist")
        _local_images[path] = Image(name or path.name, path, kind, describe_file(path, kind))
    return _local_images[path]


def cached_image(directory: Path, kind: str) -> Image | None:
    try:
        info = json.loads((directory / "info.json").read_text())
    except (OSError, json.JSONDecodeError):
        return None
    path = directory / str(info.get("file", ""))
    return Image(directory.name, path, kind, info) if path.is_file() else None


def oras_binary() -> str:
    """oras from PATH, or ORAS_VERSION downloaded to the cache and checked against its digest."""
    found = shutil.which("oras")
    if found:
        return found
    binary = TOOLS / f"oras-{ORAS_VERSION}" / "oras"
    if binary.is_file():
        return str(binary)
    url = (f"https://github.com/oras-project/oras/releases/download/v{ORAS_VERSION}/"
           f"oras_{ORAS_VERSION}_linux_amd64.tar.gz")
    log(f"oras isn't on PATH; downloading ORAS {ORAS_VERSION} to {binary.parent}")
    try:
        with urllib.request.urlopen(url, timeout=300) as response:
            archive = response.read()
    except OSError as error:
        raise StepError(f"couldn't download ORAS from {url}: {error}; install it "
                        "(https://oras.land/docs/installation)") from error
    digest = hashlib.sha256(archive).hexdigest()
    if digest != ORAS_SHA256:
        raise StepError(f"the ORAS download's sha256 is {digest}, not {ORAS_SHA256}")
    with tarfile.open(fileobj=io.BytesIO(archive), mode="r:gz") as tar:
        member = tar.extractfile("oras")
        if member is None:
            raise StepError("the ORAS archive has no oras binary")
        content = member.read()
    binary.parent.mkdir(parents=True, exist_ok=True)
    partial = binary.with_suffix(".partial")
    partial.write_bytes(content)
    partial.chmod(0o755)
    os.replace(partial, binary)
    return str(binary)


def oras(*args: str, capture: bool = True) -> subprocess.CompletedProcess[str]:
    # A registry config of the harness's own keeps the token out of ~/.docker/config.json.
    command = [oras_binary(), *args, "--registry-config", str(ORAS_AUTH)]
    if capture:
        return subprocess.run(command, text=True, capture_output=True, check=False)
    # The progress goes where the log does, so --json leaves stdout to the result.
    return subprocess.run(command, text=True, stdout=_stream(), check=False)


def registry_host() -> str:
    return REGISTRY_PATH.split("/", 1)[0]


def missing_settings(names: list[str]) -> str:
    """The error for image settings that aren't set, which have no defaults."""
    names = [name for name in names if name]
    return (f"set {', '.join(names)}: the images' registry, repositories, and tags have no defaults; see "
            "Settings in hack/acl-abupdate/README.md. Local files can stand in, with ACL_AB_BASE_ISO, "
            "ACL_AB_BASE_COSI, and ACL_AB_UPDATE_COSI")


def use_images(state: State) -> None:
    """Take the image settings the VM was brought up with, so later commands don't need them set.
    Those up didn't record, as a VM brought up before it did, come from the environment."""
    global REGISTRY_PATH, ISO_REPO, COSI_REPO
    REGISTRY_PATH = state.registry or REGISTRY_PATH
    ISO_REPO = state.iso_repo or ISO_REPO
    COSI_REPO = state.cosi_repo or COSI_REPO


def acr_login() -> None:
    """Log oras in to the registry with an ACR token from az, passed on stdin."""
    host = registry_host()
    if not shutil.which("az"):
        raise StepError(f"az isn't installed; it gets the token oras pulls from {host} with")
    command = ["az", "acr", "login", "--name", host.split(".", 1)[0], "--expose-token",
               "--query", "accessToken", "--output", "tsv"]
    if SUBSCRIPTION:
        command += ["--subscription", SUBSCRIPTION]
    log(f"getting an ACR token for {host} from az")
    result = run(command, check=False, capture=True)
    token = result.stdout.strip()
    if result.returncode != 0 or not token:
        raise StepError(f"az couldn't get a token for {host}: {last_lines(result.stderr, 4)}\n"
                        "Log in with `az login` to the tenant the registry is in, and set ACL_AB_SUBSCRIPTION "
                        "if it isn't in az's current subscription")
    CACHE.mkdir(parents=True, exist_ok=True)
    if not ORAS_AUTH.exists():
        with os.fdopen(os.open(ORAS_AUTH, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "w") as out:
            out.write("{}\n")
    login = subprocess.run([oras_binary(), "login", host, "--registry-config", str(ORAS_AUTH),
                            "--username", ACR_TOKEN_USER, "--password-stdin"],
                           input=token, text=True, capture_output=True, check=False)
    if login.returncode != 0:
        raise StepError(f"oras login {host} failed: {last_lines(login.stderr, 4)}")


_AUTH_ERROR = re.compile(r"unauthori[sz]ed|denied|\b401\b|\b403\b|credential|authenticat", re.IGNORECASE)


def oras_remote(*args: str) -> subprocess.CompletedProcess[str]:
    """Run an oras command against the registry, logging in first if the token may have expired,
    and again if the registry refuses it."""
    fresh = ORAS_AUTH.is_file() and time.time() - ORAS_AUTH.stat().st_mtime < ACR_TOKEN_SECONDS
    if not fresh:
        acr_login()
    result = oras(*args)
    if result.returncode != 0 and _AUTH_ERROR.search(result.stderr):
        acr_login()
        result = oras(*args)
    return result


def fetch_artifact(repo: str, tag: str, kind: str, *, refresh: bool = False) -> Image:
    """Pull a tag's ISO or COSI into the cache, unless it's there. The artifact holds the file
    itself as its one layer, so oras pull gives it as it was built."""
    if not TAG_PATTERN.fullmatch(tag):
        raise StepError(f"{tag!r} isn't an image tag")
    if not REGISTRY_PATH or not repo:
        raise StepError(missing_settings(["ACL_AB_REGISTRY" if not REGISTRY_PATH else "",
                                          ("ACL_AB_ISO_REPO" if kind == "iso" else "ACL_AB_COSI_REPO") if not repo else ""]))
    directory = IMAGES / repo / tag
    if not refresh:
        cached = cached_image(directory, kind)
        if cached:
            return cached
    reference = f"{REGISTRY_PATH}/{repo}:{tag}"
    manifest = oras_remote("manifest", "fetch", reference)
    if manifest.returncode != 0:
        detail = last_lines(manifest.stderr, 4)
        if "not found" in detail.lower():
            raise StepError(f"{reference} isn't published: {detail}\n"
                            f"List the tags with `oras repo tags {REGISTRY_PATH}/{repo}`")
        raise StepError(f"oras couldn't fetch the manifest of {reference}: {detail}")
    try:
        annotations = json.loads(manifest.stdout).get("annotations") or {}
    except (json.JSONDecodeError, AttributeError):
        annotations = {}
    partial = directory.with_name(f"{tag}.partial")
    shutil.rmtree(partial, ignore_errors=True)
    partial.mkdir(parents=True)
    (partial / "manifest.json").write_text(manifest.stdout)
    build = annotations.get("com.microsoft.acl.buildNumber", "")
    log(f"pulling {reference}" + (f" (build {build})" if build else ""))
    pulled = oras("pull", "--output", str(partial), reference, capture=False)
    if pulled.returncode != 0:
        raise StepError(f"oras pull {reference} exited {pulled.returncode}")
    files = sorted(path for path in partial.iterdir() if path.is_file() and path.suffix == f".{kind}")
    if len(files) != 1:
        raise StepError(f"{reference} should hold one .{kind} file; it holds "
                        f"{', '.join(path.name for path in partial.iterdir())}")
    info = describe_file(files[0], kind)
    info.update(reference=reference, annotations=annotations, pulled=now_iso())
    (partial / "info.json").write_text(json.dumps(info, indent=2) + "\n")
    shutil.rmtree(directory, ignore_errors=True)
    partial.rename(directory)
    image = cached_image(directory, kind)
    if image is None:
        raise StepError(f"the pull of {reference} left no image in {directory}")
    log(f"pulled {image.describe()}")
    return image


def base_iso(tag: str, *, refresh: bool = False) -> Image:
    if BASE_ISO:
        return local_image(Path(BASE_ISO), "iso", tag)
    return fetch_artifact(ISO_REPO, tag, "iso", refresh=refresh)


def cosi(tag: str, state: State | None = None, *, refresh: bool = False) -> Image:
    local = local_cosi(tag, state)
    return local if local else fetch_artifact(COSI_REPO, tag, "cosi", refresh=refresh)


def local_cosi(tag: str, state: State | None = None) -> Image | None:
    """The COSI ACL_AB_BASE_COSI or ACL_AB_UPDATE_COSI names for a tag, if either does."""
    base_tag, update_tag = (state.base_tag, state.update_tag) if state else (BASE_TAG, UPDATE_TAG)
    if BASE_COSI and tag == base_tag:
        return local_image(Path(BASE_COSI), "cosi", tag)
    if UPDATE_COSI and tag == update_tag:
        return local_image(Path(UPDATE_COSI), "cosi", tag)
    return None


def cosi_if_present(tag: str, state: State | None = None) -> Image | None:
    """A tag's COSI if it's local or cached, without pulling it."""
    local = local_cosi(tag, state)
    if local or not COSI_REPO:
        return local
    return cached_image(IMAGES / COSI_REPO / tag, "cosi")


def build_image(name: str) -> Image | None:
    """The build of that name, if `image build` made one."""
    return cached_image(BUILDS / name, "cosi") if BUILD_NAME_PATTERN.fullmatch(name) else None


def known_builds() -> list[Image]:
    builds = [build_image(directory.name) for directory in sorted(BUILDS.iterdir())] if BUILDS.is_dir() else []
    return [image for image in builds if image]


# https://semver.org's pattern. Nebraska compares versions as semver, and trident-acl-agent reports
# VERSION_ID as the node's version, so a build's has to be canonical semver.
SEMVER_PATTERN = re.compile(
    r"(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)"
    r"(?:-(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*)?"
    r"(?:\+[0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*)?", re.ASCII)


HOSTNAME_PATTERN = re.compile(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", re.ASCII)


def is_semver(version: str) -> bool:
    return SEMVER_PATTERN.fullmatch(version) is not None


def build_version(name: str, source: Image, previous: Image | None, requested: str = "") -> tuple[str, int]:
    """A build's VERSION_ID, and which build of its name it is. By default it's its source's with
    -NAME.N for the Nth build of NAME, so an update to it changes the OS version, as one to a new
    release of ACL does. Semver sorts that below the source's own, so Nebraska won't offer it over
    the source; --os-version gives one that sorts above. Empty, when the source has no semver
    VERSION_ID and nothing is requested, keeps the source's os-release."""
    count = int(previous.built.get("count", 0) if previous else 0) + 1
    if requested:
        return requested, count
    release = source.info.get("osRelease") or {}
    core = re.split(r"[-+]", str(release.get("VERSION_ID", "")), maxsplit=1)[0]
    return (f"{core}-{name}.{count}" if is_semver(core) else ""), count


def known_cosis() -> list[Image]:
    """The COSIs the harness has seen: the cache's, builds, and local ones, to name the booted image."""
    images: list[Image] = []
    repo = IMAGES / COSI_REPO
    if COSI_REPO and repo.is_dir():
        for directory in sorted(repo.iterdir()):
            if directory.is_dir() and not directory.name.endswith(".partial"):
                image = cached_image(directory, "cosi")
                if image:
                    images.append(image)
    images += known_builds()
    for value, tag in ((BASE_COSI, BASE_TAG), (UPDATE_COSI, UPDATE_TAG)):
        if value:
            with contextlib.suppress(StepError):
                local_image(Path(value), "cosi", tag)
    images += [image for image in _local_images.values() if image.kind == "cosi"]
    return images


def image_name(roothash: str) -> str:
    if not roothash:
        return "unknown"
    for image in known_cosis():
        if image.roothash == roothash:
            return image.name
    return f"unknown image ({short(roothash)})"


def pick_other(roothash: str, base: Image, update: Image) -> Image:
    """The one of base and update that the VM doesn't run."""
    if roothash == base.roothash:
        return update
    if roothash == update.roothash:
        return base
    raise StepError(f"the VM runs neither {base.name} nor {update.name} (/usr root hash "
                    f"{short(roothash)}), so `other` names neither; name a target")


def resolve_target(spec: str, state: State, current_roothash: Callable[[], str]) -> Image:
    """A target: base, update, other (whichever of those the VM doesn't run), a build, a tag, or a COSI."""
    if spec == "base":
        return cosi(state.base_tag, state)
    if spec == "update":
        return cosi(state.update_tag, state)
    if spec == "other":
        return pick_other(current_roothash(), cosi(state.base_tag, state), cosi(state.update_tag, state))
    built = build_image(spec)
    if built:
        return built
    path = Path(spec).expanduser()
    if spec.endswith(".cosi") or "/" in spec or path.is_file():
        return local_image(path, "cosi")
    if TAG_PATTERN.fullmatch(spec):
        return cosi(spec, state)
    raise StepError(f"{spec!r} isn't a target: name base, update, other, a build, a tag, or a .cosi file")


# ---------------------------------------------------------------------------------------------
# Ignition seed


def ignition_config(public_key: str) -> dict[str, Any]:
    """The first-boot config ACL's KVM images document: user core, with the harness's key, in sudo and
    systemd-journal, and passwordless sudo, which is fine for this disposable VM. It also sets the
    hostname, and claims the second NIC, which `up --nic-tap` attaches."""
    return {
        "ignition": {"version": "3.4.0"},
        "passwd": {"users": [{
            "name": SSH_USER,
            "uid": 500,
            "homeDir": f"/home/{SSH_USER}",
            "shell": "/bin/bash",
            "groups": ["sudo", "systemd-journal"],
            "sshAuthorizedKeys": [public_key],
        }]},
        "storage": {"files": [
            {
                "path": f"/etc/sudoers.d/90-{SSH_USER}",
                "mode": 0o440,
                "contents": {"source": f"data:,{SSH_USER}%20ALL%3D%28ALL%29%20NOPASSWD%3A%20ALL%0A"},
            },
            {
                "path": "/etc/hostname",
                "mode": 0o644,
                "overwrite": True,
                "contents": {"source": "data:," + urllib.parse.quote(f"{DEFAULT_HOSTNAME}\n")},
            },
            {
                "path": NIC_NETWORK,
                "mode": 0o644,
                "contents": {"source": "data:," + urllib.parse.quote(NIC_NETWORK_CONTENTS)},
            },
        ]},
    }


def seed_digest() -> str:
    """A digest of what the seed provisions, leaving out the SSH key, which each install makes.
    The image reads the seed on its first boot only, so a cached install whose digest differs has
    to be installed again."""
    return hashlib.sha256(json.dumps(ignition_config(""), sort_keys=True).encode()).hexdigest()


def make_iso(source: Path, iso: Path, label: str) -> None:
    for tool in ("xorriso", "genisoimage", "mkisofs"):
        if shutil.which(tool):
            prefix = [tool, "-as", "mkisofs"] if tool == "xorriso" else [tool]
            run([*prefix, "-quiet", "-V", label, "-J", "-R", "-o", str(iso), str(source)], capture=True)
            return
    if shutil.which("bsdtar"):
        run(["bsdtar", "-c", "-f", str(iso), "--format", "iso9660", "--options",
             f"iso9660:volume-id={label},iso9660:rockridge,iso9660:joliet", "-C", str(source),
             *sorted(path.name for path in source.iterdir())], capture=True)
        return
    raise StepError("no tool to make the seed ISO with; install xorriso, genisoimage, or bsdtar (libarchive)")


def build_seed(directory: Path, public_key: str) -> Path:
    """The configuration media the installed image waits for on its first boot: a volume labelled
    ACLSEED holding /ignition/config.ign."""
    source = directory / "seed"
    (source / "ignition").mkdir(parents=True, exist_ok=True)
    (source / "ignition" / "config.ign").write_text(json.dumps(ignition_config(public_key), indent=2) + "\n")
    iso = directory / "seed.iso"
    iso.unlink(missing_ok=True)
    make_iso(source, iso, SEED_LABEL)
    label = iso_volume_id(iso)
    if label != SEED_LABEL:
        raise StepError(f"the seed ISO's volume label is {label!r}, not {SEED_LABEL}")
    return iso


# ---------------------------------------------------------------------------------------------
# VM


def ovmf_firmware() -> tuple[Path, Path]:
    for code, variables in OVMF_CANDIDATES:
        if Path(code).is_file() and Path(variables).is_file():
            return Path(code), Path(variables)
    die("OVMF firmware not found; install the ovmf or edk2-ovmf package")


def check_vm_prerequisites() -> None:
    missing = [tool for tool in ("qemu-system-x86_64", "qemu-img", "ssh", "ssh-keygen", "setsid")
               if not shutil.which(tool)]
    if missing:
        die(f"missing {', '.join(missing)}; install QEMU (qemu-system-x86 and qemu-utils, or "
            "qemu-kvm) and OpenSSH")
    if not os.access("/dev/kvm", os.R_OK | os.W_OK):
        die("/dev/kvm isn't readable and writable; add yourself to the kvm group")
    ovmf_firmware()
    if len(str(QMP_SOCKET)) > 100:
        die(f"{QMP_SOCKET} is too long for a unix socket; set ACL_AB_STATE_DIR to a shorter path")


def port_free(port: int) -> bool:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe_socket:
        probe_socket.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        try:
            probe_socket.bind(("127.0.0.1", port))
        except OSError:
            return False
    return True


def qemu_command(*, code: Path, disk: Path, variables: Path, memory: int, cpus: int, ssh_port: int,
                 installer: Path | None = None, seed: Path | None = None, loadvm: str = "",
                 nic_tap: str = "", nic_mac: str = "", smbios_uuid: str = "", paused: bool = False) -> list[str]:
    """QEMU's command line. Each device's bootindex makes OVMF try the disk first, then the
    installer; -boot order isn't enough for OVMF. User networking forwards SSH from 127.0.0.1
    only. The second NIC, on a TAP device in the caller's bridge, comes after it, so the user
    NIC stays eth0 and keeps carrying SSH, the default route, and DNS. An SMBIOS UUID gives the VM
    a /sys/class/dmi/id/product_uuid, as an Azure VM has, which kubelet reports as the Node's
    systemUUID. paused starts it stopped, as `cont` resumes."""
    command = [
        "setsid", "qemu-system-x86_64", "-name", "acl-abupdate",
        "-machine", "q35", "-cpu", "host", "-accel", "kvm", "-m", str(memory), "-smp", str(cpus),
    ]
    if smbios_uuid:
        command += ["-uuid", smbios_uuid]
    command += [
        "-drive", f"if=pflash,format=raw,unit=0,readonly=on,file={code}",
        "-drive", f"if=pflash,format=qcow2,unit=1,file={variables}",
        "-drive", f"if=none,id=osdisk,format=qcow2,file={disk}",
        "-device", "virtio-blk-pci,drive=osdisk,bootindex=1",
    ]
    if installer:
        command += ["-drive", f"if=none,id=installer,media=cdrom,readonly=on,format=raw,file={installer}",
                    "-device", "ide-cd,bus=ide.0,drive=installer,bootindex=2"]
    if seed:
        command += ["-drive", f"if=none,id=seed,media=cdrom,readonly=on,format=raw,file={seed}",
                    "-device", "ide-cd,bus=ide.1,drive=seed"]
    command += ["-nic", f"user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:{ssh_port}-:22"]
    if nic_tap:
        command += ["-netdev", f"tap,id=nic1,ifname={nic_tap},script=no,downscript=no",
                    "-device", f"virtio-net-pci,netdev=nic1,mac={nic_mac},addr={NIC_PCI_SLOT:#x}"]
    command += [
        "-chardev", f"file,id=serial0,path={SERIAL_LOG},append=on", "-serial", "chardev:serial0",
        "-qmp", f"unix:{QMP_SOCKET},server=on,wait=off",
        "-display", "none", "-daemonize", "-pidfile", str(PID_FILE),
    ]
    if loadvm:
        command += ["-loadvm", loadvm]
    if paused:
        command.append("-S")
    return command


def launch_vm(*, disk: Path, variables: Path, memory: int, cpus: int, ssh_port: int,
              installer: Path | None = None, seed: Path | None = None, loadvm: str = "",
              nic_tap: str = "", nic_mac: str = "", smbios_uuid: str = "", paused: bool = False) -> None:
    """Start QEMU in the background, as qemu_command describes. The second NIC's TAP device has to
    exist, owned by the user, as the caller's bridge setup creates it."""
    if vm_pid() is not None:
        raise StepError("the VM is running already")
    for path in (disk, variables, installer, seed):
        if path is not None and "," in str(path):
            raise StepError(f"QEMU can't take a path with a comma: {path}")
    if not port_free(ssh_port):
        raise StepError(f"127.0.0.1:{ssh_port} is in use; pass up another --ssh-port, or set ACL_AB_SSH_PORT")
    if nic_tap and not Path(f"/sys/class/net/{nic_tap}").exists():
        raise StepError(f"the TAP device {nic_tap} doesn't exist; set up its bridge first")
    code, _ = ovmf_firmware()
    QMP_SOCKET.unlink(missing_ok=True)
    PID_FILE.unlink(missing_ok=True)
    run(qemu_command(code=code, disk=disk, variables=variables, memory=memory, cpus=cpus, ssh_port=ssh_port,
                     installer=installer, seed=seed, loadvm=loadvm, nic_tap=nic_tap, nic_mac=nic_mac,
                     smbios_uuid=smbios_uuid, paused=paused), capture=True)
    log(f"QEMU started (pid {PID_FILE.read_text().strip()}); serial log {SERIAL_LOG}")


def vm_pid() -> int | None:
    """The VM's QEMU process while it runs. A PID can be reused, so the process has to be QEMU
    started with this harness's PID file."""
    try:
        pid = int(PID_FILE.read_text().strip())
        args = Path(f"/proc/{pid}/cmdline").read_bytes().split(b"\0")
    except (OSError, ValueError):
        return None
    if not args or b"qemu-system" not in args[0] or str(PID_FILE).encode() not in args:
        return None
    return pid


class QMP:
    """A client for QEMU's machine protocol socket: hard resets, quitting, and snapshots, which
    go through the human monitor's savevm and loadvm, as QMP has no synchronous command for them."""

    def __init__(self, timeout: float = 30) -> None:
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(timeout)
        try:
            self.sock.connect(str(QMP_SOCKET))
        except OSError as error:
            self.sock.close()
            raise StepError(f"can't reach QEMU's control socket {QMP_SOCKET}: {error}") from error
        self.stream = self.sock.makefile("rwb")
        self._receive()
        self.execute("qmp_capabilities")

    def __enter__(self) -> QMP:
        return self

    def __exit__(self, *_: object) -> None:
        with contextlib.suppress(OSError):
            self.stream.close()
        with contextlib.suppress(OSError):
            self.sock.close()

    def _receive(self) -> dict[str, Any]:
        while True:
            line = self.stream.readline()
            if not line:
                raise StepError("QEMU closed its control socket")
            message = json.loads(line)
            if "event" not in message:
                return message

    def execute(self, command: str, arguments: dict[str, Any] | None = None, *,
                timeout: float | None = None) -> Any:
        if timeout is not None:
            self.sock.settimeout(timeout)
        request: dict[str, Any] = {"execute": command}
        if arguments:
            request["arguments"] = arguments
        try:
            self.stream.write(json.dumps(request).encode() + b"\n")
            self.stream.flush()
            reply = self._receive()
        except (OSError, json.JSONDecodeError) as error:
            raise StepError(f"QEMU {command}: {error}") from error
        if "error" in reply:
            raise StepError(f"QEMU {command}: {reply['error'].get('desc', reply['error'])}")
        return reply.get("return")

    def hmp(self, command_line: str, *, timeout: float = 600) -> str:
        """Run a human monitor command. It returns its output, which is empty when savevm, loadvm,
        and delvm succeed, and an error message when they don't."""
        output = self.execute("human-monitor-command", {"command-line": command_line}, timeout=timeout)
        return str(output or "").strip()

    def snapshot_tags(self) -> set[str]:
        tags = set()
        for line in self.hmp("info snapshots").splitlines():
            parts = line.split()
            if len(parts) >= 2 and parts[0] != "ID" and not line.startswith(("List", "There")):
                tags.add(parts[1])
        return tags


def stop_vm(*, hard: bool = False, timeout: float = 120) -> None:
    """Power the VM off, or with hard, stop QEMU at once, as pulling the power would."""
    pid = vm_pid()
    if pid is None:
        PID_FILE.unlink(missing_ok=True)
        return
    if not hard:
        log("powering the VM off")
        ssh("sudo systemctl poweroff", check=False, timeout=30)
        if poll(timeout, lambda: vm_pid() is None, 2):
            PID_FILE.unlink(missing_ok=True)
            return
        warn("the VM didn't power off; stopping QEMU")
    else:
        log(f"stopping QEMU (pid {pid})")
    with contextlib.suppress(StepError), QMP(timeout=10) as qmp:
        qmp.execute("quit")
    if not poll(15, lambda: vm_pid() is None, 1):
        with contextlib.suppress(ProcessLookupError):
            os.kill(pid, signal.SIGTERM)
        if not poll(10, lambda: vm_pid() is None, 1):
            with contextlib.suppress(ProcessLookupError):
                os.kill(pid, signal.SIGKILL)
    PID_FILE.unlink(missing_ok=True)


def use_port(port: int) -> None:
    global _ssh_port
    _ssh_port = port


def ssh_command() -> list[str]:
    return ["ssh", *SSH_OPTS, "-p", str(_ssh_port), f"{SSH_USER}@127.0.0.1"]


def ssh(command: str, *, check: bool = True, timeout: float = 120,
        input: str | None = None) -> subprocess.CompletedProcess[str]:
    try:
        result = subprocess.run([*ssh_command(), command], capture_output=True, text=True,
                                timeout=timeout, input=input, check=False)
    except subprocess.TimeoutExpired:
        result = subprocess.CompletedProcess(["ssh"], 255, "", f"timed out after {duration(timeout)}: {command}")
    if check and result.returncode != 0:
        raise StepError(f"on the VM, {command!r} exited {result.returncode}: {last_lines(result.stderr, 6)}")
    return result


def copy_to_vm(local: Path, remote: str, timeout: float = 3600) -> None:
    """Copy a file into the VM through ssh, which needs no SFTP server in the image."""
    with local.open("rb") as source:
        result = subprocess.run([*ssh_command(), f"cat > {shlex.quote(remote)}"], stdin=source,
                                capture_output=True, timeout=timeout, check=False)
    if result.returncode != 0:
        raise StepError(f"copying {local.name} to the VM failed: {last_lines(result.stderr.decode(errors='replace'), 4)}")


def copy_from_vm(remote: str, local: Path, timeout: float = 1800) -> None:
    with local.open("wb") as target:
        result = subprocess.run([*ssh_command(), f"cat {shlex.quote(remote)}"], stdout=target,
                                stderr=subprocess.PIPE, timeout=timeout, check=False)
    if result.returncode != 0:
        raise StepError(f"copying {remote} from the VM failed: {last_lines(result.stderr.decode(errors='replace'), 4)}")


def boot_id() -> str | None:
    result = ssh("cat /proc/sys/kernel/random/boot_id", check=False, timeout=20)
    if result.returncode != 0:
        return None
    return result.stdout.strip() or None


def wait_for_ssh(timeout: float) -> str:
    deadline = time.monotonic() + timeout
    while True:
        if vm_pid() is None:
            raise StepError(f"the VM exited; see {SERIAL_LOG}")
        current = boot_id()
        if current:
            return current
        if time.monotonic() >= deadline:
            raise StepError(f"timed out after {duration(timeout)} waiting for SSH; see {SERIAL_LOG}")
        time.sleep(5)


def wait_for_new_boot(previous: str, timeout: float, *, required: bool = True) -> str | None:
    """Wait for the VM to answer SSH from a boot other than previous. A reboot shows as a new boot
    ID whether or not the harness saw SSH go down."""
    deadline = time.monotonic() + timeout
    while True:
        if vm_pid() is None:
            raise StepError(f"the VM exited; see {SERIAL_LOG}")
        current = boot_id()
        if current and current != previous:
            return current
        if time.monotonic() >= deadline:
            if not required:
                return None
            raise StepError(f"timed out after {duration(timeout)} waiting for the VM to boot again; see {SERIAL_LOG}")
        time.sleep(5)


def wait_settled(timeout: float = 600) -> None:
    """Wait for systemd to finish booting, so the probes see the boot's final state, including a
    commit that a trident unit runs at boot. Then step the VM's clock to the host's: the image runs
    no NTP client, and a boot after a restore starts from a clock that's behind."""
    result = ssh("systemctl is-system-running --wait", check=False, timeout=timeout)
    state = result.stdout.strip()
    if not state and result.returncode == 255:
        log("SSH dropped while the boot finished; the VM may be rebooting again")
        return
    if state == "degraded":
        failed = ssh("systemctl --failed --no-legend --plain", check=False).stdout.strip()
        warn(f"systemd reports the boot degraded; failed units:\n{failed}")
    elif state != "running":
        warn(f"systemd reports {state or 'nothing'} rather than running")
    sync_guest_clock()


def read_serial(offset: int) -> tuple[str, int]:
    try:
        with SERIAL_LOG.open("rb") as serial:
            serial.seek(offset)
            data = serial.read()
    except OSError:
        return "", offset
    return data.decode("utf-8", "replace"), offset + len(data)


_ESCAPES = re.compile(r"\x1b\[[0-?]*[ -/]*[@-~]|\x1b[()][0-9A-Za-z]|\x1b[=>]")


def last_serial_line(text: str) -> str:
    for line in reversed(_ESCAPES.sub("", text).replace("\r", "\n").splitlines()):
        line = "".join(char for char in line if char.isprintable()).strip()
        if line:
            return line[:120]
    return "(nothing on the console yet)"


# ---------------------------------------------------------------------------------------------
# Probe

# One SSH round trip reads everything the checks compare. Each section is parsed on this side.
PROBE_SCRIPT = r"""
section() { printf '\n### %s\n' "$1"; }
efivar() {
  f=/sys/firmware/efi/efivars/$1-@GUID@
  [ -e "$f" ] && printf '%s=%s\n' "$1" "$(sudo od -An -tx1 -v "$f" | tr -d ' \n')"
}
section boot_id; cat /proc/sys/kernel/random/boot_id
section btime; awk '/^btime/{print $2}' /proc/stat
section boots; journalctl --list-boots --no-pager -q 2>/dev/null | wc -l
section os_release; cat /etc/os-release
section kernel; uname -r
section verity; sudo veritysetup status usr 2>&1
section status
# stdout has the Host Status alone; the log goes to stderr. Not --outfile: root's trident can't write
# a file the user made in /tmp, which fs.protected_regular forbids.
err=$(mktemp)
if status=$(sudo trident get status 2>"$err"); then printf '%s\n' "$status"; else echo '!error'; tail -n 5 "$err"; fi
rm -f "$err"
section efi
efivar BootCurrent; efivar BootNext; efivar BootOrder
for f in /sys/firmware/efi/efivars/Boot[0-9A-F][0-9A-F][0-9A-F][0-9A-F]-@GUID@; do
  [ -e "$f" ] && efivar "$(basename "$f" | cut -c1-8)"
done
section usr_writable
if sudo touch /usr/.acl-abupdate-probe 2>/dev/null; then sudo rm -f /usr/.acl-abupdate-probe; echo yes; else echo no; fi
section markers
for p in @MARKERS@; do printf '%s\t%s\n' "$p" "$(sudo cat "$p" 2>/dev/null | tr '\n' ' ')"; done
section system_state; systemctl is-system-running 2>/dev/null
section trident_units; systemctl list-units 'trident*' --all --no-legend --plain 2>/dev/null
section trident_version; sudo trident --version 2>/dev/null
section trident_acl_agent
printf 'is-enabled=%s\n' "$(systemctl is-enabled @AGENT@ 2>/dev/null)"
systemctl show @AGENT@ --no-pager -p LoadState -p ActiveState -p SubState -p NRestarts \
  -p InactiveExitTimestampMonotonic -p DropInPaths -p Environment 2>/dev/null
if [ -S @SOCKET@ ]; then echo socket=present; else echo socket=missing; fi
true
"""


class Probe:
    """What one SSH round trip reads from the VM: its boot, /usr's verity device, Trident's Host
    Status, the UEFI boot variables, and the harness's markers."""

    def __init__(self, sections: dict[str, str]) -> None:
        self.sections = sections
        self.verity = parse_verity(sections.get("verity", ""))
        status = sections.get("status", "")
        self.status_error = status[len("!error"):].strip() if status.startswith("!error") else ""
        self.status_text = "" if status.startswith("!error") else status
        self.status: dict[str, str | None] | None = parse_yaml(self.status_text) if self.status_text else None
        self.efi = dict(line.split("=", 1) for line in sections.get("efi", "").splitlines() if "=" in line)
        self.os_release = parse_os_release(sections.get("os_release", ""))

    @property
    def boot_id(self) -> str:
        return self.sections.get("boot_id", "").strip()

    @property
    def booted_at(self) -> datetime.datetime | None:
        """When this boot started, by the VM's clock."""
        value = self.sections.get("btime", "").strip()
        return datetime.datetime.fromtimestamp(int(value), datetime.timezone.utc) if value.isdigit() else None

    @property
    def data_device(self) -> str:
        return self.verity.get("data device", "")

    @property
    def pair(self) -> tuple[str, str]:
        """The booted slot, as /usr's verity data and hash devices. findmnt shows /dev/dm-0 for
        either slot, so these are what tell them apart."""
        return self.data_device, self.verity.get("hash device", "")

    @property
    def pair_text(self) -> str:
        return ", ".join(device for device in self.pair if device) or "no verity device"

    @property
    def roothash(self) -> str:
        return self.verity.get("root hash", "")

    @property
    def state(self) -> str | None:
        return self.status.get("servicingState") if self.status else None

    @property
    def active_volume(self) -> str | None:
        return self.status.get("abActiveVolume") if self.status else None

    @property
    def last_error(self) -> str | None:
        if not self.status:
            return None
        value = self.status.get("lastError")
        if value == BLOCK:
            return yaml_block(self.status_text, "lastError") or "(set)"
        return value

    @property
    def image_url(self) -> str | None:
        return self.status.get("spec.image.url") if self.status else None

    @property
    def image_sha384(self) -> str | None:
        return self.status.get("spec.image.sha384") if self.status else None

    @property
    def boot_current(self) -> str:
        return efi_boot_number(self.efi.get("BootCurrent", ""))

    @property
    def boot_next(self) -> str:
        return efi_boot_number(self.efi.get("BootNext", ""))

    @property
    def boot_order(self) -> list[str]:
        return efi_boot_order(self.efi.get("BootOrder", ""))

    def boot_label(self, number: str) -> str:
        description = efi_load_option_description(self.efi.get(f"Boot{number}", ""))
        return f"{number} ({description})" if description else number

    @property
    def usr_writable(self) -> bool:
        return self.sections.get("usr_writable", "").strip() == "yes"

    @property
    def markers(self) -> dict[str, list[str]]:
        markers = {}
        for line in self.sections.get("markers", "").splitlines():
            path, _, tokens = line.partition("\t")
            markers[path.strip()] = tokens.split()
        return markers

    @property
    def boot_count(self) -> int:
        value = self.sections.get("boots", "").strip()
        return int(value) if value.isdigit() else 0

    @property
    def kernel(self) -> str:
        return self.sections.get("kernel", "").strip()

    @property
    def trident_version(self) -> str:
        return self.sections.get("trident_version", "").strip()

    @property
    def trident_agent(self) -> dict[str, str]:
        """trident-acl-agent.service's state: `systemctl show` properties, is-enabled, and
        whether tridentd's socket exists."""
        return parse_properties(self.sections.get("trident_acl_agent", ""))

    def text(self) -> str:
        return "".join(f"### {name}\n{content}\n\n" for name, content in self.sections.items())


def probe_script() -> str:
    return (PROBE_SCRIPT.replace("@GUID@", EFI_GLOBAL_GUID)
            .replace("@MARKERS@", " ".join(shlex.quote(path) for path in MARKER_PATHS))
            .replace("@AGENT@", TRIDENT_AGENT_UNIT)
            .replace("@SOCKET@", shlex.quote(TRIDENT_SOCKET)))


def probe() -> Probe:
    result = ssh("bash -s", input=probe_script(), timeout=180, check=False)
    if "### boot_id" not in result.stdout:
        raise StepError(f"couldn't read the VM's state (ssh exited {result.returncode}): "
                        f"{last_lines(result.stderr, 4)}")
    return Probe(parse_sections(result.stdout))


# ---------------------------------------------------------------------------------------------
# Harness state


@dataclasses.dataclass
class State:
    """What the harness knows of its VM between commands, in state.json. Trident's Host Status is
    the source of truth for the VM's servicing; this keeps what it doesn't hold: the harness's
    markers, the update staged, and which devices each slot's /usr is on."""

    base_tag: str = BASE_TAG
    update_tag: str = UPDATE_TAG
    # Where the images come from, as up found them set.
    registry: str = ""
    iso_repo: str = ""
    cosi_repo: str = ""
    ssh_port: int = SSH_PORT
    memory: int = VM_MEMORY
    cpus: int = VM_CPUS
    installed: str = ""
    # Whether this Trident can stage without finalizing; None until asked.
    two_step: bool | None = None
    # Trident's volume name (volume-a) to the [data, hash] devices of /usr in that slot.
    slot_pairs: dict[str, list[str]] = dataclasses.field(default_factory=dict)
    # A slot's /usr data device to the image last seen in it: {"roothash", "image"}.
    slots: dict[str, dict[str, str]] = dataclasses.field(default_factory=dict)
    markers: list[str] = dataclasses.field(default_factory=list)
    staged: dict[str, Any] | None = None
    committed_update: dict[str, Any] | None = None
    # The second NIC `up --nic-tap` attached: {"tap", "mac", "address"}. None without one.
    nic: dict[str, str] | None = None
    # The hostname `up --hostname` gave the VM; empty for the seed's.
    hostname: str = ""
    # The VM's SMBIOS UUID, its product_uuid; empty for a VM made before the harness gave it one.
    smbios_uuid: str = ""
    history: list[dict[str, Any]] = dataclasses.field(default_factory=list)

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> State:
        known = {field.name for field in dataclasses.fields(cls)}
        return cls(**{key: value for key, value in data.items() if key in known})

    @classmethod
    def load(cls) -> State:
        try:
            return cls.from_dict(json.loads(STATE_FILE.read_text()))
        except (OSError, json.JSONDecodeError) as error:
            die(f"can't read {STATE_FILE}: {error}")

    def save(self) -> None:
        partial = STATE_FILE.with_suffix(".partial")
        partial.write_text(json.dumps(dataclasses.asdict(self), indent=2) + "\n")
        os.replace(partial, STATE_FILE)


def claim_state_dir() -> None:
    """Make the state directory and mark it, for down to remove. An existing one has to be the
    default, marked already, or empty: down mustn't remove a directory that holds anything else."""
    claimable = STATE.exists() and not STATE_MARKER.exists() and STATE.resolve() != DEFAULT_STATE.resolve()
    if claimable and (not STATE.is_dir() or any(STATE.iterdir())):
        die(f"{STATE} isn't a directory up made, or an empty one; set ACL_AB_STATE_DIR to a new or empty directory")
    STATE.mkdir(parents=True, exist_ok=True)
    STATE.chmod(0o700)
    STATE_MARKER.touch()


@contextlib.contextmanager
def state_lock() -> Iterator[None]:
    """Keep two commands from driving the VM at once."""
    if not STATE.is_dir():
        die(f"there's no VM in {STATE}; run `up` first")
    with LOCK_FILE.open("a") as handle:
        try:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            die("another abupdate.py command is driving this VM; wait for it, or set another ACL_AB_STATE_DIR")
        yield


def require_state() -> State:
    if not STATE_FILE.is_file():
        die(f"there's no VM in {STATE}; run `up` first")
    state = State.load()
    use_port(state.ssh_port)
    use_images(state)
    return state


def require_running() -> State:
    state = require_state()
    if vm_pid() is None:
        die("the VM isn't running; `start` it")
    return state


def launch(state: State, *, loadvm: str = "", paused: bool = False) -> None:
    # A snapshot's devices have to match the VM it's loaded into, so a VM brought up with a second
    # NIC always has it.
    nic = state.nic or {}
    launch_vm(disk=VM_DISK, variables=VM_VARS, memory=state.memory, cpus=state.cpus,
              ssh_port=state.ssh_port, loadvm=loadvm, nic_tap=nic.get("tap", ""), nic_mac=nic.get("mac", ""),
              smbios_uuid=state.smbios_uuid, paused=paused)


def sync_guest_clock() -> None:
    """Step the VM's clock to the host's. A snapshot holds the clock as it was when it was saved,
    so after a restore the guest is behind by however long ago that was, and chronyd only steps
    it at its next poll, which can be many minutes away. In a cluster the node's Lease renewals
    would go back in time meanwhile."""
    result = ssh(f"before=$(date +%s.%N); sudo date -u -s @{time.time():.3f} >/dev/null && "
                 "(sudo chronyc -a makestep >/dev/null 2>&1 || true); echo \"$before $(date +%s.%N)\"",
                 check=False, timeout=30)
    try:
        before, after = (float(value) for value in result.stdout.split()[-2:])
    except ValueError:
        warn(f"couldn't step the VM's clock: {last_lines(result.stderr, 3)}")
        return
    log(f"stepped the VM's clock {after - before:+.0f}s to the host's (now {after - time.time():+.1f}s off)")


def kernel_slot(command_line: str) -> str:
    """The A/B slot a kernel command line boots, from ACL's acl.slot= argument, or ""."""
    match = re.search(r"(?:^|\s)acl\.slot=(\S+)", command_line)
    return match.group(1) if match else ""


def reset_at_next_kernel(offset: int, timeout: float) -> str:
    """Reset the VM as soon as the serial console, past offset, shows a kernel's command line, and
    return it. By then the firmware has booted that kernel, and so used its one-time BootNext: the
    reset boots the first entry of BootOrder, as a failed first boot of a new slot would."""
    deadline = time.monotonic() + timeout
    text = ""
    while time.monotonic() < deadline:
        chunk, offset = read_serial(offset)
        text = (text + chunk)[-65536:]
        match = re.search(r"Command line: ([^\r\n]*)[\r\n]", text)
        if match:
            with QMP() as qmp:
                qmp.execute("system_reset")
            log(f"reset the VM as its next kernel started, slot {kernel_slot(match.group(1)) or '?'}")
            return match.group(1)
        if vm_pid() is None:
            raise StepError(f"the VM exited; see {SERIAL_LOG}")
        time.sleep(0.2)
    raise StepError(f"no kernel started in {duration(timeout)}; see {SERIAL_LOG}")


def nic_mac(address: str) -> str:
    """A locally administered MAC for the second NIC, from its address's last two octets."""
    octets = [int(octet) for octet in address.split(".")]
    return f"52:54:00:ab:{octets[2]:02x}:{octets[3]:02x}"


def configure_nic(address: str, mac: str) -> None:
    """Give the VM's second NIC its address, with a drop-in on the seed's network file, which
    persists under /etc like the rest of the node's configuration."""
    content = f"[Network]\nAddress={address}/24\n"
    dropin = f"{NIC_NETWORK}.d/50-address.conf"
    ssh(f"sudo install -D -m 0644 /dev/stdin {shlex.quote(dropin)} && sudo networkctl reload && "
        f"for link in /sys/class/net/*; do [ \"$(cat $link/address)\" = {mac} ] && "
        "sudo networkctl reconfigure \"${link##*/}\"; done; true", input=content)

    def addressed() -> bool:
        return f" {address}/24" in ssh("ip -4 -o addr", check=False, timeout=20).stdout

    if not poll(60, addressed, 2):
        raise StepError(f"the VM's second NIC didn't take {address}; `ip -br addr` on the VM shows its links")
    log(f"the VM's second NIC is {address}")

# ---------------------------------------------------------------------------------------------
# Snapshots


def snapshot_names() -> list[str]:
    return sorted(path.stem for path in SNAPSHOT_DIR.glob("*.json"))


def save_snapshot(state: State, name: str, *, force: bool = False, summary: str = "") -> None:
    """Save the running VM, memory included, in its disk and UEFI variables, and the harness's
    state beside them, so restore returns to this moment, mid-update or not."""
    if not SNAPSHOT_PATTERN.fullmatch(name):
        die(f"{name!r} isn't a snapshot name: use letters, digits, '.', '_', and '-'")
    meta = SNAPSHOT_DIR / f"{name}.json"
    if meta.exists() and not force:
        die(f"snapshot {name} exists; pass --force to replace it")
    if vm_pid() is None:
        die("snapshots are taken of the running VM; `start` it first")
    log(f"saving snapshot {name}; the VM pauses while its memory is written")
    started = time.monotonic()
    with QMP() as qmp:
        if name in qmp.snapshot_tags():
            qmp.hmp(f"delvm {name}", timeout=600)
        output = qmp.hmp(f"savevm {name}", timeout=1800)
    if output:
        raise StepError(f"savevm {name}: {output}")
    SNAPSHOT_DIR.mkdir(exist_ok=True)
    meta.write_text(json.dumps({"name": name, "created": now_iso(), "summary": summary,
                                "state": dataclasses.asdict(state)}, indent=2) + "\n")
    log(f"saved snapshot {name} in {duration(time.monotonic() - started)}")

def restore_snapshot(name: str, current: State | None = None, *, hold: bool = False) -> State:
    """Return the VM, and the harness's state, to a snapshot. The history of what the harness ran
    is kept, with the restore added to it. hold leaves the VM paused at the snapshot, so a caller
    can set what the VM talks to back as well, say a cluster's Node, before `resume` runs it."""
    meta = SNAPSHOT_DIR / f"{name}.json"
    if not meta.is_file():
        die(f"there's no snapshot {name}; `snapshot list` shows them")
    saved = json.loads(meta.read_text())
    state = State.from_dict(saved["state"])
    if current is not None:
        state.history = [*current.history]
        # A snapshot saved before up recorded the image settings keeps the VM's.
        state.registry = state.registry or current.registry
        state.iso_repo = state.iso_repo or current.iso_repo
        state.cosi_repo = state.cosi_repo or current.cosi_repo
    state.history.append({"time": now_iso(), "operation": "restore", "snapshot": name})
    use_port(state.ssh_port)
    log(f"restoring snapshot {name}")
    if vm_pid() is not None:
        with QMP() as qmp:
            if hold:
                qmp.execute("stop")
            # loadvm leaves a paused VM paused.
            output = qmp.hmp(f"loadvm {name}", timeout=1800)
        if output:
            raise StepError(f"loadvm {name}: {output}")
    else:
        launch(state, loadvm=name, paused=hold)
    state.save()
    if hold:
        log(f"the VM is paused at snapshot {name}; `resume` runs it")
        return state
    wait_for_ssh(120)
    sync_guest_clock()
    return state

def delete_snapshot(name: str) -> None:
    meta = SNAPSHOT_DIR / f"{name}.json"
    if vm_pid() is not None:
        with QMP() as qmp:
            if name in qmp.snapshot_tags():
                output = qmp.hmp(f"delvm {name}", timeout=600)
                if output:
                    raise StepError(f"delvm {name}: {output}")
    else:
        for image in (VM_DISK, VM_VARS):
            if image.exists():
                run(["qemu-img", "snapshot", "-d", name, str(image)], check=False, capture=True)
    meta.unlink(missing_ok=True)
    log(f"deleted snapshot {name}")


# ---------------------------------------------------------------------------------------------
# Checks and steps


class Checks:
    """Check results, labelled with their scopes: forward/A1, or pingpong/P2/A3."""

    def __init__(self) -> None:
        self.results: list[dict[str, str]] = []
        self.scopes: list[str] = []

    @contextlib.contextmanager
    def scope(self, name: str) -> Iterator[None]:
        self.scopes.append(name)
        try:
            yield
        finally:
            self.scopes.pop()

    def label(self, check_id: str) -> str:
        return "/".join([*self.scopes, check_id])

    def expect(self, ok: bool, check_id: str, description: str, detail: str = "") -> bool:
        label = self.label(check_id)
        result = {"id": label, "description": description, "result": "pass" if ok else "fail"}
        if detail and not ok:
            result["detail"] = detail
        self.results.append(result)
        if ok:
            log(f"ok: {label} {description}")
        else:
            log(f"FAIL: {label} {description}" + (f": {detail}" if detail else ""))
        return ok

    def skip(self, check_id: str, description: str, reason: str) -> None:
        label = self.label(check_id)
        self.results.append({"id": label, "description": description, "result": "skip", "detail": reason})
        log(f"skip: {label} {description}: {reason}")

    def failed(self, since: int = 0) -> list[dict[str, str]]:
        return [result for result in self.results[since:] if result["result"] == "fail"]


class Context:
    """One command's run: the harness's state, its checks, and the directory its artifacts go to.
    Its step methods are what the step commands and the scenarios run."""

    def __init__(self, command: str, state: State) -> None:
        self.command = command
        self.state = state
        self.checks = Checks()
        stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        self.run_dir = RUNS_DIR / f"{stamp}-{command}"
        suffix = 1
        while self.run_dir.exists():
            suffix += 1
            self.run_dir = RUNS_DIR / f"{stamp}-{command}-{suffix}"
        self.run_dir.mkdir(parents=True)
        self.started = time.monotonic()
        self.timings: list[tuple[str, float]] = []
        self.errors: list[str] = []
        self.scenarios: list[dict[str, Any]] = []
        # The servicing states drive_to_terminal passed through, for the scenarios that look.
        self.seen: list[str] = []
        self.target_spec = ""
        self.rounds = 2
        self.no_setup = False
        self.single_step = False
        # What a run found that isn't a check's result, such as where the agent and its contract differ.
        self._sequence = 0
        log(f"artifacts go to {self.run_dir}")

    # Bookkeeping

    def save(self, name: str, content: str | bytes) -> Path:
        self._sequence += 1
        path = self.run_dir / f"{self._sequence:02d}-{name}"
        path.write_bytes(content if isinstance(content, bytes) else content.encode())
        return path

    def scope(self, name: str) -> contextlib.AbstractContextManager[None]:
        return self.checks.scope(name)

    @contextlib.contextmanager
    def timed(self, name: str) -> Iterator[None]:
        started = time.monotonic()
        try:
            yield
        finally:
            seconds = time.monotonic() - started
            self.timings.append((self.checks.label(name), seconds))
            log(f"{name} took {duration(seconds)}")

    def record(self, operation: str, **fields: Any) -> None:
        self.state.history.append({"time": now_iso(), "operation": operation, "run": self.run_dir.name, **fields})
        self.state.save()

    def probe(self, label: str = "") -> Probe:
        result = probe()
        if label:
            self.save(f"{label}.txt", result.text())
        self.learn(result)
        return result

    def learn(self, probe: Probe, image: str = "") -> None:
        """Note the image in the booted slot, and once Trident is provisioned, which volume the
        slot is: until commit, abActiveVolume still names the slot the update came from."""
        if probe.data_device and probe.roothash:
            self.state.slots[probe.data_device] = {"roothash": probe.roothash,
                                                   "image": image or image_name(probe.roothash)}
        if probe.state == "provisioned" and probe.active_volume and probe.data_device:
            self.state.slot_pairs.setdefault(probe.active_volume, list(probe.pair))
        self.state.save()

    def target(self, default: str, *, allow_override: bool = True) -> Image:
        spec = self.target_spec if allow_override and self.target_spec else default
        return resolve_target(spec, self.state, lambda: self.probe().roothash)

    def alternative(self, image: Image) -> Image:
        base = cosi(self.state.base_tag, self.state)
        return cosi(self.state.update_tag, self.state) if image.roothash == base.roothash else base

    def trident(self, arguments: str, label: str, *, timeout: float = COMMAND_TIMEOUT) -> subprocess.CompletedProcess[str]:
        log(f"trident {arguments}")
        result = ssh(f"sudo trident {arguments}", check=False, timeout=timeout)
        self.save(f"trident-{label}.log", f"$ sudo trident {arguments}\n# exit {result.returncode}\n\n"
                                          f"## stdout\n{result.stdout}\n## stderr\n{result.stderr}")
        return result

    # Scenarios

    def ensure(self, scenario: Scenario) -> None:
        """Set the VM up for what the scenario needs, unless --no-setup."""
        if scenario.needs is Needs.NONE:
            return
        current = self.probe()
        if scenario.needs is Needs.OFF_TARGET:
            target = self.target(scenario.target)
            if current.roothash != target.roothash:
                return
            reason = f"the VM runs {target.name} already"

            def setup() -> None:
                self.update(self.alternative(target))
        else:
            if self.state.committed_update and current.state == "provisioned":
                return
            reason = "there's no committed A/B update to work from"

            def setup() -> None:
                self.update(resolve_target("other", self.state, lambda: self.probe().roothash))
        if self.no_setup:
            raise StepError(f"{reason}, and --no-setup was given")
        log(f"setting up: {reason}")
        with self.checks.scope("setup"):
            setup()

    def run_scenario(self, scenario: Scenario) -> str:
        started, first = time.monotonic(), len(self.checks.results)
        result, reason = "pass", ""
        log(f"scenario {scenario.name}: {scenario.description}")
        try:
            with self.checks.scope(scenario.name):
                self.ensure(scenario)
                scenario.run(self)
        except Skip as skip:
            result, reason = "skip", str(skip)
        except StepError as error:
            result, reason = "fail", str(error)
            log(f"FAIL: {scenario.name} stopped: {error}")
        if result != "skip" and self.checks.failed(first):
            result = "fail"
        if result == "fail":
            self.collect_diagnostics(scenario.name)
        seconds = time.monotonic() - started
        self.scenarios.append({"name": scenario.name, "result": result, "reason": reason,
                               "experimental": scenario.experimental, "seconds": round(seconds, 1)})
        log(f"scenario {scenario.name}: {result}" + (f" ({reason})" if reason else "") + f" in {duration(seconds)}")
        return result

    # Steps

    def write_markers(self) -> None:
        token = f"{len(self.state.markers) + 1}-{secrets.token_hex(4)}"
        commands = [f"sudo mkdir -p {shlex.quote(str(Path(path).parent))} && "
                    f"echo {token} | sudo tee -a {shlex.quote(path)} >/dev/null" for path in MARKER_PATHS]
        ssh(" && ".join(commands) + " && sync")
        self.state.markers.append(token)
        self.state.save()
        log(f"wrote marker {token} to {', '.join(MARKER_PATHS)}")

    def copy_cosi(self, target: Image, check_id: str) -> str:
        directory = shlex.quote(GUEST_DIR)
        name = f"{target.sha384[:16]}.cosi"
        remote = f"{GUEST_DIR}/{name}"
        ssh(f"sudo mkdir -p {directory} && sudo chown {SSH_USER}: {directory}")
        size = target.path.stat().st_size
        present = ssh(f"stat -c %s {shlex.quote(remote)} 2>/dev/null", check=False).stdout.strip()
        if present == str(size):
            log(f"{target.name}'s COSI is in the VM already, at {remote}")
        else:
            ssh(f"rm -f {shlex.quote(remote + '.partial')}", check=False)
            needed = size + 256 * 2**20
            if self.guest_free(directory) < needed:
                # Pingpong alternates two COSIs, so they're kept while there's room for both.
                ssh(f"find {directory} -maxdepth 1 -name '*.cosi' ! -name {shlex.quote(name)} -delete", check=False)
            free = self.guest_free(directory)
            if free < needed:
                raise StepError(f"{GUEST_DIR} in the VM has {human_size(free)} free, and {target.name}'s "
                                f"COSI is {human_size(size)}; set ACL_AB_GUEST_DIR to a directory with room")
            log(f"copying {target.name}'s COSI ({human_size(size)}) to {remote}")
            with self.timed("copy"):
                copy_to_vm(target.path, f"{remote}.partial")
                ssh(f"mv {shlex.quote(remote + '.partial')} {shlex.quote(remote)}")
        hashed = ssh(f"tar -xOf {shlex.quote(remote)} metadata.json | sha384sum", check=False, timeout=600)
        digest = hashed.stdout.split()[0] if hashed.returncode == 0 and hashed.stdout.split() else ""
        if digest:
            self.checks.expect(digest == target.sha384, check_id,
                               "the COSI in the VM has the expected metadata.json sha384", f"{digest} != {target.sha384}")
        else:
            warn(f"couldn't hash the COSI's metadata.json in the VM: {last_lines(hashed.stderr, 3)}")
        return remote

    def guest_free(self, directory: str) -> int:
        """Free bytes in a directory in the VM; a large number if df can't say, so the copy tries."""
        free = ssh(f"df -B1 --output=avail {directory} | tail -n 1", check=False).stdout.strip()
        return int(free) if free.isdigit() else 2**62

    def write_config(self, target: Image, guest_cosi: str, health_check: bool) -> str:
        """An image-only Host Configuration, which keeps the installed storage layout."""
        content = f'image:\n  url: file://{guest_cosi}\n  sha384: "{target.sha384}"\n'
        if health_check:
            content += HEALTH_CHECK_CONFIG
        remote = f"{GUEST_DIR}/trident-{target.sha384[:16]}{'-health' if health_check else ''}.yaml"
        ssh(f"cat > {shlex.quote(remote)}", input=content)
        self.save("trident-config.yaml", content)
        return remote

    def two_step(self) -> bool:
        if self.single_step:
            return False
        if self.state.two_step is None:
            usage = ssh("sudo trident update --help 2>&1", check=False).stdout
            self.state.two_step = "--allowed-operations" in usage
            self.state.save()
            if not self.state.two_step:
                warn("this Trident's update has no --allowed-operations, so it stages and finalizes in one step")
        return bool(self.state.two_step)

    def stage(self, target: Image, *, health_check: bool = False, check_id: str = "A1") -> Probe:
        """Write target to the inactive slot, without rebooting: copy the COSI in, write the Host
        Configuration, and run trident update --allowed-operations stage."""
        before = self.probe("before-stage")
        if before.state not in (None, "provisioned", "ab-update-staged"):
            raise StepError(f"Trident is in {before.state}; finish that, or restore a snapshot, before staging")
        if before.state is None:
            log(f"Trident has no Host Status yet ({first_line(before.status_error) or 'no output'}); "
                "the first update initializes its datastore")
        log(f"staging {target.name} (OS {target.version or '?'}, /usr root hash {short(target.roothash)}) "
            f"over {image_name(before.roothash)} on {before.pair_text}")
        self.write_markers()
        guest_cosi = self.copy_cosi(target, check_id)
        config = self.write_config(target, guest_cosi, health_check)
        two_step = self.two_step()
        self.state.staged = {
            "image": target.name, "sha384": target.sha384, "roothash": target.roothash,
            "guestCosi": guest_cosi, "config": config, "healthCheck": health_check,
            "fromPair": list(before.pair), "fromVolume": before.active_volume,
            "fromRoothash": before.roothash, "twoStep": two_step,
        }
        self.state.save()
        if not two_step:
            self.checks.skip(check_id, "staging leaves the VM on its slot", "this Trident stages and finalizes in one step")
            return before
        with self.timed("stage"):
            # With "=", so the option, which takes several values, doesn't take the config's path too.
            result = self.trident(f"update --allowed-operations=stage {shlex.quote(config)}", "stage",
                                  timeout=STAGE_TIMEOUT)
        after = self.probe("after-stage")
        if result.returncode != 0:
            output = f"{result.stdout}\n{result.stderr}"
            self.state.staged = None
            self.state.save()
            if health_check and "health" in output.lower() and "unknown field" in output.lower():
                raise Skip(f"this Trident rejects a health section in an image-only Host Configuration: "
                           f"{last_lines(result.stderr or result.stdout, 3)}")
            self.checks.expect(False, check_id, "trident update --allowed-operations stage succeeds",
                               f"exit {result.returncode}: {last_lines(result.stderr or result.stdout)}")
            raise StepError("staging failed")
        c = self.checks
        c.expect(after.boot_id == before.boot_id, check_id, "staging doesn't reboot the VM")
        c.expect(after.state == "ab-update-staged", check_id, "servicingState is ab-update-staged", f"it's {after.state}")
        c.expect(after.pair == before.pair, check_id, "the VM still runs the slot it ran",
                 f"{before.pair_text} -> {after.pair_text}")
        if before.active_volume:
            c.expect(after.active_volume == before.active_volume, check_id, "abActiveVolume is unchanged",
                     f"{before.active_volume} -> {after.active_volume}")
        c.expect(after.last_error is None, check_id, "lastError is empty", after.last_error or "")
        self.record("stage", image=target.name, healthCheck=health_check)
        return after

    def wait_for_reboot(self, before: Probe, result: subprocess.CompletedProcess[str], what: str) -> str:
        """Wait for the reboot a Trident command starts, and return the new boot's ID. The reboot
        ends the SSH session, so ssh exits 255, or 0 if the command returned first. Any other
        status is the command's failure, unless the VM rebooted anyway."""
        if result.returncode not in (0, 255):
            new = wait_for_new_boot(before.boot_id, 60, required=False)
            if new is None:
                raise StepError(f"{what} exited {result.returncode} and didn't reboot the VM: "
                                f"{last_lines(result.stderr or result.stdout)}")
        else:
            new = wait_for_new_boot(before.boot_id, BOOT_TIMEOUT)
        wait_settled()
        return str(new)

    def probe_after_boot(self, boot: str, label: str) -> Probe:
        """Probe the VM in the boot it came up in. If that boot goes down first, as it does when a
        trident unit commits at boot and a health check rolls it back, probe the next one."""
        try:
            return self.probe(label)
        except StepError:
            wait_for_new_boot(boot, BOOT_TIMEOUT)
            wait_settled()
            return self.probe(label)

    def finalize(self, *, verify: bool = True) -> Probe:
        """Finalize what's staged, which sets BootNext and reboots into the new slot, and check
        the VM booted it: A2, the slot and its root hash, and A3, the markers and a read-only /usr."""
        staged = self.state.staged
        if not staged:
            raise StepError("nothing is staged; `stage` first")
        before = self.probe("before-finalize")
        config = shlex.quote(staged["config"])
        if staged["twoStep"]:
            if before.state != "ab-update-staged":
                raise StepError(f"Trident is in {before.state}, not ab-update-staged; stage again")
            arguments, timeout = f"update --allowed-operations=finalize {config}", COMMAND_TIMEOUT
        else:
            arguments, timeout = f"update {config}", STAGE_TIMEOUT
        with self.timed("finalize"):
            result = self.trident(arguments, "finalize", timeout=timeout)
            boot = self.wait_for_reboot(before, result, "trident update")
        after = self.probe_after_boot(boot, "after-finalize")
        self.record("finalize", image=staged["image"])
        log(f"the VM booted {image_name(after.roothash)} on {after.pair_text}; Trident is {after.state}")
        if not verify:
            return after
        c = self.checks
        c.expect(after.pair != tuple(staged["fromPair"]), "A2", "the VM booted the other slot",
                 f"it booted {after.pair_text} again")
        if staged["roothash"]:
            c.expect(after.roothash == staged["roothash"], "A2", f"/usr's verity root hash is {staged['image']}'s",
                     f"{short(after.roothash)} != {short(staged['roothash'])}")
        self.learn(after, staged["image"])
        self.check_persistence("A3", after)
        return after

    def commit(self, *, check_id: str = "A4") -> Probe:
        """Commit the boot into the new slot, unless a trident unit did at boot, and check A4: the
        Host Status is provisioned on the new volume, with the update's image, and no lastError."""
        before = self.probe("before-commit")
        staged = self.state.staged
        if before.state == "provisioned":
            log("Trident has committed this boot already (the image's trident service commits at boot)")
            after = before
        elif before.state in FINALIZED_STATES or before.state == HEALTH_FAILED_STATE:
            with self.timed("commit"):
                after = self.drive_to_terminal(before)
        else:
            raise StepError(f"Trident is in {before.state}; there's nothing to commit")
        if not staged:
            return after
        c = self.checks
        c.expect(after.state == "provisioned", check_id, "servicingState is provisioned", f"it's {after.state}")
        c.expect(after.last_error is None, check_id, "lastError is empty", after.last_error or "")
        if staged["fromVolume"]:
            c.expect(after.active_volume not in (None, staged["fromVolume"]), check_id, "abActiveVolume flipped",
                     f"{staged['fromVolume']} -> {after.active_volume}")
        else:
            c.expect(after.active_volume is not None, check_id, "abActiveVolume is set", "it's empty")
        c.expect(after.pair != tuple(staged["fromPair"]), check_id, "the VM runs the new slot",
                 f"it runs {after.pair_text}")
        if staged["roothash"]:
            c.expect(after.roothash == staged["roothash"], check_id, f"/usr's root hash is {staged['image']}'s",
                     f"{short(after.roothash)} != {short(staged['roothash'])}")
        c.expect(after.image_url == f"file://{staged['guestCosi']}", check_id,
                 "the Host Status names the update's image URL", f"it names {after.image_url}")
        c.expect((after.image_sha384 or "").lower() == staged["sha384"], check_id,
                 "the Host Status names the update's metadata sha384", f"it names {after.image_sha384}")
        self.check_slot_agrees(check_id, after)
        self.check_boot_order(check_id, after)
        self.state.committed_update = {"image": staged["image"], "fromPair": staged["fromPair"],
                                       "fromRoothash": staged["fromRoothash"], "toPair": list(after.pair)}
        self.state.staged = None
        self.learn(after, staged["image"])
        self.record("commit", image=staged["image"], activeVolume=after.active_volume)
        return after

    def update(self, target: Image, *, commit: bool = True) -> Probe:
        self.stage(target)
        after = self.finalize()
        return self.commit() if commit else after

    def drive_to_terminal(self, current: Probe) -> Probe:
        """Take Trident from a finalized state to provisioned: commit, which reboots into the
        previous slot when a health check fails, and commit again there. The states passed are
        kept in self.seen."""
        from_pair = tuple(self.state.staged["fromPair"]) if self.state.staged else None
        deadline = time.monotonic() + 3 * BOOT_TIMEOUT
        commits = 0
        boot = current.boot_id
        while True:
            self.seen.append(current.state or "none")
            if current.state == "provisioned":
                return current
            if time.monotonic() > deadline:
                raise StepError(f"Trident didn't reach provisioned in {duration(3 * BOOT_TIMEOUT)}; it's {current.state}")
            if current.state == HEALTH_FAILED_STATE and from_pair and current.pair != from_pair:
                log("a health check failed; waiting for Trident to reboot into the previous slot")
                boot = str(wait_for_new_boot(boot, BOOT_TIMEOUT))
                wait_settled()
            elif current.state in FINALIZED_STATES or current.state == HEALTH_FAILED_STATE:
                commits += 1
                if commits > 3:
                    raise StepError(f"Trident is still {current.state} after {commits - 1} commits")
                result = self.trident("commit", f"commit-{commits}")
                if result.returncode == 255:
                    # ssh lost the VM: commit rebooted it, as it does into the previous slot after a
                    # failed health check.
                    boot = str(wait_for_new_boot(boot, BOOT_TIMEOUT))
                    wait_settled()
                elif result.returncode != 0:
                    # Commit exits non-zero after it completes a rollback too, to report the failed
                    # health check that caused it, so what counts is whether the state moved on.
                    after = self.probe_after_boot(boot, f"after-commit-{commits}")
                    if (after.boot_id, after.state) == (boot, current.state):
                        # Nothing moved yet: give a reboot that's on its way a minute.
                        new = wait_for_new_boot(boot, 60, required=False)
                        if new is None:
                            raise StepError(f"trident commit exited {result.returncode} and changed nothing: "
                                            f"{last_lines(result.stderr or result.stdout)}")
                        boot = new
                        wait_settled()
                    elif after.state != current.state:
                        log(f"trident commit exited {result.returncode}, and Trident is {after.state}")
            elif current.state is None:
                raise StepError(f"Trident's Host Status can't be read: {last_lines(current.status_error, 3)}")
            else:
                raise StepError(f"Trident is {current.state}, which commit doesn't finish")
            current = self.probe_after_boot(boot, f"state-{len(self.seen)}")
            boot = current.boot_id

    def reboot(self, *, hard: bool = False) -> tuple[Probe, Probe]:
        before = self.probe("before-reboot")
        log("resetting the VM" if hard else "rebooting the VM")
        with self.timed("reboot"):
            if hard:
                with QMP() as qmp:
                    qmp.execute("system_reset")
            else:
                ssh("sudo systemctl reboot", check=False, timeout=30)
            boot = str(wait_for_new_boot(before.boot_id, BOOT_TIMEOUT))
            wait_settled()
        after = self.probe_after_boot(boot, "after-reboot")
        log(f"the VM booted {image_name(after.roothash)} on {after.pair_text}; Trident is {after.state}")
        self.record("reboot", hard=hard)
        return before, after

    def previous_slot(self, current: Probe) -> dict[str, str] | None:
        for device, slot in self.state.slots.items():
            if device != current.data_device:
                return slot
        return None

    def rollback(self, *, check_only: bool = False, check_id: str = "R1") -> Probe:
        """Roll the last A/B update back with trident rollback --ab, and check R1: the VM runs the
        previous slot and image, provisioned, with no lastError."""
        before = self.probe("before-rollback")
        if ssh("sudo trident rollback --help", check=False).returncode != 0:
            raise Skip("this Trident has no rollback command")
        checked = self.trident("rollback --check", "rollback-check")
        kind = (checked.stdout.strip().splitlines() or [""])[-1].strip().lower()
        if check_only:
            log(f"trident rollback --check: {kind or 'nothing'} (exit {checked.returncode})")
            return before
        c = self.checks
        if not c.expect(checked.returncode == 0 and kind == "ab", check_id,
                        "trident rollback --check reports an A/B rollback", f"it reports {kind!r} (exit {checked.returncode})"):
            raise StepError("there's no A/B update to roll back")
        self.save("rollback-target.yaml", self.trident("get rollback-target", "rollback-target").stdout)
        expected = self.previous_slot(before)
        self.seen = []
        with self.timed("rollback"):
            result = self.trident("rollback --ab", "rollback")
            boot = self.wait_for_reboot(before, result, "trident rollback --ab")
            after = self.drive_to_terminal(self.probe_after_boot(boot, "after-rollback-boot"))
        c.expect(after.pair != before.pair, check_id, "the VM booted the other slot", f"it booted {after.pair_text} again")
        if expected:
            c.expect(after.roothash == expected["roothash"], check_id,
                     f"/usr's root hash is {expected['image']}'s, which that slot held",
                     f"it's {image_name(after.roothash)}")
        c.expect(after.state == "provisioned", check_id, "servicingState is provisioned", f"it's {after.state}")
        if before.active_volume:
            c.expect(after.active_volume not in (None, before.active_volume), check_id, "abActiveVolume flipped back",
                     f"{before.active_volume} -> {after.active_volume}")
        c.expect(after.last_error is None, check_id, "lastError is empty", after.last_error or "")
        self.check_slot_agrees(check_id, after)
        self.check_boot_order(check_id, after)
        self.check_persistence(check_id, after)
        self.state.committed_update = None
        self.state.staged = None
        self.learn(after)
        self.record("rollback", image=image_name(after.roothash), states=self.seen)
        return after

    def health_rollback(self, target: Image, *, check_id: str = "H1") -> Probe:
        """Update to target with a health check that fails, and check H1: commit rolls back, and
        the VM ends provisioned on the slot and image it ran, with lastError naming the check."""
        before = self.probe("before-health-rollback")
        if before.state not in (None, "provisioned"):
            raise StepError(f"Trident is in {before.state}; finish that before updating")
        self.stage(target, health_check=True, check_id=check_id)
        staged = self.state.staged or {}
        self.seen = []
        with self.timed("health-rollback"):
            booted = self.finalize(verify=False)
            after = self.drive_to_terminal(booted)
        log(f"Trident went {' -> '.join(self.seen)}")
        last_error = after.last_error or self.trident("get last-error", "last-error").stdout.strip()
        c = self.checks
        c.expect(after.state == "provisioned", check_id, "Trident ends provisioned", f"it's {after.state}")
        c.expect(after.pair == before.pair, check_id, "the VM is back on the slot it ran before the update",
                 f"{before.pair_text} -> {after.pair_text}")
        c.expect(after.roothash == before.roothash, check_id, "/usr's root hash is the previous image's",
                 f"it's {image_name(after.roothash)}")
        c.expect(HEALTH_CHECK_NAME in last_error or "health" in last_error.lower(), check_id,
                 "lastError reports the failed health check", last_error[:400] or "lastError is empty")
        logs = ssh("sudo sh -c 'ls /var/lib/trident/trident-health-check-failure-*.log' 2>/dev/null",
                   check=False).stdout.split()
        if c.expect(bool(logs), check_id, "Trident wrote a health check failure log in /var/lib/trident"):
            self.save("health-check-failure.log", ssh(f"sudo cat {shlex.quote(logs[-1])}", check=False).stdout)
        if before.boot_count > 1 or after.boot_count > 1:
            booted_times = after.boot_count - before.boot_count
            c.expect(booted_times >= 2, check_id, "the VM booted the update, then the previous slot",
                     f"{booted_times} boot(s) since the update")
        else:
            c.skip(check_id, "the VM booted the update, then the previous slot", "the journal keeps no earlier boots")
        self.check_boot_order(check_id, after)
        self.check_persistence(check_id, after)
        if booted.data_device and booted.data_device != after.data_device and staged:
            self.state.slots[booted.data_device] = {"roothash": staged.get("roothash", ""), "image": staged.get("image", "")}
        self.state.staged = None
        self.state.committed_update = None
        self.record("health-rollback", image=staged.get("image", ""), states=self.seen)
        return after

    def verify(self, *, expect_target: str = "", expect_slot: str = "", expect_state: str = "",
               check_id: str = "V1") -> Probe:
        current = self.probe("verify")
        log(f"the VM runs {image_name(current.roothash)} on {current.pair_text}; Trident is {current.state}")
        c = self.checks
        if expect_target:
            image = resolve_target(expect_target, self.state, lambda: current.roothash)
            c.expect(current.roothash == image.roothash, check_id, f"the VM runs {image.name}",
                     f"it runs {image_name(current.roothash)}")
        if expect_slot:
            volume = f"volume-{expect_slot}"
            c.expect(current.active_volume == volume, check_id, f"abActiveVolume is {volume}",
                     f"it's {current.active_volume}")
        if expect_state:
            c.expect(current.state == expect_state, check_id, f"servicingState is {expect_state}", f"it's {current.state}")
        c.expect(current.data_device.startswith("/dev/"), check_id, "/usr is a dm-verity device", current.pair_text)
        self.check_slot_agrees(check_id, current)
        if current.state == "provisioned":
            self.check_boot_order(check_id, current)
        self.check_persistence(check_id, current)
        return current

    # Shared checks

    def check_persistence(self, check_id: str, current: Probe) -> None:
        c = self.checks
        if not self.state.markers:
            c.skip(check_id, "the markers persist", "no markers have been written yet")
        else:
            for path in MARKER_PATHS:
                missing = [token for token in self.state.markers if token not in current.markers.get(path, [])]
                c.expect(not missing, check_id, f"{path} holds every marker the harness wrote",
                         f"missing {', '.join(missing)}")
        c.expect(not current.usr_writable, check_id, "/usr is read-only")

    def check_boot_order(self, check_id: str, current: Probe) -> None:
        if not current.boot_current or not current.boot_order:
            self.checks.skip(check_id, "BootCurrent is first in BootOrder", "the UEFI variables can't be read")
            return
        self.checks.expect(current.boot_order[0] == current.boot_current, check_id,
                           f"BootCurrent {current.boot_label(current.boot_current)} is first in BootOrder",
                           f"BootOrder is {', '.join(current.boot_label(n) for n in current.boot_order)}")

    def check_slot_agrees(self, check_id: str, current: Probe) -> None:
        """Trident's abActiveVolume and the devices /usr was booted from name the same slot."""
        if current.state != "provisioned" or not current.active_volume:
            return
        known = self.state.slot_pairs.get(current.active_volume)
        if known is None:
            self.state.slot_pairs[current.active_volume] = list(current.pair)
            return
        self.checks.expect(tuple(known) == current.pair, check_id,
                           f"abActiveVolume {current.active_volume} is the slot the VM booted",
                           f"{current.active_volume} was {', '.join(known)}; the VM booted {current.pair_text}")

    def expect_same_slot(self, check_id: str, before: Probe, after: Probe) -> None:
        c = self.checks
        c.expect(after.boot_id != before.boot_id, check_id, "the VM rebooted")
        c.expect(after.pair == before.pair, check_id, "the VM booted the same slot again",
                 f"{before.pair_text} -> {after.pair_text}")
        c.expect(after.roothash == before.roothash, check_id, "/usr's root hash is unchanged",
                 f"{image_name(before.roothash)} -> {image_name(after.roothash)}")
        c.expect(after.state == "provisioned", check_id, "servicingState is provisioned", f"it's {after.state}")
        if before.active_volume:
            c.expect(after.active_volume == before.active_volume, check_id, "abActiveVolume is unchanged",
                     f"{before.active_volume} -> {after.active_volume}")
        self.check_boot_order(check_id, after)
        self.check_persistence(check_id, after)

    # Reporting

    def collect_diagnostics(self, label: str) -> None:
        directory = self.run_dir / f"diagnostics-{label}"
        directory.mkdir(exist_ok=True)
        try:
            if SERIAL_LOG.exists():
                with SERIAL_LOG.open("rb") as serial:
                    serial.seek(max(0, SERIAL_LOG.stat().st_size - 4 * 2**20))
                    (directory / "serial.log").write_bytes(serial.read())
            if vm_pid() is None or boot_id() is None:
                log(f"the VM isn't reachable; its serial log is in {directory}")
                return
            commands = {
                "journal.txt": "sudo journalctl -b --no-pager -n 5000",
                "journal-previous-boot.txt": "sudo journalctl -b -1 --no-pager -n 5000",
                "trident-status.yaml": "sudo trident get status 2>&1",
                "trident-last-error.yaml": "sudo trident get last-error 2>&1",
                "trident-units.txt": "systemctl status 'trident*' --all --no-pager -n 50",
                "trident-acl-agent.txt": f"systemctl cat {TRIDENT_AGENT_UNIT} --no-pager 2>&1; "
                                         f"sudo journalctl -b -u {TRIDENT_AGENT_UNIT} --no-pager -n 500",
                "failed-units.txt": "systemctl --failed --no-pager",
                "lsblk.txt": "lsblk -o NAME,SIZE,TYPE,FSTYPE,LABEL,PARTLABEL,MOUNTPOINTS",
                "efibootmgr.txt": "sudo efibootmgr -v 2>&1",
                "network.txt": "ip -br addr; ip route; networkctl list --no-pager",
                "unbounded-agent.txt": "systemctl status 'unbounded-agent*' --all --no-pager -n 0 2>&1; "
                                       "sudo journalctl -b -u 'unbounded-agent*' --no-pager -n 1000",
            }
            for name, command in commands.items():
                result = ssh(command, check=False, timeout=180)
                (directory / name).write_text(result.stdout + (f"\n## stderr\n{result.stderr}" if result.stderr.strip() else ""))
            bundle = f"{GUEST_DIR}/trident-diagnose.tar.zst"
            made = ssh(f"sudo mkdir -p {shlex.quote(GUEST_DIR)} && sudo rm -f {bundle} && "
                       f"sudo trident diagnose --output {bundle} --journal >/dev/null 2>&1 && sudo chmod a+r {bundle}",
                       check=False, timeout=600)
            if made.returncode == 0:
                copy_from_vm(bundle, directory / "trident-diagnose.tar.zst")
            log(f"diagnostics are in {directory}")
        except (StepError, OSError, subprocess.SubprocessError) as error:
            warn(f"couldn't collect every diagnostic: {error}")

    def finish(self) -> int:
        results = self.checks.results
        counts = {kind: sum(1 for result in results if result["result"] == kind) for kind in ("pass", "fail", "skip")}
        ok = counts["fail"] == 0 and not self.errors and all(s["result"] != "fail" for s in self.scenarios)
        elapsed = time.monotonic() - self.started
        summary = {
            "command": self.command, "ok": ok, "runDir": str(self.run_dir), "seconds": round(elapsed, 1),
            "counts": counts, "errors": self.errors, "scenarios": self.scenarios,
            "checks": results,
            "timings": [{"name": name, "seconds": round(seconds, 1)} for name, seconds in self.timings],
        }
        (self.run_dir / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
        for scenario in self.scenarios:
            log(f"{scenario['result'].upper():4} {scenario['name']} ({duration(scenario['seconds'])})"
                + (f": {scenario['reason']}" if scenario["reason"] else ""))
        for error in self.errors:
            log(f"error: {error}")
        log(f"{counts['pass']} passed, {counts['fail']} failed, {counts['skip']} skipped in {duration(elapsed)}; "
            f"{self.run_dir / 'summary.json'}")
        if _json_output:
            print(json.dumps(summary, indent=2))
        return 0 if ok else 1


# ---------------------------------------------------------------------------------------------
# Install


def install_dir(tag: str) -> Path:
    """Where the base installed from tag's ISO, with the current seed, is kept. The image reads the
    seed on its first boot only, so each seed has its own install: a harness with another seed,
    sharing the cache, never replaces the base under a VM that runs on it."""
    return INSTALLED / f"{f'local-{Path(BASE_ISO).name}' if BASE_ISO else tag}-{seed_digest()[:12]}"


def install_needed(installed: Path, fresh: bool) -> str:
    """Why up has to run the installer, or "" if the cached install will do."""
    if fresh:
        return "--fresh-install was given"
    if not (installed / "info.json").is_file():
        return f"there's no installed base in {installed}"
    return ""


def ensure_ssh_key() -> str:
    SSH_KEY.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    if not SSH_KEY.exists():
        run(["ssh-keygen", "-t", "ed25519", "-N", "", "-q", "-C", "acl-abupdate", "-f", str(SSH_KEY)])
    return SSH_KEY.with_suffix(".pub").read_text().strip()


def wait_for_install(timeout: float) -> None:
    """Wait for the installed image's first boot to finish provisioning: the installer streams
    the base COSI to the disk and reboots, and the disk's first boot reads the seed."""
    started = reported = time.monotonic()
    offset, tail = 0, ""
    waiting_since: float | None = None
    while True:
        if vm_pid() is None:
            raise StepError(f"QEMU exited during the install; see {SERIAL_LOG}")
        text, offset = read_serial(offset)
        tail = (tail + text)[-8192:]
        if "WAITING FOR CONFIGURATION MEDIA" in text and waiting_since is None:
            waiting_since = time.monotonic()
            log(f"the installed image is waiting for its configuration media, the {SEED_LABEL} volume")
        if waiting_since is not None and time.monotonic() - waiting_since > 300:
            raise StepError(f"the installed image waited five minutes for a volume labelled {SEED_LABEL}; see {SERIAL_LOG}")
        if boot_id():
            return
        now = time.monotonic()
        if now - started > timeout:
            raise StepError(f"the install didn't finish in {duration(timeout)}; see {SERIAL_LOG}")
        if now - reported >= 60:
            reported = now
            log(f"installing, {duration(now - started)}: {last_serial_line(tail)}")
        time.sleep(10)


def install_base(state: State, iso: Image, destination: Path) -> None:
    """Install the base image from its ISO on a new disk, with the seed attached for its first
    boot, then power off. The disk, UEFI variables, and SSH key are kept in the cache, and each
    up starts from an overlay on them."""
    partial = destination.with_name(destination.name + ".partial")
    shutil.rmtree(partial, ignore_errors=True)
    partial.mkdir(parents=True)
    seed = build_seed(partial, ensure_ssh_key())
    disk, variables = partial / "disk.qcow2", partial / "vars.qcow2"
    run(["qemu-img", "create", "-q", "-f", "qcow2", str(disk), DISK_SIZE], capture=True)
    _, template = ovmf_firmware()
    run(["qemu-img", "convert", "-f", "raw", "-O", "qcow2", str(template), str(variables)], capture=True)
    log(f"installing {iso.name} on a new {DISK_SIZE} disk; `{PROG} console -f` follows it")
    started = time.monotonic()
    launch_vm(disk=disk, variables=variables, memory=state.memory, cpus=state.cpus, ssh_port=state.ssh_port,
              installer=iso.path, seed=seed)
    try:
        wait_for_install(INSTALL_TIMEOUT)
        installed = probe()
        if not installed.data_device.startswith("/dev/vd"):
            raise StepError(f"SSH reached a system whose /usr isn't on the VM's disk "
                            f"({installed.pair_text}); see {SERIAL_LOG}")
        log(f"installed in {duration(time.monotonic() - started)}: /usr on {installed.pair_text}; "
            "powering off to detach the installer and seed")
        # A disk that stopped booting would fall through to the installer, which would install the
        # base again and look like a rollback. So later boots have only the disk attached.
        stop_vm()
    except BaseException:
        stop_vm(hard=True)
        raise
    shutil.copytree(SSH_KEY.parent, partial / "ssh")
    seed.unlink()
    shutil.rmtree(partial / "seed")
    (partial / "info.json").write_text(json.dumps({
        "iso": iso.name, "isoInfo": iso.info, "installed": now_iso(), "diskSize": DISK_SIZE,
        "roothash": installed.roothash, "seedDigest": seed_digest(),
    }, indent=2) + "\n")
    shutil.rmtree(destination, ignore_errors=True)
    partial.rename(destination)


def prepare_overlay(installed: Path) -> None:
    SSH_KEY.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    for name in ("id_ed25519", "id_ed25519.pub"):
        shutil.copyfile(installed / "ssh" / name, SSH_KEY.parent / name)
    SSH_KEY.chmod(0o600)
    VM_DISK.unlink(missing_ok=True)
    run(["qemu-img", "create", "-q", "-f", "qcow2", "-b", str((installed / "disk.qcow2").resolve()),
         "-F", "qcow2", str(VM_DISK)], capture=True)
    shutil.copyfile(installed / "vars.qcow2", VM_VARS)


def baseline(ctx: Context) -> None:
    """B1: the VM runs the installed base from its disk."""
    current = ctx.probe("baseline")
    c = ctx.checks
    c.expect(current.data_device.startswith("/dev/vd"), "B1", "the VM runs from its disk",
             f"/usr's data device is {current.data_device or 'not a verity device'}")
    c.expect(bool(current.roothash), "B1", "/usr is a dm-verity device with a root hash")
    c.expect(not current.usr_writable, "B1", "/usr is read-only")
    name = ""
    base = None
    try:
        base = cosi_if_present(ctx.state.base_tag, ctx.state)
    except StepError as error:
        warn(f"can't read the base COSI: {error}")
    if base is None:
        log(f"the {ctx.state.base_tag} COSI isn't cached, so /usr's root hash isn't compared with it; "
            "`image fetch` pulls it")
    elif current.roothash == base.roothash:
        name = base.name
        log(f"/usr's root hash is the base COSI's ({base.name})")
    else:
        warn(f"/usr's root hash {short(current.roothash)} isn't the {base.name} COSI's "
             f"({short(base.roothash)}); the installer ISO may embed another build")
    status = f"servicingState {current.state}" if current.state else (
        f"no Host Status yet ({first_line(current.status_error) or 'no output'}); the first update initializes it")
    log(f"{current.os_release.get('PRETTY_NAME', 'unknown OS')}, kernel {current.kernel}, "
        f"Trident {current.trident_version or '?'}; {status}")
    ctx.learn(current, name or f"installed from {ctx.state.base_tag}")


# ---------------------------------------------------------------------------------------------
# Commands


def image_fetch(args: argparse.Namespace) -> None:
    state = State.load() if STATE_FILE.is_file() else None
    if state:
        use_images(state)
    base, update = (state.base_tag, state.update_tag) if state else (BASE_TAG, UPDATE_TAG)
    if not args.tags and not (base and update):
        die(missing_settings(["ACL_AB_BASE_TAG" if not base else "", "ACL_AB_UPDATE_TAG" if not update else ""]))
    tags = args.tags or [base, update]
    if not args.tags:
        iso_tags = [base]
    else:
        iso_tags = args.tags if args.iso else []
    for tag in iso_tags:
        log(base_iso(tag, refresh=args.refresh).describe())
    for tag in tags:
        log(cosi(tag, state, refresh=args.refresh).describe())


def image_list(_: argparse.Namespace) -> None:
    if STATE_FILE.is_file():
        use_images(State.load())
    rows = []
    for kind, repo in (("iso", ISO_REPO), ("cosi", COSI_REPO)):
        for directory in sorted((IMAGES / repo).iterdir()) if repo and (IMAGES / repo).is_dir() else []:
            image = cached_image(directory, kind)
            if image:
                rows.append({"kind": kind, "tag": image.name, "build": image.build, "os": image.version,
                             "versionId": str((image.info.get("osRelease") or {}).get("VERSION_ID", "")),
                             "roothash": image.roothash, "metadataSha384": image.sha384,
                             "size": int(image.info.get("size", 0)), "path": str(image.path)})
    builds = [{"name": image.name, "from": image.built.get("from", {}).get("name", ""),
               "version": image.built.get("version", ""), "created": image.built.get("created", ""),
               "files": image.built.get("files", []), "config": image.built.get("config"),
               "roothash": image.roothash, "metadataSha384": image.sha384,
               "size": int(image.info.get("size", 0)), "path": str(image.path)} for image in known_builds()]
    installed = [{"tag": path.name, **json.loads((path / "info.json").read_text())}
                 for path in sorted(INSTALLED.glob("*")) if (path / "info.json").is_file()]
    if _json_output:
        print(json.dumps({"images": rows, "builds": builds, "installed": installed}, indent=2))
        return
    if not rows and not builds:
        log(f"no images in {IMAGES}; `image fetch` pulls them")
    for row in rows:
        print(f"{row['kind']:5} {row['tag']:14} build {row['build'] or '-':10} OS {row['os'] or '-':22} "
              f"root hash {short(row['roothash']):12} sha384 {short(row['metadataSha384']):12} {human_size(row['size'])}")
    for row in builds:
        print(f"build {row['name']:14} from {row['from'] or '-':10} version {row['version'] or '-':30} "
              f"root hash {short(row['roothash']):12} sha384 {short(row['metadataSha384']):12} {human_size(row['size'])}")
    for entry in installed:
        print(f"installed base {entry['tag']} from {entry.get('iso', '?')}, {entry.get('installed', '?')}")


def check_build_prerequisites() -> None:
    missing = [tool for tool in ("podman", "zstd", "mdir", "mtype", "mcopy") if not shutil.which(tool)]
    if missing:
        die(f"image build needs {', '.join(missing)}; mdir, mtype, and mcopy are in mtools")
    if run(["sudo", "-n", "true"], check=False, capture=True).returncode != 0:
        die("image build needs sudo without a password prompt, for Image Customizer's privileged container; "
            "run `sudo -v` first if your sudo caches credentials")


def build_source(spec: str, state: State | None) -> Image:
    """The image a build starts from: a target as `update --to` takes one, but not `other`. Without
    a VM, base and update are the tags the settings give."""
    if spec == "other":
        raise StepError("--from takes base, update, a build, a tag, or a .cosi file")
    if state is None and spec in ("base", "update"):
        tag, variable = (BASE_TAG, "ACL_AB_BASE_TAG") if spec == "base" else (UPDATE_TAG, "ACL_AB_UPDATE_TAG")
        if not tag:
            raise StepError(missing_settings([variable]))
    return resolve_target(spec, state or State(), lambda: "")


def build_name_problem(name: str) -> str:
    if name in ("base", "update", "other") or not BUILD_NAME_PATTERN.fullmatch(name):
        return (f"{name!r} can't name a build: use up to 40 lowercase letters, digits, and hyphens, starting with "
                "a letter, other than base, update, and other")
    return ""


def image_build(args: argparse.Namespace) -> None:
    name = args.name
    if build_name_problem(name):
        die(build_name_problem(name))
    if not args.files and not args.config:
        die("give --files DIR, --config FILE, or both")
    if args.os_version and not is_semver(args.os_version):
        die(f"--os-version {args.os_version!r} isn't a semver version, which Nebraska needs")
    files_dir = Path(args.files).expanduser().resolve() if args.files else None
    config = Path(args.config).expanduser().resolve() if args.config else None
    if config and not config.is_file():
        die(f"--config {config} isn't a file")
    files = cosibuild.collect_files(files_dir) if files_dir else []
    check_build_prerequisites()
    state = State.load() if STATE_FILE.is_file() else None
    if state:
        use_images(state)
    source = build_source(args.source, state)
    previous = build_image(name)
    os_version, count = build_version(name, source, previous, args.os_version)

    BUILDS.mkdir(parents=True, exist_ok=True)
    partial = BUILDS / f"{name}.partial"
    shutil.rmtree(partial, ignore_errors=True)
    partial.mkdir()
    adding = []
    if files:
        adding.append(f"{len(files)} file{'s' if len(files) != 1 else ''} from {files_dir}")
    if config:
        adding.append(f"the config {config}")
    log(f"building {name} from {source.name} (/usr root hash {short(source.roothash)}), adding {' and '.join(adding)}")
    started = time.monotonic()
    request = cosibuild.Request(source=source.path, files_dir=files_dir, files=files, config=config,
                                image_customizer=IMAGE_CUSTOMIZER, work=BUILDS / f"{name}.work",
                                output=partial / f"{name}.cosi", keep_work=args.keep_work, os_version=os_version)
    try:
        result = cosibuild.build(request, log)
    except StepError as error:
        raise StepError(f"{error}\nThe build's config and Image Customizer's log are in {partial}") from error
    if config:
        shutil.copy2(config, partial / f"config-{config.name}")
    info = describe_file(request.output, "cosi")
    info["built"] = {"from": {"target": args.source, "name": source.name, "roothash": source.roothash,
                              "path": str(source.path)},
                     "version": os_version or source.version, "count": count, "created": now_iso(),
                     "filesDir": str(files_dir) if files_dir else None, "config": str(config) if config else None,
                     **result}
    (partial / "info.json").write_text(json.dumps(info, indent=2) + "\n")
    shutil.rmtree(BUILDS / name, ignore_errors=True)
    partial.rename(BUILDS / name)
    image = build_image(name)
    if image is None:
        raise StepError(f"the build left no image in {BUILDS / name}")
    if previous and previous.roothash != image.roothash:
        log(f"replaced the previous {name}, whose /usr root hash was {short(previous.roothash)}")
    log(f"built {image.describe()}, in {duration(time.monotonic() - started)}")
    log(f"update to it with `{PROG} update --to {name}`")
    if _json_output:
        print(json.dumps(info, indent=2))


def image_delete(args: argparse.Namespace) -> None:
    name = args.name
    directories = [BUILDS / name, BUILDS / f"{name}.partial", BUILDS / f"{name}.work"]
    if BUILD_NAME_PATTERN.fullmatch(name) and any(directory.exists() for directory in directories):
        for directory in directories:
            cosibuild.remove_work(directory)
        log(f"deleted build {name}")
        return
    if STATE_FILE.is_file():
        use_images(State.load())
    cached = [IMAGES / repo / name for repo in dict.fromkeys((ISO_REPO, COSI_REPO))
              if repo and TAG_PATTERN.fullmatch(name) and (IMAGES / repo / name).is_dir()]
    if not cached:
        die(f"there's no build or cached tag {name!r}; `image list` shows them")
    for directory in cached:
        shutil.rmtree(directory)
        log(f"deleted {directory}")


def cmd_image(args: argparse.Namespace) -> None:
    {"fetch": image_fetch, "list": image_list, "build": image_build, "delete": image_delete}[args.action](args)


def up_settings_missing(base_tag: str, update_tag: str) -> list[str]:
    """The image settings up needs that aren't set: the tags always, as they name the images; the
    registry and the repositories unless local files stand in for what they'd give."""
    missing = ["ACL_AB_BASE_TAG" if not base_tag else "", "ACL_AB_UPDATE_TAG" if not update_tag else ""]
    needs_iso = not BASE_ISO
    needs_cosi = not (BASE_COSI and UPDATE_COSI)
    if (needs_iso or needs_cosi) and not REGISTRY_PATH:
        missing.append("ACL_AB_REGISTRY")
    if needs_iso and not ISO_REPO:
        missing.append("ACL_AB_ISO_REPO")
    if needs_cosi and not COSI_REPO:
        missing.append("ACL_AB_COSI_REPO")
    return [name for name in missing if name]


def set_identity(hostname: str) -> None:
    """Give the VM its own hostname and machine ID, and reboot so every service picks them up.
    VMs started from one installed base would otherwise share its machine ID, from which
    trident-acl-agent derives the node's Nebraska instance ID. Without a stored ID, systemd makes
    a new one, from the VM's SMBIOS UUID, which up gives each VM."""
    previous = boot_id() or ""
    ssh(f"sudo hostnamectl set-hostname {shlex.quote(hostname)} && sudo rm -f /etc/machine-id && "
        "sudo systemd-machine-id-setup >/dev/null && (sudo systemctl reboot || true)", check=False, timeout=60)
    wait_for_new_boot(previous, BOOT_TIMEOUT)
    wait_settled()
    found = ssh("hostname; cat /etc/machine-id", timeout=30).stdout.split()
    if found[:1] != [hostname]:
        raise StepError(f"the VM's hostname is {found[:1]}, not {hostname}")
    log(f"the VM is {hostname}, with machine ID {found[1] if len(found) > 1 else '?'}")


def cmd_up(args: argparse.Namespace) -> None:
    check_vm_prerequisites()
    missing = up_settings_missing(args.base_tag, args.update_tag)
    if missing:
        die(missing_settings(missing))
    if bool(args.nic_tap) != bool(args.nic_address):
        die("give both --nic-tap and --nic-address, or neither")
    if args.nic_address and not re.fullmatch(r"(\d{1,3}\.){3}\d{1,3}", args.nic_address):
        die(f"--nic-address {args.nic_address!r} isn't an IPv4 address")
    if args.hostname and not HOSTNAME_PATTERN.fullmatch(args.hostname):
        die(f"--hostname {args.hostname!r} isn't a hostname")
    claim_state_dir()
    with state_lock():
        if STATE_FILE.exists() or VM_DISK.exists():
            die(f"{STATE} has a VM already; `start` it, or `down` it first")
        state = State(base_tag=args.base_tag, update_tag=args.update_tag, ssh_port=args.ssh_port,
                      memory=args.memory, cpus=args.cpus, smbios_uuid=str(uuid.uuid4()),
                      registry=REGISTRY_PATH, iso_repo=ISO_REPO, cosi_repo=COSI_REPO)
        if args.nic_tap:
            state.nic = {"tap": args.nic_tap, "address": args.nic_address, "mac": nic_mac(args.nic_address)}
        use_port(state.ssh_port)
        ctx = Context("up", state)
        try:
            installed = install_dir(state.base_tag)
            reason = install_needed(installed, args.fresh_install)
            if reason:
                log(f"installing the base: {reason}")
                install_base(state, base_iso(state.base_tag), installed)
            else:
                log(f"starting from the base installed in {installed}; --fresh-install installs it again")
            prepare_overlay(installed)
            state.installed = str(installed)
            state.save()
            launch(state)
            wait_for_ssh(BOOT_TIMEOUT)
            wait_settled()
            if args.hostname:
                set_identity(args.hostname)
                state.hostname = args.hostname
                state.save()
            baseline(ctx)
            if state.nic:
                configure_nic(state.nic["address"], state.nic["mac"])
            save_snapshot(state, "base", force=True, summary=f"{state.base_tag}, freshly installed")
        except StepError as error:
            log(f"up failed: {error}")
            ctx.errors.append(str(error))
            ctx.collect_diagnostics("up")
            ctx.finish()
            die(f"the serial log is {SERIAL_LOG}; `{PROG} down` removes what up made", 1)
        code = ctx.finish()
    log(f"the VM is up; try `{PROG} status`, `{PROG} test`, or `{PROG} update`")
    sys.exit(code)


def cmd_start(args: argparse.Namespace) -> None:
    with state_lock():
        state = require_state()
        if vm_pid() is not None:
            die("the VM is running already")
        if args.snapshot:
            restore_snapshot(args.snapshot, state)
        else:
            launch(state)
            wait_for_ssh(BOOT_TIMEOUT)
            wait_settled()
    log("the VM is running")


def cmd_stop(args: argparse.Namespace) -> None:
    with state_lock():
        require_state()
        stop_vm(hard=args.hard)
    log("the VM is stopped")


def cmd_resume(args: argparse.Namespace) -> None:
    """Run the VM a held restore left paused: step its clock to the host's, and restart the units
    given, whose connections went stale while the snapshot sat, as a watch on the API server does."""
    with state_lock():
        require_running()
        with QMP() as qmp:
            qmp.execute("cont")
        wait_for_ssh(120)
        sync_guest_clock()
        for unit in args.restart:
            ssh(f"sudo systemctl try-restart {shlex.quote(unit)}", check=False, timeout=120)
    log("the VM is running")


def cmd_fail_next_boot(args: argparse.Namespace) -> None:
    """Reset the VM as its next kernel starts, which uses its one-time BootNext: it then boots the
    first entry of BootOrder, as a failed first boot of a new slot would. The caller starts this
    before the reboot it targets, such as one trident-acl-agent makes."""
    with state_lock():
        require_running()
        offset = SERIAL_LOG.stat().st_size if SERIAL_LOG.exists() else 0
        command_line = reset_at_next_kernel(offset, args.timeout)
    if _json_output:
        print(json.dumps({"commandLine": command_line, "slot": kernel_slot(command_line)}))


def cmd_down(args: argparse.Namespace) -> None:
    if STATE.is_dir():
        with state_lock():
            state = State.load() if STATE_FILE.is_file() else None
            if state:
                use_port(state.ssh_port)
            stop_vm(hard=True)
    if STATE.resolve() == DEFAULT_STATE.resolve() or STATE_MARKER.exists():
        shutil.rmtree(STATE, ignore_errors=True)
        log(f"removed the VM and {STATE}")
    else:
        log(f"stopped the VM; not removing {STATE}, which up didn't make")
    if CACHE.is_dir():
        log(f"kept the downloads and installed bases in {CACHE}; remove it to reclaim the space")


def cmd_status(_: argparse.Namespace) -> None:
    if not STATE_FILE.is_file():
        if _json_output:
            print(json.dumps({"exists": False, "running": False}))
        log(f"there's no VM in {STATE}; run `up`")
        return
    state = require_state()
    pid = vm_pid()
    info: dict[str, Any] = {
        "exists": True, "running": pid is not None, "pid": pid,
        "ssh": f"ssh -p {state.ssh_port} {SSH_USER}@127.0.0.1", "sshKey": str(SSH_KEY),
        "hostname": state.hostname or DEFAULT_HOSTNAME, "nic": state.nic,
        "baseTag": state.base_tag, "updateTag": state.update_tag, "snapshots": snapshot_names(),
        "staged": state.staged, "committedUpdate": state.committed_update, "markers": len(state.markers),
    }
    current = None
    if pid is not None:
        try:
            current = probe()
        except StepError as error:
            info["error"] = str(error)
    if current:
        agent = current.trident_agent
        info.update({
            "image": image_name(current.roothash), "roothash": current.roothash, "slot": list(current.pair),
            "servicingState": current.state, "abActiveVolume": current.active_volume,
            "lastError": current.last_error, "statusError": current.status_error or None,
            "imageUrl": current.image_url, "bootCurrent": current.boot_current, "bootNext": current.boot_next,
            "bootOrder": current.boot_order, "os": current.os_release.get("PRETTY_NAME"),
            "versionId": current.os_release.get("VERSION_ID"),
            "kernel": current.kernel, "trident": current.trident_version,
            "tridentAclAgent": {
                "loadState": agent.get("LoadState"), "enabled": agent.get("is-enabled"),
                "activeState": agent.get("ActiveState"), "subState": agent.get("SubState"),
                "restarts": agent.get("NRestarts"), "dropInPaths": agent.get("DropInPaths", "").split(),
                "tridentSocket": agent.get("socket"),
            },
        })
    if _json_output:
        print(json.dumps(info, indent=2))
        return
    print(f"VM        {'running, pid ' + str(pid) if pid else 'stopped'}; {info['ssh']} (key {SSH_KEY})")
    nic = f"; second NIC {state.nic['address']} on {state.nic['tap']}" if state.nic else ""
    print(f"Host      {info['hostname']}{nic}")
    print(f"Images    base {state.base_tag}, update {state.update_tag}")
    if current:
        print(f"Booted    {info['image']} on {current.pair_text}")
        print(f"Trident   {current.state or 'no Host Status: ' + first_line(current.status_error)}"
              f", abActiveVolume {current.active_volume or '-'}, lastError {'set' if current.last_error else 'none'}")
        print(f"Image     {current.image_url or '-'}")
        print(f"UEFI      BootCurrent {current.boot_label(current.boot_current) if current.boot_current else '-'}, "
              f"BootNext {current.boot_next or '-'}, BootOrder {', '.join(current.boot_order) or '-'}")
        print(f"OS        {info['os'] or '-'}, kernel {current.kernel or '-'}, Trident {current.trident_version or '-'}")
        agent = current.trident_agent
        if agent.get("LoadState") == "loaded":
            dropins = ", ".join(Path(path).name for path in agent.get("DropInPaths", "").split()) or "none"
            print(f"Agent     {TRIDENT_AGENT_UNIT} {agent.get('is-enabled') or '?'}, "
                  f"{agent.get('ActiveState', '?')}/{agent.get('SubState', '?')}, {agent.get('NRestarts', '?')} restarts, "
                  f"drop-ins {dropins}; tridentd socket {agent.get('socket', '?')}")
        else:
            print(f"Agent     {TRIDENT_AGENT_UNIT} isn't installed ({agent.get('LoadState') or 'unknown'})")
    elif "error" in info:
        print(f"Probe     {info['error']}")
    print(f"Staged    {state.staged['image'] if state.staged else '-'}")
    print(f"Committed {state.committed_update['image'] if state.committed_update else '-'}")
    print(f"Snapshots {', '.join(info['snapshots']) or '-'}")


def cmd_ssh(args: argparse.Namespace) -> None:
    require_running()
    command = args.command[1:] if args.command[:1] == ["--"] else args.command
    base = ssh_command()
    os.execvp("ssh", [*base[:-1], *(["-t"] if not command else []), base[-1], *command])


def cmd_console(args: argparse.Namespace) -> None:
    if not SERIAL_LOG.exists():
        die(f"there's no serial log at {SERIAL_LOG}")
    os.execvp("tail", ["tail", "-n", "+1", *(["-f"] if args.follow else []), str(SERIAL_LOG)])


def cmd_snapshot(args: argparse.Namespace) -> None:
    if args.action == "list":
        require_state()
        names = snapshot_names()
        if _json_output:
            print(json.dumps([json.loads((SNAPSHOT_DIR / f"{name}.json").read_text()) | {"state": None}
                              for name in names], indent=2))
            return
        for name in names:
            meta = json.loads((SNAPSHOT_DIR / f"{name}.json").read_text())
            print(f"{name:20} {meta.get('created', '?')}  {meta.get('summary', '')}")
        if not names:
            log("no snapshots")
        return
    with state_lock():
        state = require_state()
        if args.action == "save":
            summary = ""
            if vm_pid() is not None:
                with contextlib.suppress(StepError):
                    current = probe()
                    summary = f"{image_name(current.roothash)} on {current.pair_text}, {current.state or 'no Host Status'}"
            save_snapshot(state, args.name, force=args.force, summary=summary)
        elif args.action == "restore":
            restore_snapshot(args.name, state, hold=args.hold)
            log(f"restored snapshot {args.name}")
        else:
            delete_snapshot(args.name)


def drive(command: str, action: Callable[[Context], None], *, single_step: bool = False) -> NoReturn:
    """Run a step command: the action's checks, and diagnostics if it fails."""
    with state_lock():
        state = require_running()
        ctx = Context(command, state)
        ctx.single_step = single_step
        try:
            action(ctx)
        except Skip as skip:
            ctx.checks.skip(command, f"the {command} step", str(skip))
        except StepError as error:
            ctx.errors.append(str(error))
            log(f"FAIL: {error}")
            ctx.collect_diagnostics(command)
        finally:
            ctx.state.save()
        code = ctx.finish()
    sys.exit(code)


def cmd_update(args: argparse.Namespace) -> None:
    def action(ctx: Context) -> None:
        target = ctx.target(args.to)
        if args.failing_health_check:
            ctx.health_rollback(target)
        else:
            ctx.update(target, commit=not args.no_commit)

    drive("update", action, single_step=args.single_step)


def cmd_stage(args: argparse.Namespace) -> None:
    drive("stage", lambda ctx: ctx.stage(ctx.target(args.to), health_check=args.failing_health_check),
          single_step=args.single_step)


def cmd_finalize(args: argparse.Namespace) -> None:
    drive("finalize", lambda ctx: ctx.finalize())


def cmd_commit(_: argparse.Namespace) -> None:
    drive("commit", lambda ctx: ctx.commit())


def cmd_rollback(args: argparse.Namespace) -> None:
    drive("rollback", lambda ctx: ctx.rollback(check_only=args.check))


def cmd_reboot(args: argparse.Namespace) -> None:
    drive("reboot", lambda ctx: ctx.reboot(hard=args.hard))


def cmd_verify(args: argparse.Namespace) -> None:
    drive("verify", lambda ctx: ctx.verify(expect_target=args.expect, expect_slot=args.expect_slot,
                                           expect_state=args.expect_state))


def cmd_scenarios(_: argparse.Namespace) -> None:
    if _json_output:
        print(json.dumps([{"name": s.name, "description": s.description, "needs": s.needs.value,
                           "checks": list(s.checks), "target": s.target, "experimental": s.experimental}
                          for s in REGISTRY.values()], indent=2))
        return
    for item in REGISTRY.values():
        flags = f"needs {item.needs.value}" + (", experimental" if item.experimental else "")
        print(f"{item.name:20} {', '.join(item.checks):34} {flags}")
        print(textwrap.indent(textwrap.fill(item.description, 76), " " * 21))


def run_scenarios(command: str, names: list[str], *, from_snapshot: str, rounds: int, target: str,
                  keep_going: bool, no_setup: bool, single_step: bool) -> NoReturn:
    if from_snapshot and not (SNAPSHOT_DIR / f"{from_snapshot}.json").is_file():
        die(f"there's no snapshot {from_snapshot}; `snapshot list` shows them")
    with state_lock():
        state = require_state()
        if vm_pid() is None and not from_snapshot:
            die("the VM isn't running; `start` it, or pass --from SNAPSHOT")
        ctx = Context(command, state)
        ctx.rounds, ctx.target_spec, ctx.no_setup, ctx.single_step = rounds, target, no_setup, single_step
        for name in names:
            if from_snapshot:
                try:
                    ctx.state = restore_snapshot(from_snapshot, ctx.state)
                except StepError as error:
                    ctx.errors.append(f"restoring {from_snapshot} for {name}: {error}")
                    break
            result = ctx.run_scenario(REGISTRY[name])
            ctx.state.save()
            if result == "fail" and not keep_going and name != names[-1]:
                log("stopping at the first failure; --keep-going runs the rest")
                break
        code = ctx.finish()
    sys.exit(code)


def cmd_run(args: argparse.Namespace) -> None:
    names = list(REGISTRY) if "all" in args.scenarios else list(dict.fromkeys(args.scenarios))
    run_scenarios("run", names, from_snapshot=args.from_snapshot, rounds=args.rounds, target=args.to,
                  keep_going=args.keep_going, no_setup=args.no_setup, single_step=args.single_step)


def cmd_test(args: argparse.Namespace) -> None:
    run_scenarios("test", list(REGISTRY), from_snapshot="base", rounds=args.rounds, target="",
                  keep_going=not args.stop_on_failure, no_setup=False, single_step=args.single_step)


def positive(value: str) -> int:
    if not value.isdigit() or int(value) < 1:
        raise argparse.ArgumentTypeError(f"{value!r} isn't a positive whole number")
    return int(value)


TARGET_HELP = "base, update, other (whichever of those the VM doesn't run), a build, a registry tag, or a .cosi file"


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog=PROG, description=__doc__.split("\n\n", 1)[0],
                                     epilog="See hack/acl-abupdate/README.md.")
    parser.add_argument("--json", action="store_true",
                        help="print the result as JSON on stdout, and the log on stderr")
    commands = parser.add_subparsers(dest="command", required=True, metavar="COMMAND")

    def command(name: str, func: Callable[[argparse.Namespace], None], summary: str) -> argparse.ArgumentParser:
        sub = commands.add_parser(name, help=summary, description=summary)
        sub.set_defaults(func=func)
        return sub

    image = command("image", cmd_image, "fetch, list, build, or delete images")
    image_actions = image.add_subparsers(dest="action", required=True, metavar="ACTION")
    fetch = image_actions.add_parser("fetch", help="pull the base tag's installer ISO and the base and update COSIs, "
                                                   "or the COSIs of the tags given, into the cache")
    fetch.add_argument("tags", nargs="*", metavar="TAG")
    fetch.add_argument("--iso", action="store_true", help="pull the given tags' installer ISOs too")
    fetch.add_argument("--refresh", action="store_true", help="pull again what the cache has")
    image_actions.add_parser("list", help="list the cached images, builds, and installed bases")
    build = image_actions.add_parser("build", help="build a COSI from a base and your files or Image Customizer "
                                                   "config, to update to; see README.md")
    build.add_argument("name", metavar="NAME", help="the build's name, which targets take")
    build.add_argument("--files", metavar="DIR", help="files to add, laid out like the root: usr/... and etc/...")
    build.add_argument("--config", metavar="FILE", help="an Image Customizer config to apply too")
    build.add_argument("--from", dest="source", default="base", metavar="TARGET",
                       help="the image to build on: base, update, a build, a tag, or a .cosi file (default base)")
    build.add_argument("--os-version", default="", metavar="VERSION",
                       help="its os-release VERSION_ID, which Nebraska offers it as and trident-acl-agent reports "
                            "(default: its source's, with -NAME.N, which sorts below the source's)")
    build.add_argument("--keep-work", action="store_true", help="keep the work directory, with its raw disks")
    image_actions.add_parser("delete", help="delete a build, or a tag's cached images").add_argument("name")

    up = command("up", cmd_up, "install the base image in a new VM, or start from the base installed "
                               "earlier, check it (B1), and save snapshot base")
    up.add_argument("--base-tag", default=BASE_TAG, metavar="TAG", help="image installed (or ACL_AB_BASE_TAG)")
    up.add_argument("--update-tag", default=UPDATE_TAG, metavar="TAG", help="target `update` (or ACL_AB_UPDATE_TAG)")
    up.add_argument("--fresh-install", action="store_true", help="install from the ISO even if the cache has the base")
    up.add_argument("--memory", type=positive, default=VM_MEMORY, metavar="MIB", help=f"VM memory (default {VM_MEMORY})")
    up.add_argument("--cpus", type=positive, default=VM_CPUS, metavar="N", help=f"VM CPUs (default {VM_CPUS})")
    up.add_argument("--ssh-port", type=positive, default=SSH_PORT, metavar="PORT",
                    help=f"host port forwarded to the VM's SSH (default {SSH_PORT})")
    up.add_argument("--nic-tap", default="", metavar="TAP",
                    help="attach a second NIC to this TAP device, which must exist, in a bridge you own")
    up.add_argument("--nic-address", default="", metavar="IP", help="the second NIC's address, in a /24")
    up.add_argument("--hostname", default="", metavar="NAME",
                    help="the VM's hostname, with a machine ID of its own, for VMs from one installed base")
    start = command("start", cmd_start, "start the stopped VM")
    start.add_argument("--snapshot", metavar="NAME", help="start it from a snapshot")
    stop = command("stop", cmd_stop, "power the VM off; its disk and snapshots stay")
    stop.add_argument("--hard", action="store_true", help="stop QEMU at once, as pulling the power would")
    command("down", cmd_down, "stop the VM and remove its state; the cache stays")
    resume = command("resume", cmd_resume, "run a VM `snapshot restore --hold` left paused")
    resume.add_argument("--restart", action="append", default=[], metavar="UNIT",
                        help="a unit to restart once it runs, such as one with a watch that went stale")
    fail = command("fail-next-boot", cmd_fail_next_boot,
                   "reset the VM as its next kernel starts, so a new slot's first boot fails")
    fail.add_argument("--timeout", type=positive, default=1800, metavar="SECONDS",
                      help="how long to wait for that boot (default 1800)")
    command("status", cmd_status, "show the VM, its booted slot and image, Trident's state, and the UEFI boot variables")
    ssh_parser = command("ssh", cmd_ssh, "open a shell on the VM, or run a command there")
    ssh_parser.add_argument("command", nargs=argparse.REMAINDER)
    console = command("console", cmd_console, "show the VM's serial console log")
    console.add_argument("-f", "--follow", action="store_true", help="keep following it")

    snapshot = command("snapshot", cmd_snapshot, "save, restore, list, or delete snapshots of the running VM")
    actions = snapshot.add_subparsers(dest="action", required=True, metavar="ACTION")
    save = actions.add_parser("save", help="save the running VM, memory included")
    save.add_argument("name")
    save.add_argument("--force", action="store_true", help="replace a snapshot of that name")
    restore = actions.add_parser("restore", help="return the VM to a snapshot")
    restore.add_argument("name")
    restore.add_argument("--hold", action="store_true",
                         help="leave the VM paused at it, for `resume`, so you can reset what it talks to first")
    actions.add_parser("list", help="list the snapshots")
    actions.add_parser("delete", help="delete a snapshot").add_argument("name")

    update = command("update", cmd_update, "stage, finalize, and commit an A/B update, checking each (A1-A4)")
    update.add_argument("--to", default="other", metavar="TARGET", help=f"{TARGET_HELP} (default other)")
    update.add_argument("--no-commit", action="store_true", help="stop after the new slot boots")
    update.add_argument("--failing-health-check", action="store_true",
                        help="add a health check that fails, and check that commit rolls back (H1)")
    update.add_argument("--single-step", action="store_true", help="stage and finalize in one trident update")
    stage = command("stage", cmd_stage, "write an image to the inactive slot without rebooting (A1)")
    stage.add_argument("--to", default="other", metavar="TARGET", help=f"{TARGET_HELP} (default other)")
    stage.add_argument("--failing-health-check", action="store_true", help="add a health check that fails")
    stage.add_argument("--single-step", action="store_true",
                       help="only copy the COSI and write the config; finalize then stages and finalizes")
    command("finalize", cmd_finalize, "finalize the staged update, which reboots into the new slot (A2, A3)")
    command("commit", cmd_commit, "commit the update the VM booted (A4)")
    rollback = command("rollback", cmd_rollback, "roll the last A/B update back with trident rollback --ab (R1)")
    rollback.add_argument("--check", action="store_true", help="only report what trident rollback --check says")
    reboot = command("reboot", cmd_reboot, "reboot the VM, and report the slot it boots")
    reboot.add_argument("--hard", action="store_true", help="reset the VM rather than reboot it")
    verify = command("verify", cmd_verify, "check the VM's state without changing it (V1)")
    verify.add_argument("--expect", default="", metavar="TARGET", help=f"the image it should run: {TARGET_HELP}")
    verify.add_argument("--expect-slot", choices=("a", "b"), help="the volume Trident should report active")
    verify.add_argument("--expect-state", default="", metavar="STATE", help="the servicingState it should report")

    command("scenarios", cmd_scenarios, "list the scenarios")
    run_parser = command("run", cmd_run, "run scenarios, in the order given, on the running VM or from a snapshot")
    run_parser.add_argument("scenarios", nargs="+", metavar="SCENARIO", choices=[*REGISTRY, "all"],
                            help=f"{', '.join(REGISTRY)}, or all")
    run_parser.add_argument("--from", dest="from_snapshot", default="", metavar="SNAPSHOT",
                            help="restore this snapshot before each scenario")
    run_parser.add_argument("--rounds", type=positive, default=2, help="pingpong's updates (default 2)")
    run_parser.add_argument("--to", default="", metavar="TARGET", help=f"the scenarios' target: {TARGET_HELP}")
    run_parser.add_argument("--keep-going", action="store_true", help="run the rest after a scenario fails")
    run_parser.add_argument("--no-setup", action="store_true", help="fail a scenario the VM isn't set up for")
    run_parser.add_argument("--single-step", action="store_true", help="stage and finalize in one trident update")
    test = command("test", cmd_test, "run every scenario, each from snapshot base")
    test.add_argument("--rounds", type=positive, default=2, help="pingpong's updates (default 2)")
    test.add_argument("--stop-on-failure", action="store_true", help="stop at the first scenario that fails")
    test.add_argument("--single-step", action="store_true", help="stage and finalize in one trident update")
    return parser


def main() -> None:
    global _json_output
    args = build_parser().parse_args()
    _json_output = args.json
    try:
        args.func(args)
    except StepError as error:
        die(str(error), 1)
    except KeyboardInterrupt:
        die("interrupted", 130)


if __name__ == "__main__":
    main()
