#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

if [[ $# == 1 && $1 == --describe ]]; then
  printf '%s\n' 'nccl-nvlink: first-party, single-host, one-process multi-GPU all-reduce on
direct local NVLink P2P paths. No MPI launcher or remote execution.
--describe does no work and needs only Bash. Normal invocation accepts no arguments;
missing configuration is Inconclusive, not Pass.

Required runtime: Bash, Python >=3.9 (stdlib) with functional POSIX waitid/WNOWAIT,
nvidia-smi with topo -m, a runtime
standard nccl-tests all_reduce_perf (v2-style checked 13-column output and parameter
header), CUDA/NCCL 2.x, full GPUs/driver device nodes and working NVLink peer access.
Bootstrap uses mkdir/chmod; checking pre-existing outputs also requires BSD/GNU stat.
No downloads, vendor suites, MPI, SSH, or arbitrary shell arguments.

Environment inputs (all required unless an explicit default is stated):
  SCRATCH_ROOT: absolute existing directory, no symlink component; only an owned
    private 0700 child is created/removed. Caller root/other children are preserved.
  PROJECT_SIGNAL_RESULTS_DIR: absolute directory, default /results; existing parents
    required, final directory may be created. No symlink components/result files.
  COMMAND_TIMEOUT_SECONDS: finite positive decimal seconds; overall wall deadline
    from Python clock initialization for ALL subprocesses (inventory/topology and
    isolated Python product-regex checking included).
  DURATION_SECONDS: finite positive decimal seconds; maximum benchmark-phase wall
    budget, NOT a minimum stress duration. Exactly ITERATIONS complete size sweeps.
    Benchmark deadline is min(overall deadline, phase start + DURATION_SECONDS).
    No new command starts at a deadline. Abort termination/drain/reap adds at most
    2 seconds per aborted command; bounded parsing/results/cleanup I/O is additional.
  ITERATIONS: integer 1..10000, repetitions of every selected message size.
  GPU_DEVICES: unique CSV visible ordinals 0..63, no whitespace or empty fields,
    into nvidia-smi full-GPU inventory sorted by numeric PCI bus ID.
    Inherited CUDA_VISIBLE_DEVICES is ignored; selected UUIDs define CUDA visibility.
  EXPECTED_GPU_COUNT: integer 2..64, exact SELECTED count and CSV length, not the
    count of unselected visible GPUs. MIG is unsupported.
  EXPECTED_GPU_PRODUCT_REGEX: Python regex (1..512 characters), fullmatch of EACH
    selected GPU name. No default GPU product/count/performance policy.
  NCCL_MESSAGE_SIZES_BYTES: unique CSV of 1..64 canonical integer byte sizes,
    each 4..9223372036854775807 and divisible by four (float32 element size).
  NCCL_TEST_ITERATIONS: integer 1..1000000, timed -n iterations per exact size.
  NCCL_WARMUP_ITERATIONS: integer 0..1000000, -w warmup iterations per size.
  NCCL_MIN_BUS_BANDWIDTH_GBPS_JSON: optional JSON object, default {}, canonical
    selected byte-size keys to finite positive GB/s floors. Decimal 10^9 bytes/s.
    Compare BOTH out-of-place and in-place busbw in EVERY size/repetition.
    No global floor, no inference from the aggregate Avg bus bandwidth line.
  NCCL_TESTS_BIN: optional absolute executable path, basename all_reduce_perf;
    default resolve all_reduce_perf on PATH. Not a suite wrapper.
  NVIDIA_SMI_BIN: optional absolute executable path, basename nvidia-smi;
    default resolve nvidia-smi on PATH.
  TOOL_LIBRARY_PATH: optional colon-separated absolute existing non-symlink library
    directories; default unset. Sole source of child LD_LIBRARY_PATH.
  PATH: only resolves python3 and unspecified executables. Child PATH is fixed
    system paths plus Python interpreter directory. No other inherited inputs:
    inherited NCCL_*, CUDA_*, LD_*, MPI/UCX/FI/transport variables are not passed.
    HOME/TMPDIR/TMP/TEMP are owned scratch; CUDA caching is disabled.

Exact per-size argv: all_reduce_perf -b SIZE -e SIZE -f 2 -g COUNT -t 1
  -n NCCL_TEST_ITERATIONS -w NCCL_WARMUP_ITERATIONS -c 1 -d float -o sum
  -r 0 -m 1 -z 0 -G 0 -N 1
Requires matching nThread/nGpus/minBytes/maxBytes/step/warmup/iters/agg/validation/
graph header, units, exactly one requested row, float/sum/root=-1/count=SIZE/4,
both finite timing/algbw/busbw/#wrong columns and final out-of-bounds/average lines.
Nonzero #wrong or out-of-bounds counts are Fail, including a nonzero tool exit
with complete typed correctness evidence. Any other nonzero exit is Inconclusive.

Fixed transport profile (not environment inputs): one collective channel,
Ring/Simple, one thread/process, no NVLS/CollNet/MNNVL, P2P restricted to NVL, SHM off.
Internal Socket over =lo is bootstrap-only; IB/plugins/tuner/profiler are disabled.
Require every selected off-diagonal nvidia-smi topo -m link to be NV[positive].
Require NCCL 2.x version, matching Init COMPLETE rank/cudaDev/PCI busId for ALL ranks,
and complete, connected per-channel ring edges via P2P/IPC, P2P/CUMEM, or
P2P/direct pointer (optional /read). NET, SHM, unknown, missing or conflicting
channel evidence is Inconclusive. Non-NVLink measured topology is Fail.
Actual coll-channel count must be one; all channel-0 directed ring edges are required.
NVLS_ENABLE alone is NEVER evidence of NVLink. Actual topology + actual P2P channel
evidence + completed checked collective are required. This is not a multi-node test.
NCCL global /etc/nccl.conf must be absent, empty or comments only; active system
configuration is Inconclusive. HOME and NCCL_CONF_FILE are isolated empty scratch.

Outputs: nonblank verdict, reason, measurements.txt (JSON <=16384 bytes), and
audit-nccl-nvlink-* (0700), with restricted 0600 per-command argv/exit/timing/capture
metadata and stdout/stderr. No environment dump. Limit 1 MiB per stream/command;
capture loss/truncation, timeout, signal, incomplete/malformed rows or missing path
observability are Inconclusive. Max 4096 commands, larger plans rejected.
Evidence closes are checked before verdict publication; result streams are staged
and closed before replacement. Audit/close/publication errors cannot publish Pass.
The leader remains unreaped through TERM/KILL; SIGCHLD is set to its default.
Post-reap cleanup only probes signal 0; missing waitid/WNOWAIT is Inconclusive.
Pass/Fail exit 0; Inconclusive exit 2. Explicit hardware/product/threshold/correctness
mismatch is Fail; absent input/tool/observability or unexpected runtime is
Inconclusive. Only selected sizes/GPUs are claimed; no implicit duration or floors.'
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
bootstrap_result RunNotCompleted || { printf '%s\n' 'Inconclusive: unsafe/unwritable results' >&2; exit 2; }
trap 'bootstrap_result PythonLaunchFailed || printf "%s\n" "Inconclusive: Python launch result publication failed" >&2' EXIT
exec python3 -I -B - nvlink "$@" <<'PY'
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
PROFILE = sys.argv[1]
CAP = 1048576
LIMIT = 16384
interrupted = 0
active = None
results_fd = audit_fd = scratch_root_fd = scratch_fd = None
scratch_name = scratch_path = None
deadline = phase_deadline = None
sequence = 0
summary = {"test": "nccl-" + PROFILE, "commands": 0, "capture_loss": False}


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


def child_environment(selected, backend, interface, hcas):
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
    if selected:
        env.update({"NCCL_DEBUG": "INFO", "NCCL_DEBUG_SUBSYS": "INIT,GRAPH,P2P,NET,COLL,TUNING",
                    "NCCL_CONF_FILE": os.path.join(scratch_path, "nccl.conf"),
                    "NCCL_ALGO": "Ring", "NCCL_PROTO": "Simple", "NCCL_NVLS_ENABLE": "0",
                    "NCCL_MIN_NCHANNELS": "1", "NCCL_MAX_NCHANNELS": "1",
                    "NCCL_COLLNET_ENABLE": "0", "NCCL_MNNVL_ENABLE": "0", "NCCL_NVB_DISABLE": "1",
                    "NCCL_SHM_DISABLE": "1", "NCCL_P2P_DISABLE": "0" if PROFILE == "nvlink" else "1",
                    "NCCL_P2P_LEVEL": "NVL", "NCCL_NET": backend, "NCCL_NET_PLUGIN": "none",
                    "NCCL_TUNER_PLUGIN": "none", "NCCL_PROFILER_PLUGIN": "none",
                    "NCCL_IB_DISABLE": "0" if backend == "IB" else "1",
                    "NCCL_SOCKET_IFNAME": "=" + interface,
                    "NCCL_CUMEM_ENABLE": "0", "NCCL_CUMEM_HOST_ENABLE": "0"})
        if backend == "IB":
            env["NCCL_IB_HCA"] = "=" + ",".join(hcas)
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


def topology(smi, env, selected):
    rc, text, err = run([smi, "topo", "-m"], env)
    if rc != 0 or err.strip():
        inconclusive("TopologyCommandFailed")
    headers = []
    rows = {}
    for line in text.splitlines():
        line = re.sub(r"\x1b\[[0-9;]*m", "", line)
        if any((ord(character) < 32 and character != "\t") or ord(character) == 127 for character in line):
            inconclusive("TopologyControlSequenceUnsupported")
        tokens = line.split()
        if not tokens:
            continue
        if not headers and tokens[0].startswith("GPU"):
            for token in tokens:
                if re.fullmatch(r"GPU[0-9]+", token):
                    headers.append(token)
                else:
                    break
            if len(headers) < 2 or len(set(headers)) != len(headers):
                inconclusive("TopologyHeaderMalformed")
        elif headers and re.fullmatch(r"GPU[0-9]+", tokens[0]):
            if tokens[0] in rows or len(tokens) < 1 + len(headers):
                inconclusive("TopologyRowsMalformed")
            rows[tokens[0]] = tokens[1:1 + len(headers)]
    names = ["GPU%d" % gpu["index"] for gpu in selected]
    if not headers or any(name not in headers or name not in rows for name in names):
        inconclusive("SelectedTopologyMissing")
    links = []
    for source in names:
        for destination in names:
            link = rows[source][headers.index(destination)]
            if source == destination:
                if link != "X":
                    inconclusive("TopologyDiagonalMalformed")
            elif not re.fullmatch(r"NV[1-9][0-9]*", link):
                if link in ("SYS", "NODE", "PHB", "PXB", "PIX", "SOC"):
                    raise Outcome("Fail", "SelectedTopologyNotNVLink")
                inconclusive("SelectedTopologyUnobservable")
            else:
                links.append(link)
    summary["topology"] = {"selected_directed_links": len(links), "link_types": sorted(set(links))}


def system_config():
    try:
        fd = os.open("/etc/nccl.conf", os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except FileNotFoundError:
        return
    with os.fdopen(fd, "rb") as stream:
        if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
            inconclusive("UnverifiableNCCLSystemConfig")
        data = stream.read(65537)
    if len(data) > 65536 or any(line.strip() and not line.lstrip().startswith(b"#") for line in data.splitlines()):
        inconclusive("ActiveNCCLSystemConfig")


NUMBER = r"(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?"
HEADER = re.compile(r"# nThread ([0-9]+) nGpus ([0-9]+) minBytes ([0-9]+) maxBytes ([0-9]+) "
                    r"step: ([0-9]+)\(factor\) warmup iters: ([0-9]+) iters: ([0-9]+) "
                    r"agg iters: ([0-9]+) validation: ([0-9]+) graph: ([0-9]+)")
INFO = re.compile(r"[^ \t]+:[0-9]+:[0-9]+ \[[0-9]+\] NCCL INFO (.+)")
INLINE_INFO = re.compile(r"[A-Za-z][A-Za-z0-9_.-]*:[0-9]+:[0-9]+ \[[0-9]+\] NCCL INFO (.+)")
CHANNEL = re.compile(r"Channel ([0-9]+)(?:/[0-9]+)? : ([0-9]+)\[([0-9a-fA-F]+)\] -> "
                     r"([0-9]+)\[([0-9a-fA-F]+)\](?: \[(send|receive)\])? via (.+)")
INIT = re.compile(r"\bcomm \S+ rank ([0-9]+) nranks ([0-9]+) cudaDev ([0-9]+)"
                  r"(?: nvmlDev ([0-9]+))? busId ([0-9a-fA-F]+)\b.*\bInit COMPLETE\b")


def metric(value, positive_time=False):
    if not re.fullmatch(NUMBER, value):
        inconclusive("MalformedNCCLMetric")
    number = float(value)
    if not math.isfinite(number) or number < 0 or (positive_time and number == 0):
        inconclusive("NonfiniteOrInvalidNCCLMetric")
    return number


def transport(messages, selected, backend, interface, hcas):
    count = len(selected)
    ranks = {}
    nvml_devices = {}
    channels = {}
    channel_devices = []
    networks = []
    devices = {}
    used_devices = set()
    versions = set()
    channel_counts = set()
    path_types = set()
    for message in messages:
        channel_count = re.fullmatch(r"([0-9]+) coll channels(?:,.*)?", message)
        if channel_count:
            channel_counts.add(int(channel_count.group(1)))
        version = re.fullmatch(r"NCCL version (2\.[0-9]+\.[0-9]+)(?:[+~][A-Za-z0-9_.+-]+)?", message)
        if version:
            versions.add(version.group(1))
        initialized = INIT.search(message)
        if initialized:
            rank, nranks, cuda, nvml, bus = initialized.groups()
            rank, nranks, cuda, bus = int(rank), int(nranks), int(cuda), int(bus, 16)
            if rank in ranks or nranks != count or rank >= count or cuda != rank or bus != selected[rank]["packed_bus"]:
                inconclusive("NCCLRankDeviceMismatch")
            ranks[rank] = bus
            if nvml is not None:
                nvml_devices[rank] = int(nvml)
        if message.startswith("Using network "):
            networks.append(message[len("Using network "):])
        if re.search(r"(?:Loaded|Using) (?:net|tuner|profiler) plugin", message, re.IGNORECASE):
            inconclusive("UnexpectedNCCLPlugin")
        if message.startswith("NET/Socket : Using "):
            if backend != "Socket":
                inconclusive("BackendFallbackObserved")
            listing = message[len("NET/Socket : Using "):]
            matches = list(re.finditer(r"\[([0-9]+)\]([A-Za-z0-9_.-]+):([^ \t]+)", listing))
            if not matches or " ".join(match.group(0) for match in matches) != listing:
                inconclusive("SocketDeviceEvidenceMalformed")
            for match in matches:
                index, name, _address = match.groups()
                if name != interface:
                    inconclusive("SocketInterfaceMismatch")
                if int(index) in devices and devices[int(index)] != name:
                    inconclusive("ConflictingNetworkDevice")
                devices[int(index)] = name
        if message.startswith("NET/IB : Using "):
            if backend != "IB":
                inconclusive("BackendFallbackObserved")
            match = re.fullmatch(r"NET/IB : Using (.+?)(?: \[RO\])?; OOB ([A-Za-z0-9_.-]+):\S+", message)
            if not match or match.group(2) != interface:
                inconclusive("IBDeviceOrBootstrapEvidenceMalformed")
            listing = match.group(1)
            entries = list(re.finditer(r"\[([0-9]+)\]([A-Za-z0-9_.-]+:[1-9][0-9]*)/(?:IB|RoCE)", listing))
            if not entries or " ".join(entry.group(0) for entry in entries) != listing:
                inconclusive("IBDeviceEvidenceMalformed")
            for entry in entries:
                index, name = entry.groups()
                if name not in hcas or (int(index) in devices and devices[int(index)] != name):
                    inconclusive("IBHCAMismatch")
                devices[int(index)] = name
        if "Channel " in message and " via " in message:
            edge = CHANNEL.fullmatch(message)
            if not edge:
                inconclusive("UnknownChannelEvidence")
            channel, source, source_bus, destination, destination_bus, direction, path = edge.groups()
            path_types.add(path)
            source, destination = int(source), int(destination)
            if source >= count or destination >= count or source == destination:
                inconclusive("UnexpectedChannelRank")
            channel_devices.append((source, source_bus, destination, destination_bus))
            if PROFILE == "nvlink":
                if direction is not None or not re.fullmatch(r"P2P/(?:IPC|CUMEM|direct pointer)(?:/read)?", path):
                    inconclusive("LocalNVLinkPathNotEstablished")
            else:
                path_match = re.fullmatch(r"NET/" + re.escape(backend) + r"/([0-9]+)(?:/(GDRDMA|GDRCOPY))?", path)
                if not path_match or direction is None or (backend == "Socket" and path_match.group(2)):
                    inconclusive("BackendFallbackOrUnknownPath")
                used_devices.add(int(path_match.group(1)))
            key = (source, destination)
            record = channels.setdefault(int(channel), {})
            record.setdefault(key, set()).add(direction or "p2p")
    if len(versions) != 1:
        inconclusive("NCCLVersionUnobservable")
    if set(ranks) != set(range(count)):
        inconclusive("NCCLRankInitializationMissing")
    # This pinned release logs decimal NVML IDs, linked to PCI identities by Init COMPLETE.
    nvml_channel_ids = versions == {"2.27.3"}
    if nvml_channel_ids and (set(nvml_devices) != set(ranks) or len(set(nvml_devices.values())) != count):
        inconclusive("NCCLRankDeviceMismatch")
    for source, source_id, destination, destination_id in channel_devices:
        if nvml_channel_ids:
            if not all(re.fullmatch(r"(?:0|[1-9][0-9]*)", value) for value in (source_id, destination_id)):
                inconclusive("ChannelDeviceMismatch")
            if int(source_id) != nvml_devices[source] or int(destination_id) != nvml_devices[destination]:
                inconclusive("ChannelDeviceMismatch")
        elif int(source_id, 16) != ranks[source] or int(destination_id, 16) != ranks[destination]:
            inconclusive("ChannelDeviceMismatch")
    if not networks or set(networks) != {backend}:
        inconclusive("SelectedNetworkNotEstablished")
    if channel_counts != {1} or set(channels) != {0}:
        inconclusive("CollectiveChannelCountOrEvidenceMissing")
    for edges in channels.values():
        if len(edges) != count or {source for source, _dest in edges} != set(range(count)) or {dest for _source, dest in edges} != set(range(count)):
            inconclusive("IncompleteCollectiveRing")
        visited = set()
        current = 0
        while current not in visited:
            visited.add(current)
            current = next(dest for source, dest in edges if source == current)
        if current != 0 or len(visited) != count:
            inconclusive("DisconnectedCollectiveRing")
        if PROFILE == "backend" and any(directions != {"send", "receive"} for directions in edges.values()):
            inconclusive("IncompleteNetworkDirectionEvidence")
    if PROFILE == "backend":
        if not used_devices or any(index not in devices for index in used_devices):
            inconclusive("SelectedNetworkDeviceUnobservable")
        expected = set(hcas) if backend == "IB" else {interface}
        if set(devices.values()) != expected or {devices[index] for index in used_devices} != expected:
            inconclusive("RequestedNetworkDevicesNotExercised")
    return {"nccl_version": next(iter(versions)), "channels": len(channels),
            "channel_device_identifier": "nvml-index" if nvml_channel_ids else "packed-pci",
            "transport": "NVLink-topology+P2P" if PROFILE == "nvlink" else "NET/" + backend,
            "channel_paths": sorted(path_types),
            "observed_network_devices": sorted({devices[index] for index in used_devices})}


def parse_collective(stdout, stderr, size, selected, test_iters, warmup, backend, interface, hcas):
    header = []
    row = []
    bounds = []
    averages = []
    messages = []
    column_headers = unit_headers = layouts = 0
    for line in (stdout + "\n" + stderr).splitlines():
        line = line.strip()
        if not line:
            continue
        if "NCCL WARN" in line or "NCCL ERROR" in line:
            inconclusive("NCCLRuntimeWarningOrError")
        info = INFO.fullmatch(line)
        if info:
            messages.append(info.group(1))
            continue
        info = INLINE_INFO.search(line)
        if info:
            messages.append(info.group(1))
            line = line[:info.start()].strip()
        version = re.fullmatch(r"NCCL version 2\.[0-9]+\.[0-9]+(?:[+~][A-Za-z0-9_.+-]+)?", line)
        if version:
            messages.append(line)
            continue
        normalized = " ".join(line.split())
        match = HEADER.fullmatch(normalized)
        if match:
            header.append(tuple(int(value) for value in match.groups()))
        elif normalized == "# size count type redop root time algbw busbw #wrong time algbw busbw #wrong":
            column_headers += 1
        elif normalized == "# (B) (elements) (us) (GB/s) (GB/s) (us) (GB/s) (GB/s)":
            unit_headers += 1
        elif normalized == "# out-of-place in-place":
            layouts += 1
        elif normalized.startswith("# Out of bounds values"):
            match = re.fullmatch(r"# Out of bounds values : ([0-9]+) (OK|FAILED)", normalized)
            if not match or ((int(match.group(1)) == 0) != (match.group(2) == "OK")):
                inconclusive("MalformedCorrectnessSummary")
            bounds.append(int(match.group(1)))
        elif normalized.startswith("# Avg bus bandwidth"):
            match = re.fullmatch(r"# Avg bus bandwidth : (" + NUMBER + ")", normalized)
            if not match:
                inconclusive("MalformedBandwidthSummary")
            averages.append(metric(match.group(1)))
        elif line.startswith("#"):
            continue
        elif re.match(r"[0-9]", line):
            row.extend(line.split())
        else:
            inconclusive("UnknownNCCLOutput")
    expected_header = (1, len(selected), size, size, 2, warmup, test_iters, 1, 1, 0)
    if header != [expected_header] or column_headers != 1 or unit_headers != 1 or layouts != 1:
        inconclusive("NCCLParametersOrUnitsUnverified")
    if len(row) != 13 or len(bounds) != 1 or len(averages) != 1:
        inconclusive("RequestedCollectiveRowOrSummaryMissing")
    if row[:5] != [str(size), str(size // 4), "float", "sum", "-1"]:
        inconclusive("RequestedCollectiveRowMismatch")
    out_time = metric(row[5], True)
    out_alg = metric(row[6])
    out_bus = metric(row[7])
    out_wrong = integer(row[8], "OutOfPlaceWrongCount", 0, 9223372036854775807)
    in_time = metric(row[9], True)
    in_alg = metric(row[10])
    in_bus = metric(row[11])
    in_wrong = integer(row[12], "InPlaceWrongCount", 0, 9223372036854775807)
    evidence = transport(messages, selected, backend, interface, hcas)
    return {"out_of_place_time_us": out_time, "in_place_time_us": in_time,
            "out_of_place_algbw_gbps": out_alg, "in_place_algbw_gbps": in_alg,
            "out_of_place_busbw_gbps": out_bus, "in_place_busbw_gbps": in_bus,
            "wrong_count": out_wrong + in_wrong, "out_of_bounds_count": bounds[0], "evidence": evidence}


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
    if len(sys.argv) != 2:
        inconclusive("UnexpectedArguments")
    timeout = seconds("COMMAND_TIMEOUT_SECONDS")
    deadline = START + timeout
    duration = seconds("DURATION_SECONDS")
    iterations = integer(required("ITERATIONS"), "ITERATIONS", upper=10000)
    ordinals = [integer(value, "GPU_DEVICES", 0, 63) for value in unique_csv("GPU_DEVICES")]
    count = integer(required("EXPECTED_GPU_COUNT"), "EXPECTED_GPU_COUNT", 2, 64)
    if count != len(ordinals):
        inconclusive("SelectedCountInputMismatch")
    pattern = required("EXPECTED_GPU_PRODUCT_REGEX")
    if len(pattern) > 512:
        inconclusive("InvalidProductRegex")
    try:
        product = re.compile(pattern)
    except re.error:
        inconclusive("InvalidProductRegex")
    sizes = [integer(value, "NCCL_MESSAGE_SIZES_BYTES", 4, 9223372036854775807)
             for value in unique_csv("NCCL_MESSAGE_SIZES_BYTES")]
    if any(size % 4 for size in sizes):
        inconclusive("MessageSizeNotFloatAligned")
    test_iters = integer(required("NCCL_TEST_ITERATIONS"), "NCCL_TEST_ITERATIONS", upper=1000000)
    warmup = integer(required("NCCL_WARMUP_ITERATIONS"), "NCCL_WARMUP_ITERATIONS", 0, 1000000)
    limits = json_load(os.environ.get("NCCL_MIN_BUS_BANDWIDTH_GBPS_JSON", "{}"))
    if not isinstance(limits, dict) or any(key not in {str(size) for size in sizes} for key in limits):
        inconclusive("BandwidthFloorSizeNotSelected")
    floors = {int(key): positive(value) for key, value in limits.items()}
    backend, interface, hcas = "Socket", "lo", []
    if PROFILE == "backend":
        backend = required("NCCL_BACKEND")
        interface = required("NCCL_SOCKET_INTERFACE")
        if backend not in ("IB", "Socket") or not re.fullmatch(r"[A-Za-z0-9_.-]{1,64}", interface):
            inconclusive("InvalidBackendOrInterface")
        if backend == "IB":
            hcas = unique_csv("NCCL_IB_HCAS")
            if any(not re.fullmatch(r"[A-Za-z0-9_.-]{1,64}:[1-9][0-9]{0,4}", hca) or int(hca.rsplit(":", 1)[1]) > 65535 for hca in hcas):
                inconclusive("InvalidIBHCASelection")
        elif "NCCL_IB_HCAS" in os.environ:
            inconclusive("IBInputWithSocketBackend")
    if (3 if PROFILE == "nvlink" else 2) + iterations * len(sizes) > 4096:
        inconclusive("CommandPlanTooLarge")
    smi = tool("NVIDIA_SMI_BIN", "nvidia-smi")
    bench = tool("NCCL_TESTS_BIN", "all_reduce_perf")
    system_config()
    scratch_root_fd = directory(required("SCRATCH_ROOT"))
    scratch_name = "project-signal-nccl-" + PROFILE + "-" + uuid.uuid4().hex
    os.mkdir(scratch_name, 0o700, dir_fd=scratch_root_fd)
    scratch_path = os.path.join(os.environ["SCRATCH_ROOT"], scratch_name)
    scratch_fd = os.open(scratch_name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=scratch_root_fd)
    write_file(scratch_fd, "nccl.conf", b"# deliberately empty private configuration\n")
    audit_name = "audit-nccl-" + PROFILE + "-" + uuid.uuid4().hex
    os.mkdir(audit_name, 0o700, dir_fd=results_fd)
    audit_fd = os.open(audit_name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=results_fd)
    summary.update({"audit_directory": audit_name, "duration_budget_seconds": duration,
                    "overall_deadline_seconds": timeout, "iterations": iterations,
                    "test_iterations": test_iters, "warmup_iterations": warmup,
                    "selected_backend": backend if PROFILE == "backend" else "local-NVLink-P2P",
                    "bootstrap_interface": interface, "selected_ib_hcas": hcas, "size_results": {}})
    selected = inventory(smi, child_environment([], backend, interface, hcas), ordinals, product)
    env = child_environment(selected, backend, interface, hcas)
    if PROFILE == "nvlink":
        topology(smi, env, selected)
    phase_deadline = time.monotonic() + duration
    failures = set()
    versions = set()
    for _iteration in range(iterations):
        for size in sizes:
            argv = [bench, "-b", str(size), "-e", str(size), "-f", "2", "-g", str(count), "-t", "1",
                    "-n", str(test_iters), "-w", str(warmup), "-c", "1", "-d", "float", "-o", "sum",
                    "-r", "0", "-m", "1", "-z", "0", "-G", "0", "-N", "1"]
            rc, stdout, stderr = run(argv, env, True)
            measured = parse_collective(stdout, stderr, size, selected, test_iters, warmup, backend, interface, hcas)
            if rc != 0 and measured["wrong_count"] == 0 and measured["out_of_bounds_count"] == 0:
                inconclusive("CollectiveNonzeroExit")
            versions.add(measured["evidence"]["nccl_version"])
            if len(versions) != 1:
                inconclusive("NCCLVersionChanged")
            worst = min(measured["out_of_place_busbw_gbps"], measured["in_place_busbw_gbps"])
            row = summary["size_results"].setdefault(str(size), {"completed_runs": 0, "worst_busbw_gbps": worst,
                                                                "wrong_count": 0, "out_of_bounds_count": 0,
                                                                "explicit_floor_gbps": floors.get(size),
                                                                "path_evidence": measured["evidence"]})
            row["completed_runs"] += 1
            row["worst_busbw_gbps"] = min(row["worst_busbw_gbps"], worst)
            row["wrong_count"] += measured["wrong_count"]
            row["out_of_bounds_count"] += measured["out_of_bounds_count"]
            row["path_evidence"]["channel_paths"] = sorted(set(row["path_evidence"]["channel_paths"]) |
                                                          set(measured["evidence"]["channel_paths"]))
            if measured["wrong_count"] or measured["out_of_bounds_count"]:
                failures.add("DataCorrectnessFailure")
            if size in floors and worst < floors[size]:
                failures.add("ExplicitMessageFloorViolated")
    check(True)
    summary["failures"] = sorted(failures)
    summary["data_checking"] = "enabled (-c 1); header, both #wrong fields and final bounds count validated"
    verdict, reason = ("Fail", "MeasuredCollectiveFailure") if failures else ("Pass", "SelectedCollectivesPassed")
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
