# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""A local kind cluster for ACL VMs to join as Unbounded nodes, on a bridge the harness owns.

Each VM keeps its QEMU user-mode NIC for SSH, the internet, and the host's services, and gets a
second NIC on a TAP device in the harness's bridge, one TAP per VM. The kind node container joins
that bridge too, by a veth, at subnet.2: the VMs reach the API server there, and the API server
reaches their kubelets across it. Nothing on the bridge has to leave it, so there's no NAT and no
forwarding sysctl, and one firewall rule: kind loads br_netfilter, so frames bridged between the
TAPs and the veth traverse FORWARD, whose policy is DROP on a host running docker.

kind runs on rootful Podman, since attaching its node to the bridge needs root anyway. Every host
command goes through an injected run, root through `sudo -n` in the command, so the module can be
tested with a fake run (test_cluster.py). The kind node advertises its bridge address, and kindnet
and kube-proxy reach the API server there rather than at the node's Podman hostname, which the VMs
can't resolve.
"""

from __future__ import annotations

import dataclasses
import json
import os
import re
import secrets
import shlex
import shutil
import subprocess
import textwrap
import time
from collections.abc import Callable
from pathlib import Path
from typing import Any, Protocol

from errors import StepError

# The node lifecycle controller owns these taints, so reset_to leaves them as they are.
SYSTEM_TAINT_PREFIX = "node.kubernetes.io/"
INOTIFY_INSTANCES = Path("/proc/sys/fs/inotify/max_user_instances")
INOTIFY_MINIMUM = 512

_INTERFACE = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,14}")
_CLUSTER_NAME = re.compile(r"[a-z0-9]([a-z0-9-]{0,47}[a-z0-9])?")
_USER = re.compile(r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,31}")
_BOOTSTRAP_TOKEN = re.compile(r"\b[a-z0-9]{6}\.[a-z0-9]{16}\b")
_PRIVATE_KEY = re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----", re.S)


class Runner(Protocol):
    def __call__(self, cmd: list[str], *, input: str | None = None, timeout: float | None = None,
                 stream: bool = False) -> subprocess.CompletedProcess[str]:
        """Run a host command, with its output as text, or shown as it runs, and none kept, if stream."""


# ---------------------------------------------------------------------------------------------
# The cluster's settings


@dataclasses.dataclass(frozen=True)
class ClusterSpec:
    """Names and addresses of the cluster and its network. They default to the harness's own, apart
    from the other harnesses in hack, so they can run alongside it."""

    name: str = "acl-e2e"
    subnet: str = "192.168.114"
    bridge: str = "virbr-acle2e"
    # Each VM's TAP is this with its number, from 1.
    tap_prefix: str = "tap-acle2e"
    veth_host: str = "veth-acle2e"
    veth_kind: str = "eth-acle2e"
    node_image: str = ""
    user: str = ""
    vms: int = 2

    @property
    def gateway(self) -> str:
        return f"{self.subnet}.1"

    @property
    def kind_ip(self) -> str:
        return f"{self.subnet}.2"

    @property
    def taps(self) -> list[str]:
        return [f"{self.tap_prefix}{number}" for number in range(1, self.vms + 1)]

    def vm_ip(self, number: int) -> str:
        """The address of VM number, from 1, on the bridge."""
        return f"{self.subnet}.{9 + number}"

    @property
    def api_server(self) -> str:
        return f"{self.kind_ip}:6443"

    @property
    def node_container(self) -> str:
        return f"{self.name}-control-plane"

    def to_dict(self) -> dict[str, str]:
        return dataclasses.asdict(self)

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> ClusterSpec:
        known = {field.name for field in dataclasses.fields(cls)}
        spec = cls(**{key: value for key, value in data.items() if key in known})
        spec.validate()
        return spec

    def validate(self) -> None:
        for field in dataclasses.fields(self):
            kind = int if field.name == "vms" else str
            if not isinstance(getattr(self, field.name), kind) or isinstance(getattr(self, field.name), bool):
                raise StepError(f"the cluster's {field.name} must be a {kind.__name__}, not {getattr(self, field.name)!r}")
        if not 1 <= self.vms <= 9:
            raise StepError(f"the cluster takes 1 to 9 VMs, not {self.vms}")
        if not _CLUSTER_NAME.fullmatch(self.name):
            raise StepError(f"{self.name!r} isn't a kind cluster name: use 1 to 49 lowercase letters, digits, "
                            "and '-', starting and ending with a letter or digit")
        octets = self.subnet.split(".")
        if len(octets) != 3 or not all(o.isdigit() and str(int(o)) == o and int(o) <= 255 for o in octets):
            raise StepError(f"{self.subnet!r} isn't the first three octets of a /24, such as 192.168.113")
        interfaces = {"bridge": self.bridge, **{f"tap {number}": tap for number, tap in enumerate(self.taps, 1)},
                      "veth_host": self.veth_host, "veth_kind": self.veth_kind}
        for what, value in interfaces.items():
            # The kernel's names are at most 15 characters (IFNAMSIZ).
            if not _INTERFACE.fullmatch(value):
                raise StepError(f"the {what} name {value!r} isn't an interface name: use 1 to 15 letters, "
                                "digits, '_', '.', and '-'")
        if len(set(interfaces.values())) != len(interfaces):
            raise StepError(f"the interface names must differ: {', '.join(interfaces.values())}")
        if self.node_image and not re.fullmatch(r"[^\s\"'\\]+", self.node_image):
            raise StepError(f"{self.node_image!r} isn't an image reference")
        if self.user and not _USER.fullmatch(self.user):
            raise StepError(f"{self.user!r} isn't a user name or ID")


# ---------------------------------------------------------------------------------------------
# Host network


def forward_rule(spec: ClusterSpec) -> list[str]:
    """The FORWARD rule's match and target, for the frames bridged between the VM and kind."""
    return ["-i", spec.bridge, "-o", spec.bridge, "-j", "ACCEPT"]


