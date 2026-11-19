#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""centerpivot's end-to-end test, with nothing faked: two Azure Container Linux VMs join a kind
cluster as Unbounded nodes, through Unbounded's host agent, and centerpivot updates them from a
real Nebraska, through trident-acl-agent and Trident.

    python3 hack/acl-abupdate/centerpivot_e2e.py test

`up` builds and starts everything, joins the VMs, and snapshots them; `run` runs scenarios, each
from that snapshot, with a Nebraska app of its own; `down` removes it all. README.md explains
the setup and each scenario.
"""

from __future__ import annotations

import argparse
import base64
import concurrent.futures
import contextlib
import dataclasses
import datetime
import getpass
import hashlib
import json
import os
import re
import secrets
import shlex
import shutil
import subprocess
import sys
import threading
import time
from collections.abc import Callable
from pathlib import Path
from typing import Any

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import cluster  # noqa: E402
import nebraska  # noqa: E402
import serve  # noqa: E402
from errors import StepError  # noqa: E402

REPO = HERE.parents[1]
STATE = Path(os.environ.get("CP_E2E_STATE_DIR", str(REPO / ".vm" / "centerpivot-e2e")))
STATE_FILE = STATE / "e2e.json"
KUBECONFIG = STATE / "kubeconfig"
BUILD = STATE / "build"
TLS_DIR = STATE / "tls"
RUNS = STATE / "runs"
ABUPDATE = HERE / "abupdate.py"


def _env_int(name: str, default: int) -> int:
    value = os.environ.get(name, "")
    return int(value) if value.isdigit() else default


VM_MEMORY = _env_int("CP_E2E_VM_MEMORY", 4096)
SSH_PORT = _env_int("CP_E2E_SSH_PORT", 2240)
FRONT_PORT = _env_int("CP_E2E_FRONT_PORT", 8443)
NEBRASKA_PORT = _env_int("CP_E2E_NEBRASKA_PORT", 18000)
KIND_NODE_IMAGE = os.environ.get("CP_E2E_KIND_NODE_IMAGE", "")

SITE = "e2e"
NAMESPACE = "unbounded-system"
WORKLOAD_NAMESPACE = "centerpivot-e2e"
WORKLOAD = "e2e-workload"
WORKLOAD_IMAGE = "mcr.microsoft.com/azurelinux/busybox:1.36"
CA_CONFIGMAP = "centerpivot-e2e-ca"
TRACK = "e2e"
APP_NAME = "acl"
CENTERPIVOT_IMAGE = "localhost/centerpivot:e2e"
MACHINA_IMAGE = "localhost/machina:e2e"
MACHINA_BASE = "docker.io/library/ubuntu:noble"
BOOTSTRAP_GROUP = "system:bootstrappers:unbounded-agent-daemons"
# The agent binary is copied onto each VM, so the URL its Ignition config names is never fetched.
AGENT_URL = "https://unbounded.invalid/unbounded-agent"
SNAPSHOT = "joined"

PREFIX = "centerpivot.unbounded-cloud.io"
MANAGED_LABEL = f"{PREFIX}/managed"
CANARY_LABEL = f"{PREFIX}/canary"
PAUSED_LABEL = f"{PREFIX}/paused"
UPDATING_TAINT = f"{PREFIX}/updating"
AGENT_INFO = f"{PREFIX}/agent-info"
STATE_ANNOTATION = f"{PREFIX}/state"
REQUEST_ANNOTATION = f"{PREFIX}/update-request"
TRIDENT_AGENT = "trident-acl-agent.service"
HOST_AGENT_DROP_IN = f"/etc/systemd/system/{TRIDENT_AGENT}.d/50-unbounded-centerpivot.conf"
UPDATING_PHASES = frozenset({"Checking", "Staging", "Draining", "Finalizing", "Restarting", "Recovering"})
# An update's phases, each with the one that follows it.
UPDATE_PATH = (("Checking", "Staging"), ("Staging", "Draining"), ("Draining", "Finalizing"),
               ("Finalizing", "Restarting"), ("Restarting", "Recovering"), ("Recovering", "Idle"))

# The Site's centerPivot timings: short enough for a test, and within its CRD's budget.
SETTINGS: dict[str, Any] = {
    "maxUnavailable": 1,
    "checkInterval": "2m",
    "updateTimeout": "60m",
    "statusDeadline": "2m",
    "drainDeadline": "4m",
    "restartDeadline": "8m",
    "recoveryDeadline": "5m",
    "settleTime": "30s",
}
# Nebraska's group timeout must be at least the Site's updateTimeout.
GROUP_UPDATE_TIMEOUT_MINUTES = 120
# How long one node's update may take here, from its check to its slot given back.
UPDATE_SECONDS = 40 * 60
SAMPLE_SECONDS = 5


# ---------------------------------------------------------------------------------------------
# Output and commands

_log_lock = threading.Lock()


def log(message: str) -> None:
    with _log_lock:
        print(f"[cp-e2e {time.strftime('%H:%M:%S')}] {message}", file=sys.stderr, flush=True)


def die(message: str, code: int = 2) -> None:
    log(f"error: {message}")
    sys.exit(code)


def host_run(cmd: list[str], *, input: str | None = None, timeout: float | None = None, stream: bool = False,
             env: dict[str, str] | None = None, cwd: Path | None = None) -> subprocess.CompletedProcess[str]:
    """Run a host command. stream shows its output as it runs, and captures none."""
    try:
        return subprocess.run(cmd, input=input, text=True, timeout=timeout, env=env, cwd=cwd, check=False,
                              capture_output=not stream)
    except subprocess.TimeoutExpired:
        return subprocess.CompletedProcess(cmd, 124, "", f"timed out after {int(timeout or 0)}s")


def must(cmd: list[str], **kwargs: Any) -> str:
    result = host_run(cmd, **kwargs)
    if result.returncode != 0:
        detail = (result.stderr or result.stdout or "").strip()[-1500:]
        raise StepError(f"{shlex.join(cmd[:4])} exited {result.returncode}: {detail}")
    return result.stdout or ""


def parallel(action: Callable[[Any], None], items: list[Any]) -> None:
    """Run action on each item at once, and raise the first failure once all are done."""
    with concurrent.futures.ThreadPoolExecutor(len(items)) as pool:
        errors = [future.exception() for future in [pool.submit(action, item) for item in items]]
    for error in errors:
        if error:
            raise error


def poll(timeout: float, probe: Callable[[], bool], interval: float = 5) -> bool:
    deadline = time.monotonic() + timeout
    while True:
        if probe():
            return True
        if time.monotonic() >= deadline:
            return False
        time.sleep(interval)


def stamp() -> str:
    return datetime.datetime.now().strftime("%Y%m%d-%H%M%S")


# ---------------------------------------------------------------------------------------------
# What the harness writes to the cluster


def parse_duration(text: str) -> int:
    """Seconds in a duration as the Site takes them, such as 1h5m or 30s."""
    match = re.fullmatch(r"(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?", text)
    if not text or not match:
        raise StepError(f"{text!r} isn't a duration such as 1h5m or 30s")
    hours, minutes, seconds = (int(part or 0) for part in match.groups())
    return hours * 3600 + minutes * 60 + seconds


def budget_problem(settings: dict[str, Any]) -> str:
    """Why the settings break the Site CRD's rule, or "": every update ends its Nebraska grant 10m
    before updateTimeout, so the agent's 20m stage and 10m finalize, each with statusDeadline, the
    drain, and the restart must fit in it (designs/centerpivot.md, section 4.8)."""
    status = parse_duration(settings["statusDeadline"])
    need = 40 * 60 + 2 * status + parse_duration(settings["drainDeadline"]) + parse_duration(settings["restartDeadline"])
    timeout = parse_duration(settings["updateTimeout"])
    return "" if timeout >= need else f"updateTimeout {settings['updateTimeout']} is shorter than the {need // 60}m it needs"


def allowed_sources(server: str, app_ids: list[str]) -> str:
    """The node admission policy's allowedSources: the JSON fragments that name the Nebraska URL and
    each app ID in an update request, as a CEL list of pairs. The operator component sets the same
    (cmd/centerpivot/operatorcomponent, allowedSource and sourcesExpression)."""
    pairs = [[f'"server":{json.dumps(server)}', f'"appId":{json.dumps(app_id)}'] for app_id in app_ids]
    return "[" + ", ".join(f"[{json.dumps(a)}, {json.dumps(b)}]" for a, b in pairs) + "]"


def site_manifest(spec: cluster.ClusterSpec, center_pivot: dict[str, Any]) -> dict[str, Any]:
    return {
        "apiVersion": "unbounded-cloud.io/v1alpha3",
        "kind": "Site",
        "metadata": {"name": SITE},
        "spec": {
            "nodeCidrs": [f"{spec.subnet}.0/24"],
            # kindnet gives pods their addresses; the Site only needs a range.
            "podCidrAssignments": [{"cidrBlocks": ["10.244.0.0/16"], "assignmentEnabled": False}],
            "manageCniPlugin": False,
            "components": {"centerPivot": center_pivot},
        },
    }


def center_pivot(server: str, app_id: str, settings: dict[str, Any]) -> dict[str, Any]:
    return {"enabled": True, "nebraskaURL": server, "caConfigMap": CA_CONFIGMAP,
            "apps": [{"name": APP_NAME, "appID": app_id, "defaultTrack": TRACK}], **settings}


def bootstrap_token_secret(token_id: str, token_secret: str, expires: str) -> dict[str, Any]:
    """A bootstrap token kubectl-unbounded finds by the Site's label. Its extra group lets machina
    approve the agent daemon's certificate."""
    def b64(value: str) -> str:
        return base64.b64encode(value.encode()).decode()

    return {
        "apiVersion": "v1", "kind": "Secret", "type": "bootstrap.kubernetes.io/token",
        "metadata": {"name": f"bootstrap-token-{token_id}", "namespace": "kube-system",
                     "labels": {"unbounded-cloud.io/site": SITE}},
        "data": {"token-id": b64(token_id), "token-secret": b64(token_secret), "expiration": b64(expires),
                 "usage-bootstrap-authentication": b64("true"), "usage-bootstrap-signing": b64("true"),
                 "auth-extra-groups": b64(BOOTSTRAP_GROUP)},
    }


