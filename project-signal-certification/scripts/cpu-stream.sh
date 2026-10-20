#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
set -Eeuo pipefail
umask 077

if [[ $# -eq 1 && "$1" == --describe ]]; then
  while IFS= read -r line; do printf '%s\n' "$line"; done <<'DESCRIPTION'
Purpose: first-party STREAM-like copy/scale/add/triad memory benchmark, with every
  element checked each iteration. Readable OpenMP C source is embedded below.
Tools/devices: python3 stdlib with POSIX waitid/WNOWAIT, an explicitly supplied C/OpenMP compiler, numactl,
  lscpu; CPU/NUMA and memory allocation permissions. Not the external STREAM suite.
Required explicit inputs (no hardware/workload/threshold defaults):
  SCRATCH_ROOT, C_COMPILER (absolute executable), COMMAND_TIMEOUT_SECONDS,
  DURATION_SECONDS (minimum measured-loop duration), STREAM_ITERATIONS (minimum
  iterations; BOTH must be met before stopping), STREAM_ELEMENTS, STREAM_THREADS,
  STREAM_ALPHA (finite positive), STREAM_ABS_TOLERANCE, STREAM_REL_TOLERANCE
  (finite nonnegative tolerances),
  STREAM_MAX_BYTES (explicit allocation budget; three double arrays = 24*elements),
  CPU_LIST (CPU CSV/ranges), NUMA_NODES (node CSV/ranges), EXPECTED_ONLINE_CPU_COUNT,
  EXPECTED_NUMA_NODE_COUNT, EXPECTED_CPU_MODEL_REGEX (fullmatch),
  STREAM_LIMITS_JSON={} or {"copy":min_MB_per_second,...}; copy/scale/add/triad only.
  Optional limits must be positive; the slowest iteration is tested, not the best.
Operational paths: PROJECT_SIGNAL_RESULTS_DIR=/results, HOST_SYSFS_ROOT=/sys,
  HOST_PROC_ROOT=/proc; overrides permit controlled offline validation.
Outputs: verdict (Pass|Fail|Inconclusive), reason, measurements.txt <=16384 bytes,
  cpu-stream.audit.log. Correctness/explicit floor violations -> Fail/exit0;
  tool/input/memory/binding failure or timeout -> Inconclusive/exit2.
Duration: compile/probes plus max(DURATION_SECONDS, time for STREAM_ITERATIONS),
  bounded by COMMAND_TIMEOUT_SECONDS per command. Timeout must exceed duration.
Exact placement: numactl --physcpubind=CPU_LIST --membind=NUMA_NODES; OpenMP thread
  count fixed, inherited OpenMP affinity is removed. Only private scratch is removed.
No downloads, image construction, memory policy guessing, or automatic NUMA selection.
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
  printf 'python3 is required; no memory benchmark was run.\n' > "$results/measurements.txt"
  exit 2
fi
exec python3 - "$@" <<'PY'
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


SOURCE = r"""
#define _POSIX_C_SOURCE 200809L
#include <inttypes.h>
#include <math.h>
#include <omp.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <time.h>

static double now(void) {
    struct timespec value;
    if (clock_gettime(CLOCK_MONOTONIC, &value) != 0) exit(2);
    return (double)value.tv_sec + (double)value.tv_nsec / 1e9;
}

int main(int argc, char **argv) {
    if (argc != 8) return 2;
    const size_t n = (size_t)strtoull(argv[1], NULL, 10);
    const int threads = atoi(argv[2]);
    const double alpha = strtod(argv[3], NULL);
    const double atol = strtod(argv[4], NULL);
    const double rtol = strtod(argv[5], NULL);
    const double duration = strtod(argv[6], NULL);
    const uint64_t minimum = strtoull(argv[7], NULL, 10);
    if (!n || n > SIZE_MAX / sizeof(double) || threads <= 0 || minimum == 0) return 2;
    double *a = malloc(n * sizeof(double)), *b = malloc(n * sizeof(double)), *c = malloc(n * sizeof(double));
    if (!a || !b || !c) { free(a); free(b); free(c); return 2; }
    omp_set_dynamic(0);
    omp_set_num_threads(threads);
    int observed_threads = 0;
    #pragma omp parallel
    {
        #pragma omp single
        observed_threads = omp_get_num_threads();
    }
    if (observed_threads != threads) { free(a); free(b); free(c); return 2; }
    double seconds[4] = {0, 0, 0, 0}, worst[4] = {INFINITY, INFINITY, INFINITY, INFINITY};
    const int accesses[4] = {2, 2, 3, 3};
    uint64_t iterations = 0, mismatches = 0;
    const double start = now();
    do {
        #pragma omp parallel for
        for (size_t i = 0; i < n; ++i) { a[i] = (double)(i % 1024 + 1) / 1024.0; b[i] = 0; c[i] = 0; }
        double times[4], begin = now();
        #pragma omp parallel for
        for (size_t i = 0; i < n; ++i) c[i] = a[i];
        times[0] = now() - begin;
        begin = now();
        #pragma omp parallel for
        for (size_t i = 0; i < n; ++i) b[i] = alpha * c[i];
        times[1] = now() - begin;
        begin = now();
        #pragma omp parallel for
        for (size_t i = 0; i < n; ++i) c[i] = a[i] + b[i];
        times[2] = now() - begin;
        begin = now();
        #pragma omp parallel for
        for (size_t i = 0; i < n; ++i) a[i] = b[i] + alpha * c[i];
        times[3] = now() - begin;
        uint64_t bad = 0;
        #pragma omp parallel for reduction(+:bad)
        for (size_t i = 0; i < n; ++i) {
            const double input = (double)(i % 1024 + 1) / 1024.0;
            const double expected = (2 * alpha + alpha * alpha) * input;
            if (!isfinite(a[i]) || !isfinite(b[i]) || !isfinite(c[i]) ||
                fabs(a[i] - expected) > atol + rtol * fabs(expected) ||
                fabs(b[i] - alpha * input) > atol + rtol * fabs(alpha * input) ||
                fabs(c[i] - (1 + alpha) * input) > atol + rtol * fabs((1 + alpha) * input)) ++bad;
        }
        mismatches += bad;
        for (int op = 0; op < 4; ++op) {
            if (!isfinite(times[op]) || times[op] <= 0) { free(a); free(b); free(c); return 2; }
            const double mbps = ((double)n * sizeof(double) * accesses[op] / 1e6) / times[op];
            if (!isfinite(mbps)) { free(a); free(b); free(c); return 2; }
            seconds[op] += times[op];
            if (mbps < worst[op]) worst[op] = mbps;
        }
        ++iterations;
        if (bad) break;
    } while (iterations < minimum || now() - start < duration);
    printf("{\"elements\":%zu,\"threads\":%d,\"iterations\":%" PRIu64
           ",\"elapsed_seconds\":%.9g,\"mismatches\":%" PRIu64 ",\"operations\":{",
           n, observed_threads, iterations, now() - start, mismatches);
    const char *names[4] = {"copy", "scale", "add", "triad"};
    for (int op = 0; op < 4; ++op)
        printf("%s\"%s\":{\"worst_mbps\":%.9g,\"mean_mbps\":%.9g}", op ? "," : "", names[op],
               worst[op], ((double)n * sizeof(double) * accesses[op] * iterations / 1e6) / seconds[op]);
    printf("}}\n");
    free(a); free(b); free(c);
    return 0;
}
"""


class Stop(Exception):
    def __init__(self, reason, detail, verdict="Inconclusive"):
        super().__init__(detail)
        self.reason, self.detail, self.verdict = reason, detail, verdict


def required(name):
    value = os.environ.get(name, "")
    if not value.strip():
        raise Stop("InputMissing", name)
    return value


def integer(value, name):
    if isinstance(value, bool) or not re.fullmatch(r"[0-9]+", str(value)) or not 1 <= int(value) <= (1 << 63) - 1:
        raise Stop("InvalidInput", name + " must be a positive signed64 integer")
    return int(value)


def finite(value, name, zero=False):
    try:
        number = float(value)
    except (ValueError, TypeError) as exc:
        raise Stop("InvalidInput", name) from exc
    if isinstance(value, bool) or not math.isfinite(number) or number < 0 or (not zero and number == 0):
        raise Stop("InvalidInput", name + " must be finite and " + ("nonnegative" if zero else "positive"))
    return number


def indexes(value, name):
    result = set()
    for part in value.split(","):
        if not re.fullmatch(r"[0-9]+(?:-[0-9]+)?", part):
            raise Stop("InvalidInput", name + " requires nonempty CPU/node ranges")
        bounds = [int(token) for token in part.split("-")]
        if bounds[-1] < bounds[0] or bounds[-1] > 1048576:
            raise Stop("InvalidInput", name + " range is invalid")
        group = set(range(bounds[0], bounds[-1] + 1))
        if result & group:
            raise Stop("InvalidInput", name + " has duplicates")
        result.update(group)
    return result


class Context:
    def __init__(self):
        self.scratch, self.child = None, None
        self.notes = []
        self.results = pathlib.Path(os.environ.get("PROJECT_SIGNAL_RESULTS_DIR", "/results"))
        if not self.results.is_absolute() or self.results.is_symlink():
            raise Stop("UnsafeResultsPath", "results must be an absolute non-symlink directory")
        self.results.mkdir(parents=True, exist_ok=True)
        self.audit = self.open("cpu-stream.audit.log")
        for name, data in (("verdict", b"Inconclusive"), ("reason", b"DiagnosticNotCompleted"),
                           ("measurements.txt", b"CPU memory diagnostic has not completed.\n")):
            with self.open(name) as stream:
                stream.write(data)

    def open(self, name):
        fd = os.open(self.results / name, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
        os.fchmod(fd, 0o600)
        return os.fdopen(fd, "wb")

    def run(self, argv, environment=None):
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
        header = ("$ " + " ".join(argv) + "\n").encode()
        if self.audit.tell() + len(header) > 4 * 1024 * 1024:
            raise Stop("AuditLimitExceeded", "command evidence exceeded 4 MiB")
        self.audit.write(header)
        self.audit.flush()
        failure = None
        code = None
        with tempfile.TemporaryFile(dir=self.scratch) as stdout, tempfile.TemporaryFile(dir=self.scratch) as stderr:
            self.child = subprocess.Popen([executable, *argv[1:]], stdout=stdout, stderr=stderr,
                                          env=environment or {**os.environ, "LC_ALL": "C"}, start_new_session=True)
            try:
                code = self.wait_for_exit(self.timeout)
            except subprocess.TimeoutExpired:
                failure = Stop("CommandTimeout", pathlib.Path(argv[0]).name)
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
                raise Stop("AuditLimitExceeded", "command output exceeded 4 MiB")
            stdout.seek(0)
            stderr.seek(0)
            text, errors = (stream.read().decode("utf-8", errors="strict") for stream in (stdout, stderr))
        evidence = (f"exit={code}\n" + text + "\nstderr:\n" + errors + "\n").encode()
        if self.audit.tell() + len(evidence) > 4 * 1024 * 1024:
            raise Stop("AuditLimitExceeded", "cumulative output exceeded 4 MiB")
        self.audit.write(evidence)
        self.audit.flush()
        if failure is not None:
            raise failure
        if code:
            raise Stop("CompilerOrBenchmarkFailed", f"{pathlib.Path(argv[0]).name} exit={code}")
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
                with self.open(name) as output:
                    output.write(data)
        except OSError as exc:
            detail = ("Result publication failed: " + str(exc) + "\n").encode()[:16384].decode(errors="ignore").encode()
            print(detail.decode(errors="replace"), file=sys.stderr)
            for name, data in (("verdict", b"Inconclusive"), ("reason", b"ResultPublicationFailed"), ("measurements.txt", detail)):
                try:
                    with self.open(name) as output:
                        output.write(data)
                except OSError as fallback:
                    print(f"Cannot finalize {name}: {fallback}", file=sys.stderr)
            return 2
        return 2 if verdict == "Inconclusive" else 0


def main(ctx):
    if sys.argv[1:]:
        raise Stop("InvalidArguments", "only --describe or no arguments are accepted")
    ctx.timeout = integer(required("COMMAND_TIMEOUT_SECONDS"), "COMMAND_TIMEOUT_SECONDS")
    duration = integer(required("DURATION_SECONDS"), "DURATION_SECONDS")
    iterations = integer(required("STREAM_ITERATIONS"), "STREAM_ITERATIONS")
    elements = integer(required("STREAM_ELEMENTS"), "STREAM_ELEMENTS")
    threads = integer(required("STREAM_THREADS"), "STREAM_THREADS")
    budget = integer(required("STREAM_MAX_BYTES"), "STREAM_MAX_BYTES")
    alpha = finite(required("STREAM_ALPHA"), "STREAM_ALPHA")
    atol = finite(required("STREAM_ABS_TOLERANCE"), "STREAM_ABS_TOLERANCE", zero=True)
    rtol = finite(required("STREAM_REL_TOLERANCE"), "STREAM_REL_TOLERANCE", zero=True)
    if ctx.timeout <= duration or elements * 24 > budget or not math.isfinite(alpha * (2 + alpha)) or threads > 2147483647:
        raise Stop("InvalidInput", "duration/timeout, allocation budget, thread count or alpha range invalid")
    cpu_text, node_text = required("CPU_LIST"), required("NUMA_NODES")
    cpus, nodes = indexes(cpu_text, "CPU_LIST"), indexes(node_text, "NUMA_NODES")
    if threads > len(cpus):
        raise Stop("InvalidInput", "threads exceed explicitly bound CPUs")
    expected_cpus = integer(required("EXPECTED_ONLINE_CPU_COUNT"), "EXPECTED_ONLINE_CPU_COUNT")
    expected_nodes = integer(required("EXPECTED_NUMA_NODE_COUNT"), "EXPECTED_NUMA_NODE_COUNT")
    regex = required("EXPECTED_CPU_MODEL_REGEX")
    if len(regex) > 512:
        raise Stop("InvalidInput", "CPU product regex too long")
    try:
        model_pattern = re.compile(regex)
    except re.error as exc:
        raise Stop("InvalidInput", "invalid CPU product regex") from exc
    limits = json.loads(required("STREAM_LIMITS_JSON"))
    if not isinstance(limits, dict) or set(limits) - {"copy", "scale", "add", "triad"}:
        raise Stop("InvalidInput", "unknown STREAM operation limit")
    for operation, value in limits.items():
        limits[operation] = finite(value, operation)
    root = pathlib.Path(required("SCRATCH_ROOT"))
    compiler = pathlib.Path(required("C_COMPILER"))
    if not root.is_absolute() or root.is_symlink() or not root.is_dir():
        raise Stop("InvalidInput", "SCRATCH_ROOT must be an existing absolute non-symlink directory")
    if not compiler.is_absolute() or not compiler.is_file() or not os.access(compiler, os.X_OK):
        raise Stop("PrerequisiteMissing", "C_COMPILER must name an absolute executable")
    sysfs, proc = (pathlib.Path(os.environ.get(name, default)) for name, default in (("HOST_SYSFS_ROOT", "/sys"), ("HOST_PROC_ROOT", "/proc")))
    if not sysfs.is_absolute() or not proc.is_absolute():
        raise Stop("InvalidInput", "sysfs/proc roots must be absolute")
    node_root = sysfs / "devices/system/node"
    if not node_root.is_dir():
        raise Stop("CPUObservationUnavailable", "NUMA sysfs inventory is missing")
    actual_nodes = {int(path.name[4:]) for path in node_root.glob("node[0-9]*") if re.fullmatch(r"node[0-9]+", path.name)}
    if not actual_nodes:
        raise Stop("CPUObservationUnavailable", "NUMA sysfs inventory is empty")
    if len(actual_nodes) != expected_nodes or not nodes <= actual_nodes:
        raise Stop("CPUInventoryMismatch", "NUMA inventory differs from declared hardware", "Fail")
    bound_cpus = set()
    for node in nodes:
        bound_cpus.update(indexes((sysfs / f"devices/system/node/node{node}/cpulist").read_text().strip(), "observed CPU list"))
    if not cpus <= bound_cpus:
        raise Stop("InvalidInput", "bound CPUs do not belong to declared NUMA nodes")
    memory_text = (proc / "meminfo").read_text()
    memory_values = re.findall(r"^MemAvailable:\s+([0-9]+)\s+kB$", memory_text, re.M)
    if len(memory_values) != 1:
        raise Stop("MemoryObservationUnavailable", "MemAvailable missing")
    if elements * 24 > int(memory_values[0]) * 1024:
        raise Stop("MemoryBudgetUnavailable", "allocation exceeds currently available host memory")
    ctx.scratch = tempfile.mkdtemp(prefix="signal-stream-", dir=root)
    cpu_info = json.loads(ctx.run(["lscpu", "--json"]))["lscpu"]
    fields = {item["field"].rstrip(":"): str(item["data"]) for item in cpu_info}
    if not fields.get("Model name") or not fields.get("On-line CPU(s) list"):
        raise Stop("CPUObservationUnavailable", "CPU identity/online list missing")
    online = indexes(fields["On-line CPU(s) list"], "observed online CPUs")
    if len(online) != expected_cpus or not cpus <= online or not model_pattern.fullmatch(fields["Model name"]):
        raise Stop("CPUInventoryMismatch", "CPU model/count/assignment mismatch", "Fail")
    source, executable = pathlib.Path(ctx.scratch) / "stream.c", pathlib.Path(ctx.scratch) / "stream"
    source.write_text(SOURCE)
    ctx.run([str(compiler), "--version"])
    ctx.run([str(compiler), "-O3", "-std=c11", "-fopenmp", str(source), "-o", str(executable), "-lm"])
    if not executable.is_file() or not os.access(executable, os.X_OK):
        raise Stop("CompilerOutputMissing", "compiler did not create executable")
    environment = {key: value for key, value in os.environ.items() if not key.startswith(("OMP_", "GOMP_", "KMP_"))}
    environment.update({"LC_ALL": "C", "OMP_NUM_THREADS": str(threads), "OMP_THREAD_LIMIT": str(threads),
                        "OMP_DYNAMIC": "FALSE", "OMP_PROC_BIND": "TRUE", "OMP_PLACES": "threads"})
    text = ctx.run(["numactl", "--physcpubind=" + cpu_text, "--membind=" + node_text, str(executable),
                    str(elements), str(threads), repr(alpha), repr(atol), repr(rtol), str(duration), str(iterations)], environment)
    observed = json.loads(text)
    if observed["elements"] != elements or observed["threads"] != threads:
        raise Stop("BenchmarkObservationInvalid", "benchmark did not honor dimensions/threads")
    count = integer(observed["iterations"], "observed iterations")
    elapsed = finite(observed["elapsed_seconds"], "elapsed")
    mismatch = observed["mismatches"]
    if not isinstance(mismatch, int) or isinstance(mismatch, bool) or mismatch < 0:
        raise Stop("BenchmarkObservationInvalid", "mismatch count")
    ctx.notes.append(json.dumps({"cpu_list": cpu_text, "numa_nodes": node_text, "allocation_bytes": elements * 24,
                                 "alpha": alpha, "absolute_tolerance": atol, "relative_tolerance": rtol, **observed}, sort_keys=True))
    if mismatch:
        raise Stop("STREAMCorrectnessMismatch", f"mismatched_elements={mismatch}", "Fail")
    if count < iterations or elapsed < duration or set(observed["operations"]) != {"copy", "scale", "add", "triad"}:
        raise Stop("BenchmarkObservationIncomplete", "duration/iteration/operation coverage missing")
    failures = []
    for operation, row in observed["operations"].items():
        worst, mean = (finite(row[key], operation + "." + key) for key in ("worst_mbps", "mean_mbps"))
        if worst > mean * (1 + 1e-6):
            raise Stop("BenchmarkObservationInvalid", "slowest throughput exceeds mean")
        if operation in limits and worst < limits[operation]:
            failures.append(f"{operation} worst_MBps={worst} floor={limits[operation]}")
    if failures:
        raise Stop("STREAMThroughputBelowFloor", "; ".join(failures), "Fail")
    return "Pass", "STREAMCasesChecked", "Four first-party operations checked across the explicit CPU/memory binding."


ctx = None
verdict, reason, detail, code = "Inconclusive", "UnhandledError", "CPU benchmark did not complete.", 2
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
except (OSError, ValueError, KeyError, TypeError, OverflowError) as exc:
    reason, detail = "ObservationOrInputError", f"{type(exc).__name__}: {exc}"
finally:
    if ctx is not None:
        code = ctx.finish(verdict, reason, detail)
    else:
        print(f"{reason}: {detail}", file=sys.stderr)
sys.exit(code)
PY
