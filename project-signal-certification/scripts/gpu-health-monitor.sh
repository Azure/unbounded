#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
set -Eeuo pipefail
umask 077

if [[ $# -eq 1 && "$1" == --describe ]]; then
  while IFS= read -r line; do printf '%s\n' "$line"; done <<'DESCRIPTION'
Purpose: explicit snapshot or bounded monitor of assigned GPU identity, Xid, ECC,
  graphics/memory clocks, temperature/power and declared NVLink/C2C state/counters.
Tools/devices: python3 stdlib including decimal and POSIX waitid/WNOWAIT, nvidia-smi with nvlink -s/-e reads,
  c2c -s/-e reads for required mode, or per-GPU -q C2C mode evidence for disabled mode;
  journalctl with readable current-boot kernel journal. Missing telemetry is NOT Pass.
Required inputs (no hardware/threshold/workload defaults):
  SCRATCH_ROOT, COMMAND_TIMEOUT_SECONDS (per probe), GPU_DEVICES (index CSV),
  EXPECTED_GPU_COUNT, EXPECTED_GPU_PRODUCT_REGEX (fullmatch), HEALTH_MODE=snapshot|monitor,
  GPU_HEALTH_C2C_MODE=required|disabled (explicit; no default),
  GPU_HEALTH_TEMPERATURE_MODE=enforce|observe (explicit; no default),
  DURATION_SECONDS (0 for snapshot, positive for monitor), SAMPLE_INTERVAL_SECONDS
  (positive, <= monitor duration), XID_LOOKBACK_SECONDS (snapshot history window),
  MAX_KERNEL_LOG_AGE_SECONDS (positive explicit observability freshness guard),
  HEALTH_LIMITS_JSON with ALL numeric fields:
    max_power_w,min_graphics_clock_mhz,min_memory_clock_mhz,
    max_ecc_corrected_total,max_ecc_uncorrected_total,max_ecc_corrected_delta,
    max_ecc_uncorrected_delta,max_nvlink_total,max_nvlink_delta,max_xids.
  required C2C mode also requires max_c2c_total,max_c2c_delta; disabled mode forbids them.
  enforce temperature mode also requires positive max_temperature_c and fails above it.
  observe temperature mode forbids max_temperature_c, requires valid temperature on
  every GPU/sample, and records exact readings without evaluating temperature health.
  Its reason/measurements state: functional-validation observation only; no temperature health claim
  max power is positive, clocks nonnegative, ECC/Xid limits nonnegative
  integers; applicable link total/delta limits are exact nonnegative decimal values.
  HEALTH_LINKS_JSON={"0":{"nvlink":{"links":[0,1],"expected_state":"Active",
    "error_counters":["Replay Errors","Recovery Errors","CRC Errors"],
    "observation_counters":[]},"c2c":{"links":[0],"expected_state":"Active",
    "error_counters":["<exact installed-driver error labels>"],
    "observation_counters":[]}},...}
  Each GPU requires nonempty nvlink coverage; required C2C mode also requires the
  nonempty c2c policy above. Disabled mode forbids the c2c policy, and instead requires
  nvidia-smi -i <index> -q to report exactly GPU C2C Mode: Disabled or Not Supported,
  with matching GPU PCI identity and UUID, on every selected GPU and every sample.
  Enabled C2C fails; missing/unknown/ambiguous mode evidence is Inconclusive.
  Disabled mode skips only C2C link/counter thresholds and records explicit
  not-applicable evidence; it never treats generic N/A, empty output or errors as disabled.
  Link IDs/labels must match the installed driver's inventory exactly.
  expected_state=Active|Inactive|Sleep.
  Explicit observation_counters permit non-error traffic counters (e.g. NVLink5
  Tx bytes) without treating traffic as errors; no counters are silently ignored.
  Link values/limits accept canonical unsigned integers, leading-digit decimals and
  scientific notation (0, 0.0, 15e-255, 1.2E+3); no leading dot/sign/zero padding,
  NaN/Inf, units or trailing junk. Exponent signs are allowed; negative values are not.
  Link arithmetic is exact, not binary float; numeric JSON limit tokens are preserved.
  Link values/limits are serialized as decimal strings without precision loss.
  Arithmetic beyond Python decimal's range or a 4 MiB digit span is Inconclusive,
  never rounded/underflowed into a passing result. Counter reset/loss is Inconclusive.
Operational path default: PROJECT_SIGNAL_RESULTS_DIR=/results (isolated-test override).
  No implicit not-applicable mode, counter resets or undocumented C2C flags.
Outputs: verdict (Pass|Fail|Inconclusive), reason, measurements.txt <=16384 bytes,
  gpu-health-monitor.audit.log. Limit/state violation -> Fail/exit0; missing input,
  tools/permissions/observability, reset, parse error or timeout -> Inconclusive/exit2.
Snapshot checks absolute counters and Xids in its explicit lookback. Monitor also
  checks monotonic counter growth from baseline and current-run Xids, sampling through
  DURATION_SECONDS including a final sample. Wall time includes bounded query overhead.
No stress workload, telemetry resets, downloads, background service, or hardware writes.
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
  temperature_note=""
  if [[ "${GPU_HEALTH_TEMPERATURE_MODE:-}" == observe ]]; then
    temperature_note='; functional-validation observation only; no temperature health claim'
  fi
  printf 'Inconclusive\n' > "$results/verdict"
  printf 'PythonMissing%s\n' "$temperature_note" > "$results/reason"
  printf 'python3 is required; no health evidence was collected.%s\n' "$temperature_note" > "$results/measurements.txt"
  exit 2
fi
exec python3 - "$@" <<'PY'
import csv
import io
import json
import math
import os
import pathlib
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time

try:
    import decimal
except ImportError:
    decimal = None


class Stop(Exception):
    def __init__(self, reason, detail, verdict="Inconclusive"):
        super().__init__(detail)
        self.reason, self.detail, self.verdict = reason, detail, verdict


def required(name):
    value = os.environ.get(name, "")
    if not value.strip():
        raise Stop("InputMissing", name)
    return value


def integer(value, name, zero=False):
    if isinstance(value, bool) or not re.fullmatch(r"[0-9]+", str(value)) or int(value) < (0 if zero else 1) or int(value) > (1 << 63) - 1:
        raise Stop("InvalidInputOrCounter", name + " must be an unsigned signed64-range integer")
    return int(value)


def numeric(value, name, zero=False):
    try:
        result = float(value)
    except (TypeError, ValueError) as exc:
        raise Stop("ObservationOrInputInvalid", name) from exc
    if isinstance(value, bool) or not math.isfinite(result) or result < 0 or (result == 0 and not zero):
        raise Stop("ObservationOrInputInvalid", name)
    return result


def link_number(value, name):
    if isinstance(value, bool) or not isinstance(value, (str, int, decimal.Decimal)):
        raise Stop("LinkObservationUnavailable", name + " must be a finite nonnegative decimal")
    text = str(value)
    if not re.fullmatch(r"(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?(?:0|[1-9][0-9]*))?", text):
        raise Stop("LinkObservationUnavailable", name + " has invalid numeric syntax")
    try:
        result = decimal.Decimal(text)
    except decimal.DecimalException as exc:
        raise Stop("LinkObservationUnavailable", name + " exceeds decimal representation") from exc
    if not result.is_finite() or result.is_signed():
        raise Stop("LinkObservationUnavailable", name + " must be finite and nonnegative")
    return result


def link_sum(values):
    values = [value for value in values if value]
    if not values:
        return decimal.Decimal(0)
    if len(values) == 1:
        return values[0]
    precision = max(value.adjusted() for value in values) - min(value.as_tuple().exponent for value in values) + 1 + len(str(len(values)))
    if precision > 4 * 1024 * 1024:
        raise Stop("LinkArithmeticUnavailable", "exact link arithmetic exceeds the 4 MiB digit span")
    try:
        with decimal.localcontext() as context:
            context.prec, context.Emin, context.Emax = precision, decimal.MIN_EMIN, decimal.MAX_EMAX
            for failure in (decimal.Inexact, decimal.Underflow, decimal.Overflow, decimal.InvalidOperation):
                context.traps[failure] = True
            return sum(values, decimal.Decimal(0))
    except decimal.DecimalException as exc:
        raise Stop("LinkArithmeticUnavailable", "exact link arithmetic exceeds decimal range") from exc


def decimal_json(value):
    if isinstance(value, decimal.Decimal):
        return str(value)
    raise TypeError("Unsupported measurement value: " + type(value).__name__)


def pci(value):
    value = value.lower()
    if not re.fullmatch(r"(?:[0-9a-f]{4,8}:)?[0-9a-f]{2}:[0-9a-f]{2}(?:\.[0-7])?", value):
        raise Stop("GPUObservationInvalid", "invalid PCI identity")
    parts = value.split(":")
    if len(parts) == 2:
        parts.insert(0, "0000")
    parts[0] = f"{int(parts[0], 16):04x}"
    return ":".join(parts).split(".")[0]


class Context:
    def __init__(self):
        self.scratch, self.child = None, None
        self.notes = []
        self.results = pathlib.Path(os.environ.get("PROJECT_SIGNAL_RESULTS_DIR", "/results"))
        if not self.results.is_absolute() or self.results.is_symlink():
            raise Stop("UnsafeResultsPath", "results must be an absolute non-symlink directory")
        self.results.mkdir(parents=True, exist_ok=True)
        self.audit = self.open("gpu-health-monitor.audit.log")
        for name, data in (("verdict", b"Inconclusive"), ("reason", b"DiagnosticNotCompleted"),
                           ("measurements.txt", b"Health observations have not completed.\n")):
            with self.open(name) as stream:
                stream.write(data)

    def open(self, name):
        fd = os.open(self.results / name, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
        os.fchmod(fd, 0o600)
        return os.fdopen(fd, "wb")

    def log(self, text):
        data = text.encode()
        if self.audit.tell() + len(data) > 4 * 1024 * 1024:
            raise Stop("AuditLimitExceeded", "telemetry evidence exceeded 4 MiB")
        self.audit.write(data)
        self.audit.flush()

    def run(self, argv, kernel=False):
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
        self.log("$ " + " ".join(argv) + "\n")
        failure = None
        code = None
        with tempfile.TemporaryFile(dir=self.scratch) as stdout, tempfile.TemporaryFile(dir=self.scratch) as stderr:
            self.child = subprocess.Popen([executable, *argv[1:]], stdout=stdout, stderr=stderr,
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
            if stdout.tell() + stderr.tell() > 4 * 1024 * 1024:
                raise Stop("ObservationTooLarge", argv[0] + " output exceeded 4 MiB")
            stdout.seek(0)
            stderr.seek(0)
            text, errors = (stream.read().decode("utf-8", errors="strict") for stream in (stdout, stderr))
        # Kernel messages unrelated to GPU health are not copied into audit artifacts.
        self.log(f"exit={code}\n" + ("" if kernel else text + "\n" + errors) + "\n")
        if failure is not None:
            raise failure
        if code or errors.strip():
            raise Stop("TelemetryProbeFailed", f"{argv[0]} exit={code}; stderr present={bool(errors.strip())}")
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
        temperature_note = ("; functional-validation observation only; no temperature health claim"
                            if os.environ.get("GPU_HEALTH_TEMPERATURE_MODE") == "observe" else "")
        reason += temperature_note
        body = (f"verdict={verdict} reason={reason}\n{detail}\n" + "\n".join(self.notes)).encode()
        if len(body) > 16384:
            body = body[:16280].decode(errors="ignore").encode() + b"\nmeasurements_truncated=true; see audit log\n"
        try:
            for name, data in (("measurements.txt", body), ("reason", reason.encode()), ("verdict", verdict.encode())):
                with self.open(name) as stream:
                    stream.write(data)
        except OSError as exc:
            detail = ("Result publication failed" + temperature_note + ": " + str(exc) + "\n").encode()[:16384].decode(errors="ignore").encode()
            print(detail.decode(errors="replace"), file=sys.stderr)
            for name, data in (("verdict", b"Inconclusive"), ("reason", ("ResultPublicationFailed" + temperature_note).encode()), ("measurements.txt", detail)):
                try:
                    with self.open(name) as stream:
                        stream.write(data)
                except OSError as fallback:
                    print(f"Cannot finalize {name}: {fallback}", file=sys.stderr)
            return 2
        return 2 if verdict == "Inconclusive" else 0


def kernel_records(ctx, since=None):
    args = ["journalctl", "--dmesg", "--boot=0", "--no-pager", "--quiet", "--output=json"]
    args += ["--lines=1"] if since is None else ["--since=@" + str(since)]
    text = ctx.run(args, kernel=True)
    result = []
    for line in text.splitlines():
        row = json.loads(line)
        if row.get("_TRANSPORT") != "kernel" or not isinstance(row.get("MESSAGE"), str) or not row.get("_BOOT_ID"):
            raise Stop("KernelObservationInvalid", "incomplete kernel journal record")
        timestamp = integer(row["__REALTIME_TIMESTAMP"], "kernel timestamp") / 1e6
        if timestamp > time.time() + 1:
            raise Stop("KernelObservationInvalid", "kernel record timestamp is in the future")
        result.append((timestamp, row["_BOOT_ID"], row["MESSAGE"]))
    if since is None and len(result) != 1:
        raise Stop("KernelObservationUnavailable", "cannot establish current-boot kernel journal access")
    return result


def link_observation(ctx, kind, index, uuid, policy):
    def parse(text, counter_mode):
        header_seen, current = False, None
        result = {}
        for line in text.splitlines():
            line = line.strip()
            if not line:
                continue
            header = re.fullmatch(r"GPU\s+([0-9]+):.*\(UUID:\s*([^)]+)\)", line)
            if header:
                if header_seen or str(int(header[1])) != index or header[2].strip() != uuid:
                    raise Stop("LinkObservationInvalid", kind + " wrong/duplicate GPU identity")
                header_seen = True
                continue
            if not header_seen:
                raise Stop("LinkObservationInvalid", kind + " has no GPU identity header")
            link = re.fullmatch(r"(?:C2C\s+)?Link\s+([0-9]+):\s*(.*)", line, re.I)
            if link:
                current = int(link[1])
                if current not in policy["links"]:
                    raise Stop("LinkInventoryMismatch", f"GPU={index} {kind} unexpected link={current}", "Fail")
                payload = link[2].strip()
                if not counter_mode:
                    if current in result:
                        raise Stop("LinkObservationInvalid", kind + " duplicate link status")
                    bandwidth = re.fullmatch(r"(\S+)\s+GB/s", payload)
                    if bandwidth:
                        if link_number(bandwidth[1], kind + " active link bandwidth") <= 0:
                            raise Stop("LinkObservationInvalid", kind + " nonpositive active bandwidth")
                        result[current] = "Active"
                    elif payload.strip("<>").capitalize() in ("Active", "Inactive", "Sleep"):
                        result[current] = payload.strip("<>").capitalize()
                    else:
                        raise Stop("LinkObservationUnavailable", kind + " unknown link status")
                    continue
                result.setdefault(current, {})
                if not payload:
                    continue
                line = payload
            if not counter_mode or current is None:
                raise Stop("LinkObservationInvalid", kind + " unexpected output")
            counter = re.fullmatch(r"([^:]+):\s*(\S+)", line)
            if not counter:
                raise Stop("LinkObservationUnavailable", kind + " malformed/missing numeric counter")
            label = " ".join(counter[1].split())
            if label in result[current] or label not in policy["error_counters"] + policy["observation_counters"]:
                raise Stop("LinkObservationInvalid", kind + " duplicate/undeclared counter: " + label)
            result[current][label] = link_number(counter[2], kind + " counter")
        if not header_seen or set(result) != set(policy["links"]):
            raise Stop("LinkObservationUnavailable", kind + " missing declared links")
        if counter_mode:
            expected = set(policy["error_counters"] + policy["observation_counters"])
            if any(set(row) != expected for row in result.values()):
                raise Stop("LinkObservationUnavailable", kind + " missing declared counters")
        return result
    states = parse(ctx.run(["nvidia-smi", kind, "-i", index, "-s"]), False)
    counters = parse(ctx.run(["nvidia-smi", kind, "-i", index, "-e"]), True)
    if any(value != policy["expected_state"] for value in states.values()):
        raise Stop("LinkStateMismatch", f"GPU={index} {kind} states={states}", "Fail")
    return {f"{link}/{label}": values[label] for link, values in counters.items() for label in policy["error_counters"]}


def c2c_disabled_observation(ctx, index, uuid, bus):
    text = ctx.run(["nvidia-smi", "-i", index, "-q"])
    header_seen, fields = False, {}
    for line in text.splitlines():
        line = line.strip()
        header = re.fullmatch(r"GPU\s+([0-9a-fA-F:.]+)", line)
        if header:
            if header_seen or pci(header[1]) != bus:
                raise Stop("C2CObservationInvalid", "wrong/duplicate GPU PCI identity")
            header_seen = True
            continue
        field = re.fullmatch(r"(GPU UUID|GPU C2C Mode)\s*:\s*(.*)", line)
        if field:
            if not header_seen or field[1] in fields:
                raise Stop("C2CObservationInvalid", "unbound/duplicate C2C mode or GPU UUID")
            fields[field[1]] = field[2].strip()
    if not header_seen or set(fields) != {"GPU UUID", "GPU C2C Mode"} or fields["GPU UUID"] != uuid:
        raise Stop("C2CObservationUnavailable", "missing/mismatched per-GPU C2C mode evidence")
    state = fields["GPU C2C Mode"]
    if state == "Enabled":
        raise Stop("C2CExpectationMismatch", f"GPU={index} declared C2C disabled but observed Enabled", "Fail")
    if state not in ("Disabled", "Not Supported"):
        raise Stop("C2CObservationUnavailable", f"GPU={index} unrecognized explicit C2C mode")
    return state


def main(ctx):
    if sys.argv[1:]:
        raise Stop("InvalidArguments", "only --describe or no arguments are accepted")
    if decimal is None:
        raise Stop("PrerequisiteMissing", "Python decimal stdlib is required for exact link arithmetic")
    ctx.timeout = integer(required("COMMAND_TIMEOUT_SECONDS"), "COMMAND_TIMEOUT_SECONDS")
    mode = required("HEALTH_MODE")
    c2c_mode = required("GPU_HEALTH_C2C_MODE")
    if c2c_mode not in ("required", "disabled"):
        raise Stop("InvalidInput", "GPU_HEALTH_C2C_MODE must be required or disabled")
    temperature_mode = required("GPU_HEALTH_TEMPERATURE_MODE")
    if temperature_mode not in ("enforce", "observe"):
        raise Stop("InvalidInput", "GPU_HEALTH_TEMPERATURE_MODE must be enforce or observe")
    ctx.notes.append(json.dumps({"temperature_mode": temperature_mode,
                                 "temperature_health_evaluated": temperature_mode == "enforce"}))
    duration = integer(required("DURATION_SECONDS"), "DURATION_SECONDS", zero=True)
    interval = numeric(required("SAMPLE_INTERVAL_SECONDS"), "SAMPLE_INTERVAL_SECONDS")
    lookback = integer(required("XID_LOOKBACK_SECONDS"), "XID_LOOKBACK_SECONDS")
    freshness = integer(required("MAX_KERNEL_LOG_AGE_SECONDS"), "MAX_KERNEL_LOG_AGE_SECONDS")
    if mode not in ("snapshot", "monitor") or (mode == "snapshot" and duration != 0) or (mode == "monitor" and (duration == 0 or interval > duration)):
        raise Stop("InvalidInput", "mode/duration/interval mismatch")
    devices = required("GPU_DEVICES").split(",")
    if any(not re.fullmatch(r"[0-9]+", value) for value in devices) or len(set(devices)) != len(devices):
        raise Stop("InvalidInput", "GPU_DEVICES must be unique nonempty numeric indexes")
    devices = [str(int(value)) for value in devices]
    count = integer(required("EXPECTED_GPU_COUNT"), "EXPECTED_GPU_COUNT")
    if count != len(devices) or len(set(devices)) != count:
        raise Stop("InvalidInput", "GPU index count/alias mismatch")
    regex = required("EXPECTED_GPU_PRODUCT_REGEX")
    if len(regex) > 512:
        raise Stop("InvalidInput", "GPU product regex too long")
    try:
        product = re.compile(regex)
    except re.error as exc:
        raise Stop("InvalidInput", "invalid product regex") from exc
    limits = json.loads(required("HEALTH_LIMITS_JSON"), parse_int=str, parse_float=str)
    keys = {"max_power_w", "min_graphics_clock_mhz", "min_memory_clock_mhz",
            "max_ecc_corrected_total", "max_ecc_uncorrected_total", "max_ecc_corrected_delta",
            "max_ecc_uncorrected_delta", "max_nvlink_total", "max_nvlink_delta", "max_xids"}
    if temperature_mode == "enforce":
        keys.add("max_temperature_c")
    if c2c_mode == "required":
        keys.update(("max_c2c_total", "max_c2c_delta"))
    if not isinstance(limits, dict) or set(limits) != keys:
        raise Stop("InvalidInput", "all and only documented HEALTH_LIMITS_JSON fields are required")
    for name, value in limits.items():
        if name.startswith(("max_nvlink", "max_c2c")):
            limits[name] = link_number(value, name)
        elif name.startswith("max_ecc") or name == "max_xids":
            limits[name] = integer(value, name, zero=True)
        else:
            limits[name] = numeric(value, name, zero=name.startswith("min_"))
    links = json.loads(required("HEALTH_LINKS_JSON"))
    if not isinstance(links, dict) or set(links) != set(devices):
        raise Stop("InvalidInput", "link map must cover exactly GPU_DEVICES")
    link_kinds = {"nvlink", "c2c"} if c2c_mode == "required" else {"nvlink"}
    for gpu, kinds in links.items():
        if not isinstance(kinds, dict) or set(kinds) != link_kinds:
            raise Stop("InvalidInput", "link policies must exactly match GPU_HEALTH_C2C_MODE")
        for kind, policy in kinds.items():
            if not isinstance(policy, dict) or set(policy) != {"links", "expected_state", "error_counters", "observation_counters"}:
                raise Stop("InvalidInput", "link policy has missing/unknown fields")
            if not isinstance(policy["links"], list) or not policy["links"]:
                raise Stop("InvalidInput", gpu + " missing " + kind + " telemetry")
            policy["links"] = [integer(value, "link index", zero=True) for value in policy["links"]]
            if len(set(policy["links"])) != len(policy["links"]) or policy["expected_state"] not in ("Active", "Inactive", "Sleep"):
                raise Stop("InvalidInput", "duplicate link index or invalid explicit state")
            for key in ("error_counters", "observation_counters"):
                if not isinstance(policy[key], list) or (key == "error_counters" and not policy[key]):
                    raise Stop("InvalidInput", "error/observation counters must be explicit arrays")
                if any(not isinstance(label, str) or not re.fullmatch(r"[A-Za-z0-9 /_.-]{1,128}", label)
                       or label != " ".join(label.split()) for label in policy[key]):
                    raise Stop("InvalidInput", "invalid/unnormalized exact counter label")
            all_labels = policy["error_counters"] + policy["observation_counters"]
            if len(set(all_labels)) != len(all_labels):
                raise Stop("InvalidInput", "duplicate or ambiguously classified counter")
    root = pathlib.Path(required("SCRATCH_ROOT"))
    if not root.is_absolute() or root.is_symlink() or not root.is_dir():
        raise Stop("InvalidInput", "SCRATCH_ROOT must be an existing absolute non-symlink directory")
    ctx.scratch = tempfile.mkdtemp(prefix="signal-health-", dir=root)
    baseline, previous, identity = {}, {}, {}
    health_counters = ("ecc_corrected", "ecc_uncorrected", "nvlink")
    if c2c_mode == "required":
        health_counters += ("c2c",)
    start_wall, start_mono = time.time(), time.monotonic()
    since = start_wall - lookback if mode == "snapshot" else start_wall
    samples = 0
    while True:
        fields = "index,uuid,pci.bus_id,name,ecc.errors.corrected.volatile.total,ecc.errors.uncorrected.volatile.total,clocks.current.graphics,clocks.current.memory,temperature.gpu,power.draw"
        text = ctx.run(["nvidia-smi", "--id=" + ",".join(devices), "--query-gpu=" + fields, "--format=csv,noheader,nounits"])
        rows = list(csv.reader(io.StringIO(text)))
        if len(rows) != count or any(len(row) != 10 for row in rows):
            raise Stop("GPUObservationUnavailable", "missing GPU/telemetry columns")
        snapshot = {}
        buses = set()
        for row in rows:
            index, uuid, bus, name, corrected, uncorrected, graphics, memory, temperature, power = (value.strip() for value in row)
            if index not in devices or index in snapshot or not uuid or not name or name in ("N/A", "[N/A]"):
                raise Stop("GPUObservationInvalid", "unexpected/duplicate GPU or missing UUID")
            if not product.fullmatch(name):
                raise Stop("GPUProductMismatch", "GPU=" + index, "Fail")
            bus = pci(bus)
            if bus in buses or any(other != index and prior[0] == uuid for other, prior in identity.items()):
                raise Stop("GPUObservationInvalid", "GPU UUID/PCI identities must be unique")
            if index in identity and identity[index] != (uuid, bus):
                raise Stop("GPUIdentityChanged", "GPU identity changed during observation")
            identity[index] = (uuid, bus)
            buses.add(bus)
            observed = {"ecc_corrected": integer(corrected, "corrected ECC", zero=True),
                        "ecc_uncorrected": integer(uncorrected, "uncorrected ECC", zero=True),
                        "graphics_clock_mhz": numeric(graphics, "graphics clock", zero=True),
                        "memory_clock_mhz": numeric(memory, "memory clock", zero=True),
                        "temperature_c": numeric(temperature, "temperature", zero=True),
                        "power_w": numeric(power, "power", zero=True)}
            if temperature_mode == "observe":
                observed["temperature_c"] = temperature
                ctx.notes.append(json.dumps({"temperature_sample": samples + 1, "gpu": index,
                                             "temperature_c": temperature}, sort_keys=True))
            for kind, policy in links[index].items():
                counters = link_observation(ctx, kind, index, uuid, policy)
                observed[kind + "_counters"] = counters
                observed[kind] = link_sum(counters.values())
            if c2c_mode == "disabled":
                observed["c2c_status"] = c2c_disabled_observation(ctx, index, uuid, bus)
                observed["c2c_applicable"] = False
            snapshot[index] = observed
        latest = kernel_records(ctx)
        if time.time() - latest[0][0] > freshness:
            raise Stop("KernelObservationStale", "no kernel record within explicit freshness window")
        boot = latest[0][1]
        records = kernel_records(ctx, since)
        xid_count = 0
        for timestamp, record_boot, message in records:
            if record_boot != boot or timestamp < since:
                raise Stop("KernelObservationInvalid", "journal returned wrong boot/window")
            if re.search(r"\bXid\b", message, re.I) and "NVRM" in message:
                match = re.search(r"NVRM:\s*Xid\s*\(PCI:([0-9a-fA-F:.]+)\):\s*([0-9]+)\b", message)
                if not match:
                    raise Stop("XidObservationInvalid", "cannot attribute kernel Xid")
                if pci(match[1]) in buses:
                    xid_count += 1
                    ctx.log(json.dumps({"time": timestamp, "gpu_pci": pci(match[1]), "xid": int(match[2])}) + "\n")
        if xid_count > limits["max_xids"]:
            raise Stop("XidLimitViolation", f"selected_gpu_xids={xid_count} limit={limits['max_xids']}", "Fail")
        if not baseline:
            baseline = snapshot
        for index, observed in snapshot.items():
            failures = []
            if temperature_mode == "enforce" and observed["temperature_c"] > limits["max_temperature_c"]:
                failures.append("temperature_c")
            if observed["power_w"] > limits["max_power_w"]:
                failures.append("power_w")
            for value, limit in (("graphics_clock_mhz", "min_graphics_clock_mhz"), ("memory_clock_mhz", "min_memory_clock_mhz")):
                if observed[value] < limits[limit]:
                    failures.append(value)
            for counter in health_counters:
                if previous and observed[counter] < previous[index][counter]:
                    raise Stop("CounterReset", f"GPU={index} {counter}")
                if counter in ("nvlink", "c2c") and previous:
                    for label, value in observed[counter + "_counters"].items():
                        if value < previous[index][counter + "_counters"][label]:
                            raise Stop("CounterReset", f"GPU={index} {counter}/{label}")
                if counter in ("nvlink", "c2c"):
                    delta = decimal.Decimal(0) if observed[counter] == baseline[index][counter] else link_sum(
                        (observed[counter], baseline[index][counter].copy_negate()))
                else:
                    delta = observed[counter] - baseline[index][counter]
                if observed[counter] > limits["max_" + counter + "_total"] or delta > limits["max_" + counter + "_delta"]:
                    failures.append(counter + "_total_or_growth")
            if failures:
                ctx.notes.append(json.dumps({"gpu": index, "observation": observed}, sort_keys=True, default=decimal_json))
                raise Stop("GPUHealthLimitViolation", f"GPU={index} fields={','.join(failures)}", "Fail")
        previous = snapshot
        samples += 1
        ctx.log(json.dumps({"sample": samples, "observations": snapshot, "selected_gpu_xids": xid_count}, sort_keys=True, default=decimal_json) + "\n")
        elapsed = time.monotonic() - start_mono
        if mode == "snapshot" or elapsed >= duration:
            break
        time.sleep(min(interval, duration - elapsed))
    ctx.notes.append(json.dumps({"mode": mode, "c2c_mode": c2c_mode, "temperature_mode": temperature_mode,
                                 "samples": samples, "elapsed_seconds": elapsed,
                                 "last_observations": snapshot, "selected_gpu_xids": xid_count, "limits": limits}, sort_keys=True, default=decimal_json))
    if c2c_mode == "disabled":
        checks = "all other" if temperature_mode == "enforce" else "all evaluated"
        return "Pass", "DeclaredGPUHealthCheckedC2CNotApplicable", f"C2C explicitly declared not applicable and verified disabled/not supported on every selected GPU; {checks} health checks remained within explicit limits."
    observations = "declared" if temperature_mode == "enforce" else "evaluated"
    return "Pass", "DeclaredGPUHealthChecked", f"All {observations} GPU, kernel, NVLink and C2C observations remained within explicit limits."


ctx = None
verdict, reason, detail, code = "Inconclusive", "UnhandledError", "Health observations did not complete.", 2
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
