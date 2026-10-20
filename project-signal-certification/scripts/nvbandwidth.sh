#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

if [[ $# == 1 && $1 == --describe ]]; then
  printf '%s\n' 'nvbandwidth: first-party, selected local CUDA bandwidth and latency measurements.
No arguments are accepted except --describe (which does no work and needs only Bash).
Normal invocation requires configuration; absent configuration is Inconclusive, not Pass.

Required runtime: Bash, Python >=3.9 (stdlib) with functional POSIX waitid/WNOWAIT,
nvidia-smi, and a runtime-installed
standard NVIDIA nvbandwidth with -j -t -b -i and default data verification enabled.
Bootstrap uses mkdir/chmod; checking pre-existing outputs also requires BSD/GNU stat.
No downloads, MPI, SSH, vendor suite, or shell argument expansion are used.
Requires full CUDA GPUs and their driver/device nodes, host memory, and peer access
for selected device-to-device cases. MIG instances are not supported.

Inputs (environment; no defaults unless stated):
  SCRATCH_ROOT: absolute existing directory; no symlink component. Only a private
    0700 child is created/removed; the caller root and other children are untouched.
  PROJECT_SIGNAL_RESULTS_DIR: absolute directory, default /results; existing parents
    required, final directory may be created. No symlink component or result files.
  COMMAND_TIMEOUT_SECONDS: finite positive decimal seconds, overall wall deadline
    from Python clock initialization for ALL subprocesses, including inventory and
    isolated Python product-regex checking (which also has the overall deadline).
  DURATION_SECONDS: finite positive decimal seconds, maximum benchmark-phase wall
    budget, NOT a minimum stress duration. Complete exactly ITERATIONS sweeps.
    Every benchmark shares min(overall deadline, phase start + DURATION_SECONDS).
    No new command starts at a deadline. Termination/drain/reap adds at most 2 seconds
    per aborted command; bounded parsing/result/cleanup I/O is additional overhead.
  ITERATIONS: integer 1..10000, complete repetitions of every selected case.
  GPU_DEVICES: unique CSV visible ordinals (0,1,...; no whitespace/empty fields),
    into nvidia-smi full-GPU inventory sorted by numeric PCI bus ID.
    Inherited CUDA_VISIBLE_DEVICES is ignored; selected UUIDs define CUDA visibility.
  EXPECTED_GPU_COUNT: integer 1..64, exact SELECTED count, equal to CSV length.
    Other visible GPUs are not selected or claimed tested.
  EXPECTED_GPU_PRODUCT_REGEX: Python regex, 1..512 characters, fullmatch of EACH
    selected product name; no default product or count policy.
  NVBANDWIDTH_CASES: unique CSV of exact names, no default selection:
    host_to_device_memcpy_ce             GB/s, 1 x selected-GPU matrix
    device_to_host_memcpy_ce             GB/s, 1 x selected-GPU matrix
    device_to_device_memcpy_read_ce      GB/s, GPU x GPU off-diagonal matrix
    device_to_device_memcpy_write_ce     GB/s, GPU x GPU off-diagonal matrix
    host_device_latency_sm              ns, GPU access to host-memory latency
    device_to_device_latency_sm          ns, GPU x GPU off-diagonal matrix
    Host latency is a host/device access test, not two directional DMA latencies.
    Device-to-device cases require at least two selected GPUs.
  NVBANDWIDTH_BUFFER_MIB: integer 1..2147483647, -b buffer size in MiB.
  NVBANDWIDTH_SAMPLES: integer 1..1000000, -i tool samples per case per sweep.
  NVBANDWIDTH_LIMITS_JSON: optional JSON object, default {}, exact selected-case
    keys only. Bandwidth values: {"min_gbps": positive finite number}.
    Latency values: {"max_latency_ns": positive finite number}. No global limit.
    Compare EVERY relevant matrix cell in EVERY sweep; minimum bandwidth or maximum
    latency is the reported worst value. GB/s is decimal 10^9 bytes/s.
  NVBANDWIDTH_BIN: optional absolute executable path, basename nvbandwidth;
    default resolve nvbandwidth on PATH (absence is Inconclusive).
  NVIDIA_SMI_BIN: optional absolute executable path, basename nvidia-smi;
    default resolve nvidia-smi on PATH.
  TOOL_LIBRARY_PATH: optional colon-separated absolute existing non-symlink library
    directories; default unset. Sole source of child LD_LIBRARY_PATH.
  PATH: used only to locate python3 and unspecified executables. Child PATH is fixed
    system paths plus the Python interpreter directory. No other inherited inputs.
    Child HOME/TMPDIR/TMP/TEMP are private scratch, CUDA caches are disabled.

Exact benchmark argv: nvbandwidth -j -t CASE -b BUFFER_MIB -i SAMPLES.
Format contract: one JSON document, nvbandwidth.testcases is exactly one entry with
the requested name, status Passed, and bandwidth_description/bandwidth_matrix
(or latency_description/latency_matrix). Description must declare matching GB/s or ns.
Host matrices are 1 x N; device matrices N x N, diagonal N/A/null, ALL off-diagonals
required. No loose SUM matching. Finite numeric cells only; negative/corrupt cells
are Inconclusive. Zero metrics or positive explicit verification_errors are Fail.
Default verification is never disabled; unsupported/failed/partial JSON is not Pass.
Tool git_version, if supplied, is recorded; this is a format contract, not a version pin.

Outputs: verdict, reason, measurements.txt (nonblank JSON, <=16384 bytes), and a
private audit-nvbandwidth-* directory (0700; per-command argv/exit/timing/capture
metadata and stdout/stderr logs 0600; no environment dump). Capture limit is 1 MiB
PER stream PER command; truncation, timeout, signal, or incomplete capture is
Inconclusive and explicitly recorded. At most 4096 commands; larger plans rejected.
Evidence closes are checked before verdict publication; result streams are staged
and closed before replacement. Audit/close/publication errors cannot publish Pass.
The leader remains unreaped through TERM/KILL; SIGCHLD is set to its default.
Post-reap cleanup only probes signal 0; missing waitid/WNOWAIT is Inconclusive.
Pass/Fail exit 0; missing inputs/tools/observability, parser/runtime errors, deadline
or signal: Inconclusive exit 2. Hardware/product mismatch and measured-limit or
correctness failure: Fail exit 0. No implicit performance floors. Results cover only
the declared selection; they do not certify an unselected case or sustained duration.'
  exit 0
fi

bootstrap_result() {
  local code=$1 dir=${PROJECT_SIGNAL_RESULTS_DIR-/results} part current='' name links stat_bin
  local -a parts
  [[ $dir == /* && $dir != / && $dir != *[[:cntrl:]]* ]] || return 1
  IFS=/ read -r -a parts <<< "$dir"
  for part in "${parts[@]}"; do
    [[ -z $part ]] && continue
    [[ $part != . && $part != .. ]] || return 1
    current+=/"$part"
    [[ ! -L $current ]] || return 1
  done
  if [[ ! -d $dir ]]; then
    mkdir -m 700 "$dir" 2>/dev/null || return 1
  fi
  for name in verdict reason measurements.txt; do
    [[ ! -L $dir/$name && ( ! -e $dir/$name || -f $dir/$name ) ]] || return 1
    if [[ -e $dir/$name ]]; then
      [[ -O $dir/$name ]] || return 1
      if [[ -x /usr/bin/stat ]]; then stat_bin=/usr/bin/stat
      else stat_bin=$(command -v stat) || return 1
      fi
      links=$("$stat_bin" -c %h "$dir/$name" 2>/dev/null) ||
        links=$("$stat_bin" -f %l "$dir/$name" 2>/dev/null) || return 1
      [[ $links == 1 ]] || return 1
    fi
  done
  printf '%s\n' Inconclusive > "$dir/verdict" || return 1
  printf '%s\n' "$code" > "$dir/reason" || return 1
  printf '{"verdict":"Inconclusive","reason":"%s","bootstrap":true}\n' "$code" > "$dir/measurements.txt" || return 1
  if [[ -x /bin/chmod ]]; then
    /bin/chmod 600 "$dir/verdict" "$dir/reason" "$dir/measurements.txt" || return 1
  elif command -v chmod >/dev/null 2>&1; then
    chmod 600 "$dir/verdict" "$dir/reason" "$dir/measurements.txt" || return 1
  fi
}
trap 'bootstrap_result Interrupted || printf "%s\n" "Inconclusive: interrupted result publication failed" >&2; exit 2' HUP INT TERM
if ! command -v python3 >/dev/null 2>&1; then
  bootstrap_result PythonUnavailable || printf '%s\n' 'Inconclusive: results unavailable' >&2
  exit 2
fi
# Establish non-Pass evidence even if the interpreter cannot start.
bootstrap_result RunNotCompleted || { printf '%s\n' 'Inconclusive: unsafe/unwritable results' >&2; exit 2; }
trap 'bootstrap_result PythonLaunchFailed || printf "%s\n" "Inconclusive: Python launch result publication failed" >&2' EXIT
exec python3 -I -B - "$@" <<'PY'
import time
START = time.monotonic()
import csv
import errno
import io
import json
import math
import os
import re
import selectors
import shutil
import signal
import stat
import subprocess
import sys
import uuid
CAP = 1048576
LIMIT = 16384
interrupted = 0
active = None
results_fd = audit_fd = scratch_root_fd = scratch_fd = None
scratch_name = scratch_path = None
deadline = phase_deadline = None
sequence = 0
summary = {"test": "nvbandwidth", "commands": 0, "capture_loss": False}


class Outcome(Exception):
    def __init__(self, verdict, reason):
        self.verdict, self.reason = verdict, reason


def inconclusive(reason):
    raise Outcome("Inconclusive", reason)


def on_signal(number, _frame):
    global interrupted
    interrupted = number


for sig in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
    signal.signal(sig, on_signal)


def check(work=False):
    if interrupted:
        inconclusive("Interrupted")
    end = min(deadline, phase_deadline) if work else deadline
    if end is not None and time.monotonic() >= end:
        inconclusive("DeadlineExceeded")


def directory(path, create=False):
    if not path or not path.startswith("/") or path == "/" or any(ord(c) < 32 or ord(c) == 127 for c in path):
        inconclusive("UnsafeDirectory")
    parts = [p for p in path.split("/") if p]
    if any(p in (".", "..") for p in parts):
        inconclusive("UnsafeDirectory")
    fd = os.open("/", os.O_RDONLY | os.O_DIRECTORY)
    try:
        for index, part in enumerate(parts):
            try:
                child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            except FileNotFoundError:
                if not create or index != len(parts) - 1:
                    raise
                os.mkdir(part, 0o700, dir_fd=fd)
                child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            previous, fd = fd, child
            os.close(previous)
        result, fd = fd, None
        return result
    finally:
        if fd is not None:
            os.close(fd)


def file_fd(parent, name, exclusive=False):
    flags = os.O_WRONLY | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK
    fd = os.open(name, flags | (os.O_EXCL if exclusive else 0), 0o600, dir_fd=parent)
    info = os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid != os.geteuid():
        os.close(fd)
        inconclusive("UnsafeResultFile")
    os.fchmod(fd, 0o600)
    os.ftruncate(fd, 0)
    return fd


def write_file(parent, name, data, exclusive=False):
    fd = file_fd(parent, name, exclusive)
    with os.fdopen(fd, "wb") as stream:
        stream.write(data)


def write_all(fd, data):
    remaining = memoryview(data)
    while remaining:
        written = os.write(fd, remaining)
        if written <= 0:
            raise OSError("short audit write")
        remaining = remaining[written:]


def encoded(value):
    return (json.dumps(value, sort_keys=True, allow_nan=False, separators=(",", ":")) + "\n").encode()


def notify(code, error):
    detail = "Inconclusive: %s error_type=%s errno=%s\n" % (code, type(error).__name__, getattr(error, "errno", None))
    try:
        write_all(2, detail.encode())
    except OSError as stderr_error:
        summary.setdefault("stderr_notification_errors", []).append(type(stderr_error).__name__)


def publish(parent, verdict, reason, data):
    documents = (("measurements.txt", data), ("reason", (reason + "\n").encode()),
                 ("verdict", (verdict + "\n").encode()))
    staged = []
    try:
        for name, content in documents:
            try:
                info = os.stat(name, dir_fd=parent, follow_symlinks=False)
            except FileNotFoundError:
                info = None
            if info is not None and (not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid != os.geteuid()):
                inconclusive("UnsafeResultFile")
            pending = ".project-signal-" + uuid.uuid4().hex + "-" + name
            staged.append((pending, name))
            write_file(parent, pending, content, True)
        # Every data stream is successfully closed before any Pass is published.
        for pending, name in staged:
            os.replace(pending, name, src_dir_fd=parent, dst_dir_fd=parent)
    finally:
        for pending, _name in staged:
            try:
                os.unlink(pending, dir_fd=parent)
            except FileNotFoundError:
                continue


def emergency(parent, reason):
    data = encoded({"verdict": "Inconclusive", "reason": reason, "publication_failed": True,
                    "audit_directory": summary.get("audit_directory")})
    for name, content in (("measurements.txt", data), ("reason", (reason + "\n").encode()),
                          ("verdict", b"Inconclusive\n")):
        try:
            write_file(parent, name, content)
        except (OSError, ValueError, Outcome) as error:
            notify("EmergencyPublicationFailed_" + name, error)


def required(name):
    value = os.environ.get(name, "")
    if not value or len(value) > 65536 or any(ord(c) < 32 or ord(c) == 127 for c in value):
        inconclusive("MissingOrInvalidInput_" + name)
    return value


def integer(value, label, lower=1, upper=2147483647):
    if not re.fullmatch(r"0|[1-9][0-9]*", value) or len(value) > 20:
        inconclusive("InvalidInteger_" + label)
    number = int(value)
    if not lower <= number <= upper:
        inconclusive("InvalidInteger_" + label)
    return number


def seconds(name):
    value = required(name)
    if not re.fullmatch(r"(?:0|[1-9][0-9]*)(?:\.[0-9]+)?", value):
        inconclusive("InvalidSeconds_" + name)
    number = float(value)
    if not math.isfinite(number) or number <= 0 or not math.isfinite(START + number):
        inconclusive("InvalidSeconds_" + name)
    return number


def unique_csv(name):
    values = required(name).split(",")
    if not all(values) or len(values) > 64 or len(set(values)) != len(values):
        inconclusive("InvalidCSV_" + name)
    return values


def json_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            inconclusive("DuplicateJSONKey")
        result[key] = value
    return result


def json_load(text):
    try:
        return json.loads(text, object_pairs_hook=json_object,
                          parse_constant=lambda _value: inconclusive("NonfiniteJSON"))
    except (ValueError, TypeError, RecursionError):
        inconclusive("MalformedJSON")


def positive(value):
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        inconclusive("InvalidLimit")
    try:
        number = float(value)
    except (ValueError, OverflowError):
        inconclusive("InvalidLimit")
    if not math.isfinite(number) or number <= 0:
        inconclusive("InvalidLimit")
    return number


def tool(name, default):
    if name in os.environ:
        path = required(name)
        if not path.startswith("/"):
            inconclusive("ToolPathNotAbsolute_" + name)
    else:
        path = shutil.which(default)
    if not path or os.path.basename(path) != default or not os.path.isfile(path) or not os.access(path, os.X_OK):
        inconclusive("ToolUnavailable_" + name)
    return os.path.abspath(path)


def child_environment(selected):
    env = {"PATH": os.path.dirname(sys.executable) + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
           "LANG": "C", "LC_ALL": "C", "HOME": scratch_path,
           "TMPDIR": scratch_path, "TMP": scratch_path, "TEMP": scratch_path,
           "CUDA_CACHE_DISABLE": "1", "CUDA_DEVICE_ORDER": "PCI_BUS_ID"}
    if selected:
        env["CUDA_VISIBLE_DEVICES"] = ",".join(gpu["uuid"] for gpu in selected)
    if "TOOL_LIBRARY_PATH" in os.environ:
        paths = required("TOOL_LIBRARY_PATH").split(":")
        for path in paths:
            fd = directory(path)
            os.close(fd)
        env["LD_LIBRARY_PATH"] = ":".join(paths)
    return env


def require_waitid():
    names = ("P_PID", "WEXITED", "WNOHANG", "WNOWAIT")
    if not callable(getattr(os, "waitid", None)) or any(type(getattr(os, name, None)) is not int for name in names):
        inconclusive("NonReapingWaitUnavailable")
    try:
        signal.signal(signal.SIGCHLD, signal.SIG_DFL)
        os.waitid(os.P_PID, os.getpid(), os.WEXITED | os.WNOHANG | os.WNOWAIT)
    except ChildProcessError as error:
        if error.errno == errno.ECHILD:
            return
        notify("NonReapingWaitUnavailable", error)
        inconclusive("NonReapingWaitUnavailable")
    except (OSError, ValueError) as error:
        notify("NonReapingWaitUnavailable", error)
        inconclusive("NonReapingWaitUnavailable")


class OwnedGroup:
    def __init__(self, proc):
        self.proc = proc
        self.terminal = self.reaped = self.lost = self.signals_done = self.finished = False
        self.errors = []
        self.permission_errors = []
        self.details = {"leader_reaped": False, "post_reap_gone": False,
                        "transient_permission_errors": 0, "signal_events": [], "post_reap_probes": 0}

    def observe(self):
        if self.lost or self.proc.returncode is not None:
            self.lost = True
            raise ChildProcessError(errno.ECHILD, "owned leader no longer pinned")
        if not self.terminal:
            try:
                status = os.waitid(os.P_PID, self.proc.pid, os.WEXITED | os.WNOHANG | os.WNOWAIT)
            except ChildProcessError:
                self.lost = True
                raise
            if status is not None:
                if status.si_pid != self.proc.pid:
                    self.lost = True
                    raise ChildProcessError(errno.ECHILD, "unexpected owned leader status")
                self.terminal = True
        return self.terminal


def stop_group(group):
    if group.finished:
        if group.errors:
            raise group.errors[0]
        return
    proc = group.proc
    cleanup_end = time.monotonic() + 1.0
    if not group.signals_done:
        try:
            for number in (signal.SIGTERM, signal.SIGKILL):
                try:
                    group.observe()
                except OSError as error:
                    group.errors.append(error)
                    if group.lost:
                        break
                event = {"signal": int(number), "leader_returncode": proc.returncode,
                         "leader_terminal": group.terminal}
                group.details["signal_events"].append(event)
                # WNOWAIT pins the leader/PID through EVERY mutating group signal.
                try:
                    os.killpg(proc.pid, number)
                    event["result"] = "sent"
                except ProcessLookupError:
                    event["result"] = "not-found"
                except PermissionError as error:
                    event.update({"result": "permission-error", "errno": error.errno})
                    group.permission_errors.append(error)
                except OSError as error:
                    event.update({"result": "error", "errno": error.errno})
                    group.errors.append(error)
                if number == signal.SIGTERM:
                    until = min(cleanup_end, time.monotonic() + 0.2)
                    try:
                        while not group.observe() and time.monotonic() < until:
                            time.sleep(0.01)
                    except OSError as error:
                        group.errors.append(error)
        finally:
            group.signals_done = True
    # No poll/wait/reap occurs until all mutating signals are permanently disabled.
    if not group.lost:
        try:
            proc.wait(timeout=max(0, min(0.3, cleanup_end - time.monotonic())))
            group.reaped = True
        except (OSError, subprocess.SubprocessError) as error:
            group.errors.append(error)
    group.details["leader_reaped"] = group.reaped
    if group.reaped:
        last_probe_error = None
        while time.monotonic() < cleanup_end:
            group.details["post_reap_probes"] += 1
            try:
                os.killpg(proc.pid, 0)
            except ProcessLookupError:
                group.details["post_reap_gone"] = True
                break
            except PermissionError as error:
                last_probe_error = error
            except OSError as error:
                group.errors.append(error)
                break
            time.sleep(0.01)
        if group.details["post_reap_gone"]:
            group.details["transient_permission_errors"] = len(group.permission_errors)
        else:
            group.errors.append(last_probe_error or (group.permission_errors[0] if group.permission_errors else
                                OSError(errno.ETIMEDOUT, "owned group disappearance unproved")))
    else:
        group.errors.append(OSError(errno.ECHILD, "owned leader reap unproved"))
    group.finished = True
    if group.errors:
        raise group.errors[0]


def run(argv, env, work=False):
    global active, sequence
    check(work)
    require_waitid()
    sequence += 1
    if sequence > 4096:
        inconclusive("CommandCountExceeded")
    summary["commands"] = sequence
    prefix = "%04d" % sequence
    meta = {"argv": argv, "exit_status": None, "loss_status": "starting",
            "stdout_observed_bytes": 0, "stderr_observed_bytes": 0,
            "stdout_retained_bytes": 0, "stderr_retained_bytes": 0,
            "truncated": False, "timeout": False, "signal": 0}
    write_file(audit_fd, prefix + ".json", encoded(meta))
    logs = [file_fd(audit_fd, prefix + ".stdout.log"), file_fd(audit_fd, prefix + ".stderr.log")]
    buffers = [bytearray(), bytearray()]
    sel = selectors.DefaultSelector()
    proc = None
    group = None
    problem = None
    start = time.monotonic()

    def capture(key):
        try:
            block = os.read(key.fd, 65536)
        except BlockingIOError:
            return
        if not block:
            sel.unregister(key.fileobj)
            return
        index = key.data
        label = "stdout" if index == 0 else "stderr"
        meta[label + "_observed_bytes"] += len(block)
        room = CAP - len(buffers[index])
        keep = block[:room]
        buffers[index].extend(keep)
        write_all(logs[index], keep)
        meta[label + "_retained_bytes"] += len(keep)
        if len(block) > room:
            meta["truncated"] = True

    try:
        proc = subprocess.Popen(argv, cwd=scratch_path, env=env, start_new_session=True,
                                stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        group = OwnedGroup(proc)
        active = group
        for index, stream in enumerate((proc.stdout, proc.stderr)):
            os.set_blocking(stream.fileno(), False)
            sel.register(stream, selectors.EVENT_READ, index)
        end = min(deadline, phase_deadline) if work else deadline
        while sel.get_map() or not group.observe():
            if interrupted:
                problem = "Interrupted"
                meta["signal"] = interrupted
                break
            if time.monotonic() >= end:
                problem = "DeadlineExceeded"
                meta["timeout"] = True
                break
            for key, _event in sel.select(min(0.05, max(0, end - time.monotonic()))):
                capture(key)
                if meta["truncated"]:
                    problem = "CommandOutputTruncated"
                    break
            if problem:
                break
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        problem = "CommandRuntimeError"
        meta["runtime_error_type"] = type(error).__name__
    finally:
        if group is not None:
            try:
                stop_group(group)
            except (OSError, subprocess.SubprocessError) as error:
                problem = "ProcessCleanupFailed"
                meta["cleanup_error_type"] = type(error).__name__
                meta["cleanup_errno"] = getattr(error, "errno", None)
                notify("ProcessCleanupFailed", error)
            meta["group_cleanup"] = group.details
            meta["exit_status"] = proc.returncode
        active = None
        try:
            drain_end = time.monotonic() + 0.25
            while sel.get_map() and time.monotonic() < drain_end:
                for key, _event in sel.select(min(0.01, max(0, drain_end - time.monotonic()))):
                    capture(key)
        except (OSError, ValueError) as error:
            problem = problem or "CommandOutputDrainFailed"
            meta["drain_error_type"] = type(error).__name__
        meta["capture_complete"] = not bool(sel.get_map())
        if not meta["capture_complete"]:
            problem = problem or "CommandOutputIncomplete"
        if meta["truncated"]:
            problem = problem or "CommandOutputTruncated"
        try:
            sel.close()
            if proc is not None:
                proc.stdout.close()
                proc.stderr.close()
        except (OSError, ValueError) as error:
            problem = problem or "CommandPipeCloseFailed"
            meta["pipe_close_error_type"] = type(error).__name__
        for fd in logs:
            try:
                os.close(fd)
            except OSError as error:
                problem = "CommandAuditCloseFailed"
                meta["audit_close_error_type"] = type(error).__name__
        meta["elapsed_seconds"] = round(time.monotonic() - start, 6)
        meta["loss_status"] = problem or "complete"
        if problem:
            summary["capture_loss"] = True
        write_file(audit_fd, prefix + ".json", encoded(meta))
    if problem:
        inconclusive(problem)
    check(work)
    try:
        return meta["exit_status"], buffers[0].decode("utf-8"), buffers[1].decode("utf-8")
    except UnicodeError:
        inconclusive("NonUTF8Output")


def inventory(smi, env, ordinals, product):
    rc, text, err = run([smi, "--query-gpu=index,uuid,name,pci.bus_id,driver_version",
                         "--format=csv,noheader,nounits"], env)
    if rc != 0 or err.strip():
        inconclusive("GPUInventoryCommandFailed")
    rows = list(csv.reader(io.StringIO(text), skipinitialspace=True))
    if not rows or len(rows) > 64:
        inconclusive("GPUInventoryMissing")
    gpus = []
    for row in rows:
        if len(row) != 5:
            inconclusive("GPUInventoryMalformed")
        index, ident, name, bus, driver = (field.strip() for field in row)
        integer(index, "GPUIndex", 0)
        match = re.fullmatch(r"([0-9a-fA-F]{4,8}):([0-9a-fA-F]{2}):([0-9a-fA-F]{2})\.([0-7])", bus)
        if not re.fullmatch(r"GPU-[0-9a-fA-F]{8}(?:-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}", ident) or not match:
            inconclusive("GPUInventoryMalformed")
        if not name or len(name) > 256 or not re.fullmatch(r"[0-9]+(?:\.[0-9]+){1,3}", driver):
            inconclusive("GPUInventoryMalformed")
        if any(ord(c) < 32 or ord(c) == 127 for c in name):
            inconclusive("GPUInventoryMalformed")
        domain, pci_bus, device, function = (int(value, 16) for value in match.groups())
        if domain > 65535 or device > 31:
            inconclusive("GPUInventoryMalformed")
        packed = (domain << 20) | (pci_bus << 12) | (device << 4) | function
        gpus.append({"index": int(index), "uuid": ident, "name": name, "bus": bus,
                     "packed_bus": packed, "driver": driver})
    for field in ("index", "uuid", "packed_bus"):
        observed = {gpu[field].lower() if field == "uuid" else gpu[field] for gpu in gpus}
        if len(observed) != len(gpus):
            inconclusive("GPUInventoryDuplicate")
    gpus.sort(key=lambda gpu: gpu["packed_bus"])
    summary["visible_gpu_count"] = len(gpus)
    if max(ordinals) >= len(gpus):
        raise Outcome("Fail", "SelectedGPUAbsent")
    selected = [gpus[index] for index in ordinals]
    summary["selected_gpu_uuids"] = [gpu["uuid"] for gpu in selected]
    write_file(audit_fd, "selected-gpus.json", encoded(selected))
    code = ("import json,re,sys; p=re.compile(sys.argv[1]); "
            "print(json.dumps([p.fullmatch(n) is not None for n in json.loads(sys.argv[2])]))")
    rc, checked, err = run([sys.executable, "-I", "-B", "-c", code, product.pattern,
                            json.dumps([gpu["name"] for gpu in selected])], env)
    matches = json_load(checked)
    if rc != 0 or err.strip() or not isinstance(matches, list) or len(matches) != len(selected) or any(type(value) is not bool for value in matches):
        inconclusive("ProductGuardUnobservable")
    if not all(matches):
        raise Outcome("Fail", "GPUProductMismatch")
    return selected


CASES = {
    "host_to_device_memcpy_ce": ("GB/s", "host"),
    "device_to_host_memcpy_ce": ("GB/s", "host"),
    "device_to_device_memcpy_read_ce": ("GB/s", "peer"),
    "device_to_device_memcpy_write_ce": ("GB/s", "peer"),
    "host_device_latency_sm": ("ns", "host"),
    "device_to_device_latency_sm": ("ns", "peer"),
}


def parse_case(text, case, count):
    root = json_load(text)
    if not isinstance(root, dict) or not isinstance(root.get("nvbandwidth"), dict):
        inconclusive("UnknownNvbandwidthFormat")
    body = root["nvbandwidth"]
    revision = body.get("git_version", "unreported")
    if not isinstance(revision, str) or not revision.strip() or len(revision) > 256:
        inconclusive("InvalidToolRevision")
    tests = body.get("testcases")
    if not isinstance(tests, list) or len(tests) != 1 or not isinstance(tests[0], dict):
        inconclusive("RequestedCaseMissingOrExtra")
    test = tests[0]
    if test.get("name") != case:
        inconclusive("RequestedCaseMismatch")
    if any(part.get(key) for part in (root, body, test) for key in ("error", "errors", "error_message")):
        inconclusive("ConflictingNvbandwidthErrorEvidence")
    errors = test.get("verification_errors", 0)
    if isinstance(errors, bool) or not isinstance(errors, int) or errors < 0:
        inconclusive("InvalidVerificationCount")
    if test.get("status") != "Passed":
        inconclusive("CaseDidNotReportPassed")
    choices = [prefix for prefix in ("bandwidth", "latency")
               if prefix + "_matrix" in test or prefix + "_description" in test]
    if len(choices) != 1:
        inconclusive("AmbiguousOrMissingMetric")
    prefix = choices[0]
    description = test.get(prefix + "_description")
    unit, kind = CASES[case]
    metric = "bandwidth" if unit == "GB/s" else "latency"
    native_latency = {
        "host_device_latency_sm": "memory latency SM CPU(row) <-> GPU(column) (ns)",
        "device_to_device_latency_sm": "Device to Device Latency SM GPU(row) <-> GPU(column) (ns)",
    }
    if not isinstance(description, str) or (
        re.findall(r"\b(bandwidth|latency)\s*\(([^()]*)\)", description) != [(metric, unit)]
        and not (prefix == "bandwidth" and description == native_latency.get(case))
    ):
        inconclusive("MetricUnitMissingOrWrong")
    matrix = test.get(prefix + "_matrix")
    expected_rows = 1 if kind == "host" else count
    if not isinstance(matrix, list) or len(matrix) != expected_rows:
        inconclusive("MetricMatrixShapeMismatch")
    values = []
    for row_index, row in enumerate(matrix):
        if not isinstance(row, list) or len(row) != count:
            inconclusive("MetricMatrixShapeMismatch")
        for col_index, value in enumerate(row):
            if kind == "peer" and row_index == col_index:
                if value not in (None, "N/A"):
                    inconclusive("UnexpectedDiagonalMeasurement")
                continue
            if isinstance(value, bool) or not isinstance(value, (int, float, str)):
                inconclusive("MalformedMetricCell")
            if isinstance(value, str) and not re.fullmatch(r"(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?", value):
                inconclusive("MalformedMetricCell")
            try:
                number = float(value)
            except (ValueError, OverflowError):
                inconclusive("MalformedMetricCell")
            if not math.isfinite(number) or number < 0:
                inconclusive("NonfiniteOrNegativeMetric")
            values.append(number)
    if not values:
        inconclusive("MetricCellsMissing")
    return values, errors, revision


def remove_child(parent, name, expected=None):
    fd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent)
    try:
        if expected is not None and os.fstat(fd).st_ino != expected:
            inconclusive("ScratchIdentityChanged")
        for entry in os.listdir(fd):
            info = os.stat(entry, dir_fd=fd, follow_symlinks=False)
            if stat.S_ISDIR(info.st_mode):
                remove_child(fd, entry)
            else:
                os.unlink(entry, dir_fd=fd)
    finally:
        os.close(fd)
    os.rmdir(name, dir_fd=parent)


verdict, reason = "Inconclusive", "RunNotCompleted"
try:
    results_fd = directory(os.environ.get("PROJECT_SIGNAL_RESULTS_DIR", "/results"), True)
    if sys.version_info < (3, 9):
        inconclusive("PythonVersionUnsupported")
    if len(sys.argv) != 1:
        inconclusive("UnexpectedArguments")
    timeout = seconds("COMMAND_TIMEOUT_SECONDS")
    deadline = START + timeout
    duration = seconds("DURATION_SECONDS")
    iterations = integer(required("ITERATIONS"), "ITERATIONS", upper=10000)
    ordinals = [integer(value, "GPU_DEVICES", 0, 63) for value in unique_csv("GPU_DEVICES")]
    count = integer(required("EXPECTED_GPU_COUNT"), "EXPECTED_GPU_COUNT", upper=64)
    if count != len(ordinals):
        inconclusive("SelectedCountInputMismatch")
    pattern = required("EXPECTED_GPU_PRODUCT_REGEX")
    if len(pattern) > 512:
        inconclusive("InvalidProductRegex")
    try:
        product = re.compile(pattern)
    except re.error:
        inconclusive("InvalidProductRegex")
    cases = unique_csv("NVBANDWIDTH_CASES")
    if any(case not in CASES for case in cases):
        inconclusive("UnsupportedCase")
    if count < 2 and any(CASES[case][1] == "peer" for case in cases):
        inconclusive("PeerCasesNeedTwoGPUs")
    buffer_mib = integer(required("NVBANDWIDTH_BUFFER_MIB"), "NVBANDWIDTH_BUFFER_MIB")
    samples = integer(required("NVBANDWIDTH_SAMPLES"), "NVBANDWIDTH_SAMPLES", upper=1000000)
    limits = json_load(os.environ.get("NVBANDWIDTH_LIMITS_JSON", "{}"))
    if not isinstance(limits, dict) or any(case not in cases for case in limits):
        inconclusive("LimitCaseNotSelected")
    thresholds = {}
    for case, value in limits.items():
        key = "min_gbps" if CASES[case][0] == "GB/s" else "max_latency_ns"
        if not isinstance(value, dict) or set(value) != {key}:
            inconclusive("InvalidCaseLimit")
        thresholds[case] = positive(value[key])
    if 2 + iterations * len(cases) > 4096:
        inconclusive("CommandPlanTooLarge")
    smi = tool("NVIDIA_SMI_BIN", "nvidia-smi")
    bench = tool("NVBANDWIDTH_BIN", "nvbandwidth")
    scratch_root_fd = directory(required("SCRATCH_ROOT"))
    scratch_name = "project-signal-nvbandwidth-" + uuid.uuid4().hex
    os.mkdir(scratch_name, 0o700, dir_fd=scratch_root_fd)
    scratch_path = os.path.join(os.environ["SCRATCH_ROOT"], scratch_name)
    scratch_fd = os.open(scratch_name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=scratch_root_fd)
    audit_name = "audit-nvbandwidth-" + uuid.uuid4().hex
    os.mkdir(audit_name, 0o700, dir_fd=results_fd)
    audit_fd = os.open(audit_name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=results_fd)
    summary.update({"audit_directory": audit_name, "duration_budget_seconds": duration,
                    "overall_deadline_seconds": timeout, "iterations": iterations,
                    "buffer_mib": buffer_mib, "tool_samples": samples, "case_results": {}})
    selected = inventory(smi, child_environment([]), ordinals, product)
    env = child_environment(selected)
    phase_deadline = time.monotonic() + duration
    failures = set()
    revisions = set()
    for _iteration in range(iterations):
        for case in cases:
            rc, text, err = run([bench, "-j", "-t", case, "-b", str(buffer_mib), "-i", str(samples)], env, True)
            if err.strip():
                inconclusive("UnexpectedBenchmarkStderr")
            values, errors, revision = parse_case(text, case, count)
            if rc != 0 and errors == 0:
                inconclusive("BenchmarkNonzeroExit")
            revisions.add(revision)
            if len(revisions) != 1:
                inconclusive("ToolRevisionChanged")
            unit = CASES[case][0]
            worst = min(values) if unit == "GB/s" else max(values)
            row = summary["case_results"].setdefault(case, {"unit": unit, "completed_runs": 0,
                                                           "measured_cells": 0, "worst": worst,
                                                           "explicit_limit": thresholds.get(case)})
            row["completed_runs"] += 1
            row["measured_cells"] += len(values)
            row["worst"] = min(row["worst"], worst) if unit == "GB/s" else max(row["worst"], worst)
            if errors or any(value == 0 for value in values):
                failures.add("CorrectnessOrZeroMetric")
            if case in thresholds and ((unit == "GB/s" and worst < thresholds[case]) or
                                       (unit == "ns" and worst > thresholds[case])):
                failures.add("ExplicitCaseLimitViolated")
    check(True)
    summary["tool_revision"] = next(iter(revisions))
    summary["verification"] = "stock-default-enabled; every requested testcase reported Passed"
    summary["failures"] = sorted(failures)
    verdict, reason = ("Fail", "MeasuredCaseFailure") if failures else ("Pass", "SelectedCasesPassed")
except Outcome as outcome:
    verdict, reason = outcome.verdict, outcome.reason
except (OSError, ValueError, OverflowError, csv.Error, subprocess.SubprocessError) as error:
    verdict, reason = "Inconclusive", "UnexpectedRuntime_" + type(error).__name__
    summary["capture_loss"] = True
    notify(reason, error)
finally:
    if active is not None:
        try:
            stop_group(active)
        except (OSError, subprocess.SubprocessError) as error:
            verdict, reason = "Inconclusive", "ProcessCleanupFailed"
            summary["process_cleanup_error_type"] = type(error).__name__
            notify(reason, error)
    if scratch_name is not None and scratch_root_fd is not None:
        try:
            remove_child(scratch_root_fd, scratch_name, os.fstat(scratch_fd).st_ino if scratch_fd is not None else None)
        except (OSError, Outcome, RecursionError) as error:
            verdict, reason = "Inconclusive", "ScratchCleanupFailed"
            summary["scratch_cleanup_error_type"] = type(error).__name__
            notify(reason, error)
    if interrupted:
        verdict, reason = "Inconclusive", "Interrupted"
    summary.update({"verdict": verdict, "reason": reason, "elapsed_seconds": round(time.monotonic() - START, 6),
                    "interrupted_signal": interrupted})
    full_summary_saved = False
    if len(encoded(summary)) > LIMIT:
        verdict, reason = "Inconclusive", "MeasurementSummaryTooLarge"
        summary.update({"verdict": verdict, "reason": reason, "summary_truncated": True})
        if audit_fd is not None:
            try:
                write_file(audit_fd, "full-summary.json", encoded(summary))
                full_summary_saved = True
            except (OSError, ValueError, Outcome) as error:
                verdict, reason = "Inconclusive", "AuditFinalizationFailed"
                summary["audit_finalization_error_type"] = type(error).__name__
                summary["capture_loss"] = True
                notify(reason, error)
    for label, fd in (("scratch", scratch_fd), ("scratch-root", scratch_root_fd), ("audit", audit_fd)):
        if fd is not None:
            try:
                os.close(fd)
            except OSError as error:
                verdict, reason = "Inconclusive", "EvidenceCloseFailed"
                summary.setdefault("finalization_errors", []).append({"resource": label, "error_type": type(error).__name__})
                summary["capture_loss"] = True
                notify(reason, error)
    if interrupted:
        verdict, reason = "Inconclusive", "Interrupted"
    summary.update({"verdict": verdict, "reason": reason, "elapsed_seconds": round(time.monotonic() - START, 6)})
    try:
        if results_fd is None:
            raise OSError("results unavailable")
        data = encoded(summary)
        if len(data) > LIMIT:
            if verdict != "Inconclusive":
                verdict, reason = "Inconclusive", "MeasurementSummaryTooLarge"
            data = encoded({"verdict": verdict, "reason": reason, "detail_in_audit": full_summary_saved,
                            "audit_directory": summary.get("audit_directory"), "summary_truncated": True,
                            "finalization_errors": summary.get("finalization_errors", [])})
        publish(results_fd, verdict, reason, data)
    except (OSError, ValueError, Outcome) as error:
        verdict, reason = "Inconclusive", "ResultWriteFailed"
        notify(reason, error)
        if results_fd is not None:
            emergency(results_fd, reason)
    if results_fd is not None:
        try:
            os.close(results_fd)
        except OSError as error:
            verdict, reason = "Inconclusive", "ResultDirectoryCloseFailed"
            notify(reason, error)
            try:
                fresh = directory(os.environ.get("PROJECT_SIGNAL_RESULTS_DIR", "/results"))
                try:
                    emergency(fresh, reason)
                finally:
                    os.close(fresh)
            except (OSError, Outcome) as recovery_error:
                notify("ResultDirectoryCloseRecoveryFailed", recovery_error)
try:
    write_all(1, ("verdict=%s reason=%s\n" % (verdict, reason)).encode())
except OSError as error:
    verdict, reason = "Inconclusive", "StatusOutputFailed"
    notify(reason, error)
    try:
        fresh = directory(os.environ.get("PROJECT_SIGNAL_RESULTS_DIR", "/results"))
        try:
            emergency(fresh, reason)
        finally:
            os.close(fresh)
    except (OSError, Outcome) as recovery_error:
        notify("StatusOutputRecoveryFailed", recovery_error)
sys.exit(2 if verdict == "Inconclusive" else 0)
PY
