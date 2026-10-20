#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
set -Eeuo pipefail
umask 077

if [[ $# -eq 1 && "$1" == --describe ]]; then
  while IFS= read -r line; do printf '%s\n' "$line"; done <<'DESCRIPTION'
Purpose: sequential write/read and random write/read against one private scratch file;
  CRC32C data verification, exact fio JSON parsing, per-case optional explicit limits.
Tools/devices: python3 stdlib with POSIX waitid/WNOWAIT, fio 3.x (not the unrelated Fiona CLI), findmnt; writable
  mounted filesystem with reviewed capacity. Never accepts a raw block-device filename.
Required inputs (no workload, hardware or threshold defaults):
  SCRATCH_ROOT (existing absolute non-symlink directory), EXPECTED_MOUNT_SOURCE,
  EXPECTED_FILESYSTEM_TYPE (exact findmnt values), FIO_BIN (absolute executable path),
  DURATION_SECONDS (positive runtime PER CASE), COMMAND_TIMEOUT_SECONDS (per command;
  must exceed runtime, including later verification), FIO_BYTES (file size per job),
  FIO_BLOCK_BYTES, FIO_JOBS, FIO_IODEPTH, FIO_IOENGINE=sync|psync|libaio|io_uring,
  FIO_DIRECT=0|1, FIO_RANDOM_SEED (positive integer),
  FIO_LIMITS_JSON={} or {"seq-write":{"min_bw_bytes_per_second":N,"min_iops":N,
  "max_latency_ns":N},...}; allowed cases: seq-write,seq-read,rand-write,rand-read.
  Limits are optional, finite and positive; unknown cases/metrics are rejected.
  Synchronous engines require depth=1; direct blocks and sizes must be 512-byte aligned.
Operational default: PROJECT_SIGNAL_RESULTS_DIR=/results (override for isolated tests).
Outputs: verdict (Pass|Fail|Inconclusive), reason, measurements.txt <=16384 bytes,
  fio-storage.audit.log. Measured limit or I/O/integrity failure -> typed Fail/exit0;
  missing prerequisite/space/observation, invalid input, timeout -> Inconclusive/exit2.
Duration: a full-file initialization/verification plus four separate DURATION_SECONDS
  cases; time-based writes receive a bounded full-file CRC32C verification pass.
  Only our temporary child/files are removed; logs persist.
No downloads, device formatting, raw-device access, or implicit destructive testing.
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
  printf 'python3 is required; no storage workload was run.\n' > "$results/measurements.txt"
  exit 2
fi
exec python3 - "$@" <<'PY'
import errno
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
    if isinstance(value, bool) or not re.fullmatch(r"[0-9]+", str(value)) or int(value) < 1:
        raise Stop("InvalidInput", name + " must be a positive integer")
    return int(value)


def numeric(value, name, zero=False):
    if (isinstance(value, bool) or not isinstance(value, (float, int)) or not math.isfinite(value)
            or value < 0 or (not zero and value == 0)):
        raise Stop("ObservationOrLimitInvalid", name)
    return value


class Context:
    def __init__(self):
        self.scratch, self.child = None, None
        self.notes = []
        self.results = pathlib.Path(os.environ.get("PROJECT_SIGNAL_RESULTS_DIR", "/results"))
        if not self.results.is_absolute() or self.results.is_symlink():
            raise Stop("UnsafeResultsPath", "results must be an absolute non-symlink directory")
        self.results.mkdir(parents=True, exist_ok=True)
        self.audit = self.open("fio-storage.audit.log")
        for name, data in (("verdict", b"Inconclusive"), ("reason", b"DiagnosticNotCompleted"),
                           ("measurements.txt", b"Storage diagnostic has not completed.\n")):
            with self.open(name) as stream:
                stream.write(data)

    def open(self, name):
        fd = os.open(self.results / name, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
        os.fchmod(fd, 0o600)
        return os.fdopen(fd, "wb")

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
        header = ("$ " + " ".join(argv) + "\n").encode()
        if self.audit.tell() + len(header) > 4 * 1024 * 1024:
            raise Stop("AuditLimitExceeded", "command evidence exceeded 4 MiB")
        self.audit.write(header)
        self.audit.flush()
        failure = None
        code = None
        with tempfile.TemporaryFile(dir=self.scratch) as stdout, tempfile.TemporaryFile(dir=self.scratch) as stderr:
            self.child = subprocess.Popen([executable, *argv[1:]], stdout=stdout, stderr=stderr,
                                          env={**os.environ, "LC_ALL": "C"}, start_new_session=True)
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
            output, errors = (stream.read().decode("utf-8", errors="strict") for stream in (stdout, stderr))
        evidence = (f"exit={code}\n" + output + "\nstderr:\n" + errors + "\n").encode()
        if self.audit.tell() + len(evidence) > 4 * 1024 * 1024:
            raise Stop("AuditLimitExceeded", "cumulative command evidence exceeded 4 MiB")
        self.audit.write(evidence)
        self.audit.flush()
        if failure is not None:
            raise failure
        return code, output

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
    if ctx.timeout <= duration:
        raise Stop("InvalidInput", "command timeout must exceed each case runtime")
    root = pathlib.Path(required("SCRATCH_ROOT"))
    if not root.is_absolute() or root.is_symlink() or not root.is_dir():
        raise Stop("InvalidInput", "SCRATCH_ROOT must be an existing absolute non-symlink directory")
    source, fstype = required("EXPECTED_MOUNT_SOURCE"), required("EXPECTED_FILESYSTEM_TYPE")
    if "\n" in source or not re.fullmatch(r"[A-Za-z0-9_.-]+", fstype):
        raise Stop("InvalidInput", "invalid expected mount fields")
    binary = pathlib.Path(required("FIO_BIN"))
    if not binary.is_absolute() or not binary.is_file() or not os.access(binary, os.X_OK):
        raise Stop("PrerequisiteMissing", "FIO_BIN must name an absolute executable")
    size = integer(required("FIO_BYTES"), "FIO_BYTES")
    block = integer(required("FIO_BLOCK_BYTES"), "FIO_BLOCK_BYTES")
    jobs = integer(required("FIO_JOBS"), "FIO_JOBS")
    depth = integer(required("FIO_IODEPTH"), "FIO_IODEPTH")
    seed = integer(required("FIO_RANDOM_SEED"), "FIO_RANDOM_SEED")
    engine, direct = required("FIO_IOENGINE"), required("FIO_DIRECT")
    if engine not in ("sync", "psync", "libaio", "io_uring") or direct not in ("0", "1"):
        raise Stop("InvalidInput", "unsupported I/O engine/direct setting")
    if size < block or size % block or block % 512 or (engine in ("sync", "psync") and depth != 1):
        raise Stop("InvalidInput", "size/block alignment or synchronous depth mismatch")
    if size > (1 << 63) - 1 or jobs > 65535 or seed > (1 << 32) - 1:
        raise Stop("InvalidInput", "size/job/seed outside tool integer range")
    limits = json.loads(required("FIO_LIMITS_JSON"))
    cases = (("seq-write", "write"), ("seq-read", "read"), ("rand-write", "randwrite"), ("rand-read", "randread"))
    if not isinstance(limits, dict) or set(limits) - {case for case, _ in cases}:
        raise Stop("InvalidInput", "unknown case in FIO_LIMITS_JSON")
    metrics = {"min_bw_bytes_per_second", "min_iops", "max_latency_ns"}
    for name, value in limits.items():
        if not isinstance(value, dict) or set(value) - metrics:
            raise Stop("InvalidInput", "unknown limit for " + name)
        for key, threshold in value.items():
            numeric(threshold, name + "." + key)
    free = os.statvfs(root)
    if size * jobs > free.f_bavail * free.f_frsize:
        raise Stop("ScratchCapacityInsufficient", "explicit file allocation exceeds available filesystem capacity")
    ctx.scratch = tempfile.mkdtemp(prefix="signal-fio-", dir=root)
    code, mount_text = ctx.run(["findmnt", "--json", "--target", ctx.scratch, "--output", "SOURCE,FSTYPE,TARGET"])
    if code:
        raise Stop("MountObservationFailed", "findmnt returned nonzero")
    mounts = json.loads(mount_text)["filesystems"]
    if len(mounts) != 1 or any(not mounts[0].get(field) for field in ("source", "fstype", "target")):
        raise Stop("MountObservationUnavailable", "expected exactly one complete filesystem")
    if mounts[0]["source"] != source or mounts[0]["fstype"] != fstype:
        raise Stop("ScratchMountMismatch", "scratch filesystem does not match explicit approval")
    if not pathlib.Path(ctx.scratch).resolve().is_relative_to(pathlib.Path(mounts[0]["target"]).resolve()):
        raise Stop("MountObservationInvalid", "findmnt target does not contain scratch")
    code, version = ctx.run([str(binary), "--version"])
    if code or not re.fullmatch(r"fio-3\.[0-9]+(?:[-.][A-Za-z0-9.+_-]+)?\s*", version):
        raise Stop("UnsupportedFio", "requires the fio 3.x JSON interface")
    for index in range(jobs):
        file = pathlib.Path(ctx.scratch) / f"payload-{index}"
        with file.open("xb") as stream:
            stream.truncate(size)
    failures = []
    for name, mode in (("initialize", "write"), *cases):
        write_case = name != "initialize" and mode.endswith("write")
        argv = [str(binary), "--output-format=json", "--eta=never", "--name=" + name,
                "--directory=" + ctx.scratch, "--filename_format=payload-$jobnum", "--allow_file_create=0",
                "--size=" + str(size), "--bs=" + str(block), "--numjobs=" + str(jobs),
                "--iodepth=" + str(depth), "--ioengine=" + engine, "--direct=" + direct,
                "--rw=" + mode, "--group_reporting=1",
                "--randrepeat=1", "--randseed=" + str(seed), "--verify=crc32c", "--verify_fatal=1",
                "--verify_dump=0", "--do_verify=" + ("0" if write_case else "1")]
        if name != "initialize":
            argv += ["--runtime=" + str(duration), "--time_based=1"]
        code, text = ctx.run(argv)
        document = json.loads(text)
        observations = document["jobs"]
        if not isinstance(observations, list) or len(observations) != 1 or observations[0].get("jobname") != name:
            raise Stop("FioObservationUnavailable", "missing/duplicate/unexpected job: " + name)
        row = observations[0]
        error = row.get("error")
        if not isinstance(error, int) or isinstance(error, bool) or error < 0:
            raise Stop("FioObservationUnavailable", "job has no integer error field")
        if error in (errno.EIO, errno.EILSEQ, errno.EBADMSG):
            raise Stop("StorageIOOrIntegrityFailure", f"case={name} fio_errno={error}", "Fail")
        if code or error:
            raise Stop("FioExecutionFailed", f"case={name} exit={code} errno={error}")
        direction = "read" if mode.endswith("read") else "write"
        observed = row[direction]
        for key in ("io_bytes", "total_ios", "runtime"):
            numeric(observed[key], name + "." + key)
        if name == "initialize" and observed["io_bytes"] < size * jobs:
            raise Stop("ScratchInitializationIncomplete", "not every scratch-file byte was initialized")
        bandwidth = numeric(observed["bw_bytes"], name + ".bw_bytes")
        iops = numeric(observed["iops"], name + ".iops")
        latency = numeric(observed["clat_ns"]["max"], name + ".clat_ns.max", zero=True)
        if numeric(observed["short_ios"], name + ".short_ios", zero=True) != 0:
            raise Stop("ShortStorageIO", name, "Fail")
        values = {"bw_bytes_per_second": bandwidth, "iops": iops, "max_latency_ns": latency}
        verification = "inline"
        if write_case:
            verify_name = name + "-verify"
            verify_argv = [str(binary), "--output-format=json", "--eta=never", "--name=" + verify_name,
                           "--directory=" + ctx.scratch, "--filename_format=payload-$jobnum",
                           "--allow_file_create=0", "--size=" + str(size), "--bs=" + str(block),
                           "--numjobs=" + str(jobs), "--iodepth=" + str(depth), "--ioengine=" + engine,
                           "--direct=" + direct, "--rw=read", "--group_reporting=1",
                           "--randrepeat=1", "--randseed=" + str(seed), "--verify=crc32c",
                           "--verify_fatal=1", "--verify_dump=0", "--verify_only=1"]
            verify_code, verify_text = ctx.run(verify_argv)
            verify_document = json.loads(verify_text)
            verify_observations = verify_document["jobs"]
            if (not isinstance(verify_observations, list) or len(verify_observations) != 1
                    or verify_observations[0].get("jobname") != verify_name):
                raise Stop("FioObservationUnavailable", "missing/duplicate/unexpected job: " + verify_name)
            verify_row = verify_observations[0]
            verify_error = verify_row.get("error")
            if not isinstance(verify_error, int) or isinstance(verify_error, bool) or verify_error < 0:
                raise Stop("FioObservationUnavailable", "verification job has no integer error field")
            if verify_error in (errno.EIO, errno.EILSEQ, errno.EBADMSG):
                raise Stop("StorageIOOrIntegrityFailure", f"case={name} fio_errno={verify_error}", "Fail")
            if verify_code or verify_error:
                raise Stop("FioExecutionFailed", f"case={verify_name} exit={verify_code} errno={verify_error}")
            verified = verify_row["read"]
            for key in ("io_bytes", "total_ios", "runtime"):
                numeric(verified[key], verify_name + "." + key)
            if verified["io_bytes"] < size * jobs:
                raise Stop("ScratchVerificationIncomplete", "post-write CRC32C verification did not cover every byte")
            verification = "post_write_full_file"
        ctx.notes.append(json.dumps({"case": name, "fio": version.strip(), "bytes": observed["io_bytes"],
                                     "runtime_ms": observed["runtime"], "jobs": jobs, "direct": direct,
                                     "crc32c_verification": verification, **values}, sort_keys=True))
        for metric, threshold in limits.get(name, {}).items():
            observed_value = {"min_bw_bytes_per_second": bandwidth, "min_iops": iops, "max_latency_ns": latency}[metric]
            if (metric.startswith("min_") and observed_value < threshold) or (metric.startswith("max_") and observed_value > threshold):
                failures.append(f"{name}:{metric} observed={observed_value} limit={threshold}")
    if failures:
        raise Stop("StorageLimitViolation", "; ".join(failures), "Fail")
    return "Pass", "StorageCasesChecked", "All four explicit scratch-file cases produced verified, nonempty I/O."


ctx = None
verdict, reason, detail, code = "Inconclusive", "UnhandledError", "Storage checks did not complete.", 2
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