def workload_manifests(replicas: int) -> list[dict[str, Any]]:
    """A Deployment on the managed nodes, a pod per node where it can, and a PodDisruptionBudget
    that keeps one available, which centerpivot's drains have to respect."""
    labels = {"app.kubernetes.io/name": WORKLOAD}
    return [
        {"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": WORKLOAD_NAMESPACE}},
        {"apiVersion": "apps/v1", "kind": "Deployment",
         "metadata": {"name": WORKLOAD, "namespace": WORKLOAD_NAMESPACE},
         "spec": {"replicas": replicas, "selector": {"matchLabels": labels}, "template": {
             "metadata": {"labels": labels},
             "spec": {
                 "terminationGracePeriodSeconds": 2,
                 "affinity": {
                     "nodeAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": {"nodeSelectorTerms": [
                         {"matchExpressions": [{"key": MANAGED_LABEL, "operator": "In", "values": ["true"]}]}]}},
                     "podAntiAffinity": {"preferredDuringSchedulingIgnoredDuringExecution": [{
                         "weight": 100, "podAffinityTerm": {"labelSelector": {"matchLabels": labels},
                                                            "topologyKey": "kubernetes.io/hostname"}}]},
                 },
                 "containers": [{"name": "sleep", "image": WORKLOAD_IMAGE, "command": ["sleep", "infinity"]}],
             }}}},
        {"apiVersion": "policy/v1", "kind": "PodDisruptionBudget",
         "metadata": {"name": WORKLOAD, "namespace": WORKLOAD_NAMESPACE},
         "spec": {"minAvailable": 1, "selector": {"matchLabels": labels}}},
    ]


def ignition_payload(doc: dict[str, Any], local_api_server: str, api_server: str) -> dict[str, Any]:
    """What a VM needs of the Ignition config kubectl-unbounded renders, delivered over SSH as e2e.py's
    reinstall does: the agent's config, its binary's path, and the bootstrap unit. The config's API
    server is the kubeconfig's loopback one, which the VMs reach at api_server instead."""
    files: dict[str, dict[str, Any]] = {}
    binary = ""
    for item in doc.get("storage", {}).get("files", []):
        source = item.get("contents", {}).get("source", "")
        if source.startswith("data:"):
            files[item["path"]] = {"mode": int(item.get("mode", 0o644)),
                                   "content": base64.b64decode(source.partition(",")[2]).decode()}
        elif item["path"].endswith("/unbounded-agent"):
            binary = item["path"]
    config_path = "/etc/unbounded/agent/config.json"
    if config_path not in files or not binary:
        raise StepError(f"the Ignition config has no agent config or binary: {sorted(files)}, {binary!r}")
    config = json.loads(files[config_path]["content"])
    kubelet = config["Kubelet"]
    kubelet["ApiServer"] = kubelet["ApiServer"].replace(local_api_server, api_server)
    if kubelet["ApiServer"] != api_server:
        raise StepError(f"the agent config's API server is {kubelet['ApiServer']}, not {api_server}")
    units = [unit for unit in doc.get("systemd", {}).get("units", []) if unit.get("contents")]
    bootstrap = [unit for unit in units if "unbounded-agent" in unit["name"]]
    if len(bootstrap) != 1:
        raise StepError(f"the Ignition config's units are {[unit['name'] for unit in units]}, not one bootstrap unit")
    return {"config_path": config_path, "config": json.dumps(config, indent=2),
            "config_mode": files[config_path]["mode"], "binary": binary,
            "unit": bootstrap[0]["name"], "unit_contents": bootstrap[0]["contents"]}


# ---------------------------------------------------------------------------------------------
# What the harness reads from the cluster


def state_of(machine: dict[str, Any] | None) -> dict[str, Any]:
    """A Machine's centerpivot state; a Machine without one is Idle."""
    value = ((machine or {}).get("metadata", {}).get("annotations") or {}).get(STATE_ANNOTATION)
    if not value:
        return {"phase": "Idle"}
    try:
        return json.loads(value)
    except json.JSONDecodeError:
        return {"phase": "Unreadable", "raw": value}


def node_view(node: dict[str, Any] | None, machine: dict[str, Any] | None) -> dict[str, Any]:
    """What a sample keeps of a node: its phase, and what centerpivot and the agent wrote."""
    annotations = ((node or {}).get("metadata", {}).get("annotations")) or {}
    try:
        info = json.loads(annotations.get(AGENT_INFO, "{}"))
    except json.JSONDecodeError:
        info = {}
    taints = ((node or {}).get("spec", {}).get("taints")) or []
    state = state_of(machine)
    return {
        "phase": state.get("phase", ""),
        "reason": state.get("reason", ""),
        "targetVersion": state.get("targetVersion", ""),
        "attempts": state.get("attempts", 0),
        "heldApp": state.get("heldApp", ""),
        "grant": bool(state.get("grant")),
        "endGrant": bool(state.get("endGrant")),
        "uid": (node or {}).get("metadata", {}).get("uid", ""),
        "exists": node is not None,
        "ready": cluster.condition(node, "Ready").get("status") == "True",
        "tainted": any(t.get("key") == UPDATING_TAINT for t in taints),
        "version": info.get("currentVersion", ""),
        "machineId": info.get("machineId", ""),
        "request": annotations.get(REQUEST_ANNOTATION, ""),
    }


def sample_of(names: list[str], nodes: dict[str, Any], machines: dict[str, Any],
              deployment: dict[str, Any] | None) -> dict[str, Any]:
    by_node = {item["metadata"]["name"]: item for item in nodes.get("items", [])}
    by_machine = {item["metadata"]["name"]: item for item in machines.get("items", [])}
    available = None
    if deployment is not None:
        available = int((deployment.get("status") or {}).get("availableReplicas") or 0)
    return {"time": datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds"),
            "nodes": {name: node_view(by_node.get(name), by_machine.get(name)) for name in names},
            "available": available}


def phase_counts(text: str, site: str) -> dict[tuple[str, str], float]:
    """centerpivot_phase_seconds' counts for the Site, by phase and the phase that followed, from
    Prometheus's text format."""
    counts: dict[tuple[str, str], float] = {}
    prefix = "centerpivot_phase_seconds_count{"
    for line in text.splitlines():
        if not line.startswith(prefix):
            continue
        labels_text, _, value = line[len(prefix):].rpartition("} ")
        labels = dict(re.findall(r'(\w+)="([^"]*)"', labels_text))
        if labels.get("site") == site:
            key = (labels.get("phase", ""), labels.get("next", ""))
            counts[key] = counts.get(key, 0) + float(value)
    return counts


