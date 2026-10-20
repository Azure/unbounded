#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

if [[ $# == 1 && $1 == --describe ]]; then
  printf '%s\n' \
    'softmax-determinism: first-party repeated identical-input CUDA softmax, full correctness and bitwise determinism.' \
    'Tools: Bash, python3 POSIX waitid/WNOWAIT supervisor, installed PyTorch CUDA; no downloads or opaque suites.' \
    'Devices: GPU_DEVICES is a strict CSV of distinct visible CUDA ordinals; CUDA visibility is respected.' \
    'Required inputs (no defaults): SCRATCH_ROOT, COMMAND_TIMEOUT_SECONDS, DURATION_SECONDS, ITERATIONS,' \
    'GPU_DEVICES, EXPECTED_GPU_COUNT, EXPECTED_GPU_PRODUCT_REGEX, MEMORY_BUDGET_MIB, WORKSPACE_MIB, INPUT_SEED,' \
    'SOFTMAX_ROWS, SOFTMAX_COLUMNS, SOFTMAX_DTYPES, ABS_TOLERANCE, REL_TOLERANCE.' \
    'Optional inputs: PROJECT_SIGNAL_RESULTS_DIR (operational default /results); CUDA_VISIBLE_DEVICES' \
    '(inherited CUDA runtime visibility, unchanged by this script).' \
    'SCRATCH_ROOT: absolute existing directory with no symlink components; only a private child is removed.' \
    'Results path: absolute, no symlink components or symlink/nonregular/shared-hardlink output files.' \
    'EXPECTED_GPU_COUNT equals assigned ordinal count; product regex uses Python fullmatch.' \
    'SOFTMAX_DTYPES is a strict CSV of distinct names from float16,bfloat16,float32,float64; no dtype default.' \
    'Integer inputs use decimal digits and fit sys.maxsize. Dimensions/count/budget are positive, ITERATIONS>=2,' \
    'INPUT_SEED and WORKSPACE_MIB nonnegative; tolerances explicit finite nonnegative numbers; duration/timeout' \
    'explicit finite positive seconds. Empty/malformed/duplicate CSV entries and unsupported APIs are Inconclusive.' \
    'ITERATIONS is a maximum number of complete all-device/all-dtype sweeps. DURATION_SECONDS is the maximum' \
    'window for starting new sweeps, not a minimum stress duration; the last sweep may overrun that window.' \
    'At least two full sweeps are necessary for Pass; a shorter duration that prevents repeats is Inconclusive.' \
    'COMMAND_TIMEOUT_SECONDS bounds the entire workload process, including imports/reference/setup and loops.' \
    'Completion/expiry/signals terminate only its owned group, including descendants of an exited leader;' \
    'termination/reap/disappearance verification has at most two seconds grace.' \
    'Expected duration: setup/reference plus sweeps admitted by both limits, bounded by the tool deadline.' \
    'Every assigned GPU and declared dtype is checked every iteration. Index/seed logits remain unchanged.' \
    'For each dtype, full output bytes must equal the first assigned GPU first-iteration baseline across GPUs' \
    'and repeats. Different dtypes are not compared bitwise to one another. Any mismatch is Fail, never tolerated.' \
    'Independent reference is single-threaded CPU FP64 exp(x-row_max)/sum(exp(x-row_max)) on quantized logits.' \
    'Inputs/reference/output must be finite; output probabilities must be in [0,1]; full elementwise correctness' \
    'requires abs(error)<=ABS_TOLERANCE+REL_TOLERANCE*abs(reference). These tolerances do not relax determinism.' \
    'CUDA events record softmax-only seconds/elements-per-second; no throughput floor, warmup or calibration.' \
    'Event time includes cold first-call dispatch/library costs; all samples are retained in the audit log.' \
    'Allocation plan: E=ROWS*COLUMNS, s=dtype bytes; host tensors <=sum((96+4*s)*E), each GPU <=sum(2*s*E).' \
    'WORKSPACE_MIB is an explicit extra reserve per host/GPU; MEMORY_BUDGET_MIB caps their combined total.' \
    'Plans must fit host memory (capped by detected cgroup headroom) and each GPU free memory; allocator peaks checked.' \
    'Outputs: verdict (Pass|Fail|Inconclusive), reason, nonblank measurements.txt <=16384 bytes and restricted' \
    'softmax-determinism-*.log audit records in the results directory, without environment dumps.' \
    'Audit write/flush/close is checked before verdict publication; evidence-finalization errors are Inconclusive.' \
    'Numerical/determinism/product mismatches: Fail exit 0; missing inputs/tools/devices, memory limits, unsupported' \
    'dtype/API, runtime error, signal or timeout: Inconclusive exit 2. Normal invocation accepts no arguments.'
  exit 0
fi

# This path also works when python3 is absent or cannot start.
fallback() (
  trap - ERR INT TERM HUP
  local reason=$1 directory=${PROJECT_SIGNAL_RESULTS_DIR-/results} part prefix=/
  local stage leaf text token="${BASHPID:-$$}-${RANDOM}" failed=0 owned
  local -a parts=()
  if [[ $directory != /* || $directory == *$'\n'* || $directory == *$'\r'* ||
        $directory == *'//'* || $directory == */./* || $directory == */../* ||
        $directory == */. || $directory == */.. ]]; then
    printf 'Inconclusive: unsafe results directory\n' >&2
    exit 2
  fi
  IFS=/ read -r -a parts <<< "${directory#/}"
  for part in "${parts[@]}"; do
    [[ -z $part ]] && continue
    prefix="${prefix%/}/$part"
    if [[ -L $prefix || ( -e $prefix && ! -d $prefix ) ]]; then
      printf 'Inconclusive: unsafe results directory\n' >&2
      exit 2
    fi
  done
  if [[ ! -d $directory ]] && ! command mkdir -m 700 -p -- "$directory"; then
    printf 'Inconclusive: results directory unavailable\n' >&2
    exit 2
  fi
  cd -P -- "$directory" || exit 2
  for leaf in verdict reason measurements.txt "softmax-determinism-${token}.log"; do
    if [[ -L $leaf || ( -e $leaf && ( ! -f $leaf || ! -O $leaf ) ) ]]; then
      printf 'Inconclusive: unsafe output %s\n' "$leaf" >&2
      failed=1
      continue
    fi
    case $leaf in
      verdict) text=Inconclusive ;;
      reason) text=$reason ;;
      *) text="test=softmax-determinism verdict=Inconclusive reason=$reason; workload completion not certified" ;;
    esac
    if [[ ! -e $leaf && ! -L $leaf ]]; then
      if ( set -C; printf '%s\n' "$text" > "$leaf" ); then
        continue
      fi
      printf 'Inconclusive: could not create %s\n' "$leaf" >&2
      failed=1
      continue
    fi
    stage=".softmax-determinism-${token}-${leaf}"
    owned=0
    if ( set -C; printf '%s\n' "$text" > "$stage" ); then
      owned=1
    fi
    if (( owned )) && [[ ! -L $leaf && ! -d $leaf ]] && command mv -f -- "$stage" "$leaf"; then
      :
    else
      printf 'Inconclusive: could not publish %s\n' "$leaf" >&2
      if (( owned )); then command rm -f -- "$stage" || :; fi
      failed=1
    fi
  done
  printf 'softmax-determinism: Inconclusive %s (publication_errors=%s)\n' "$reason" "$failed" >&2
  exit 2
)