def _sudo(*cmd: str) -> list[str]:
    return ["sudo", "-n", *cmd]


def _tap_add(spec: ClusterSpec, tap: str) -> list[str]:
    if not spec.user:
        raise StepError("the TAP device needs an owner, for QEMU to open it as that user: set the cluster's user")
    return _sudo("ip", "tuntap", "add", "dev", tap, "mode", "tap", "user", spec.user)


def network_up_commands(spec: ClusterSpec) -> list[list[str]]:
    """The bridge, with the host's address on it, each VM's TAP device in it, owned by the user
    QEMU runs as, and the FORWARD rule for the bridge."""
    commands = [
        _sudo("ip", "link", "add", spec.bridge, "type", "bridge"),
        _sudo("ip", "addr", "add", f"{spec.gateway}/24", "dev", spec.bridge),
        _sudo("ip", "link", "set", spec.bridge, "up"),
    ]
    for tap in spec.taps:
        commands += [_tap_add(spec, tap), _sudo("ip", "link", "set", tap, "master", spec.bridge),
                     _sudo("ip", "link", "set", tap, "up")]
    return [*commands, _sudo("iptables", "-I", "FORWARD", *forward_rule(spec))]


def network_down_commands(spec: ClusterSpec) -> list[list[str]]:
    """Undo network_up_commands, in reverse, and remove the kind node's veth, which attaching it
    made. Each may fail, as what it removes may be gone; the rule's is repeated until it fails."""
    return [
        _sudo("iptables", "-D", "FORWARD", *forward_rule(spec)),
        _sudo("ip", "link", "delete", spec.veth_host),
        *(_sudo("ip", "link", "delete", tap) for tap in spec.taps),
        _sudo("ip", "link", "delete", spec.bridge),
    ]


def _is_rule_delete(cmd: list[str]) -> bool:
    return "iptables" in cmd and "-D" in cmd


def links_by_name(links: list[dict[str, Any]]) -> dict[str, dict[str, Any]]:
    """`ip -j addr show`'s links by name."""
    return {link.get("ifname", ""): link for link in links if isinstance(link, dict)}


def link_addresses(link: dict[str, Any]) -> list[str]:
    return [f"{a.get('local')}/{a.get('prefixlen')}" for a in link.get("addr_info", []) or []
            if a.get("family") == "inet"]


def _link_up(link: dict[str, Any] | None) -> bool:
    # A TAP no QEMU holds is NO-CARRIER and operstate DOWN while set up, so the flag is what counts.
    return link is not None and "UP" in (link.get("flags") or [])


def network_repair_commands(spec: ClusterSpec, links: list[dict[str, Any]], rule_present: bool) -> list[list[str]]:
    """What network_up_commands would make that's missing, from `ip -j addr show`'s links: a host
    restart drops the bridge, the TAPs, and the rule, and deleting the bridge alone detaches the
    TAPs. An existing TAP is kept, since a running QEMU may hold it."""
    by_name = links_by_name(links)
    bridge = by_name.get(spec.bridge)
    commands = []
    if bridge is None:
        commands.append(_sudo("ip", "link", "add", spec.bridge, "type", "bridge"))
    if bridge is None or f"{spec.gateway}/24" not in link_addresses(bridge):
        commands.append(_sudo("ip", "addr", "add", f"{spec.gateway}/24", "dev", spec.bridge))
    if not _link_up(bridge):
        commands.append(_sudo("ip", "link", "set", spec.bridge, "up"))
    for name in spec.taps:
        tap = by_name.get(name)
        if tap is None:
            commands.append(_tap_add(spec, name))
        if tap is None or tap.get("master") != spec.bridge:
            commands.append(_sudo("ip", "link", "set", name, "master", spec.bridge))
        if not _link_up(tap):
            commands.append(_sudo("ip", "link", "set", name, "up"))
    if not rule_present:
        commands.append(_sudo("iptables", "-I", "FORWARD", *forward_rule(spec)))
    return commands