def updated(view: dict[str, Any], target: str) -> bool:
    """Whether the node finished an update to target. Its reason doesn't say so for long: the next
    check, a check interval later, writes its own."""
    return view["phase"] == "Idle" and view["version"] == view["targetVersion"] == target


def operations_seen(samples: list[dict[str, Any]], name: str) -> set[str]:
    """The operations of the requests centerpivot wrote on the node. A request stays until the
    next, so one whose phase passed between two samples still shows."""
    found = set()
    for sample in samples:
        request = sample["nodes"][name]["request"]
        with contextlib.suppress(json.JSONDecodeError, AttributeError):
            found.add(json.loads(request).get("operation", "") if request else "")
    return found - {""}


def most_updating(samples: list[dict[str, Any]]) -> int:
    """The most nodes updating at once in any sample."""
    return max((sum(view["phase"] in UPDATING_PHASES for view in sample["nodes"].values()) for sample in samples),
               default=0)


def phases_seen(samples: list[dict[str, Any]], name: str) -> list[str]:
    """The node's phases, in order, each once per stretch."""
    seen: list[str] = []
    for sample in samples:
        phase = sample["nodes"][name]["phase"]
        if not seen or seen[-1] != phase:
            seen.append(phase)
    return seen


# ---------------------------------------------------------------------------------------------
# The VMs


@dataclasses.dataclass(frozen=True)
class VM:
    """One ACL VM, which abupdate.py runs from its own state directory."""

    number: int
    spec: cluster.ClusterSpec

    @property
    def name(self) -> str:
        """The VM's hostname, which is its Machine's and its Node's name too."""
        return f"acl-e2e-{self.number}"

    @property
    def state_dir(self) -> Path:
        return STATE / f"vm{self.number}"

    @property
    def address(self) -> str:
        return self.spec.vm_ip(self.number)

    @property
    def ssh_port(self) -> int:
        return SSH_PORT + self.number

    def abupdate(self, *args: str, timeout: float = 600, stream: bool = False,
                 check: bool = True) -> subprocess.CompletedProcess[str]:
        result = host_run([sys.executable, str(ABUPDATE), *args], timeout=timeout, stream=stream,
                          env={**os.environ, "ACL_AB_STATE_DIR": str(self.state_dir)})
        if check and result.returncode != 0:
            detail = cluster.last_lines((result.stderr or "") + (result.stdout or ""), 12)
            raise StepError(f"{self.name}: abupdate {' '.join(args)} exited {result.returncode}\n{detail}")
        return result

    def popen_abupdate(self, *args: str) -> subprocess.Popen[str]:
        return subprocess.Popen([sys.executable, str(ABUPDATE), *args], text=True, stdout=subprocess.PIPE,
                                stderr=subprocess.STDOUT, env={**os.environ, "ACL_AB_STATE_DIR": str(self.state_dir)})

    def ssh_command(self) -> list[str]:
        return ["ssh", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR",
                "-o", "ConnectTimeout=10", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes",
                "-i", str(self.state_dir / "ssh" / "id_ed25519"), "-p", str(self.ssh_port), "core@127.0.0.1"]

    def ssh(self, command: str, *, input: str | None = None, check: bool = True,
            timeout: float = 120) -> subprocess.CompletedProcess[str]:
        result = host_run([*self.ssh_command(), command], input=input, timeout=timeout)
        if check and result.returncode != 0:
            raise StepError(f"on {self.name}, {command!r} exited {result.returncode}: "
                            f"{cluster.last_lines(result.stderr, 6)}")
        return result

    def copy(self, local: Path, remote: str, *, timeout: float = 600) -> None:
        """Copy a file to the VM, as root, through ssh."""
        with local.open("rb") as source:
            result = subprocess.run([*self.ssh_command(), f"sudo install -D -m 0644 /dev/stdin {shlex.quote(remote)}"],
                                    stdin=source, capture_output=True, timeout=timeout, check=False)
        if result.returncode != 0:
            raise StepError(f"copying {local.name} to {self.name} failed: {result.stderr.decode(errors='replace')[-500:]}")

    def version(self) -> str:
        """The VERSION_ID the VM booted, which trident-acl-agent reports."""
        return self.ssh(". /etc/os-release && echo \"$VERSION_ID\"").stdout.strip()

    def trust(self, ca: str) -> None:
        """Trust the harness's CA, as trident's tests do: in the system store, through
        update-ca-trust, and hashed into /etc/pki/tls/certs, which OpenSSL reads and update-ca-trust
        doesn't fill. /etc survives A/B updates."""
        anchor = "/etc/pki/ca-trust/source/anchors/centerpivot-e2e.crt"
        self.ssh(f"sudo install -D -m 0644 /dev/stdin {anchor} && sudo update-ca-trust extract && "
                 f"sudo install -m 0644 {anchor} /etc/pki/tls/certs/centerpivot-e2e.pem && "
                 "sudo openssl rehash /etc/pki/tls/certs", input=ca, timeout=180)

    def machine(self) -> str:
        """The node's nspawn machine, kube1 or kube2."""
        names = self.ssh("sudo machinectl list --no-legend", check=False).stdout.split()
        found = [name for name in names if re.fullmatch(r"kube\d", name)]
        if not found:
            raise StepError(f"{self.name} runs no node machine")
        return found[0]

    def diagnostics(self, directory: Path) -> None:
        commands = {
            "trident-acl-agent.log": f"sudo journalctl -b -u {TRIDENT_AGENT} --no-pager -n 2000",
            "trident.log": "sudo journalctl -b -u 'trident*' --no-pager -n 2000",
            "unbounded-agent.log": "sudo journalctl -b -u 'unbounded-agent*' --no-pager -n 2000",
            "kubelet.log": "sudo /opt/unbounded/agent/bin/unbounded-agent logs --unit kubelet --lines 1000",
            "host-status.yaml": "sudo trident get status",
            "boots.txt": "journalctl --list-boots --no-pager; . /etc/os-release; echo VERSION_ID=$VERSION_ID",
            "units.txt": f"systemctl status {TRIDENT_AGENT} 'unbounded-agent*' --no-pager -n 0 --all",
        }
        directory.mkdir(parents=True, exist_ok=True)
        for filename, command in commands.items():
            result = self.ssh(command, check=False, timeout=60)
            (directory / filename).write_text((result.stdout or "") + (result.stderr or ""))
        serial = self.state_dir / "serial.log"
        if serial.is_file():
            (directory / "serial.log").write_text(cluster.last_lines(serial.read_text(errors="replace"), 400))


# ---------------------------------------------------------------------------------------------
# The environment