if (( $# != 0 )); then
  fallback InvalidArguments || :
  exit 2
fi
if ! command -v python3 >/dev/null 2>&1; then
  fallback Python3Missing || :
  exit 2
fi

runner_pid=
interrupted=
run_token="${BASHPID:-$$}-$RANDOM-$RANDOM"
trap 'interrupted=INT; if [[ -n $runner_pid ]]; then kill -TERM "$runner_pid" 2>/dev/null || :; fi' INT
trap 'interrupted=TERM; if [[ -n $runner_pid ]]; then kill -TERM "$runner_pid" 2>/dev/null || :; fi' TERM
trap 'interrupted=HUP; if [[ -n $runner_pid ]]; then kill -TERM "$runner_pid" 2>/dev/null || :; fi' HUP

python3 - "$run_token" <<'PY' &
import errno
import json
import math
import os
import re
import secrets
import signal
import stat
import subprocess
import sys
import time
import traceback

TEST = "softmax-determinism"
LIMIT = 16384
MIB = 1024 * 1024
received_signal = None


def interrupted(signum, _frame):
    global received_signal
    received_signal = signum


for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
    signal.signal(sig, interrupted)


class Inconclusive(Exception):
    pass


def prepare_child_supervision(audit):
    if any(not hasattr(os, name) for name in ("waitid", "P_PID", "WEXITED", "WNOHANG", "WNOWAIT", "CLD_EXITED")) or not hasattr(signal, "SIGCHLD"):
        raise Inconclusive("NonReapingWaitAPIUnavailable")
    try:
        inherited_ignore = signal.getsignal(signal.SIGCHLD) == signal.SIG_IGN
        signal.signal(signal.SIGCHLD, signal.SIG_DFL)
        if signal.getsignal(signal.SIGCHLD) != signal.SIG_DFL:
            raise Inconclusive("SIGCHLDSupervisionUnavailable")
    except (OSError, ValueError) as exc:
        raise Inconclusive("SIGCHLDSupervisionUnavailable") from exc
    try:
        # Waiting on self cannot reap a child; ECHILD verifies the call and flags.
        os.waitid(os.P_PID, os.getpid(), os.WEXITED | os.WNOHANG | os.WNOWAIT)
    except OSError as exc:
        if exc.errno != errno.ECHILD:
            raise Inconclusive("NonReapingWaitAPIUnavailable") from exc
    except (NotImplementedError, TypeError, ValueError) as exc:
        raise Inconclusive("NonReapingWaitAPIUnavailable") from exc
    else:
        raise Inconclusive("NonReapingWaitReadinessUnverified")
    audit.write(json.dumps({"event": "child_supervision_ready", "inherited_sigchld_ignored": inherited_ignore,
                            "sigchld_default": True, "waitid_probe": "self_pid_ECHILD"}, sort_keys=True) + "\n")


def directory_fd(path, create=False):
    if not path or not path.startswith("/") or "\n" in path or "\r" in path:
        raise Inconclusive("UnsafeDirectory")
    parts = path.split("/")[1:]
    if any(p in (".", "..") for p in parts) or any(not p for p in parts[:-1]):
        raise Inconclusive("UnsafeDirectory")
    fd = os.open("/", os.O_RDONLY | os.O_DIRECTORY)
    completed = False
    try:
        for part in parts:
            if not part:
                continue
            if create:
                try:
                    os.mkdir(part, 0o700, dir_fd=fd)
                except FileExistsError:
                    pass
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            previous, fd = fd, child
            os.close(previous)
        completed = True
        return fd
    finally:
        if not completed:
            os.close(fd)


def safe_output(fd, name):
    try:
        info = os.stat(name, dir_fd=fd, follow_symlinks=False)
    except FileNotFoundError:
        return
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid != os.geteuid():
        raise Inconclusive("UnsafeOutput:" + name)


def publish(fd, name, text):
    safe_output(fd, name)
    stage = "." + TEST + "-" + secrets.token_hex(12)
    handle = os.open(stage, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=fd)
    try:
        with os.fdopen(handle, "w", encoding="utf-8") as stream:
            stream.write(text + "\n")
            stream.flush()
        safe_output(fd, name)
        os.replace(stage, name, src_dir_fd=fd, dst_dir_fd=fd)
    finally:
        try:
            os.unlink(stage, dir_fd=fd)
        except FileNotFoundError:
            pass


def integer(name, minimum=1):
    value = os.environ.get(name, "")
    if re.fullmatch(r"[0-9]+", value) is None:
        raise Inconclusive("InvalidInput:" + name)
    significant = value.lstrip("0") or "0"
    maximum = str(sys.maxsize)
    if len(significant) > len(maximum) or (len(significant) == len(maximum) and significant > maximum):
        raise Inconclusive("IntegerOverflow:" + name)
    result = int(significant)
    if result < minimum:
        raise Inconclusive("InvalidInput:" + name)
    return result


def number(name, minimum=0.0, strict=False):
    value = os.environ.get(name, "")
    if re.fullmatch(r"\+?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?", value) is None:
        raise Inconclusive("InvalidInput:" + name)
    result = float(value)
    if not math.isfinite(result) or result < minimum or (strict and result == minimum):
        raise Inconclusive("InvalidInput:" + name)
    return result


def stop_owned_group(process, audit):
    deadline = time.monotonic() + 2.0
    transient_errors = []
    fatal_errors = []
    diagnostics = []
    # The leader has not been reaped: its PID pins this owned group against reuse.
    for sig in (signal.SIGTERM, signal.SIGKILL):
        try:
            os.killpg(process.pid, sig)
        except ProcessLookupError:
            break
        except PermissionError as exc:
            transient_errors.append(exc)
            diagnostics.append(json.dumps({"event": "cleanup_signal_error", "signal": int(sig), "errno": exc.errno}) + "\n")
        except OSError as exc:
            fatal_errors.append(exc)
            diagnostics.append(traceback.format_exc())
        if sig == signal.SIGTERM:
            time.sleep(min(0.05, max(0.0, deadline - time.monotonic())))
    try:
        process.wait(timeout=max(0.0, deadline - time.monotonic()))
    except (OSError, subprocess.TimeoutExpired) as exc:
        fatal_errors.append(exc)
        diagnostics.append(traceback.format_exc())
    gone = False
    last_probe_error = None
    while True:
        try:
            os.killpg(process.pid, 0)
        except ProcessLookupError:
            gone = True
            break
        except PermissionError as exc:
            last_probe_error = exc
        except OSError as exc:
            fatal_errors.append(exc)
            diagnostics.append(traceback.format_exc())
            break
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            break
        time.sleep(min(0.02, remaining))
    for diagnostic in diagnostics:
        audit.write(diagnostic)
    if fatal_errors or not gone:
        cause = (fatal_errors or transient_errors or [last_probe_error])[-1]
        raise Inconclusive("OwnedProcessGroupCleanupFailed") from cause
    if transient_errors or last_probe_error is not None:
        audit.write(json.dumps({"event": "cleanup_transient_error_verified_disappeared", "pgid": process.pid}) + "\n")
    audit.write(json.dumps({"event": "owned_group_cleanup", "pgid": process.pid,
                           "group_disappeared": True, "returncode": process.returncode}) + "\n")


WORKER = r'''
import json
import math
import os
import re
import sys
import time
import traceback

cfg = json.load(sys.stdin)
report = {"verdict": "Inconclusive", "reason": "WorkloadNotCompleted", "completed_iterations": 0}
stats = []


class Inconclusive(Exception):
    pass


class Failure(Exception):
    pass


def event(kind, **values):
    print(json.dumps({"event": kind, **values}, sort_keys=True, allow_nan=False), flush=True)


def finite(tensor):
    return bool(torch.isfinite(tensor).all().item())


def host_available():
    if sys.platform.startswith("linux"):
        available = None
        with open("/proc/meminfo", encoding="ascii") as stream:
            for line in stream:
                if line.startswith("MemAvailable:"):
                    available = int(line.split()[1]) * 1024
                    break
        if available is None:
            raise Inconclusive("HostAvailableMemoryUnknown")
        for limit_path, usage_path in (
            ("/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory.current"),
            ("/sys/fs/cgroup/memory/memory.limit_in_bytes", "/sys/fs/cgroup/memory/memory.usage_in_bytes"),
        ):
            try:
                with open(limit_path, encoding="ascii") as stream:
                    limit = stream.read().strip()
            except FileNotFoundError:
                continue
            if limit == "max":
                continue
            with open(usage_path, encoding="ascii") as stream:
                used = int(stream.read().strip())
            available = min(available, max(0, int(limit) - used))
        return available
    try:
        return int(os.sysconf("SC_AVPHYS_PAGES")) * int(os.sysconf("SC_PAGE_SIZE"))
    except (ValueError, OSError) as exc:
        raise Inconclusive("HostAvailableMemoryUnknown") from exc


def peak_check(device):
    peak = int(torch.cuda.max_memory_reserved(device))
    if peak > cfg["gpu_plan_bytes"]:
        raise Inconclusive("ObservedCUDAAllocationExceedsPlan")
    return peak


def compare(actual, reference):
    if not finite(actual):
        return False, None, None
    difference = (actual - reference).abs()
    bound = cfg["abs_tolerance"] + cfg["rel_tolerance"] * reference.abs()
    if not finite(difference) or not finite(bound):
        raise Inconclusive("ComparisonArithmeticNonfinite")
    probability_ok = bool(((actual >= 0.0) & (actual <= 1.0)).all().item())
    return probability_ok and bool((difference <= bound).all().item()), float(difference.max().item()), float(bound.max().item())


def pattern(dtype):
    index = torch.arange(cfg["rows"] * cfg["columns"], dtype=torch.int64)
    values = ((index.remainder(251) + cfg["seed"] % 251).remainder(251) - 125).to(torch.float64) / 16.0
    return values.reshape(cfg["rows"], cfg["columns"]).to(dtype)


try:
    import torch

    event("runtime", torch_version=str(torch.__version__), workload="same-dtype CUDA torch.softmax",
          reference="CPU FP64 stable exp/sum, one thread", determinism="exact output bytes across repeats and devices")
    if not torch.cuda.is_available():
        raise Inconclusive("CUDAUnavailable")
    visible = int(torch.cuda.device_count())
    if any(device >= visible for device in cfg["devices"]):
        raise Inconclusive("AssignedDeviceUnavailable")
    torch.set_num_threads(1)
    torch.set_num_interop_threads(1)
    dtypes = {name: getattr(torch, name) for name in cfg["dtypes"]}
    for device in cfg["devices"]:
        product = str(torch.cuda.get_device_properties(device).name)
        event("identity", device=device, product=product)
        if re.fullmatch(cfg["product_regex"], product) is None:
            raise Failure("GPUProductMismatch")
        if "bfloat16" in dtypes:
            with torch.cuda.device(device):
                if not torch.cuda.is_bf16_supported(including_emulation=False):
                    raise Inconclusive("NativeBF16Unavailable")
        free, total = (int(x) for x in torch.cuda.mem_get_info(device))
        event("memory_preflight", device=device, free_bytes=free, total_bytes=total, plan_bytes=cfg["gpu_plan_bytes"])
        if cfg["gpu_plan_bytes"] > free:
            raise Inconclusive("InsufficientFreeGPUMemory")
        torch.cuda.reset_peak_memory_stats(device)
    available = host_available()
    event("host_memory_preflight", available_bytes=available, plan_bytes=cfg["host_plan_bytes"])
    if cfg["host_plan_bytes"] > available:
        raise Inconclusive("InsufficientAvailableHostMemory")

    references = {}
    for name, dtype in dtypes.items():
        inputs = pattern(dtype)
        values = inputs.to(torch.float64)
        if not finite(values):
            raise Inconclusive("NonfiniteGeneratedInput")
        shifted = values - values.amax(dim=-1, keepdim=True)
        weights = torch.exp(shifted)
        reference = weights / weights.sum(dim=-1, keepdim=True)
        if not finite(reference):
            raise Inconclusive("NonfiniteCPUReference")
        references[name] = (inputs, values, reference)
        del shifted, weights
    states = []
    for device in cfg["devices"]:
        with torch.cuda.device(device):
            for name, dtype in dtypes.items():
                target = torch.device("cuda", device)
                inputs = references[name][0].to(device=target)
                output = torch.empty((cfg["rows"], cfg["columns"]), dtype=dtype, device=target)
                torch.cuda.synchronize(device)
                peak_check(device)
                states.append((device, name, inputs, output))
                stats.append({"device": device, "dtype": name, "samples": 0,
                              "determinism_comparisons": 0, "max_abs_error": 0.0})
    baselines = {}
    first_failure = None
    failures = 0
    started = time.monotonic()
    for iteration in range(1, cfg["iterations"] + 1):
        if iteration > 1 and time.monotonic() - started >= cfg["duration"]:
            break
        for state, summary in zip(states, stats):
            device, name, inputs, output = state
            _, input_reference, reference = references[name]
            with torch.cuda.device(device):
                check = inputs.cpu().to(torch.float64)
                inputs_ok = finite(check) and torch.equal(check, input_reference)
                del check
                torch.cuda.synchronize(device)
                begin, end = torch.cuda.Event(enable_timing=True), torch.cuda.Event(enable_timing=True)
                begin.record()
                torch.softmax(inputs, dim=-1, out=output)
                end.record()
                end.synchronize()
                seconds = float(begin.elapsed_time(end)) / 1000.0
                if not math.isfinite(seconds) or seconds <= 0:
                    raise Inconclusive("NonpositiveOrNonfiniteCUDAElapsedTime")
                elements_per_second = cfg["rows"] * cfg["columns"] / seconds
                if not math.isfinite(elements_per_second) or elements_per_second <= 0:
                    raise Inconclusive("NonpositiveOrNonfiniteThroughput")
                actual_host = output.cpu()
                actual = actual_host.to(torch.float64)
                correct, error, bound = compare(actual, reference)
                bits = actual_host.contiguous().view(torch.uint8)
                baseline_created = name not in baselines
                if baseline_created:
                    baselines[name] = bits.clone()
                else:
                    summary["determinism_comparisons"] += 1
                deterministic = bool(torch.equal(bits, baselines[name]))
                peak = peak_check(device)
                summary["samples"] += 1
                if error is not None:
                    summary["max_abs_error"] = max(summary["max_abs_error"], error)
                event("sample", iteration=iteration, device=device, dtype=name, inputs_ok=bool(inputs_ok),
                      correct=correct, deterministic=deterministic, baseline_created=baseline_created,
                      baseline_device=cfg["devices"][0], max_abs_error=error, maximum_allowed_error=bound,
                      softmax_seconds=seconds, elements_per_second=elements_per_second, cuda_peak_reserved_bytes=peak)
                if not inputs_ok or not correct or not deterministic:
                    failures += 1
                    first_failure = first_failure or ("InputMismatch" if not inputs_ok else "SoftmaxCorrectnessMismatch" if not correct else "SoftmaxDeterminismMismatch")
                del actual_host, actual, bits
        report["completed_iterations"] = iteration
        if first_failure:
            break
    if report["completed_iterations"] < 1 or any(s["samples"] != report["completed_iterations"] for s in stats):
        raise Inconclusive("IncompleteAllDeviceDtypeSweep")
    if not first_failure and report["completed_iterations"] < 2:
        raise Inconclusive("DurationWindowPreventedRepeatedSweeps")
    report.update(verdict="Fail" if first_failure else "Pass", reason=first_failure or "SoftmaxDeterminismAndCorrectnessPassed",
                  loop_seconds=time.monotonic() - started, mismatched_samples=failures)
except Failure as exc:
    report.update(verdict="Fail", reason=str(exc))
except (Inconclusive, ImportError) as exc:
    traceback.print_exc()
    report.update(verdict="Inconclusive", reason=str(exc) if isinstance(exc, Inconclusive) else "PyTorchDependencyUnavailable")
except (OSError, RuntimeError, TypeError, ValueError, AttributeError, OverflowError, MemoryError, AssertionError):
    traceback.print_exc()
    report.update(verdict="Inconclusive", reason="WorkloadRuntimeError")
finally:
    report["devices"] = stats
    event("outcome", **report)
    fd = os.open(cfg["report_path"], os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as stream:
        json.dump(report, stream, sort_keys=True, allow_nan=False)
sys.exit(2 if report["verdict"] == "Inconclusive" else 0)
'''

result_fd = scratch_fd = None
scratch_name = None
scratch_root = None
audit = None
audit_name = None
worker = None
outcome = {"verdict": "Inconclusive", "reason": "SupervisorNotCompleted"}
cfg = {}
try:
    result_fd = directory_fd(os.environ.get("PROJECT_SIGNAL_RESULTS_DIR", "/results"), create=True)
    audit_name = TEST + "-" + secrets.token_hex(12) + ".log"
    handle = os.open(audit_name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=result_fd)
    audit = os.fdopen(handle, "w", encoding="utf-8", buffering=1)
    for name in ("verdict", "reason", "measurements.txt"):
        safe_output(result_fd, name)
    cfg = {
        "duration": number("DURATION_SECONDS", strict=True),
        "timeout": number("COMMAND_TIMEOUT_SECONDS", strict=True),
        "iterations": integer("ITERATIONS", minimum=2),
        "expected_count": integer("EXPECTED_GPU_COUNT"),
        "rows": integer("SOFTMAX_ROWS"), "columns": integer("SOFTMAX_COLUMNS"),
        "seed": integer("INPUT_SEED", minimum=0),
        "abs_tolerance": number("ABS_TOLERANCE"), "rel_tolerance": number("REL_TOLERANCE"),
        "budget_bytes": integer("MEMORY_BUDGET_MIB") * MIB,
        "workspace_bytes": integer("WORKSPACE_MIB", minimum=0) * MIB,
    }
    sizes = {"float16": 2, "bfloat16": 2, "float32": 4, "float64": 8}
    cfg["dtypes"] = os.environ.get("SOFTMAX_DTYPES", "").split(",")
    if any(name not in sizes for name in cfg["dtypes"]) or len(set(cfg["dtypes"])) != len(cfg["dtypes"]):
        raise Inconclusive("InvalidInput:SOFTMAX_DTYPES")
    devices = os.environ.get("GPU_DEVICES", "").split(",")
    if any(re.fullmatch(r"[0-9]+", device) is None or len(device) > 19 for device in devices):
        raise Inconclusive("InvalidInput:GPU_DEVICES")
    cfg["devices"] = [int(device) for device in devices]
    if len(set(cfg["devices"])) != len(devices) or len(devices) != cfg["expected_count"] or any(d > sys.maxsize for d in cfg["devices"]):
        raise Inconclusive("InvalidInput:GPU_DEVICES")
    cfg["product_regex"] = os.environ.get("EXPECTED_GPU_PRODUCT_REGEX", "")
    if not cfg["product_regex"]:
        raise Inconclusive("MissingInput:EXPECTED_GPU_PRODUCT_REGEX")
    try:
        re.compile(cfg["product_regex"])
    except re.error as exc:
        raise Inconclusive("InvalidInput:EXPECTED_GPU_PRODUCT_REGEX") from exc
    elements = cfg["rows"] * cfg["columns"]
    cfg["host_plan_bytes"] = sum((96 + 4 * sizes[name]) * elements for name in cfg["dtypes"]) + cfg["workspace_bytes"]
    cfg["gpu_plan_bytes"] = sum(2 * sizes[name] * elements for name in cfg["dtypes"]) + cfg["workspace_bytes"]
    total_plan = cfg["host_plan_bytes"] + len(devices) * cfg["gpu_plan_bytes"]
    if max(total_plan, cfg["budget_bytes"]) > sys.maxsize:
        raise Inconclusive("AllocationPlanOverflow")
    if total_plan > cfg["budget_bytes"]:
        raise Inconclusive("AllocationPlanExceedsMemoryBudget")
    prepare_child_supervision(audit)
    scratch_root = os.environ.get("SCRATCH_ROOT", "")
    scratch_fd = directory_fd(scratch_root)
    candidate = "." + TEST + "-" + secrets.token_hex(12)
    os.mkdir(candidate, 0o700, dir_fd=scratch_fd)
    scratch_name = candidate
    scratch_path = os.path.join(scratch_root, scratch_name)
    cfg["report_path"] = os.path.join(scratch_path, "outcome.json")
    audit.write(json.dumps({"event": "configuration", "test": TEST, **cfg}, sort_keys=True, allow_nan=False) + "\n")
    if received_signal is not None:
        raise Inconclusive("Interrupted")
    config_path = os.path.join(scratch_path, "configuration.json")
    handle = os.open(config_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(handle, "w", encoding="utf-8") as stream:
        json.dump(cfg, stream, allow_nan=False)
    with open(config_path, "rb") as configuration:
        started = time.monotonic()
        environment = os.environ.copy()
        environment.update(TMPDIR=scratch_path, TEMP=scratch_path, TMP=scratch_path,
                           PYTHONDONTWRITEBYTECODE="1", CUDA_CACHE_PATH=scratch_path,
                           TORCHINDUCTOR_CACHE_DIR=scratch_path, TRITON_CACHE_DIR=scratch_path)
        if signal.getsignal(signal.SIGCHLD) != signal.SIG_DFL:
            raise Inconclusive("SIGCHLDSupervisionUnavailable")
        worker = subprocess.Popen([sys.executable, "-u", "-c", WORKER], stdin=configuration,
                                  stdout=audit, stderr=subprocess.STDOUT, start_new_session=True, env=environment)
    audit.write("command=installed-python -u -c <embedded first-party softmax workload>; pid=" + str(worker.pid) + "\n")
    while True:
        if received_signal is not None:
            raise Inconclusive("Interrupted")
        remaining = cfg["timeout"] - (time.monotonic() - started)
        if remaining <= 0:
            raise Inconclusive("CommandTimeout")
        status = os.waitid(os.P_PID, worker.pid, os.WEXITED | os.WNOHANG | os.WNOWAIT)
        if status is not None and status.si_pid == worker.pid:
            break
        time.sleep(min(0.05, remaining))
    if received_signal is not None:
        raise Inconclusive("Interrupted")
    if time.monotonic() - started > cfg["timeout"]:
        raise Inconclusive("CommandTimeout")
    handle = os.open(cfg["report_path"], os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(handle, "r", encoding="utf-8") as stream:
        text = stream.read()
    outcome = json.loads(text)
    if outcome.get("verdict") not in ("Pass", "Fail", "Inconclusive") or not outcome.get("reason"):
        raise Inconclusive("InvalidWorkerOutcome")
    if status.si_code != os.CLD_EXITED or status.si_status != (2 if outcome["verdict"] == "Inconclusive" else 0):
        raise Inconclusive("WorkerExitStatusMismatch")
except Inconclusive as exc:
    outcome = {"verdict": "Inconclusive", "reason": str(exc)}
    if audit:
        traceback.print_exc(file=audit)
except (OSError, ValueError, TypeError, AttributeError, OverflowError, MemoryError, RecursionError, subprocess.SubprocessError):
    outcome = {"verdict": "Inconclusive", "reason": "SupervisorRuntimeError"}
    traceback.print_exc(file=audit or sys.stderr)
finally:
    cleanup_diagnostics = []
    if worker is not None:
        try:
            stop_owned_group(worker, audit)
        except (Inconclusive, OSError, ValueError):
            outcome = {"verdict": "Inconclusive", "reason": "OwnedProcessGroupCleanupFailed"}
            cleanup_diagnostics.append(traceback.format_exc())
    if scratch_fd is not None:
        if scratch_name is not None:
            try:
                import shutil
                os.fchdir(scratch_fd)
                shutil.rmtree(scratch_name)
            except (OSError, ImportError):
                outcome = {"verdict": "Inconclusive", "reason": "PrivateScratchCleanupFailed"}
                cleanup_diagnostics.append(traceback.format_exc())
        os.close(scratch_fd)
    for diagnostic in cleanup_diagnostics:
        try:
            (audit or sys.stderr).write(diagnostic)
        except (OSError, ValueError):
            sys.stderr.write("Cleanup diagnostic could not be saved to the audit log:\n" + diagnostic)
    outcome["test"] = TEST
    outcome["run_token"] = sys.argv[1]
    outcome["audit_log"] = audit_name
    if received_signal is not None:
        outcome.update(verdict="Inconclusive", reason="Interrupted")
    audit_errors = []
    if audit:
        try:
            pending = {**outcome, "proposed_verdict": outcome["verdict"]}
            pending.pop("verdict")
            audit.write(json.dumps({"event": "supervisor_pending_evidence_finalization", **pending}, sort_keys=True, allow_nan=False) + "\n")
            audit.flush()
        except (OSError, ValueError, TypeError):
            audit_errors.append(traceback.format_exc())
        try:
            audit.close()
        except (OSError, ValueError):
            audit_errors.append(traceback.format_exc())
        audit = None
    if audit_errors:
        outcome.update(verdict="Inconclusive", reason="AuditFinalizationFailed")
        try:
            sys.stderr.write("AuditFinalizationFailed:\n" + "\n".join(audit_errors))
        except (OSError, ValueError):
            audit_errors.append(traceback.format_exc())
        if result_fd is not None:
            diagnostic_name = TEST + "-audit-finalization-" + secrets.token_hex(12) + ".log"
            try:
                publish(result_fd, diagnostic_name, "\n".join(audit_errors))
                outcome["audit_error_log"] = diagnostic_name
            except (Inconclusive, OSError, ValueError):
                traceback.print_exc(file=sys.stderr)
    measurement = json.dumps({**outcome, "configuration": cfg}, sort_keys=True, allow_nan=False)
    if len(measurement.encode("utf-8")) >= LIMIT:
        measurement = json.dumps({"test": TEST, "run_token": sys.argv[1], "verdict": outcome["verdict"], "reason": outcome["reason"],
                                  "audit_log": audit_name, "audit_error_log": outcome.get("audit_error_log"),
                                  "detail": "Full measurements are in the private audit log."}, sort_keys=True)
    if result_fd is not None:
        for name, text in (("measurements.txt", measurement), ("reason", outcome["reason"]), ("verdict", outcome["verdict"])):
            try:
                publish(result_fd, name, text)
            except (Inconclusive, OSError, ValueError):
                outcome["verdict"] = "Inconclusive"
                outcome["reason"] = "ResultsPublicationFailed"
                traceback.print_exc(file=audit or sys.stderr)
        os.close(result_fd)
    else:
        outcome.update(verdict="Inconclusive", reason="ResultsDirectoryUnavailable")
    print(TEST + ": " + outcome["verdict"] + " " + outcome["reason"], flush=True)
sys.exit(2 if outcome["verdict"] == "Inconclusive" else 0)
PY
runner_pid=$!
if [[ -n $interrupted ]]; then
  kill -TERM "$runner_pid" 2>/dev/null || :
fi
if wait "$runner_pid"; then
  status=0
else
  status=$?
  if [[ -n $interrupted ]]; then
    wait "$runner_pid" 2>/dev/null || :
    status=2
  fi
fi
runner_pid=
trap - INT TERM HUP
if [[ $status != 0 && $status != 2 ]]; then
  fallback PythonRunnerFailed || :
  exit 2
fi
if [[ -n $interrupted ]]; then
  fallback Interrupted || :
  exit 2
fi
directory=${PROJECT_SIGNAL_RESULTS_DIR-/results}
verification=
verdict=
if [[ ! -L $directory/measurements.txt && -s $directory/measurements.txt &&
      ! -L $directory/verdict && -s $directory/verdict ]]; then
  IFS= read -r verification < "$directory/measurements.txt" || :
  IFS= read -r verdict < "$directory/verdict" || :
fi
if [[ $verification != *"\"run_token\": \"$run_token\""* ||
      ( $status == 0 && $verdict != Pass && $verdict != Fail ) ||
      ( $status == 2 && $verdict != Inconclusive ) ]]; then
  fallback PythonRunnerOutcomeMissing || :
  exit 2
fi
exit "$status"