def forward_rule_count(spec: ClusterSpec, listing: str) -> int:
    """How many times `iptables -S FORWARD` lists the bridge's rule."""
    wanted = ["-A", "FORWARD", *forward_rule(spec)]
    return sum(1 for line in listing.splitlines() if line.split() == wanted)


def attach_commands(spec: ClusterSpec, pid: str) -> list[list[str]]:
    """Give the kind node, whose container's PID is pid, an address on the bridge, by a veth."""
    return [
        _sudo("ip", "link", "add", spec.veth_host, "type", "veth", "peer", "name", spec.veth_kind),
        _sudo("ip", "link", "set", spec.veth_host, "master", spec.bridge),
        _sudo("ip", "link", "set", spec.veth_host, "up"),
        _sudo("ip", "link", "set", spec.veth_kind, "netns", pid),
        _sudo("nsenter", "-t", pid, "-n", "ip", "addr", "add", f"{spec.kind_ip}/24", "dev", spec.veth_kind),
        _sudo("nsenter", "-t", pid, "-n", "ip", "link", "set", spec.veth_kind, "up"),
    ]


def inotify_warning(value: str) -> str | None:
    """A warning when the host's inotify instances are too few for kind's pods."""
    try:
        instances = int(value.strip())
    except ValueError:
        return None
    if instances >= INOTIFY_MINIMUM:
        return None
    return (f"fs.inotify.max_user_instances is {instances}, and kind needs {INOTIFY_MINIMUM} or more, "
            "or kube-proxy and the controller crash-loop with 'too many open files'; raise it with "
            f"`sudo sysctl fs.inotify.max_user_instances={INOTIFY_MINIMUM}`")


# ---------------------------------------------------------------------------------------------
# kind and the cluster's configuration


def kind_config(spec: ClusterSpec) -> str:
    """kind's cluster config. The API server's certificate also names the bridge address, where
    the VM reaches it; patches replace lists, so it keeps kind's own names too."""
    node = "  - role: control-plane\n"
    if spec.node_image:
        node += f"    image: {json.dumps(spec.node_image)}\n"
    sans = ", ".join(json.dumps(name) for name in ("localhost", "127.0.0.1", spec.kind_ip))
    return ("kind: Cluster\n"
            "apiVersion: kind.x-k8s.io/v1alpha4\n"
            "nodes:\n" + node +
            "kubeadmConfigPatches:\n"
            "  - |\n"
            "    kind: ClusterConfiguration\n"
            "    apiServer:\n"
            f"      certSANs: [{sans}]\n")


# Rewrites the kubelet's flags so it runs with --node-ip on the bridge, and restarts it if they
# changed, or if it isn't running, as after a host restart, when it fails until the veth is back.
ADVERTISE_SCRIPT = textwrap.dedent("""\
    set -eu
    flags=/var/lib/kubelet/kubeadm-flags.env
    . "$flags"
    set -- $KUBELET_KUBEADM_ARGS
    args=""
    for arg do
      case "$arg" in
        --node-ip=*) ;;
        *) args="$args $arg" ;;
      esac
    done
    want="KUBELET_KUBEADM_ARGS=\\"${args# } --node-ip=@NODE_IP@\\""
    if [ "$(cat "$flags")" != "$want" ]; then
      printf '%s\\n' "$want" >"$flags"
      systemctl restart kubelet
    elif ! systemctl is-active --quiet kubelet; then
      systemctl restart kubelet
    fi
    """)


def advertise_script(spec: ClusterSpec) -> str:
    return ADVERTISE_SCRIPT.replace("@NODE_IP@", spec.kind_ip)


def kube_proxy_kubeconfig(text: str, api_server: str) -> str:
    """kube-proxy's kubeconfig, with its server at the bridge address."""
    lines = []
    for line in text.splitlines():
        if line.strip().startswith("server:"):
            line = f"{line[:len(line) - len(line.lstrip())]}server: https://{api_server}"
        lines.append(line)
    return "\n".join(lines) + "\n"


def kindnet_patch(daemonset: dict[str, Any], api_server: str) -> dict[str, Any] | None:
    """A strategic merge patch that points kindnet's CONTROL_PLANE_ENDPOINT at the bridge address,
    or None if it does already."""
    containers = daemonset.get("spec", {}).get("template", {}).get("spec", {}).get("containers", [])
    for container in containers:
        env = {item.get("name"): item.get("value") for item in container.get("env", []) or []}
        if "CONTROL_PLANE_ENDPOINT" not in env:
            continue
        if env["CONTROL_PLANE_ENDPOINT"] == api_server:
            return None
        return {"spec": {"template": {"spec": {"containers": [
            {"name": container["name"], "env": [{"name": "CONTROL_PLANE_ENDPOINT", "value": api_server}]}]}}}}
    raise StepError("kindnet's DaemonSet has no container with CONTROL_PLANE_ENDPOINT; this kind version "
                    "configures it differently")