class E2E:
    """Everything up makes: the build, the cluster, Nebraska's pod, and the VMs, with what the
    scenarios need to know of them in STATE_FILE."""

    def __init__(self, spec: cluster.ClusterSpec, record: dict[str, Any]) -> None:
        self.spec = spec
        self.record = record
        self.cluster = cluster.Cluster(spec, kubeconfig=KUBECONFIG, work_dir=STATE / "cluster",
                                       run=host_run, log=log)
        self.vms = [VM(number, spec) for number in range(1, spec.vms + 1)]
        self.nebraska = nebraska.Nebraska(name="centerpivot-e2e-nebraska", port=NEBRASKA_PORT,
                                          work_dir=BUILD, run=host_run, log=log)

    @classmethod
    def load(cls) -> E2E:
        if not STATE_FILE.is_file():
            die(f"there's no environment in {STATE}; run `up`")
        record = json.loads(STATE_FILE.read_text())
        return cls(cluster.ClusterSpec.from_dict(record["cluster"]), record)

    def save(self) -> None:
        STATE_FILE.write_text(json.dumps(self.record, indent=2) + "\n")

    def kubectl(self, *args: str, input: str | None = None, check: bool = True) -> subprocess.CompletedProcess[str]:
        return self.cluster.kubectl(*args, input=input, check=check)

    def kubectl_json(self, *args: str) -> Any:
        return json.loads(self.kubectl(*args, "-o", "json").stdout or "null")

    def apply(self, *objects: dict[str, Any]) -> None:
        self.kubectl("apply", "-f", "-", input=json.dumps({"apiVersion": "v1", "kind": "List", "items": list(objects)}))

    @property
    def api_server(self) -> str:
        return f"https://{self.spec.api_server}"

    # Building

    def build(self) -> None:
        """The binaries, from this checkout, and the controllers' images."""
        bin_dir = BUILD / "bin"
        bin_dir.mkdir(parents=True, exist_ok=True)
        linux = {**os.environ, "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": "amd64"}
        for name, package, env in (("unbounded-agent", "./cmd/agent", linux),
                                   ("kubectl-unbounded", "./cmd/kubectl-unbounded", {**os.environ, "CGO_ENABLED": "0"}),
                                   ("machina", "./cmd/machina", linux),
                                   ("centerpivot", "./cmd/centerpivot", linux)):
            log(f"building {name}")
            must(["go", "build", "-trimpath", "-o", str(bin_dir / name), package], cwd=REPO, env=env, timeout=1800)
        for image, name, base in ((MACHINA_IMAGE, "machina", MACHINA_BASE), (CENTERPIVOT_IMAGE, "centerpivot", "scratch")):
            context = BUILD / "images" / name
            context.mkdir(parents=True, exist_ok=True)
            shutil.copy2(bin_dir / name, context / name)
            (context / "Containerfile").write_text(f"FROM {base}\nCOPY {name} /unbounded/bin/{name}\n"
                                                   f"ENTRYPOINT [\"/unbounded/bin/{name}\"]\n")
            must(["podman", "build", "-q", "-t", image, str(context)], timeout=600)
        self.render("machina", {"Namespace": NAMESPACE, "ControllerImage": MACHINA_IMAGE,
                                "APIServerEndpoint": self.api_server, "ExperimentalContainerIsolation": "false"})
        self.render("centerpivot", {"Namespace": NAMESPACE, "Image": CENTERPIVOT_IMAGE})
        self.nebraska.build()

    def render(self, component: str, values: dict[str, str]) -> None:
        output = BUILD / "manifests" / component
        shutil.rmtree(output, ignore_errors=True)
        output.mkdir(parents=True)
        must(["go", "run", "./hack/cmd/render-manifests", "--templates-dir", str(REPO / "deploy" / component),
              "--output-dir", str(output), *[arg for key, value in values.items() for arg in ("--set", f"{key}={value}")]],
             cwd=REPO, timeout=600)
        # The images are loaded into the nodes, not pulled.
        for path in output.glob("*.yaml"):
            path.write_text(path.read_text().replace("imagePullPolicy: Always", "imagePullPolicy: IfNotPresent"))

    def manifest(self, component: str, name: str) -> Path:
        return BUILD / "manifests" / component / name

    def images(self) -> dict[str, Any]:
        """The base and the update, fetched into abupdate's cache: the update is what Nebraska
        offers, as its os-release VERSION_ID."""
        vm = self.vms[0]
        vm.abupdate("image", "fetch", timeout=3600, stream=True)
        listing = json.loads(vm.abupdate("--json", "image", "list").stdout)
        tags = {"base": os.environ.get("ACL_AB_BASE_TAG", ""), "update": os.environ.get("ACL_AB_UPDATE_TAG", "")}
        found = {row["tag"]: row for row in listing["images"] if row["kind"] == "cosi"}
        if tags["update"] not in found or tags["base"] not in found:
            raise StepError(f"abupdate has no COSI for {tags}; it lists {sorted(found)}")
        update, base = found[tags["update"]], found[tags["base"]]
        if not base["versionId"] or not update["versionId"] or base["versionId"] == update["versionId"]:
            raise StepError(f"the update's VERSION_ID {update['versionId']!r} has to differ from the base's "
                            f"{base['versionId']!r}, and be newer")
        return {"tag": update["tag"], "path": update["path"], "sha384": update["metadataSha384"],
                "size": update["size"], "version": update["versionId"], "baseVersion": base["versionId"],
                "filename": f"acl-{update['tag']}.cosi"}

    # The control plane

    def install_control_plane(self) -> None:
        """Machina, the CRDs, and the bootstrap token's RBAC, as e2e.py installs them, and the Site."""
        self.kubectl("apply", "-f", str(REPO / "deploy" / "machina" / "crd"))
        self.kubectl("wait", "--for=condition=Established", "crd", "--all", "--timeout=120s")
        for name in ("01-namespace.yaml", "03-config.yaml", "02-rbac.yaml", "05-service.yaml", "04-deployment.yaml",
                     "07-bootstrapper-rbac.yaml"):
            self.kubectl("apply", "-f", str(self.manifest("machina", name)))
        self.kubectl("-n", NAMESPACE, "rollout", "status", "deployment/machina-controller", "--timeout=300s")
        expires = (datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(days=7)).strftime("%Y-%m-%dT%H:%M:%SZ")
        self.apply(bootstrap_token_secret(secrets.token_hex(3), secrets.token_hex(8), expires),
                   site_manifest(self.spec, {"enabled": False}))
        log("machina, the CRDs, the bootstrap token, and Site e2e are in the cluster")

    def install_centerpivot(self, ca: str) -> None:
        """centerpivot from its rendered manifests, as the operator would apply them, with its
        image loaded into kind and imported into each VM's containerd, for node-info."""
        archive = BUILD / "centerpivot-image.tar"
        must(["podman", "save", "-q", "-o", str(archive), CENTERPIVOT_IMAGE], timeout=600)

        def load(vm: VM) -> None:
            machine = vm.machine()
            vm.copy(archive, f"/var/lib/machines/{machine}/var/tmp/centerpivot-image.tar")
            vm.ssh("sudo /opt/unbounded/agent/bin/unbounded-agent exec -- "
                   "ctr -n k8s.io images import /var/tmp/centerpivot-image.tar", timeout=600)
            vm.ssh(f"sudo rm -f /var/lib/machines/{machine}/var/tmp/centerpivot-image.tar")

        parallel(load, self.vms)
        self.apply({"apiVersion": "v1", "kind": "ConfigMap",
                    "metadata": {"name": CA_CONFIGMAP, "namespace": NAMESPACE}, "data": {"ca.crt": ca}})
        for name in ("01-rbac.yaml", "02-configmap.yaml", "03-deployment.yaml", "04-node-info.yaml", "05-admission.yaml"):
            self.kubectl("apply", "-f", str(self.manifest("centerpivot", name)))
        self.kubectl("-n", NAMESPACE, "rollout", "status", "deployment/centerpivot", "--timeout=300s")
        self.kubectl("-n", NAMESPACE, "rollout", "status", "daemonset/centerpivot-node-info", "--timeout=300s")
        log("centerpivot is installed")

    def phase_counts(self) -> dict[tuple[str, str], float]:
        """centerpivot_phase_seconds' counts for Site e2e, from each centerpivot pod's metrics: only
        the leader's grow."""
        total: dict[tuple[str, str], float] = {}
        pods = self.kubectl_json("-n", NAMESPACE, "get", "pods", "-l", "app.kubernetes.io/name=centerpivot")
        for pod in pods["items"]:
            text = self.kubectl("get", "--raw", f"/api/v1/namespaces/{NAMESPACE}/pods/{pod['metadata']['name']}:8080"
                                "/proxy/metrics").stdout
            for key, count in phase_counts(text, SITE).items():
                total[key] = total.get(key, 0) + count
        return total

    def site(self, center_pivot_spec: dict[str, Any]) -> None:
        self.apply(site_manifest(self.spec, center_pivot_spec))

    def allow_sources(self, server: str, app_ids: list[str]) -> None:
        patch = [{"op": "replace", "path": "/spec/variables",
                  "value": [{"name": "allowedSources", "expression": allowed_sources(server, app_ids)}]}]
        self.kubectl("patch", "validatingadmissionpolicy", "centerpivot-node-restriction", "--type=json",
                     "-p", json.dumps(patch))

    # Joining

    def join(self, vm: VM) -> None:
        """Join the VM as Unbounded's ACL path does, but over SSH: the Ignition config
        kubectl-unbounded renders, with the agent's config, binary, and bootstrap unit."""
        binary = BUILD / "bin" / "unbounded-agent"
        digest = hashlib.sha256(binary.read_bytes()).hexdigest()
        env = {**os.environ, "KUBECONFIG": str(KUBECONFIG)}
        doc = json.loads(must([str(BUILD / "bin" / "kubectl-unbounded"), "machine", "manual-bootstrap", vm.name,
                               "--site", SITE, "--variant", "ignition", "--agent-url", AGENT_URL,
                               "--agent-sha256", digest, "--node-label", f"{MANAGED_LABEL}=true",
                               "--node-ip", vm.address], env=env, timeout=120))
        local = self.kubectl("config", "view", "--minify", "--raw", "-o",
                             "jsonpath={.clusters[0].cluster.server}").stdout.strip()
        payload = ignition_payload(doc, local, self.api_server)
        vm.copy(binary, payload["binary"])
        vm.ssh(f"sudo chmod 0755 {shlex.quote(payload['binary'])}")
        vm.ssh(f"sudo install -D -m {payload['config_mode']:o} /dev/stdin {shlex.quote(payload['config_path'])}",
               input=payload["config"])
        vm.ssh(f"sudo install -m 0644 /dev/stdin /etc/systemd/system/{payload['unit']} && sudo systemctl daemon-reload && "
               f"sudo systemctl enable --now --no-block {payload['unit']}", input=payload["unit_contents"])
        log(f"{vm.name}: started {payload['unit']}")

        def finished() -> bool:
            shown = vm.ssh(f"systemctl show {payload['unit']} -p ActiveState -p Result", check=False).stdout
            return "ActiveState=active" in shown or "ActiveState=failed" in shown

        done = poll(1500, finished, 10)
        shown = vm.ssh(f"systemctl show {payload['unit']} -p ActiveState -p Result", check=False).stdout
        if not done or "ActiveState=active" not in shown:
            journal = vm.ssh(f"sudo journalctl -u {payload['unit']} --no-pager -n 60", check=False).stdout
            raise StepError(f"{vm.name}: {payload['unit']} didn't finish: {shown.split()}\n{journal}")
        self.cluster.wait_ready(vm.name, 600)

    def check_joined(self, vm: VM, base_version: str) -> None:
        """What the host agent and node-info have to have done for centerpivot to update the node."""
        shown = vm.ssh(f"systemctl show {TRIDENT_AGENT} -p ActiveState -p DropInPaths", check=False).stdout
        if HOST_AGENT_DROP_IN not in shown or "ActiveState=active" not in shown:
            raise StepError(f"{vm.name}: the host agent didn't configure and start {TRIDENT_AGENT}: {shown.split()}")

        def watching() -> bool:
            journal = vm.ssh(f"sudo journalctl -b -u {TRIDENT_AGENT} -o cat --no-pager", check=False).stdout
            return "received node update" in journal.rsplit("agent process started", 1)[-1]

        if not poll(120, watching, 5):
            journal = vm.ssh(f"sudo journalctl -b -u {TRIDENT_AGENT} -o cat --no-pager -n 8", check=False).stdout
            raise StepError(f"{vm.name}: {TRIDENT_AGENT} isn't watching its Node:\n{journal}")
        machine = self.kubectl_json("get", "machine", vm.name)
        if machine is None:
            raise StepError(f"there's no Machine {vm.name}")

        def published() -> bool:
            return node_view(self.cluster.node(vm.name), machine)["version"] == base_version

        if not poll(300, published, 5):
            raise StepError(f"{vm.name}'s agent-info doesn't show {base_version}: "
                            f"{node_view(self.cluster.node(vm.name), machine)}")
        log(f"{vm.name}: trident-acl-agent watches its Node with the host agent's drop-in, and agent-info shows {base_version}")

    # Diagnostics

    def collect(self, directory: Path, *, vms: bool = True) -> None:
        log(f"collecting diagnostics in {directory}")
        self.cluster.diagnostics(directory / "cluster", {
            "sites.yaml": ["get", "sites", "-o", "yaml"],
            "centerpivot.log": ["-n", NAMESPACE, "logs", "-l", "app.kubernetes.io/name=centerpivot", "--prefix",
                                "--tail=2000"],
            "node-info.log": ["-n", NAMESPACE, "logs", "-l", "app.kubernetes.io/name=centerpivot-node-info",
                              "--prefix", "--tail=200"],
            "workload.yaml": ["-n", WORKLOAD_NAMESPACE, "get", "deployments,pods,pdb", "-o", "yaml"],
        })
        if self.nebraska.running():
            (directory / "nebraska.log").write_text(self.nebraska.logs())
        if vms:
            for vm in self.vms:
                try:
                    vm.diagnostics(directory / vm.name)
                except (StepError, OSError) as error:
                    log(f"warning: no diagnostics from {vm.name}: {error}")


