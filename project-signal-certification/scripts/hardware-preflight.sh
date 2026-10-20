#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
set -Eeuo pipefail
umask 077

if [[ $# -eq 1 && "$1" == --describe ]]; then
  while IFS= read -r line; do printf '%s\n' "$line"; done <<'DESCRIPTION'
Purpose: snapshot declared GPU, CPU/NUMA, memory, physical NIC/fabric and NVMe inventory.
Tools/devices: python3 stdlib with POSIX waitid/WNOWAIT, nvidia-smi, lscpu, ip, lsblk;
  readable Linux proc/sysfs.
Required inputs (no hardware/threshold defaults):
  DURATION_SECONDS=0 (inventory only), COMMAND_TIMEOUT_SECONDS (positive per-probe deadline),
  SCRATCH_ROOT (existing absolute private scratch parent), EXPECTED_HARDWARE_JSON:
  {"gpu":{"count":N,"product_regex":"...","vbios_regex":"...","ecc":"Enabled|Disabled",
          "memory_min_mib":N},
   "cpu":{"online_count":N,"numa_nodes":N,"model_regex":"..."},
   "memory_min_mib":N,
   "nics":[{"name":"...","state":"UP","mtu_min":N,"speed_min_mbps":N}],
   "fabric":[{"device":"...","port":N,"state":"ACTIVE","physical_state":"LinkUp",
              "link_layer":"InfiniBand|Ethernet","rate_min_gbps":N}],
   "nvme":[{"name":"nvme0n1","model_regex":"...","size_min_bytes":N}]}
  Each NIC requires exactly one speed_min_mbps (legacy positive integer) or speed:
    {"kind":"minimum","min_mbps":N}
    {"kind":"unavailable","evidence_sha256":"64 lowercase hex characters",
     "approved_unavailability_class":"errno:EINVAL|errno:EOPNOTSUPP"}
    {"kind":"inventory_only","reason":"explicit site reason","approval_reference":"..."}
  Unavailable matches only the explicitly approved sysfs read errno AND observation hash.
  Hash the UTF-8 JSON speed_observation with sort_keys=True, separators=(",",":"),
  ensure_ascii=True; it includes name, source path, status, errno name and strerror.
  These selectors do not approve a site's error form or intended NIC role.
  Unknown speed formats/errors, missing files and permission/tool errors are Inconclusive.
  inventory_only records numeric/recognized unavailable speed; speed health not evaluated.
  All schema fields are required; explicit empty arrays declare no devices of that class.
  Duplicate/unknown/mixed fields are rejected; no speed-policy fallback or defaults.
  GPU count, physical NIC names, fabric ports and NVMe names must match exactly.
  Host MemTotal, each HCA link_layer, complete ip JSON and lsblk evidence are mandatory;
  redirected proc/sysfs does not redirect external ip/lsblk. No inferred replacement values.
Paths: PROJECT_SIGNAL_RESULTS_DIR defaults to /results; HOST_PROC_ROOT=/proc,
  HOST_SYSFS_ROOT=/sys are operational paths, optionally redirected for controlled tests.
Outputs: verdict (Pass|Fail|Inconclusive), reason, measurements.txt <=16384 bytes,
  hardware-preflight.audit.log (restricted, bounded command evidence). Hardware mismatch:
  typed Fail/exit0; missing input/tool/observable inventory: Inconclusive/exit2.
Duration: no workload; at most five probes with COMMAND_TIMEOUT_SECONDS each,
  plus bounded owned-process cleanup.
No downloads, image changes, device writes, or universal hardware policy defaults.
DESCRIPTION
  exit 0
fi
if ! command -v python3 >/dev/null 2>&1; then
  results="${PROJECT_SIGNAL_RESULTS_DIR:-/results}"
  [[ "$results" == /* && ! -L "$results" ]] || { printf 'Unsafe results path\n' >&2; exit 2; }
  if [[ ! -d "$results" ]] && ! mkdir -p -- "$results"; then
    printf 'Cannot create results directory without Python\n' >&2
    exit 2
  fi
  for file in verdict reason measurements.txt; do
    [[ ! -L "$results/$file" ]] || { printf 'Unsafe result file\n' >&2; exit 2; }
  done
  printf 'Inconclusive\n' > "$results/verdict"
  printf 'PythonMissing\n' > "$results/reason"
  printf 'python3 is required; no inventory was checked.\n' > "$results/measurements.txt"
  exit 2
fi
exec python3 - "$@" <<'PY'
import csv
import errno
import hashlib
import io
import json
import os
import pathlib
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time


class Stop(Exception):
    def __init__(self, reason, detail, verdict="Inconclusive"):
        super().__init__(detail)
        self.reason, self.detail, self.verdict = reason, detail, verdict


def required(name):
    value = os.environ.get(name, "")
    if not value.strip():
        raise Stop("InputMissing", name)
    return value


def number(value, name, zero=False):
    if isinstance(value, bool) or not re.fullmatch(r"[0-9]+", str(value)):
        raise Stop("InvalidInput", name + " must be an integer")
    value = int(value)
    if value < (0 if zero else 1):
        raise Stop("InvalidInput", name + " is out of range")
    return value


def fields(value, expected, name):
    if not isinstance(value, dict) or set(value) != set(expected):
        raise Stop("InvalidInput", name + " has missing/unknown fields")


def pattern(value, name):
    if not isinstance(value, str) or not value or len(value) > 512:
        raise Stop("InvalidInput", name)
    try:
        return re.compile(value)
    except re.error as exc:
        raise Stop("InvalidInput", name + ": invalid regex") from exc


def identifier(value, name):
    if not isinstance(value, str) or not re.fullmatch(r"[A-Za-z0-9_.:-]{1,128}", value) or value in (".", ".."):
        raise Stop("InvalidInput", name)
    return value


def unique_object(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise ValueError("duplicate JSON field: " + key)
        value[key] = item
    return value


def speed_policy(nic):
    legacy = isinstance(nic, dict) and "speed_min_mbps" in nic
    fields(nic, ("name", "state", "mtu_min", "speed_min_mbps" if legacy else "speed"), "nics")
    if legacy:
        policy = {"kind": "minimum", "min_mbps": nic["speed_min_mbps"]}
    else:
        policy = nic["speed"]
    schemas = {"minimum": ("kind", "min_mbps"),
               "unavailable": ("kind", "evidence_sha256", "approved_unavailability_class"),
               "inventory_only": ("kind", "reason", "approval_reference")}
    if not isinstance(policy, dict) or not isinstance(policy.get("kind"), str) or policy["kind"] not in schemas:
        raise Stop("InvalidInput", "nics.speed requires minimum, unavailable or inventory_only")
    fields(policy, schemas[policy["kind"]], "nics.speed")
    if policy["kind"] == "minimum":
        if type(policy["min_mbps"]) is not int or policy["min_mbps"] <= 0:
            raise Stop("InvalidInput", "NIC minimum speed must be a positive integer")
    elif policy["kind"] == "unavailable":
        if not isinstance(policy["evidence_sha256"], str) or not re.fullmatch(r"[0-9a-f]{64}", policy["evidence_sha256"]):
            raise Stop("InvalidInput", "nics.speed.evidence_sha256")
        if policy["approved_unavailability_class"] not in ("errno:EINVAL", "errno:EOPNOTSUPP"):
            raise Stop("InvalidInput", "nics.speed.approved_unavailability_class requires an explicit supported errno")
    else:
        for key in ("reason", "approval_reference"):
            if not isinstance(policy[key], str) or not policy[key].strip():
                raise Stop("InvalidInput", "nics.speed." + key + " must be explicit")
    return policy


def observe_speed(path, name):
    observation = {"name": name, "source": str(path)}
    try:
        raw = path.read_text()
    except OSError as exc:
        observation.update(status="unavailable", errno=errno.errorcode.get(exc.errno, str(exc.errno)),
                           error=exc.strerror or str(exc))
        recognized = exc.errno in (errno.EINVAL, errno.EOPNOTSUPP)
    else:
        text = raw.strip()
        if re.fullmatch(r"[0-9]+", text):
            observation.update(status="numeric", mbps=int(text), raw=raw)
            recognized = True
        else:
            observation.update(status="unknown", raw=raw)
            recognized = False
    return observation, recognized


class Context:
    def __init__(self):
        self.scratch = None
        self.child = None
        self.notes = []
        self.results = pathlib.Path(os.environ.get("PROJECT_SIGNAL_RESULTS_DIR", "/results"))
        if not self.results.is_absolute() or self.results.is_symlink():
            raise Stop("UnsafeResultsPath", "results must be an absolute non-symlink directory")
        self.results.mkdir(parents=True, exist_ok=True)
        self.audit = self.open("hardware-preflight.audit.log")
        for name, data in (("verdict", b"Inconclusive"), ("reason", b"DiagnosticNotCompleted"),
                           ("measurements.txt", b"Inventory has not completed.\n")):
            with self.open(name) as stream:
                stream.write(data)

    def open(self, name):
        fd = os.open(self.results / name, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
        os.fchmod(fd, 0o600)
        return os.fdopen(fd, "wb")

    def log(self, text):
        data = text.encode("utf-8", errors="replace")
        if self.audit.tell() + len(data) > 4 * 1024 * 1024:
            raise Stop("AuditLimitExceeded", "command evidence exceeded 4 MiB; inventory is not complete")
        self.audit.write(data)
        self.audit.flush()

    def run(self, argv):
        if any(not hasattr(os, name) for name in ("waitid", "P_PID", "WEXITED", "WNOHANG", "WNOWAIT", "CLD_EXITED")):
            raise Stop("PrerequisiteMissing", "POSIX non-reaping waitid/WNOWAIT")
        # Our own PID cannot be a child; verify readiness without consuming status.
        try:
            os.waitid(os.P_PID, os.getpid(), os.WEXITED | os.WNOHANG | os.WNOWAIT)
        except ChildProcessError:
            signal.signal(signal.SIGCHLD, signal.SIG_DFL)
        except (OSError, NotImplementedError, ValueError, TypeError) as exc:
            raise Stop("PrerequisiteMissing", "waitid/WNOWAIT unavailable: " + type(exc).__name__) from exc
        else:
            raise Stop("PrerequisiteMissing", "waitid/WNOWAIT returned unexpected probe status")
        executable = shutil.which(argv[0])
        if not executable:
            raise Stop("PrerequisiteMissing", argv[0])
        self.log(f"$ {argv[0]} {' '.join(argv[1:])}\n")
        failure = None
        code = None
        with tempfile.TemporaryFile(dir=self.scratch) as output:
            self.child = subprocess.Popen([executable, *argv[1:]], stdout=output, stderr=subprocess.STDOUT,
                                          env={**os.environ, "LC_ALL": "C"}, start_new_session=True)
            try:
                code = self.wait_for_exit(self.timeout)
            except subprocess.TimeoutExpired:
                failure = Stop("CommandTimeout", argv[0])
            except Stop as exc:
                failure = exc
            finally:
                try:
                    self.terminate()
                except OSError as exc:
                    if failure is None:
                        failure = Stop("ProcessCleanupFailed", str(exc))
                    else:
                        failure.detail += "; cleanup: " + str(exc)
            if output.tell() > 4 * 1024 * 1024:
                raise Stop("ObservationTooLarge", argv[0])
            output.seek(0)
            text = output.read().decode("utf-8", errors="strict")
        self.log(f"exit={code}\n{text}\n")
        if failure is not None:
            raise failure
        if code:
            raise Stop("InventoryProbeFailed", f"{argv[0]} exit={code}")
        return text

    def wait_for_exit(self, timeout):
        deadline = time.monotonic() + timeout
        while True:
            status = os.waitid(os.P_PID, self.child.pid, os.WEXITED | os.WNOHANG | os.WNOWAIT)
            if status is not None and status.si_pid == self.child.pid:
                return status.si_status if status.si_code == os.CLD_EXITED else -status.si_status
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise subprocess.TimeoutExpired(self.child.args, timeout)
            time.sleep(min(0.02, remaining))

    def signal_group(self, signum):
        if self.child.returncode is not None:
            raise OSError("Refusing owned-group mutation after leader reap")
        try:
            os.killpg(self.child.pid, signum)
        except ProcessLookupError:
            return

    def terminate(self):
        if self.child is not None:
            # Keep the leader unreaped until all mutating group signals are complete.
            pid = self.child.pid
            errors, permissions = [], []
            for signum in (signal.SIGTERM, signal.SIGKILL):
                try:
                    self.signal_group(signum)
                except PermissionError as exc:
                    permissions.append(str(exc))
                except OSError as exc:
                    errors.append(str(exc))
                if signum == signal.SIGTERM:
                    try:
                        self.wait_for_exit(2)
                    except subprocess.TimeoutExpired:
                        pass
                    except OSError as exc:
                        errors.append(str(exc))
            try:
                self.child.wait(timeout=2)
            except (OSError, subprocess.TimeoutExpired) as exc:
                errors.append(str(exc))
            if self.child.returncode is not None:
                self.child = None
            else:
                raise OSError("Owned process did not terminate after SIGKILL; " + "; ".join(errors + permissions))
            # Darwin can deny signals to a pinned zombie; accept only proved disappearance.
            deadline = time.monotonic() + 2
            gone = False
            while time.monotonic() < deadline:
                try:
                    os.killpg(pid, 0)
                except ProcessLookupError:
                    gone = True
                    break
                except PermissionError:
                    pass
                except OSError as exc:
                    errors.append(str(exc))
                    break
                time.sleep(0.02)
            if not gone:
                errors.append("Owned process-group disappearance could not be verified")
            if errors:
                raise OSError("; ".join(errors + permissions))
            if permissions:
                self.notes.append("Owned group disappeared after transient signal permission errors: " + str(pid))

    def finish(self, verdict, reason, detail):
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        signal.signal(signal.SIGINT, signal.SIG_IGN)
        cleanup_errors = []
        try:
            self.terminate()
        except OSError as exc:
            cleanup_errors.append(str(exc))
        try:
            if self.scratch is not None:
                shutil.rmtree(self.scratch)
        except OSError as exc:
            cleanup_errors.append(str(exc))
        if cleanup_errors:
            verdict, reason, detail = "Inconclusive", "CleanupFailed", "; ".join(cleanup_errors)
        try:
            try:
                full = (f"proposed_verdict={verdict} reason={reason}\n{detail}\n" + "\n".join(self.notes)).encode()
                if len(full) > 16384:
                    if self.audit.tell() + len(full) > 4 * 1024 * 1024:
                        raise OSError("Full measurement evidence exceeds the 4 MiB audit budget")
                    self.audit.write(full)
            finally:
                self.audit.close()
        except OSError as exc:
            verdict, reason, detail = "Inconclusive", "AuditFinalizeFailed", str(exc)
        body = (f"verdict={verdict} reason={reason}\n{detail}\n" + "\n".join(self.notes)).encode()
        if len(body) > 16384:
            body = body[:16280].decode(errors="ignore").encode() + b"\nmeasurements_truncated=true; see audit log\n"
        try:
            for name, data in (("measurements.txt", body), ("reason", reason.encode()), ("verdict", verdict.encode())):
                with self.open(name) as stream:
                    stream.write(data)
        except OSError as exc:
            detail = ("Result publication failed: " + str(exc) + "\n").encode()[:16384].decode(errors="ignore").encode()
            print(detail.decode(errors="replace"), file=sys.stderr)
            for name, data in (("verdict", b"Inconclusive"), ("reason", b"ResultPublicationFailed"), ("measurements.txt", detail)):
                try:
                    with self.open(name) as stream:
                        stream.write(data)
                except OSError as fallback:
                    print(f"Cannot finalize {name}: {fallback}", file=sys.stderr)
            return 2
        return 2 if verdict == "Inconclusive" else 0


def main(ctx):
    if sys.argv[1:]:
        raise Stop("InvalidArguments", "only --describe or no arguments are accepted")
    if required("DURATION_SECONDS") != "0":
        raise Stop("InvalidInput", "DURATION_SECONDS must be 0 for a snapshot")
    ctx.timeout = number(required("COMMAND_TIMEOUT_SECONDS"), "COMMAND_TIMEOUT_SECONDS")
    scratch = pathlib.Path(required("SCRATCH_ROOT"))
    if not scratch.is_absolute() or scratch.is_symlink() or not scratch.is_dir():
        raise Stop("InvalidInput", "SCRATCH_ROOT must be an existing absolute non-symlink directory")
    try:
        expected = json.loads(required("EXPECTED_HARDWARE_JSON"), object_pairs_hook=unique_object)
    except ValueError as exc:
        raise Stop("InvalidInput", "EXPECTED_HARDWARE_JSON: " + str(exc)) from exc
    fields(expected, ("gpu", "cpu", "memory_min_mib", "nics", "fabric", "nvme"), "EXPECTED_HARDWARE_JSON")
    gpu, cpu = expected["gpu"], expected["cpu"]
    fields(gpu, ("count", "product_regex", "vbios_regex", "ecc", "memory_min_mib"), "gpu")
    fields(cpu, ("online_count", "numa_nodes", "model_regex"), "cpu")
    for key in ("count", "memory_min_mib"):
        gpu[key] = number(gpu[key], "gpu." + key)
    for key in ("online_count", "numa_nodes"):
        cpu[key] = number(cpu[key], "cpu." + key)
    product, vbios, model = (pattern(value, label) for value, label in (
        (gpu["product_regex"], "GPU product"), (gpu["vbios_regex"], "GPU VBIOS"), (cpu["model_regex"], "CPU model")))
    if gpu["ecc"] not in ("Enabled", "Disabled"):
        raise Stop("InvalidInput", "gpu.ecc")
    minimum_memory = number(expected["memory_min_mib"], "memory_min_mib")
    nic_policies = {}
    for group, schema in (
        ("nics", ("name", "state", "mtu_min")),
        ("fabric", ("device", "port", "state", "physical_state", "link_layer", "rate_min_gbps")),
        ("nvme", ("name", "model_regex", "size_min_bytes")),
    ):
        if not isinstance(expected[group], list):
            raise Stop("InvalidInput", group + " must be an array")
        seen = set()
        for item in expected[group]:
            if group == "nics":
                policy = speed_policy(item)
            else:
                fields(item, schema, group)
            key = identifier(item.get("name", item.get("device")), group + " device")
            if group == "fabric":
                key += ":" + str(number(item["port"], "fabric.port"))
            if key in seen:
                raise Stop("InvalidInput", "duplicate " + group + " device")
            seen.add(key)
            if group == "nics":
                nic_policies[key] = policy
            for name in schema:
                if name.endswith(("_min", "_mbps", "_gbps", "_bytes")):
                    item[name] = number(item[name], group + "." + name)
            for name in ("state", "physical_state", "link_layer"):
                if name in item:
                    identifier(item[name], group + "." + name)
            if group == "nvme":
                pattern(item["model_regex"], "NVMe model")
                if not re.fullmatch(r"nvme[0-9]+n[0-9]+", item["name"]):
                    raise Stop("InvalidInput", "NVMe name must be a namespace, not a raw path")
    ctx.scratch = tempfile.mkdtemp(prefix="signal-preflight-", dir=scratch)
    rows = list(csv.reader(io.StringIO(ctx.run([
        "nvidia-smi", "--query-gpu=index,name,vbios_version,ecc.mode.current,memory.total",
        "--format=csv,noheader,nounits"]))))
    if any(len(row) != 5 for row in rows):
        raise Stop("ObservationUnavailable", "GPU inventory has missing columns")
    mismatch = []
    if len(rows) != gpu["count"]:
        mismatch.append("GPU count")
    seen_gpus = set()
    for row in rows:
        index, name, version, ecc, memory = (part.strip() for part in row)
        if index in seen_gpus or not index.isdigit() or not memory.isdigit():
            raise Stop("ObservationUnavailable", "GPU inventory has invalid or duplicate fields")
        seen_gpus.add(index)
        if any(value in ("", "N/A", "[N/A]", "Not Supported") for value in (name, version, ecc)):
            raise Stop("ObservationUnavailable", "GPU product/VBIOS/ECC unavailable")
        if ecc not in ("Enabled", "Disabled"):
            raise Stop("ObservationUnavailable", "GPU ECC mode is not observable")
        if not product.fullmatch(name) or not vbios.fullmatch(version) or ecc != gpu["ecc"] or int(memory) < gpu["memory_min_mib"]:
            mismatch.append("GPU " + index + " product/VBIOS/ECC/memory")
    info = json.loads(ctx.run(["lscpu", "--json"]))
    values = {item["field"].rstrip(":"): str(item["data"]) for item in info["lscpu"]}
    online = values.get("On-line CPU(s) list", "")
    cpus = set()
    for token in online.split(","):
        if not re.fullmatch(r"[0-9]+(?:-[0-9]+)?", token):
            raise Stop("ObservationUnavailable", "lscpu online CPU list")
        ends = [int(value) for value in token.split("-")]
        if ends[-1] < ends[0] or ends[-1] > 1048576:
            raise Stop("ObservationUnavailable", "lscpu CPU range")
        group = set(range(ends[0], ends[-1] + 1))
        if cpus & group:
            raise Stop("ObservationUnavailable", "duplicate online CPU inventory")
        cpus.update(group)
    if not values.get("NUMA node(s)", "").isdigit() or not values.get("Model name", ""):
        raise Stop("ObservationUnavailable", "lscpu model/NUMA inventory")
    if len(cpus) != cpu["online_count"] or int(values["NUMA node(s)"]) != cpu["numa_nodes"] or not model.fullmatch(values["Model name"]):
        mismatch.append("CPU model/online count/NUMA nodes")
    proc = pathlib.Path(os.environ.get("HOST_PROC_ROOT", "/proc"))
    sysfs = pathlib.Path(os.environ.get("HOST_SYSFS_ROOT", "/sys"))
    if not proc.is_absolute() or not sysfs.is_absolute():
        raise Stop("InvalidInput", "proc/sysfs roots must be absolute")
    memory_text = (proc / "meminfo").read_text()
    memories = re.findall(r"^MemTotal:\s+([0-9]+)\s+kB$", memory_text, re.M)
    if len(memories) != 1 or int(memories[0]) <= 0:
        raise Stop("ObservationUnavailable", "MemTotal unavailable")
    observed_memory_kib = int(memories[0])
    if observed_memory_kib < minimum_memory * 1024:
        mismatch.append("host memory")
    try:
        links = json.loads(ctx.run(["ip", "-json", "link", "show"]), object_pairs_hook=unique_object)
    except ValueError as exc:
        raise Stop("ObservationUnavailable", "invalid ip JSON: " + str(exc)) from exc
    if not isinstance(links, list) or any(not isinstance(item, dict) for item in links):
        raise Stop("ObservationUnavailable", "NIC inventory must be a JSON array")
    if any(not isinstance(item.get("ifname"), str) or not re.fullmatch(r"[A-Za-z0-9_.:-]{1,128}", item["ifname"])
           or item["ifname"] in (".", "..") for item in links):
        raise Stop("ObservationUnavailable", "ip JSON NIC name unavailable")
    observed_names = [item["ifname"] for item in links]
    net_root = sysfs / "class/net"
    if len(set(observed_names)) != len(observed_names) or not net_root.is_dir():
        raise Stop("ObservationUnavailable", "duplicate NICs or missing sysfs NIC inventory")
    sysfs_physical = set()
    for path in net_root.iterdir():
        if not path.is_dir():
            continue
        try:
            (path / "device").stat()
        except FileNotFoundError:
            continue
        sysfs_physical.add(path.name)
    if sysfs_physical - set(observed_names):
        raise Stop("ObservationUnavailable", "ip JSON omits physical NICs visible in declared sysfs view")
    physical = {}
    for item in links:
        if item["ifname"] not in sysfs_physical:
            continue
        if (not isinstance(item.get("operstate"), str) or not item["operstate"].strip()
                or type(item.get("mtu")) is not int or item["mtu"] <= 0):
            raise Stop("ObservationUnavailable", "ip JSON NIC state/MTU unavailable: " + item["ifname"])
        physical[item["ifname"]] = item
    if set(physical) != {item["name"] for item in expected["nics"]}:
        mismatch.append("physical NIC inventory")
    for nic in expected["nics"]:
        if nic["name"] not in physical:
            continue
        actual = physical[nic["name"]]
        if actual["operstate"] != nic["state"] or actual["mtu"] < nic["mtu_min"]:
            mismatch.append("NIC state/MTU: " + nic["name"])
        policy = nic_policies[nic["name"]]
        observation, recognized = observe_speed(sysfs / "class/net" / nic["name"] / "speed", nic["name"])
        evidence_hash = hashlib.sha256(json.dumps(observation, sort_keys=True, separators=(",", ":"),
                                                ensure_ascii=True).encode()).hexdigest()
        record = {"nic": nic["name"], "state": actual["operstate"], "mtu": actual["mtu"],
                  "speed_policy": policy["kind"], "speed_observation": observation,
                  "evidence_sha256": evidence_hash, "speed_evaluation": "not_evaluated"}
        if policy["kind"] == "inventory_only":
            record.update(reason=policy["reason"], approval_reference=policy["approval_reference"])
        elif policy["kind"] == "unavailable":
            record["approved_unavailability_class"] = policy["approved_unavailability_class"]
        if not recognized:
            ctx.notes.append(json.dumps(record, sort_keys=True))
            raise Stop("ObservationUnavailable", "NIC speed has unapproved error or unknown format: " + nic["name"])
        if policy["kind"] == "minimum":
            if observation["status"] != "numeric":
                ctx.notes.append(json.dumps(record, sort_keys=True))
                raise Stop("ObservationUnavailable", "numeric NIC speed unavailable: " + nic["name"])
            record["min_mbps"] = policy["min_mbps"]
            record["speed_evaluation"] = "minimum_met" if observation["mbps"] >= policy["min_mbps"] else "below_minimum"
            if observation["mbps"] < policy["min_mbps"]:
                mismatch.append("NIC speed below minimum: " + nic["name"])
        elif policy["kind"] == "unavailable":
            matched = (observation["status"] == "unavailable"
                       and "errno:" + observation["errno"] == policy["approved_unavailability_class"]
                       and evidence_hash == policy["evidence_sha256"])
            record["speed_evaluation"] = "unavailability_matched" if matched else "unavailability_mismatch"
            if not matched:
                mismatch.append("NIC approved unavailability mismatch: " + nic["name"])
        ctx.notes.append(json.dumps(record, sort_keys=True))
    fabric_root = sysfs / "class/infiniband"
    if not fabric_root.is_dir():
        raise Stop("ObservationUnavailable", "fabric sysfs inventory is missing")
    actual_ports = {f"{path.parent.parent.name}:{path.name}": path for path in fabric_root.glob("*/ports/*")}
    if set(actual_ports) != {f"{item['device']}:{item['port']}" for item in expected["fabric"]}:
        mismatch.append("fabric port inventory")
    for port in expected["fabric"]:
        name = f"{port['device']}:{port['port']}"
        if name not in actual_ports:
            continue
        directory = actual_ports[name]
        state, physical_state, layer, rate = ((directory / name).read_text().strip() for name in ("state", "phys_state", "link_layer", "rate"))
        if not state or not physical_state or layer not in ("InfiniBand", "Ethernet"):
            raise Stop("ObservationUnavailable", "fabric state/physical state/link_layer unavailable: " + name)
        rate_match = re.fullmatch(r"([0-9]+(?:\.[0-9]+)?)\s+Gb/sec(?:\s+\([^\n]*\))?", rate)
        if not rate_match:
            raise Stop("ObservationUnavailable", "fabric rate unavailable: " + name)
        if (state.split(":")[-1].strip() != port["state"] or physical_state.split(":")[-1].strip() != port["physical_state"]
                or layer != port["link_layer"] or float(rate_match[1]) < port["rate_min_gbps"]):
            mismatch.append("fabric state/layer/rate: " + name)
    try:
        block = json.loads(ctx.run(["lsblk", "--json", "--bytes", "--nodeps", "--output", "NAME,TYPE,SIZE,MODEL,TRAN,ROTA"]),
                           object_pairs_hook=unique_object)
    except ValueError as exc:
        raise Stop("ObservationUnavailable", "invalid lsblk JSON: " + str(exc)) from exc
    if not isinstance(block, dict) or "blockdevices" not in block:
        raise Stop("ObservationUnavailable", "lsblk blockdevices observation missing")
    devices = block["blockdevices"]
    if not isinstance(devices, list) or any(not isinstance(item, dict) for item in devices):
        raise Stop("ObservationUnavailable", "block-device inventory must be a JSON array")
    device_names = [identifier(item["name"], "observed block device") for item in devices]
    if len(set(device_names)) != len(device_names):
        raise Stop("ObservationUnavailable", "duplicate block-device inventory")
    nvme = {item["name"]: item for item in devices if re.fullmatch(r"nvme[0-9]+n[0-9]+", item["name"])}
    if set(nvme) != {item["name"] for item in expected["nvme"]}:
        mismatch.append("NVMe inventory")
    for wanted in expected["nvme"]:
        if wanted["name"] not in nvme:
            continue
        actual = nvme[wanted["name"]]
        if (any(not isinstance(actual.get(key), str) or not actual[key].strip() for key in ("type", "model", "tran"))
                or type(actual.get("size")) is not int or actual["size"] <= 0
                or type(actual.get("rota")) not in (bool, int) or actual["rota"] not in (False, True, 0, 1)):
            raise Stop("ObservationUnavailable", "NVMe fields missing")
        if (actual["type"] != "disk" or actual["tran"] != "nvme" or actual["rota"] not in (False, 0)
                or number(actual["size"], "observed NVMe bytes") < wanted["size_min_bytes"]
                or not re.fullmatch(wanted["model_regex"], actual["model"].strip())):
            mismatch.append("NVMe model/transport/size: " + wanted["name"])
    ctx.notes.append(json.dumps({"gpu_count": len(rows), "online_cpus": len(cpus), "numa_nodes": values["NUMA node(s)"],
                                 "memory_kib": observed_memory_kib, "nics": sorted(physical), "fabric_ports": sorted(actual_ports),
                                 "nvme": sorted(nvme)}, sort_keys=True))
    if mismatch:
        raise Stop("HardwareInventoryMismatch", "; ".join(mismatch), "Fail")
    detail = "All explicitly declared inventory guards matched."
    if any(policy["kind"] == "inventory_only" for policy in nic_policies.values()):
        detail += " inventory_only NIC speed health not evaluated; see per-interface observations and approvals."
    return "Pass", "DeclaredInventoryMatched", detail


ctx = None
verdict, reason, detail, code = "Inconclusive", "UnhandledError", "Inventory did not complete.", 2
try:
    ctx = Context()
    def interrupted(signum, _frame):
        raise Stop("Interrupted", f"signal={signum}")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    verdict, reason, detail = main(ctx)
    code = 0
except Stop as exc:
    verdict, reason, detail = exc.verdict, exc.reason, exc.detail
    code = 0 if verdict == "Fail" else 2
except (OSError, ValueError, KeyError, TypeError, OverflowError, csv.Error) as exc:
    reason, detail = "ObservationOrInputError", f"{type(exc).__name__}: {exc}"
finally:
    if ctx is not None:
        code = ctx.finish(verdict, reason, detail)
    else:
        print(f"{reason}: {detail}", file=sys.stderr)
sys.exit(code)
PY