# ---------------------------------------------------------------------------------------------
# Nodes


def condition(obj: dict[str, Any] | None, kind: str) -> dict[str, Any]:
    for item in (obj or {}).get("status", {}).get("conditions", []) or []:
        if item.get("type") == kind:
            return item
    return {}


def describe_conditions(node: dict[str, Any] | None) -> str:
    if node is None:
        return "the Node doesn't exist"
    parts = []
    for item in node.get("status", {}).get("conditions", []) or []:
        reason = ": ".join(part for part in (item.get("reason"), item.get("message")) if part)
        parts.append(f"{item.get('type')}={item.get('status')}" + (f" ({reason})" if reason else ""))
    return "; ".join(parts) or "the Node has no conditions"


def _taint(taint: dict[str, Any]) -> dict[str, str]:
    """A taint without timeAdded, which the API server sets."""
    normal = {"key": taint.get("key", ""), "effect": taint.get("effect", "")}
    if taint.get("value"):
        normal["value"] = taint["value"]
    return normal


def _taint_key(taint: dict[str, str]) -> tuple[str, str, str]:
    return taint.get("key", ""), taint.get("value", ""), taint.get("effect", "")


def node_record(node: dict[str, Any], prefix: str) -> dict[str, Any]:
    """What reset_to restores: the node's UID, its annotations under prefix/, whether it's
    cordoned, and its taints, apart from the node lifecycle controller's."""
    metadata, spec = node.get("metadata", {}), node.get("spec", {})
    annotations = metadata.get("annotations") or {}
    taints = [_taint(t) for t in spec.get("taints") or [] if not t.get("key", "").startswith(SYSTEM_TAINT_PREFIX)]
    return {
        "node": metadata.get("name", ""),
        "uid": metadata.get("uid", ""),
        "annotations": {key: value for key, value in sorted(annotations.items()) if key.startswith(f"{prefix}/")},
        "unschedulable": bool(spec.get("unschedulable")),
        "taints": sorted(taints, key=_taint_key),
    }


def reset_patch(node: dict[str, Any], record: dict[str, Any], prefix: str) -> dict[str, Any] | None:
    """A JSON merge patch that makes the node's annotations under prefix/, unschedulable, and
    taints other than node.kubernetes.io/ ones as record has them, or None if they are. It names
    the node's resourceVersion, so it fails rather than overwrite a change made meanwhile."""
    if node.get("metadata", {}).get("uid") != record.get("uid"):
        raise StepError(f"node {node.get('metadata', {}).get('name', '')} was recreated: its UID is "
                        f"{node.get('metadata', {}).get('uid')}, not {record.get('uid')}; rejoin it")
    current = node_record(node, prefix)
    wanted_annotations = record.get("annotations") or {}
    annotations: dict[str, str | None] = {key: None for key in current["annotations"] if key not in wanted_annotations}
    annotations.update({key: value for key, value in wanted_annotations.items()
                        if current["annotations"].get(key) != value})
    spec: dict[str, Any] = {}
    if current["unschedulable"] != bool(record.get("unschedulable")):
        spec["unschedulable"] = True if record.get("unschedulable") else None
    wanted_taints = sorted((_taint(t) for t in record.get("taints") or []), key=_taint_key)
    if [_taint_key(t) for t in current["taints"]] != [_taint_key(t) for t in wanted_taints]:
        system = [t for t in node.get("spec", {}).get("taints") or []
                  if t.get("key", "").startswith(SYSTEM_TAINT_PREFIX)]
        spec["taints"] = (system + wanted_taints) or None
    if not annotations and not spec:
        return None
    patch: dict[str, Any] = {"metadata": {"resourceVersion": node.get("metadata", {}).get("resourceVersion", "")}}
    if annotations:
        patch["metadata"]["annotations"] = annotations
    if spec:
        patch["spec"] = spec
    return patch


# ---------------------------------------------------------------------------------------------
# Helpers


def last_lines(text: str | None, count: int = 12) -> str:
    return "\n".join((text or "").strip().splitlines()[-count:])


def redact(text: str) -> str:
    """Drop bootstrap tokens and private keys from text the harness writes for diagnostics."""
    return _PRIVATE_KEY.sub("<private key redacted>", _BOOTSTRAP_TOKEN.sub("<bootstrap token redacted>", text))