# ---------------------------------------------------------------------------------------------
# Commands: up and down


def check_prerequisites() -> None:
    missing = [tool for tool in ("podman", "kind", "kubectl", "go", "qemu-system-x86_64", "openssl", "ssh")
               if not shutil.which(tool)]
    if missing:
        die(f"the e2e needs {', '.join(missing)}")
    if host_run(["sudo", "-n", "true"]).returncode != 0:
        die("the e2e needs sudo without a password prompt, for kind on root Podman and its bridge")
    unset = [name for name in ("ACL_AB_REGISTRY", "ACL_AB_ISO_REPO", "ACL_AB_COSI_REPO", "ACL_AB_BASE_TAG",
                               "ACL_AB_UPDATE_TAG") if not os.environ.get(name)]
    if unset:
        die(f"set {', '.join(unset)}; README.md explains them")


def cmd_up(_: argparse.Namespace) -> None:
    check_prerequisites()
    if STATE_FILE.exists():
        die(f"{STATE} has an environment already; `down` it first")
    STATE.mkdir(parents=True, exist_ok=True)
    spec = cluster.ClusterSpec(user=getpass.getuser(), node_image=KIND_NODE_IMAGE)
    # Written first, so down can remove what a failed up made.
    e2e = E2E(spec, {"cluster": spec.to_dict(), "created": datetime.datetime.now().isoformat(timespec="seconds")})
    e2e.save()
    try:
        up(e2e)
    except StepError as error:
        log(f"up failed: {error}")
        e2e.collect(RUNS / f"{stamp()}-up")
        die("`down` removes what up made", 1)
    log(f"up: {', '.join(vm.name for vm in e2e.vms)} joined, at snapshot {SNAPSHOT}; try `run`")


def up(e2e: E2E) -> None:
    e2e.build()
    e2e.record["update"] = e2e.images()
    e2e.save()
    e2e.cluster.create()
    e2e.cluster.load_image(MACHINA_IMAGE)
    e2e.cluster.load_image(CENTERPIVOT_IMAGE)
    e2e.install_control_plane()
    tls = serve.make_tls(TLS_DIR, [f"IP:{e2e.spec.gateway}"], run=host_run, log=log)
    ca = tls.ca_cert.read_text()
    base_version = e2e.record["update"]["baseVersion"]
    # One at a time: the first may install the base into the shared cache.
    for vm in e2e.vms:
        vm.abupdate("up", "--nic-tap", e2e.spec.taps[vm.number - 1], "--nic-address", vm.address,
                    "--hostname", vm.name, "--ssh-port", str(vm.ssh_port), "--memory", str(VM_MEMORY),
                    timeout=7200, stream=True)
        if vm.version() != base_version:
            raise StepError(f"{vm.name} runs {vm.version()}, not the base's {base_version}")
        vm.trust(ca)
    parallel(e2e.join, e2e.vms)
    e2e.install_centerpivot(ca)
    for vm in e2e.vms:
        e2e.check_joined(vm, base_version)
    e2e.record["nodes"] = {vm.name: e2e.cluster.record(vm.name, PREFIX) for vm in e2e.vms}
    e2e.save()
    parallel(lambda vm: vm.abupdate("snapshot", "save", SNAPSHOT, "--force", timeout=1800), e2e.vms)


def cmd_down(_: argparse.Namespace) -> None:
    if not STATE.is_dir():
        log(f"there's nothing in {STATE}")
        return
    spec = cluster.ClusterSpec.from_dict(json.loads(STATE_FILE.read_text())["cluster"]) if STATE_FILE.is_file() \
        else cluster.ClusterSpec(user=getpass.getuser())
    e2e = E2E(spec, {})
    for vm in e2e.vms:
        if vm.state_dir.is_dir():
            vm.abupdate("down", check=False, timeout=300)
    e2e.nebraska.stop()
    failures = e2e.cluster.delete()
    for path in STATE.iterdir():
        if path.is_dir() and path.name != "runs":
            shutil.rmtree(path)
        elif not path.is_dir():
            path.unlink()
    for failure in failures:
        log(f"warning: {failure}")
    log(f"removed the environment; the runs' artifacts are in {RUNS}" if RUNS.is_dir() else "removed the environment")


def cmd_status(_: argparse.Namespace) -> None:
    e2e = E2E.load()
    update = e2e.record.get("update", {})
    print(f"Update    {update.get('tag', '-')}, {update.get('baseVersion', '?')} to {update.get('version', '?')}")
    print(f"Nebraska  {'running at ' + e2e.nebraska.url if e2e.nebraska.running() else 'stopped'}")
    if not e2e.cluster.exists():
        print("Cluster   missing")
        return
    machines = e2e.kubectl_json("get", "machines")
    nodes = e2e.kubectl_json("get", "nodes")
    sample = sample_of([vm.name for vm in e2e.vms], nodes, machines, None)
    for vm in e2e.vms:
        view = sample["nodes"][vm.name]
        print(f"{vm.name}  {'Ready' if view['ready'] else 'NotReady'}, {view['version'] or '?'}, {view['phase']}"
              f"{', tainted' if view['tainted'] else ''}{': ' + view['reason'] if view['reason'] else ''}; "
              f"ssh -p {vm.ssh_port} -i {vm.state_dir / 'ssh' / 'id_ed25519'} core@127.0.0.1")


# ---------------------------------------------------------------------------------------------
# Scenarios


class Monitor:
    """Samples the nodes' phases, and the workload's availability, every SAMPLE_SECONDS, in a
    thread, for the scenario's checks and its timeline."""

    def __init__(self, e2e: E2E) -> None:
        self.e2e = e2e
        self.names = [vm.name for vm in e2e.vms]
        self.samples: list[dict[str, Any]] = []
        self.errors = 0
        self._stop = threading.Event()
        self._lock = threading.Lock()
        self._thread = threading.Thread(target=self._loop, name="monitor", daemon=True)

    def __enter__(self) -> Monitor:
        self.sample()
        self._thread.start()
        return self

    def __exit__(self, *exc_info: object) -> None:
        self._stop.set()
        self._thread.join(timeout=60)

    def _loop(self) -> None:
        while not self._stop.wait(SAMPLE_SECONDS):
            try:
                self.sample()
            except (StepError, json.JSONDecodeError, KeyError) as error:
                self.errors += 1
                log(f"warning: couldn't sample the cluster: {error}")

    def sample(self) -> dict[str, Any]:
        deployment = self.e2e.kubectl("-n", WORKLOAD_NAMESPACE, "get", "deployment", WORKLOAD, "-o", "json",
                                      "--ignore-not-found").stdout
        sample = sample_of(self.names, self.e2e.kubectl_json("get", "nodes"), self.e2e.kubectl_json("get", "machines"),
                           json.loads(deployment) if deployment.strip() else None)
        with self._lock:
            previous = self.samples[-1] if self.samples else None
            self.samples.append(sample)
        for name, view in sample["nodes"].items():
            before = previous["nodes"][name] if previous else None
            if before is None or (before["phase"], before["reason"]) != (view["phase"], view["reason"]):
                log(f"{name}: {view['phase']}{', tainted' if view['tainted'] else ''}"
                    f"{': ' + view['reason'] if view['reason'] else ''}")
        return sample

    def latest(self) -> dict[str, Any]:
        with self._lock:
            return self.samples[-1]

    def all(self) -> list[dict[str, Any]]:
        with self._lock:
            return list(self.samples)


class Context:
    """One scenario's run: its Nebraska app, its checks, and its artifacts."""

    def __init__(self, e2e: E2E, front: serve.Front, name: str, directory: Path) -> None:
        self.e2e = e2e
        self.front = front
        self.name = name
        self.directory = directory
        self.update = e2e.record["update"]
        self.target = self.update["version"]
        self.base = self.update["baseVersion"]
        self.results: list[dict[str, Any]] = []
        self.app: nebraska.App | None = None
        self.monitor: Monitor | None = None
        # The front's Omaha log is the whole run's; the scenario's starts here.
        self.omaha_start = len(front.omaha)
        self.a, self.b = e2e.vms[0], e2e.vms[1]

    # Checks

    def expect(self, ok: bool, check: str, detail: str = "") -> bool:
        self.results.append({"scenario": self.name, "check": check, "ok": bool(ok), "detail": "" if ok else detail})
        log(f"{'ok' if ok else 'FAIL'}: {self.name}: {check}{'' if ok or not detail else ': ' + detail}")
        return bool(ok)

    def failed(self) -> bool:
        return any(not result["ok"] for result in self.results)

    # Setting up

    def reset(self) -> None:
        """Return the VMs and the cluster to the joined snapshot, with centerpivot off."""
        e2e = self.e2e
        e2e.site({"enabled": False})
        e2e.kubectl("delete", "namespace", WORKLOAD_NAMESPACE, "--ignore-not-found", "--wait=false")
        parallel(lambda vm: vm.abupdate("snapshot", "restore", SNAPSHOT, "--hold", timeout=1800), e2e.vms)
        for vm in e2e.vms:
            e2e.cluster.reset_to(vm.name, e2e.record["nodes"][vm.name], PREFIX)
            e2e.kubectl("label", "node", vm.name, f"{CANARY_LABEL}-", f"{PAUSED_LABEL}-")
            e2e.kubectl("annotate", "machine", vm.name, f"{STATE_ANNOTATION}-")
        e2e.kubectl("-n", WORKLOAD_NAMESPACE, "delete", "pods", "--all", "--grace-period=0", "--force",
                    "--ignore-not-found", check=False)
        parallel(lambda vm: vm.abupdate("resume", "--restart", TRIDENT_AGENT, timeout=600), e2e.vms)
        for vm in e2e.vms:
            e2e.cluster.wait_ready(vm.name, 300)
        if not poll(300, lambda: e2e.kubectl("get", "namespace", WORKLOAD_NAMESPACE, "--ignore-not-found").stdout == ""):
            raise StepError(f"namespace {WORKLOAD_NAMESPACE} is still there")

    def label(self, vm: VM, label: str) -> None:
        self.e2e.kubectl("label", "node", vm.name, "--overwrite", f"{label}=true")

    def deploy_workload(self, replicas: int = 2) -> None:
        self.e2e.apply(*workload_manifests(replicas))
        self.e2e.kubectl("-n", WORKLOAD_NAMESPACE, "rollout", "status", f"deployment/{WORKLOAD}", "--timeout=300s")

    def enable(self, **settings: Any) -> None:
        """A Nebraska app of the scenario's own, which offers the update, and centerpivot on for it."""
        self.app = self.e2e.nebraska.create_app(
            name=self.name, track=TRACK, version=self.target, base_url=self.front.image_url(""),
            filename=self.update["filename"], size=self.update["size"], sha384=self.update["sha384"],
            update_timeout_minutes=GROUP_UPDATE_TIMEOUT_MINUTES)
        values = {**SETTINGS, **settings}
        problem = budget_problem(values)
        if problem:
            raise StepError(problem)
        self.e2e.allow_sources(self.front.update_url, [self.app.id])
        self.e2e.site(center_pivot(self.front.update_url, self.app.id, values))
        log(f"centerpivot is on for app {self.app.id}")

    # Waiting

    def wait(self, what: str, done: Callable[[dict[str, Any]], bool], timeout: float = UPDATE_SECONDS) -> dict[str, Any]:
        assert self.monitor is not None
        deadline = time.monotonic() + timeout
        while True:
            sample = self.monitor.latest()
            if done(sample):
                return sample
            if time.monotonic() >= deadline:
                raise StepError(f"timed out after {int(timeout)}s waiting for {what}; the nodes: "
                                f"{json.dumps({name: {k: v[k] for k in ('phase', 'reason', 'version')} for name, v in sample['nodes'].items()})}")
            time.sleep(SAMPLE_SECONDS)

    def updated(self, vm: VM) -> Callable[[dict[str, Any]], bool]:
        return lambda sample: updated(sample["nodes"][vm.name], self.target)

    def phase(self, vm: VM, *phases: str) -> Callable[[dict[str, Any]], bool]:
        return lambda sample: sample["nodes"][vm.name]["phase"] in phases

    # Checking

    def instances(self) -> dict[str, dict[str, Any]]:
        """Nebraska's instances of the app, by ID, with their status histories, oldest first."""
        assert self.app is not None
        found = {}
        for instance in self.e2e.nebraska.instances(self.app):
            history = self.e2e.nebraska.status_history(self.app, instance["id"])
            found[instance["id"]] = {**(instance.get("application") or {}),
                                     "history": [entry.get("status") for entry in reversed(history)]}
        return found

    def check_updated(self, vm: VM) -> None:
        view = self.monitor.latest()["nodes"][vm.name] if self.monitor else {}
        self.expect(vm.version() == self.target, f"{vm.name} booted {self.target}", f"it runs {vm.version()}")
        self.expect(view.get("version") == self.target, f"{vm.name}'s agent-info shows {self.target}", str(view))
        self.expect(not view.get("tainted") and not view.get("grant") and not view.get("endGrant"),
                    f"{vm.name} has no taint and no grant left", str(view))
        instance = self.instances().get(view.get("machineId", ""), {})
        self.expect(instance.get("status") == nebraska.STATUS_COMPLETE and instance.get("version") == self.target,
                    f"Nebraska has {vm.name}'s update Complete at {self.target}",
                    f"{nebraska.status_name(instance.get('status'))} at {instance.get('version')}")
        progress = [nebraska.STATUS_DOWNLOADING, nebraska.STATUS_DOWNLOADED, nebraska.STATUS_INSTALLED,
                    nebraska.STATUS_COMPLETE]
        history = [status for status in instance.get("history", []) if status in progress]
        self.expect(history[-len(progress):] == progress,
                    f"trident-acl-agent reported {vm.name}'s download, install, and completion to Nebraska",
                    str([nebraska.status_name(status) for status in instance.get("history", [])]))

    def check_not_updated(self, vm: VM) -> None:
        self.expect(vm.version() == self.base, f"{vm.name} still runs {self.base}", f"it runs {vm.version()}")

    def check_one_instance_each(self, vms: list[VM]) -> None:
        """The service's checks and the agent's are the same Nebraska instance for each node."""
        ids = {self.monitor.latest()["nodes"][vm.name]["machineId"] for vm in vms} if self.monitor else set()
        found = set(self.instances())
        self.expect(found == ids, f"Nebraska has one instance for each of {', '.join(vm.name for vm in vms)}, "
                    "the ID agent-info shows", f"it has {sorted(found)}, agent-info shows {sorted(ids)}")

    def write(self) -> None:
        self.directory.mkdir(parents=True, exist_ok=True)
        if self.monitor:
            (self.directory / "timeline.json").write_text(json.dumps(self.monitor.all(), indent=1) + "\n")
        (self.directory / "omaha.json").write_text(json.dumps(self.front.omaha[self.omaha_start:], indent=1) + "\n")
        if self.app:
            try:
                (self.directory / "nebraska.json").write_text(json.dumps(
                    {"app": dataclasses.asdict(self.app), "instances": self.instances()}, indent=1) + "\n")
            except StepError as error:
                log(f"warning: couldn't read Nebraska's instances: {error}")
        (self.directory / "results.json").write_text(json.dumps(self.results, indent=1) + "\n")