def write_private(path: Path, text: str) -> None:
    """Write a file only its owner can read, in place of any there, all at once."""
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    partial = path.with_name(f".{path.name}.partial")
    partial.unlink(missing_ok=True)
    with os.fdopen(os.open(partial, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "w") as out:
        out.write(text)
    os.replace(partial, path)


def _command_error(cmd: list[str], result: subprocess.CompletedProcess[str]) -> StepError:
    if result.stderr is None and result.stdout is None:
        detail = " (its output is above)"
    else:
        tail = last_lines(result.stderr) or last_lines(result.stdout)
        detail = f": {tail}" if tail else ""
    return StepError(f"{shlex.join(cmd)} exited {result.returncode}{detail}")


# ---------------------------------------------------------------------------------------------
# The cluster


class Cluster:
    """The kind cluster and its network: made by create, restored by ensure after a host restart,
    and removed by delete."""

    def __init__(self, spec: ClusterSpec, *, kubeconfig: Path, work_dir: Path,
                 run: Runner, log: Callable[[str], None]) -> None:
        spec.validate()
        self.spec = spec
        self.kubeconfig = kubeconfig
        self.work_dir = work_dir
        self.run = run
        self.log = log
        # Seams for the tests, which don't wait, and don't need kind installed.
        self.sleep: Callable[[float], None] = time.sleep
        self.monotonic: Callable[[], float] = time.monotonic
        self.which: Callable[[str], str | None] = shutil.which
        self.inotify_path = INOTIFY_INSTANCES

    @property
    def _root_kubeconfig(self) -> Path:
        """Where kind, run as root, writes and edits its kubeconfig, rather than root's own. The
        user's kubeconfig is written from `kind get kubeconfig` instead, so it's the user's."""
        return self.work_dir / "kind-root.kubeconfig"

    # Commands

    def _run(self, cmd: list[str], *, input: str | None = None, timeout: float | None = 120,
             stream: bool = False, check: bool = True) -> subprocess.CompletedProcess[str]:
        try:
            result = self.run(cmd, input=input, timeout=timeout, stream=stream)
        except subprocess.TimeoutExpired:
            result = subprocess.CompletedProcess(cmd, 124, "", f"timed out after {int(timeout or 0)}s")
        if check and result.returncode != 0:
            raise _command_error(cmd, result)
        return result

    def _sudo(self, *cmd: str, check: bool = True, timeout: float | None = 120) -> subprocess.CompletedProcess[str]:
        return self._run(_sudo(*cmd), check=check, timeout=timeout)

    def _kind_binary(self) -> str:
        # Under sudo, PATH is secure_path, which a user's kind, from go install, isn't on.
        binary = self.which("kind")
        if binary is None:
            raise StepError("kind is needed for the cluster the VM joins; install it, such as with "
                            "`go install sigs.k8s.io/kind@latest`")
        return binary

    def _kind(self, *args: str, check: bool = True, timeout: float | None = 300,
              stream: bool = False) -> subprocess.CompletedProcess[str]:
        cmd = _sudo("env", "KIND_EXPERIMENTAL_PROVIDER=podman", self._kind_binary(), *args)
        return self._run(cmd, check=check, timeout=timeout, stream=stream)

    def _kubectl(self, *args: str, input: str | None = None, check: bool = True,
                 timeout: float | None = 120) -> subprocess.CompletedProcess[str]:
        return self._run(["kubectl", "--kubeconfig", str(self.kubeconfig), *args], input=input, check=check,
                         timeout=timeout)

    def kubectl(self, *args: str, input: str | None = None, check: bool = True) -> subprocess.CompletedProcess[str]:
        return self._kubectl(*args, input=input, check=check, timeout=600)

    def _kubectl_json(self, *args: str) -> Any:
        result = self._kubectl(*args, "-o", "json")
        text = (result.stdout or "").strip()
        try:
            return json.loads(text) if text else None
        except json.JSONDecodeError as error:
            raise StepError(f"kubectl {' '.join(args)} printed no JSON: {error}") from error

    def _poll(self, timeout: float, probe: Callable[[], bool], interval: float = 5) -> bool:
        deadline = self.monotonic() + timeout
        while True:
            if probe():
                return True
            if self.monotonic() >= deadline:
                return False
            self.sleep(interval)

    def _private_dir(self, *parts: str) -> Path:
        self.work_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.work_dir.chmod(0o700)
        directory = self.work_dir.joinpath(*parts)
        directory.mkdir(mode=0o700, parents=True, exist_ok=True)
        return directory

    # Host network

    def _links(self) -> list[dict[str, Any]]:
        result = self._run(["ip", "-j", "addr", "show"])
        try:
            return json.loads(result.stdout or "[]")
        except json.JSONDecodeError as error:
            raise StepError(f"ip -j addr show printed no JSON: {error}") from error

    def _forward_rules(self) -> int:
        return forward_rule_count(self.spec, self._sudo("iptables", "-S", "FORWARD").stdout or "")

    def _network_down(self) -> list[str]:
        failures = []
        for cmd in network_down_commands(self.spec):
            if _is_rule_delete(cmd):
                # Each run deletes one copy of the rule, and an earlier run may have left several.
                for _ in range(100):
                    if self._run(cmd, check=False).returncode != 0:
                        break
                continue
            result = self._run(cmd, check=False)
            if result.returncode != 0 and "Cannot find device" not in (result.stderr or ""):
                failures.append(str(_command_error(cmd, result)))
        return failures

    def _network_up(self) -> None:
        self.log(f"creating bridge {self.spec.bridge} ({self.spec.gateway}/24) and {', '.join(self.spec.taps)}, "
                 f"owned by {self.spec.user or '(no one)'}")
        for cmd in network_up_commands(self.spec):
            self._run(cmd)

    def _ensure_network(self) -> None:
        commands = network_repair_commands(self.spec, self._links(), self._forward_rules() > 0)
        if commands:
            self.log("restoring the network: " + "; ".join(shlex.join(cmd[2:]) for cmd in commands))
        for cmd in commands:
            self._run(cmd)

    # The kind node

    def _container(self, field: str) -> str:
        result = self._sudo("podman", "inspect", "--format", f"{{{{.State.{field}}}}}", self.spec.node_container)
        return (result.stdout or "").strip()

    def _container_pid(self) -> str:
        pid = self._container("Pid")
        if not pid.isdigit() or pid == "0":
            raise StepError(f"{self.spec.node_container} isn't running (its PID is {pid or 'unknown'})")
        return pid

    def _veth_attached(self, pid: str) -> bool:
        host = links_by_name(self._links()).get(self.spec.veth_host)
        if host is None or host.get("master") != self.spec.bridge or not _link_up(host):
            return False
        inside = self._sudo("nsenter", "-t", pid, "-n", "ip", "-j", "addr", "show", "dev", self.spec.veth_kind,
                            check=False)
        if inside.returncode != 0:
            return False
        try:
            links = json.loads(inside.stdout or "[]")
        except json.JSONDecodeError:
            return False
        return any(f"{self.spec.kind_ip}/24" in link_addresses(link) and _link_up(link) for link in links)

    def _attach_to_bridge(self) -> None:
        """Give the kind node its address on the bridge, unless it has it, where the VM reaches the
        API server and the API server reaches the VM's kubelet."""
        pid = self._container_pid()
        if self._veth_attached(pid):
            return
        self.log(f"attaching {self.spec.node_container} to {self.spec.bridge} as {self.spec.kind_ip}")
        self._sudo("ip", "link", "delete", self.spec.veth_host, check=False)
        self._sudo("nsenter", "-t", pid, "-n", "ip", "link", "delete", self.spec.veth_kind, check=False)
        for cmd in attach_commands(self.spec, pid):
            self._run(cmd)

    def _advertise_bridge_address(self, timeout: float = 180) -> None:
        """Make the kind node's kubelet advertise its bridge address. kindnet on the VM routes the
        kind node's pod CIDR through it, and the VM can't reach the node's Podman address."""
        cmd = _sudo("podman", "exec", self.spec.node_container, "sh", "-c", advertise_script(self.spec))
        attempts: list[subprocess.CompletedProcess[str]] = []

        def advertised_flags() -> bool:
            # Right after a start, the node's systemd may not take a restart yet.
            attempts.append(self._run(cmd, check=False))
            return attempts[-1].returncode == 0

        if not self._poll(60, advertised_flags, 3):
            raise _command_error(cmd, attempts[-1])

        def advertised() -> bool:
            result = self._kubectl("get", "node", self.spec.node_container, "-o",
                                   "jsonpath={.status.addresses[?(@.type=='InternalIP')].address}", check=False,
                                   timeout=30)
            return result.returncode == 0 and (result.stdout or "").split() == [self.spec.kind_ip]

        if not self._poll(timeout, advertised, 3):
            raise StepError(f"{self.spec.node_container} didn't advertise {self.spec.kind_ip} within {int(timeout)}s")

    def _point_cluster_at_bridge(self) -> None:
        """kindnet and kube-proxy on the VM would otherwise reach the API server at the kind node's
        container hostname, which the VM can't resolve."""
        daemonset = self._kubectl_json("-n", "kube-system", "get", "daemonset", "kindnet")
        patch = kindnet_patch(daemonset or {}, self.spec.api_server)
        if patch is not None:
            self._kubectl("-n", "kube-system", "patch", "daemonset", "kindnet", "--type=strategic",
                          "-p", json.dumps(patch))
            self._kubectl("-n", "kube-system", "delete", "pod", "-l", "app=kindnet", "--wait=false")
        configmap = self._kubectl_json("-n", "kube-system", "get", "configmap", "kube-proxy") or {}
        current = configmap.get("data", {}).get("kubeconfig.conf", "")
        wanted = kube_proxy_kubeconfig(current, self.spec.api_server)
        if wanted != current:
            self._kubectl("-n", "kube-system", "patch", "configmap", "kube-proxy", "--type=merge",
                          "-p", json.dumps({"data": {"kubeconfig.conf": wanted}}))
            self._kubectl("-n", "kube-system", "rollout", "restart", "daemonset/kube-proxy")
        for daemonset_name in ("kindnet", "kube-proxy"):
            self._kubectl("-n", "kube-system", "rollout", "status", f"daemonset/{daemonset_name}",
                          "--timeout=180s", timeout=240)

    def _write_kubeconfig(self) -> None:
        """The user's kubeconfig, private, from kind's: its server is the API server's port on the
        host's loopback, which needs no bridge."""
        text = self._kind("get", "kubeconfig", "--name", self.spec.name).stdout or ""
        if "server:" not in text:
            raise StepError(f"kind get kubeconfig printed no kubeconfig for {self.spec.name}")
        current = self.kubeconfig.read_text() if self.kubeconfig.is_file() else None
        if current != text:
            write_private(self.kubeconfig, text)

    def _wait_for_api_server(self, timeout: float) -> None:
        def ready() -> bool:
            return self._kubectl("get", "--raw", "/readyz", check=False, timeout=30).returncode == 0

        if not self._poll(timeout, ready, 3):
            raise StepError(f"the API server of {self.spec.name} wasn't ready within {int(timeout)}s")

    def _warn_about_inotify(self) -> None:
        try:
            warning = inotify_warning(self.inotify_path.read_text())
        except OSError:
            return
        if warning:
            self.log(f"warning: {warning}")

    # Lifecycle

    def exists(self) -> bool:
        return self.spec.name in (self._kind("get", "clusters").stdout or "").split()

    def create(self) -> None:
        """Create the network and the cluster, replacing any left from an earlier run, and write
        the user's kubeconfig."""
        self._warn_about_inotify()
        if self.exists():
            self.log(f"deleting kind cluster {self.spec.name}, left from an earlier run")
            self._kind("delete", "cluster", "--name", self.spec.name, "--kubeconfig", str(self._root_kubeconfig))
        self._network_down()
        self._network_up()
        config = self._private_dir() / "kind.yaml"
        config.write_text(kind_config(self.spec))
        self.log(f"creating kind cluster {self.spec.name} on rootful Podman"
                 + (f" with {self.spec.node_image}" if self.spec.node_image else ""))
        self._kind("create", "cluster", "--name", self.spec.name, "--config", str(config),
                   "--kubeconfig", str(self._root_kubeconfig), "--wait", "180s", timeout=900, stream=True)
        self._write_kubeconfig()
        self._sudo("rm", "-f", str(self._root_kubeconfig), f"{self._root_kubeconfig}.lock")
        self._attach_to_bridge()
        self._advertise_bridge_address()
        self._point_cluster_at_bridge()
        self.log(f"kind cluster {self.spec.name} is up; the VMs reach its API server at {self.spec.api_server}")

    def ensure(self) -> None:
        """Bring the cluster back after a host restart, or check it's whole: start the kind node,
        restore the bridge, the TAP, and the rule, reattach the node, and have it advertise its
        bridge address again. Each part is left alone if it's as it should be."""
        if not self.exists():
            raise StepError(f"there's no kind cluster {self.spec.name}; create it")
        self._ensure_network()
        if self._container("Running") != "true":
            self.log(f"starting {self.spec.node_container}")
            self._sudo("podman", "start", self.spec.node_container)
        self._write_kubeconfig()
        self._attach_to_bridge()
        self._advertise_bridge_address()
        self._wait_for_api_server(180)
        self.wait_ready(self.spec.node_container, 300)
        self.log(f"kind cluster {self.spec.name} is up at {self.spec.api_server}")

    def delete(self) -> list[str]:
        """Remove the cluster, then the network, as far as possible, and return what failed or is
        left. It never raises, so down can go on."""
        failures: list[str] = []

        def attempt(what: str, action: Callable[[], None]) -> None:
            try:
                action()
            except Exception as error:  # best effort: report it, and go on
                failures.append(f"{what}: {error}")

        def delete_cluster() -> None:
            result = self._kind("delete", "cluster", "--name", self.spec.name,
                                "--kubeconfig", str(self._root_kubeconfig), check=False)
            self._sudo("rm", "-f", str(self._root_kubeconfig), f"{self._root_kubeconfig}.lock", check=False)
            if result.returncode != 0:
                raise _command_error(result.args, result)
            if self.exists():
                raise StepError(f"kind cluster {self.spec.name} is still there")

        def delete_network() -> None:
            failures.extend(self._network_down())

        def network_gone() -> None:
            names = links_by_name(self._links())
            left = [name for name in (self.spec.bridge, *self.spec.taps, self.spec.veth_host) if name in names]
            if left:
                failures.append(f"these interfaces are still there: {', '.join(left)}")
            if self._forward_rules():
                failures.append(f"the FORWARD rule for {self.spec.bridge} is still there")

        self.log(f"deleting kind cluster {self.spec.name} and its network")
        attempt("deleting the kind cluster", delete_cluster)
        attempt("deleting the network", delete_network)
        attempt("checking the network is gone", network_gone)
        attempt("removing the kubeconfig", lambda: self.kubeconfig.unlink(missing_ok=True))
        return failures

    # Images

    def load_image(self, image: str) -> None:
        """Load an image from the user's Podman into the kind node, so pods can run it with
        imagePullPolicy IfNotPresent or Never."""
        archive = self.work_dir / f"image-{secrets.token_hex(4)}.tar"
        try:
            self._run(["podman", "save", "-q", "-o", str(archive), image], timeout=600)
            self.log(f"loading {image} into kind cluster {self.spec.name}")
            self._kind("load", "image-archive", str(archive), "--name", self.spec.name, timeout=600)
        finally:
            archive.unlink(missing_ok=True)

    # Nodes

    def node(self, name: str) -> dict[str, Any] | None:
        return self._kubectl_json("get", "node", name, "--ignore-not-found")

    def node_ready(self, name: str) -> bool:
        try:
            node = self.node(name)
        except StepError:
            return False
        return condition(node, "Ready").get("status") == "True"

    def wait_ready(self, name: str, timeout: float) -> None:
        self.log(f"waiting for node {name} to be Ready")
        if self._poll(timeout, lambda: self.node_ready(name), 5):
            self.log(f"node {name} is Ready")
            return
        try:
            detail = describe_conditions(self.node(name))
        except StepError as error:
            detail = str(error)
        raise StepError(f"node {name} wasn't Ready within {int(timeout)}s: {detail}")

    def record(self, name: str, prefix: str) -> dict[str, Any]:
        """What reset_to returns the node to."""
        node = self.node(name)
        if node is None:
            raise StepError(f"there's no node {name} to record")
        return node_record(node, prefix)

    def reset_to(self, name: str, record: dict[str, Any], prefix: str) -> None:
        """Return the node's annotations under prefix, its cordon, and its taints to record."""
        for attempt in range(5):
            node = self.node(name)
            if node is None:
                raise StepError(f"node {name} is gone; rejoin it")
            patch = reset_patch(node, record, prefix)
            if patch is None:
                break
            result = self._kubectl("patch", "node", name, "--type=merge", "-p", json.dumps(patch), check=False)
            if result.returncode == 0:
                self.log(f"reset node {name}'s {prefix}/ annotations, cordon, and taints")
                break
            # A Node changes under the kubelet and the node controller; a conflict is read again.
            if "the object has been modified" not in (result.stderr or "") or attempt == 4:
                raise _command_error(result.args, result)

    # Checks

    def diagnostics(self, directory: Path, extra: dict[str, list[str]] | None = None) -> None:
        """Write what tells why the nodes or the cluster misbehaved to directory, as far as it can
        be read, with extra's kubectl arguments by file name. Bootstrap tokens and private keys are
        redacted; Secrets aren't read."""
        collect = {
            "nodes.txt": ["get", "nodes", "-o", "wide"],
            "nodes.yaml": ["get", "nodes", "-o", "yaml"],
            "nodes-describe.txt": ["describe", "nodes"],
            "leases.yaml": ["-n", "kube-node-lease", "get", "leases", "-o", "yaml"],
            "events.txt": ["get", "events", "--all-namespaces", "--sort-by=.lastTimestamp"],
            "pods.txt": ["get", "pods", "--all-namespaces", "-o", "wide"],
            "machines.yaml": ["get", "machines", "-o", "yaml"],
            "machina.log": ["-n", "unbounded-system", "logs", "deployment/machina-controller", "--all-containers",
                            "--tail=1000"],
            **(extra or {}),
        }
        try:
            directory.mkdir(parents=True, exist_ok=True)
        except OSError as error:
            self.log(f"warning: can't write the cluster's diagnostics to {directory}: {error}")
            return
        for filename, args in collect.items():
            try:
                result = self._kubectl(*args, check=False, timeout=60)
                text = result.stdout or ""
                if filename == "events.txt":
                    lines = text.splitlines()
                    text = "\n".join(lines[:1] + lines[1:][-200:]) + "\n"
                if result.returncode != 0:
                    text += f"\n# kubectl exited {result.returncode}\n{result.stderr or ''}"
                (directory / filename).write_text(redact(text))
            except Exception as error:  # best effort: one file's failure doesn't stop the others
                self.log(f"warning: couldn't write {directory / filename}: {error}")