Scenario = Callable[[Context], None]
SCENARIOS: dict[str, tuple[str, Scenario]] = {}


def scenario(name: str, description: str) -> Callable[[Scenario], Scenario]:
    def register(func: Scenario) -> Scenario:
        SCENARIOS[name] = (description, func)
        return func
    return register


@scenario("rolling", "Both nodes update, one at a time, and a workload with a PodDisruptionBudget stays available")
def rolling(ctx: Context) -> None:
    ctx.deploy_workload()
    # The metric counts since centerpivot started, so the scenario's are what it adds.
    counted = ctx.e2e.phase_counts()
    ctx.enable()
    with Monitor(ctx.e2e) as ctx.monitor:
        ctx.wait("both nodes to update", lambda s: ctx.updated(ctx.a)(s) and ctx.updated(ctx.b)(s),
                 2 * UPDATE_SECONDS)
    added = {step: count - counted.get(step, 0) for step, count in ctx.e2e.phase_counts().items()}
    ctx.expect(all(added.get(step) == 2 for step in UPDATE_PATH),
               "centerpivot_phase_seconds counted each phase of both updates",
               str({f"{a}->{b}": added.get((a, b), 0) for a, b in UPDATE_PATH}))
    samples = ctx.monitor.all()
    ctx.expect(most_updating(samples) == 1, "no more than maxUnavailable, 1, node updated at once",
               f"{most_updating(samples)} did")
    unavailable = [s["time"] for s in samples if s["available"] is not None and s["available"] < 1]
    ctx.expect(not unavailable, "the workload stayed available", f"no pod was available at {unavailable[:5]}")
    for vm in (ctx.a, ctx.b):
        ctx.check_updated(vm)
        # Finalizing can pass between two samples; its request stays on the Node through the restart.
        seen = phases_seen(samples, vm.name)
        operations = operations_seen(samples, vm.name)
        ctx.expect(all(phase in seen for phase in ("Staging", "Draining", "Restarting", "Recovering"))
                   and operations == {"stage", "finalize"},
                   f"{vm.name} staged, drained, finalized, restarted, and recovered",
                   f"phases {seen}, requests {sorted(operations)}")
    ctx.check_one_instance_each([ctx.a, ctx.b])


@scenario("silent-agent", "A node whose agent doesn't answer is held on its own, and the other node updates")
def silent_agent(ctx: Context) -> None:
    ctx.label(ctx.a, CANARY_LABEL)
    # --runtime: gone at the next boot, and the next scenario restores the snapshot anyway.
    ctx.a.ssh(f"sudo systemctl mask --runtime --now {TRIDENT_AGENT}")
    ctx.enable()
    with Monitor(ctx.e2e) as ctx.monitor:
        ctx.wait(f"{ctx.a.name} to be held", ctx.phase(ctx.a, "Held"))
        ctx.wait(f"{ctx.b.name} to update", ctx.updated(ctx.b))
    view = ctx.monitor.latest()["nodes"][ctx.a.name]
    ctx.expect("didn't answer the stage request" in view["reason"], f"{ctx.a.name} is held because its agent didn't answer",
               view["reason"])
    ctx.expect(not view["heldApp"], f"the hold keeps only {ctx.a.name} from updating, not its app", view["heldApp"])
    ctx.expect(not view["tainted"] and not view["request"], f"{ctx.a.name} has no taint and no request",
               str(view))
    ctx.expect(not view["endGrant"], f"centerpivot ended {ctx.a.name}'s grant", str(view))
    seen = phases_seen(ctx.monitor.all(), ctx.b.name)
    first_a_held = next(i for i, s in enumerate(ctx.monitor.all()) if s["nodes"][ctx.a.name]["phase"] == "Held")
    b_before = {s["nodes"][ctx.b.name]["phase"] for s in ctx.monitor.all()[:first_a_held]}
    ctx.expect(not b_before & UPDATING_PHASES, f"{ctx.b.name} waited for the canary, {ctx.a.name}", str(seen))
    ctx.check_not_updated(ctx.a)
    instance = ctx.instances().get(view["machineId"], {})
    ctx.expect(instance.get("status") == nebraska.STATUS_ERROR, f"Nebraska has {ctx.a.name}'s update as failed, "
               "not in progress", nebraska.status_name(instance.get("status")))
    ctx.check_updated(ctx.b)
    ctx.check_one_instance_each([ctx.a, ctx.b])


@scenario("blocked-drain", "A drain a PodDisruptionBudget blocks past its deadline fails the attempt, "
                           "and the next attempt updates the node")
def blocked_drain(ctx: Context) -> None:
    ctx.label(ctx.b, PAUSED_LABEL)
    ctx.deploy_workload()
    ctx.enable()
    pdb = ["-n", WORKLOAD_NAMESPACE, "patch", "pdb", WORKLOAD, "--type=merge", "-p"]
    with Monitor(ctx.e2e) as ctx.monitor:
        # A PodDisruptionBudget that refuses at the check gets no grant, so it blocks once staging starts.
        ctx.wait(f"{ctx.a.name} to stage", ctx.phase(ctx.a, "Staging"))
        ctx.e2e.kubectl(*pdb, json.dumps({"spec": {"minAvailable": None, "maxUnavailable": 0}}))
        log("the PodDisruptionBudget lets no pod go")
        failed = ctx.wait(f"{ctx.a.name}'s drain to miss its deadline",
                          lambda s: s["nodes"][ctx.a.name]["attempts"] >= 1 and not s["nodes"][ctx.a.name]["endGrant"])
        view = failed["nodes"][ctx.a.name]
        reasons = {s["nodes"][ctx.a.name]["reason"] for s in ctx.monitor.all()}
        ctx.expect(any("drain missed its deadline" in reason for reason in reasons),
                   f"{ctx.a.name}'s attempt failed at the drain's deadline", str(sorted(reasons)))
        instance = ctx.instances().get(view["machineId"], {})
        ctx.expect(instance.get("status") == nebraska.STATUS_ERROR,
                   f"centerpivot ended {ctx.a.name}'s grant, which Nebraska has as failed",
                   nebraska.status_name(instance.get("status")))
        ctx.e2e.kubectl(*pdb, json.dumps({"spec": {"maxUnavailable": None, "minAvailable": 1}}))
        log("the PodDisruptionBudget lets one pod go again")
        ctx.wait(f"{ctx.a.name} to update", ctx.updated(ctx.a))
    samples = ctx.monitor.all()
    after = [s for s in samples if s["nodes"][ctx.a.name]["attempts"] >= 1 and s["nodes"][ctx.a.name]["phase"] == "Idle"]
    ctx.expect(after and not any(s["nodes"][ctx.a.name]["tainted"] for s in after),
               f"{ctx.a.name}'s taint went with the failed attempt", "")
    ctx.check_updated(ctx.a)
    seen = phases_seen(samples, ctx.b.name)
    ctx.expect(not set(seen) & UPDATING_PHASES, f"{ctx.b.name}, paused, got no slot", str(seen))
    ctx.check_not_updated(ctx.b)


@scenario("boot-failure", "A node that can't boot the new version falls back and is held, which stops its app")
def boot_failure(ctx: Context) -> None:
    ctx.label(ctx.a, CANARY_LABEL)
    ctx.enable()
    failer: subprocess.Popen[str] | None = None
    try:
        with Monitor(ctx.e2e) as ctx.monitor:
            # Staging downloads the image, so this is well before the restart it fails.
            ctx.wait(f"{ctx.a.name} to stage", ctx.phase(ctx.a, "Staging"))
            failer = ctx.a.popen_abupdate("--json", "fail-next-boot", "--timeout", "1800")
            log(f"{ctx.a.name}'s next boot will fail")
            held = ctx.wait(f"{ctx.a.name} to be held", ctx.phase(ctx.a, "Held"))
            output, _ = failer.communicate(timeout=300)
            ctx.expect(failer.returncode == 0, f"{ctx.a.name}'s first boot of the new version failed",
                       cluster.last_lines(output, 6))
            # The scheduler would give the slot within a check interval and its own interval.
            time.sleep(parse_duration(SETTINGS["checkInterval"]) + 60)
    finally:
        if failer and failer.poll() is None:
            failer.kill()
    view = held["nodes"][ctx.a.name]
    # Trident 0.30.0 doesn't see the fallback on ACL, and the agent reports the commit a success
    # (microsoft/trident#956). agent-info then shows the old version at the restart deadline, which
    # holds the app too (designs/centerpivot.md, section 4.9).
    detected = "didn't boot" in view["reason"]
    ctx.expect(detected or f"agent-info showing {ctx.target}" in view["reason"],
               f"{ctx.a.name} is held because it didn't boot {ctx.target}", view["reason"])
    ctx.expect(view["heldApp"] == (ctx.app.id if ctx.app else ""), "the hold stops the app", view["heldApp"])
    ctx.check_not_updated(ctx.a)
    seen = phases_seen(ctx.monitor.all(), ctx.b.name)
    ctx.expect(not set(seen) & UPDATING_PHASES, f"{ctx.b.name} got no slot while the app is held", str(seen))
    ctx.check_not_updated(ctx.b)
    if detected:
        instance = ctx.instances().get(view["machineId"], {})
        ctx.expect(instance.get("status") == nebraska.STATUS_ERROR, f"Nebraska has {ctx.a.name}'s update as failed",
                   nebraska.status_name(instance.get("status")))
    ctx.check_one_instance_each([ctx.a])


# ---------------------------------------------------------------------------------------------
# Commands: run and test


def run_scenarios(names: list[str]) -> int:
    e2e = E2E.load()
    if "nodes" not in e2e.record:
        die("up didn't finish; `down`, then `up` again")
    e2e.cluster.ensure()
    e2e.nebraska.start()
    tls = serve.make_tls(TLS_DIR, [f"IP:{e2e.spec.gateway}"], run=host_run, log=log)
    run_dir = RUNS / stamp()
    results: list[dict[str, Any]] = []
    with serve.Front(host=e2e.spec.gateway, port=FRONT_PORT, tls=tls, upstream=e2e.nebraska.url,
                     public_host=e2e.spec.gateway, log=log) as front:
        front.serve_image(e2e.record["update"]["filename"], Path(e2e.record["update"]["path"]))
        for name in names:
            description, func = SCENARIOS[name]
            log(f"scenario {name}: {description}")
            ctx = Context(e2e, front, name, run_dir / name)
            started = time.monotonic()
            try:
                ctx.reset()
                func(ctx)
            except StepError as error:
                ctx.expect(False, "the scenario ran to its end", str(error))
            finally:
                ctx.write()
                if ctx.failed():
                    e2e.collect(ctx.directory / "diagnostics")
                else:
                    e2e.collect(ctx.directory / "diagnostics", vms=False)
            log(f"scenario {name}: {'FAILED' if ctx.failed() else 'passed'} in {int(time.monotonic() - started) // 60}m")
            results.extend(ctx.results)
        e2e.site({"enabled": False})
    failed = [result for result in results if not result["ok"]]
    (run_dir / "results.json").write_text(json.dumps(results, indent=1) + "\n")
    log(f"{len(results) - len(failed)} of {len(results)} checks passed; the artifacts are in {run_dir}")
    for result in failed:
        log(f"FAIL: {result['scenario']}: {result['check']}: {result['detail']}")
    return 1 if failed else 0


def cmd_run(args: argparse.Namespace) -> None:
    unknown = [name for name in args.scenarios if name not in SCENARIOS]
    if unknown:
        die(f"there's no scenario {', '.join(unknown)}; `scenarios` lists them")
    sys.exit(run_scenarios(args.scenarios or list(SCENARIOS)))


def cmd_test(args: argparse.Namespace) -> None:
    try:
        cmd_up(args)
        code = run_scenarios(list(SCENARIOS))
    finally:
        if not args.keep:
            cmd_down(args)
    sys.exit(code)


def cmd_scenarios(_: argparse.Namespace) -> None:
    for name, (description, _func) in SCENARIOS.items():
        print(f"{name:14} {description}")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="centerpivot_e2e.py", description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    commands = parser.add_subparsers(dest="command", required=True, metavar="COMMAND")
    commands.add_parser("up", help="build, start, and join everything").set_defaults(func=cmd_up)
    run = commands.add_parser("run", help="run scenarios, all by default")
    run.add_argument("scenarios", nargs="*", metavar="SCENARIO", help=f"one of {', '.join(SCENARIOS)}")
    run.set_defaults(func=cmd_run)
    test = commands.add_parser("test", help="up, run every scenario, and down")
    test.add_argument("--keep", action="store_true", help="leave the environment up afterward")
    test.set_defaults(func=cmd_test)
    commands.add_parser("status", help="show the VMs and nodes").set_defaults(func=cmd_status)
    commands.add_parser("scenarios", help="list the scenarios").set_defaults(func=cmd_scenarios)
    commands.add_parser("down", help="remove everything but the runs' artifacts").set_defaults(func=cmd_down)
    return parser


def main() -> None:
    args = build_parser().parse_args()
    try:
        args.func(args)
    except StepError as error:
        die(str(error), 1)


if __name__ == "__main__":
    main()
